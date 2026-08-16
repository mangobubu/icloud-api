package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestMigrateV2ToV7(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-v2.db")
	legacy, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open v2 fixture: %v", err)
	}
	for _, statement := range legacyV1Schema {
		if _, err := legacy.ExecContext(ctx, statement); err != nil {
			_ = legacy.Close()
			t.Fatalf("create v2 fixture: %v", err)
		}
	}
	for _, statement := range []string{
		`ALTER TABLE admins ADD COLUMN password_version INTEGER NOT NULL DEFAULT 1 CHECK(password_version >= 1)`,
		`ALTER TABLE admin_sessions ADD COLUMN password_version INTEGER NOT NULL DEFAULT 1 CHECK(password_version >= 1)`,
		`INSERT INTO accounts(
			id, name, email, imap_host, imap_port, imap_username, password_ciphertext,
			enabled, last_sync_status, last_sync_error, created_at, updated_at
		) VALUES(1, 'Legacy v2', 'legacy-v2@icloud.com', 'imap.mail.me.com', 993,
			'legacy-v2@icloud.com', 'ciphertext', 1, 'pending', '', 1, 1)`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := legacy.ExecContext(ctx, statement); err != nil {
			_ = legacy.Close()
			t.Fatalf("prepare v2 fixture with %q: %v", statement, err)
		}
	}
	if err := legacy.Close(); err != nil {
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
	if version != 8 {
		t.Fatalf("schema version = %d, want 8", version)
	}
	retained, err := migrated.GetAccount(ctx, 1)
	if err != nil {
		t.Fatalf("read retained v2 account: %v", err)
	}
	if retained.Email != "legacy-v2@icloud.com" {
		t.Fatalf("retained account email = %q", retained.Email)
	}
	if _, err := migrated.UpsertAppleWebSession(ctx, domain.AppleWebSession{
		AccountID: 1, Ciphertext: "as1.fixture", AppleID: retained.Email,
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
