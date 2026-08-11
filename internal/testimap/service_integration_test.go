package testimap_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	mailfetch "icloud-api/internal/mail"
	"icloud-api/internal/testimap"
)

func TestServiceSupportsProjectFetchAndMarkSeenOverTLS(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "test-imap-ca.pem")
	service, err := testimap.NewService(testimap.ServiceConfig{
		IMAPAddr:     "127.0.0.1:0",
		ControlAddr:  "127.0.0.1:0",
		ServerName:   "localhost",
		CAFile:       caFile,
		ControlToken: "0123456789abcdef-test-token",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	testAccount, err := service.Backend().CreateAccount(
		"ui-test@icloud.test", "ui-test-password", "ui-test@icloud.test",
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serviceDone := make(chan error, 1)
	go func() { serviceDone <- service.Run(ctx) }()
	var endpoints testimap.ServiceEndpoints
	select {
	case endpoints = <-service.Ready():
	case err := <-serviceDone:
		t.Fatalf("service stopped before readiness: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("test IMAP service did not become ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serviceDone:
			if err != nil {
				t.Errorf("service shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("test IMAP service did not stop")
		}
	})

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	fetcher := mailfetch.NewFetcher()
	fetcher.IMAPTimeout = 3 * time.Second
	if err := fetcher.ConfigureTestIMAPEndpoint(endpoints.IMAPAddress, endpoints.TLSServerName, caPEM); err != nil {
		t.Fatal(err)
	}
	account := domain.Account{
		ID:           testAccount.ID,
		Email:        testAccount.ForwardAddress,
		IMAPHost:     "imap.mail.me.com",
		IMAPPort:     993,
		IMAPUsername: testAccount.Username,
		Enabled:      true,
	}
	alias := domain.Alias{ID: 41, AccountID: account.ID, Address: "alias@icloud.test", Enabled: true}

	baseline, err := fetcher.FetchIncremental(context.Background(), account, "ui-test-password", []domain.Alias{alias}, nil, nil)
	if err != nil {
		t.Fatalf("establish empty baseline: %v", err)
	}
	if !baseline.Reset || baseline.State.UIDValidity == 0 || baseline.State.LastUID != 0 {
		t.Fatalf("baseline = %#v", baseline)
	}

	preset, _, err := testimap.PresetMessage("verification-code", alias.Address, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := testimap.RenderMessage(preset, account.Email, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.Backend().AddMessage(testAccount.ID, raw, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}

	result, err := fetcher.FetchIncremental(
		context.Background(), account, "ui-test-password", []domain.Alias{alias}, &baseline.State, nil,
	)
	if err != nil {
		t.Fatalf("fetch incremental message: %v", err)
	}
	message, ok := result.Messages[alias.ID]
	if !ok {
		t.Fatalf("result has no message for alias: %#v", result)
	}
	if message.UID != stored.UID || message.Subject != "Your temporary ChatGPT verification code" ||
		!strings.Contains(message.HTMLBody, "123456") {
		t.Fatalf("fetched message = %#v", message)
	}

	if err := fetcher.MarkSeen(
		context.Background(), account, "ui-test-password", result.State.UIDValidity, []uint32{message.UID},
	); err != nil {
		t.Fatalf("mark message seen: %v", err)
	}
	messages, err := service.Backend().ListStoredMessages(testAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || !messages[0].Seen {
		t.Fatalf("stored messages after MarkSeen = %#v", messages)
	}
}
