package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestArchiveDeduplicatesMIMEAcrossAliasesAndKeepsStableLocalUIDs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, archiveRoot := openArchiveV2Store(t, 1<<20)
	account := createAccount(t, ctx, db, "Shared archive", "shared-archive@icloud.com")
	first := createAlias(t, ctx, db, account.ID, "first-shared@icloud.com", bytes.Repeat([]byte{0x11}, 32))
	second := createAlias(t, ctx, db, account.ID, "second-shared@icloud.com", bytes.Repeat([]byte{0x12}, 32))
	received := time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC)
	raw := []byte("From: sender@example.test\r\nTo: first-shared@icloud.com, second-shared@icloud.com\r\nSubject: shared\r\n\r\nbody")
	message := domain.ArchivedMessage{
		AccountID: account.ID, UIDValidity: 22, UID: 9,
		MessageID: "<shared@example.test>", InternalDate: received,
		Subject: "shared", RawMIME: raw, OTP: "443322", AliasIDs: []int64{first.ID, second.ID},
	}
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{first, second}, []domain.ArchivedMessage{message}, 22, 9, true)

	firstMessage := oneArchivedMailboxMessage(t, ctx, db, first.ID)
	secondMessage := oneArchivedMailboxMessage(t, ctx, db, second.ID)
	if firstMessage.ID != secondMessage.ID || firstMessage.MailboxUID != 1 || secondMessage.MailboxUID != 1 {
		t.Fatalf("shared archive identities = first:%#v second:%#v", firstMessage, secondMessage)
	}
	if firstMessage.ContentState != domain.ArchiveContentAvailable || firstMessage.ContentBytes != int64(len(raw)) {
		t.Fatalf("shared archive metadata = %#v", firstMessage)
	}
	if content, err := db.ReadArchivedContent(firstMessage); err != nil || !bytes.Equal(content, raw) {
		t.Fatalf("read shared MIME = %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, filepath.FromSlash(firstMessage.ContentPath))); err != nil {
		t.Fatalf("shared MIME file: %v", err)
	}

	// Replaying an already committed upstream identity updates metadata without
	// allocating another local UID or another MIME row.
	message.Subject = "shared metadata refreshed"
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{first, second}, []domain.ArchivedMessage{message}, 22, 9, false)
	firstMessages, err := db.ListArchivedMailboxMessages(ctx, first.ID)
	if err != nil || len(firstMessages) != 1 || firstMessages[0].MailboxUID != 1 || firstMessages[0].Subject != message.Subject {
		t.Fatalf("replayed shared message = %#v, %v", firstMessages, err)
	}
	stats, err := db.MailArchiveStats(ctx)
	if err != nil || stats.MessageCount != 1 || stats.ContentBytes != int64(len(raw)) {
		t.Fatalf("shared archive stats = %#v, %v", stats, err)
	}
}

func TestArchiveGlobalFIFORetainsMetadataAndReconcilesMissingFiles(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const limit = int64(80)
	db, archiveRoot := openArchiveV2Store(t, limit)
	account := createAccount(t, ctx, db, "FIFO archive", "fifo-archive@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "fifo-alias@icloud.com", bytes.Repeat([]byte{0x21}, 32))
	base := time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC)
	firstRaw := bytes.Repeat([]byte("a"), 60)
	secondRaw := bytes.Repeat([]byte("b"), 60)
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{alias}, []domain.ArchivedMessage{
		{AccountID: account.ID, UIDValidity: 31, UID: 1, InternalDate: base, Subject: "oldest title", RawMIME: firstRaw, AliasIDs: []int64{alias.ID}},
		{AccountID: account.ID, UIDValidity: 31, UID: 2, InternalDate: base.Add(time.Minute), Subject: "newest title", RawMIME: secondRaw, AliasIDs: []int64{alias.ID}},
	}, 31, 2, true)

	messages, err := db.ListArchivedMailboxMessages(ctx, alias.ID)
	if err != nil || len(messages) != 2 {
		t.Fatalf("list FIFO messages = %#v, %v", messages, err)
	}
	if messages[0].MailboxUID != 1 || messages[0].Subject != "oldest title" || messages[0].ContentState != domain.ArchiveContentEvicted {
		t.Fatalf("oldest FIFO metadata = %#v", messages[0])
	}
	if messages[1].MailboxUID != 2 || messages[1].Subject != "newest title" || messages[1].ContentState != domain.ArchiveContentAvailable {
		t.Fatalf("newest FIFO metadata = %#v", messages[1])
	}
	if _, err := db.ReadArchivedContent(messages[0]); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("evicted content read error = %v", err)
	}
	stats, err := db.MailArchiveStats(ctx)
	if err != nil || stats.ContentBytes != 60 || stats.EvictedCount != 1 || stats.MessageCount != 2 {
		t.Fatalf("FIFO archive stats = %#v, %v", stats, err)
	}

	availablePath := filepath.Join(archiveRoot, filepath.FromSlash(messages[1].ContentPath))
	if err := os.Remove(availablePath); err != nil {
		t.Fatalf("remove archived file fixture: %v", err)
	}
	if err := db.ReconcileMailArchive(ctx); err != nil {
		t.Fatalf("reconcile missing archive file: %v", err)
	}
	reconciled, err := db.GetArchivedMailboxMessage(ctx, alias.ID, 2)
	if err != nil || reconciled.ContentState != domain.ArchiveContentMissing || reconciled.Subject != "newest title" {
		t.Fatalf("reconciled missing message = %#v, %v", reconciled, err)
	}
}

func TestArchiveKeepsOneHundredNewestOTPsButEveryTitle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _ := openArchiveV2Store(t, 1<<20)
	account := createAccount(t, ctx, db, "OTP retention", "otp-retention@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "otp-retention-alias@icloud.com", bytes.Repeat([]byte{0x31}, 32))
	base := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	messages := make([]domain.ArchivedMessage, 0, 101)
	for index := 1; index <= 101; index++ {
		messages = append(messages, domain.ArchivedMessage{
			AccountID: account.ID, UIDValidity: 41, UID: uint32(index),
			InternalDate: base.Add(time.Duration(index) * time.Minute),
			Subject:      fmt.Sprintf("OTP title %03d", index),
			ContentState: domain.ArchiveContentOversized,
			RawSize:      100<<20 + 1,
			OTP:          fmt.Sprintf("%06d", index),
			AliasIDs:     []int64{alias.ID},
		})
	}
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{alias}, messages, 41, 101, true)

	otps, err := db.ListAliasOTPs(ctx, alias.ID, 100)
	if err != nil || len(otps) != 100 {
		t.Fatalf("OTP history count = %d, %v", len(otps), err)
	}
	if otps[0].OTP != "000101" || otps[len(otps)-1].OTP != "000002" {
		t.Fatalf("OTP history bounds = first:%#v last:%#v", otps[0], otps[len(otps)-1])
	}
	archived, err := db.ListArchivedMailboxMessages(ctx, alias.ID)
	if err != nil || len(archived) != 101 {
		t.Fatalf("archived title count = %d, %v", len(archived), err)
	}
	if archived[0].Subject != "OTP title 001" || archived[0].OTP != "" ||
		archived[0].ContentState != domain.ArchiveContentOversized || archived[100].MailboxUID != 101 {
		t.Fatalf("retained archive endpoints = oldest:%#v newest:%#v", archived[0], archived[100])
	}
}

func TestLegacySnapshotExpungeProjectionDoesNotDeleteV2Archive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _ := openArchiveV2Store(t, 1<<20)
	account := createAccount(t, ctx, db, "Legacy snapshot expunge", "legacy-expunge@icloud.com")
	legacy := createLegacyAlias(t, ctx, db, account.ID, "legacy-expunge-alias@icloud.com", bytes.Repeat([]byte{0x41}, 32))
	v2 := createAlias(t, ctx, db, account.ID, "v2-expunge-alias@icloud.com", bytes.Repeat([]byte{0x42}, 32))
	base := time.Date(2026, 8, 12, 6, 0, 0, 0, time.UTC)
	seed := []domain.ArchivedMessage{
		{AccountID: account.ID, UIDValidity: 71, UID: 10, InternalDate: base,
			Subject: "legacy latest", ContentState: domain.ArchiveContentMetadata, AliasIDs: []int64{legacy.ID}},
		{AccountID: account.ID, UIDValidity: 71, UID: 11, InternalDate: base.Add(time.Minute),
			Subject: "v2 archive", ContentState: domain.ArchiveContentMetadata, AliasIDs: []int64{v2.ID}},
	}
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{legacy, v2}, seed, 71, 11, true)

	positions, err := db.ListMailboxSnapshotPositions(ctx, account.ID)
	if err != nil {
		t.Fatalf("list compatibility snapshot positions: %v", err)
	}
	if len(positions) != 2 || positions[legacy.ID].UID != 10 || positions[v2.ID].UID != 11 {
		t.Fatalf("compatibility snapshot positions = %#v, want legacy UID 10 and v2 UID 11", positions)
	}
	account, err = db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("read account before expunge projection: %v", err)
	}
	projectedAt := base.Add(2 * time.Minute)
	if err := db.ApplyMailboxSync(ctx, account.ID, account.UpdatedAt, []domain.Alias{legacy, v2}, domain.MailboxSyncResult{
		LegacySnapshotUpdates: map[int64]domain.LatestMessage{
			legacy.ID: {
				AliasID: legacy.ID, UIDValidity: 71, SnapshotState: domain.SnapshotEmpty,
			},
		},
		State: domain.IMAPSyncState{
			AccountID: account.ID, UIDValidity: 71, LastUID: 11, UpdatedAt: projectedAt,
		},
	}, projectedAt); err != nil {
		t.Fatalf("apply legacy expunge projection: %v", err)
	}
	if _, err := db.GetLatestMessage(ctx, legacy.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy latest after expunge = %v, want ErrNotFound", err)
	}
	for _, aliasID := range []int64{legacy.ID, v2.ID} {
		archived, err := db.ListArchivedMailboxMessages(ctx, aliasID)
		if err != nil || len(archived) != 1 {
			t.Fatalf("alias %d archive after legacy expunge = %#v, %v", aliasID, archived, err)
		}
	}
	state, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil || state.UIDValidity != 71 || state.LastUID != 11 {
		t.Fatalf("archive cursor after legacy expunge = %#v, %v", state, err)
	}
}

func TestLegacySnapshotEmptyProjectionClearsV2CompatibilityRowOnly(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _ := openArchiveV2Store(t, 1<<20)
	account := createAccount(t, ctx, db, "V2 projection boundary", "v2-projection@icloud.com")
	v2 := createAlias(t, ctx, db, account.ID, "v2-projection-alias@icloud.com", bytes.Repeat([]byte{0x43}, 32))
	at := time.Date(2026, 8, 12, 6, 30, 0, 0, time.UTC)
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{v2}, []domain.ArchivedMessage{{
		AccountID: account.ID, UIDValidity: 72, UID: 5, InternalDate: at,
		Subject: "v2 only", ContentState: domain.ArchiveContentMetadata, AliasIDs: []int64{v2.ID},
	}}, 72, 5, true)

	account, err := db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = db.ApplyMailboxSync(ctx, account.ID, account.UpdatedAt, []domain.Alias{v2}, domain.MailboxSyncResult{
		LegacySnapshotUpdates: map[int64]domain.LatestMessage{
			v2.ID: {AliasID: v2.ID, UIDValidity: 72, SnapshotState: domain.SnapshotEmpty},
		},
		State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 72, LastUID: 5, UpdatedAt: at.Add(time.Minute)},
	}, at.Add(time.Minute))
	if err != nil {
		t.Fatalf("apply v2 compatibility projection: %v", err)
	}
	if _, latestErr := db.GetLatestMessage(ctx, v2.ID); !errors.Is(latestErr, store.ErrNotFound) {
		t.Fatalf("v2 compatibility latest after expunge = %v, want ErrNotFound", latestErr)
	}
	archived, listErr := db.ListArchivedMailboxMessages(ctx, v2.ID)
	if listErr != nil || len(archived) != 1 {
		t.Fatalf("v2 archive changed after compatibility projection = %#v, %v", archived, listErr)
	}
}

func openArchiveV2Store(t *testing.T, limit int64) (*store.Store, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "archive-v2.db"))
	if err != nil {
		t.Fatalf("open archive v2 store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := filepath.Join(t.TempDir(), "mail-archive")
	if err := db.ConfigureMailArchive(root, limit); err != nil {
		t.Fatalf("configure archive v2 store: %v", err)
	}
	return db, root
}

func createLegacyAlias(
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
		APIKeyHash: hash, CredentialMode: domain.AliasCredentialModeLegacy, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create legacy alias %q: %v", address, err)
	}
	return alias
}

func applyArchiveV2Batch(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	accountID int64,
	aliases []domain.Alias,
	messages []domain.ArchivedMessage,
	uidValidity, lastUID uint32,
	reset bool,
) {
	t.Helper()
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatalf("read account before archive batch: %v", err)
	}
	observed := time.Date(2026, 8, 11, 12, 0, int(lastUID%60), 0, time.UTC)
	if err := db.ApplyMailboxSync(ctx, accountID, account.UpdatedAt, aliases, domain.MailboxSyncResult{
		ArchivedMessages: messages,
		State: domain.IMAPSyncState{
			AccountID: accountID, UIDValidity: uidValidity, LastUID: lastUID, UpdatedAt: observed,
		},
		Reset: reset,
	}, observed); err != nil {
		t.Fatalf("apply archive v2 batch: %v", err)
	}
}
