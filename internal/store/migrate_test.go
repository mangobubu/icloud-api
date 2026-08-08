package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestMigrateV1ToV3(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-v1.db")
	legacy, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}

	for _, statement := range legacyV1Schema {
		if _, err := legacy.ExecContext(ctx, statement); err != nil {
			_ = legacy.Close()
			t.Fatalf("create legacy schema: %v", err)
		}
	}

	now := time.Now().UTC()
	expiresAt := now.Add(time.Hour)
	for _, fixture := range []struct {
		query string
		args  []any
	}{
		{
			`INSERT INTO admins(id, username, password_hash, created_at) VALUES(1, 'legacy-admin', 'legacy-hash', ?)`,
			[]any{now.UnixNano()},
		},
		{
			`INSERT INTO admin_sessions(token_hash, admin_id, csrf, expires_at, created_at)
			 VALUES(?, 1, 'legacy-csrf', ?, ?)`,
			[]any{[]byte("legacy-session-hash"), expiresAt.UnixNano(), now.UnixNano()},
		},
		{
			`INSERT INTO accounts(
				id, name, email, imap_host, imap_port, imap_username, password_ciphertext,
				enabled, last_sync_status, last_sync_error, last_synced_at, created_at, updated_at
			) VALUES(1, 'Legacy primary', 'primary@icloud.com', 'imap.mail.me.com', 993,
				'primary@icloud.com', 'ciphertext', 1, 'ok', '', ?, ?, ?)`,
			[]any{now.UnixNano(), now.UnixNano(), now.UnixNano()},
		},
		{
			`INSERT INTO aliases(
				id, account_id, address, label, api_key_hash, api_key_prefix, enabled,
				last_sync_status, last_sync_error, last_synced_at, created_at, updated_at
			) VALUES(1, 1, 'invalid@icloud.com', 'invalid', ?, 'invalid', 1,
				'ok', 'stale error', ?, ?, ?)`,
			[]any{[]byte("invalid-hash"), now.UnixNano(), now.UnixNano(), now.UnixNano()},
		},
		{
			`INSERT INTO aliases(
				id, account_id, address, label, api_key_hash, api_key_prefix, enabled,
				last_sync_status, last_sync_error, last_synced_at, created_at, updated_at
			) VALUES(2, 1, 'valid@icloud.com', 'valid', ?, 'valid', 1,
				'ok', '', ?, ?, ?)`,
			[]any{[]byte("valid-hash"), now.UnixNano(), now.UnixNano(), now.UnixNano()},
		},
		{
			`INSERT INTO latest_messages(
				alias_id, uid_validity, uid, message_id, internal_date, subject, synced_at
			) VALUES(1, 100, 0, 'invalid-message', ?, 'invalid snapshot', ?)`,
			[]any{now.UnixNano(), now.UnixNano()},
		},
		{
			`INSERT INTO latest_messages(
				alias_id, uid_validity, uid, message_id, internal_date, subject, synced_at
			) VALUES(2, 100, 7, 'valid-message', ?, 'retained snapshot', ?)`,
			[]any{now.UnixNano(), now.UnixNano()},
		},
	} {
		if _, err := legacy.ExecContext(ctx, fixture.query, fixture.args...); err != nil {
			_ = legacy.Close()
			t.Fatalf("insert legacy fixture: %v", err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	db, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("open and migrate legacy database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close migrated database: %v", err)
		}
	})

	var schemaVersion int
	if err := db.DB().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&schemaVersion); err != nil {
		t.Fatalf("read migrated schema version: %v", err)
	}
	if schemaVersion != 5 {
		t.Fatalf("schema version = %d, want 5", schemaVersion)
	}

	var adminPasswordVersion, sessionPasswordVersion int64
	if err := db.DB().QueryRowContext(ctx, `
		SELECT a.password_version, s.password_version
		FROM admins a JOIN admin_sessions s ON s.admin_id = a.id
		WHERE a.id = 1`).Scan(&adminPasswordVersion, &sessionPasswordVersion); err != nil {
		t.Fatalf("read migrated password versions: %v", err)
	}
	if adminPasswordVersion != 1 || sessionPasswordVersion != 1 {
		t.Fatalf("migrated password versions = (%d, %d), want (1, 1)",
			adminPasswordVersion, sessionPasswordVersion)
	}
	session, err := db.GetSessionByHash(ctx, []byte("legacy-session-hash"))
	if err != nil {
		t.Fatalf("read migrated session: %v", err)
	}
	if session.PasswordVersion != 1 || session.Username != "legacy-admin" {
		t.Fatalf("migrated session = %#v", session)
	}

	message, err := db.GetLatestMessage(ctx, 2)
	if err != nil {
		t.Fatalf("read retained latest message: %v", err)
	}
	if message.Subject != "retained snapshot" || message.UIDValidity != 100 || message.UID != 7 {
		t.Fatalf("retained latest message = %#v", message)
	}
	validAlias, err := db.GetAlias(ctx, 2)
	if err != nil {
		t.Fatalf("read alias with retained snapshot: %v", err)
	}
	if validAlias.LastSyncStatus != domain.SyncStatusOK || validAlias.LastSyncedAt == nil {
		t.Fatalf("valid alias sync state changed: %#v", validAlias)
	}

	if _, err := db.GetLatestMessage(ctx, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("removed UID 0 snapshot error = %v, want ErrNotFound", err)
	}
	invalidAlias, err := db.GetAlias(ctx, 1)
	if err != nil {
		t.Fatalf("read reset alias: %v", err)
	}
	if invalidAlias.LastSyncStatus != domain.SyncStatusPending ||
		invalidAlias.LastSyncError != "" || invalidAlias.LastSyncedAt != nil {
		t.Fatalf("reset alias sync state = %#v", invalidAlias)
	}

	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO latest_messages(alias_id, uid_validity, uid, internal_date, synced_at)
		VALUES(1, 100, 0, ?, ?)`, now.UnixNano(), now.UnixNano()); err == nil {
		t.Fatal("migrated latest_messages constraint accepted UID 0")
	}
}

func TestSQLiteV4ConvergenceAddsAliasQueryIndexes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "v4-index-convergence.db")
	current, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("create current database: %v", err)
	}
	for _, index := range []string{
		"aliases_account_address_idx",
		"aliases_enabled_account_address_idx",
	} {
		if _, err := current.DB().ExecContext(ctx, `DROP INDEX `+index); err != nil {
			_ = current.Close()
			t.Fatalf("drop %s from v4 fixture: %v", index, err)
		}
	}
	if err := current.Close(); err != nil {
		t.Fatalf("close v4 fixture: %v", err)
	}

	converged, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("reopen and converge v4 database: %v", err)
	}
	t.Cleanup(func() { _ = converged.Close() })

	wanted := map[string]string{
		"aliases_account_address_idx":         "create index aliases_account_address_idx on aliases(account_id, address, id)",
		"aliases_enabled_account_address_idx": "create index aliases_enabled_account_address_idx on aliases(account_id, enabled, address, id)",
	}
	for name, definition := range wanted {
		var got string
		if err := converged.DB().QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, name,
		).Scan(&got); err != nil {
			t.Fatalf("read converged %s definition: %v", name, err)
		}
		if normalizeTestSQL(got) != definition {
			t.Fatalf("%s definition = %q, want %q", name, normalizeTestSQL(got), definition)
		}
	}
}

func normalizeTestSQL(statement string) string {
	return strings.Join(strings.Fields(strings.ToLower(statement)), " ")
}

var legacyV1Schema = []string{
	`CREATE TABLE admins (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL COLLATE NOCASE UNIQUE,
		password_hash TEXT NOT NULL,
		created_at INTEGER NOT NULL
	)`,
	`CREATE TABLE admin_sessions (
		token_hash BLOB PRIMARY KEY,
		admin_id INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
		csrf TEXT NOT NULL,
		expires_at INTEGER NOT NULL,
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX admin_sessions_expires_at_idx ON admin_sessions(expires_at)`,
	`CREATE TABLE accounts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		email TEXT NOT NULL COLLATE NOCASE UNIQUE,
		imap_host TEXT NOT NULL,
		imap_port INTEGER NOT NULL CHECK(imap_port BETWEEN 1 AND 65535),
		imap_username TEXT NOT NULL,
		password_ciphertext TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)),
		last_sync_status TEXT NOT NULL DEFAULT 'pending',
		last_sync_error TEXT NOT NULL DEFAULT '',
		last_synced_at INTEGER,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE TABLE aliases (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		address TEXT NOT NULL COLLATE NOCASE UNIQUE,
		label TEXT NOT NULL DEFAULT '',
		api_key_hash BLOB NOT NULL UNIQUE CHECK(length(api_key_hash) > 0),
		api_key_prefix TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)),
		last_sync_status TEXT NOT NULL DEFAULT 'pending',
		last_sync_error TEXT NOT NULL DEFAULT '',
		last_synced_at INTEGER,
		last_accessed_at INTEGER,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE INDEX aliases_account_id_idx ON aliases(account_id)`,
	`CREATE TABLE latest_messages (
		alias_id INTEGER PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
		uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 0 AND 4294967295),
		uid INTEGER NOT NULL CHECK(uid BETWEEN 0 AND 4294967295),
		message_id TEXT NOT NULL DEFAULT '',
		internal_date INTEGER NOT NULL,
		header_date INTEGER,
		from_json TEXT NOT NULL DEFAULT '[]',
		to_json TEXT NOT NULL DEFAULT '[]',
		cc_json TEXT NOT NULL DEFAULT '[]',
		subject TEXT NOT NULL DEFAULT '',
		text_body TEXT NOT NULL DEFAULT '',
		html_body TEXT NOT NULL DEFAULT '',
		attachments_json TEXT NOT NULL DEFAULT '[]',
		body_truncated INTEGER NOT NULL DEFAULT 0 CHECK(body_truncated IN (0, 1)),
		synced_at INTEGER NOT NULL
	)`,
	`CREATE TABLE audit_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		admin_id INTEGER REFERENCES admins(id) ON DELETE SET NULL,
		username TEXT NOT NULL DEFAULT '',
		action TEXT NOT NULL,
		resource_type TEXT NOT NULL DEFAULT '',
		resource_id TEXT NOT NULL DEFAULT '',
		result TEXT NOT NULL DEFAULT '',
		ip TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX audit_logs_created_at_idx ON audit_logs(created_at DESC, id DESC)`,
	`CREATE INDEX audit_logs_admin_id_idx ON audit_logs(admin_id)`,
	`PRAGMA user_version = 1`,
}
