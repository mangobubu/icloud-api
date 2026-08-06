package store

import (
	"context"
	"database/sql"
	"fmt"
)

const schemaVersion = 2

// Migrate applies schema changes transactionally. Repeated calls are safe.
func (s *Store) Migrate(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", current, schemaVersion)
	}
	if current == schemaVersion {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	statements := schemaV2
	migrationName := "schema v2"
	if current == 1 {
		statements = migrateV1ToV2
		migrationName = "migration v1 to v2"
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply %s: %w", migrationName, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

var schemaV2 = []string{
	`CREATE TABLE admins (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL COLLATE NOCASE UNIQUE,
		password_hash TEXT NOT NULL,
		password_version INTEGER NOT NULL DEFAULT 1 CHECK(password_version >= 1),
		created_at INTEGER NOT NULL
	)`,
	`CREATE TABLE admin_sessions (
		token_hash BLOB PRIMARY KEY,
		admin_id INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
		password_version INTEGER NOT NULL CHECK(password_version >= 1),
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
		uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
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
}

var migrateV1ToV2 = []string{
	`ALTER TABLE admins
		ADD COLUMN password_version INTEGER NOT NULL DEFAULT 1 CHECK(password_version >= 1)`,
	`ALTER TABLE admin_sessions
		ADD COLUMN password_version INTEGER NOT NULL DEFAULT 1 CHECK(password_version >= 1)`,
	`UPDATE aliases
		SET last_sync_status = 'pending', last_sync_error = '', last_synced_at = NULL
		WHERE id IN (
			SELECT alias_id FROM latest_messages WHERE uid_validity = 0 OR uid = 0
		)`,
	`CREATE TABLE latest_messages_v2 (
		alias_id INTEGER PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
		uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
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
	`INSERT INTO latest_messages_v2(
		alias_id, uid_validity, uid, message_id, internal_date, header_date,
		from_json, to_json, cc_json, subject, text_body, html_body,
		attachments_json, body_truncated, synced_at
	)
	SELECT
		alias_id, uid_validity, uid, message_id, internal_date, header_date,
		from_json, to_json, cc_json, subject, text_body, html_body,
		attachments_json, body_truncated, synced_at
	FROM latest_messages
	WHERE uid_validity > 0 AND uid > 0`,
	`DROP TABLE latest_messages`,
	`ALTER TABLE latest_messages_v2 RENAME TO latest_messages`,
}
