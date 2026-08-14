package store_test

import (
	"context"
	"testing"

	"icloud-api/internal/store"
)

func TestDeleteAliasLocksAccountBeforeDeletingAlias(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Delete lock", "delete-lock@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "delete-lock-alias@icloud.com", []byte("delete-lock-hash"))
	installAliasMutationOrderLog(t, ctx, db)
	if _, err := db.DB().ExecContext(ctx, `
		CREATE TRIGGER require_account_lock_before_alias_delete
		BEFORE DELETE ON aliases
		BEGIN
			SELECT CASE WHEN NOT EXISTS(
				SELECT 1 FROM alias_mutation_order WHERE event = 'account-lock'
			) THEN RAISE(ABORT, 'alias deleted before account lock') END;
			INSERT INTO alias_mutation_order(event) VALUES('alias-delete');
		END`); err != nil {
		t.Fatalf("create alias delete order trigger: %v", err)
	}
	if err := db.DeleteAlias(ctx, alias.ID); err != nil {
		t.Fatalf("delete alias with account lock: %v", err)
	}
	assertMutationOrder(t, ctx, db, "account-lock,alias-delete,account-lock")
}

func installAliasMutationOrderLog(t *testing.T, ctx context.Context, db *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`CREATE TABLE alias_mutation_order(position INTEGER PRIMARY KEY AUTOINCREMENT, event TEXT NOT NULL)`,
		`CREATE TRIGGER record_account_lock BEFORE UPDATE ON accounts BEGIN
			INSERT INTO alias_mutation_order(event) VALUES('account-lock');
		END`,
	} {
		if _, err := db.DB().ExecContext(ctx, statement); err != nil {
			t.Fatalf("create mutation order fixture: %v", err)
		}
	}
}

func assertMutationOrder(t *testing.T, ctx context.Context, db *store.Store, want string) {
	t.Helper()
	var got string
	if err := db.DB().QueryRowContext(ctx, `
		SELECT group_concat(event, ',')
		FROM (SELECT event FROM alias_mutation_order ORDER BY position)`,
	).Scan(&got); err != nil {
		t.Fatalf("read alias mutation order: %v", err)
	}
	if got != want {
		t.Fatalf("alias mutation order = %q, want %q", got, want)
	}
}
