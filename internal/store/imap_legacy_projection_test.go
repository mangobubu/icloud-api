package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestSeenArchivedMessageDoesNotPolluteLegacyLatestProjection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _ := openArchiveV2Store(t, 1<<20)
	account := createAccount(t, ctx, db, "Seen projection boundary", "seen-projection@icloud.com")
	alias := createLegacyAlias(t, ctx, db, account.ID, "seen-projection-alias@icloud.com", bytes.Repeat([]byte{0x71}, 32))
	base := time.Date(2026, 8, 12, 8, 0, 0, 0, time.UTC)

	first := domain.ArchivedMessage{
		AccountID: account.ID, UIDValidity: 91, UID: 1, InternalDate: base,
		MessageID: "<unread@example.test>", Subject: "unread latest",
		ContentState: domain.ArchiveContentMetadata, AliasIDs: []int64{alias.ID},
	}
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{alias}, []domain.ArchivedMessage{first}, 91, 1, true)

	account, err := db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("reload account before seen batch: %v", err)
	}
	seen := domain.ArchivedMessage{
		AccountID: account.ID, UIDValidity: 91, UID: 2, InternalDate: base.Add(time.Minute),
		MessageID: "<read@example.test>", Subject: "already read upstream",
		ContentState: domain.ArchiveContentMetadata, AliasIDs: []int64{alias.ID},
		UpstreamSeen: true,
	}
	if err := db.ApplyMailboxSync(ctx, account.ID, account.UpdatedAt, []domain.Alias{alias}, domain.MailboxSyncResult{
		ArchivedMessages: []domain.ArchivedMessage{seen},
		State:            domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 91, LastUID: 2, UpdatedAt: base.Add(2 * time.Minute)},
	}, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("apply seen archive batch: %v", err)
	}

	latest, err := db.GetLatestMessage(ctx, alias.ID)
	if err != nil {
		t.Fatalf("read legacy latest projection: %v", err)
	}
	if latest.UIDValidity != 91 || latest.UID != 1 || latest.Subject != first.Subject {
		t.Fatalf("legacy latest projection = %#v, want unread UID 1", latest)
	}
	archived, err := db.ListArchivedMailboxMessages(ctx, alias.ID)
	if err != nil {
		t.Fatalf("list v2 archive: %v", err)
	}
	if len(archived) != 2 || archived[1].MailboxUID != 2 || archived[1].Subject != seen.Subject {
		t.Fatalf("v2 archive = %#v, want both UIDs including seen message", archived)
	}
}

func TestSeenArchivedMessageDoesNotCreateLegacyLatestProjection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _ := openArchiveV2Store(t, 1<<20)
	account := createAccount(t, ctx, db, "Seen only projection", "seen-only@icloud.com")
	alias := createLegacyAlias(t, ctx, db, account.ID, "seen-only-alias@icloud.com", bytes.Repeat([]byte{0x72}, 32))
	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{alias}, []domain.ArchivedMessage{{
		AccountID: account.ID, UIDValidity: 92, UID: 4, InternalDate: at,
		MessageID: "<read-only@example.test>", Subject: "read only",
		ContentState: domain.ArchiveContentMetadata, AliasIDs: []int64{alias.ID}, UpstreamSeen: true,
	}}, 92, 4, true)

	if _, err := db.GetLatestMessage(ctx, alias.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy latest for seen-only alias = %v, want ErrNotFound", err)
	}
	archived, err := db.ListArchivedMailboxMessages(ctx, alias.ID)
	if err != nil || len(archived) != 1 || archived[0].MailboxUID != 1 {
		t.Fatalf("seen-only v2 archive = %#v, %v", archived, err)
	}
}

func TestSameGenerationResetPreservesLegacySnapshotAfterCursorInvalidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _ := openArchiveV2Store(t, 1<<20)
	account := createAccount(t, ctx, db, "Reset snapshot preservation", "reset-preservation@icloud.com")
	alias := createLegacyAlias(t, ctx, db, account.ID, "reset-preservation-alias@icloud.com", bytes.Repeat([]byte{0x73}, 32))
	at := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{alias}, []domain.ArchivedMessage{{
		AccountID: account.ID, UIDValidity: 93, UID: 7, InternalDate: at,
		MessageID: "<preserved@example.test>", Subject: "preserve this snapshot",
		ContentState: domain.ArchiveContentMetadata, AliasIDs: []int64{alias.ID},
	}}, 93, 7, true)

	if _, err := db.DB().ExecContext(ctx, `DELETE FROM imap_sync_states WHERE account_id = ?`, account.ID); err != nil {
		t.Fatalf("invalidate mailbox cursor: %v", err)
	}
	account, err := db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("reload account after cursor invalidation: %v", err)
	}
	if err := db.ApplyMailboxSync(ctx, account.ID, account.UpdatedAt, []domain.Alias{alias}, domain.MailboxSyncResult{
		State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 93, LastUID: 7, UpdatedAt: at.Add(time.Minute)},
		Reset: true,
	}, at.Add(time.Minute)); err != nil {
		t.Fatalf("apply same-generation reset: %v", err)
	}
	latest, err := db.GetLatestMessage(ctx, alias.ID)
	if err != nil {
		t.Fatalf("read snapshot after same-generation reset: %v", err)
	}
	if latest.UIDValidity != 93 || latest.UID != 7 || latest.Subject != "preserve this snapshot" {
		t.Fatalf("snapshot after same-generation reset = %#v", latest)
	}
}
