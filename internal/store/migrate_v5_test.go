package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestSQLiteFreshSchemaV6Tables(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "fresh-v6.db"))
	if err != nil {
		t.Fatalf("create fresh v6 database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close fresh v6 database: %v", err)
		}
	})

	var version int
	if err := db.DB().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read fresh schema version: %v", err)
	}
	if version != 6 {
		t.Fatalf("fresh schema version = %d, want 6", version)
	}

	wantedDefinitions := map[string][]string{
		"consumed_messages": {
			"alias_id integer not null references aliases(id) on delete cascade",
			"uid_validity integer not null check(uid_validity between 1 and 4294967295)",
			"uid integer not null check(uid between 1 and 4294967295)",
			"consumed_at integer not null",
			"primary key(alias_id, uid_validity, uid)",
		},
		"imap_seen_tasks": {
			"account_id integer not null references accounts(id) on delete cascade",
			"uid_validity integer not null check(uid_validity between 1 and 4294967295)",
			"uid integer not null check(uid between 1 and 4294967295)",
			"created_at integer not null",
			"primary key(account_id, uid_validity, uid)",
		},
	}
	for table, fragments := range wantedDefinitions {
		var definition string
		if err := db.DB().QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&definition); err != nil {
			t.Fatalf("read %s definition: %v", table, err)
		}
		normalized := normalizeV5SQL(definition)
		for _, fragment := range fragments {
			if !strings.Contains(normalized, fragment) {
				t.Errorf("%s definition %q is missing %q", table, normalized, fragment)
			}
		}
	}

	const indexName = "imap_seen_tasks_account_created_idx"
	var indexDefinition string
	if err := db.DB().QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, indexName,
	).Scan(&indexDefinition); err != nil {
		t.Fatalf("read %s definition: %v", indexName, err)
	}
	const wantedIndex = "create index imap_seen_tasks_account_created_idx on imap_seen_tasks(account_id, created_at, uid_validity, uid)"
	if normalized := normalizeV5SQL(indexDefinition); normalized != wantedIndex {
		t.Fatalf("%s definition = %q, want %q", indexName, normalized, wantedIndex)
	}

	rows, err := db.DB().QueryContext(ctx, `PRAGMA index_info('imap_seen_tasks_account_created_idx')`)
	if err != nil {
		t.Fatalf("inspect %s columns: %v", indexName, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var sequence, columnID int
		var column string
		if err := rows.Scan(&sequence, &columnID, &column); err != nil {
			t.Fatalf("scan %s column: %v", indexName, err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s columns: %v", indexName, err)
	}
	wantedColumns := []string{"account_id", "created_at", "uid_validity", "uid"}
	if !reflect.DeepEqual(columns, wantedColumns) {
		t.Fatalf("%s columns = %v, want %v", indexName, columns, wantedColumns)
	}
}

func TestMigrateV4ToV6AddsAliasAndSeenTablesAndRetainsData(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-v4.db")
	fixture, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("create v4 migration fixture: %v", err)
	}
	account := createAccount(t, ctx, fixture, "Legacy v4", "legacy-v4@icloud.com")
	alias := createAlias(t, ctx, fixture, account.ID, "legacy-v4-alias@icloud.com", []byte("legacy-v4-hash"))
	if _, err := fixture.DB().ExecContext(ctx, `
		INSERT INTO imap_sync_states(account_id, uid_validity, last_uid, updated_at)
		VALUES(?, 31, 32, 33)`, account.ID); err != nil {
		_ = fixture.Close()
		t.Fatalf("seed v4 IMAP sync state: %v", err)
	}

	for _, statement := range []string{
		`DROP TABLE imap_seen_tasks`,
		`DROP TABLE consumed_messages`,
		`DROP TABLE pending_alias_api_keys`,
		`DROP TABLE alias_creation_schedules`,
		`PRAGMA user_version = 4`,
	} {
		if _, err := fixture.DB().ExecContext(ctx, statement); err != nil {
			_ = fixture.Close()
			t.Fatalf("prepare v4 migration fixture with %q: %v", statement, err)
		}
	}
	if err := fixture.Close(); err != nil {
		t.Fatalf("close v4 migration fixture: %v", err)
	}

	migrated, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("open and migrate v4 database: %v", err)
	}
	t.Cleanup(func() {
		if err := migrated.Close(); err != nil {
			t.Errorf("close migrated v6 database: %v", err)
		}
	})

	var version int
	if err := migrated.DB().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read migrated schema version: %v", err)
	}
	if version != 6 {
		t.Fatalf("migrated schema version = %d, want 6", version)
	}
	retained, err := migrated.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("read retained v4 alias: %v", err)
	}
	if retained.AccountID != account.ID || retained.Address != alias.Address {
		t.Fatalf("retained v4 alias = %#v, want account %d address %q", retained, account.ID, alias.Address)
	}
	var retainedUIDValidity, retainedUID int64
	if err := migrated.DB().QueryRowContext(ctx, `
		SELECT uid_validity, last_uid FROM imap_sync_states WHERE account_id = ?`, account.ID,
	).Scan(&retainedUIDValidity, &retainedUID); err != nil {
		t.Fatalf("read retained v4 IMAP sync state: %v", err)
	}
	if retainedUIDValidity != 31 || retainedUID != 32 {
		t.Fatalf("retained v4 IMAP sync state = (%d, %d), want (31, 32)", retainedUIDValidity, retainedUID)
	}

	if _, err := migrated.DB().ExecContext(ctx, `
		INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at)
		VALUES(?, 41, 42, 43)`, alias.ID); err != nil {
		t.Fatalf("use migrated consumed_messages table: %v", err)
	}
	if _, err := migrated.DB().ExecContext(ctx, `
		INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at)
		VALUES(?, 41, 42, 43)`, account.ID); err != nil {
		t.Fatalf("use migrated imap_seen_tasks table: %v", err)
	}

	var queued int
	if err := migrated.DB().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM consumed_messages c
		JOIN imap_seen_tasks q
		  ON q.account_id = ? AND q.uid_validity = c.uid_validity AND q.uid = c.uid
		WHERE c.alias_id = ?`, account.ID, alias.ID,
	).Scan(&queued); err != nil {
		t.Fatalf("read migrated v6 rows: %v", err)
	}
	if queued != 1 {
		t.Fatalf("joined migrated v6 row count = %d, want 1", queued)
	}
}

func TestMigrateV3ToV6AddsSyncAliasAndSeenTables(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-v3.db")
	fixture, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("create v3 migration fixture: %v", err)
	}
	account := createAccount(t, ctx, fixture, "Legacy v3", "legacy-v3@icloud.com")
	alias := createAlias(t, ctx, fixture, account.ID, "legacy-v3-alias@icloud.com", []byte("legacy-v3-hash"))
	if _, err := fixture.UpsertAppleWebSession(ctx, domain.AppleWebSession{
		AccountID: account.ID, Ciphertext: "as1.legacy-v3", AppleID: account.Email,
		Region: "US", Authenticated: true,
	}); err != nil {
		_ = fixture.Close()
		t.Fatalf("seed v3 Apple web session: %v", err)
	}

	for _, statement := range []string{
		`DROP TABLE imap_seen_tasks`,
		`DROP TABLE consumed_messages`,
		`DROP TABLE pending_alias_api_keys`,
		`DROP TABLE alias_creation_schedules`,
		`DROP TABLE imap_sync_states`,
		`PRAGMA user_version = 3`,
	} {
		if _, err := fixture.DB().ExecContext(ctx, statement); err != nil {
			_ = fixture.Close()
			t.Fatalf("prepare v3 migration fixture with %q: %v", statement, err)
		}
	}
	if err := fixture.Close(); err != nil {
		t.Fatalf("close v3 migration fixture: %v", err)
	}

	migrated, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("open and migrate v3 database: %v", err)
	}
	t.Cleanup(func() {
		if err := migrated.Close(); err != nil {
			t.Errorf("close migrated v3 database: %v", err)
		}
	})

	var version int
	if err := migrated.DB().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read v3 migration schema version: %v", err)
	}
	if version != 6 {
		t.Fatalf("v3 migration schema version = %d, want 6", version)
	}
	retainedSession, err := migrated.GetAppleWebSession(ctx, account.ID)
	if err != nil {
		t.Fatalf("read retained v3 Apple web session: %v", err)
	}
	if retainedSession.Ciphertext != "as1.legacy-v3" || !retainedSession.Authenticated {
		t.Fatalf("retained v3 Apple web session = %#v", retainedSession)
	}
	if _, err := migrated.DB().ExecContext(ctx, `
		INSERT INTO imap_sync_states(account_id, uid_validity, last_uid, updated_at)
		VALUES(?, 51, 52, 53)`, account.ID); err != nil {
		t.Fatalf("use v3-to-v6 migrated IMAP sync state: %v", err)
	}
	if _, err := migrated.DB().ExecContext(ctx, `
		INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at)
		VALUES(?, 51, 52, 53)`, alias.ID); err != nil {
		t.Fatalf("use v3-to-v6 migrated consumption table: %v", err)
	}
	if _, err := migrated.DB().ExecContext(ctx, `
		INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at)
		VALUES(?, 51, 52, 53)`, account.ID); err != nil {
		t.Fatalf("use v3-to-v6 migrated seen queue: %v", err)
	}
}

func TestMigrateHistoricalV5LayoutsToV6(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		drop           []string
		retainedRows   []string
		newEmptyTables []string
	}{
		{
			name: "automatic alias tables only",
			drop: []string{
				`DROP TABLE imap_seen_tasks`,
				`DROP TABLE consumed_messages`,
			},
			retainedRows:   []string{"alias_creation_schedules", "pending_alias_api_keys"},
			newEmptyTables: []string{"consumed_messages", "imap_seen_tasks"},
		},
		{
			name: "seen tables only",
			drop: []string{
				`DROP TABLE pending_alias_api_keys`,
				`DROP TABLE alias_creation_schedules`,
			},
			retainedRows:   []string{"consumed_messages", "imap_seen_tasks"},
			newEmptyTables: []string{"alias_creation_schedules", "pending_alias_api_keys"},
		},
		{
			name: "both table groups",
			retainedRows: []string{
				"alias_creation_schedules", "pending_alias_api_keys",
				"consumed_messages", "imap_seen_tasks",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			databasePath := filepath.Join(t.TempDir(), "historical-v5.db")
			fixture, err := store.Open(databasePath)
			if err != nil {
				t.Fatalf("create historical v5 fixture: %v", err)
			}
			account := createAccount(t, ctx, fixture, "Historical v5", "historical-v5@icloud.com")
			alias := createAlias(
				t, ctx, fixture, account.ID, "historical-v5-alias@icloud.com", []byte("historical-v5-hash"),
			)
			for _, statement := range []struct {
				query string
				args  []any
			}{
				{
					query: `INSERT INTO alias_creation_schedules(
						account_id, enabled, planned_at_json, last_alias_address,
						last_error, created_at, updated_at
					) VALUES(?, 1, '[]', 'retained-auto@icloud.com', '', 11, 12)`,
					args: []any{account.ID},
				},
				{
					query: `INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
						VALUES(?, 'retained-ciphertext', 13)`,
					args: []any{alias.ID},
				},
				{
					query: `INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at)
						VALUES(?, 71, 72, 73)`,
					args: []any{alias.ID},
				},
				{
					query: `INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at)
						VALUES(?, 71, 72, 74)`,
					args: []any{account.ID},
				},
			} {
				if _, err := fixture.DB().ExecContext(ctx, statement.query, statement.args...); err != nil {
					_ = fixture.Close()
					t.Fatalf("seed historical v5 fixture: %v", err)
				}
			}
			for _, statement := range append(test.drop, `PRAGMA user_version = 5`) {
				if _, err := fixture.DB().ExecContext(ctx, statement); err != nil {
					_ = fixture.Close()
					t.Fatalf("prepare historical v5 fixture with %q: %v", statement, err)
				}
			}
			if err := fixture.Close(); err != nil {
				t.Fatalf("close historical v5 fixture: %v", err)
			}

			migrated, err := store.Open(databasePath)
			if err != nil {
				t.Fatalf("migrate historical v5 fixture: %v", err)
			}
			t.Cleanup(func() { _ = migrated.Close() })
			var version int
			if err := migrated.DB().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatalf("read migrated schema version: %v", err)
			}
			if version != 6 {
				t.Fatalf("migrated schema version = %d, want 6", version)
			}
			for _, table := range test.retainedRows {
				assertSQLiteRowCount(t, ctx, migrated.DB(), table, 1)
			}
			for _, table := range test.newEmptyTables {
				assertSQLiteRowCount(t, ctx, migrated.DB(), table, 0)
			}
		})
	}
}

func TestMigrateHistoricalV5RejectsPartialTableGroup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dropTable string
		wantError string
	}{
		{
			name:      "automatic alias group",
			dropTable: "pending_alias_api_keys",
			wantError: "automatic alias",
		},
		{
			name:      "seen group",
			dropTable: "imap_seen_tasks",
			wantError: "seen",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			databasePath := filepath.Join(t.TempDir(), "partial-v5.db")
			fixture, err := store.Open(databasePath)
			if err != nil {
				t.Fatalf("create partial v5 fixture: %v", err)
			}
			if _, err := fixture.DB().ExecContext(ctx, `DROP TABLE `+test.dropTable); err != nil {
				_ = fixture.Close()
				t.Fatalf("drop %s from partial v5 fixture: %v", test.dropTable, err)
			}
			if _, err := fixture.DB().ExecContext(ctx, `PRAGMA user_version = 5`); err != nil {
				_ = fixture.Close()
				t.Fatalf("mark partial fixture as v5: %v", err)
			}
			if err := fixture.Close(); err != nil {
				t.Fatalf("close partial v5 fixture: %v", err)
			}

			reopened, err := store.Open(databasePath)
			if err == nil {
				_ = reopened.Close()
				t.Fatal("opening v5 schema with a partial table group succeeded")
			}
			if !strings.Contains(strings.ToLower(err.Error()), test.wantError) {
				t.Fatalf("partial v5 schema error = %q, want fragment %q", err, test.wantError)
			}
		})
	}
}

func TestSQLiteV6SeenTableConstraintsAndCascades(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Seen constraints", "seen-constraints@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "seen-constraints-alias@icloud.com", []byte("seen-constraints-hash"))
	unusedAlias := createAlias(t, ctx, db, account.ID, "seen-constraints-unused@icloud.com", []byte("seen-constraints-unused-hash"))

	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at)
		VALUES(?, 4294967295, 4294967295, 1)`, alias.ID); err != nil {
		t.Fatalf("insert valid consumed message at uint32 maximum: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at)
		VALUES(?, 2, 2, 2)`, alias.ID); err != nil {
		t.Fatalf("insert a newer consumed message for the same alias: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at)
		VALUES(?, 4294967295, 4294967295, 1)`, account.ID); err != nil {
		t.Fatalf("insert valid seen task at uint32 maximum: %v", err)
	}

	rejected := []struct {
		name  string
		query string
		args  []any
	}{
		{
			"consumed message null alias",
			`INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at) VALUES(NULL, 1, 1, 1)`,
			nil,
		},
		{
			"consumed message missing alias",
			`INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at) VALUES(?, 1, 1, 1)`,
			[]any{alias.ID + 100000},
		},
		{
			"consumed message zero UIDVALIDITY",
			`INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at) VALUES(?, 0, 1, 1)`,
			[]any{unusedAlias.ID},
		},
		{
			"consumed message UIDVALIDITY overflow",
			`INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at) VALUES(?, 4294967296, 1, 1)`,
			[]any{unusedAlias.ID},
		},
		{
			"consumed message zero UID",
			`INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at) VALUES(?, 1, 0, 1)`,
			[]any{unusedAlias.ID},
		},
		{
			"consumed message UID overflow",
			`INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at) VALUES(?, 1, 4294967296, 1)`,
			[]any{unusedAlias.ID},
		},
		{
			"consumed message null timestamp",
			`INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at) VALUES(?, 1, 1, NULL)`,
			[]any{unusedAlias.ID},
		},
		{
			"consumed message duplicate identity",
			`INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at) VALUES(?, 4294967295, 4294967295, 2)`,
			[]any{alias.ID},
		},
		{
			"seen task null account",
			`INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at) VALUES(NULL, 1, 1, 1)`,
			nil,
		},
		{
			"seen task missing account",
			`INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at) VALUES(?, 1, 1, 1)`,
			[]any{account.ID + 100000},
		},
		{
			"seen task zero UIDVALIDITY",
			`INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at) VALUES(?, 0, 1, 1)`,
			[]any{account.ID},
		},
		{
			"seen task UIDVALIDITY overflow",
			`INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at) VALUES(?, 4294967296, 1, 1)`,
			[]any{account.ID},
		},
		{
			"seen task zero UID",
			`INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at) VALUES(?, 1, 0, 1)`,
			[]any{account.ID},
		},
		{
			"seen task UID overflow",
			`INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at) VALUES(?, 1, 4294967296, 1)`,
			[]any{account.ID},
		},
		{
			"seen task null timestamp",
			`INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at) VALUES(?, 1, 1, NULL)`,
			[]any{account.ID},
		},
		{
			"seen task duplicate identity",
			`INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at) VALUES(?, 4294967295, 4294967295, 2)`,
			[]any{account.ID},
		},
	}
	for _, test := range rejected {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.DB().ExecContext(ctx, test.query, test.args...); err == nil {
				t.Fatal("invalid row was accepted")
			}
		})
	}

	if _, err := db.DB().ExecContext(ctx, `DELETE FROM aliases WHERE id = ?`, alias.ID); err != nil {
		t.Fatalf("delete consumed alias: %v", err)
	}
	assertSQLiteRowCount(t, ctx, db.DB(), "consumed_messages", 0)
	assertSQLiteRowCount(t, ctx, db.DB(), "imap_seen_tasks", 1)

	if _, err := db.DB().ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, account.ID); err != nil {
		t.Fatalf("delete queued account: %v", err)
	}
	assertSQLiteRowCount(t, ctx, db.DB(), "consumed_messages", 0)
	assertSQLiteRowCount(t, ctx, db.DB(), "imap_seen_tasks", 0)
	assertSQLiteRowCount(t, ctx, db.DB(), "aliases", 0)
}

func TestSQLiteV6ConvergenceRestoresSeenTaskIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "v6-seen-index-convergence.db")
	fixture, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("create v6 convergence fixture: %v", err)
	}
	if _, err := fixture.DB().ExecContext(ctx, `DROP INDEX imap_seen_tasks_account_created_idx`); err != nil {
		_ = fixture.Close()
		t.Fatalf("drop seen task index from fixture: %v", err)
	}
	if err := fixture.Close(); err != nil {
		t.Fatalf("close v6 convergence fixture: %v", err)
	}

	converged, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("reopen and converge v6 database: %v", err)
	}
	t.Cleanup(func() {
		if err := converged.Close(); err != nil {
			t.Errorf("close converged v6 database: %v", err)
		}
	})

	var definition string
	if err := converged.DB().QueryRowContext(ctx, `
		SELECT sql FROM sqlite_master
		WHERE type = 'index' AND name = 'imap_seen_tasks_account_created_idx'`,
	).Scan(&definition); err != nil {
		t.Fatalf("read restored seen task index: %v", err)
	}
	const wanted = "create index imap_seen_tasks_account_created_idx on imap_seen_tasks(account_id, created_at, uid_validity, uid)"
	if normalized := normalizeV5SQL(definition); normalized != wanted {
		t.Fatalf("restored seen task index = %q, want %q", normalized, wanted)
	}
}

func TestSQLiteV6ValidationRejectsMissingSeenTables(t *testing.T) {
	t.Parallel()

	for _, table := range []string{"consumed_messages", "imap_seen_tasks"} {
		t.Run(table, func(t *testing.T) {
			databasePath := filepath.Join(t.TempDir(), "v6-missing-table.db")
			fixture, err := store.Open(databasePath)
			if err != nil {
				t.Fatalf("create v6 missing-table fixture: %v", err)
			}
			if _, err := fixture.DB().ExecContext(context.Background(), `DROP TABLE `+table); err != nil {
				_ = fixture.Close()
				t.Fatalf("drop %s from v6 fixture: %v", table, err)
			}
			if err := fixture.Close(); err != nil {
				t.Fatalf("close v6 missing-table fixture: %v", err)
			}

			reopened, err := store.Open(databasePath)
			if err == nil {
				_ = reopened.Close()
				t.Fatal("opening v6 schema with a missing required table succeeded")
			}
			wantError := "missing required table " + table
			if !strings.Contains(err.Error(), wantError) {
				t.Fatalf("open v6 schema error = %q, want fragment %q", err, wantError)
			}
		})
	}
}

func TestSQLiteV6ValidationRejectsMalformedSeenSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statements []string
		wantError  string
	}{
		{
			name: "missing required column",
			statements: []string{
				`DROP TABLE consumed_messages`,
				`CREATE TABLE consumed_messages (
					alias_id INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
					uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
					uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
					PRIMARY KEY(alias_id, uid_validity, uid)
				)`,
			},
			wantError: "consumed_messages is missing required column consumed_at",
		},
		{
			name: "wrong composite primary key",
			statements: []string{
				`DROP TABLE consumed_messages`,
				`CREATE TABLE consumed_messages (
					alias_id INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
					uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
					uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
					consumed_at INTEGER NOT NULL,
					PRIMARY KEY(alias_id, uid)
				)`,
			},
			wantError: "consumed_messages primary key",
		},
		{
			name: "wrong foreign key target",
			statements: []string{
				`DROP TABLE imap_seen_tasks`,
				`CREATE TABLE imap_seen_tasks (
					account_id INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
					uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
					uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
					created_at INTEGER NOT NULL,
					PRIMARY KEY(account_id, uid_validity, uid)
				)`,
			},
			wantError: "imap_seen_tasks foreign key",
		},
		{
			name: "wrong foreign key cascade",
			statements: []string{
				`DROP TABLE imap_seen_tasks`,
				`CREATE TABLE imap_seen_tasks (
					account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
					uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
					uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
					created_at INTEGER NOT NULL,
					PRIMARY KEY(account_id, uid_validity, uid)
				)`,
			},
			wantError: "ON DELETE RESTRICT",
		},
		{
			name: "missing one UID range check",
			statements: []string{
				`DROP TABLE consumed_messages`,
				`CREATE TABLE consumed_messages (
					alias_id INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
					uid_validity INTEGER NOT NULL CHECK(uid_validity BETWEEN 1 AND 4294967295),
					uid INTEGER NOT NULL,
					consumed_at INTEGER NOT NULL,
					PRIMARY KEY(alias_id, uid_validity, uid)
				)`,
			},
			wantError: "consumed_messages column uid is missing required CHECK(uid BETWEEN 1 AND 4294967295)",
		},
		{
			name: "missing both UID range checks",
			statements: []string{
				`DROP TABLE imap_seen_tasks`,
				`CREATE TABLE imap_seen_tasks (
					account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
					uid_validity INTEGER NOT NULL,
					uid INTEGER NOT NULL,
					created_at INTEGER NOT NULL,
					PRIMARY KEY(account_id, uid_validity, uid)
				)`,
				`CREATE INDEX imap_seen_tasks_account_created_idx
					ON imap_seen_tasks(account_id, created_at, uid_validity, uid)`,
			},
			wantError: "imap_seen_tasks column uid_validity is missing required CHECK(uid_validity BETWEEN 1 AND 4294967295)",
		},
		{
			name: "extra marker column cannot forge range checks",
			statements: []string{
				`DROP TABLE consumed_messages`,
				`CREATE TABLE consumed_messages (
					alias_id INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
					uid_validity INTEGER NOT NULL,
					uid INTEGER NOT NULL,
					consumed_at INTEGER NOT NULL,
					marker TEXT NOT NULL DEFAULT 'CHECK(uid_validity BETWEEN 1 AND 4294967295) CHECK(uid BETWEEN 1 AND 4294967295)',
					PRIMARY KEY(alias_id, uid_validity, uid)
				)`,
			},
			wantError: "consumed_messages has 5 columns, want exactly 4",
		},
		{
			name: "defaults strings and comments cannot forge range checks",
			statements: []string{
				`DROP TABLE consumed_messages`,
				`CREATE TABLE consumed_messages (
					alias_id INTEGER NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
					uid_validity INTEGER NOT NULL DEFAULT 'CHECK(uid_validity BETWEEN 1 AND 4294967295)',
					uid INTEGER NOT NULL,
					consumed_at INTEGER NOT NULL,
					PRIMARY KEY(alias_id, uid_validity, uid),
					CHECK('CHECK(uid BETWEEN 1 AND 4294967295)' <> '')
					/* CHECK(uid_validity BETWEEN 1 AND 4294967295) */
					-- CHECK(uid BETWEEN 1 AND 4294967295)
				)`,
			},
			wantError: "consumed_messages column uid_validity is missing required CHECK(uid_validity BETWEEN 1 AND 4294967295)",
		},
		{
			name: "wrong index column order",
			statements: []string{
				`DROP INDEX imap_seen_tasks_account_created_idx`,
				`CREATE INDEX imap_seen_tasks_account_created_idx
					ON imap_seen_tasks(account_id, uid_validity, created_at, uid)`,
			},
			wantError: "index imap_seen_tasks_account_created_idx column position 2",
		},
		{
			name: "partial index",
			statements: []string{
				`DROP INDEX imap_seen_tasks_account_created_idx`,
				`CREATE INDEX imap_seen_tasks_account_created_idx
					ON imap_seen_tasks(account_id, created_at, uid_validity, uid)
					WHERE created_at > 0`,
			},
			wantError: "index imap_seen_tasks_account_created_idx must not be partial",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			databasePath := filepath.Join(t.TempDir(), "malformed-v6.db")
			fixture, err := store.Open(databasePath)
			if err != nil {
				t.Fatalf("create malformed v6 fixture: %v", err)
			}
			for _, statement := range test.statements {
				if _, err := fixture.DB().ExecContext(context.Background(), statement); err != nil {
					_ = fixture.Close()
					t.Fatalf("prepare malformed v6 fixture with %q: %v", statement, err)
				}
			}
			if err := fixture.Close(); err != nil {
				t.Fatalf("close malformed v6 fixture: %v", err)
			}

			reopened, err := store.Open(databasePath)
			if err == nil {
				_ = reopened.Close()
				t.Fatal("opening malformed v6 schema succeeded")
			}
			if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("open malformed v6 schema error = %q, want fragment %q", err, test.wantError)
			}
		})
	}
}

func assertSQLiteRowCount(t *testing.T, ctx context.Context, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&got); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s row count = %d, want %d", table, got, want)
	}
}

func normalizeV5SQL(statement string) string {
	return strings.Join(strings.Fields(strings.ToLower(statement)), " ")
}
