package testimap

import (
	"testing"
	"time"

	"github.com/emersion/go-imap"
)

func TestResetDoesNotReuseAccountIdentityForExistingSessions(t *testing.T) {
	backend := NewBackend()
	first, err := backend.CreateAccount("first@icloud.test", "first-password", "first@icloud.test")
	if err != nil {
		t.Fatal(err)
	}
	user, err := backend.Login(&imap.ConnInfo{}, first.Username, "first-password")
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := user.GetMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}

	backend.Reset()
	second, err := backend.CreateAccount("second@icloud.test", "second-password", "second@icloud.test")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("account ID was reused after reset: %d", second.ID)
	}
	if _, err := mailbox.Status([]imap.StatusItem{imap.StatusMessages}); err == nil {
		t.Fatal("mailbox from the reset account accessed newly created state")
	}
}

func TestAccountSnapshotIncludesRawMessageBytes(t *testing.T) {
	backend := NewBackend()
	account, err := backend.CreateAccount("size@icloud.test", "size-password", "size@icloud.test")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{[]byte("Subject: one\r\n\r\n1234"), []byte("Subject: two\r\n\r\n123456")} {
		if _, err := backend.AddMessage(account.ID, raw, time.Now(), false); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := backend.GetAccount(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MessageCount != 2 || snapshot.MessageBytes != int64(len("Subject: one\r\n\r\n1234")+len("Subject: two\r\n\r\n123456")) {
		t.Fatalf("account size snapshot = %#v", snapshot)
	}
}

func TestBackendUsesConfiguredInitialUIDValidity(t *testing.T) {
	backend := newBackendWithUIDValidity(424242)
	account, err := backend.CreateAccount("uid@icloud.test", "uid-password", "uid@icloud.test")
	if err != nil {
		t.Fatal(err)
	}
	if account.UIDValidity != 424242 {
		t.Fatalf("UIDVALIDITY = %d, want 424242", account.UIDValidity)
	}
}

func TestRenderMessageRejectsHeaderInjection(t *testing.T) {
	tests := []MessageInput{
		{FromEmail: "sender@example.test\r\nBcc: target@example.test", Alias: "alias@icloud.test"},
		{FromEmail: "sender@example.test", Alias: "alias@icloud.test\r\nBcc: target@example.test"},
		{FromName: "Sender\r\nBcc: target@example.test", FromEmail: "sender@example.test", Alias: "alias@icloud.test"},
		{FromEmail: "sender@example.test", Alias: "alias@icloud.test", Headers: map[string]string{"X-Test: Injected": "value"}},
	}
	for index, input := range tests {
		if _, err := RenderMessage(input, "owner@icloud.test", time.Now()); err == nil {
			t.Fatalf("injection case %d unexpectedly rendered", index)
		}
	}
}
