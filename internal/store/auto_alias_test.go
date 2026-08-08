package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestAliasCreationScheduleCASAndLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account, err := db.CreateAccount(ctx, domain.Account{
		Name: "scheduled", Email: "scheduled@icloud.com", IMAPHost: "imap.mail.me.com",
		IMAPPort: 993, IMAPUsername: "scheduled@icloud.com", PasswordCiphertext: "im1.fixture", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	planned := []time.Time{anchor.Add(5 * time.Minute), anchor.Add(20 * time.Minute), anchor.Add(35 * time.Minute)}
	if err := db.EnableAliasCreation(ctx, account.ID, planned, anchor); err != nil {
		t.Fatal(err)
	}
	schedule, err := db.GetAliasCreationSchedule(ctx, account.ID)
	if err != nil || !schedule.Enabled || len(schedule.PlannedAt) != len(planned) {
		t.Fatalf("schedule = %#v, err=%v", schedule, err)
	}
	due, err := db.ListDueAliasCreationSchedules(ctx, anchor.Add(5*time.Minute))
	if err != nil || len(due) != 1 {
		t.Fatalf("due schedules = %#v, err=%v", due, err)
	}
	wrong := anchor.Add(6 * time.Minute)
	claimed, err := db.ClaimAliasCreation(ctx, account.ID, wrong, planned[1:], anchor.Add(5*time.Minute))
	if err != nil || claimed {
		t.Fatalf("wrong CAS = (%v, %v), want false", claimed, err)
	}
	claimed, err = db.ClaimAliasCreation(ctx, account.ID, *schedule.NextRunAt, planned[1:], anchor.Add(5*time.Minute))
	if err != nil || !claimed {
		t.Fatalf("claim = (%v, %v), want true", claimed, err)
	}
	if err := db.RecordAliasCreationFailure(ctx, account.ID, anchor.Add(5*time.Minute), "temporary upstream failure"); err != nil {
		t.Fatal(err)
	}
	current, err := db.GetAliasCreationSchedule(ctx, account.ID)
	if err != nil || current.LastError != "temporary upstream failure" || current.NextRunAt == nil {
		t.Fatalf("recorded failure schedule = %#v, err=%v", current, err)
	}
	if err := db.DisableAliasCreation(ctx, account.ID, anchor.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	disabled, err := db.GetAliasCreationSchedule(ctx, account.ID)
	if err != nil || disabled.Enabled || disabled.NextRunAt != nil || len(disabled.PlannedAt) != 0 {
		t.Fatalf("disabled schedule = %#v, err=%v", disabled, err)
	}
	if err := db.DisableAliasCreation(ctx, account.ID, anchor.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAliasCreationSchedule(ctx, account.ID+100); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing schedule error = %v", err)
	}
}

func TestDisablingAccountClosesAutomaticAliasSchedule(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "disable-schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account, err := db.CreateAccount(ctx, domain.Account{
		Name: "scheduled disable", Email: "scheduled-disable@icloud.com", IMAPHost: "imap.mail.me.com",
		IMAPPort: 993, IMAPUsername: "scheduled-disable@icloud.com", PasswordCiphertext: "im1.fixture", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	planned := []time.Time{anchor.Add(5 * time.Minute), anchor.Add(20 * time.Minute)}
	if err := db.EnableAliasCreation(ctx, account.ID, planned, anchor); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateAccount(ctx, domain.Account{
		ID: account.ID, Name: account.Name, Email: account.Email,
		IMAPHost: account.IMAPHost, IMAPPort: account.IMAPPort,
		IMAPUsername: account.IMAPUsername, Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	schedule, err := db.GetAliasCreationSchedule(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.Enabled || schedule.NextRunAt != nil || len(schedule.PlannedAt) != 0 {
		t.Fatalf("schedule after disabling account = %#v, want no future work", schedule)
	}
	if err := db.EnableAliasCreation(ctx, account.ID, planned, anchor); !errors.Is(err, store.ErrAccountDisabled) {
		t.Fatalf("enabling schedule for disabled account error = %v, want ErrAccountDisabled", err)
	}
}

func TestCreateAliasWithPendingKeyPublishesAtomically(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "auto-alias.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account, err := db.CreateAccount(ctx, domain.Account{
		Name: "automatic", Email: "automatic@icloud.com", IMAPHost: "imap.mail.me.com",
		IMAPPort: 993, IMAPUsername: "automatic@icloud.com", PasswordCiphertext: "im1.fixture", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, saved, err := db.CreateAliasWithPendingAPIKey(ctx,
		domain.AppleWebSession{
			AccountID: account.ID, Ciphertext: "as1.fixture", AppleID: account.Email,
			Region: "global", Authenticated: true,
		},
		domain.Alias{
			AccountID: account.ID, Address: "automatic-alias@icloud.com", Label: "自动创建",
			APIKeyHash: []byte("hash-for-automatic-alias"), APIKeyPrefix: "icm_test", Enabled: true,
		}, "ak1.fixture-ciphertext")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID < 1 || created.Address != "automatic-alias@icloud.com" || saved.AccountID != account.ID {
		t.Fatalf("created alias/session = %#v / %#v", created, saved)
	}
	pending, err := db.ListPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %#v, err=%v", pending, err)
	}
	if pending[0].Alias.ID != created.ID || pending[0].APIKeyCiphertext != "ak1.fixture-ciphertext" {
		t.Fatalf("pending record = %#v", pending[0])
	}
	count, err := db.CountPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil || count != 1 {
		t.Fatalf("pending count = %d, err=%v", count, err)
	}
	if err := db.DeletePendingAliasAPIKeys(ctx, account.ID, []int64{created.ID + 1}); err != nil {
		t.Fatal(err)
	}
	count, _ = db.CountPendingAliasAPIKeysByAccount(ctx, account.ID)
	if count != 1 {
		t.Fatalf("wrong-account/id delete changed count to %d", count)
	}
	if err := db.DeletePendingAliasAPIKeys(ctx, account.ID, []int64{created.ID}); err != nil {
		t.Fatal(err)
	}
	count, _ = db.CountPendingAliasAPIKeysByAccount(ctx, account.ID)
	if count != 0 {
		t.Fatalf("acknowledgement left %d pending records", count)
	}
}
