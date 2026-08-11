package store_test

import (
	"context"
	"fmt"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestListAccountsPageOrdersWithoutOverlapAndReportsTotal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	for _, fixture := range []struct {
		name  string
		email string
	}{
		{name: "Echo", email: "echo-page@icloud.com"},
		{name: "Alpha", email: "alpha-page@icloud.com"},
		{name: "Delta", email: "delta-page@icloud.com"},
		{name: "Bravo", email: "bravo-page@icloud.com"},
		{name: "Charlie", email: "charlie-page@icloud.com"},
	} {
		createAccount(t, ctx, db, fixture.name, fixture.email)
	}

	var got []string
	for offset := 0; offset < 6; offset += 2 {
		page, err := db.ListAccountsPage(ctx, store.AccountListFilter{Limit: 2, Offset: offset})
		if err != nil {
			t.Fatalf("list account page at offset %d: %v", offset, err)
		}
		if page.Total != 5 {
			t.Fatalf("account page total at offset %d = %d, want 5", offset, page.Total)
		}
		if len(page.Items) > 2 {
			t.Fatalf("account page at offset %d returned %d items, want at most 2", offset, len(page.Items))
		}
		for _, account := range page.Items {
			got = append(got, account.Email)
		}
	}

	want := []string{
		"alpha-page@icloud.com",
		"bravo-page@icloud.com",
		"charlie-page@icloud.com",
		"delta-page@icloud.com",
		"echo-page@icloud.com",
	}
	assertStringsEqual(t, got, want)
}

func TestListAccountsPageFiltersEmailAndNameCaseInsensitively(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	createAccount(t, ctx, db, "Production Team", "owner@icloud.com")
	createAccount(t, ctx, db, "Sales", "billing-special@icloud.com")
	createAccount(t, ctx, db, "100% Service", "percent@icloud.com")
	createAccount(t, ctx, db, "Unrelated", "other@icloud.com")

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{name: "name", query: "PRODUCTION", want: "owner@icloud.com"},
		{name: "email", query: "SPECIAL", want: "billing-special@icloud.com"},
		{name: "literal wildcard", query: "%", want: "percent@icloud.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page, err := db.ListAccountsPage(ctx, store.AccountListFilter{
				Query: test.query,
				Limit: 10,
			})
			if err != nil {
				t.Fatalf("search accounts for %q: %v", test.query, err)
			}
			if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Email != test.want {
				t.Fatalf("search accounts for %q = total %d items %#v, want %q", test.query, page.Total, page.Items, test.want)
			}
		})
	}
}

func TestListAliasesPageOrdersFiltersAndReportsTotal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	first := createAccount(t, ctx, db, "First", "first-alias-page@icloud.com")
	second := createAccount(t, ctx, db, "Second", "second-alias-page@icloud.com")

	for index, fixture := range []struct {
		accountID int64
		address   string
	}{
		{accountID: first.ID, address: "echo-alias-page@icloud.com"},
		{accountID: first.ID, address: "alpha-alias-page@icloud.com"},
		{accountID: second.ID, address: "foxtrot-alias-page@icloud.com"},
		{accountID: first.ID, address: "charlie-alias-page@icloud.com"},
		{accountID: second.ID, address: "bravo-alias-page@icloud.com"},
		{accountID: first.ID, address: "delta-alias-page@icloud.com"},
	} {
		createAlias(t, ctx, db, fixture.accountID, fixture.address, []byte(fmt.Sprintf("alias-page-hash-%d", index)))
	}

	var all []string
	for offset := 0; offset < 8; offset += 2 {
		page, err := db.ListAliasesPage(ctx, store.AliasListFilter{Limit: 2, Offset: offset})
		if err != nil {
			t.Fatalf("list alias page at offset %d: %v", offset, err)
		}
		if page.Total != 6 {
			t.Fatalf("alias page total at offset %d = %d, want 6", offset, page.Total)
		}
		if len(page.Items) > 2 {
			t.Fatalf("alias page at offset %d returned %d items, want at most 2", offset, len(page.Items))
		}
		for _, alias := range page.Items {
			all = append(all, alias.Address)
		}
	}
	assertStringsEqual(t, all, []string{
		"alpha-alias-page@icloud.com",
		"bravo-alias-page@icloud.com",
		"charlie-alias-page@icloud.com",
		"delta-alias-page@icloud.com",
		"echo-alias-page@icloud.com",
		"foxtrot-alias-page@icloud.com",
	})

	filtered, err := db.ListAliasesPage(ctx, store.AliasListFilter{
		AccountID: &first.ID,
		Limit:     2,
		Offset:    1,
	})
	if err != nil {
		t.Fatalf("list filtered alias page: %v", err)
	}
	if filtered.Total != 4 {
		t.Fatalf("filtered alias total = %d, want 4", filtered.Total)
	}
	if len(filtered.Items) != 2 {
		t.Fatalf("filtered alias item count = %d, want 2", len(filtered.Items))
	}
	for _, alias := range filtered.Items {
		if alias.AccountID != first.ID {
			t.Fatalf("filtered alias belongs to account %d, want %d", alias.AccountID, first.ID)
		}
	}
	assertStringsEqual(t, aliasAddresses(filtered.Items), []string{
		"charlie-alias-page@icloud.com",
		"delta-alias-page@icloud.com",
	})
}

func TestListPagesRejectInvalidBounds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	if _, err := db.ListAccountsPage(ctx, store.AccountListFilter{Limit: 0}); err == nil {
		t.Fatal("account page accepted a non-positive limit")
	}
	if _, err := db.ListAccountsPage(ctx, store.AccountListFilter{Limit: 1, Offset: -1}); err == nil {
		t.Fatal("account page accepted a negative offset")
	}
	invalidAccountID := int64(0)
	if _, err := db.ListAliasesPage(ctx, store.AliasListFilter{
		AccountID: &invalidAccountID,
		Limit:     1,
	}); err == nil {
		t.Fatal("alias page accepted a non-positive account ID")
	}
}

func aliasAddresses(aliases []domain.Alias) []string {
	addresses := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		addresses = append(addresses, alias.Address)
	}
	return addresses
}

func assertStringsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("values = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("value %d = %q, want %q; all values = %#v", index, got[index], want[index], got)
		}
	}
}
