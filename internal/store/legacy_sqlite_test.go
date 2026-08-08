package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icloud-api/internal/secure"
)

var _ LegacySQLiteCipherValidator = (*secure.Cipher)(nil)

type legacyCipherValidatorStub struct {
	credentialValues []string
	appleValues      []string
	err              error
}

func (validator *legacyCipherValidatorStub) Decrypt(value string) (string, error) {
	validator.credentialValues = append(validator.credentialValues, value)
	return "", validator.err
}

func (validator *legacyCipherValidatorStub) DecryptAppleSession(value string) (string, error) {
	validator.appleValues = append(validator.appleValues, value)
	return "", validator.err
}

func TestLegacySQLiteReadOnlyDSN(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE fixture(value TEXT NOT NULL)`); err != nil {
		_ = db.Close()
		t.Fatalf("create fixture: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO fixture(value) VALUES('preserved')`); err != nil {
		_ = db.Close()
		t.Fatalf("write fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	dsn, resolvedPath, err := legacySQLiteReadOnlyDSN(databasePath)
	if err != nil {
		t.Fatalf("build read-only DSN: %v", err)
	}
	if resolvedPath != databasePath {
		t.Fatalf("resolved path = %q, want %q", resolvedPath, databasePath)
	}
	if !strings.Contains(dsn, "mode=ro") {
		t.Fatalf("read-only DSN does not contain mode=ro: %q", dsn)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse read-only DSN: %v", err)
	}
	if parsed.Query().Get("immutable") != "" {
		t.Fatalf("read-only DSN unexpectedly disables SQLite locking: %q", dsn)
	}

	readOnly, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open read-only fixture: %v", err)
	}
	t.Cleanup(func() { _ = readOnly.Close() })
	readTx, err := readOnly.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read-only snapshot: %v", err)
	}
	var value string
	if err := readTx.QueryRowContext(ctx, `SELECT value FROM fixture`).Scan(&value); err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if value != "preserved" {
		t.Fatalf("fixture value = %q, want preserved", value)
	}
	if err := readTx.Commit(); err != nil {
		t.Fatalf("commit read-only snapshot: %v", err)
	}
	if _, err := readOnly.ExecContext(ctx, `DELETE FROM fixture`); err == nil {
		t.Fatal("read-only fixture unexpectedly accepted a write")
	}
}

func TestLegacySQLiteReadOnlyDSNDecodesFileURIPaths(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "legacy 100%.db")
	fixture, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open encoded-path fixture: %v", err)
	}
	if _, err := fixture.Exec(`CREATE TABLE fixture(value TEXT); INSERT INTO fixture(value) VALUES('ok')`); err != nil {
		_ = fixture.Close()
		t.Fatalf("create encoded-path fixture: %v", err)
	}
	if err := fixture.Close(); err != nil {
		t.Fatalf("close encoded-path fixture: %v", err)
	}
	normalizedPath := filepath.ToSlash(databasePath)
	if !strings.HasPrefix(normalizedPath, "/") {
		normalizedPath = "/" + normalizedPath
	}

	opaquePath := filepath.ToSlash(databasePath)
	for _, rawURI := range []string{
		(&url.URL{Scheme: "file", Path: normalizedPath}).String(),
		"file:" + url.PathEscape(opaquePath),
	} {
		dsn, resolvedPath, err := legacySQLiteReadOnlyDSN(rawURI)
		if err != nil {
			t.Fatalf("build read-only DSN for %q: %v", rawURI, err)
		}
		if resolvedPath != databasePath {
			t.Fatalf("resolved path for %q = %q, want %q", rawURI, resolvedPath, databasePath)
		}
		if !strings.Contains(dsn, "mode=ro") {
			t.Fatalf("read-only DSN for %q does not contain mode=ro: %q", rawURI, dsn)
		}
		readOnly, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("open read-only DSN for %q: %v", rawURI, err)
		}
		var value string
		if err := readOnly.QueryRow(`SELECT value FROM fixture`).Scan(&value); err != nil {
			_ = readOnly.Close()
			t.Fatalf("read encoded-path fixture for %q: %v", rawURI, err)
		}
		if err := readOnly.Close(); err != nil {
			t.Fatalf("close read-only DSN for %q: %v", rawURI, err)
		}
		if value != "ok" {
			t.Fatalf("encoded-path fixture value for %q = %q", rawURI, value)
		}
	}
}

func TestLegacySQLiteCopySpecsHandleOlderSchemas(t *testing.T) {
	t.Parallel()

	v1 := legacySQLiteCopySpecs(false, false, false)
	if len(v1) != 6 {
		t.Fatalf("v1 copy spec count = %d, want 6", len(v1))
	}
	if !strings.Contains(v1[0].selectSQL, "password_hash, 1, created_at") {
		t.Fatalf("v1 admin query does not default password_version: %q", v1[0].selectSQL)
	}
	if !strings.Contains(v1[1].selectSQL, "admin_id, 1, csrf") {
		t.Fatalf("v1 session query does not default password_version: %q", v1[1].selectSQL)
	}

	v3 := legacySQLiteCopySpecs(true, true, true)
	if len(v3) != 7 || v3[len(v3)-1].table != "apple_web_sessions" {
		t.Fatalf("v3 copy specs do not include Apple sessions: %#v", v3)
	}
	for _, spec := range v3 {
		if spec.table == "imap_sync_states" {
			t.Fatal("legacy importer must not synthesize or copy an IMAP cursor")
		}
	}
}

func TestLegacySQLiteReadOnlyDSNDropsUnsafeURIOptions(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "legacy.db")
	normalizedPath := filepath.ToSlash(databasePath)
	if !strings.HasPrefix(normalizedPath, "/") {
		normalizedPath = "/" + normalizedPath
	}
	legacyURL := &url.URL{Scheme: "file", Path: normalizedPath}
	query := legacyURL.Query()
	query.Set("immutable", "1")
	query.Add("_pragma", "journal_mode(WAL)")
	legacyURL.RawQuery = query.Encode()

	dsn, resolvedPath, err := legacySQLiteReadOnlyDSN(legacyURL.String())
	if err != nil {
		t.Fatalf("build read-only URI: %v", err)
	}
	if resolvedPath != databasePath {
		t.Fatalf("resolved path = %q, want %q", resolvedPath, databasePath)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse read-only URI: %v", err)
	}
	if parsed.Query().Get("immutable") != "" {
		t.Fatalf("read-only URI retained immutable option: %q", dsn)
	}
	for _, pragma := range parsed.Query()["_pragma"] {
		if strings.Contains(strings.ToLower(pragma), "journal_mode") {
			t.Fatalf("read-only URI retained journal_mode pragma: %q", dsn)
		}
	}
}

func TestLegacySQLiteReadOnlyDSNNeverIgnoresSQLiteJournals(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "legacy.db")
	if err := os.WriteFile(databasePath+"-wal", []byte("pending WAL"), 0o600); err != nil {
		t.Fatalf("create WAL fixture: %v", err)
	}
	dsn, _, err := legacySQLiteReadOnlyDSN(databasePath)
	if err != nil {
		t.Fatalf("build read-only DSN: %v", err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse read-only DSN: %v", err)
	}
	if parsed.Query().Get("immutable") != "" {
		t.Fatalf("DSN would ignore an existing WAL: %q", dsn)
	}
}

func TestImportLegacySQLiteRequiresCiphertextValidator(t *testing.T) {
	t.Parallel()

	database := &Store{}
	if err := database.ImportLegacySQLite(context.Background(), "legacy.db"); err == nil ||
		!strings.Contains(err.Error(), "ciphertext validator") {
		t.Fatalf("unvalidated legacy import error = %v", err)
	}
}

func TestLegacySQLiteSequenceKeepsDeletedHighWatermark(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE admins(id INTEGER PRIMARY KEY AUTOINCREMENT);
		INSERT INTO admins(id) VALUES(42);
		DELETE FROM admins WHERE id = 42;`); err != nil {
		t.Fatalf("create sequence fixture: %v", err)
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin fixture snapshot: %v", err)
	}
	defer tx.Rollback()
	sequence, err := legacySQLiteSequence(ctx, tx, "admins")
	if err != nil {
		t.Fatalf("read legacy sequence: %v", err)
	}
	if sequence != 42 {
		t.Fatalf("legacy sequence = %d, want 42", sequence)
	}
}

func TestValidateLegacySQLiteCiphertext(t *testing.T) {
	t.Parallel()

	validator := &legacyCipherValidatorStub{}
	account := make([]any, 13)
	account[6] = "v1.account-ciphertext"
	if err := validateLegacySQLiteCiphertext("accounts", account, validator); err != nil {
		t.Fatalf("validate account ciphertext: %v", err)
	}
	appleSession := make([]any, 8)
	appleSession[1] = "as1.apple-ciphertext"
	if err := validateLegacySQLiteCiphertext("apple_web_sessions", appleSession, validator); err != nil {
		t.Fatalf("validate Apple session ciphertext: %v", err)
	}
	if len(validator.credentialValues) != 1 || validator.credentialValues[0] != account[6] {
		t.Fatalf("validated account ciphertexts = %#v", validator.credentialValues)
	}
	if len(validator.appleValues) != 1 || validator.appleValues[0] != appleSession[1] {
		t.Fatalf("validated Apple ciphertexts = %#v", validator.appleValues)
	}

	validator.err = sql.ErrNoRows
	if err := validateLegacySQLiteCiphertext("accounts", account, validator); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("validator error = %v, want wrapped sentinel", err)
	}
	if err := validateLegacySQLiteCiphertext("aliases", []any{"not encrypted"}, validator); err != nil {
		t.Fatalf("unrelated table validation error: %v", err)
	}
}

func TestPrepareLegacySQLiteValuesNormalizesPostgresTextAfterCipherValidation(t *testing.T) {
	t.Parallel()

	validator := &legacyCipherValidatorStub{}
	account := make([]any, 13)
	account[6] = "raw\x00cipher"
	if err := prepareLegacySQLiteValues("accounts", account, validator); err != nil {
		t.Fatalf("prepare account values: %v", err)
	}
	if len(validator.credentialValues) != 1 || validator.credentialValues[0] != "raw\x00cipher" {
		t.Fatalf("ciphertext was not validated before normalization: %#v", validator.credentialValues)
	}
	if account[6] != "raw\x00cipher" {
		t.Fatalf("ciphertext changed after validation: %q", account[6])
	}

	message := make([]any, 15)
	message[3] = "message\x00id"
	message[9] = string([]byte{'s', 0xff, 'u', 'b'})
	message[10] = "text\x00body"
	message[11] = string([]byte{0xfe, 'h', 't', 'm', 'l'})
	if err := prepareLegacySQLiteValues("latest_messages", message, validator); err != nil {
		t.Fatalf("prepare latest message values: %v", err)
	}
	for index, want := range map[int]string{
		3:  "message\uFFFDid",
		9:  "s\uFFFDub",
		10: "text\uFFFDbody",
		11: "\uFFFDhtml",
	} {
		if message[index] != want {
			t.Fatalf("latest_messages value %d = %q, want %q", index, message[index], want)
		}
	}
}

func TestValidateLegacySQLiteCiphertextRejectsWrongMasterKey(t *testing.T) {
	t.Parallel()

	correct, err := secure.NewCipher(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatalf("create correct cipher: %v", err)
	}
	wrong, err := secure.NewCipher(bytes.Repeat([]byte{0x32}, 32))
	if err != nil {
		t.Fatalf("create wrong cipher: %v", err)
	}
	credential, err := correct.Encrypt("app-specific-password")
	if err != nil {
		t.Fatalf("encrypt credential: %v", err)
	}
	account := make([]any, 13)
	account[6] = credential
	if err := validateLegacySQLiteCiphertext("accounts", account, correct); err != nil {
		t.Fatalf("correct master key rejected: %v", err)
	}
	if err := validateLegacySQLiteCiphertext("accounts", account, wrong); err == nil {
		t.Fatal("wrong master key accepted legacy ciphertext")
	}
}
