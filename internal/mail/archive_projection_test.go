package mail

import (
	"context"
	"errors"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

func TestProjectParsedArchiveContentPreservesLegacyMessageFields(t *testing.T) {
	headerDate := time.Date(2026, 8, 12, 3, 4, 5, 0, time.UTC)
	parsed := parsedMessage{
		messageID:  "<compat@example.test>",
		headerDate: &headerDate,
		from:       []domain.MailAddress{{Name: "Sender", Email: "sender@example.test"}},
		to:         []domain.MailAddress{{Email: "alias@icloud.com"}},
		cc:         []domain.MailAddress{{Email: "copy@example.test"}},
		subject:    "Compatibility subject",
		textBody:   "Plain compatibility body",
		htmlBody:   "<p>HTML compatibility body</p>",
		attachments: []domain.Attachment{{
			Filename: "invoice.pdf", ContentType: "application/pdf", Size: 42,
		}},
		bodyTruncated: true,
	}
	var archived domain.ArchivedMessage
	projectParsedArchiveContent(&archived, parsed)

	if archived.MessageID != parsed.messageID || archived.HeaderDate != parsed.headerDate ||
		archived.Subject != parsed.subject || archived.TextBody != parsed.textBody ||
		archived.HTMLBody != parsed.htmlBody || !archived.BodyTruncated {
		t.Fatalf("archive projection omitted parsed content: %#v", archived)
	}
	if len(archived.From) != 1 || len(archived.To) != 1 || len(archived.CC) != 1 ||
		len(archived.Attachments) != 1 || archived.Attachments[0] != parsed.attachments[0] {
		t.Fatalf("archive projection omitted address or attachment metadata: %#v", archived)
	}
	parsed.attachments[0].Filename = "changed-after-projection.pdf"
	if archived.Attachments[0].Filename != "invoice.pdf" {
		t.Fatal("archive projection retained the parser's mutable attachment slice")
	}
}

func TestValidateLegacySnapshotPositionsAcceptsEveryEnabledCredentialMode(t *testing.T) {
	aliases := []domain.Alias{
		{ID: 1, Enabled: true, CredentialMode: domain.AliasCredentialModeLegacy},
		{ID: 2, Enabled: true, CredentialMode: domain.AliasCredentialModeV2},
		{ID: 3, Enabled: false, CredentialMode: domain.AliasCredentialModeLegacy},
	}
	if err := validateLegacySnapshotPositions(aliases, map[int64]domain.MailboxSnapshotPosition{
		1: {AliasID: 1, UIDValidity: 7, UID: 9},
		2: {AliasID: 2, UIDValidity: 7, UID: 10},
	}); err != nil {
		t.Fatalf("valid compatibility positions: %v", err)
	}
	for _, position := range []domain.MailboxSnapshotPosition{
		{AliasID: 3, UIDValidity: 7, UID: 9},
		{AliasID: 1, UIDValidity: 0, UID: 9},
		{AliasID: 1, UIDValidity: 7, UID: 0},
	} {
		if err := validateLegacySnapshotPositions(aliases, map[int64]domain.MailboxSnapshotPosition{
			position.AliasID: position,
		}); err == nil {
			t.Fatalf("invalid position accepted: %#v", position)
		}
	}
}

func TestReconcileLegacySnapshotPositionsSkipsIMAPForEmptyOrOldGenerationOnly(t *testing.T) {
	updates, err := reconcileLegacySnapshotPositions(
		context.Background(), nil, nil, 88,
	)
	if err != nil || len(updates) != 0 {
		t.Fatalf("empty positions = %#v, %v", updates, err)
	}

	updates, err = reconcileLegacySnapshotPositions(
		context.Background(), nil,
		map[int64]domain.MailboxSnapshotPosition{
			5: {AliasID: 5, UIDValidity: 77, UID: 12},
		},
		88,
	)
	if err != nil {
		t.Fatalf("old-generation reconciliation: %v", err)
	}
	update := updates[5]
	if len(updates) != 1 || update.AliasID != 5 || update.UIDValidity != 88 ||
		update.UID != 0 || update.SnapshotState != domain.SnapshotEmpty {
		t.Fatalf("old-generation update = %#v", updates)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = reconcileLegacySnapshotPositions(
		canceled, nil,
		map[int64]domain.MailboxSnapshotPosition{
			5: {AliasID: 5, UIDValidity: 88, UID: 12},
		},
		88,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reconciliation error = %v", err)
	}
}

func TestLegacySnapshotUpdatesCoverExpungeEmptyMailboxAndReset(t *testing.T) {
	positions := map[int64]domain.MailboxSnapshotPosition{
		1: {AliasID: 1, UIDValidity: 88, UID: 10},
		2: {AliasID: 2, UIDValidity: 88, UID: 11},
	}

	expunge := legacySnapshotUpdatesForFoundUIDs(
		positions, 88, map[uint32]struct{}{10: {}},
	)
	if len(expunge) != 1 || expunge[2].SnapshotState != domain.SnapshotEmpty {
		t.Fatalf("expunge updates = %#v, want only alias 2 empty", expunge)
	}
	if _, replaced := expunge[1]; replaced {
		t.Fatalf("existing UID was invalidated: %#v", expunge)
	}

	emptyMailbox := legacySnapshotUpdatesForFoundUIDs(positions, 88, nil)
	if len(emptyMailbox) != 2 || emptyMailbox[1].SnapshotState != domain.SnapshotEmpty ||
		emptyMailbox[2].SnapshotState != domain.SnapshotEmpty {
		t.Fatalf("empty mailbox updates = %#v", emptyMailbox)
	}

	reset := legacySnapshotUpdatesForFoundUIDs(
		positions, 99, map[uint32]struct{}{10: {}, 11: {}},
	)
	if len(reset) != 2 || reset[1].UIDValidity != 99 || reset[2].UIDValidity != 99 {
		t.Fatalf("UIDVALIDITY reset updates = %#v", reset)
	}
}
