package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestConsumeLatestMessageClaimsExactSnapshotAndQueuesSeen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Consume", "consume@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "consume-alias@icloud.com", []byte("consume-hash"))
	base := time.Date(2026, 8, 8, 2, 0, 0, 0, time.UTC)
	mustUpsert(t, ctx, db, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 100, UID: 11,
		InternalDate: base.Add(-time.Minute), SyncedAt: base,
	})
	alias = markAliasSyncedForConsumption(t, ctx, db, alias, base)

	for _, position := range []struct {
		uidValidity uint32
		uid         uint32
	}{{99, 11}, {100, 10}} {
		consumed, err := consumeSnapshot(ctx, db, alias, position.uidValidity, position.uid, base)
		if err != nil || consumed {
			t.Fatalf("consume stale position (%d, %d) = (%v, %v), want false, nil",
				position.uidValidity, position.uid, consumed, err)
		}
	}
	assertSeenTaskCount(t, ctx, db, 0)

	consumed, err := consumeSnapshot(ctx, db, alias, 100, 11, base)
	if err != nil || !consumed {
		t.Fatalf("consume current snapshot = (%v, %v), want true, nil", consumed, err)
	}
	consumed, err = consumeSnapshot(ctx, db, alias, 100, 11, base.Add(time.Hour))
	if err != nil || consumed {
		t.Fatalf("consume current snapshot again = (%v, %v), want false, nil", consumed, err)
	}

	tasks, err := db.ListSeenTasks(ctx, account.ID, 10)
	if err != nil {
		t.Fatalf("list queued seen task: %v", err)
	}
	if len(tasks) != 1 || tasks[0].AccountID != account.ID || tasks[0].UIDValidity != 100 ||
		tasks[0].UID != 11 || !tasks[0].CreatedAt.Equal(base) {
		t.Fatalf("queued seen tasks = %#v", tasks)
	}

	newerAt := base.Add(2 * time.Hour)
	if err := db.ReplaceLatestMessage(ctx, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 100, UID: 12,
		InternalDate: newerAt.Add(-time.Minute), SyncedAt: newerAt,
	}); err != nil {
		t.Fatalf("publish newer snapshot: %v", err)
	}
	alias = markAliasSyncedForConsumption(t, ctx, db, alias, newerAt)
	consumed, err = consumeSnapshot(ctx, db, alias, 100, 11, newerAt)
	if err != nil || consumed {
		t.Fatalf("consume replaced snapshot = (%v, %v), want false, nil", consumed, err)
	}
	consumed, err = consumeSnapshot(ctx, db, alias, 100, 12, newerAt)
	if err != nil || !consumed {
		t.Fatalf("consume newer snapshot = (%v, %v), want true, nil", consumed, err)
	}

	var count int
	if err := db.DB().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM consumed_messages WHERE alias_id = ?`, alias.ID,
	).Scan(&count); err != nil {
		t.Fatalf("inspect consumption history: %v", err)
	}
	if count != 2 {
		t.Fatalf("consumption history rows = %d, want 2", count)
	}

	// An authoritative snapshot can move backwards after a newer message is
	// deleted. Strict history keeps that older identity consumed.
	rollbackAt := newerAt.Add(time.Hour)
	if err := db.ReplaceLatestMessage(ctx, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 100, UID: 11,
		InternalDate: rollbackAt.Add(-time.Minute), SyncedAt: rollbackAt,
	}); err != nil {
		t.Fatalf("roll snapshot back to consumed UID: %v", err)
	}
	alias = markAliasSyncedForConsumption(t, ctx, db, alias, rollbackAt)
	consumed, err = consumeSnapshot(ctx, db, alias, 100, 11, rollbackAt)
	if err != nil || consumed {
		t.Fatalf("consume rolled-back historical snapshot = (%v, %v), want false, nil", consumed, err)
	}
}

func TestConsumeLatestMessageRollsBackClaimWhenQueueInsertFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Rollback", "consume-rollback@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "consume-rollback-alias@icloud.com", []byte("consume-rollback-hash"))
	at := time.Date(2026, 8, 8, 3, 0, 0, 0, time.UTC)
	mustUpsert(t, ctx, db, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 20, UID: 5, InternalDate: at, SyncedAt: at,
	})
	alias = markAliasSyncedForConsumption(t, ctx, db, alias, at)
	if _, err := db.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_seen_enqueue
		BEFORE INSERT ON imap_seen_tasks
		BEGIN SELECT RAISE(ABORT, 'forced seen enqueue failure'); END`); err != nil {
		t.Fatalf("create seen queue failure trigger: %v", err)
	}

	if consumed, err := consumeSnapshot(ctx, db, alias, 20, 5, at); err == nil || consumed {
		t.Fatalf("consume with rejected queue = (%v, %v), want false and error", consumed, err)
	}
	var claims int
	if err := db.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM consumed_messages WHERE alias_id = ?`, alias.ID,
	).Scan(&claims); err != nil {
		t.Fatalf("count rolled back claims: %v", err)
	}
	if claims != 0 {
		t.Fatalf("consumption claim survived failed enqueue: %d", claims)
	}
}

func TestConsumeLatestMessageRequiresEnabledAliasAndAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	at := time.Date(2026, 8, 8, 3, 30, 0, 0, time.UTC)

	aliasAccount := createAccount(t, ctx, db, "Disabled alias", "disabled-alias-account@icloud.com")
	disabledAlias := createAlias(
		t, ctx, db, aliasAccount.ID, "disabled-consume-alias@icloud.com", []byte("disabled-consume-alias-hash"),
	)
	mustUpsert(t, ctx, db, domain.LatestMessage{
		AliasID: disabledAlias.ID, UIDValidity: 21, UID: 1, InternalDate: at, SyncedAt: at,
	})
	disabledAlias = markAliasSyncedForConsumption(t, ctx, db, disabledAlias, at)
	if err := db.SetAliasEnabled(ctx, disabledAlias.ID, false); err != nil {
		t.Fatalf("disable alias before consumption: %v", err)
	}
	if consumed, err := consumeSnapshot(ctx, db, disabledAlias, 21, 1, at); err != nil || consumed {
		t.Fatalf("consume disabled alias snapshot = (%v, %v), want false, nil", consumed, err)
	}

	disabledAccount := createAccount(t, ctx, db, "Disabled account", "disabled-consume-account@icloud.com")
	accountAlias := createAlias(
		t, ctx, db, disabledAccount.ID, "disabled-account-alias@icloud.com", []byte("disabled-account-alias-hash"),
	)
	mustUpsert(t, ctx, db, domain.LatestMessage{
		AliasID: accountAlias.ID, UIDValidity: 22, UID: 2, InternalDate: at, SyncedAt: at,
	})
	accountAlias = markAliasSyncedForConsumption(t, ctx, db, accountAlias, at)
	disabledAccount.Enabled = false
	if _, err := db.UpdateAccount(ctx, disabledAccount); err != nil {
		t.Fatalf("disable account before consumption: %v", err)
	}
	if consumed, err := consumeSnapshot(ctx, db, accountAlias, 22, 2, at); err != nil || consumed {
		t.Fatalf("consume disabled account snapshot = (%v, %v), want false, nil", consumed, err)
	}
	assertSeenTaskCount(t, ctx, db, 0)
}

func TestConsumeLatestMessageRequiresCurrentCredentialAndPublishedSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Consume CAS", "consume-cas@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "consume-cas-alias@icloud.com", []byte("consume-cas-hash"))
	base := time.Date(2026, 8, 8, 3, 45, 0, 0, time.UTC)
	mustUpsert(t, ctx, db, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 25, UID: 3, InternalDate: base, SyncedAt: base,
	})
	alias = markAliasSyncedForConsumption(t, ctx, db, alias, base)
	originalBinding := alias

	for name, consume := range map[string]func() (bool, error){
		"rotated key": func() (bool, error) {
			return db.ConsumeLatestMessage(
				ctx, alias.ID, []byte("different-hash"), base, base, 25, 3, base,
			)
		},
		"different sync version": func() (bool, error) {
			return db.ConsumeLatestMessage(
				ctx, alias.ID, alias.APIKeyHash, base.Add(-time.Nanosecond), base, 25, 3, base,
			)
		},
	} {
		t.Run(name, func(t *testing.T) {
			consumed, err := consume()
			if err != nil || consumed {
				t.Fatalf("consume with stale binding = (%v, %v), want false, nil", consumed, err)
			}
		})
	}

	if err := db.UpdateAliasSyncStatus(ctx, alias.ID, domain.SyncStatusError, "sync failed", &base); err != nil {
		t.Fatalf("mark alias sync failed: %v", err)
	}
	if consumed, err := consumeSnapshot(ctx, db, alias, 25, 3, base); err != nil || consumed {
		t.Fatalf("consume failed-sync snapshot = (%v, %v), want false, nil", consumed, err)
	}

	refreshed := base.Add(time.Minute)
	alias = markAliasSyncedForConsumption(t, ctx, db, alias, refreshed)
	if consumed, err := consumeSnapshot(ctx, db, alias, 25, 3, refreshed); err != nil || consumed {
		t.Fatalf("consume snapshot from older publication = (%v, %v), want false, nil", consumed, err)
	}
	if err := db.ReplaceLatestMessage(ctx, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 25, UID: 3, InternalDate: base, SyncedAt: refreshed,
	}); err != nil {
		t.Fatalf("republish same UID: %v", err)
	}
	if consumed, err := consumeSnapshot(ctx, db, originalBinding, 25, 3, refreshed); err != nil || consumed {
		t.Fatalf("consume with old sync binding = (%v, %v), want false, nil", consumed, err)
	}
	if consumed, err := consumeSnapshot(ctx, db, alias, 25, 3, refreshed); err != nil || !consumed {
		t.Fatalf("consume current credential and sync = (%v, %v), want true, nil", consumed, err)
	}

	unchanged := createAlias(
		t, ctx, db, account.ID, "consume-unchanged-alias@icloud.com", []byte("consume-unchanged-hash"),
	)
	mustUpsert(t, ctx, db, domain.LatestMessage{
		AliasID: unchanged.ID, UIDValidity: 25, UID: 4, InternalDate: base, SyncedAt: base,
	})
	unchanged = markAliasSyncedForConsumption(t, ctx, db, unchanged, refreshed)
	consumed, err := db.ConsumeLatestMessage(
		ctx, unchanged.ID, unchanged.APIKeyHash, *unchanged.LastSyncedAt, base,
		25, 4, refreshed,
	)
	if err != nil || !consumed {
		t.Fatalf("consume unchanged snapshot after sync refresh = (%v, %v), want true, nil", consumed, err)
	}
	assertSeenTaskCount(t, ctx, db, 2)
}

func TestConsumeLatestMessageAllowsOnlyOneConcurrentClaim(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Concurrent consume", "concurrent-consume@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "concurrent-consume-alias@icloud.com", []byte("concurrent-consume-hash"))
	at := time.Date(2026, 8, 8, 4, 0, 0, 0, time.UTC)
	mustUpsert(t, ctx, db, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 30, UID: 7, InternalDate: at, SyncedAt: at,
	})
	alias = markAliasSyncedForConsumption(t, ctx, db, alias, at)

	const callers = 16
	start := make(chan struct{})
	results := make(chan bool, callers)
	errorsByCall := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for index := 0; index < callers; index++ {
		go func() {
			ready.Done()
			<-start
			consumed, err := consumeSnapshot(ctx, db, alias, 30, 7, at)
			results <- consumed
			errorsByCall <- err
		}()
	}
	ready.Wait()
	close(start)

	successes := 0
	for index := 0; index < callers; index++ {
		if err := <-errorsByCall; err != nil {
			t.Errorf("concurrent consume %d: %v", index, err)
		}
		if <-results {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent claims = %d, want 1", successes)
	}
	assertSeenTaskCount(t, ctx, db, 1)
}

func TestSeenQueueDeduplicatesPhysicalMessageAcrossAliases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Deduplicate", "seen-deduplicate@icloud.com")
	first := createAlias(t, ctx, db, account.ID, "seen-first@icloud.com", []byte("seen-first-hash"))
	second := createAlias(t, ctx, db, account.ID, "seen-second@icloud.com", []byte("seen-second-hash"))
	at := time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC)
	for _, alias := range []domain.Alias{first, second} {
		mustUpsert(t, ctx, db, domain.LatestMessage{
			AliasID: alias.ID, UIDValidity: 40, UID: 8, InternalDate: at, SyncedAt: at,
		})
		alias = markAliasSyncedForConsumption(t, ctx, db, alias, at)
		consumed, err := consumeSnapshot(ctx, db, alias, 40, 8, at)
		if err != nil || !consumed {
			t.Fatalf("consume alias %d = (%v, %v)", alias.ID, consumed, err)
		}
	}
	assertSeenTaskCount(t, ctx, db, 1)
}

func TestSeenTaskListingAndExactDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	firstAccount := createAccount(t, ctx, db, "First queue", "seen-queue-first@icloud.com")
	secondAccount := createAccount(t, ctx, db, "Second queue", "seen-queue-second@icloud.com")
	base := time.Date(2026, 8, 8, 6, 0, 0, 0, time.UTC)

	type fixture struct {
		account  domain.Account
		address  string
		hash     string
		validity uint32
		uid      uint32
		at       time.Time
	}
	fixtures := []fixture{
		{firstAccount, "queue-first-current@icloud.com", "queue-first-current", 50, 1, base},
		{secondAccount, "queue-second@icloud.com", "queue-second", 50, 1, base.Add(time.Minute)},
		{firstAccount, "queue-first-old@icloud.com", "queue-first-old", 49, 2, base.Add(2 * time.Minute)},
	}
	for _, item := range fixtures {
		alias := createAlias(t, ctx, db, item.account.ID, item.address, []byte(item.hash))
		mustUpsert(t, ctx, db, domain.LatestMessage{
			AliasID: alias.ID, UIDValidity: item.validity, UID: item.uid,
			InternalDate: item.at, SyncedAt: item.at,
		})
		alias = markAliasSyncedForConsumption(t, ctx, db, alias, item.at)
		consumed, err := consumeSnapshot(ctx, db, alias, item.validity, item.uid, item.at)
		if err != nil || !consumed {
			t.Fatalf("seed seen task %s = (%v, %v)", item.address, consumed, err)
		}
	}

	accounts, err := db.ListSeenTaskAccountIDs(ctx)
	if err != nil {
		t.Fatalf("list queued accounts: %v", err)
	}
	if len(accounts) != 2 || accounts[0] != firstAccount.ID || accounts[1] != secondAccount.ID {
		t.Fatalf("queued account IDs = %v", accounts)
	}
	firstTasks, err := db.ListSeenTasks(ctx, firstAccount.ID, 10)
	if err != nil {
		t.Fatalf("list first account tasks: %v", err)
	}
	if len(firstTasks) != 2 || firstTasks[0].UIDValidity != 50 || firstTasks[1].UIDValidity != 49 {
		t.Fatalf("first account tasks = %#v", firstTasks)
	}

	if err := db.DeleteSeenTasks(ctx, firstAccount.ID, 50, []uint32{1, 1}); err != nil {
		t.Fatalf("delete exact current-generation task: %v", err)
	}
	assertSeenTaskCount(t, ctx, db, 2)
	if err := db.DeleteSeenTasks(ctx, firstAccount.ID, 49, []uint32{2}); err != nil {
		t.Fatalf("delete obsolete first-account task: %v", err)
	}
	remaining, err := db.ListSeenTasks(ctx, secondAccount.ID, 10)
	if err != nil {
		t.Fatalf("list remaining task: %v", err)
	}
	if len(remaining) != 1 || remaining[0].AccountID != secondAccount.ID ||
		remaining[0].UIDValidity != 50 || remaining[0].UID != 1 {
		t.Fatalf("remaining task = %#v", remaining)
	}
}

func TestMigrateV4ToV6AddsConsumptionAndSeenQueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-v4-seen.db")
	current, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("create current fixture: %v", err)
	}
	account := createAccount(t, ctx, current, "Legacy v4", "legacy-v4-seen@icloud.com")
	alias := createAlias(t, ctx, current, account.ID, "legacy-v4-alias@icloud.com", []byte("legacy-v4-seen-hash"))
	at := time.Date(2026, 8, 8, 7, 0, 0, 0, time.UTC)
	mustUpsert(t, ctx, current, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 60, UID: 3, InternalDate: at, SyncedAt: at,
	})
	alias = markAliasSyncedForConsumption(t, ctx, current, alias, at)
	for _, statement := range []string{
		`DROP TABLE imap_seen_tasks`,
		`DROP TABLE consumed_messages`,
		`DROP TABLE pending_alias_api_keys`,
		`DROP TABLE alias_creation_schedules`,
		`PRAGMA user_version = 4`,
	} {
		if _, err := current.DB().ExecContext(ctx, statement); err != nil {
			_ = current.Close()
			t.Fatalf("prepare v4 fixture with %q: %v", statement, err)
		}
	}
	if err := current.Close(); err != nil {
		t.Fatalf("close v4 fixture: %v", err)
	}

	migrated, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("migrate v4 fixture: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })
	var version int
	if err := migrated.DB().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read migrated schema version: %v", err)
	}
	if version != 6 {
		t.Fatalf("migrated schema version = %d, want 6", version)
	}
	consumed, err := consumeSnapshot(ctx, migrated, alias, 60, 3, at)
	if err != nil || !consumed {
		t.Fatalf("consume retained v4 snapshot after migration = (%v, %v)", consumed, err)
	}
}

func TestConsumeLatestMessageRejectsInvalidIdentity(t *testing.T) {
	t.Parallel()
	db := openTestStore(t)
	ctx := context.Background()
	for name, position := range map[string]struct {
		aliasID     int64
		uidValidity uint32
		uid         uint32
	}{
		"alias":        {0, 1, 1},
		"uid validity": {1, 0, 1},
		"uid":          {1, 1, 0},
	} {
		t.Run(name, func(t *testing.T) {
			consumed, err := db.ConsumeLatestMessage(
				ctx, position.aliasID, []byte("expected-hash"), time.Now(),
				time.Now(), position.uidValidity, position.uid, time.Time{},
			)
			if err == nil || consumed {
				t.Fatalf("invalid consumption = (%v, %v)", consumed, err)
			}
		})
	}
	for name, consume := range map[string]func() (bool, error){
		"api key hash": func() (bool, error) {
			return db.ConsumeLatestMessage(ctx, 1, nil, time.Now(), time.Now(), 1, 1, time.Now())
		},
		"last synced time": func() (bool, error) {
			return db.ConsumeLatestMessage(
				ctx, 1, []byte("expected-hash"), time.Time{}, time.Now(), 1, 1, time.Now(),
			)
		},
		"message synced time": func() (bool, error) {
			return db.ConsumeLatestMessage(
				ctx, 1, []byte("expected-hash"), time.Now(), time.Time{}, 1, 1, time.Now(),
			)
		},
	} {
		t.Run(name, func(t *testing.T) {
			consumed, err := consume()
			if err == nil || consumed {
				t.Fatalf("invalid consumption CAS = (%v, %v)", consumed, err)
			}
		})
	}
	if consumed, err := db.ConsumeLatestMessage(
		ctx, 999, []byte("expected-hash"), time.Now(), time.Now(), 1, 1, time.Time{},
	); !errors.Is(err, store.ErrNotFound) || consumed {
		t.Fatalf("missing alias consumption = (%v, %v), want false, ErrNotFound", consumed, err)
	}
}

func markAliasSyncedForConsumption(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	alias domain.Alias,
	syncedAt time.Time,
) domain.Alias {
	t.Helper()
	syncedAt = syncedAt.UTC()
	if err := db.UpdateAliasSyncStatus(ctx, alias.ID, domain.SyncStatusOK, "", &syncedAt); err != nil {
		t.Fatalf("mark alias %d synced for consumption: %v", alias.ID, err)
	}
	alias.LastSyncStatus = domain.SyncStatusOK
	alias.LastSyncedAt = &syncedAt
	return alias
}

func consumeSnapshot(
	ctx context.Context,
	db *store.Store,
	alias domain.Alias,
	uidValidity, uid uint32,
	consumedAt time.Time,
) (bool, error) {
	return db.ConsumeLatestMessage(
		ctx, alias.ID, alias.APIKeyHash, *alias.LastSyncedAt, *alias.LastSyncedAt,
		uidValidity, uid, consumedAt,
	)
}

func assertSeenTaskCount(t *testing.T, ctx context.Context, db *store.Store, want int) {
	t.Helper()
	var got int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM imap_seen_tasks`).Scan(&got); err != nil {
		t.Fatalf("count seen tasks: %v", err)
	}
	if got != want {
		t.Fatalf("seen task count = %d, want %d", got, want)
	}
}
