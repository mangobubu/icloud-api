package store_test

import (
	"context"
	"testing"

	"icloud-api/internal/domain"
)

func TestListEnabledAliasesByAccountFiltersWithoutChangingAdminList(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "alias-query@icloud.com")
	otherAccount := createAccount(t, ctx, db, "Other", "alias-query-other@icloud.com")

	createAlias(t, ctx, db, account.ID, "z-enabled@icloud.com", []byte("z-enabled-hash"))
	disabled := createAlias(t, ctx, db, account.ID, "m-disabled@icloud.com", []byte("m-disabled-hash"))
	createAlias(t, ctx, db, account.ID, "a-enabled@icloud.com", []byte("a-enabled-hash"))
	createAlias(t, ctx, db, otherAccount.ID, "b-other@icloud.com", []byte("b-other-hash"))
	if err := db.SetAliasEnabled(ctx, disabled.ID, false); err != nil {
		t.Fatalf("disable alias fixture: %v", err)
	}

	all, err := db.ListAliasesByAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("list all account aliases: %v", err)
	}
	assertAliasAddresses(t, all,
		"a-enabled@icloud.com", "m-disabled@icloud.com", "z-enabled@icloud.com",
	)
	if all[1].Enabled {
		t.Fatal("administrator alias list hid the disabled state")
	}

	enabled, err := db.ListEnabledAliasesByAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("list enabled account aliases: %v", err)
	}
	assertAliasAddresses(t, enabled, "a-enabled@icloud.com", "z-enabled@icloud.com")
}

func assertAliasAddresses(t *testing.T, aliases []domain.Alias, wanted ...string) {
	t.Helper()
	if len(aliases) != len(wanted) {
		t.Fatalf("alias count = %d, want %d: %#v", len(aliases), len(wanted), aliases)
	}
	for index, address := range wanted {
		if aliases[index].Address != address {
			t.Fatalf("alias %d address = %q, want %q", index, aliases[index].Address, address)
		}
	}
}
