package store_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestLegacyImportAliasesPreservesCallerCredentialsAndExistingState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Legacy import", "legacy-import@icloud.com")
	originalHash := bytes.Repeat([]byte{0x31}, 32)

	first, err := db.ImportAliases(ctx, account.ID, []domain.AliasImportCandidate{{
		Address:      "LEGACY-IMPORTED@ICLOUD.COM",
		Label:        "Original label",
		APIKeyHash:   originalHash,
		APIKeyPrefix: "legacy-key12",
		Active:       true,
	}})
	if err != nil {
		t.Fatalf("first legacy import: %v", err)
	}
	if len(first.Created) != 1 || len(first.Existing) != 0 || len(first.Conflicts) != 0 {
		t.Fatalf("first legacy import result = %#v", first)
	}
	created := first.Created[0]
	if created.Address != "legacy-imported@icloud.com" || created.Label != "Original label" ||
		created.CredentialMode != domain.AliasCredentialModeLegacy ||
		created.APIKeyPrefix != "legacy-key12" || !bytes.Equal(created.APIKeyHash, originalHash) {
		t.Fatalf("created legacy alias did not preserve caller credentials: %#v", created)
	}

	syncedAt := time.Date(2026, 8, 14, 8, 30, 0, 0, time.UTC)
	accessedAt := syncedAt.Add(time.Minute)
	if err := db.UpdateAliasSyncStatus(ctx, created.ID, domain.SyncStatusOK, "", &syncedAt); err != nil {
		t.Fatalf("mark imported alias synced: %v", err)
	}
	if err := db.TouchAliasAccess(ctx, created.ID, accessedAt); err != nil {
		t.Fatalf("touch imported alias: %v", err)
	}
	before, err := db.GetAlias(ctx, created.ID)
	if err != nil {
		t.Fatalf("read imported alias before repeat: %v", err)
	}

	repeated, err := db.ImportAliases(ctx, account.ID, []domain.AliasImportCandidate{{
		Address:      before.Address,
		Label:        "Replacement must not apply",
		APIKeyHash:   bytes.Repeat([]byte{0x32}, 32),
		APIKeyPrefix: "changed-key1",
		Active:       false,
	}})
	if err != nil {
		t.Fatalf("repeat legacy import: %v", err)
	}
	if len(repeated.Created) != 0 || len(repeated.Existing) != 1 || len(repeated.Conflicts) != 0 {
		t.Fatalf("repeat legacy import result = %#v", repeated)
	}
	if !reflect.DeepEqual(repeated.Existing[0], before) {
		t.Fatalf("existing result changed stored alias:\n before=%#v\n result=%#v", before, repeated.Existing[0])
	}
	after, err := db.GetAlias(ctx, created.ID)
	if err != nil {
		t.Fatalf("read imported alias after repeat: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("repeat import mutated existing alias:\n before=%#v\n after=%#v", before, after)
	}
}

func TestLegacyImportAliasesOwnershipConflictRollsBackWholeBatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	importingAccount := createAccount(t, ctx, db, "Importing account", "importing@icloud.com")
	ownerAccount := createAccount(t, ctx, db, "Existing owner", "owner@icloud.com")
	ownerAlias := createAlias(
		t, ctx, db, ownerAccount.ID, "z-owned@icloud.com", bytes.Repeat([]byte{0x41}, 32),
	)
	ownerBefore, err := db.GetAlias(ctx, ownerAlias.ID)
	if err != nil {
		t.Fatalf("read owner alias before conflict: %v", err)
	}
	importerBefore, err := db.GetAccount(ctx, importingAccount.ID)
	if err != nil {
		t.Fatalf("read importing account before conflict: %v", err)
	}

	result, err := db.ImportAliases(ctx, importingAccount.ID, []domain.AliasImportCandidate{
		{
			Address: "a-would-be-new@icloud.com", Label: "Must roll back",
			APIKeyHash: bytes.Repeat([]byte{0x42}, 32), APIKeyPrefix: "new-key-pref", Active: true,
		},
		{
			Address: ownerAlias.Address, Label: "Must not take ownership",
			APIKeyHash: bytes.Repeat([]byte{0x43}, 32), APIKeyPrefix: "takeover-key", Active: true,
		},
	})
	if !errors.Is(err, store.ErrAliasOwnershipConflict) {
		t.Fatalf("ownership conflict error = %v, want ErrAliasOwnershipConflict", err)
	}
	if len(result.Created) != 0 || len(result.Conflicts) != 1 {
		t.Fatalf("ownership conflict result = %#v", result)
	}
	conflict := result.Conflicts[0]
	if conflict.Address != ownerAlias.Address || conflict.ExistingAliasID != ownerAlias.ID ||
		conflict.ExistingAccountID != ownerAccount.ID || conflict.ExistingAccountEmail != ownerAccount.Email {
		t.Fatalf("ownership conflict metadata = %#v", conflict)
	}
	if _, err := db.GetAliasByAddress(ctx, "a-would-be-new@icloud.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-conflicting batch alias survived rollback: %v", err)
	}
	ownerAfter, err := db.GetAlias(ctx, ownerAlias.ID)
	if err != nil {
		t.Fatalf("read owner alias after conflict: %v", err)
	}
	if !reflect.DeepEqual(ownerAfter, ownerBefore) {
		t.Fatalf("ownership conflict mutated owner alias:\n before=%#v\n after=%#v", ownerBefore, ownerAfter)
	}
	importerAfter, err := db.GetAccount(ctx, importingAccount.ID)
	if err != nil {
		t.Fatalf("read importing account after conflict: %v", err)
	}
	if !reflect.DeepEqual(importerAfter, importerBefore) {
		t.Fatalf("failed import mutated importing account:\n before=%#v\n after=%#v", importerBefore, importerAfter)
	}
}

func TestConsumeLatestMessageCASAllowsOneConcurrentClaimAndQueuesSeen(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := store.Open(filepath.Join(t.TempDir(), "legacy-consume.db"))
	if err != nil {
		t.Fatalf("open concurrent store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close concurrent store: %v", err)
		}
	})

	account := createAccount(t, ctx, db, "Legacy consume", "legacy-consume@icloud.com")
	apiKeyHash := bytes.Repeat([]byte{0x51}, 32)
	lastSyncedAt := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	alias, err := db.CreateAlias(ctx, domain.Alias{
		AccountID: account.ID, Address: "legacy-consume-alias@icloud.com", Label: "Legacy consume",
		APIKeyHash: apiKeyHash, APIKeyPrefix: "consume-key1", CredentialMode: domain.AliasCredentialModeLegacy,
		Enabled: true, LastSyncStatus: domain.SyncStatusOK, LastSyncedAt: &lastSyncedAt,
	})
	if err != nil {
		t.Fatalf("create consumable alias: %v", err)
	}
	messageSyncedAt := lastSyncedAt.Add(time.Minute)
	const uidValidity uint32 = 701
	const uid uint32 = 33
	if _, err := db.UpsertLatestMessage(ctx, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: uidValidity, UID: uid,
		MessageID: "<legacy-consume@example.test>", InternalDate: lastSyncedAt.Add(-time.Minute),
		Subject: "single claim", SyncedAt: messageSyncedAt,
	}); err != nil {
		t.Fatalf("publish latest message: %v", err)
	}
	consumedAt := messageSyncedAt.Add(time.Minute)

	type consumeInput struct {
		hash            []byte
		aliasSyncedAt   time.Time
		messageSyncedAt time.Time
		uidValidity     uint32
		uid             uint32
	}
	valid := consumeInput{
		hash: apiKeyHash, aliasSyncedAt: lastSyncedAt, messageSyncedAt: messageSyncedAt,
		uidValidity: uidValidity, uid: uid,
	}
	staleCases := []struct {
		name   string
		mutate func(*consumeInput)
	}{
		{name: "api key", mutate: func(input *consumeInput) { input.hash = bytes.Repeat([]byte{0x52}, 32) }},
		{name: "alias sync time", mutate: func(input *consumeInput) { input.aliasSyncedAt = input.aliasSyncedAt.Add(-time.Nanosecond) }},
		{name: "message sync time", mutate: func(input *consumeInput) { input.messageSyncedAt = input.messageSyncedAt.Add(-time.Nanosecond) }},
		{name: "uid validity", mutate: func(input *consumeInput) { input.uidValidity++ }},
		{name: "uid", mutate: func(input *consumeInput) { input.uid++ }},
	}
	for _, test := range staleCases {
		t.Run("reject stale "+test.name, func(t *testing.T) {
			input := valid
			input.hash = append([]byte(nil), valid.hash...)
			test.mutate(&input)
			claimed, err := db.ConsumeLatestMessage(
				ctx, alias.ID, input.hash, input.aliasSyncedAt, input.messageSyncedAt,
				input.uidValidity, input.uid, consumedAt,
			)
			if err != nil {
				t.Fatalf("consume stale snapshot: %v", err)
			}
			if claimed {
				t.Fatal("stale snapshot was claimed")
			}
		})
	}
	assertLegacyConsumeCounts(t, ctx, db, alias.ID, account.ID, 0)

	const contenders = 12
	type claimResult struct {
		claimed bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan claimResult, contenders)
	var ready sync.WaitGroup
	ready.Add(contenders)
	for range contenders {
		go func() {
			ready.Done()
			<-start
			claimed, claimErr := db.ConsumeLatestMessage(
				ctx, alias.ID, apiKeyHash, lastSyncedAt, messageSyncedAt,
				uidValidity, uid, consumedAt,
			)
			results <- claimResult{claimed: claimed, err: claimErr}
		}()
	}
	ready.Wait()
	close(start)

	claimedCount := 0
	for range contenders {
		result := <-results
		if result.err != nil {
			t.Errorf("concurrent claim: %v", result.err)
			continue
		}
		if result.claimed {
			claimedCount++
		}
	}
	if claimedCount != 1 {
		t.Fatalf("successful concurrent claims = %d, want 1", claimedCount)
	}
	assertLegacyConsumeCounts(t, ctx, db, alias.ID, account.ID, 1)

	claimed, err := db.ConsumeLatestMessage(
		ctx, alias.ID, apiKeyHash, lastSyncedAt, messageSyncedAt,
		uidValidity, uid, consumedAt.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("repeat exact claim: %v", err)
	}
	if claimed {
		t.Fatal("already consumed snapshot was claimed again")
	}

	accountIDs, err := db.ListSeenTaskAccountIDs(ctx)
	if err != nil {
		t.Fatalf("list seen task accounts: %v", err)
	}
	if !reflect.DeepEqual(accountIDs, []int64{account.ID}) {
		t.Fatalf("seen task account IDs = %v, want [%d]", accountIDs, account.ID)
	}
	tasks, err := db.ListSeenTasks(ctx, account.ID, 10)
	if err != nil {
		t.Fatalf("list seen tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].AccountID != account.ID || tasks[0].UIDValidity != uidValidity ||
		tasks[0].UID != uid || !tasks[0].CreatedAt.Equal(consumedAt) {
		t.Fatalf("seen tasks = %#v, want one exact queued snapshot", tasks)
	}
}

func assertLegacyConsumeCounts(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	aliasID, accountID int64,
	want int,
) {
	t.Helper()
	for _, query := range []struct {
		name string
		sql  string
		arg  int64
	}{
		{name: "consumed messages", sql: `SELECT COUNT(*) FROM consumed_messages WHERE alias_id = ?`, arg: aliasID},
		{name: "seen tasks", sql: `SELECT COUNT(*) FROM imap_seen_tasks WHERE account_id = ?`, arg: accountID},
	} {
		var got int
		if err := db.DB().QueryRowContext(ctx, query.sql, query.arg).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", query.name, err)
		}
		if got != want {
			t.Fatalf("%s count = %d, want %d", query.name, got, want)
		}
	}
}
