package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestAppleWebSessionRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
	lastValidatedAt := time.Date(2026, 8, 7, 9, 30, 15, 123456789, time.UTC)

	want := domain.AppleWebSession{
		AccountID:       account.ID,
		Ciphertext:      "encrypted-web-session",
		AppleID:         "primary@icloud.com",
		Region:          "CHN",
		Authenticated:   true,
		LastValidatedAt: &lastValidatedAt,
	}
	created, err := db.UpsertAppleWebSession(ctx, want)
	if err != nil {
		t.Fatalf("upsert apple web session: %v", err)
	}
	assertAppleWebSessionFields(t, created, want)
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("session timestamps were not populated: %#v", created)
	}

	got, err := db.GetAppleWebSession(ctx, account.ID)
	if err != nil {
		t.Fatalf("get apple web session: %v", err)
	}
	assertAppleWebSessionFields(t, got, want)
	if !got.CreatedAt.Equal(created.CreatedAt) || !got.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("round-trip timestamps = (%v, %v), want (%v, %v)",
			got.CreatedAt, got.UpdatedAt, created.CreatedAt, created.UpdatedAt)
	}
}

func TestUpsertAppleWebSessionPreservesCreatedAtAndUpdatesFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
	firstValidation := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)

	first, err := db.UpsertAppleWebSession(ctx, domain.AppleWebSession{
		AccountID:       account.ID,
		Ciphertext:      "first-ciphertext",
		AppleID:         "first@icloud.com",
		Region:          "USA",
		Authenticated:   false,
		LastValidatedAt: &firstValidation,
	})
	if err != nil {
		t.Fatalf("insert apple web session: %v", err)
	}

	// Ensure the second write has a distinguishable update timestamp.
	time.Sleep(time.Millisecond)
	secondValidation := firstValidation.Add(2 * time.Hour)
	want := domain.AppleWebSession{
		AccountID:       account.ID,
		Ciphertext:      "second-ciphertext",
		AppleID:         "second@icloud.com",
		Region:          "CHN",
		Authenticated:   true,
		LastValidatedAt: &secondValidation,
	}
	updated, err := db.UpsertAppleWebSession(ctx, want)
	if err != nil {
		t.Fatalf("update apple web session: %v", err)
	}

	assertAppleWebSessionFields(t, updated, want)
	if !updated.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("updated CreatedAt = %v, want preserved value %v", updated.CreatedAt, first.CreatedAt)
	}
	if !updated.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("updated UpdatedAt = %v, want after %v", updated.UpdatedAt, first.UpdatedAt)
	}
}

func TestAppleWebSessionMissingValidationAndForeignKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")

	if _, err := db.GetAppleWebSession(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session lookup error = %v, want ErrNotFound", err)
	}
	if err := db.DeleteAppleWebSession(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session delete error = %v, want ErrNotFound", err)
	}

	valid := domain.AppleWebSession{
		AccountID:  account.ID,
		Ciphertext: "ciphertext",
		AppleID:    "primary@icloud.com",
	}
	for name, mutate := range map[string]func(*domain.AppleWebSession){
		"non-positive account id": func(session *domain.AppleWebSession) { session.AccountID = 0 },
		"empty ciphertext":        func(session *domain.AppleWebSession) { session.Ciphertext = " \t " },
		"empty apple id":          func(session *domain.AppleWebSession) { session.AppleID = " \t " },
	} {
		t.Run(name, func(t *testing.T) {
			session := valid
			mutate(&session)
			if _, err := db.UpsertAppleWebSession(ctx, session); err == nil {
				t.Fatal("invalid apple web session unexpectedly succeeded")
			}
		})
	}

	foreign := valid
	foreign.AccountID = account.ID + 1000
	if _, err := db.UpsertAppleWebSession(ctx, foreign); err == nil {
		t.Fatal("apple web session for missing account unexpectedly succeeded")
	}
	if _, err := db.GetAppleWebSession(ctx, foreign.AccountID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign-key failure left a session behind: %v", err)
	}
}

func TestDeleteAccountCascadesAppleWebSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")

	if _, err := db.UpsertAppleWebSession(ctx, domain.AppleWebSession{
		AccountID:  account.ID,
		Ciphertext: "ciphertext",
		AppleID:    "primary@icloud.com",
	}); err != nil {
		t.Fatalf("upsert apple web session: %v", err)
	}
	if err := db.DeleteAccount(ctx, account.ID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if _, err := db.GetAppleWebSession(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cascaded session lookup error = %v, want ErrNotFound", err)
	}
}

func TestUpdateAccountAppleWebSessionLifecycle(t *testing.T) {
	t.Parallel()

	t.Run("identity changes delete session without aliases", func(t *testing.T) {
		for name, changeIdentity := range map[string]func(*domain.Account){
			"email": func(account *domain.Account) {
				account.Email = "replacement@icloud.com"
			},
			"imap username": func(account *domain.Account) {
				account.IMAPUsername = "replacement@icloud.com"
			},
		} {
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				db := openTestStore(t)
				account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
				if _, err := db.UpsertAppleWebSession(ctx, domain.AppleWebSession{
					AccountID:     account.ID,
					Ciphertext:    "identity-session",
					AppleID:       account.Email,
					Region:        "CHN",
					Authenticated: true,
				}); err != nil {
					t.Fatalf("upsert apple web session: %v", err)
				}

				changeIdentity(&account)
				if _, err := db.UpdateAccount(ctx, account); err != nil {
					t.Fatalf("update account identity: %v", err)
				}
				if _, err := db.GetAppleWebSession(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("session after identity change error = %v, want ErrNotFound", err)
				}
			})
		}
	})

	t.Run("password rotation preserves complete session", func(t *testing.T) {
		ctx := context.Background()
		db := openTestStore(t)
		account := createAccount(t, ctx, db, "Primary", "primary@icloud.com")
		lastValidatedAt := time.Date(2026, 8, 7, 11, 45, 30, 987654321, time.UTC)
		before, err := db.UpsertAppleWebSession(ctx, domain.AppleWebSession{
			AccountID:       account.ID,
			Ciphertext:      "password-rotation-session",
			AppleID:         account.Email,
			Region:          "CHN",
			Authenticated:   true,
			LastValidatedAt: &lastValidatedAt,
		})
		if err != nil {
			t.Fatalf("upsert apple web session: %v", err)
		}

		account.PasswordCiphertext = "rotated-app-specific-password"
		if _, err := db.UpdateAccount(ctx, account); err != nil {
			t.Fatalf("rotate account password: %v", err)
		}
		after, err := db.GetAppleWebSession(ctx, account.ID)
		if err != nil {
			t.Fatalf("get session after password rotation: %v", err)
		}
		assertAppleWebSessionFields(t, after, before)
		if !after.CreatedAt.Equal(before.CreatedAt) || !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("session timestamps changed after password rotation: before=(%v, %v) after=(%v, %v)",
				before.CreatedAt, before.UpdatedAt, after.CreatedAt, after.UpdatedAt)
		}
	})
}

func assertAppleWebSessionFields(t *testing.T, got, want domain.AppleWebSession) {
	t.Helper()
	if got.AccountID != want.AccountID ||
		got.Ciphertext != want.Ciphertext ||
		got.AppleID != want.AppleID ||
		got.Region != want.Region ||
		got.Authenticated != want.Authenticated {
		t.Fatalf("apple web session = %#v, want fields from %#v", got, want)
	}
	if want.LastValidatedAt == nil {
		if got.LastValidatedAt != nil {
			t.Fatalf("LastValidatedAt = %v, want nil", got.LastValidatedAt)
		}
		return
	}
	if got.LastValidatedAt == nil || !got.LastValidatedAt.Equal(*want.LastValidatedAt) {
		t.Fatalf("LastValidatedAt = %v, want %v", got.LastValidatedAt, want.LastValidatedAt)
	}
}
