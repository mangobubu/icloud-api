package store

import (
	"context"
	"database/sql"
	"fmt"
)

const schemaVersion = 5

// Migrate applies schema changes transactionally. Repeated calls are safe.
func (s *Store) Migrate(ctx context.Context) error {
	if s.dialect == dialectPostgres {
		return s.migratePostgres(ctx)
	}
	return s.migrateSQLite(ctx)
}

func (s *Store) migrateSQLite(ctx context.Context) error {
	var current int
	if err := s.queryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", current, schemaVersion)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var statements []string
	var migrationName string
	switch current {
	case 0:
		statements = schemaV5
		migrationName = "schema v5"
	case 1:
		statements = append([]string{}, migrateV1ToV2...)
		statements = append(statements, migrateV2ToV3...)
		statements = append(statements, migrateV3ToV4...)
		statements = append(statements, migrateV4ToV5...)
		migrationName = "migration v1 to v5"
	case 2:
		statements = append([]string{}, migrateV2ToV3...)
		statements = append(statements, migrateV3ToV4...)
		statements = append(statements, migrateV4ToV5...)
		migrationName = "migration v2 to v5"
	case 3:
		statements = append([]string{}, migrateV3ToV4...)
		statements = append(statements, migrateV4ToV5...)
		migrationName = "migration v3 to v5"
	case 4:
		statements = migrateV4ToV5
		migrationName = "migration v4 to v5"
	case schemaVersion:
		migrationName = "schema v5 convergence"
	}
	for _, statement := range statements {
		if _, err := s.txExecContext(ctx, tx, statement); err != nil {
			return fmt.Errorf("apply %s: %w", migrationName, err)
		}
	}
	for _, statement := range sqliteSchemaConvergence {
		if _, err := s.txExecContext(ctx, tx, statement); err != nil {
			return fmt.Errorf("converge sqlite schema: %w", err)
		}
	}
	if current != schemaVersion {
		if _, err := s.txExecContext(ctx, tx, "PRAGMA user_version = 5"); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

func (s *Store) migratePostgres(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin postgres migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := s.txExecContext(ctx, tx, `SELECT pg_advisory_xact_lock(6963676868764372)`); err != nil {
		return fmt.Errorf("lock postgres migrations: %w", err)
	}
	for _, statement := range postgresMigrationBootstrap {
		if _, err := s.txExecContext(ctx, tx, statement); err != nil {
			return fmt.Errorf("bootstrap postgres migrations: %w", err)
		}
	}

	var current int
	if err := s.txQueryRowContext(ctx, tx,
		`SELECT version FROM schema_migrations WHERE id = 1 FOR UPDATE`,
	).Scan(&current); err != nil {
		return fmt.Errorf("read postgres schema version: %w", err)
	}
	if current > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", current, schemaVersion)
	}

	var statements []string
	var migrationName string
	switch current {
	case 0:
		statements = postgresSchemaV5
		migrationName = "postgres schema v5"
	case 3:
		statements = append([]string{}, postgresMigrateV3ToV4...)
		statements = append(statements, postgresMigrateV4ToV5...)
		migrationName = "postgres migration v3 to v5"
	case 4:
		statements = postgresMigrateV4ToV5
		migrationName = "postgres migration v4 to v5"
	case schemaVersion:
		migrationName = "postgres schema v5 convergence"
	default:
		return fmt.Errorf("postgres schema version %d has no migration path to version %d", current, schemaVersion)
	}

	for _, statement := range statements {
		if _, err := s.txExecContext(ctx, tx, statement); err != nil {
			return fmt.Errorf("apply %s: %w", migrationName, err)
		}
	}
	if current != schemaVersion {
		if _, err := s.txExecContext(ctx, tx,
			`UPDATE schema_migrations SET version = ?, updated_at = ? WHERE id = 1`,
			schemaVersion, timestamp(s.now()),
		); err != nil {
			return fmt.Errorf("set postgres schema version: %w", err)
		}
	}
	for _, statement := range postgresSchemaConvergence {
		if _, err := s.txExecContext(ctx, tx, statement); err != nil {
			return fmt.Errorf("converge postgres schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit postgres migration: %w", err)
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
	`CREATE INDEX aliases_account_address_idx ON aliases(account_id, address, id)`,
	`CREATE INDEX aliases_enabled_account_address_idx ON aliases(account_id, enabled, address, id)`,
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

var migrateV2ToV3 = []string{
	`CREATE TABLE apple_web_sessions (
		account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		session_ciphertext TEXT NOT NULL CHECK(length(trim(session_ciphertext)) > 0),
		apple_id TEXT NOT NULL CHECK(length(trim(apple_id)) > 0),
		region TEXT NOT NULL DEFAULT '',
		authenticated INTEGER NOT NULL DEFAULT 0 CHECK(authenticated IN (0, 1)),
		last_validated_at INTEGER,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
}

var schemaV3 = append(append([]string{}, schemaV2...), migrateV2ToV3...)

var migrateV3ToV4 = []string{
	`CREATE TABLE imap_sync_states (
		account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		last_uid INTEGER NOT NULL DEFAULT 0 CHECK(last_uid BETWEEN 0 AND 4294967295),
		updated_at INTEGER NOT NULL
	)`,
}

var schemaV4 = append(append([]string{}, schemaV3...), migrateV3ToV4...)

var migrateV4ToV5 = []string{
	`CREATE TABLE alias_creation_schedules (
		account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		enabled INTEGER NOT NULL DEFAULT 0 CHECK(enabled IN (0, 1)),
		planned_at_json TEXT NOT NULL DEFAULT '[]',
		next_run_at INTEGER,
		last_attempted_at INTEGER,
		last_created_at INTEGER,
		last_alias_address TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE INDEX alias_creation_schedules_due_idx
		ON alias_creation_schedules(enabled, next_run_at, account_id)`,
	`CREATE TABLE pending_alias_api_keys (
		alias_id INTEGER PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
		api_key_ciphertext TEXT NOT NULL CHECK(length(trim(api_key_ciphertext)) > 0),
		created_at INTEGER NOT NULL
	)`,
}

var schemaV5 = append(append([]string{}, schemaV4...), migrateV4ToV5...)

var sqliteSchemaConvergence = []string{
	`CREATE INDEX IF NOT EXISTS aliases_account_address_idx ON aliases(account_id, address, id)`,
	`CREATE INDEX IF NOT EXISTS aliases_enabled_account_address_idx ON aliases(account_id, enabled, address, id)`,
	`CREATE INDEX IF NOT EXISTS alias_creation_schedules_due_idx ON alias_creation_schedules(enabled, next_run_at, account_id)`,
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

var postgresMigrationBootstrap = []string{
	`CREATE EXTENSION IF NOT EXISTS citext`,
	`CREATE TABLE IF NOT EXISTS schema_migrations (
		id SMALLINT PRIMARY KEY CHECK(id = 1),
		version INTEGER NOT NULL CHECK(version >= 0),
		updated_at BIGINT NOT NULL
	)`,
	`INSERT INTO schema_migrations(id, version, updated_at)
	 VALUES(1, 0, 0)
	 ON CONFLICT(id) DO NOTHING`,
	`CREATE TABLE IF NOT EXISTS app_metadata (
		name TEXT PRIMARY KEY CHECK(length(trim(name)) > 0),
		value BYTEA NOT NULL,
		updated_at BIGINT NOT NULL
	)`,
}

var postgresSchemaV4 = []string{
	`CREATE TABLE admins (
		id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
		username CITEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		password_version BIGINT NOT NULL DEFAULT 1 CHECK(password_version >= 1),
		created_at BIGINT NOT NULL
	)`,
	`CREATE TABLE admin_sessions (
		token_hash BYTEA PRIMARY KEY,
		admin_id BIGINT NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
		password_version BIGINT NOT NULL CHECK(password_version >= 1),
		csrf TEXT NOT NULL,
		expires_at BIGINT NOT NULL,
		created_at BIGINT NOT NULL
	)`,
	`CREATE INDEX admin_sessions_expires_at_idx ON admin_sessions(expires_at)`,
	`CREATE INDEX admin_sessions_admin_id_idx ON admin_sessions(admin_id)`,
	`CREATE TABLE accounts (
		id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
		name TEXT NOT NULL,
		email CITEXT NOT NULL UNIQUE,
		imap_host TEXT NOT NULL,
		imap_port INTEGER NOT NULL CHECK(imap_port BETWEEN 1 AND 65535),
		imap_username TEXT NOT NULL,
		password_ciphertext TEXT NOT NULL,
		enabled BOOLEAN NOT NULL DEFAULT TRUE,
		last_sync_status TEXT NOT NULL DEFAULT 'pending',
		last_sync_error TEXT NOT NULL DEFAULT '',
		last_synced_at BIGINT,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL
	)`,
	`CREATE INDEX accounts_enabled_email_idx ON accounts(email, id) WHERE enabled`,
	`CREATE TABLE aliases (
		id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
		account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		address CITEXT NOT NULL UNIQUE,
		label TEXT NOT NULL DEFAULT '',
		api_key_hash BYTEA NOT NULL UNIQUE CHECK(octet_length(api_key_hash) > 0),
		api_key_prefix TEXT NOT NULL,
		enabled BOOLEAN NOT NULL DEFAULT TRUE,
		last_sync_status TEXT NOT NULL DEFAULT 'pending',
		last_sync_error TEXT NOT NULL DEFAULT '',
		last_synced_at BIGINT,
		last_accessed_at BIGINT,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL
	)`,
	`CREATE INDEX aliases_account_id_idx ON aliases(account_id)`,
	`CREATE INDEX aliases_account_address_idx ON aliases(account_id, address, id)`,
	`CREATE INDEX aliases_enabled_account_address_idx ON aliases(account_id, address, id) WHERE enabled`,
	`CREATE TABLE latest_messages (
		alias_id BIGINT PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
		uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid BIGINT NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		message_id TEXT NOT NULL DEFAULT '',
		internal_date BIGINT NOT NULL,
		header_date BIGINT,
		from_json TEXT NOT NULL DEFAULT '[]',
		to_json TEXT NOT NULL DEFAULT '[]',
		cc_json TEXT NOT NULL DEFAULT '[]',
		subject TEXT NOT NULL DEFAULT '',
		text_body TEXT NOT NULL DEFAULT '',
		html_body TEXT NOT NULL DEFAULT '',
		attachments_json TEXT NOT NULL DEFAULT '[]',
		body_truncated BOOLEAN NOT NULL DEFAULT FALSE,
		synced_at BIGINT NOT NULL
	)`,
	`CREATE TABLE audit_logs (
		id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
		admin_id BIGINT REFERENCES admins(id) ON DELETE SET NULL,
		username TEXT NOT NULL DEFAULT '',
		action TEXT NOT NULL,
		resource_type TEXT NOT NULL DEFAULT '',
		resource_id TEXT NOT NULL DEFAULT '',
		result TEXT NOT NULL DEFAULT '',
		ip TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL
	)`,
	`CREATE INDEX audit_logs_created_at_idx ON audit_logs(created_at DESC, id DESC)`,
	`CREATE INDEX audit_logs_admin_id_idx ON audit_logs(admin_id)`,
	`CREATE TABLE apple_web_sessions (
		account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		session_ciphertext TEXT NOT NULL CHECK(length(trim(session_ciphertext)) > 0),
		apple_id TEXT NOT NULL CHECK(length(trim(apple_id)) > 0),
		region TEXT NOT NULL DEFAULT '',
		authenticated BOOLEAN NOT NULL DEFAULT FALSE,
		last_validated_at BIGINT,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL
	)`,
	`CREATE TABLE imap_sync_states (
		account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		last_uid BIGINT NOT NULL DEFAULT 0 CHECK(last_uid BETWEEN 0 AND 4294967295),
		updated_at BIGINT NOT NULL
	)`,
	`CREATE TABLE data_migrations (
		name TEXT PRIMARY KEY,
		applied_at BIGINT NOT NULL
	)`,
}

var postgresMigrateV4ToV5 = []string{
	`CREATE TABLE alias_creation_schedules (
		account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		enabled BOOLEAN NOT NULL DEFAULT FALSE,
		planned_at_json TEXT NOT NULL DEFAULT '[]',
		next_run_at BIGINT,
		last_attempted_at BIGINT,
		last_created_at BIGINT,
		last_alias_address TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL
	)`,
	`CREATE INDEX alias_creation_schedules_due_idx
		ON alias_creation_schedules(enabled, next_run_at, account_id)`,
	`CREATE TABLE pending_alias_api_keys (
		alias_id BIGINT PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
		api_key_ciphertext TEXT NOT NULL CHECK(length(trim(api_key_ciphertext)) > 0),
		created_at BIGINT NOT NULL
	)`,
}

var postgresSchemaV5 = append(append([]string{}, postgresSchemaV4...), postgresMigrateV4ToV5...)

var postgresMigrateV3ToV4 = []string{
	`CREATE INDEX IF NOT EXISTS accounts_enabled_email_idx ON accounts(email, id) WHERE enabled`,
	`CREATE INDEX IF NOT EXISTS aliases_account_address_idx ON aliases(account_id, address, id)`,
	`CREATE INDEX IF NOT EXISTS aliases_enabled_account_address_idx ON aliases(account_id, address, id) WHERE enabled`,
	`CREATE TABLE imap_sync_states (
		account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		last_uid BIGINT NOT NULL DEFAULT 0 CHECK(last_uid BETWEEN 0 AND 4294967295),
		updated_at BIGINT NOT NULL
	)`,
	`CREATE TABLE data_migrations (
		name TEXT PRIMARY KEY,
		applied_at BIGINT NOT NULL
	)`,
}

var postgresSchemaConvergence = []string{
	`CREATE INDEX IF NOT EXISTS admin_sessions_admin_id_idx ON admin_sessions(admin_id)`,
	`CREATE INDEX IF NOT EXISTS accounts_enabled_email_idx ON accounts(email, id) WHERE enabled`,
	`CREATE INDEX IF NOT EXISTS aliases_account_address_idx ON aliases(account_id, address, id)`,
	`CREATE INDEX IF NOT EXISTS aliases_enabled_account_address_idx ON aliases(account_id, address, id) WHERE enabled`,
	`CREATE INDEX IF NOT EXISTS alias_creation_schedules_due_idx ON alias_creation_schedules(enabled, next_run_at, account_id)`,
}
