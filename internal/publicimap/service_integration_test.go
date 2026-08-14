package publicimap

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

func TestIMAPSReadOnlyMailboxPasswordLoginFetchIdleAndRotation(t *testing.T) {
	db, cipher, service, account, alias, credentials := startIMAPV2Fixture(t)
	client := dialIMAPV2Client(t, service.Address(), nil)
	defer client.Close()
	if err := client.Login(alias.Address, credentials.IMAPPassword).Wait(); err != nil {
		t.Fatalf("password login: %v", err)
	}

	mailboxes, err := client.List("", "*", nil).Collect()
	if err != nil || len(mailboxes) != 1 || mailboxes[0].Mailbox != "INBOX" {
		t.Fatalf("LIST = %#v, %v", mailboxes, err)
	}
	status, err := client.Status("INBOX", &imap.StatusOptions{
		NumMessages: true, UIDNext: true, UIDValidity: true, NumUnseen: true,
	}).Wait()
	if err != nil || status.NumMessages == nil || *status.NumMessages != 2 ||
		status.UIDNext != imap.UID(alias.MailboxUIDNext) || status.UIDValidity != alias.MailboxUIDValidity {
		t.Fatalf("STATUS = %#v, %v", status, err)
	}
	selected, err := client.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil || selected.NumMessages != 2 || len(selected.PermanentFlags) != 0 {
		t.Fatalf("EXAMINE = %#v, %v", selected, err)
	}
	search, err := client.Search(&imap.SearchCriteria{}, nil).Wait()
	if err != nil || !equalUint32s(search.AllSeqNums(), []uint32{1, 2}) {
		t.Fatalf("SEARCH ALL = %#v, %v", search, err)
	}
	uidSearch, err := client.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{
		{Key: "Subject", Value: "oversized original"},
	}}, nil).Wait()
	if err != nil || !equalUIDs(uidSearch.AllUIDs(), []imap.UID{2}) {
		t.Fatalf("UID SEARCH subject = %#v, %v", uidSearch, err)
	}

	section := &imap.FetchItemBodySection{Peek: true}
	var all imap.SeqSet
	all.AddRange(1, 2)
	fetched, err := client.Fetch(all, &imap.FetchOptions{
		UID: true, Flags: true, Envelope: true, RFC822Size: true,
		BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil || len(fetched) != 2 {
		t.Fatalf("FETCH = %#v, %v", fetched, err)
	}
	wantRaw := []byte("From: sender@example.test\r\nTo: imaps-alias@icloud.com\r\nSubject: complete original\r\n\r\ncomplete body")
	if got := fetched[0].FindBodySection(section); !bytes.Equal(got, wantRaw) || fetched[0].UID != 1 {
		t.Fatalf("complete FETCH body = %q UID=%d", got, fetched[0].UID)
	}
	placeholder := string(fetched[1].FindBodySection(section))
	if fetched[1].UID != 2 || !strings.Contains(placeholder, "Subject: oversized original") ||
		!strings.Contains(placeholder, "Message-ID: <oversized@example.test>") ||
		!strings.Contains(placeholder, "100 MiB") {
		t.Fatalf("placeholder FETCH body = %q UID=%d", placeholder, fetched[1].UID)
	}
	uidFetched, err := client.Fetch(imap.UIDSetNum(2), &imap.FetchOptions{
		UID: true, BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil || len(uidFetched) != 1 || uidFetched[0].UID != 2 {
		t.Fatalf("UID FETCH = %#v, %v", uidFetched, err)
	}

	assertIMAPReadOnlyError(t, "APPEND", appendIMAPV2(client))
	assertIMAPReadOnlyError(t, "STORE", client.Store(
		imap.SeqSetNum(1),
		&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}},
		nil,
	).Close())
	_, moveErr := client.Move(imap.SeqSetNum(1), "INBOX").Wait()
	assertIMAPReadOnlyError(t, "MOVE", moveErr)
	assertIMAPReadOnlyError(t, "DELETE", client.Delete("INBOX").Wait())
	assertIMAPReadOnlyError(t, "EXPUNGE", client.Expunge().Close())

	updates := make(chan uint32, 1)
	client.Close()
	client = dialIMAPV2Client(t, service.Address(), &imapclient.UnilateralDataHandler{
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil {
				select {
				case updates <- *data.NumMessages:
				default:
				}
			}
		},
	})
	defer client.Close()
	if err := client.Login(alias.Address, credentials.IMAPPassword).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	idle, err := client.Idle()
	if err != nil {
		t.Fatalf("start IDLE: %v", err)
	}
	applyIMAPV2Messages(t, db, account.ID, []domain.Alias{alias}, []domain.ArchivedMessage{
		{
			AccountID: account.ID, UIDValidity: 88, UID: 3,
			InternalDate: time.Date(2026, 8, 11, 3, 0, 0, 0, time.UTC),
			Subject:      "arrived during IDLE", RawMIME: []byte("Subject: arrived during IDLE\r\n\r\nbody"),
			AliasIDs: []int64{alias.ID},
		},
	}, 3, false)
	select {
	case count := <-updates:
		if count != 3 {
			t.Fatalf("IDLE update count = %d, want 3", count)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("IDLE did not publish the new message count")
	}
	if err := idle.Close(); err != nil {
		t.Fatalf("stop IDLE: %v", err)
	}
	if err := idle.Wait(); err != nil {
		t.Fatalf("finish IDLE: %v", err)
	}

	rotated, err := db.RotateAliasCredentials(context.Background(), alias.ID)
	if err != nil {
		t.Fatalf("rotate IMAPS credentials: %v", err)
	}
	if _, err := client.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait(); err == nil {
		t.Fatal("existing password session survived credential rotation")
	}
	oldPasswordClient := dialIMAPV2Client(t, service.Address(), nil)
	defer oldPasswordClient.Close()
	if err := oldPasswordClient.Login(alias.Address, credentials.IMAPPassword).Wait(); err == nil {
		t.Fatal("old IMAP password survived credential rotation")
	}
	newCredentials, err := cipher.DecryptAliasCredentials(rotated.ID, rotated.CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	newPasswordClient := dialIMAPV2Client(t, service.Address(), nil)
	defer newPasswordClient.Close()
	if err := newPasswordClient.Login(rotated.Address, newCredentials.IMAPPassword).Wait(); err != nil {
		t.Fatalf("new IMAP password login: %v", err)
	}
}

func TestIMAPSXOAUTH2LoginAndRotationInvalidation(t *testing.T) {
	db, cipher, service, _, alias, credentials := startIMAPV2Fixture(t)
	now := time.Now().UTC()
	accessToken, err := cipher.IssueAliasAccessToken(
		alias.ID, alias.CredentialVersion, alias.RefreshTokenHash, now.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := dialIMAPV2Client(t, service.Address(), nil)
	defer client.Close()
	if err := client.Authenticate(&xoauth2TestClient{username: alias.Address, token: accessToken}); err != nil {
		t.Fatalf("XOAUTH2 authenticate: %v", err)
	}
	if _, err := client.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait(); err != nil {
		t.Fatalf("XOAUTH2 STATUS: %v", err)
	}

	rotated, err := db.RotateAliasCredentials(context.Background(), alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait(); err == nil {
		t.Fatal("existing XOAUTH2 session survived credential rotation")
	}
	oldTokenClient := dialIMAPV2Client(t, service.Address(), nil)
	defer oldTokenClient.Close()
	if err := oldTokenClient.Authenticate(&xoauth2TestClient{username: alias.Address, token: accessToken}); err == nil {
		t.Fatal("old XOAUTH2 access token survived credential rotation")
	}
	newCredentials, err := cipher.DecryptAliasCredentials(rotated.ID, rotated.CredentialCiphertext)
	if err != nil || newCredentials.RefreshToken == credentials.RefreshToken {
		t.Fatalf("decrypt rotated credentials = %#v, %v", newCredentials, err)
	}
	newAccessToken, err := cipher.IssueAliasAccessToken(
		rotated.ID, rotated.CredentialVersion, rotated.RefreshTokenHash, now.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	newTokenClient := dialIMAPV2Client(t, service.Address(), nil)
	defer newTokenClient.Close()
	if err := newTokenClient.Authenticate(&xoauth2TestClient{username: rotated.Address, token: newAccessToken}); err != nil {
		t.Fatalf("new XOAUTH2 access token login: %v", err)
	}
}

type xoauth2TestClient struct {
	username string
	token    string
}

var _ sasl.Client = (*xoauth2TestClient)(nil)

func (client *xoauth2TestClient) Start() (string, []byte, error) {
	return "XOAUTH2", []byte("user=" + client.username + "\x01auth=Bearer " + client.token + "\x01\x01"), nil
}

func (*xoauth2TestClient) Next([]byte) ([]byte, error) {
	return nil, sasl.ErrUnexpectedServerChallenge
}

func startIMAPV2Fixture(
	t *testing.T,
) (*store.Store, *secure.Cipher, *Service, domain.Account, domain.Alias, domain.AliasCredentials) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "public-imap-v2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.ConfigureMailArchive(filepath.Join(t.TempDir(), "archive"), 1<<20); err != nil {
		t.Fatal(err)
	}
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, issueErr
	})
	passwordCiphertext, err := cipher.Encrypt("upstream-password")
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateAccount(ctx, domain.Account{
		Name: "Public IMAPS", Email: "public-imaps@icloud.com",
		IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPUsername: "public-imaps@icloud.com",
		PasswordCiphertext: passwordCiphertext, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	alias, err := db.CreateAlias(ctx, domain.Alias{
		AccountID: account.ID, Address: "imaps-alias@icloud.com", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := cipher.DecryptAliasCredentials(alias.ID, alias.CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	applyIMAPV2Messages(t, db, account.ID, []domain.Alias{alias}, []domain.ArchivedMessage{
		{
			AccountID: account.ID, UIDValidity: 88, UID: 1,
			InternalDate: time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC),
			Subject:      "complete original",
			RawMIME:      []byte("From: sender@example.test\r\nTo: imaps-alias@icloud.com\r\nSubject: complete original\r\n\r\ncomplete body"),
			AliasIDs:     []int64{alias.ID},
		},
		{
			AccountID: account.ID, UIDValidity: 88, UID: 2,
			InternalDate: time.Date(2026, 8, 11, 2, 0, 0, 0, time.UTC),
			MessageID:    "<oversized@example.test>", Subject: "oversized original", ContentState: domain.ArchiveContentOversized,
			RawSize: 100<<20 + 1, AliasIDs: []int64{alias.ID},
		},
	}, 2, true)
	alias, err = db.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}

	certFile := filepath.Join(t.TempDir(), "imap-cert.pem")
	keyFile := filepath.Join(t.TempDir(), "imap-key.pem")
	tlsConfig, _, err := LoadOrCreateTLSConfig(certFile, keyFile, "localhost", true)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(db, cipher, tlsConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- service.Serve() }()
	t.Cleanup(func() {
		_ = service.Close()
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Error("IMAPS service did not stop")
		}
	})
	return db, cipher, service, account, alias, credentials
}

func applyIMAPV2Messages(
	t *testing.T,
	db *store.Store,
	accountID int64,
	aliases []domain.Alias,
	messages []domain.ArchivedMessage,
	lastUID uint32,
	reset bool,
) {
	t.Helper()
	ctx := context.Background()
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	if err := db.ApplyMailboxSync(ctx, accountID, account.UpdatedAt, aliases, domain.MailboxSyncResult{
		ArchivedMessages: messages,
		State: domain.IMAPSyncState{
			AccountID: accountID, UIDValidity: 88, LastUID: lastUID, UpdatedAt: observed,
		},
		Reset: reset,
	}, observed); err != nil {
		t.Fatalf("apply IMAPS fixture messages: %v", err)
	}
}

func dialIMAPV2Client(
	t *testing.T,
	address string,
	handler *imapclient.UnilateralDataHandler,
) *imapclient.Client {
	t.Helper()
	client, err := imapclient.DialTLS(address, &imapclient.Options{
		TLSConfig:             &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}, // local generated fixture
		UnilateralDataHandler: handler,
	})
	if err != nil {
		t.Fatalf("dial IMAPS fixture: %v", err)
	}
	return client
}

func appendIMAPV2(client *imapclient.Client) error {
	content := []byte("Subject: rejected append\r\n\r\nbody")
	command := client.Append("INBOX", int64(len(content)), nil)
	_, writeErr := command.Write(content)
	closeErr := command.Close()
	_, waitErr := command.Wait()
	return errors.Join(writeErr, closeErr, waitErr)
}

func assertIMAPReadOnlyError(t *testing.T, operation string, err error) {
	t.Helper()
	var protocolError *imap.Error
	if !errors.As(err, &protocolError) || protocolError.Code != imap.ResponseCode("READ-ONLY") {
		t.Fatalf("%s error = %v, want [READ-ONLY]", operation, err)
	}
}

func equalUint32s(left, right []uint32) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalUIDs(left, right []imap.UID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
