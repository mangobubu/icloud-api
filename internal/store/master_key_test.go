package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"icloud-api/internal/secure"
	modernsqlite "modernc.org/sqlite"
)

func init() {
	modernsqlite.MustRegisterScalarFunction(
		"pg_advisory_xact_lock",
		1,
		func(_ *modernsqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
			return int64(0), nil
		},
	)
}

func TestVerifyMasterKeyBindsOnceAndRejectsAnotherKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, raw := openMasterKeyFixture(t, dialectPostgres)
	firstKey := bytes.Repeat([]byte{0x41}, masterKeySize)
	firstKeyCopy := append([]byte(nil), firstKey...)

	if err := db.VerifyMasterKey(ctx, firstKey); err != nil {
		t.Fatalf("bind first master key: %v", err)
	}
	if !bytes.Equal(firstKey, firstKeyCopy) {
		t.Fatal("fingerprint verification modified the caller's master key")
	}

	var stored []byte
	if err := raw.QueryRowContext(ctx,
		`SELECT value FROM app_metadata WHERE name = ?`, masterKeyFingerprintName,
	).Scan(&stored); err != nil {
		t.Fatalf("read stored fingerprint: %v", err)
	}
	want := fingerprintMasterKey(firstKey)
	if !bytes.Equal(stored, want[:]) {
		t.Fatalf("stored fingerprint = %x, want %x", stored, want)
	}
	rawKeyHash := sha256.Sum256(firstKey)
	if bytes.Equal(stored, rawKeyHash[:]) {
		t.Fatal("stored fingerprint omitted the fixed domain separator")
	}

	if err := db.VerifyMasterKey(ctx, firstKey); err != nil {
		t.Fatalf("verify repeated master key: %v", err)
	}
	otherKey := bytes.Repeat([]byte{0x42}, masterKeySize)
	if err := db.VerifyMasterKey(ctx, otherKey); !errors.Is(err, ErrMasterKeyMismatch) {
		t.Fatalf("different master key error = %v, want ErrMasterKeyMismatch", err)
	}

	var retained []byte
	if err := raw.QueryRowContext(ctx,
		`SELECT value FROM app_metadata WHERE name = ?`, masterKeyFingerprintName,
	).Scan(&retained); err != nil {
		t.Fatalf("read retained fingerprint: %v", err)
	}
	if !bytes.Equal(retained, want[:]) {
		t.Fatal("mismatched verification replaced the database fingerprint")
	}
}

func TestVerifyMasterKeyRejectsInvalidInputsAndCorruptMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, raw := openMasterKeyFixture(t, dialectPostgres)

	if err := db.VerifyMasterKey(ctx, []byte("short")); err == nil || !strings.Contains(err.Error(), "want 32") {
		t.Fatalf("short key error = %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO app_metadata(name, value, updated_at) VALUES(?, ?, ?)`,
		masterKeyFingerprintName, []byte{0x01}, int64(1),
	); err != nil {
		t.Fatalf("insert corrupt fingerprint: %v", err)
	}
	if err := db.VerifyMasterKey(ctx, bytes.Repeat([]byte{0x41}, masterKeySize)); err == nil ||
		!strings.Contains(err.Error(), "invalid length") {
		t.Fatalf("corrupt fingerprint error = %v", err)
	}
}

func TestVerifyMasterKeyValidatesExistingCiphertextsBeforeFirstBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, raw := openMasterKeyFixture(t, dialectPostgres)
	correctKey := bytes.Repeat([]byte{0x61}, masterKeySize)
	correctCipher, err := secure.NewCipher(correctKey)
	if err != nil {
		t.Fatal(err)
	}
	imapCiphertext, err := correctCipher.Encrypt("legacy-imap-password")
	if err != nil {
		t.Fatal(err)
	}
	appleCiphertext, err := correctCipher.EncryptAppleSession(`{"token":"legacy"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO accounts(id, password_ciphertext) VALUES(?, ?)`, 7, imapCiphertext,
	); err != nil {
		t.Fatalf("insert encrypted account fixture: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO apple_web_sessions(account_id, session_ciphertext) VALUES(?, ?)`, 7, appleCiphertext,
	); err != nil {
		t.Fatalf("insert encrypted Apple session fixture: %v", err)
	}

	bound, err := db.VerifyStoredMasterKey(ctx, correctKey)
	if err != nil || bound {
		t.Fatalf("unbound database check = (%v, %v), want false, nil", bound, err)
	}
	if err := db.VerifyMasterKey(ctx, correctKey); err == nil ||
		!strings.Contains(err.Error(), "requires ciphertext validation") {
		t.Fatalf("unvalidated first binding error = %v", err)
	}

	wrongKey := bytes.Repeat([]byte{0x62}, masterKeySize)
	wrongCipher, err := secure.NewCipher(wrongKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.VerifyMasterKeyWithValidator(ctx, wrongKey, wrongCipher); err == nil ||
		!strings.Contains(err.Error(), "validate stored") {
		t.Fatalf("wrong key validation error = %v", err)
	}
	var fingerprintCount int
	if err := raw.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM app_metadata WHERE name = ?`, masterKeyFingerprintName,
	).Scan(&fingerprintCount); err != nil {
		t.Fatalf("count fingerprints after rejected key: %v", err)
	}
	if fingerprintCount != 0 {
		t.Fatal("rejected key was permanently bound to the database")
	}

	if err := db.VerifyMasterKeyWithValidator(ctx, correctKey, correctCipher); err != nil {
		t.Fatalf("bind validated existing database: %v", err)
	}
	bound, err = db.VerifyStoredMasterKey(ctx, correctKey)
	if err != nil || !bound {
		t.Fatalf("stored correct key check = (%v, %v), want true, nil", bound, err)
	}
	if _, err := db.VerifyStoredMasterKey(ctx, wrongKey); !errors.Is(err, ErrMasterKeyMismatch) {
		t.Fatalf("stored wrong key error = %v, want ErrMasterKeyMismatch", err)
	}
}

func TestVerifyMasterKeyValidatesPendingAliasAPIKeyBeforeFirstBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, raw := openMasterKeyFixture(t, dialectPostgres)
	correctKey := bytes.Repeat([]byte{0x63}, masterKeySize)
	correctCipher, err := secure.NewCipher(correctKey)
	if err != nil {
		t.Fatal(err)
	}
	pendingCiphertext, err := correctCipher.EncryptPendingAliasAPIKey("legacy-pending-api-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO aliases(id, credential_ciphertext) VALUES(?, '')`, 11,
	); err != nil {
		t.Fatalf("insert alias fixture: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext) VALUES(?, ?)`, 11, pendingCiphertext,
	); err != nil {
		t.Fatalf("insert pending API key fixture: %v", err)
	}

	wrongKey := bytes.Repeat([]byte{0x64}, masterKeySize)
	wrongCipher, err := secure.NewCipher(wrongKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.VerifyMasterKeyWithValidator(ctx, wrongKey, wrongCipher); err == nil ||
		!strings.Contains(err.Error(), "pending_alias_api_key") {
		t.Fatalf("wrong pending API key validation error = %v", err)
	}
	var fingerprintCount int
	if err := raw.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM app_metadata WHERE name = ?`, masterKeyFingerprintName,
	).Scan(&fingerprintCount); err != nil {
		t.Fatalf("count fingerprints after rejected pending API key: %v", err)
	}
	if fingerprintCount != 0 {
		t.Fatal("key rejected by pending API key validation was permanently bound")
	}
	if err := db.VerifyMasterKeyWithValidator(ctx, correctKey, correctCipher); err != nil {
		t.Fatalf("bind key after validating pending API key: %v", err)
	}
}

func TestVerifyMasterKeyIsPostgresOnly(t *testing.T) {
	t.Parallel()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	db := newStore(raw, dialectSQLite)
	err = db.VerifyMasterKey(context.Background(), bytes.Repeat([]byte{0x41}, masterKeySize))
	if err == nil || !strings.Contains(err.Error(), "requires PostgreSQL") {
		t.Fatalf("SQLite verification error = %v", err)
	}
}

func TestPostgresBootstrapAddsMetadataWithCurrentSchemaVersion(t *testing.T) {
	t.Parallel()
	if schemaVersion != 7 {
		t.Fatalf("schema version = %d, want v7", schemaVersion)
	}
	bootstrap := strings.Join(postgresMigrationBootstrap, "\n")
	if !strings.Contains(bootstrap, "CREATE TABLE IF NOT EXISTS app_metadata") {
		t.Fatal("PostgreSQL bootstrap does not create app_metadata for existing databases")
	}
	if strings.Contains(strings.Join(schemaV7, "\n"), "app_metadata") {
		t.Fatal("SQLite runtime schema unexpectedly contains master key metadata")
	}
}

func openMasterKeyFixture(t *testing.T, databaseDialect dialect) (*Store, *sql.DB) {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open SQL fixture: %v", err)
	}
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close SQL fixture: %v", err)
		}
	})
	if _, err := raw.Exec(`CREATE TABLE app_metadata (
		name TEXT PRIMARY KEY,
		value BLOB NOT NULL,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create app metadata fixture: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE accounts (
		id INTEGER PRIMARY KEY,
		password_ciphertext TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create account fixture: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE apple_web_sessions (
		account_id INTEGER PRIMARY KEY,
		session_ciphertext TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create Apple session fixture: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE aliases (
		id INTEGER PRIMARY KEY,
		credential_ciphertext TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create alias credential fixture: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE pending_alias_api_keys (
		alias_id INTEGER PRIMARY KEY,
		api_key_ciphertext TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create pending alias API key fixture: %v", err)
	}
	return newStore(raw, databaseDialect), raw
}
