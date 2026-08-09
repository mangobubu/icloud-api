package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestApplyMailboxSyncCommitsIncrementalSnapshotsCursorAndFreshness(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Primary", "primary-sync@icloud.com")
	foundAlias := createAlias(t, ctx, db, account.ID, "found-sync@icloud.com", []byte("found-sync-hash"))
	emptyAlias := createAlias(t, ctx, db, account.ID, "empty-sync@icloud.com", []byte("empty-sync-hash"))
	unchangedAlias := createAlias(t, ctx, db, account.ID, "unchanged-sync@icloud.com", []byte("unchanged-sync-hash"))
	disabledAlias := createAlias(t, ctx, db, account.ID, "disabled-sync@icloud.com", []byte("disabled-sync-hash"))

	oldAt := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	baselineMessages := make(map[int64]domain.LatestMessage)
	for _, fixture := range []struct {
		aliasID int64
		uid     uint32
		subject string
	}{
		{foundAlias.ID, 5, "old found"},
		{emptyAlias.ID, 6, "old empty"},
		{unchangedAlias.ID, 7, "keep incremental"},
		{disabledAlias.ID, 8, "keep disabled"},
	} {
		message := mailboxSyncTestMessage(fixture.aliasID, 40, fixture.uid, fixture.subject, oldAt)
		message.SnapshotState = domain.SnapshotFound
		baselineMessages[fixture.aliasID] = message
	}
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID,
		[]domain.Alias{foundAlias, emptyAlias, unchangedAlias, disabledAlias},
		domain.MailboxSyncResult{
			Messages: baselineMessages,
			State: domain.IMAPSyncState{
				AccountID: account.ID, UIDValidity: 40, LastUID: 8, UpdatedAt: oldAt,
			},
			Reset: true,
		}, oldAt,
	); err != nil {
		t.Fatalf("apply baseline mailbox sync: %v", err)
	}
	if err := db.SetAliasEnabled(ctx, disabledAlias.ID, false); err != nil {
		t.Fatalf("disable alias: %v", err)
	}
	disabledBefore, err := db.GetAlias(ctx, disabledAlias.ID)
	if err != nil {
		t.Fatalf("get disabled alias baseline: %v", err)
	}

	syncedAt := oldAt.Add(2 * time.Hour)
	headerDate := syncedAt.Add(-20 * time.Minute)
	found := domain.LatestMessage{
		AliasID: foundAlias.ID, UIDValidity: 40, UID: 12,
		MessageID: "<winner@example.com>", InternalDate: syncedAt.Add(-time.Hour), HeaderDate: &headerDate,
		From:    []domain.MailAddress{{Name: "Sender", Email: "sender@example.com"}},
		To:      []domain.MailAddress{{Email: foundAlias.Address}},
		CC:      []domain.MailAddress{{Email: "copy@example.com"}},
		Subject: "new winner", TextBody: "plain", HTMLBody: "<p>html</p>",
		Attachments:   []domain.Attachment{{Filename: "report.txt", ContentType: "text/plain", Size: 12}},
		BodyTruncated: true, SnapshotState: domain.SnapshotFound,
	}
	result := domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{
			foundAlias.ID: found,
			emptyAlias.ID: {AliasID: emptyAlias.ID, SnapshotState: domain.SnapshotEmpty},
		},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 40, LastUID: 15,
			UpdatedAt: syncedAt.Add(-time.Minute),
		},
	}
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID,
		[]domain.Alias{foundAlias, emptyAlias, unchangedAlias}, result, syncedAt,
	); err != nil {
		t.Fatalf("apply incremental mailbox sync: %v", err)
	}

	got, err := db.GetLatestMessage(ctx, foundAlias.ID)
	if err != nil {
		t.Fatalf("get found snapshot: %v", err)
	}
	if got.Subject != found.Subject || got.MessageID != found.MessageID || got.TextBody != found.TextBody ||
		got.HTMLBody != found.HTMLBody || !got.BodyTruncated || len(got.From) != 1 ||
		len(got.To) != 1 || len(got.CC) != 1 || len(got.Attachments) != 1 {
		t.Fatalf("found snapshot was not fully persisted: %#v", got)
	}
	if !got.SyncedAt.Equal(syncedAt) {
		t.Fatalf("found snapshot synced at = %v, want %v", got.SyncedAt, syncedAt)
	}
	if _, err := db.GetLatestMessage(ctx, emptyAlias.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty snapshot lookup error = %v, want ErrNotFound", err)
	}
	assertLatestSubject(t, ctx, db, unchangedAlias.ID, "keep incremental", 40, 7)
	assertLatestSubject(t, ctx, db, disabledAlias.ID, "keep disabled", 40, 8)

	state, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatalf("get committed IMAP state: %v", err)
	}
	if state.AccountID != account.ID || state.UIDValidity != 40 || state.LastUID != 15 ||
		!state.UpdatedAt.Equal(syncedAt.Add(-time.Minute)) {
		t.Fatalf("committed IMAP state = %#v", state)
	}
	for _, aliasID := range []int64{foundAlias.ID, emptyAlias.ID, unchangedAlias.ID} {
		assertAliasSyncState(t, ctx, db, aliasID, domain.SyncStatusOK, "", syncedAt)
	}
	disabled, err := db.GetAlias(ctx, disabledAlias.ID)
	if err != nil {
		t.Fatalf("get disabled alias: %v", err)
	}
	sameLastSyncedAt := disabled.LastSyncedAt == nil && disabledBefore.LastSyncedAt == nil ||
		disabled.LastSyncedAt != nil && disabledBefore.LastSyncedAt != nil &&
			disabled.LastSyncedAt.Equal(*disabledBefore.LastSyncedAt)
	if disabled.LastSyncStatus != disabledBefore.LastSyncStatus || !sameLastSyncedAt {
		t.Fatalf("disabled alias freshness changed: %#v", disabled)
	}
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", syncedAt)
}

func TestApplyMailboxSyncKeepsPendingWhileIncrementalBatchesRemain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Batched", "batched-sync@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "batched-alias@icloud.com", []byte("batched-sync-hash"))
	base := time.Date(2026, 8, 7, 10, 30, 0, 0, time.UTC)
	baseline := mailboxSyncTestMessage(alias.ID, 63, 5, "baseline", base)
	baseline.SnapshotState = domain.SnapshotFound
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{alias.ID: baseline},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 63, LastUID: 5, UpdatedAt: base,
		},
		Reset: true,
	}, base); err != nil {
		t.Fatalf("apply baseline mailbox sync: %v", err)
	}
	if err := recordMailboxSyncFailureForCurrentAccount(t, ctx, db, account.ID, "old batch error", base.Add(time.Minute)); err != nil {
		t.Fatalf("seed mailbox failure: %v", err)
	}

	partialAt := base.Add(2 * time.Minute)
	partial := mailboxSyncTestMessage(alias.ID, 63, 10, "partial batch", partialAt)
	partial.SnapshotState = domain.SnapshotFound
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{alias.ID: partial},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 63, LastUID: 10, UpdatedAt: partialAt,
		},
		HasMore: true,
	}, partialAt); err != nil {
		t.Fatalf("apply partial mailbox batch: %v", err)
	}
	assertLatestSubject(t, ctx, db, alias.ID, "partial batch", 63, 10)
	state, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatalf("get partial mailbox cursor: %v", err)
	}
	if state.LastUID != 10 || !state.UpdatedAt.Equal(partialAt) {
		t.Fatalf("partial mailbox cursor = %#v", state)
	}
	assertAliasSyncState(t, ctx, db, alias.ID, domain.SyncStatusPending, "", partialAt)
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusPending, "", partialAt)

	finalAt := base.Add(3 * time.Minute)
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 63, LastUID: 15, UpdatedAt: finalAt,
		},
	}, finalAt); err != nil {
		t.Fatalf("apply final mailbox batch: %v", err)
	}
	assertLatestSubject(t, ctx, db, alias.ID, "partial batch", 63, 10)
	state, err = db.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatalf("get final mailbox cursor: %v", err)
	}
	if state.LastUID != 15 || !state.UpdatedAt.Equal(finalAt) {
		t.Fatalf("final mailbox cursor = %#v", state)
	}
	assertAliasSyncState(t, ctx, db, alias.ID, domain.SyncStatusOK, "", finalAt)
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", finalAt)
}

func TestApplyMailboxSyncCommitsResetBatchThenContinuesIncrementally(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Reset batches", "reset-batches@icloud.com")
	firstAlias := createAlias(t, ctx, db, account.ID, "reset-batches-first@icloud.com", []byte("reset-batches-first-hash"))
	secondAlias := createAlias(t, ctx, db, account.ID, "reset-batches-second@icloud.com", []byte("reset-batches-second-hash"))
	base := time.Date(2026, 8, 7, 10, 45, 0, 0, time.UTC)
	first := mailboxSyncTestMessage(firstAlias.ID, 77, 4, "reset first batch", base)
	first.SnapshotState = domain.SnapshotFound
	aliases := []domain.Alias{firstAlias, secondAlias}
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, aliases, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{firstAlias.ID: first},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 77, LastUID: 5, UpdatedAt: base,
		},
		Reset: true, HasMore: true,
	}, base); err != nil {
		t.Fatalf("apply first reset batch: %v", err)
	}
	assertLatestSubject(t, ctx, db, firstAlias.ID, "reset first batch", 77, 4)
	if _, err := db.GetLatestMessage(ctx, secondAlias.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second alias snapshot after first reset batch error = %v, want ErrNotFound", err)
	}
	state, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil || state.UIDValidity != 77 || state.LastUID != 5 || !state.UpdatedAt.Equal(base) {
		t.Fatalf("first reset batch cursor = %#v, error = %v", state, err)
	}
	for _, aliasID := range []int64{firstAlias.ID, secondAlias.ID} {
		assertAliasSyncState(t, ctx, db, aliasID, domain.SyncStatusPending, "", base)
	}
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusPending, "", base)

	finalAt := base.Add(time.Minute)
	second := mailboxSyncTestMessage(secondAlias.ID, 77, 9, "incremental final batch", finalAt)
	second.SnapshotState = domain.SnapshotFound
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, aliases, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{secondAlias.ID: second},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 77, LastUID: 10, UpdatedAt: finalAt,
		},
	}, finalAt); err != nil {
		t.Fatalf("apply final incremental batch: %v", err)
	}
	assertLatestSubject(t, ctx, db, firstAlias.ID, "reset first batch", 77, 4)
	assertLatestSubject(t, ctx, db, secondAlias.ID, "incremental final batch", 77, 9)
	state, err = db.GetIMAPSyncState(ctx, account.ID)
	if err != nil || state.UIDValidity != 77 || state.LastUID != 10 || !state.UpdatedAt.Equal(finalAt) {
		t.Fatalf("final incremental batch cursor = %#v, error = %v", state, err)
	}
	for _, aliasID := range []int64{firstAlias.ID, secondAlias.ID} {
		assertAliasSyncState(t, ctx, db, aliasID, domain.SyncStatusOK, "", finalAt)
	}
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", finalAt)
}

func TestApplyMailboxSyncKeepsSnapshotUIDMonotonicWithinGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Monotonic", "monotonic-sync@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "monotonic-alias@icloud.com", []byte("monotonic-hash"))
	base := time.Date(2026, 8, 7, 10, 50, 0, 0, time.UTC)
	mustUpsert(t, ctx, db, mailboxSyncTestMessage(alias.ID, 77, 100, "existing UID 100", base))

	apply := func(uid uint32, subject string, reset, hasMore bool, at time.Time) {
		t.Helper()
		message := mailboxSyncTestMessage(alias.ID, 77, uid, subject, at)
		message.SnapshotState = domain.SnapshotFound
		if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{alias}, domain.MailboxSyncResult{
			Messages: map[int64]domain.LatestMessage{alias.ID: message},
			State: domain.IMAPSyncState{
				AccountID: account.ID, UIDValidity: 77, LastUID: uid, UpdatedAt: at,
			},
			Reset: reset, HasMore: hasMore,
		}, at); err != nil {
			t.Fatalf("apply UID %d mailbox batch: %v", uid, err)
		}
	}

	apply(80, "older reset winner", true, true, base.Add(time.Minute))
	assertLatestSubject(t, ctx, db, alias.ID, "existing UID 100", 77, 100)
	assertAliasSyncState(t, ctx, db, alias.ID, domain.SyncStatusPending, "", base.Add(time.Minute))
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusPending, "", base.Add(time.Minute))

	apply(90, "older incremental winner", false, true, base.Add(2*time.Minute))
	assertLatestSubject(t, ctx, db, alias.ID, "existing UID 100", 77, 100)

	apply(110, "newer incremental winner", false, false, base.Add(3*time.Minute))
	assertLatestSubject(t, ctx, db, alias.ID, "newer incremental winner", 77, 110)
	assertAliasSyncState(t, ctx, db, alias.ID, domain.SyncStatusOK, "", base.Add(3*time.Minute))
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", base.Add(3*time.Minute))
}

func TestApplyMailboxSyncResetReplacesWholeAccountGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Reset", "reset-sync@icloud.com")
	winner := createAlias(t, ctx, db, account.ID, "reset-winner@icloud.com", []byte("reset-winner-hash"))
	replacedLater := createAlias(t, ctx, db, account.ID, "reset-later@icloud.com", []byte("reset-later-hash"))
	disabled := createAlias(t, ctx, db, account.ID, "reset-disabled@icloud.com", []byte("reset-disabled-hash"))
	if err := db.SetAliasEnabled(ctx, disabled.ID, false); err != nil {
		t.Fatalf("disable reset fixture: %v", err)
	}
	otherAccount := createAccount(t, ctx, db, "Other", "other-reset-sync@icloud.com")
	otherAlias := createAlias(t, ctx, db, otherAccount.ID, "other-reset-alias@icloud.com", []byte("other-reset-hash"))

	base := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	oldWinner := mailboxSyncTestMessage(winner.ID, 91, 10, "old winner", base)
	oldWinner.SnapshotState = domain.SnapshotFound
	oldLater := mailboxSyncTestMessage(replacedLater.ID, 91, 11, "old later", base)
	oldLater.SnapshotState = domain.SnapshotFound
	aliases := []domain.Alias{winner, replacedLater}
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, aliases, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{
			winner.ID:        oldWinner,
			replacedLater.ID: oldLater,
		},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 91, LastUID: 11, UpdatedAt: base,
		},
		Reset: true,
	}, base); err != nil {
		t.Fatalf("apply old generation baseline: %v", err)
	}
	mustUpsert(t, ctx, db, mailboxSyncTestMessage(disabled.ID, 91, 12, "old disabled", base))
	mustUpsert(t, ctx, db, mailboxSyncTestMessage(otherAlias.ID, 91, 13, "other account", base))

	partialAt := base.Add(time.Hour)
	newWinner := mailboxSyncTestMessage(winner.ID, 7, 2, "new generation first batch", partialAt)
	newWinner.SnapshotState = domain.SnapshotFound
	result := domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{winner.ID: newWinner},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 7, LastUID: 3, UpdatedAt: partialAt,
		},
		Reset: true, HasMore: true,
	}
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, aliases, result, partialAt); err != nil {
		t.Fatalf("apply first new-generation batch: %v", err)
	}

	assertLatestSubject(t, ctx, db, winner.ID, "new generation first batch", 7, 2)
	for _, aliasID := range []int64{replacedLater.ID, disabled.ID} {
		if _, err := db.GetLatestMessage(ctx, aliasID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("old-generation alias %d lookup error = %v, want ErrNotFound", aliasID, err)
		}
	}
	assertLatestSubject(t, ctx, db, otherAlias.ID, "other account", 91, 13)
	state, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil || state.UIDValidity != 7 || state.LastUID != 3 || !state.UpdatedAt.Equal(partialAt) {
		t.Fatalf("first new-generation cursor = %#v, error = %v", state, err)
	}
	for _, aliasID := range []int64{winner.ID, replacedLater.ID} {
		assertAliasSyncState(t, ctx, db, aliasID, domain.SyncStatusPending, "", partialAt)
	}
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusPending, "", partialAt)

	finalAt := partialAt.Add(time.Minute)
	newLater := mailboxSyncTestMessage(replacedLater.ID, 7, 5, "new generation final batch", finalAt)
	newLater.SnapshotState = domain.SnapshotFound
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, aliases, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{replacedLater.ID: newLater},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 7, LastUID: 5, UpdatedAt: finalAt,
		},
	}, finalAt); err != nil {
		t.Fatalf("apply final new-generation batch: %v", err)
	}
	assertLatestSubject(t, ctx, db, winner.ID, "new generation first batch", 7, 2)
	assertLatestSubject(t, ctx, db, replacedLater.ID, "new generation final batch", 7, 5)
	for _, aliasID := range []int64{winner.ID, replacedLater.ID} {
		assertAliasSyncState(t, ctx, db, aliasID, domain.SyncStatusOK, "", finalAt)
	}
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", finalAt)
}

func TestApplyMailboxSyncSameGenerationResetPreservesOmittedSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Same generation", "same-generation@icloud.com")
	preserved := createAlias(t, ctx, db, account.ID, "preserved@icloud.com", []byte("preserved-hash"))
	confirmedEmpty := createAlias(t, ctx, db, account.ID, "confirmed-empty@icloud.com", []byte("empty-hash"))
	oldGeneration := createAlias(t, ctx, db, account.ID, "old-generation@icloud.com", []byte("old-generation-hash"))
	at := time.Date(2026, 8, 7, 11, 15, 0, 0, time.UTC)
	mustUpsert(t, ctx, db, mailboxSyncTestMessage(preserved.ID, 50, 5, "outside reset window", at))
	mustUpsert(t, ctx, db, mailboxSyncTestMessage(confirmedEmpty.ID, 50, 6, "expunged in window", at))
	mustUpsert(t, ctx, db, mailboxSyncTestMessage(oldGeneration.ID, 49, 99, "old generation", at))

	result := domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{
			confirmedEmpty.ID: {
				AliasID: confirmedEmpty.ID, UIDValidity: 50, SnapshotState: domain.SnapshotEmpty,
			},
		},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 50, LastUID: 100, UpdatedAt: at.Add(time.Minute),
		},
		Reset: true, HasMore: true,
	}
	aliases := []domain.Alias{preserved, confirmedEmpty, oldGeneration}
	partialAt := at.Add(time.Minute)
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, aliases, result, partialAt); err != nil {
		t.Fatalf("apply same-generation bounded reset: %v", err)
	}

	assertLatestSubject(t, ctx, db, preserved.ID, "outside reset window", 50, 5)
	for _, aliasID := range []int64{confirmedEmpty.ID, oldGeneration.ID} {
		if _, err := db.GetLatestMessage(ctx, aliasID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("alias %d after bounded reset error = %v, want ErrNotFound", aliasID, err)
		}
	}
	state, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil || state.UIDValidity != 50 || state.LastUID != 100 {
		t.Fatalf("same-generation reset cursor = %#v, error = %v", state, err)
	}
	for _, aliasID := range []int64{preserved.ID, confirmedEmpty.ID, oldGeneration.ID} {
		assertAliasSyncState(t, ctx, db, aliasID, domain.SyncStatusPending, "", partialAt)
	}
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusPending, "", partialAt)

	finalAt := at.Add(2 * time.Minute)
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, aliases, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 50, LastUID: 110, UpdatedAt: finalAt,
		},
	}, finalAt); err != nil {
		t.Fatalf("apply same-generation final batch: %v", err)
	}
	assertLatestSubject(t, ctx, db, preserved.ID, "outside reset window", 50, 5)
	for _, aliasID := range []int64{confirmedEmpty.ID, oldGeneration.ID} {
		if _, err := db.GetLatestMessage(ctx, aliasID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("alias %d after final batch error = %v, want ErrNotFound", aliasID, err)
		}
	}
	for _, aliasID := range []int64{preserved.ID, confirmedEmpty.ID, oldGeneration.ID} {
		assertAliasSyncState(t, ctx, db, aliasID, domain.SyncStatusOK, "", finalAt)
	}
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", finalAt)
}

func TestListMailboxSnapshotPositionsReturnsEnabledAliasesOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Positions", "positions@icloud.com")
	enabled := createAlias(t, ctx, db, account.ID, "position-enabled@icloud.com", []byte("position-enabled-hash"))
	disabled := createAlias(t, ctx, db, account.ID, "position-disabled@icloud.com", []byte("position-disabled-hash"))
	withoutSnapshot := createAlias(t, ctx, db, account.ID, "position-empty@icloud.com", []byte("position-empty-hash"))
	if err := db.SetAliasEnabled(ctx, disabled.ID, false); err != nil {
		t.Fatalf("disable position fixture: %v", err)
	}
	at := time.Date(2026, 8, 7, 11, 20, 0, 0, time.UTC)
	mustUpsert(t, ctx, db, mailboxSyncTestMessage(enabled.ID, 81, 123, "enabled", at))
	mustUpsert(t, ctx, db, mailboxSyncTestMessage(disabled.ID, 81, 124, "disabled", at))

	positions, err := db.ListMailboxSnapshotPositions(ctx, account.ID)
	if err != nil {
		t.Fatalf("list mailbox snapshot positions: %v", err)
	}
	want := domain.MailboxSnapshotPosition{AliasID: enabled.ID, UIDValidity: 81, UID: 123}
	if len(positions) != 1 || positions[enabled.ID] != want {
		t.Fatalf("snapshot positions = %#v, want %#v", positions, map[int64]domain.MailboxSnapshotPosition{enabled.ID: want})
	}
	if _, exists := positions[withoutSnapshot.ID]; exists {
		t.Fatalf("alias without snapshot unexpectedly returned: %#v", positions)
	}
}

func TestApplyMailboxSyncRejectsUIDRegressionAndIncrementalGenerationChange(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Ordered", "ordered-sync@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "ordered-alias@icloud.com", []byte("ordered-sync-hash"))
	base := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)

	apply := func(uidValidity, uid uint32, subject string, observedAt, committedAt time.Time, reset bool) {
		t.Helper()
		message := mailboxSyncTestMessage(alias.ID, uidValidity, uid, subject, committedAt)
		message.SnapshotState = domain.SnapshotFound
		err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{alias}, domain.MailboxSyncResult{
			Messages: map[int64]domain.LatestMessage{alias.ID: message},
			State: domain.IMAPSyncState{
				AccountID: account.ID, UIDValidity: uidValidity, LastUID: uid, UpdatedAt: observedAt,
			},
			Reset: reset,
		}, committedAt)
		if err != nil {
			t.Fatalf("apply mailbox publication %q: %v", subject, err)
		}
	}

	apply(100, 20, "current", base, base.Add(time.Minute), true)
	apply(100, 10, "stale uid", base.Add(-time.Minute), base.Add(2*time.Minute), false)
	assertLatestSubject(t, ctx, db, alias.ID, "current", 100, 20)
	state, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatalf("get cursor after stale UID: %v", err)
	}
	if state.UIDValidity != 100 || state.LastUID != 20 || !state.UpdatedAt.Equal(base) {
		t.Fatalf("cursor after stale UID = %#v", state)
	}

	apply(7, 3, "new generation", base.Add(3*time.Minute), base.Add(4*time.Minute), true)
	apply(100, 30, "old incremental generation", base.Add(5*time.Minute), base.Add(6*time.Minute), false)
	assertLatestSubject(t, ctx, db, alias.ID, "new generation", 7, 3)
	state, err = db.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatalf("get cursor after incremental generation change: %v", err)
	}
	if state.UIDValidity != 7 || state.LastUID != 3 || !state.UpdatedAt.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("cursor after incremental generation change = %#v", state)
	}
}

func TestApplyMailboxSyncRecoversAfterClockRollback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Clock rollback", "clock-rollback@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "clock-rollback-alias@icloud.com", []byte("clock-rollback-hash"))
	base := time.Date(2026, 8, 7, 11, 30, 0, 0, time.UTC)
	futureObservation := base.Add(24 * time.Hour)

	applyReset := func(uidValidity, uid uint32, subject string, observedAt, committedAt time.Time) {
		t.Helper()
		message := mailboxSyncTestMessage(alias.ID, uidValidity, uid, subject, committedAt)
		message.SnapshotState = domain.SnapshotFound
		if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{alias}, domain.MailboxSyncResult{
			Messages: map[int64]domain.LatestMessage{alias.ID: message},
			State: domain.IMAPSyncState{
				AccountID: account.ID, UIDValidity: uidValidity, LastUID: uid, UpdatedAt: observedAt,
			},
			Reset: true,
		}, committedAt); err != nil {
			t.Fatalf("apply generation %d: %v", uidValidity, err)
		}
	}

	applyReset(101, 20, "generation one", futureObservation, base)

	healthAt := base.Add(time.Minute)
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 101, LastUID: 20, UpdatedAt: healthAt,
		},
	}, healthAt); err != nil {
		t.Fatalf("apply same-position health refresh after clock rollback: %v", err)
	}
	assertLatestSubject(t, ctx, db, alias.ID, "generation one", 101, 20)
	assertAliasSyncState(t, ctx, db, alias.ID, domain.SyncStatusOK, "", healthAt)
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", healthAt)
	state, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatalf("get cursor after same-position health refresh: %v", err)
	}
	if state.UIDValidity != 101 || state.LastUID != 20 || !state.UpdatedAt.Equal(futureObservation) {
		t.Fatalf("cursor after same-position health refresh = %#v", state)
	}

	forwardAt := base.Add(2 * time.Minute)
	forward := mailboxSyncTestMessage(alias.ID, 101, 21, "forward after rollback", forwardAt)
	forward.SnapshotState = domain.SnapshotFound
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{alias.ID: forward},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 101, LastUID: 21, UpdatedAt: forwardAt,
		},
	}, forwardAt); err != nil {
		t.Fatalf("advance same-generation cursor after clock rollback: %v", err)
	}
	assertLatestSubject(t, ctx, db, alias.ID, "forward after rollback", 101, 21)
	state, err = db.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatalf("get advanced cursor after clock rollback: %v", err)
	}
	if state.UIDValidity != 101 || state.LastUID != 21 || !state.UpdatedAt.Equal(futureObservation) {
		t.Fatalf("advanced cursor after clock rollback = %#v", state)
	}

	newGenerationObservedAt := base.Add(3 * time.Minute)
	newGenerationCommittedAt := base.Add(4 * time.Minute)
	applyReset(202, 4, "generation two", newGenerationObservedAt, newGenerationCommittedAt)

	assertLatestSubject(t, ctx, db, alias.ID, "generation two", 202, 4)
	state, err = db.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatalf("get new-generation cursor after clock rollback: %v", err)
	}
	if state.UIDValidity != 202 || state.LastUID != 4 || !state.UpdatedAt.Equal(newGenerationObservedAt) {
		t.Fatalf("new-generation cursor after clock rollback = %#v", state)
	}
	assertAliasSyncState(t, ctx, db, alias.ID, domain.SyncStatusOK, "", newGenerationCommittedAt)
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", newGenerationCommittedAt)
}

func TestApplyMailboxSyncDoesNotEstablishMissingCursorFromIncrementalResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Missing", "missing-cursor@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "missing-alias@icloud.com", []byte("missing-hash"))
	at := time.Date(2026, 8, 7, 11, 45, 0, 0, time.UTC)
	message := mailboxSyncTestMessage(alias.ID, 77, 12, "stale incremental", at)
	message.SnapshotState = domain.SnapshotFound

	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{alias.ID: message},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 77, LastUID: 12, UpdatedAt: at,
		},
	}, at); err != nil {
		t.Fatalf("apply incremental result without cursor: %v", err)
	}
	if _, err := db.GetIMAPSyncState(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing cursor after incremental result error = %v, want ErrNotFound", err)
	}
	if _, err := db.GetLatestMessage(ctx, alias.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("snapshot after ignored incremental result error = %v, want ErrNotFound", err)
	}
}

func TestApplyMailboxSyncDiscardsResetForStaleEnabledAliasSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Alias set", "alias-set-sync@icloud.com")
	original := createAlias(t, ctx, db, account.ID, "alias-set-original@icloud.com", []byte("alias-set-original-hash"))
	base := time.Date(2026, 8, 7, 11, 50, 0, 0, time.UTC)
	baseline := mailboxSyncTestMessage(original.ID, 71, 10, "baseline snapshot", base)
	baseline.SnapshotState = domain.SnapshotFound
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{original}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{original.ID: baseline},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 71, LastUID: 10, UpdatedAt: base,
		},
		Reset: true,
	}, base); err != nil {
		t.Fatalf("apply baseline mailbox sync: %v", err)
	}

	// This reset was collected before another process added an enabled alias.
	// Alias creation invalidates the account cursor while holding the same lock.
	staleAt := base.Add(time.Hour)
	stale := mailboxSyncTestMessage(original.ID, 72, 20, "stale reset", staleAt)
	stale.SnapshotState = domain.SnapshotFound
	newAlias := createAlias(t, ctx, db, account.ID, "alias-set-new@icloud.com", []byte("alias-set-new-hash"))
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{original}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{original.ID: stale},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 72, LastUID: 20, UpdatedAt: staleAt,
		},
		Reset: true,
	}, staleAt); err != nil {
		t.Fatalf("apply reset for stale alias set: %v", err)
	}

	if _, err := db.GetIMAPSyncState(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cursor after stale alias publication error = %v, want ErrNotFound", err)
	}
	assertLatestSubject(t, ctx, db, original.ID, "baseline snapshot", 71, 10)
	if _, err := db.GetLatestMessage(ctx, newAlias.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("new alias snapshot error = %v, want ErrNotFound", err)
	}
	storedNewAlias, err := db.GetAlias(ctx, newAlias.ID)
	if err != nil {
		t.Fatalf("get new alias after stale publication: %v", err)
	}
	if storedNewAlias.LastSyncStatus != domain.SyncStatusPending || storedNewAlias.LastSyncError != "" ||
		storedNewAlias.LastSyncedAt != nil {
		t.Fatalf("stale publication marked new alias healthy: %#v", storedNewAlias)
	}
	assertAliasSyncState(t, ctx, db, original.ID, domain.SyncStatusOK, "", base)
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", base)
}

func TestApplyMailboxSyncDropsOlderResetAfterNewGenerationCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Generation CAS", "generation-cas@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "generation-cas-alias@icloud.com", []byte("generation-cas-hash"))
	sharedVersion := currentAccountVersion(t, ctx, db, account.ID)
	base := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)

	newGeneration := mailboxSyncTestMessage(alias.ID, 202, 4, "new generation", base.Add(time.Minute))
	newGeneration.SnapshotState = domain.SnapshotFound
	if err := db.ApplyMailboxSync(ctx, account.ID, sharedVersion, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{alias.ID: newGeneration},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 202, LastUID: 4, UpdatedAt: base.Add(time.Minute),
		},
		Reset: true,
	}, base.Add(time.Minute)); err != nil {
		t.Fatalf("commit new mailbox generation: %v", err)
	}

	oldGeneration := mailboxSyncTestMessage(alias.ID, 101, 50, "late old reset", base)
	oldGeneration.SnapshotState = domain.SnapshotFound
	if err := db.ApplyMailboxSync(ctx, account.ID, sharedVersion, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{alias.ID: oldGeneration},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 101, LastUID: 50, UpdatedAt: base,
		},
		Reset: true,
	}, base.Add(time.Hour)); err != nil {
		t.Fatalf("drop late old-generation reset: %v", err)
	}

	assertLatestSubject(t, ctx, db, alias.ID, "new generation", 202, 4)
	state, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil || state.UIDValidity != 202 || state.LastUID != 4 {
		t.Fatalf("cursor after late old reset = %#v, error=%v", state, err)
	}
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", base.Add(time.Minute))
}

func TestMailboxSyncCASPreservesPendingAfterCredentialChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Credential CAS", "credential-cas@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "credential-cas-alias@icloud.com", []byte("credential-cas-hash"))
	base := time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC)
	baseline := mailboxSyncTestMessage(alias.ID, 90, 8, "credential baseline", base)
	baseline.SnapshotState = domain.SnapshotFound
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{alias.ID: baseline},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 90, LastUID: 8, UpdatedAt: base,
		},
		Reset: true,
	}, base); err != nil {
		t.Fatalf("seed credential baseline: %v", err)
	}

	staleVersion := currentAccountVersion(t, ctx, db, account.ID)
	current, err := db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("get credential fixture account: %v", err)
	}
	current.PasswordCiphertext = "rotated-encrypted-password"
	if _, err := db.UpdateAccount(ctx, current); err != nil {
		t.Fatalf("rotate account credentials: %v", err)
	}

	staleAt := base.Add(time.Hour)
	stale := mailboxSyncTestMessage(alias.ID, 91, 20, "stale credential result", staleAt)
	stale.SnapshotState = domain.SnapshotFound
	if err := db.ApplyMailboxSync(ctx, account.ID, staleVersion, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{alias.ID: stale},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 91, LastUID: 20, UpdatedAt: staleAt,
		},
		Reset: true,
	}, staleAt); err != nil {
		t.Fatalf("drop stale success after credential change: %v", err)
	}
	if err := db.RecordMailboxSyncFailure(ctx, account.ID, staleVersion, "stale credential failure", staleAt); err != nil {
		t.Fatalf("drop stale failure after credential change: %v", err)
	}

	updatedAccount, err := db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("get account after stale credential attempt: %v", err)
	}
	if updatedAccount.LastSyncStatus != domain.SyncStatusPending || updatedAccount.LastSyncError != "" || updatedAccount.LastSyncedAt != nil {
		t.Fatalf("stale credential attempt changed pending account: %#v", updatedAccount)
	}
	updatedAlias, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("get alias after stale credential attempt: %v", err)
	}
	if updatedAlias.LastSyncStatus != domain.SyncStatusPending || updatedAlias.LastSyncError != "" || updatedAlias.LastSyncedAt != nil {
		t.Fatalf("stale credential attempt changed pending alias: %#v", updatedAlias)
	}
	if _, err := db.GetIMAPSyncState(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("credential change cursor error = %v, want ErrNotFound", err)
	}
	assertLatestSubject(t, ctx, db, alias.ID, "credential baseline", 90, 8)
}

func TestMailboxSyncCASPreservesManualAliasReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Manual reset CAS", "manual-reset-cas@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "manual-reset-cas-alias@icloud.com", []byte("manual-reset-cas-hash"))
	base := time.Date(2026, 8, 8, 11, 30, 0, 0, time.UTC)
	baseline := mailboxSyncTestMessage(alias.ID, 60, 7, "manual reset baseline", base)
	baseline.SnapshotState = domain.SnapshotFound
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{alias.ID: baseline},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 60, LastUID: 7, UpdatedAt: base,
		},
		Reset: true,
	}, base); err != nil {
		t.Fatalf("seed manual reset baseline: %v", err)
	}

	staleVersion := currentAccountVersion(t, ctx, db, account.ID)
	if err := db.ResetAliasSnapshot(ctx, alias.ID); err != nil {
		t.Fatalf("reset alias snapshot: %v", err)
	}
	resetVersion := currentAccountVersion(t, ctx, db, account.ID)
	if !resetVersion.After(staleVersion) {
		t.Fatalf("account version after alias reset = %v, want after %v", resetVersion, staleVersion)
	}

	staleAt := base.Add(time.Hour)
	stale := mailboxSyncTestMessage(alias.ID, 61, 9, "stale reset result", staleAt)
	stale.SnapshotState = domain.SnapshotFound
	if err := db.ApplyMailboxSync(ctx, account.ID, staleVersion, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{alias.ID: stale},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 61, LastUID: 9, UpdatedAt: staleAt,
		},
		Reset: true,
	}, staleAt); err != nil {
		t.Fatalf("drop stale success after manual reset: %v", err)
	}
	if err := db.RecordMailboxSyncFailure(ctx, account.ID, staleVersion, "stale reset failure", staleAt); err != nil {
		t.Fatalf("drop stale failure after manual reset: %v", err)
	}

	resetAlias, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("get manually reset alias: %v", err)
	}
	if resetAlias.LastSyncStatus != domain.SyncStatusPending || resetAlias.LastSyncError != "" || resetAlias.LastSyncedAt != nil {
		t.Fatalf("stale attempt changed manually reset alias: %#v", resetAlias)
	}
	if _, err := db.GetLatestMessage(ctx, alias.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("manual reset snapshot error = %v, want ErrNotFound", err)
	}
	if _, err := db.GetIMAPSyncState(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("manual reset cursor error = %v, want ErrNotFound", err)
	}
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", base)
}

func TestApplyMailboxSyncRollsBackNewGenerationResetBatchWhenCursorWriteFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Atomic", "atomic-sync@icloud.com")
	firstAlias := createAlias(t, ctx, db, account.ID, "atomic-first@icloud.com", []byte("atomic-first-hash"))
	secondAlias := createAlias(t, ctx, db, account.ID, "atomic-second@icloud.com", []byte("atomic-second-hash"))
	firstAt := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	first := mailboxSyncTestMessage(firstAlias.ID, 55, 4, "committed first", firstAt)
	first.SnapshotState = domain.SnapshotFound
	second := mailboxSyncTestMessage(secondAlias.ID, 55, 5, "committed second", firstAt)
	second.SnapshotState = domain.SnapshotFound
	aliases := []domain.Alias{firstAlias, secondAlias}
	if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, aliases, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{
			firstAlias.ID:  first,
			secondAlias.ID: second,
		},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 55, LastUID: 5, UpdatedAt: firstAt,
		},
		Reset: true,
	}, firstAt); err != nil {
		t.Fatalf("apply baseline mailbox sync: %v", err)
	}
	versionBefore := currentAccountVersion(t, ctx, db, account.ID)

	if _, err := db.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_imap_cursor_update
		BEFORE UPDATE ON imap_sync_states
		BEGIN
			SELECT RAISE(ABORT, 'cursor update rejected');
		END`); err != nil {
		t.Fatalf("create cursor failure trigger: %v", err)
	}
	resetAt := firstAt.Add(time.Hour)
	newGeneration := mailboxSyncTestMessage(firstAlias.ID, 66, 2, "must roll back", resetAt)
	newGeneration.SnapshotState = domain.SnapshotFound
	err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, aliases, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{firstAlias.ID: newGeneration},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 66, LastUID: 3, UpdatedAt: resetAt,
		},
		Reset: true, HasMore: true,
	}, resetAt)
	if err == nil || !strings.Contains(err.Error(), "upsert IMAP sync state") {
		t.Fatalf("cursor failure error = %v", err)
	}

	assertLatestSubject(t, ctx, db, firstAlias.ID, "committed first", 55, 4)
	assertLatestSubject(t, ctx, db, secondAlias.ID, "committed second", 55, 5)
	state, stateErr := db.GetIMAPSyncState(ctx, account.ID)
	if stateErr != nil {
		t.Fatalf("get state after rollback: %v", stateErr)
	}
	if state.UIDValidity != 55 || state.LastUID != 5 || !state.UpdatedAt.Equal(firstAt) {
		t.Fatalf("cursor changed despite rollback: %#v", state)
	}
	versionAfter := currentAccountVersion(t, ctx, db, account.ID)
	if !versionAfter.Equal(versionBefore) {
		t.Fatalf("account version after rollback = %v, want %v", versionAfter, versionBefore)
	}
	for _, aliasID := range []int64{firstAlias.ID, secondAlias.ID} {
		assertAliasSyncState(t, ctx, db, aliasID, domain.SyncStatusOK, "", firstAt)
	}
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", firstAt)
}

func TestApplyMailboxSyncValidatesAccountAndAliasSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Validation", "validation-sync@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "validation-alias@icloud.com", []byte("validation-hash"))
	missingAlias := createAlias(t, ctx, db, account.ID, "validation-missing@icloud.com", []byte("validation-missing-hash"))
	otherAccount := createAccount(t, ctx, db, "Validation other", "validation-other@icloud.com")
	otherAlias := createAlias(t, ctx, db, otherAccount.ID, "validation-other-alias@icloud.com", []byte("validation-other-hash"))
	at := time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)
	mustUpsert(t, ctx, db, mailboxSyncTestMessage(missingAlias.ID, 1, 1, "must survive invalid reset", at))

	tests := []struct {
		name    string
		aliases []domain.Alias
		result  domain.MailboxSyncResult
	}{
		{
			name: "state account mismatch", aliases: []domain.Alias{alias},
			result: domain.MailboxSyncResult{State: domain.IMAPSyncState{AccountID: otherAccount.ID, UIDValidity: 1}},
		},
		{
			name: "message outside enabled set", aliases: []domain.Alias{alias},
			result: domain.MailboxSyncResult{
				Messages: map[int64]domain.LatestMessage{otherAlias.ID: {
					AliasID: otherAlias.ID, UIDValidity: 1, UID: 1, SnapshotState: domain.SnapshotFound,
				}},
				State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1, LastUID: 1},
			},
		},
		{
			name: "unsupported snapshot state", aliases: []domain.Alias{alias},
			result: domain.MailboxSyncResult{
				Messages: map[int64]domain.LatestMessage{alias.ID: {
					AliasID: alias.ID, SnapshotState: domain.SnapshotUnknown,
				}},
				State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := applyMailboxSyncForCurrentAccount(t, ctx, db, account.ID, test.aliases, test.result, at); err == nil {
				t.Fatal("invalid mailbox sync unexpectedly succeeded")
			}
			if _, err := db.GetIMAPSyncState(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("invalid sync persisted cursor: %v", err)
			}
		})
	}
	assertLatestSubject(t, ctx, db, missingAlias.ID, "must survive invalid reset", 1, 1)
}

func TestRecordMailboxSyncFailureUpdatesEnabledAliasesInBulk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Failure", "failure-sync@icloud.com")
	first := createAlias(t, ctx, db, account.ID, "failure-one@icloud.com", []byte("failure-one-hash"))
	second := createAlias(t, ctx, db, account.ID, "failure-two@icloud.com", []byte("failure-two-hash"))
	disabled := createAlias(t, ctx, db, account.ID, "failure-disabled@icloud.com", []byte("failure-disabled-hash"))
	if err := db.SetAliasEnabled(ctx, disabled.ID, false); err != nil {
		t.Fatalf("disable failure fixture: %v", err)
	}

	at := time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC)
	if err := recordMailboxSyncFailureForCurrentAccount(t, ctx, db, account.ID, "  context deadline exceeded  ", at); err != nil {
		t.Fatalf("record mailbox failure: %v", err)
	}
	for _, aliasID := range []int64{first.ID, second.ID} {
		assertAliasSyncState(t, ctx, db, aliasID, domain.SyncStatusError, "context deadline exceeded", at)
	}
	unchanged, err := db.GetAlias(ctx, disabled.ID)
	if err != nil {
		t.Fatalf("get disabled failure alias: %v", err)
	}
	if unchanged.LastSyncStatus != domain.SyncStatusPending || unchanged.LastSyncError != "" || unchanged.LastSyncedAt != nil {
		t.Fatalf("disabled alias failure state changed: %#v", unchanged)
	}
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusError, "context deadline exceeded", at)
}

func TestRecordMailboxSyncFailureDoesNotOverwriteNewerSuccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Failure ordering", "failure-ordering@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "failure-ordering-alias@icloud.com", []byte("failure-ordering-hash"))
	successAt := time.Date(2026, 8, 7, 14, 30, 0, 0, time.UTC)
	staleAttemptVersion := currentAccountVersion(t, ctx, db, account.ID)
	message := mailboxSyncTestMessage(alias.ID, 88, 9, "successful snapshot", successAt)
	message.SnapshotState = domain.SnapshotFound
	if err := db.ApplyMailboxSync(ctx, account.ID, staleAttemptVersion, []domain.Alias{alias}, domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{alias.ID: message},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 88, LastUID: 9, UpdatedAt: successAt,
		},
		Reset: true,
	}, successAt); err != nil {
		t.Fatalf("apply successful mailbox sync: %v", err)
	}

	for _, failureAt := range []time.Time{successAt.Add(-time.Minute), successAt} {
		if err := db.RecordMailboxSyncFailure(
			ctx, account.ID, staleAttemptVersion, "stale failure", failureAt,
		); err != nil {
			t.Fatalf("record stale mailbox failure at %v: %v", failureAt, err)
		}
		assertAliasSyncState(t, ctx, db, alias.ID, domain.SyncStatusOK, "", successAt)
		assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusOK, "", successAt)
	}

	newerFailureAt := successAt.Add(time.Minute)
	if err := recordMailboxSyncFailureForCurrentAccount(t, ctx, db, account.ID, "newer failure", newerFailureAt); err != nil {
		t.Fatalf("record newer mailbox failure: %v", err)
	}
	assertAliasSyncState(t, ctx, db, alias.ID, domain.SyncStatusError, "newer failure", newerFailureAt)
	assertAccountSyncState(t, ctx, db, account.ID, domain.SyncStatusError, "newer failure", newerFailureAt)
}

func TestRecordMailboxSyncFailureLocksAccountBeforeUpdatingAliases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Failure lock", "failure-lock@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "failure-lock-alias@icloud.com", []byte("failure-lock-hash"))
	if _, err := db.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_mailbox_failure_account_lock
		BEFORE UPDATE OF updated_at ON accounts
		WHEN NEW.updated_at = OLD.updated_at
		BEGIN
			SELECT RAISE(ABORT, 'account lock rejected');
		END`); err != nil {
		t.Fatalf("create account lock trigger: %v", err)
	}

	err := recordMailboxSyncFailureForCurrentAccount(t, ctx, db, account.ID, "must not reach aliases", time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "lock account for mailbox sync failure") {
		t.Fatalf("account lock failure error = %v", err)
	}
	storedAlias, getErr := db.GetAlias(ctx, alias.ID)
	if getErr != nil {
		t.Fatalf("get alias after rejected account lock: %v", getErr)
	}
	if storedAlias.LastSyncStatus != domain.SyncStatusPending || storedAlias.LastSyncError != "" ||
		storedAlias.LastSyncedAt != nil {
		t.Fatalf("alias changed before account lock: %#v", storedAlias)
	}
}

func currentAccountVersion(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	accountID int64,
) time.Time {
	t.Helper()
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatalf("get account %d version: %v", accountID, err)
	}
	if account.UpdatedAt.IsZero() {
		t.Fatalf("account %d has zero version", accountID)
	}
	return account.UpdatedAt
}

func applyMailboxSyncForCurrentAccount(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	accountID int64,
	aliases []domain.Alias,
	result domain.MailboxSyncResult,
	syncedAt time.Time,
) error {
	t.Helper()
	return db.ApplyMailboxSync(
		ctx, accountID, currentAccountVersion(t, ctx, db, accountID), aliases, result, syncedAt,
	)
}

func recordMailboxSyncFailureForCurrentAccount(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	accountID int64,
	message string,
	at time.Time,
) error {
	t.Helper()
	return db.RecordMailboxSyncFailure(
		ctx, accountID, currentAccountVersion(t, ctx, db, accountID), message, at,
	)
}

func mailboxSyncTestMessage(aliasID int64, uidValidity, uid uint32, subject string, at time.Time) domain.LatestMessage {
	return domain.LatestMessage{
		AliasID: aliasID, UIDValidity: uidValidity, UID: uid,
		InternalDate: at.Add(-time.Minute), Subject: subject, SyncedAt: at,
	}
}

func assertAliasSyncState(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	aliasID int64,
	status, syncError string,
	at time.Time,
) {
	t.Helper()
	alias, err := db.GetAlias(ctx, aliasID)
	if err != nil {
		t.Fatalf("get alias %d sync state: %v", aliasID, err)
	}
	if alias.LastSyncStatus != status || alias.LastSyncError != syncError || alias.LastSyncedAt == nil || !alias.LastSyncedAt.Equal(at) {
		t.Fatalf("alias %d sync state = %#v, want status=%q error=%q at=%v", aliasID, alias, status, syncError, at)
	}
}

func assertAccountSyncState(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	accountID int64,
	status, syncError string,
	at time.Time,
) {
	t.Helper()
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatalf("get account %d sync state: %v", accountID, err)
	}
	if account.LastSyncStatus != status || account.LastSyncError != syncError ||
		account.LastSyncedAt == nil || !account.LastSyncedAt.Equal(at) {
		t.Fatalf("account %d sync state = %#v, want status=%q error=%q at=%v", accountID, account, status, syncError, at)
	}
}
