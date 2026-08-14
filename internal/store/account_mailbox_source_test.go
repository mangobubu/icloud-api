package store_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestUpdateAccountMailboxSourceChangeClearsEveryUIDScopedProjection(t *testing.T) {
	ctx := context.Background()
	db, archiveRoot := openArchiveV2Store(t, 1<<20)
	account := createAccount(t, ctx, db, "Source reset", "source-reset@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "source-reset-alias@icloud.com", bytes.Repeat([]byte{0x71}, 32))
	observed := time.Date(2026, 8, 14, 4, 0, 0, 0, time.UTC)
	oldRaw := []byte("From: old@example.test\r\nTo: source-reset-alias@icloud.com\r\nSubject: old source\r\n\r\nold")
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{alias}, []domain.ArchivedMessage{{
		AccountID: account.ID, UIDValidity: 91, UID: 7, InternalDate: observed,
		Subject: "old source", RawMIME: oldRaw, OTP: "111111", AliasIDs: []int64{alias.ID},
	}}, 91, 7, true)
	oldMessage := oneArchivedMailboxMessage(t, ctx, db, alias.ID)
	oldContentPath := filepath.Join(archiveRoot, filepath.FromSlash(oldMessage.ContentPath))
	if _, err := os.Stat(oldContentPath); err != nil {
		t.Fatalf("old archive content: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at)
		VALUES(?, 91, 7, ?)`, alias.ID, observed.UnixNano()); err != nil {
		t.Fatalf("seed consumption state: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO imap_seen_tasks(account_id, uid_validity, uid, created_at)
		VALUES(?, 91, 7, ?)`, account.ID, observed.UnixNano()); err != nil {
		t.Fatalf("seed Seen task: %v", err)
	}
	before, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("read alias before source reset: %v", err)
	}

	account, err = db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("read account before source reset: %v", err)
	}
	account.IMAPUsername = "replacement-login@example.test"
	if _, err := db.UpdateAccount(ctx, account); err != nil {
		t.Fatalf("change mailbox source: %v", err)
	}

	if messages, err := db.ListArchivedMailboxMessages(ctx, alias.ID); err != nil || len(messages) != 0 {
		t.Fatalf("archive after source reset = %#v, %v", messages, err)
	}
	if stats, err := db.MailArchiveStats(ctx); err != nil || stats.MessageCount != 0 || stats.ContentBytes != 0 {
		t.Fatalf("archive stats after source reset = %#v, %v", stats, err)
	}
	if _, err := db.GetLatestMessage(ctx, alias.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy snapshot after source reset = %v, want ErrNotFound", err)
	}
	if _, err := db.GetIMAPSyncState(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("sync cursor after source reset = %v, want ErrNotFound", err)
	}
	for name, query := range map[string]string{
		"consumption": `SELECT COUNT(*) FROM consumed_messages WHERE alias_id = ?`,
		"Seen tasks":  `SELECT COUNT(*) FROM imap_seen_tasks WHERE account_id = ?`,
	} {
		var count int
		id := account.ID
		if name == "consumption" {
			id = alias.ID
		}
		if err := db.DB().QueryRowContext(ctx, query, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows after source reset = %d, %v", name, count, err)
		}
	}
	if _, err := os.Stat(oldContentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old archive content still present after source reset: %v", err)
	}
	after, err := db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatalf("read alias after source reset: %v", err)
	}
	if after.MailboxUIDValidity == before.MailboxUIDValidity || after.MailboxUIDNext != 1 {
		t.Fatalf("alias mailbox generation = validity %d -> %d, next=%d",
			before.MailboxUIDValidity, after.MailboxUIDValidity, after.MailboxUIDNext)
	}

	// Reusing the same upstream UID identity on the replacement source must
	// publish the new content without colliding with the old MIME file.
	newRaw := []byte("From: new@example.test\r\nTo: source-reset-alias@icloud.com\r\nSubject: new source\r\n\r\nnew")
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{after}, []domain.ArchivedMessage{{
		AccountID: account.ID, UIDValidity: 91, UID: 7, InternalDate: observed.Add(time.Hour),
		Subject: "new source", RawMIME: newRaw, OTP: "222222", AliasIDs: []int64{alias.ID},
	}}, 91, 7, true)
	newMessage := oneArchivedMailboxMessage(t, ctx, db, alias.ID)
	if newMessage.MailboxUID != 1 || newMessage.UIDValidity != after.MailboxUIDValidity {
		t.Fatalf("new source mailbox identity = %#v", newMessage)
	}
	if content, err := db.ReadArchivedContent(newMessage); err != nil || !bytes.Equal(content, newRaw) {
		t.Fatalf("new source MIME = %q, %v", content, err)
	}
}

func TestReconcileMailArchiveRecoversInterruptedSourceReset(t *testing.T) {
	ctx := context.Background()
	db, archiveRoot := openArchiveV2Store(t, 1<<20)
	account := createAccount(t, ctx, db, "Interrupted reset", "interrupted-reset@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "interrupted-reset-alias@icloud.com", bytes.Repeat([]byte{0x73}, 32))
	observed := time.Date(2026, 8, 14, 6, 0, 0, 0, time.UTC)
	raw := []byte("From: sender@example.test\r\nTo: interrupted-reset-alias@icloud.com\r\nSubject: recover\r\n\r\nrecover")
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{alias}, []domain.ArchivedMessage{{
		AccountID: account.ID, UIDValidity: 93, UID: 9, InternalDate: observed,
		Subject: "recover", RawMIME: raw, AliasIDs: []int64{alias.ID},
	}}, 93, 9, true)
	message := oneArchivedMailboxMessage(t, ctx, db, alias.ID)
	original := filepath.Join(archiveRoot, "account-"+strconvFormatInt64(account.ID))
	quarantine, err := os.MkdirTemp(
		filepath.Join(archiveRoot, ".tmp"),
		"source-reset-account-"+strconvFormatInt64(account.ID)+"-",
	)
	if err != nil {
		t.Fatalf("create interrupted quarantine: %v", err)
	}
	if err := os.Rename(original, filepath.Join(quarantine, "archive")); err != nil {
		t.Fatalf("simulate interrupted quarantine: %v", err)
	}
	if err := db.ReconcileMailArchive(ctx); err != nil {
		t.Fatalf("reconcile interrupted source reset: %v", err)
	}
	if _, err := os.Stat(quarantine); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine still exists after recovery: %v", err)
	}
	recovered, err := db.GetArchivedMailboxMessage(ctx, alias.ID, message.MailboxUID)
	if err != nil {
		t.Fatalf("read recovered message: %v", err)
	}
	if content, err := db.ReadArchivedContent(recovered); err != nil || !bytes.Equal(content, raw) {
		t.Fatalf("recovered MIME = %q, %v", content, err)
	}
}

func TestDeleteAccountRemovesArchivedContentFiles(t *testing.T) {
	ctx := context.Background()
	db, archiveRoot := openArchiveV2Store(t, 1<<20)
	account := createAccount(t, ctx, db, "Archive deletion", "archive-deletion@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "archive-deletion-alias@icloud.com", bytes.Repeat([]byte{0x74}, 32))
	observed := time.Date(2026, 8, 14, 7, 0, 0, 0, time.UTC)
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{alias}, []domain.ArchivedMessage{{
		AccountID: account.ID, UIDValidity: 94, UID: 10, InternalDate: observed,
		Subject: "delete", RawMIME: []byte("Subject: delete\r\n\r\ndelete"), AliasIDs: []int64{alias.ID},
	}}, 94, 10, true)
	message := oneArchivedMailboxMessage(t, ctx, db, alias.ID)
	contentPath := filepath.Join(archiveRoot, filepath.FromSlash(message.ContentPath))

	if err := db.DeleteAccount(ctx, account.ID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if _, err := os.Stat(contentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archived content remains after account deletion: %v", err)
	}
	if stats, err := db.MailArchiveStats(ctx); err != nil || stats.MessageCount != 0 || stats.ContentBytes != 0 {
		t.Fatalf("archive stats after account deletion = %#v, %v", stats, err)
	}
	if err := db.ReconcileMailArchive(ctx); err != nil {
		t.Fatalf("reconcile after account deletion: %v", err)
	}
}

func strconvFormatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
