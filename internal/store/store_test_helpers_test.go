package store_test

import (
	"context"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test store: %v", err)
		}
	})
	return db
}

func createAccount(t *testing.T, ctx context.Context, db *store.Store, name, email string) domain.Account {
	t.Helper()
	account, err := db.CreateAccount(ctx, domain.Account{
		Name: name, Email: email, IMAPHost: "imap.mail.me.com", IMAPPort: 993,
		IMAPUsername: email, PasswordCiphertext: "encrypted", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create account %q: %v", email, err)
	}
	return account
}

func createAlias(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	accountID int64,
	address string,
	hash []byte,
) domain.Alias {
	t.Helper()
	alias, err := db.CreateAlias(ctx, domain.Alias{
		AccountID: accountID, Address: address, Label: address,
		APIKeyHash: hash, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create alias %q: %v", address, err)
	}
	return alias
}
