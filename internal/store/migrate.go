package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Keep the durable schema version at v8 for compatibility with deployments
// that already recorded v8. Mail groups are additive and are converged in
// place below, so upgrading does not force an unnecessary version boundary.
const schemaVersion = 8

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
		statements = append([]string{}, schemaV8WithMailGroups...)
		migrationName = "schema v8"
	case 1:
		statements = append([]string{}, migrateV1ToV2...)
		statements = append(statements, migrateV2ToV3...)
		statements = append(statements, migrateV3ToV4...)
		statements = append(statements, migrateV4ToV5...)
		statements = append(statements, migrateV5ToV6...)
		statements = append(statements, migrateV6ToV7...)
		statements = append(statements, migrateV7ToV8...)
		statements = append(statements, sqliteMailGroupSchema...)
		migrationName = "migration v1 to v8"
	case 2:
		statements = append([]string{}, migrateV2ToV3...)
		statements = append(statements, migrateV3ToV4...)
		statements = append(statements, migrateV4ToV5...)
		statements = append(statements, migrateV5ToV6...)
		statements = append(statements, migrateV6ToV7...)
		statements = append(statements, migrateV7ToV8...)
		statements = append(statements, sqliteMailGroupSchema...)
		migrationName = "migration v2 to v8"
	case 3:
		statements = append([]string{}, migrateV3ToV4...)
		statements = append(statements, migrateV4ToV5...)
		statements = append(statements, migrateV5ToV6...)
		statements = append(statements, migrateV6ToV7...)
		statements = append(statements, migrateV7ToV8...)
		statements = append(statements, sqliteMailGroupSchema...)
		migrationName = "migration v3 to v8"
	case 4:
		statements = append([]string{}, migrateV4ToV5...)
		statements = append(statements, migrateV5ToV6...)
		statements = append(statements, migrateV6ToV7...)
		statements = append(statements, migrateV7ToV8...)
		statements = append(statements, sqliteMailGroupSchema...)
		migrationName = "migration v4 to v8"
	case 5:
		statements, err = sqliteV5CompatibilityMigration(ctx, tx)
		if err != nil {
			return fmt.Errorf("inspect SQLite schema v5 compatibility: %w", err)
		}
		statements = append(statements, migrateV6ToV7...)
		statements = append(statements, migrateV7ToV8...)
		statements = append(statements, sqliteMailGroupSchema...)
		migrationName = "migration v5 to v8"
	case 6:
		for _, statement := range sqliteLegacyCompatibilityRepair {
			if _, err := s.txExecContext(ctx, tx, statement); err != nil {
				return fmt.Errorf("repair SQLite legacy compatibility schema: %w", err)
			}
		}
		statements = append([]string{}, migrateV6ToV7...)
		statements = append(statements, migrateV7ToV8...)
		statements = append(statements, sqliteMailGroupSchema...)
		migrationName = "migration v6 to v8"
	case 7:
		statements = append([]string{}, migrateV7ToV8...)
		statements = append(statements, sqliteMailGroupSchema...)
		migrationName = "migration v7 to v8"
	case schemaVersion:
		migrationName = "schema v8 convergence"
	}
	for _, statement := range statements {
		if _, err := s.txExecContext(ctx, tx, statement); err != nil {
			return fmt.Errorf("apply %s: %w", migrationName, err)
		}
	}
	if err := convergeSQLiteAliasCredentialMode(ctx, tx); err != nil {
		return fmt.Errorf("converge sqlite alias credential schema: %w", err)
	}
	if err := s.convergeSQLiteMailGroupSchema(ctx, tx); err != nil {
		return fmt.Errorf("converge sqlite mail group schema: %w", err)
	}
	for _, statement := range sqliteSchemaConvergence {
		if _, err := s.txExecContext(ctx, tx, statement); err != nil {
			return fmt.Errorf("converge sqlite schema: %w", err)
		}
	}
	// Keep validating the durable v6 compatibility tables after repairing them;
	// this catches malformed hand-edited databases before workers issue writes.
	if err := s.convergeSQLiteV6Schema(ctx, tx); err != nil {
		return fmt.Errorf("converge sqlite legacy compatibility schema: %w", err)
	}
	if current != schemaVersion {
		if _, err := s.txExecContext(ctx, tx, "PRAGMA user_version = 8"); err != nil {
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
		statements = append([]string{}, postgresSchemaV8WithMailGroups...)
		migrationName = "postgres schema v8"
	case 3:
		statements = append([]string{}, postgresMigrateV3ToV4...)
		statements = append(statements, postgresMigrateV4ToV5...)
		statements = append(statements, postgresMigrateV5ToV6...)
		statements = append(statements, postgresMigrateV6ToV7...)
		statements = append(statements, postgresMigrateV7ToV8...)
		statements = append(statements, postgresMailGroupSchema...)
		migrationName = "postgres migration v3 to v8"
	case 4:
		statements = append([]string{}, postgresMigrateV4ToV5...)
		statements = append(statements, postgresMigrateV5ToV6...)
		statements = append(statements, postgresMigrateV6ToV7...)
		statements = append(statements, postgresMigrateV7ToV8...)
		statements = append(statements, postgresMailGroupSchema...)
		migrationName = "postgres migration v4 to v8"
	case 5:
		statements = append([]string{}, postgresV5CompatibilityMigration...)
		statements = append(statements, postgresMigrateV6ToV7...)
		statements = append(statements, postgresMigrateV7ToV8...)
		statements = append(statements, postgresMailGroupSchema...)
		migrationName = "postgres migration v5 to v8"
	case 6:
		for _, statement := range postgresLegacyCompatibilityRepair {
			if _, err := s.txExecContext(ctx, tx, statement); err != nil {
				return fmt.Errorf("repair PostgreSQL legacy compatibility schema: %w", err)
			}
		}
		statements = append([]string{}, postgresMigrateV6ToV7...)
		statements = append(statements, postgresMigrateV7ToV8...)
		statements = append(statements, postgresMailGroupSchema...)
		migrationName = "postgres migration v6 to v8"
	case 7:
		statements = append([]string{}, postgresMigrateV7ToV8...)
		statements = append(statements, postgresMailGroupSchema...)
		migrationName = "postgres migration v7 to v8"
	case schemaVersion:
		migrationName = "postgres schema v8 convergence"
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
	for _, statement := range postgresAliasCredentialModeConvergence {
		if _, err := s.txExecContext(ctx, tx, statement); err != nil {
			return fmt.Errorf("converge postgres alias credential schema: %w", err)
		}
	}
	if err := s.convergePostgresMailGroupSchema(ctx, tx); err != nil {
		return fmt.Errorf("converge postgres mail group schema: %w", err)
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

const sqliteCreateAliasCreationSchedules = `CREATE TABLE alias_creation_schedules (
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
	)`

const sqliteCreatePendingAliasAPIKeys = `CREATE TABLE pending_alias_api_keys (
		alias_id INTEGER PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
		api_key_ciphertext TEXT NOT NULL CHECK(length(trim(api_key_ciphertext)) > 0),
		created_at INTEGER NOT NULL
	)`

const sqliteCreateConsumedMessages = `CREATE TABLE consumed_messages (
		alias_id INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
		uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		consumed_at INTEGER NOT NULL,
		PRIMARY KEY(alias_id, uid_validity, uid)
	)`

const sqliteCreateIMAPSeenTasks = `CREATE TABLE imap_seen_tasks (
		account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		created_at INTEGER NOT NULL,
		PRIMARY KEY(account_id, uid_validity, uid)
	)`

const sqliteCreateIMAPSeenTasksAccountCreatedIndex = `CREATE INDEX imap_seen_tasks_account_created_idx
	ON imap_seen_tasks(account_id, created_at, uid_validity, uid)`

// These idempotent statements run before the v6-to-v7 archive copy as well as
// during normal v7 convergence. They cover a process that was interrupted
// after an earlier release removed one of the legacy tables but before it
// recorded a durable migration boundary.
var sqliteLegacyCompatibilityRepair = []string{
	`CREATE TABLE IF NOT EXISTS latest_messages (
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
	`CREATE TABLE IF NOT EXISTS pending_alias_api_keys (
		alias_id INTEGER PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
		api_key_ciphertext TEXT NOT NULL CHECK(length(trim(api_key_ciphertext)) > 0),
		created_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS consumed_messages (
		alias_id INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
		uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		consumed_at INTEGER NOT NULL,
		PRIMARY KEY(alias_id, uid_validity, uid)
	)`,
	`CREATE TABLE IF NOT EXISTS imap_seen_tasks (
		account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		created_at INTEGER NOT NULL,
		PRIMARY KEY(account_id, uid_validity, uid)
	)`,
	`CREATE INDEX IF NOT EXISTS imap_seen_tasks_account_created_idx
		ON imap_seen_tasks(account_id, created_at, uid_validity, uid)`,
}

var migrateV4ToV5 = []string{
	sqliteCreateAliasCreationSchedules,
	`CREATE INDEX alias_creation_schedules_due_idx
		ON alias_creation_schedules(enabled, next_run_at, account_id)`,
	sqliteCreatePendingAliasAPIKeys,
}

var schemaV5 = append(append([]string{}, schemaV4...), migrateV4ToV5...)

var migrateV5ToV6 = []string{
	sqliteCreateConsumedMessages,
	sqliteCreateIMAPSeenTasks,
	sqliteCreateIMAPSeenTasksAccountCreatedIndex,
}

var schemaV6 = append(append([]string{}, schemaV5...), migrateV5ToV6...)

var migrateV6ToV7 = []string{
	`ALTER TABLE aliases ADD COLUMN credential_ciphertext TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE aliases ADD COLUMN imap_password_hash BLOB NOT NULL DEFAULT X''`,
	`ALTER TABLE aliases ADD COLUMN oauth_client_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE aliases ADD COLUMN refresh_token_hash BLOB NOT NULL DEFAULT X''`,
	`ALTER TABLE aliases ADD COLUMN credential_version INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE aliases ADD COLUMN credential_mode TEXT NOT NULL DEFAULT 'legacy'
		CHECK(credential_mode IN ('legacy', 'v2'))`,
	`ALTER TABLE aliases ADD COLUMN mailbox_uid_validity INTEGER NOT NULL DEFAULT 1
		CHECK(mailbox_uid_validity BETWEEN 1 AND 4294967295)`,
	`ALTER TABLE aliases ADD COLUMN mailbox_uid_next INTEGER NOT NULL DEFAULT 1
		CHECK(mailbox_uid_next BETWEEN 1 AND 4294967295)`,
	`CREATE UNIQUE INDEX aliases_oauth_client_id_idx
		ON aliases(oauth_client_id) WHERE oauth_client_id <> ''`,
	`CREATE INDEX aliases_imap_password_hash_idx ON aliases(imap_password_hash)`,
	`CREATE TABLE archived_messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		upstream_uid INTEGER NOT NULL CHECK(upstream_uid BETWEEN 1 AND 4294967295),
		message_id TEXT NOT NULL DEFAULT '',
		internal_date INTEGER NOT NULL,
		header_date INTEGER,
		from_json TEXT NOT NULL DEFAULT '[]',
		to_json TEXT NOT NULL DEFAULT '[]',
		cc_json TEXT NOT NULL DEFAULT '[]',
		subject TEXT NOT NULL DEFAULT '',
		content_path TEXT NOT NULL DEFAULT '',
		content_bytes INTEGER NOT NULL DEFAULT 0 CHECK(content_bytes >= 0),
		content_sha256 TEXT NOT NULL DEFAULT '',
		content_state TEXT NOT NULL DEFAULT 'metadata_only',
		body_truncated INTEGER NOT NULL DEFAULT 0 CHECK(body_truncated IN (0, 1)),
		synced_at INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		UNIQUE(account_id, uid_validity, upstream_uid)
	)`,
	`CREATE INDEX archived_messages_retention_idx
		ON archived_messages(content_state, internal_date, id)`,
	`CREATE TABLE alias_messages (
		alias_id INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
		message_id INTEGER NOT NULL REFERENCES archived_messages(id) ON DELETE CASCADE,
		mailbox_uid INTEGER NOT NULL CHECK(mailbox_uid BETWEEN 1 AND 4294967295),
		otp TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		PRIMARY KEY(alias_id, message_id),
		UNIQUE(alias_id, mailbox_uid)
	)`,
	`CREATE INDEX alias_messages_alias_uid_idx
		ON alias_messages(alias_id, mailbox_uid DESC)`,
	`CREATE INDEX alias_messages_alias_otp_idx
		ON alias_messages(alias_id, created_at DESC, message_id DESC) WHERE otp <> ''`,
	`INSERT OR IGNORE INTO archived_messages(
		account_id, uid_validity, upstream_uid, message_id, internal_date, header_date,
		from_json, to_json, cc_json, subject, content_path, content_bytes,
		content_sha256, content_state, body_truncated, synced_at, created_at
	)
	SELECT al.account_id, lm.uid_validity, lm.uid, lm.message_id, lm.internal_date,
		lm.header_date, lm.from_json, lm.to_json, lm.cc_json, lm.subject,
		'', 0, '', 'metadata_only', 0, lm.synced_at, lm.synced_at
	FROM latest_messages lm
	JOIN aliases al ON al.id = lm.alias_id`,
	`INSERT OR IGNORE INTO alias_messages(alias_id, message_id, mailbox_uid, otp, created_at)
	SELECT lm.alias_id, archived.id, 1, '', lm.internal_date
	FROM latest_messages lm
	JOIN aliases al ON al.id = lm.alias_id
	JOIN archived_messages archived
	  ON archived.account_id = al.account_id
	 AND archived.uid_validity = lm.uid_validity
	 AND archived.upstream_uid = lm.uid`,
	`UPDATE aliases SET mailbox_uid_next = 2
		WHERE id IN (SELECT alias_id FROM alias_messages)`,
}

var schemaV7 = append(append([]string{}, schemaV6...), migrateV6ToV7...)

var migrateV7ToV8 = []string{
	`CREATE TABLE IF NOT EXISTS account_mailbox_settings (
		account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		mailbox_type TEXT NOT NULL DEFAULT 'icloud'
			CHECK(mailbox_type IN ('icloud', 'custom')),
		email_suffix TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS account_mailbox_settings_type_idx
		ON account_mailbox_settings(mailbox_type, account_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS account_mailbox_settings_custom_suffix_idx
		ON account_mailbox_settings(email_suffix COLLATE NOCASE)
		WHERE mailbox_type = 'custom'`,
}

var schemaV8 = append(append([]string{}, schemaV7...), migrateV7ToV8...)

// Mail groups are an additive v8 convergence feature. The group table is
// global to the installation; aliases keep a nullable foreign key so existing
// rows stay ungrouped and deleting a group never deletes an address.
var sqliteMailGroupSchema = []string{
	`CREATE TABLE IF NOT EXISTS mail_groups (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		name_key TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`ALTER TABLE aliases ADD COLUMN group_id INTEGER REFERENCES mail_groups(id) ON DELETE SET NULL`,
	`CREATE INDEX IF NOT EXISTS aliases_group_id_idx ON aliases(group_id, address, id)`,
}

var schemaV8WithMailGroups = append(append([]string{}, schemaV8...), sqliteMailGroupSchema...)

var sqliteSchemaConvergence = []string{
	// Mailbox settings live in a side table so upgrading an existing v7
	// database never rewrites the heavily used accounts table. Missing rows are
	// interpreted as the historical iCloud mode by account reads.
	`CREATE TABLE IF NOT EXISTS account_mailbox_settings (
		account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		mailbox_type TEXT NOT NULL DEFAULT 'icloud'
			CHECK(mailbox_type IN ('icloud', 'custom')),
		email_suffix TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS account_mailbox_settings_type_idx
		ON account_mailbox_settings(mailbox_type, account_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS account_mailbox_settings_custom_suffix_idx
		ON account_mailbox_settings(email_suffix COLLATE NOCASE)
		WHERE mailbox_type = 'custom'`,
	// Keep the v1 mailbox snapshot and state tables available for existing
	// clients. These IF NOT EXISTS statements also repair databases that were
	// left at schema v7 by an earlier, destructive archive migration.
	`CREATE TABLE IF NOT EXISTS latest_messages (
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
	`CREATE TABLE IF NOT EXISTS pending_alias_api_keys (
		alias_id INTEGER PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
		api_key_ciphertext TEXT NOT NULL CHECK(length(trim(api_key_ciphertext)) > 0),
		created_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS consumed_messages (
		alias_id INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
		uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		consumed_at INTEGER NOT NULL,
		PRIMARY KEY(alias_id, uid_validity, uid)
	)`,
	`CREATE TABLE IF NOT EXISTS imap_seen_tasks (
		account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		created_at INTEGER NOT NULL,
		PRIMARY KEY(account_id, uid_validity, uid)
	)`,
	`CREATE INDEX IF NOT EXISTS imap_seen_tasks_account_created_idx
		ON imap_seen_tasks(account_id, created_at, uid_validity, uid)`,
	`CREATE INDEX IF NOT EXISTS aliases_account_address_idx ON aliases(account_id, address, id)`,
	`CREATE INDEX IF NOT EXISTS aliases_enabled_account_address_idx ON aliases(account_id, enabled, address, id)`,
	`CREATE INDEX IF NOT EXISTS alias_creation_schedules_due_idx ON alias_creation_schedules(enabled, next_run_at, account_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS aliases_oauth_client_id_idx
		ON aliases(oauth_client_id) WHERE oauth_client_id <> ''`,
	`CREATE INDEX IF NOT EXISTS aliases_imap_password_hash_idx ON aliases(imap_password_hash)`,
	`CREATE INDEX IF NOT EXISTS archived_messages_retention_idx
		ON archived_messages(content_state, internal_date, id)`,
	`CREATE INDEX IF NOT EXISTS alias_messages_alias_uid_idx
		ON alias_messages(alias_id, mailbox_uid DESC)`,
	`CREATE INDEX IF NOT EXISTS alias_messages_alias_otp_idx
		ON alias_messages(alias_id, created_at DESC, message_id DESC) WHERE otp <> ''`,
}

// sqliteAliasCredentialModeConvergence is deliberately implemented as a
// small inspection helper rather than an ALTER TABLE ... IF NOT EXISTS
// statement: SQLite has no portable form of that syntax. This also lets a
// v7 database created before credential modes were introduced converge in
// place without changing its user_version.
func convergeSQLiteAliasCredentialMode(ctx context.Context, tx *sql.Tx) error {
	credentialModePresent := false
	columns := []struct {
		name       string
		definition string
	}{
		{name: "api_key_prefix", definition: `TEXT NOT NULL DEFAULT ''`},
		{name: "credential_ciphertext", definition: `TEXT NOT NULL DEFAULT ''`},
		{name: "imap_password_hash", definition: `BLOB NOT NULL DEFAULT X''`},
		{name: "oauth_client_id", definition: `TEXT NOT NULL DEFAULT ''`},
		{name: "refresh_token_hash", definition: `BLOB NOT NULL DEFAULT X''`},
		{name: "credential_version", definition: `INTEGER NOT NULL DEFAULT 0`},
		{name: "credential_mode", definition: `TEXT NOT NULL DEFAULT 'legacy' CHECK(credential_mode IN ('legacy', 'v2'))`},
		{name: "mailbox_uid_validity", definition: `INTEGER NOT NULL DEFAULT 1 CHECK(mailbox_uid_validity BETWEEN 1 AND 4294967295)`},
		{name: "mailbox_uid_next", definition: `INTEGER NOT NULL DEFAULT 1 CHECK(mailbox_uid_next BETWEEN 1 AND 4294967295)`},
	}
	for _, column := range columns {
		var present bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM pragma_table_info('aliases')
				WHERE name = ? COLLATE NOCASE
			)`, column.name).Scan(&present); err != nil {
			return fmt.Errorf("inspect aliases %s column: %w", column.name, err)
		}
		if present {
			if column.name == "credential_mode" {
				credentialModePresent = true
			}
			continue
		}
		statement := `ALTER TABLE aliases ADD COLUMN ` + column.name + ` ` + column.definition
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("add aliases %s column: %w", column.name, err)
		}
	}
	if !credentialModePresent {
		if _, err := tx.ExecContext(ctx, `
			UPDATE aliases
			SET credential_mode = 'v2'
			WHERE credential_version > 0
			  AND length(trim(credential_ciphertext)) > 0
			  AND length(api_key_hash) = 32
			  AND length(imap_password_hash) = 32
			  AND length(trim(oauth_client_id)) > 0
			  AND length(refresh_token_hash) = 32`); err != nil {
			return fmt.Errorf("classify pre-mode v7 alias credentials: %w", err)
		}
	}
	return nil
}

// convergeSQLiteMailGroupSchema repairs databases that were interrupted after
// the migration transaction created only one half of the group schema. SQLite has
// no ALTER TABLE ... ADD COLUMN IF NOT EXISTS, so inspect the catalog first.
func (s *Store) convergeSQLiteMailGroupSchema(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS mail_groups (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		name_key TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create mail groups table: %w", err)
	}
	var present bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pragma_table_info('aliases')
			WHERE name = 'group_id' COLLATE NOCASE
		)`).Scan(&present); err != nil {
		return fmt.Errorf("inspect aliases group_id column: %w", err)
	}
	if !present {
		if _, err := tx.ExecContext(ctx,
			`ALTER TABLE aliases ADD COLUMN group_id INTEGER REFERENCES mail_groups(id) ON DELETE SET NULL`); err != nil {
			return fmt.Errorf("add aliases group_id column: %w", err)
		}
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pragma_table_info('mail_groups')
			WHERE name = 'name_key' COLLATE NOCASE
		)`).Scan(&present); err != nil {
		return fmt.Errorf("inspect mail_groups name_key column: %w", err)
	}
	if !present {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE mail_groups ADD COLUMN name_key TEXT`); err != nil {
			return fmt.Errorf("add mail_groups name_key column: %w", err)
		}
	}
	if err := s.convergeMailGroupNameKeys(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS aliases_group_id_idx ON aliases(group_id, address, id)`); err != nil {
		return fmt.Errorf("create aliases group index: %w", err)
	}
	return nil
}

type mailGroupNameKeyMigrationRow struct {
	id      int64
	name    string
	nameKey sql.NullString
}

// convergeMailGroupNameKeys backfills keys in Go so SQLite and PostgreSQL use
// exactly the same Unicode normalization and casing rules. Detect collisions
// before mutating anything to return an actionable migration error.
func (s *Store) convergeMailGroupNameKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := s.txQueryContext(ctx, tx,
		`SELECT id, name, name_key FROM mail_groups ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read mail groups for name key migration: %w", err)
	}
	groups := make([]mailGroupNameKeyMigrationRow, 0)
	for rows.Next() {
		var group mailGroupNameKeyMigrationRow
		if err := rows.Scan(&group.id, &group.name, &group.nameKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan mail group for name key migration: %w", err)
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate mail groups for name key migration: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close mail group name key migration rows: %w", err)
	}

	seen := make(map[string]mailGroupNameKeyMigrationRow, len(groups))
	changed := false
	for index := range groups {
		key := mailGroupNameKey(groups[index].name)
		if previous, exists := seen[key]; exists {
			return fmt.Errorf(
				"mail group name normalization conflict: %q (id %d) and %q (id %d) resolve to the same name key; rename one group before retrying migration",
				previous.name, previous.id, groups[index].name, groups[index].id,
			)
		}
		seen[key] = groups[index]
		if !groups[index].nameKey.Valid || groups[index].nameKey.String != key {
			changed = true
		}
		groups[index].nameKey = sql.NullString{String: key, Valid: true}
	}

	if changed {
		// A partially applied earlier migration may already have this index and
		// stale keys. Rebuilding it inside the transaction avoids update-order
		// conflicts while preserving atomicity on failure.
		if _, err := s.txExecContext(ctx, tx,
			`DROP INDEX IF EXISTS mail_groups_name_key_uidx`); err != nil {
			return fmt.Errorf("drop stale mail group name key index: %w", err)
		}
		for _, group := range groups {
			if _, err := s.txExecContext(ctx, tx,
				`UPDATE mail_groups SET name_key = ? WHERE id = ?`, group.nameKey.String, group.id); err != nil {
				return fmt.Errorf("backfill mail group %d name key: %w", group.id, err)
			}
		}
	}
	if _, err := s.txExecContext(ctx, tx,
		`CREATE UNIQUE INDEX IF NOT EXISTS mail_groups_name_key_uidx ON mail_groups(name_key)`); err != nil {
		return fmt.Errorf("create mail group name key unique index: %w", err)
	}
	if s.dialect == dialectSQLite {
		for _, statement := range []string{
			`CREATE TRIGGER IF NOT EXISTS mail_groups_name_key_not_null_insert
				BEFORE INSERT ON mail_groups
				WHEN NEW.name_key IS NULL
				BEGIN
					SELECT RAISE(ABORT, 'mail_groups.name_key must not be NULL');
				END`,
			`CREATE TRIGGER IF NOT EXISTS mail_groups_name_key_not_null_update
				BEFORE UPDATE OF name_key ON mail_groups
				WHEN NEW.name_key IS NULL
				BEGIN
					SELECT RAISE(ABORT, 'mail_groups.name_key must not be NULL');
				END`,
		} {
			if _, err := s.txExecContext(ctx, tx, statement); err != nil {
				return fmt.Errorf("enforce non-null mail group name keys: %w", err)
			}
		}
	}
	return nil
}

func (s *Store) convergePostgresMailGroupSchema(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range postgresMailGroupSchema {
		if _, err := s.txExecContext(ctx, tx, statement); err != nil {
			return err
		}
	}
	if err := s.convergeMailGroupNameKeys(ctx, tx); err != nil {
		return err
	}
	if _, err := s.txExecContext(ctx, tx,
		`ALTER TABLE mail_groups ALTER COLUMN name_key SET NOT NULL`); err != nil {
		return fmt.Errorf("enforce non-null mail group name keys: %w", err)
	}
	return nil
}

var postgresAliasCredentialModeConvergence = []string{
	`ALTER TABLE aliases ADD COLUMN IF NOT EXISTS api_key_prefix TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE aliases ADD COLUMN IF NOT EXISTS credential_ciphertext TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE aliases ADD COLUMN IF NOT EXISTS imap_password_hash BYTEA NOT NULL DEFAULT decode('', 'hex')`,
	`ALTER TABLE aliases ADD COLUMN IF NOT EXISTS oauth_client_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE aliases ADD COLUMN IF NOT EXISTS refresh_token_hash BYTEA NOT NULL DEFAULT decode('', 'hex')`,
	`ALTER TABLE aliases ADD COLUMN IF NOT EXISTS credential_version BIGINT NOT NULL DEFAULT 0`,
	`DO $migration$
	DECLARE
		credential_mode_missing BOOLEAN;
	BEGIN
		SELECT NOT EXISTS(
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = 'aliases'
			  AND column_name = 'credential_mode'
		) INTO credential_mode_missing;
		IF credential_mode_missing THEN
			ALTER TABLE aliases ADD COLUMN credential_mode TEXT NOT NULL DEFAULT 'legacy'
				CHECK(credential_mode IN ('legacy', 'v2'));
			UPDATE aliases
			SET credential_mode = 'v2'
			WHERE credential_version > 0
			  AND length(trim(credential_ciphertext)) > 0
			  AND octet_length(api_key_hash) = 32
			  AND octet_length(imap_password_hash) = 32
			  AND length(trim(oauth_client_id)) > 0
			  AND octet_length(refresh_token_hash) = 32;
		END IF;
	END;
	$migration$`,
	`ALTER TABLE aliases ADD COLUMN IF NOT EXISTS mailbox_uid_validity BIGINT NOT NULL DEFAULT 1
		CHECK(mailbox_uid_validity BETWEEN 1 AND 4294967295)`,
	`ALTER TABLE aliases ADD COLUMN IF NOT EXISTS mailbox_uid_next BIGINT NOT NULL DEFAULT 1
		CHECK(mailbox_uid_next BETWEEN 1 AND 4294967295)`,
}

// Both the main branch and an earlier release candidate used version 5 for
// different table sets. Inspect the actual schema so either variant can
// converge on the complete version 6 schema without replacing existing data.
func sqliteV5CompatibilityMigration(ctx context.Context, tx *sql.Tx) ([]string, error) {
	type tableDefinition struct {
		name      string
		statement string
	}
	groups := []struct {
		name   string
		tables []tableDefinition
	}{
		{
			name: "automatic alias creation",
			tables: []tableDefinition{
				{name: "alias_creation_schedules", statement: sqliteCreateAliasCreationSchedules},
				{name: "pending_alias_api_keys", statement: sqliteCreatePendingAliasAPIKeys},
			},
		},
		{
			name: "message consumption and imap seen",
			tables: []tableDefinition{
				{name: "consumed_messages", statement: sqliteCreateConsumedMessages},
				{name: "imap_seen_tasks", statement: sqliteCreateIMAPSeenTasks},
			},
		},
	}

	statements := make([]string, 0, 2)
	presentGroups := 0
	for _, group := range groups {
		presentTables := 0
		for _, table := range group.tables {
			exists, err := sqliteSchemaObjectExists(ctx, tx, table.name, "table")
			if err != nil {
				return nil, err
			}
			if exists {
				presentTables++
			}
		}
		if presentTables != 0 && presentTables != len(group.tables) {
			return nil, fmt.Errorf(
				"SQLite schema v5 has incomplete %s table group (%d of %d tables)",
				group.name, presentTables, len(group.tables),
			)
		}
		if presentTables == len(group.tables) {
			presentGroups++
			continue
		}
		for _, table := range group.tables {
			statements = append(statements, table.statement)
		}
	}
	if presentGroups == 0 {
		return nil, errors.New("SQLite schema v5 contains neither recognized v5 table group")
	}
	return statements, nil
}

type sqliteV6ColumnRequirement struct {
	name          string
	dataType      string
	requiredCheck string
}

type sqliteV6ForeignKeyRequirement struct {
	from     string
	table    string
	to       string
	onDelete string
}

type sqliteV6TableRequirement struct {
	name       string
	columns    []sqliteV6ColumnRequirement
	primaryKey []string
	foreignKey sqliteV6ForeignKeyRequirement
}

var sqliteV6TableRequirements = []sqliteV6TableRequirement{
	{
		name: "consumed_messages",
		columns: []sqliteV6ColumnRequirement{
			{name: "alias_id", dataType: "INTEGER"},
			{
				name: "uid_validity", dataType: "INTEGER",
				requiredCheck: "CHECK(uid_validity BETWEEN 1 AND 4294967295)",
			},
			{
				name: "uid", dataType: "INTEGER",
				requiredCheck: "CHECK(uid BETWEEN 1 AND 4294967295)",
			},
			{name: "consumed_at", dataType: "INTEGER"},
		},
		primaryKey: []string{"alias_id", "uid_validity", "uid"},
		foreignKey: sqliteV6ForeignKeyRequirement{
			from: "alias_id", table: "aliases", to: "id", onDelete: "CASCADE",
		},
	},
	{
		name: "imap_seen_tasks",
		columns: []sqliteV6ColumnRequirement{
			{name: "account_id", dataType: "INTEGER"},
			{
				name: "uid_validity", dataType: "INTEGER",
				requiredCheck: "CHECK(uid_validity BETWEEN 1 AND 4294967295)",
			},
			{
				name: "uid", dataType: "INTEGER",
				requiredCheck: "CHECK(uid BETWEEN 1 AND 4294967295)",
			},
			{name: "created_at", dataType: "INTEGER"},
		},
		primaryKey: []string{"account_id", "uid_validity", "uid"},
		foreignKey: sqliteV6ForeignKeyRequirement{
			from: "account_id", table: "accounts", to: "id", onDelete: "CASCADE",
		},
	},
}

func (s *Store) convergeSQLiteV6Schema(ctx context.Context, tx *sql.Tx) error {
	for _, requirement := range sqliteV6TableRequirements {
		exists, err := sqliteSchemaObjectExists(ctx, tx, requirement.name, "table")
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("SQLite schema is missing required table %s", requirement.name)
		}
		if err := validateSQLiteV6Table(ctx, tx, requirement); err != nil {
			return err
		}
	}

	const indexName = "imap_seen_tasks_account_created_idx"
	exists, err := sqliteSchemaObjectExists(ctx, tx, indexName, "index")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := s.txExecContext(ctx, tx, sqliteCreateIMAPSeenTasksAccountCreatedIndex); err != nil {
			return fmt.Errorf("create missing SQLite index %s: %w", indexName, err)
		}
	}
	return validateSQLiteV6SeenTaskIndex(ctx, tx)
}

func sqliteSchemaObjectExists(
	ctx context.Context,
	tx *sql.Tx,
	name string,
	wantType string,
) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM sqlite_master
			WHERE type = ? AND name = ? COLLATE NOCASE
		)`, wantType, name,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("inspect SQLite schema object %s: %w", name, err)
	}
	return exists, nil
}

type sqliteV6Column struct {
	dataType string
	notNull  bool
}

func validateSQLiteV6Table(ctx context.Context, tx *sql.Tx, requirement sqliteV6TableRequirement) error {
	var createSQL string
	if err := tx.QueryRowContext(ctx, `
		SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = ? COLLATE NOCASE`, requirement.name,
	).Scan(&createSQL); err != nil {
		return fmt.Errorf("inspect SQLite table %s definition: %w", requirement.name, err)
	}
	normalizedCreateSQL := normalizeSQLiteSchemaSQL(createSQL)

	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+requirement.name+`)`)
	if err != nil {
		return fmt.Errorf("inspect SQLite table %s columns: %w", requirement.name, err)
	}
	defer rows.Close()

	columns := make(map[string]sqliteV6Column)
	primaryKeyColumns := make(map[int]string)
	primaryKeyColumnCount := 0
	for rows.Next() {
		var position, notNull, primaryKeyPosition int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(
			&position, &name, &dataType, &notNull, &defaultValue, &primaryKeyPosition,
		); err != nil {
			return fmt.Errorf("scan SQLite table %s columns: %w", requirement.name, err)
		}
		columns[strings.ToLower(name)] = sqliteV6Column{
			dataType: strings.TrimSpace(dataType),
			notNull:  notNull != 0,
		}
		if primaryKeyPosition > 0 {
			primaryKeyColumnCount++
			primaryKeyColumns[primaryKeyPosition] = name
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate SQLite table %s columns: %w", requirement.name, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close SQLite table %s column metadata: %w", requirement.name, err)
	}

	for _, wanted := range requirement.columns {
		got, ok := columns[strings.ToLower(wanted.name)]
		if !ok {
			return fmt.Errorf("SQLite table %s is missing required column %s", requirement.name, wanted.name)
		}
		if !strings.EqualFold(got.dataType, wanted.dataType) {
			return fmt.Errorf(
				"SQLite table %s column %s has type %s, want %s",
				requirement.name, wanted.name, got.dataType, wanted.dataType,
			)
		}
		if !got.notNull {
			return fmt.Errorf("SQLite table %s column %s must be NOT NULL", requirement.name, wanted.name)
		}
	}
	if len(columns) != len(requirement.columns) {
		return fmt.Errorf(
			"SQLite table %s has %d columns, want exactly %d",
			requirement.name, len(columns), len(requirement.columns),
		)
	}
	for _, wanted := range requirement.columns {
		if wanted.requiredCheck != "" &&
			!strings.Contains(normalizedCreateSQL, normalizeSQLiteSchemaSQL(wanted.requiredCheck)) {
			return fmt.Errorf(
				"SQLite table %s column %s is missing required %s",
				requirement.name, wanted.name, wanted.requiredCheck,
			)
		}
	}

	if primaryKeyColumnCount != len(requirement.primaryKey) || len(primaryKeyColumns) != len(requirement.primaryKey) {
		return fmt.Errorf(
			"SQLite table %s primary key has %d columns, want (%s)",
			requirement.name, primaryKeyColumnCount, strings.Join(requirement.primaryKey, ", "),
		)
	}
	for position, wanted := range requirement.primaryKey {
		got, ok := primaryKeyColumns[position+1]
		if !ok || !strings.EqualFold(got, wanted) {
			return fmt.Errorf(
				"SQLite table %s primary key position %d is %q, want %q",
				requirement.name, position+1, got, wanted,
			)
		}
	}

	return validateSQLiteV6ForeignKey(ctx, tx, requirement)
}

func normalizeSQLiteSchemaSQL(statement string) string {
	var normalized strings.Builder
	normalized.Grow(len(statement))
	for position := 0; position < len(statement); {
		character := statement[position]
		switch {
		case character == '\'' || character == '"' || character == '`':
			normalized.WriteByte(0)
			position = skipSQLiteQuotedSQL(statement, position, character)
		case character == '[':
			normalized.WriteByte(0)
			position++
			for position < len(statement) && statement[position] != ']' {
				position++
			}
			if position < len(statement) {
				position++
			}
		case character == '-' && position+1 < len(statement) && statement[position+1] == '-':
			normalized.WriteByte(0)
			position += 2
			for position < len(statement) && statement[position] != '\n' && statement[position] != '\r' {
				position++
			}
		case character == '/' && position+1 < len(statement) && statement[position+1] == '*':
			normalized.WriteByte(0)
			position += 2
			for position+1 < len(statement) &&
				(statement[position] != '*' || statement[position+1] != '/') {
				position++
			}
			if position+1 < len(statement) {
				position += 2
			} else {
				position = len(statement)
			}
		case isSQLiteSQLWhitespace(character):
			position++
		default:
			if character >= 'A' && character <= 'Z' {
				character += 'a' - 'A'
			}
			normalized.WriteByte(character)
			position++
		}
	}
	return normalized.String()
}

func skipSQLiteQuotedSQL(statement string, position int, delimiter byte) int {
	position++
	for position < len(statement) {
		if statement[position] != delimiter {
			position++
			continue
		}
		if position+1 < len(statement) && statement[position+1] == delimiter {
			position += 2
			continue
		}
		return position + 1
	}
	return position
}

func isSQLiteSQLWhitespace(character byte) bool {
	switch character {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

type sqliteV6ForeignKey struct {
	from     string
	table    string
	to       string
	onDelete string
}

func validateSQLiteV6ForeignKey(ctx context.Context, tx *sql.Tx, requirement sqliteV6TableRequirement) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_list(`+requirement.name+`)`)
	if err != nil {
		return fmt.Errorf("inspect SQLite table %s foreign keys: %w", requirement.name, err)
	}
	defer rows.Close()

	var foreignKeys []sqliteV6ForeignKey
	for rows.Next() {
		var id, sequence int
		var foreignKey sqliteV6ForeignKey
		var onUpdate, match string
		if err := rows.Scan(
			&id, &sequence, &foreignKey.table, &foreignKey.from, &foreignKey.to,
			&onUpdate, &foreignKey.onDelete, &match,
		); err != nil {
			return fmt.Errorf("scan SQLite table %s foreign keys: %w", requirement.name, err)
		}
		foreignKeys = append(foreignKeys, foreignKey)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate SQLite table %s foreign keys: %w", requirement.name, err)
	}

	wanted := requirement.foreignKey
	if len(foreignKeys) != 1 {
		return fmt.Errorf("SQLite table %s has %d foreign key entries, want 1", requirement.name, len(foreignKeys))
	}
	got := foreignKeys[0]
	if !strings.EqualFold(got.from, wanted.from) ||
		!strings.EqualFold(got.table, wanted.table) ||
		!strings.EqualFold(got.to, wanted.to) ||
		!strings.EqualFold(got.onDelete, wanted.onDelete) {
		return fmt.Errorf(
			"SQLite table %s foreign key is %s -> %s(%s) ON DELETE %s, want %s -> %s(%s) ON DELETE %s",
			requirement.name,
			got.from, got.table, got.to, got.onDelete,
			wanted.from, wanted.table, wanted.to, wanted.onDelete,
		)
	}
	return nil
}

func validateSQLiteV6SeenTaskIndex(ctx context.Context, tx *sql.Tx) error {
	const (
		indexName = "imap_seen_tasks_account_created_idx"
		tableName = "imap_seen_tasks"
	)
	wantedColumns := []string{"account_id", "created_at", "uid_validity", "uid"}

	var indexedTable string
	if err := tx.QueryRowContext(ctx, `
		SELECT tbl_name FROM sqlite_master
		WHERE type = 'index' AND name = ? COLLATE NOCASE`, indexName,
	).Scan(&indexedTable); err != nil {
		return fmt.Errorf("inspect SQLite index %s owner: %w", indexName, err)
	}
	if !strings.EqualFold(indexedTable, tableName) {
		return fmt.Errorf("SQLite index %s belongs to table %s, want %s", indexName, indexedTable, tableName)
	}

	rows, err := tx.QueryContext(ctx, `PRAGMA index_list(`+tableName+`)`)
	if err != nil {
		return fmt.Errorf("inspect SQLite table %s indexes: %w", tableName, err)
	}
	var found bool
	for rows.Next() {
		var sequence, unique, partial int
		var name, origin string
		if err := rows.Scan(&sequence, &name, &unique, &origin, &partial); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan SQLite table %s indexes: %w", tableName, err)
		}
		if !strings.EqualFold(name, indexName) {
			continue
		}
		found = true
		if unique != 0 {
			_ = rows.Close()
			return fmt.Errorf("SQLite index %s must be non-unique", indexName)
		}
		if !strings.EqualFold(origin, "c") {
			_ = rows.Close()
			return fmt.Errorf("SQLite index %s has origin %s, want created index", indexName, origin)
		}
		if partial != 0 {
			_ = rows.Close()
			return fmt.Errorf("SQLite index %s must not be partial", indexName)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close SQLite table %s index metadata: %w", tableName, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate SQLite table %s indexes: %w", tableName, err)
	}
	if !found {
		return fmt.Errorf("SQLite table %s is missing index %s", tableName, indexName)
	}

	rows, err = tx.QueryContext(ctx, `PRAGMA index_xinfo(`+indexName+`)`)
	if err != nil {
		return fmt.Errorf("inspect SQLite index %s columns: %w", indexName, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var sequence, columnID, descending, key int
		var name, collation sql.NullString
		if err := rows.Scan(&sequence, &columnID, &name, &descending, &collation, &key); err != nil {
			return fmt.Errorf("scan SQLite index %s columns: %w", indexName, err)
		}
		if key == 0 {
			continue
		}
		if columnID < 0 || !name.Valid {
			return fmt.Errorf("SQLite index %s contains an expression at position %d", indexName, sequence+1)
		}
		if descending != 0 {
			return fmt.Errorf("SQLite index %s column %s must use ascending order", indexName, name.String)
		}
		columns = append(columns, name.String)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate SQLite index %s columns: %w", indexName, err)
	}
	if len(columns) != len(wantedColumns) {
		return fmt.Errorf(
			"SQLite index %s has columns (%s), want (%s)",
			indexName, strings.Join(columns, ", "), strings.Join(wantedColumns, ", "),
		)
	}
	for position, wanted := range wantedColumns {
		if !strings.EqualFold(columns[position], wanted) {
			return fmt.Errorf(
				"SQLite index %s column position %d is %q, want %q",
				indexName, position+1, columns[position], wanted,
			)
		}
	}
	return nil
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

var postgresMigrateV5ToV6 = []string{
	`CREATE TABLE consumed_messages (
		alias_id BIGINT NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
		uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid BIGINT NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		consumed_at BIGINT NOT NULL,
		PRIMARY KEY(alias_id, uid_validity, uid)
	)`,
	`CREATE TABLE imap_seen_tasks (
		account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid BIGINT NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		created_at BIGINT NOT NULL,
		PRIMARY KEY(account_id, uid_validity, uid)
	)`,
	`CREATE INDEX imap_seen_tasks_account_created_idx
		ON imap_seen_tasks(account_id, created_at, uid_validity, uid)`,
}

var postgresLegacyCompatibilityRepair = []string{
	`CREATE TABLE IF NOT EXISTS latest_messages (
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
	`CREATE TABLE IF NOT EXISTS pending_alias_api_keys (
		alias_id BIGINT PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
		api_key_ciphertext TEXT NOT NULL CHECK(length(trim(api_key_ciphertext)) > 0),
		created_at BIGINT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS consumed_messages (
		alias_id BIGINT NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
		uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid BIGINT NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		consumed_at BIGINT NOT NULL,
		PRIMARY KEY(alias_id, uid_validity, uid)
	)`,
	`CREATE TABLE IF NOT EXISTS imap_seen_tasks (
		account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid BIGINT NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		created_at BIGINT NOT NULL,
		PRIMARY KEY(account_id, uid_validity, uid)
	)`,
	`CREATE INDEX IF NOT EXISTS imap_seen_tasks_account_created_idx
		ON imap_seen_tasks(account_id, created_at, uid_validity, uid)`,
}

var postgresSchemaV6 = append(append([]string{}, postgresSchemaV5...), postgresMigrateV5ToV6...)

var postgresMigrateV6ToV7 = []string{
	`ALTER TABLE aliases ADD COLUMN credential_ciphertext TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE aliases ADD COLUMN imap_password_hash BYTEA NOT NULL DEFAULT decode('', 'hex')`,
	`ALTER TABLE aliases ADD COLUMN oauth_client_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE aliases ADD COLUMN refresh_token_hash BYTEA NOT NULL DEFAULT decode('', 'hex')`,
	`ALTER TABLE aliases ADD COLUMN credential_version BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE aliases ADD COLUMN credential_mode TEXT NOT NULL DEFAULT 'legacy'
		CHECK(credential_mode IN ('legacy', 'v2'))`,
	`ALTER TABLE aliases ADD COLUMN mailbox_uid_validity BIGINT NOT NULL DEFAULT 1
		CHECK(mailbox_uid_validity BETWEEN 1 AND 4294967295)`,
	`ALTER TABLE aliases ADD COLUMN mailbox_uid_next BIGINT NOT NULL DEFAULT 1
		CHECK(mailbox_uid_next BETWEEN 1 AND 4294967295)`,
	`CREATE UNIQUE INDEX aliases_oauth_client_id_idx
		ON aliases(oauth_client_id) WHERE oauth_client_id <> ''`,
	`CREATE INDEX aliases_imap_password_hash_idx ON aliases(imap_password_hash)`,
	`CREATE TABLE archived_messages (
		id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
		account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		upstream_uid BIGINT NOT NULL CHECK(upstream_uid BETWEEN 1 AND 4294967295),
		message_id TEXT NOT NULL DEFAULT '',
		internal_date BIGINT NOT NULL,
		header_date BIGINT,
		from_json TEXT NOT NULL DEFAULT '[]',
		to_json TEXT NOT NULL DEFAULT '[]',
		cc_json TEXT NOT NULL DEFAULT '[]',
		subject TEXT NOT NULL DEFAULT '',
		content_path TEXT NOT NULL DEFAULT '',
		content_bytes BIGINT NOT NULL DEFAULT 0 CHECK(content_bytes >= 0),
		content_sha256 TEXT NOT NULL DEFAULT '',
		content_state TEXT NOT NULL DEFAULT 'metadata_only',
		body_truncated BOOLEAN NOT NULL DEFAULT FALSE,
		synced_at BIGINT NOT NULL,
		created_at BIGINT NOT NULL,
		UNIQUE(account_id, uid_validity, upstream_uid)
	)`,
	`CREATE INDEX archived_messages_retention_idx
		ON archived_messages(content_state, internal_date, id)`,
	`CREATE TABLE alias_messages (
		alias_id BIGINT NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
		message_id BIGINT NOT NULL REFERENCES archived_messages(id) ON DELETE CASCADE,
		mailbox_uid BIGINT NOT NULL CHECK(mailbox_uid BETWEEN 1 AND 4294967295),
		otp TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL,
		PRIMARY KEY(alias_id, message_id),
		UNIQUE(alias_id, mailbox_uid)
	)`,
	`CREATE INDEX alias_messages_alias_uid_idx
		ON alias_messages(alias_id, mailbox_uid DESC)`,
	`CREATE INDEX alias_messages_alias_otp_idx
		ON alias_messages(alias_id, created_at DESC, message_id DESC) WHERE otp <> ''`,
	`INSERT INTO archived_messages(
		account_id, uid_validity, upstream_uid, message_id, internal_date, header_date,
		from_json, to_json, cc_json, subject, content_path, content_bytes,
		content_sha256, content_state, body_truncated, synced_at, created_at
	)
	SELECT DISTINCT ON (al.account_id, lm.uid_validity, lm.uid)
		al.account_id, lm.uid_validity, lm.uid, lm.message_id, lm.internal_date,
		lm.header_date, lm.from_json, lm.to_json, lm.cc_json, lm.subject,
		'', 0, '', 'metadata_only', FALSE, lm.synced_at, lm.synced_at
	FROM latest_messages lm
	JOIN aliases al ON al.id = lm.alias_id
	ORDER BY al.account_id, lm.uid_validity, lm.uid, lm.alias_id
	ON CONFLICT(account_id, uid_validity, upstream_uid) DO NOTHING`,
	`INSERT INTO alias_messages(alias_id, message_id, mailbox_uid, otp, created_at)
	SELECT lm.alias_id, archived.id, 1, '', lm.internal_date
	FROM latest_messages lm
	JOIN aliases al ON al.id = lm.alias_id
	JOIN archived_messages archived
	  ON archived.account_id = al.account_id
	 AND archived.uid_validity = lm.uid_validity
	 AND archived.upstream_uid = lm.uid
	ON CONFLICT(alias_id, message_id) DO NOTHING`,
	`UPDATE aliases SET mailbox_uid_next = 2
		WHERE id IN (SELECT alias_id FROM alias_messages)`,
}

var postgresSchemaV7 = append(append([]string{}, postgresSchemaV6...), postgresMigrateV6ToV7...)

var postgresMigrateV7ToV8 = []string{
	`CREATE TABLE IF NOT EXISTS account_mailbox_settings (
		account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		mailbox_type TEXT NOT NULL DEFAULT 'icloud'
			CHECK(mailbox_type IN ('icloud', 'custom')),
		email_suffix TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS account_mailbox_settings_type_idx
		ON account_mailbox_settings(mailbox_type, account_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS account_mailbox_settings_custom_suffix_idx
		ON account_mailbox_settings(LOWER(email_suffix))
		WHERE mailbox_type = 'custom'`,
}

var postgresSchemaV8 = append(append([]string{}, postgresSchemaV7...), postgresMigrateV7ToV8...)

var postgresMailGroupSchema = []string{
	`CREATE TABLE IF NOT EXISTS mail_groups (
		id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
		name TEXT NOT NULL,
		name_key TEXT NOT NULL,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL
	)`,
	`ALTER TABLE mail_groups ADD COLUMN IF NOT EXISTS name_key TEXT`,
	// Older builds used a CITEXT UNIQUE column. Its generated constraint would
	// otherwise retain PostgreSQL/locale-specific comparison semantics.
	`ALTER TABLE mail_groups DROP CONSTRAINT IF EXISTS mail_groups_name_key`,
	`ALTER TABLE aliases ADD COLUMN IF NOT EXISTS group_id BIGINT REFERENCES mail_groups(id) ON DELETE SET NULL`,
	`CREATE INDEX IF NOT EXISTS aliases_group_id_idx ON aliases(group_id, address, id)`,
}

var postgresSchemaV8WithMailGroups = append(append([]string{}, postgresSchemaV8...), postgresMailGroupSchema...)

// Version 5 was released with two distinct feature-table layouts. These
// idempotent statements preserve either layout while filling its missing half.
var postgresV5CompatibilityMigration = []string{
	`DO $migration$
	DECLARE
		automatic_alias_table_count INTEGER :=
			(CASE WHEN pg_catalog.to_regclass('alias_creation_schedules') IS NULL THEN 0 ELSE 1 END) +
			(CASE WHEN pg_catalog.to_regclass('pending_alias_api_keys') IS NULL THEN 0 ELSE 1 END);
		seen_table_count INTEGER :=
			(CASE WHEN pg_catalog.to_regclass('consumed_messages') IS NULL THEN 0 ELSE 1 END) +
			(CASE WHEN pg_catalog.to_regclass('imap_seen_tasks') IS NULL THEN 0 ELSE 1 END);
	BEGIN
		IF automatic_alias_table_count NOT IN (0, 2) THEN
			RAISE EXCEPTION 'PostgreSQL schema v5 has incomplete automatic alias table group (% of 2 tables)',
				automatic_alias_table_count;
		END IF;
		IF seen_table_count NOT IN (0, 2) THEN
			RAISE EXCEPTION 'PostgreSQL schema v5 has incomplete message consumption and IMAP Seen table group (% of 2 tables)',
				seen_table_count;
		END IF;
		IF automatic_alias_table_count = 0 AND seen_table_count = 0 THEN
			RAISE EXCEPTION 'PostgreSQL schema v5 contains neither recognized v5 table group';
		END IF;
	END;
	$migration$`,
	`CREATE TABLE IF NOT EXISTS alias_creation_schedules (
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
	`CREATE TABLE IF NOT EXISTS pending_alias_api_keys (
		alias_id BIGINT PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
		api_key_ciphertext TEXT NOT NULL CHECK(length(trim(api_key_ciphertext)) > 0),
		created_at BIGINT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS consumed_messages (
		alias_id BIGINT NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
		uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid BIGINT NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		consumed_at BIGINT NOT NULL,
		PRIMARY KEY(alias_id, uid_validity, uid)
	)`,
	`CREATE TABLE IF NOT EXISTS imap_seen_tasks (
		account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid BIGINT NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		created_at BIGINT NOT NULL,
		PRIMARY KEY(account_id, uid_validity, uid)
	)`,
}

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
	// See the SQLite convergence note above. This side table is intentionally
	// created with IF NOT EXISTS so convergence can repair an interrupted v8
	// migration without a destructive accounts-table rewrite.
	`CREATE TABLE IF NOT EXISTS account_mailbox_settings (
		account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		mailbox_type TEXT NOT NULL DEFAULT 'icloud'
			CHECK(mailbox_type IN ('icloud', 'custom')),
		email_suffix TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS account_mailbox_settings_type_idx
		ON account_mailbox_settings(mailbox_type, account_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS account_mailbox_settings_custom_suffix_idx
		ON account_mailbox_settings(LOWER(email_suffix))
		WHERE mailbox_type = 'custom'`,
	`CREATE TABLE IF NOT EXISTS latest_messages (
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
	`CREATE TABLE IF NOT EXISTS pending_alias_api_keys (
		alias_id BIGINT PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
		api_key_ciphertext TEXT NOT NULL CHECK(length(trim(api_key_ciphertext)) > 0),
		created_at BIGINT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS consumed_messages (
		alias_id BIGINT NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
		uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid BIGINT NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		consumed_at BIGINT NOT NULL,
		PRIMARY KEY(alias_id, uid_validity, uid)
	)`,
	`CREATE TABLE IF NOT EXISTS imap_seen_tasks (
		account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		uid_validity BIGINT NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
		uid BIGINT NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
		created_at BIGINT NOT NULL,
		PRIMARY KEY(account_id, uid_validity, uid)
	)`,
	`CREATE INDEX IF NOT EXISTS imap_seen_tasks_account_created_idx
		ON imap_seen_tasks(account_id, created_at, uid_validity, uid)`,
	`CREATE INDEX IF NOT EXISTS admin_sessions_admin_id_idx ON admin_sessions(admin_id)`,
	`CREATE INDEX IF NOT EXISTS accounts_enabled_email_idx ON accounts(email, id) WHERE enabled`,
	`CREATE INDEX IF NOT EXISTS aliases_account_address_idx ON aliases(account_id, address, id)`,
	`CREATE INDEX IF NOT EXISTS aliases_enabled_account_address_idx ON aliases(account_id, address, id) WHERE enabled`,
	`CREATE INDEX IF NOT EXISTS alias_creation_schedules_due_idx ON alias_creation_schedules(enabled, next_run_at, account_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS aliases_oauth_client_id_idx
		ON aliases(oauth_client_id) WHERE oauth_client_id <> ''`,
	`CREATE INDEX IF NOT EXISTS aliases_imap_password_hash_idx ON aliases(imap_password_hash)`,
	`CREATE INDEX IF NOT EXISTS archived_messages_retention_idx
		ON archived_messages(content_state, internal_date, id)`,
	`CREATE INDEX IF NOT EXISTS alias_messages_alias_uid_idx
		ON alias_messages(alias_id, mailbox_uid DESC)`,
	`CREATE INDEX IF NOT EXISTS alias_messages_alias_otp_idx
		ON alias_messages(alias_id, created_at DESC, message_id DESC) WHERE otp <> ''`,
}
