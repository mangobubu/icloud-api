package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestMigrateV2ToV6(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-v2.db")
	current, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("create current database: %v", err)
	}
	account := createAccount(t, ctx, current, "Legacy v2", "legacy-v2@icloud.com")
	if _, err := current.DB().ExecContext(ctx, `DROP TABLE imap_seen_tasks`); err != nil {
		_ = current.Close()
		t.Fatalf("remove v6 seen queue from fixture: %v", err)
	}
	if _, err := current.DB().ExecContext(ctx, `DROP TABLE consumed_messages`); err != nil {
		_ = current.Close()
		t.Fatalf("remove v6 consumption table from fixture: %v", err)
	}
	if _, err := current.DB().ExecContext(ctx, `DROP TABLE imap_sync_states`); err != nil {
		_ = current.Close()
		t.Fatalf("remove v4 table from fixture: %v", err)
	}
	if _, err := current.DB().ExecContext(ctx, `DROP TABLE apple_web_sessions`); err != nil {
		_ = current.Close()
		t.Fatalf("remove v3 table from fixture: %v", err)
	}
	if _, err := current.DB().ExecContext(ctx, `DROP TABLE pending_alias_api_keys`); err != nil {
		_ = current.Close()
		t.Fatalf("remove v6 pending key table from fixture: %v", err)
	}
	if _, err := current.DB().ExecContext(ctx, `DROP TABLE alias_creation_schedules`); err != nil {
		_ = current.Close()
		t.Fatalf("remove v6 schedule table from fixture: %v", err)
	}
	if _, err := current.DB().ExecContext(ctx, `PRAGMA user_version = 2`); err != nil {
		_ = current.Close()
		t.Fatalf("mark fixture as v2: %v", err)
	}
	if err := current.Close(); err != nil {
		t.Fatalf("close v2 fixture: %v", err)
	}

	migrated, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("open and migrate v2 database: %v", err)
	}
	t.Cleanup(func() {
		if err := migrated.Close(); err != nil {
			t.Errorf("close migrated database: %v", err)
		}
	})

	var version int
	if err := migrated.DB().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != 6 {
		t.Fatalf("schema version = %d, want 6", version)
	}
	retained, err := migrated.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("read retained v2 account: %v", err)
	}
	if retained.Email != account.Email {
		t.Fatalf("retained account email = %q, want %q", retained.Email, account.Email)
	}
	if _, err := migrated.UpsertAppleWebSession(ctx, domain.AppleWebSession{
		AccountID: account.ID, Ciphertext: "as1.fixture", AppleID: account.Email,
		Region: "US", Authenticated: true,
	}); err != nil {
		t.Fatalf("use migrated apple web sessions table: %v", err)
	}
}

func TestMigrateV1ToV6RollsBackAsAUnit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "conflicting-v1.db")
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
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE apple_web_sessions(marker TEXT)`); err != nil {
		_ = legacy.Close()
		t.Fatalf("create conflicting v3 table: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	if _, err := store.Open(databasePath); err == nil {
		t.Fatal("conflicting migration unexpectedly succeeded")
	}

	inspect, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("reopen failed migration: %v", err)
	}
	t.Cleanup(func() { _ = inspect.Close() })
	var version int
	if err := inspect.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read rolled back version: %v", err)
	}
	if version != 1 {
		t.Fatalf("schema version after rollback = %d, want 1", version)
	}
	var addedColumnCount int
	if err := inspect.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pragma_table_info('admins') WHERE name = 'password_version'`,
	).Scan(&addedColumnCount); err != nil {
		t.Fatalf("inspect rolled back admins table: %v", err)
	}
	if addedColumnCount != 0 {
		t.Fatal("v1 to v2 changes survived failed v3 migration")
	}
}
