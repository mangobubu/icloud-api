package store_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestAccountEmailRejectsExistingAliasAddress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	owner := createAccount(t, ctx, db, "Alias owner", "identity-owner@icloud.com")
	reserved := createAlias(t, ctx, db, owner.ID, "reserved-primary@example.test", []byte("reserved-primary-hash"))

	_, err := db.CreateAccount(ctx, domain.Account{
		Name:               "Conflicting account",
		Email:              "RESERVED-PRIMARY@EXAMPLE.TEST",
		IMAPHost:           "imap.example.test",
		IMAPPort:           993,
		IMAPUsername:       "reserved-primary@example.test",
		PasswordCiphertext: "encrypted",
		Enabled:            true,
	})
	if !errors.Is(err, store.ErrAliasIdentityConflict) {
		t.Fatalf("create account alias identity error = %v, want ErrAliasIdentityConflict", err)
	}
	if _, lookupErr := db.GetAccountByEmail(ctx, reserved.Address); !errors.Is(lookupErr, store.ErrNotFound) {
		t.Fatalf("conflicting primary account survived rejected creation: %v", lookupErr)
	}
	retained, lookupErr := db.GetAlias(ctx, reserved.ID)
	if lookupErr != nil || retained.Address != reserved.Address {
		t.Fatalf("reserved alias after rejected account creation = %#v, err=%v", retained, lookupErr)
	}
}

func TestAccountEmailUpdateRejectsExistingAliasAddressAndRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	owner := createAccount(t, ctx, db, "Alias owner", "update-owner@icloud.com")
	reserved := createAlias(t, ctx, db, owner.ID, "reserved-update@example.test", []byte("reserved-update-hash"))
	target := createAccount(t, ctx, db, "Update target", "update-target@icloud.com")
	before, err := db.GetAccount(ctx, target.ID)
	if err != nil {
		t.Fatalf("read target before conflicting update: %v", err)
	}

	target.Email = "RESERVED-UPDATE@EXAMPLE.TEST"
	if _, err := db.UpdateAccount(ctx, target); !errors.Is(err, store.ErrAliasIdentityConflict) {
		t.Fatalf("update account alias identity error = %v, want ErrAliasIdentityConflict", err)
	}
	after, err := db.GetAccount(ctx, target.ID)
	if err != nil {
		t.Fatalf("read target after conflicting update: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected account update changed target:\n before=%#v\n after=%#v", before, after)
	}
	retained, err := db.GetAlias(ctx, reserved.ID)
	if err != nil || retained.Address != reserved.Address {
		t.Fatalf("reserved alias after rejected account update = %#v, err=%v", retained, err)
	}
}

func TestAccountCreationAndCustomAliasImportSerializeSharedAddress(t *testing.T) {
	t.Parallel()
	for iteration := range 4 {
		t.Run(fmt.Sprintf("race-%d", iteration), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			databasePath := filepath.Join(t.TempDir(), "address-namespace.db")
			db, err := store.Open(databasePath)
			if err != nil {
				t.Fatalf("open custom alias race store: %v", err)
			}
			t.Cleanup(func() {
				if closeErr := db.Close(); closeErr != nil {
					t.Errorf("close custom alias race store: %v", closeErr)
				}
			})
			peer, err := store.Open(databasePath)
			if err != nil {
				t.Fatalf("open account race store: %v", err)
			}
			t.Cleanup(func() {
				if closeErr := peer.Close(); closeErr != nil {
					t.Errorf("close account race store: %v", closeErr)
				}
			})
			configureStrictImportCredentials(t, db)
			suffix := fmt.Sprintf("namespace-race-%d.test", iteration)
			custom := createCustomImportAccount(t, ctx, db, suffix, true)
			address := "abcdefgh@" + suffix
			start := make(chan struct{})
			type operationResult struct {
				name string
				err  error
			}
			results := make(chan operationResult, 2)
			go func() {
				<-start
				_, createErr := peer.CreateAccount(ctx, domain.Account{
					Name:               "Racing account",
					Email:              address,
					IMAPHost:           "imap.example.test",
					IMAPPort:           993,
					IMAPUsername:       address,
					PasswordCiphertext: "encrypted",
					Enabled:            true,
				})
				results <- operationResult{name: "account", err: createErr}
			}()
			go func() {
				<-start
				_, _, importErr := db.ImportCustomAliasesWithCredentialsStrict(ctx, custom.ID, []domain.AliasImportCandidate{{
					Address: address,
					Active:  true,
				}})
				results <- operationResult{name: "alias", err: importErr}
			}()
			close(start)

			succeeded := ""
			conflicts := 0
			for range 2 {
				result := <-results
				switch {
				case result.err == nil:
					if succeeded != "" {
						t.Fatalf("both shared-address operations succeeded: %s and %s", succeeded, result.name)
					}
					succeeded = result.name
				case errors.Is(result.err, store.ErrAliasIdentityConflict):
					conflicts++
				default:
					t.Fatalf("%s shared-address operation error = %v", result.name, result.err)
				}
			}
			if succeeded == "" || conflicts != 1 {
				t.Fatalf("shared-address race succeeded=%q conflicts=%d, want one of each", succeeded, conflicts)
			}
			_, accountErr := db.GetAccountByEmail(ctx, address)
			_, aliasErr := db.GetAliasByAddress(ctx, address)
			if (accountErr == nil) == (aliasErr == nil) {
				t.Fatalf("shared address final owners: account err=%v, alias err=%v; want exactly one", accountErr, aliasErr)
			}
		})
	}
}
