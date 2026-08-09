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
	if disabled.LastError != "temporary upstream failure" {
		t.Fatalf("disabled schedule lost the diagnostic error: %#v", disabled)
	}
	if err := db.EnableAliasCreation(ctx, account.ID, planned, anchor.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	reenabled, err := db.GetAliasCreationSchedule(ctx, account.ID)
	if err != nil || !reenabled.Enabled || reenabled.LastError != "" {
		t.Fatalf("re-enabled schedule retained a stale error: %#v, err=%v", reenabled, err)
	}
	if err := db.DisableAliasCreation(ctx, account.ID, anchor.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.DisableAliasCreation(ctx, account.ID, anchor.Add(9*time.Minute)); err != nil {
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

func TestRotateAliasAPIKeyClearsPendingDeliveryAtomically(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Rotate pending key", "rotate-pending-key@icloud.com")
	oldHash := []byte("old-automatic-key-hash")
	created, _, err := db.CreateAliasWithPendingAPIKey(ctx,
		domain.AppleWebSession{
			AccountID: account.ID, Ciphertext: "as1.rotate-pending", AppleID: account.Email,
			Region: "global", Authenticated: true,
		},
		domain.Alias{
			AccountID: account.ID, Address: "rotate-pending-alias@icloud.com",
			APIKeyHash: oldHash, APIKeyPrefix: "icm_old", Enabled: true,
		}, "ak1.old-pending-ciphertext")
	if err != nil {
		t.Fatalf("create alias with pending key: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_pending_alias_key_delete
		BEFORE DELETE ON pending_alias_api_keys
		BEGIN SELECT RAISE(ABORT, 'forced pending key delete failure'); END`); err != nil {
		t.Fatalf("create pending key delete trigger: %v", err)
	}

	newHash := []byte("new-automatic-key-hash")
	if _, err := db.RotateAliasAPIKey(ctx, created.ID, newHash, "icm_new"); err == nil {
		t.Fatal("api key rotation unexpectedly succeeded when pending key deletion failed")
	}
	stored, err := db.GetAlias(ctx, created.ID)
	if err != nil {
		t.Fatalf("get alias after rolled back rotation: %v", err)
	}
	if string(stored.APIKeyHash) != string(oldHash) || stored.APIKeyPrefix != "icm_old" {
		t.Fatalf("rolled back alias key = (%q, %q)", stored.APIKeyHash, stored.APIKeyPrefix)
	}
	pending, err := db.ListPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil || len(pending) != 1 || pending[0].APIKeyCiphertext != "ak1.old-pending-ciphertext" {
		t.Fatalf("pending key after rolled back rotation = %#v, err=%v", pending, err)
	}

	if _, err := db.DB().ExecContext(ctx, `DROP TRIGGER fail_pending_alias_key_delete`); err != nil {
		t.Fatalf("drop pending key delete trigger: %v", err)
	}
	rotated, err := db.RotateAliasAPIKey(ctx, created.ID, newHash, "icm_new")
	if err != nil {
		t.Fatalf("rotate alias api key: %v", err)
	}
	if string(rotated.APIKeyHash) != string(newHash) || rotated.APIKeyPrefix != "icm_new" {
		t.Fatalf("rotated alias key = (%q, %q)", rotated.APIKeyHash, rotated.APIKeyPrefix)
	}
	if count, err := db.CountPendingAliasAPIKeysByAccount(ctx, account.ID); err != nil || count != 0 {
		t.Fatalf("pending key count after rotation = %d, err=%v", count, err)
	}
	if _, err := db.GetMailboxBindingByAPIKeyHash(ctx, oldHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old api key binding error = %v, want ErrNotFound", err)
	}
	binding, err := db.GetMailboxBindingByAPIKeyHash(ctx, newHash)
	if err != nil || binding.Alias.ID != created.ID {
		t.Fatalf("new api key binding = %#v, err=%v", binding, err)
	}
}

func TestPendingAutoAliasConfirmationPublishesOnlyAfterConfirmation(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Delayed confirmation", "delayed-confirmation@icloud.com")
	baselineAlias := createAlias(t, ctx, db, account.ID, "baseline@icloud.com", []byte("baseline-auto-confirmation-hash"))

	syncedAt := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID,
		[]domain.Alias{baselineAlias},
		domain.MailboxSyncResult{
			Messages: map[int64]domain.LatestMessage{},
			State: domain.IMAPSyncState{
				AccountID: account.ID, UIDValidity: 19, LastUID: 23, UpdatedAt: syncedAt,
			},
			Reset: true,
		}, syncedAt,
	); err != nil {
		t.Fatalf("seed mailbox cursor: %v", err)
	}
	versionBeforePending := currentAccountVersion(t, ctx, db, account.ID)

	created, saved, err := db.CreateAliasWithPendingAPIKey(ctx,
		domain.AppleWebSession{
			AccountID: account.ID, Ciphertext: "as1.pending", AppleID: account.Email,
			Region: "global", Authenticated: true,
		},
		domain.Alias{
			AccountID: account.ID, Address: "eventually-visible@icloud.com", Label: "eventually visible",
			APIKeyHash: []byte("eventually-visible-hash"), APIKeyPrefix: "icm_pending", Enabled: false,
			LastSyncStatus: domain.SyncStatusError, LastSyncError: "caller diagnostic must not replace marker",
		}, "ak1.pending-ciphertext")
	if err != nil {
		t.Fatalf("persist pending confirmation: %v", err)
	}
	if created.Enabled || created.LastSyncStatus != domain.SyncStatusPending ||
		created.LastSyncError != domain.AppleAliasConfirmationPending {
		t.Fatalf("pending alias state = %#v", created)
	}
	if saved.Ciphertext != "as1.pending" {
		t.Fatalf("saved pending session = %#v", saved)
	}
	if got := currentAccountVersion(t, ctx, db, account.ID); !got.Equal(versionBeforePending) {
		t.Fatalf("disabled pending alias advanced account version from %v to %v", versionBeforePending, got)
	}
	if state, err := db.GetIMAPSyncState(ctx, account.ID); err != nil || state.LastUID != 23 {
		t.Fatalf("cursor after pending persistence = %#v, err=%v", state, err)
	}

	listed, err := db.ListPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil || len(listed) != 0 {
		t.Fatalf("retrievable keys before confirmation = %#v, err=%v", listed, err)
	}
	if count, err := db.CountPendingAliasAPIKeysByAccount(ctx, account.ID); err != nil || count != 0 {
		t.Fatalf("retrievable key count before confirmation = %d, err=%v", count, err)
	}
	pending, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID)
	if err != nil {
		t.Fatalf("get pending confirmation: %v", err)
	}
	if pending.Alias.ID != created.ID || pending.APIKeyCiphertext != "ak1.pending-ciphertext" {
		t.Fatalf("pending confirmation = %#v", pending)
	}
	if err := db.DeletePendingAliasAPIKeys(ctx, account.ID, []int64{created.ID}); err != nil {
		t.Fatalf("ignore early pending-key acknowledgement: %v", err)
	}
	if _, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID); err != nil {
		t.Fatalf("early acknowledgement deleted pending confirmation: %v", err)
	}

	confirmed, rotated, err := db.ConfirmPendingAutoAlias(ctx, domain.AppleWebSession{
		AccountID: account.ID, Ciphertext: "as1.confirmed", AppleID: account.Email,
		Region: "global", Authenticated: true,
	}, created.ID)
	if err != nil {
		t.Fatalf("confirm pending alias: %v", err)
	}
	if !confirmed.Enabled || confirmed.LastSyncStatus != domain.SyncStatusPending || confirmed.LastSyncError != "" {
		t.Fatalf("confirmed alias state = %#v", confirmed)
	}
	if rotated.Ciphertext != "as1.confirmed" {
		t.Fatalf("rotated confirmation session = %#v", rotated)
	}
	storedSession, err := db.GetAppleWebSession(ctx, account.ID)
	if err != nil || storedSession.Ciphertext != "as1.confirmed" {
		t.Fatalf("stored confirmation session = %#v, err=%v", storedSession, err)
	}
	if _, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pending confirmation after publish error = %v, want ErrNotFound", err)
	}
	if _, err := db.GetIMAPSyncState(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cursor after confirmation error = %v, want ErrNotFound", err)
	}
	if got := currentAccountVersion(t, ctx, db, account.ID); !got.After(versionBeforePending) {
		t.Fatalf("confirmed alias did not advance account version: before=%v after=%v", versionBeforePending, got)
	}
	listed, err = db.ListPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil || len(listed) != 1 || listed[0].Alias.ID != created.ID {
		t.Fatalf("retrievable keys after confirmation = %#v, err=%v", listed, err)
	}
	if count, err := db.CountPendingAliasAPIKeysByAccount(ctx, account.ID); err != nil || count != 1 {
		t.Fatalf("retrievable key count after confirmation = %d, err=%v", count, err)
	}
	if err := db.SetAliasEnabled(ctx, created.ID, false); err != nil {
		t.Fatalf("disable confirmed alias: %v", err)
	}
	listed, err = db.ListPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil || len(listed) != 1 || listed[0].Alias.Enabled {
		t.Fatalf("unclaimed key after manual disable = %#v, err=%v", listed, err)
	}
}

func TestPendingAutoAliasConfirmationWaitsForEnabledCapacity(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Pending at capacity", "pending-at-capacity@icloud.com")
	insertEnabledAliasFixtures(t, ctx, db, account.ID, domain.MaxEnabledAliasesPerAccount)

	created, _, err := db.CreateAliasWithPendingAPIKey(ctx,
		domain.AppleWebSession{
			AccountID: account.ID, Ciphertext: "as1.capacity-before", AppleID: account.Email,
			Region: "global", Authenticated: true,
		},
		domain.Alias{
			AccountID: account.ID, Address: "pending-over-capacity@icloud.com", Label: "pending over capacity",
			APIKeyHash: []byte("pending-over-capacity-hash"), APIKeyPrefix: "icm_capacity", Enabled: false,
		}, "ak1.capacity-ciphertext")
	if err != nil {
		t.Fatalf("persist pending alias at enabled capacity: %v", err)
	}
	assertAliasCounts(t, ctx, db, account.ID, domain.MaxEnabledAliasesPerAccount+1, domain.MaxEnabledAliasesPerAccount)

	_, _, err = db.ConfirmPendingAutoAlias(ctx, domain.AppleWebSession{
		AccountID: account.ID, Ciphertext: "as1.capacity-after", AppleID: account.Email,
		Region: "global", Authenticated: true,
	}, created.ID)
	if !errors.Is(err, store.ErrAliasLimit) {
		t.Fatalf("confirmation at enabled capacity error = %v, want ErrAliasLimit", err)
	}
	stored, err := db.GetAlias(ctx, created.ID)
	if err != nil {
		t.Fatalf("get capacity-blocked alias: %v", err)
	}
	if stored.Enabled || stored.LastSyncError != domain.AppleAliasConfirmationPending {
		t.Fatalf("capacity-blocked alias state = %#v", stored)
	}
	if _, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID); err != nil {
		t.Fatalf("capacity failure lost pending confirmation: %v", err)
	}
	if count, err := db.CountPendingAliasAPIKeysByAccount(ctx, account.ID); err != nil || count != 0 {
		t.Fatalf("capacity-blocked key count = %d, err=%v", count, err)
	}
	session, err := db.GetAppleWebSession(ctx, account.ID)
	if err != nil || session.Ciphertext != "as1.capacity-before" {
		t.Fatalf("capacity failure partially rotated session = %#v, err=%v", session, err)
	}
}

func TestPendingConfirmationMarkerDoesNotHideEnabledAliasKey(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Marker collision", "marker-collision@icloud.com")

	created, _, err := db.CreateAliasWithPendingAPIKey(ctx,
		domain.AppleWebSession{
			AccountID: account.ID, Ciphertext: "as1.marker-collision", AppleID: account.Email,
			Region: "global", Authenticated: true,
		},
		domain.Alias{
			AccountID: account.ID, Address: "marker-collision-alias@icloud.com", Label: "marker collision",
			APIKeyHash: []byte("marker-collision-hash"), APIKeyPrefix: "icm_marker", Enabled: true,
			LastSyncError: domain.AppleAliasConfirmationPending,
		}, "ak1.marker-collision-ciphertext")
	if err != nil {
		t.Fatalf("create enabled marker-collision alias: %v", err)
	}
	if _, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("enabled alias treated as pending confirmation: %v", err)
	}
	listed, err := db.ListPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil || len(listed) != 1 || listed[0].Alias.ID != created.ID {
		t.Fatalf("marker-collision key list = %#v, err=%v", listed, err)
	}
	if err := db.DeletePendingAliasAPIKey(ctx, account.ID, created.ID); err != nil {
		t.Fatalf("acknowledge marker-collision key: %v", err)
	}
	if count, err := db.CountPendingAliasAPIKeysByAccount(ctx, account.ID); err != nil || count != 0 {
		t.Fatalf("marker-collision key count after acknowledgement = %d, err=%v", count, err)
	}
}

func TestPendingAutoAliasConfirmationCanUseReservedLastSlot(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Reserved last slot", "reserved-last-slot@icloud.com")
	insertEnabledAliasFixtures(t, ctx, db, account.ID, domain.MaxEnabledAliasesPerAccount-1)

	created, _, err := db.CreateAliasWithPendingAPIKey(ctx,
		domain.AppleWebSession{
			AccountID: account.ID, Ciphertext: "as1.reserved-slot", AppleID: account.Email,
			Region: "global", Authenticated: true,
		},
		domain.Alias{
			AccountID: account.ID, Address: "reserved-last-slot-alias@icloud.com", Label: "reserved last slot",
			APIKeyHash: []byte("reserved-last-slot-hash"), APIKeyPrefix: "icm_reserved", Enabled: false,
		}, "ak1.reserved-slot-ciphertext")
	if err != nil {
		t.Fatalf("persist alias in reserved last slot: %v", err)
	}

	confirmed, _, err := db.ConfirmPendingAutoAlias(ctx, domain.AppleWebSession{
		AccountID: account.ID, Ciphertext: "as1.reserved-slot-confirmed", AppleID: account.Email,
		Region: "global", Authenticated: true,
	}, created.ID)
	if err != nil {
		t.Fatalf("confirm alias in reserved last slot: %v", err)
	}
	if !confirmed.Enabled || confirmed.LastSyncError != "" {
		t.Fatalf("confirmed last-slot alias = %#v", confirmed)
	}
	assertAliasCounts(t, ctx, db, account.ID, domain.MaxEnabledAliasesPerAccount, domain.MaxEnabledAliasesPerAccount)
}
