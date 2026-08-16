package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestSQLiteFreshSchemaV8IncludesMailboxAndLegacyCompatibilityTables(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "fresh-v8.db"))
	if err != nil {
		t.Fatalf("create fresh v8 database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version int
	if err := db.DB().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read fresh schema version: %v", err)
	}
	if version != 8 {
		t.Fatalf("fresh schema version = %d, want 8", version)
	}

	for _, table := range []string{
		"archived_messages", "alias_messages", "alias_creation_schedules",
		"latest_messages", "pending_alias_api_keys", "consumed_messages", "imap_seen_tasks",
		"account_mailbox_settings",
	} {
		if !sqliteObjectExists(t, ctx, db.DB(), "table", table) {
			t.Errorf("fresh v8 schema is missing table %s", table)
		}
	}
	if !sqliteObjectExists(t, ctx, db.DB(), "index", "account_mailbox_settings_custom_suffix_idx") {
		t.Error("fresh v8 schema is missing the custom mailbox suffix unique index")
	}

	columns := sqliteTableColumns(t, ctx, db.DB(), "aliases")
	for _, column := range []string{
		"api_key_prefix", "credential_ciphertext", "credential_mode", "imap_password_hash", "oauth_client_id",
		"refresh_token_hash", "credential_version", "mailbox_uid_validity", "mailbox_uid_next",
	} {
		if !columns[column] {
			t.Errorf("fresh v8 aliases table is missing column %s", column)
		}
	}
}

func TestSQLiteV7ConvergenceRepairsLegacyTablesAndCredentialMode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "partially-migrated-v7.db")
	current, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("create v7 database: %v", err)
	}
	if err := current.Close(); err != nil {
		t.Fatalf("close v7 database before damage: %v", err)
	}

	damaged, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open v7 database for compatibility damage: %v", err)
	}
	for _, statement := range []string{
		`DROP INDEX imap_seen_tasks_account_created_idx`,
		`DROP TABLE latest_messages`,
		`DROP TABLE pending_alias_api_keys`,
		`DROP TABLE consumed_messages`,
		`DROP TABLE imap_seen_tasks`,
		`ALTER TABLE aliases DROP COLUMN credential_mode`,
	} {
		if _, err := damaged.ExecContext(ctx, statement); err != nil {
			_ = damaged.Close()
			t.Fatalf("damage v7 schema with %q: %v", statement, err)
		}
	}
	if err := damaged.Close(); err != nil {
		t.Fatalf("close damaged v7 database: %v", err)
	}

	repaired, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("repair partially migrated v7 database: %v", err)
	}
	t.Cleanup(func() { _ = repaired.Close() })

	for _, table := range []string{
		"latest_messages", "pending_alias_api_keys", "consumed_messages", "imap_seen_tasks",
	} {
		if !sqliteObjectExists(t, ctx, repaired.DB(), "table", table) {
			t.Errorf("repaired v7 database is missing compatibility table %s", table)
		}
	}
	if !sqliteObjectExists(t, ctx, repaired.DB(), "index", "imap_seen_tasks_account_created_idx") {
		t.Fatal("repaired v7 database is missing the IMAP Seen task index")
	}
	columns := sqliteTableColumns(t, ctx, repaired.DB(), "aliases")
	if !columns["credential_mode"] {
		t.Fatal("repaired v7 aliases table is missing credential_mode")
	}
}

func TestSQLiteV7ConvergenceClassifiesOnlyCompletePreModeCredentials(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "pre-mode-v7.db")
	db, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("create v7 database: %v", err)
	}
	account := createAccount(t, ctx, db, "Pre-mode credentials", "pre-mode@icloud.com")
	complete := createAlias(t, ctx, db, account.ID, "complete@icloud.com", []byte(strings.Repeat("a", 32)))
	incomplete := createAlias(t, ctx, db, account.ID, "incomplete@icloud.com", []byte(strings.Repeat("b", 32)))
	alreadyLegacy := createAlias(t, ctx, db, account.ID, "existing-mode@icloud.com", []byte(strings.Repeat("c", 32)))
	for index, id := range []int64{complete.ID, alreadyLegacy.ID} {
		if _, err := db.DB().ExecContext(ctx, `
			UPDATE aliases SET credential_ciphertext = 'mc1.complete', credential_version = 1,
				imap_password_hash = ?, oauth_client_id = ?, refresh_token_hash = ?
			WHERE id = ?`, []byte(strings.Repeat("i", 32)), fmt.Sprintf("icl_complete_credential_%d", index), []byte(strings.Repeat("r", 32)), id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB().ExecContext(ctx, `
		UPDATE aliases SET credential_ciphertext = 'mc1.incomplete', credential_version = 1,
			imap_password_hash = ?, oauth_client_id = '', refresh_token_hash = ?
		WHERE id = ?`, []byte(strings.Repeat("i", 32)), []byte(strings.Repeat("r", 32)), incomplete.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `ALTER TABLE aliases DROP COLUMN credential_mode`); err != nil {
		_ = raw.Close()
		t.Fatalf("remove pre-mode column: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("converge pre-mode v7 database: %v", err)
	}
	defer reopened.Close()
	for _, test := range []struct {
		id   int64
		want string
	}{
		{complete.ID, domain.AliasCredentialModeV2},
		{incomplete.ID, domain.AliasCredentialModeLegacy},
		{alreadyLegacy.ID, domain.AliasCredentialModeV2},
	} {
		got, err := reopened.GetAlias(ctx, test.id)
		if err != nil {
			t.Fatal(err)
		}
		if got.CredentialMode != test.want {
			t.Errorf("alias %d mode = %q, want %q", test.id, got.CredentialMode, test.want)
		}
	}

	if _, err := reopened.DB().ExecContext(ctx, `UPDATE aliases SET credential_mode = 'legacy' WHERE id = ?`, alreadyLegacy.ID); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err = store.Open(databasePath)
	if err != nil {
		t.Fatalf("repeat convergence: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.GetAlias(ctx, alreadyLegacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CredentialMode != domain.AliasCredentialModeLegacy {
		t.Fatalf("existing credential mode was reinterpreted as %q", got.CredentialMode)
	}
}

func TestSQLiteV7ConvergenceDoesNotInferV2FromStaleCredentialFields(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "stale-credential-fields-v7.db")
	db, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("create v7 database: %v", err)
	}
	account := createAccount(t, ctx, db, "Stale credential fields", "stale-fields@icloud.com")
	legacyHash := []byte(strings.Repeat("l", 32))
	alias, err := db.CreateAlias(ctx, domain.Alias{
		AccountID:      account.ID,
		Address:        "stale-fields-alias@icloud.com",
		APIKeyHash:     legacyHash,
		APIKeyPrefix:   "legacy-prefix",
		CredentialMode: domain.AliasCredentialModeLegacy,
		Enabled:        true,
	})
	if err != nil {
		_ = db.Close()
		t.Fatalf("create legacy alias: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		UPDATE aliases
		SET credential_ciphertext = 'stale-ciphertext', credential_version = 7,
			oauth_client_id = 'stale-client-id', imap_password_hash = ?, refresh_token_hash = ?
		WHERE id = ?`, []byte(strings.Repeat("i", 32)), []byte(strings.Repeat("r", 32)), alias.ID); err != nil {
		_ = db.Close()
		t.Fatalf("seed stale credential fields: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v7 database before restart: %v", err)
	}

	reopened, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("reopen v7 database: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("read alias after convergence: %v", err)
	}
	if got.CredentialMode != domain.AliasCredentialModeLegacy {
		t.Fatalf("stale credential fields changed mode to %q, want legacy", got.CredentialMode)
	}
	var issuerCalls int
	reopened.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		issuerCalls++
		return domain.AliasCredentialMaterial{}, errors.New("legacy alias must not be issued v2 credentials")
	})
	if err := reopened.EnsureAliasCredentials(ctx); err != nil {
		t.Fatalf("initialize credentials after convergence: %v", err)
	}
	if issuerCalls != 0 {
		t.Fatalf("legacy alias triggered credential issuance %d times", issuerCalls)
	}
}

func TestMigrateV6ToV7PreservesSharedSnapshotAndLegacyState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-v6.db")
	legacy := createSQLiteV6Fixture(t, databasePath)
	fixtures := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO accounts(
			id, name, email, imap_host, imap_port, imap_username, password_ciphertext,
			enabled, last_sync_status, last_sync_error, created_at, updated_at
		) VALUES(1, 'Legacy v6', 'legacy-v6@icloud.com', 'imap.mail.me.com', 993,
			'legacy-v6@icloud.com', 'ciphertext', 1, 'ok', '', 1, 1)`, nil},
		{`INSERT INTO aliases(
			id, account_id, address, label, api_key_hash, api_key_prefix, enabled,
			last_sync_status, last_sync_error, created_at, updated_at
		) VALUES(1, 1, 'first@icloud.com', '', ?, 'old-first', 1, 'ok', '', 1, 1)`, []any{[]byte("old-first-hash")}},
		{`INSERT INTO aliases(
			id, account_id, address, label, api_key_hash, api_key_prefix, enabled,
			last_sync_status, last_sync_error, created_at, updated_at
		) VALUES(2, 1, 'second@icloud.com', '', ?, 'old-second', 1, 'ok', '', 1, 1)`, []any{[]byte("old-second-hash")}},
		{`INSERT INTO latest_messages(
			alias_id, uid_validity, uid, message_id, internal_date, header_date,
			from_json, to_json, cc_json, subject, text_body, html_body,
			attachments_json, body_truncated, synced_at
		) VALUES(1, 44, 55, '<shared@example.test>', 100, 90,
			'[{"email":"sender@example.test"}]', '[{"email":"first@icloud.com"}]', '[]',
			'Shared legacy snapshot', 'discarded body', '', '[]', 1, 110)`, nil},
		{`INSERT INTO latest_messages(
			alias_id, uid_validity, uid, message_id, internal_date, header_date,
			from_json, to_json, cc_json, subject, text_body, html_body,
			attachments_json, body_truncated, synced_at
		) VALUES(2, 44, 55, '<shared@example.test>', 100, 90,
			'[{"email":"sender@example.test"}]', '[{"email":"second@icloud.com"}]', '[]',
			'Shared legacy snapshot', 'discarded body', '', '[]', 0, 111)`, nil},
		{`INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
			VALUES(1, 'legacy-key', 1)`, nil},
		{`INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at)
			VALUES(1, 44, 55, 1)`, nil},
		{`INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at)
			VALUES(1, 44, 55, 1)`, nil},
	}
	for _, fixture := range fixtures {
		if _, err := legacy.ExecContext(ctx, fixture.query, fixture.args...); err != nil {
			_ = legacy.Close()
			t.Fatalf("seed v6 fixture: %v", err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close v6 fixture: %v", err)
	}

	migrated, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("migrate v6 database: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	var version, archivedCount, routedCount int
	if err := migrated.DB().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read migrated schema version: %v", err)
	}
	if version != 8 {
		t.Fatalf("migrated schema version = %d, want 8", version)
	}
	if err := migrated.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM archived_messages`).Scan(&archivedCount); err != nil {
		t.Fatalf("count migrated archive messages: %v", err)
	}
	if err := migrated.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM alias_messages`).Scan(&routedCount); err != nil {
		t.Fatalf("count migrated alias mappings: %v", err)
	}
	if archivedCount != 1 || routedCount != 2 {
		t.Fatalf("migrated shared snapshot counts = archive:%d mappings:%d, want 1 and 2", archivedCount, routedCount)
	}

	first := oneArchivedMailboxMessage(t, ctx, migrated, 1)
	second := oneArchivedMailboxMessage(t, ctx, migrated, 2)
	if first.ID != second.ID || first.MailboxUID != 1 || second.MailboxUID != 1 {
		t.Fatalf("migrated local identities = first:%#v second:%#v", first, second)
	}
	if first.Subject != "Shared legacy snapshot" || first.MessageID != "<shared@example.test>" ||
		first.ContentState != domain.ArchiveContentMetadata || first.ContentPath != "" ||
		len(first.From) != 1 || first.From[0].Email != "sender@example.test" {
		t.Fatalf("migrated metadata message = %#v", first)
	}
	var firstNext, secondNext int64
	if err := migrated.DB().QueryRowContext(ctx,
		`SELECT mailbox_uid_next FROM aliases WHERE id = 1`).Scan(&firstNext); err != nil {
		t.Fatal(err)
	}
	if err := migrated.DB().QueryRowContext(ctx,
		`SELECT mailbox_uid_next FROM aliases WHERE id = 2`).Scan(&secondNext); err != nil {
		t.Fatal(err)
	}
	if firstNext != 2 || secondNext != 2 {
		t.Fatalf("migrated next local UIDs = (%d, %d), want (2, 2)", firstNext, secondNext)
	}

	for _, table := range []string{"latest_messages", "pending_alias_api_keys", "consumed_messages", "imap_seen_tasks"} {
		if !sqliteObjectExists(t, ctx, migrated.DB(), "table", table) {
			t.Errorf("v6 migration removed compatibility table %s", table)
		}
	}

	var firstPrefix, firstMode, secondPrefix, secondMode string
	if err := migrated.DB().QueryRowContext(ctx,
		`SELECT api_key_prefix, credential_mode FROM aliases WHERE id = 1`,
	).Scan(&firstPrefix, &firstMode); err != nil {
		t.Fatalf("read first migrated alias credentials: %v", err)
	}
	if err := migrated.DB().QueryRowContext(ctx,
		`SELECT api_key_prefix, credential_mode FROM aliases WHERE id = 2`,
	).Scan(&secondPrefix, &secondMode); err != nil {
		t.Fatalf("read second migrated alias credentials: %v", err)
	}
	if firstPrefix != "old-first" || secondPrefix != "old-second" ||
		firstMode != domain.AliasCredentialModeLegacy || secondMode != domain.AliasCredentialModeLegacy {
		t.Fatalf("migrated alias credentials = (%q, %q), (%q, %q), want preserved prefixes in legacy mode",
			firstPrefix, firstMode, secondPrefix, secondMode)
	}

	var latestText string
	var latestTruncated, latestSyncedAt int64
	if err := migrated.DB().QueryRowContext(ctx, `
		SELECT text_body, body_truncated, synced_at
		FROM latest_messages
		WHERE alias_id = 1 AND uid_validity = 44 AND uid = 55`,
	).Scan(&latestText, &latestTruncated, &latestSyncedAt); err != nil {
		t.Fatalf("read preserved latest message: %v", err)
	}
	if latestText != "discarded body" || latestTruncated != 1 || latestSyncedAt != 110 {
		t.Fatalf("preserved latest message state = (%q, %d, %d), want (%q, 1, 110)",
			latestText, latestTruncated, latestSyncedAt, "discarded body")
	}

	var pendingKey string
	var pendingCreatedAt int64
	if err := migrated.DB().QueryRowContext(ctx, `
		SELECT api_key_ciphertext, created_at FROM pending_alias_api_keys WHERE alias_id = 1`,
	).Scan(&pendingKey, &pendingCreatedAt); err != nil {
		t.Fatalf("read preserved pending API key: %v", err)
	}
	if pendingKey != "legacy-key" || pendingCreatedAt != 1 {
		t.Fatalf("preserved pending API key = (%q, %d), want (%q, 1)", pendingKey, pendingCreatedAt, "legacy-key")
	}

	for _, state := range []struct {
		name  string
		query string
	}{
		{
			name: "consumed message",
			query: `SELECT COUNT(*) FROM consumed_messages
				WHERE alias_id = 1 AND uid_validity = 44 AND uid = 55 AND consumed_at = 1`,
		},
		{
			name: "IMAP Seen task",
			query: `SELECT COUNT(*) FROM imap_seen_tasks
				WHERE account_id = 1 AND uid_validity = 44 AND uid = 55 AND created_at = 1`,
		},
	} {
		var count int
		if err := migrated.DB().QueryRowContext(ctx, state.query).Scan(&count); err != nil {
			t.Fatalf("read preserved %s: %v", state.name, err)
		}
		if count != 1 {
			t.Errorf("preserved %s count = %d, want 1", state.name, count)
		}
	}
}

func TestMigrateHistoricalV5LayoutsToV7(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		drop []string
	}{
		{name: "automatic alias tables only", drop: []string{"imap_seen_tasks", "consumed_messages"}},
		{name: "seen tables only", drop: []string{"pending_alias_api_keys", "alias_creation_schedules"}},
		{name: "both table groups"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			databasePath := filepath.Join(t.TempDir(), "historical-v5.db")
			legacy := createSQLiteV6Fixture(t, databasePath)
			if _, err := legacy.ExecContext(ctx, `INSERT INTO accounts(
				id, name, email, imap_host, imap_port, imap_username, password_ciphertext,
				enabled, last_sync_status, last_sync_error, created_at, updated_at
			) VALUES(1, 'Historical v5', 'historical-v5@icloud.com', 'imap.mail.me.com', 993,
				'historical-v5@icloud.com', 'ciphertext', 1, 'pending', '', 1, 1)`); err != nil {
				_ = legacy.Close()
				t.Fatal(err)
			}
			for _, table := range test.drop {
				if _, err := legacy.ExecContext(ctx, `DROP TABLE `+table); err != nil {
					_ = legacy.Close()
					t.Fatalf("drop %s from v5 fixture: %v", table, err)
				}
			}
			if _, err := legacy.ExecContext(ctx, `PRAGMA user_version = 5`); err != nil {
				_ = legacy.Close()
				t.Fatal(err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}

			migrated, err := store.Open(databasePath)
			if err != nil {
				t.Fatalf("migrate historical v5 layout: %v", err)
			}
			defer migrated.Close()
			var version int
			if err := migrated.DB().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != 8 || !sqliteObjectExists(t, ctx, migrated.DB(), "table", "archived_messages") {
				t.Fatalf("historical v5 layout did not converge to v8; version=%d", version)
			}
			if _, err := migrated.GetAccount(ctx, 1); err != nil {
				t.Fatalf("historical account was not retained: %v", err)
			}
		})
	}
}

func TestMigrateHistoricalV5RejectsPartialTableGroup(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		dropTable string
		want      string
	}{
		{name: "automatic alias group", dropTable: "pending_alias_api_keys", want: "automatic alias"},
		{name: "seen group", dropTable: "imap_seen_tasks", want: "seen"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			databasePath := filepath.Join(t.TempDir(), "partial-v5.db")
			legacy := createSQLiteV6Fixture(t, databasePath)
			if _, err := legacy.ExecContext(ctx, `DROP TABLE `+test.dropTable); err != nil {
				_ = legacy.Close()
				t.Fatal(err)
			}
			if _, err := legacy.ExecContext(ctx, `PRAGMA user_version = 5`); err != nil {
				_ = legacy.Close()
				t.Fatal(err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := store.Open(databasePath)
			if err == nil {
				_ = reopened.Close()
				t.Fatal("partial v5 schema unexpectedly migrated")
			}
			if !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("partial v5 error = %q, want fragment %q", err, test.want)
			}
		})
	}
}

func createSQLiteV6Fixture(t *testing.T, databasePath string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open v6 fixture: %v", err)
	}
	for _, statement := range legacyV1Schema {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			t.Fatalf("create v6 core fixture: %v", err)
		}
	}
	statements := []string{
		`ALTER TABLE admins ADD COLUMN password_version INTEGER NOT NULL DEFAULT 1 CHECK(password_version >= 1)`,
		`ALTER TABLE admin_sessions ADD COLUMN password_version INTEGER NOT NULL DEFAULT 1 CHECK(password_version >= 1)`,
		`CREATE TABLE apple_web_sessions (
			account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			session_ciphertext TEXT NOT NULL, apple_id TEXT NOT NULL, region TEXT NOT NULL DEFAULT '',
			authenticated INTEGER NOT NULL DEFAULT 0, last_validated_at INTEGER,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE imap_sync_states (
			account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
			last_uid INTEGER NOT NULL DEFAULT 0 CHECK(last_uid BETWEEN 0 AND 4294967295),
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE alias_creation_schedules (
			account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			enabled INTEGER NOT NULL DEFAULT 0, planned_at_json TEXT NOT NULL DEFAULT '[]',
			next_run_at INTEGER, last_attempted_at INTEGER, last_created_at INTEGER,
			last_alias_address TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE pending_alias_api_keys (
			alias_id INTEGER PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
			api_key_ciphertext TEXT NOT NULL, created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE consumed_messages (
			alias_id INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
			uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
			uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295), consumed_at INTEGER NOT NULL,
			PRIMARY KEY(alias_id, uid_validity, uid)
		)`,
		`CREATE TABLE imap_seen_tasks (
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
			uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295), created_at INTEGER NOT NULL,
			PRIMARY KEY(account_id, uid_validity, uid)
		)`,
		`CREATE INDEX imap_seen_tasks_account_created_idx
			ON imap_seen_tasks(account_id, created_at, uid_validity, uid)`,
		`PRAGMA user_version = 6`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			t.Fatalf("create v6 extension fixture with %q: %v", statement, err)
		}
	}
	return db
}

func sqliteObjectExists(t *testing.T, ctx context.Context, db *sql.DB, objectType, name string) bool {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`, objectType, name,
	).Scan(&count); err != nil {
		t.Fatalf("inspect SQLite %s %s: %v", objectType, name, err)
	}
	return count == 1
}

func sqliteTableColumns(t *testing.T, ctx context.Context, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		t.Fatalf("inspect SQLite table %s: %v", table, err)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var position, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&position, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

func oneArchivedMailboxMessage(t *testing.T, ctx context.Context, db *store.Store, aliasID int64) domain.ArchivedMailboxMessage {
	t.Helper()
	messages, err := db.ListArchivedMailboxMessages(ctx, aliasID)
	if err != nil {
		t.Fatalf("list migrated alias %d messages: %v", aliasID, err)
	}
	if len(messages) != 1 {
		t.Fatalf("migrated alias %d message count = %d, want 1", aliasID, len(messages))
	}
	return messages[0]
}
