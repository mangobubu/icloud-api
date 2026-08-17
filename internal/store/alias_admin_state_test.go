package store_test

import (
	"context"
	"testing"

	"icloud-api/internal/store"
)

func TestUpdateAliasAdminStateRollsBackEnabledWhenGroupWriteFails(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	account := createAccount(t, ctx, db, "Atomic alias update", "atomic-alias-update@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "atomic-alias@icloud.com", []byte("atomic-alias-hash"))
	if err := db.SetAliasEnabled(ctx, alias.ID, false); err != nil {
		t.Fatalf("disable alias: %v", err)
	}
	group, err := db.CreateMailGroup(ctx, "原子更新")
	if err != nil {
		t.Fatalf("create mail group: %v", err)
	}
	accountBefore, err := db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("read account before failed update: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_atomic_alias_group_update
		BEFORE UPDATE OF group_id ON aliases
		BEGIN
			SELECT RAISE(ABORT, 'forced alias group failure');
		END`); err != nil {
		t.Fatalf("create alias group failure trigger: %v", err)
	}

	enabled := true
	groupID := group.ID
	if _, err := db.UpdateAliasAdminState(ctx, alias.ID, store.AliasAdminStateUpdate{
		Enabled:        &enabled,
		GroupID:        &groupID,
		GroupIDPresent: true,
	}); err == nil {
		t.Fatal("combined alias update unexpectedly succeeded")
	}

	got, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("read alias after failed update: %v", err)
	}
	if got.Enabled || got.GroupID != nil {
		t.Fatalf("alias was partially updated: enabled=%v group_id=%v", got.Enabled, got.GroupID)
	}
	accountAfter, err := db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("read account after failed update: %v", err)
	}
	if !accountAfter.UpdatedAt.Equal(accountBefore.UpdatedAt) {
		t.Fatalf("account version changed after rollback: before=%v after=%v", accountBefore.UpdatedAt, accountAfter.UpdatedAt)
	}
}
