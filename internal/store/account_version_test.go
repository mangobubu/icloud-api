package store

import (
	"context"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

func TestAccountUpdatedAtAdvancesOneNanosecondWhenClockStallsOrRollsBack(t *testing.T) {
	ctx := context.Background()
	db := openAccountVersionTestStore(t)

	fixed := time.Date(2026, 8, 8, 12, 0, 0, 123, time.UTC)
	db.now = func() time.Time { return fixed }
	account, err := db.CreateAccount(ctx, domain.Account{
		Name:               "Version test",
		Email:              "version-test@icloud.com",
		IMAPHost:           "imap.mail.me.com",
		IMAPPort:           993,
		IMAPUsername:       "version-test@icloud.com",
		PasswordCiphertext: "encrypted",
		Enabled:            true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	steps := []struct {
		name string
		now  time.Time
	}{
		{name: "same clock", now: fixed},
		{name: "same clock again", now: fixed},
		{name: "clock rollback", now: fixed.Add(-time.Hour)},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			db.now = func() time.Time { return step.now }
			before, err := db.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatalf("get account before status update: %v", err)
			}
			syncedAt := fixed
			if err := db.SetAccountSyncStatus(ctx, account.ID, domain.SyncStatusPending, "", &syncedAt); err != nil {
				t.Fatalf("set account sync status: %v", err)
			}
			after, err := db.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatalf("get account after status update: %v", err)
			}
			want := before.UpdatedAt.Add(time.Nanosecond)
			if !after.UpdatedAt.Equal(want) {
				t.Fatalf("account updated_at = %v, want exactly previous +1ns (%v)", after.UpdatedAt, want)
			}
		})
	}
}

func TestMailboxWritesAdvanceAccountVersionOneNanosecond(t *testing.T) {
	ctx := context.Background()
	db := openAccountVersionTestStore(t)

	fixed := time.Date(2026, 8, 8, 13, 0, 0, 456, time.UTC)
	db.now = func() time.Time { return fixed }
	account, err := db.CreateAccount(ctx, domain.Account{
		Name:               "Mailbox version test",
		Email:              "mailbox-version-test@icloud.com",
		IMAPHost:           "imap.mail.me.com",
		IMAPPort:           993,
		IMAPUsername:       "mailbox-version-test@icloud.com",
		PasswordCiphertext: "encrypted",
		Enabled:            true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	alias, err := db.CreateAlias(ctx, domain.Alias{
		AccountID:  account.ID,
		Address:    "mailbox-version-alias@icloud.com",
		Label:      "Mailbox version alias",
		APIKeyHash: []byte("mailbox-version-hash"),
		Enabled:    true,
	})
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}

	assertMailboxVersionStep := func(name string, now time.Time, operation func(time.Time) error) {
		t.Helper()
		db.now = func() time.Time { return now }
		before, err := db.GetAccount(ctx, account.ID)
		if err != nil {
			t.Fatalf("get account before %s: %v", name, err)
		}
		if err := operation(before.UpdatedAt); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		after, err := db.GetAccount(ctx, account.ID)
		if err != nil {
			t.Fatalf("get account after %s: %v", name, err)
		}
		want := before.UpdatedAt.Add(time.Nanosecond)
		if !after.UpdatedAt.Equal(want) {
			t.Fatalf("account updated_at after %s = %v, want exactly previous +1ns (%v)", name, after.UpdatedAt, want)
		}
	}

	syncedAt := fixed
	assertMailboxVersionStep("apply mailbox sync", fixed, func(expected time.Time) error {
		return db.ApplyMailboxSync(ctx, account.ID, expected, []domain.Alias{alias}, domain.MailboxSyncResult{
			State: domain.IMAPSyncState{
				AccountID: account.ID, UIDValidity: 700, LastUID: 0, UpdatedAt: fixed,
			},
			Reset: true,
		}, syncedAt)
	})

	assertMailboxVersionStep("record mailbox sync failure", fixed.Add(-time.Hour), func(expected time.Time) error {
		return db.RecordMailboxSyncFailure(ctx, account.ID, expected, "temporary failure", fixed)
	})

}

func openAccountVersionTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(":memory:")
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
