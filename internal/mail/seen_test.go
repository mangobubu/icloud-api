package mail

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
)

func TestMarkSeenUsesOneWritableSessionAndSilentUIDStore(t *testing.T) {
	session := &fakeIMAPSession{uidValidity: 9001, uidNext: 20}
	fetcher := testFetcher(session, time.Time{})

	err := fetcher.MarkSeen(context.Background(), testAccount(), "app-password", 9001, []uint32{9, 3, 9, 4})
	if err != nil {
		t.Fatalf("MarkSeen() error = %v", err)
	}

	login, selects, searches, _, terminated := session.counters()
	if login != 1 || selects != 1 || searches != 0 {
		t.Fatalf("commands = login %d, select %d, search %d; want 1, 1, 0", login, selects, searches)
	}
	if session.selected != "INBOX" || session.readOnly {
		t.Fatalf("selected = %q, readOnly = %v; want INBOX read-write", session.selected, session.readOnly)
	}
	if !terminated {
		t.Fatal("session was not terminated")
	}

	stores := session.stores()
	if len(stores) != 1 {
		t.Fatalf("UID STORE calls = %d, want 1", len(stores))
	}
	if stores[0].seqSet != "3:4,9" {
		t.Fatalf("UID STORE set = %q, want %q", stores[0].seqSet, "3:4,9")
	}
	wantItem := imap.FormatFlagsOp(imap.AddFlags, true)
	if stores[0].item != wantItem {
		t.Fatalf("UID STORE item = %q, want %q", stores[0].item, wantItem)
	}
	if !reflect.DeepEqual(stores[0].value, []interface{}{imap.SeenFlag}) {
		t.Fatalf("UID STORE value = %#v, want Seen flag", stores[0].value)
	}
}

func TestMarkSeenEmptyUIDsDoesNotConnect(t *testing.T) {
	fetcher := NewFetcher()
	dialed := false
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		dialed = true
		return nil, errors.New("unexpected dial")
	}

	if err := fetcher.MarkSeen(context.Background(), testAccount(), "", 0, nil); err != nil {
		t.Fatalf("MarkSeen() error = %v", err)
	}
	if dialed {
		t.Fatal("empty UID list opened an IMAP connection")
	}
}

func TestMarkSeenHonorsCanceledContextBeforeValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetcher := NewFetcher()
	dialed := false
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		dialed = true
		return nil, errors.New("unexpected dial")
	}

	err := fetcher.MarkSeen(ctx, testAccount(), "app-password", 9001, []uint32{3})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("MarkSeen() error = %v, want context.Canceled", err)
	}
	if dialed {
		t.Fatal("canceled call opened an IMAP connection")
	}
}

func TestMarkSeenReturnsTypedUIDValidityMismatch(t *testing.T) {
	session := &fakeIMAPSession{uidValidity: 9002, uidNext: 20}
	err := testFetcher(session, time.Time{}).MarkSeen(
		context.Background(),
		testAccount(),
		"app-password",
		9001,
		[]uint32{3},
	)

	var mismatch *UIDValidityMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("MarkSeen() error = %v, want *UIDValidityMismatchError", err)
	}
	if mismatch.Expected != 9001 || mismatch.Actual != 9002 {
		t.Fatalf("mismatch = %#v, want expected 9001 / actual 9002", mismatch)
	}
	if stores := session.stores(); len(stores) != 0 {
		t.Fatalf("UID STORE calls = %d after UIDVALIDITY mismatch, want 0", len(stores))
	}
}

func TestMarkSeenRejectsInvalidUIDInputWithoutConnecting(t *testing.T) {
	tests := []struct {
		name     string
		expected uint32
		uids     []uint32
		want     string
	}{
		{name: "zero expected UIDVALIDITY", expected: 0, uids: []uint32{1}, want: "expected UIDVALIDITY is zero"},
		{name: "zero UID", expected: 9001, uids: []uint32{1, 0}, want: "UID is zero"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fetcher := NewFetcher()
			dialed := false
			fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
				dialed = true
				return nil, errors.New("unexpected dial")
			}
			err := fetcher.MarkSeen(context.Background(), testAccount(), "app-password", test.expected, test.uids)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("MarkSeen() error = %v, want containing %q", err, test.want)
			}
			if dialed {
				t.Fatal("invalid input opened an IMAP connection")
			}
		})
	}
}

func TestMarkSeenCancellationTerminatesBlockedUIDStore(t *testing.T) {
	session := &fakeIMAPSession{
		uidValidity:         9001,
		uidNext:             20,
		storeErr:            errors.New("connection terminated"),
		storeStarted:        make(chan struct{}),
		storeUntilTerminate: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- testFetcher(session, time.Time{}).MarkSeen(
			ctx,
			testAccount(),
			"app-password",
			9001,
			[]uint32{3},
		)
	}()

	select {
	case <-session.storeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("UID STORE did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("MarkSeen() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MarkSeen did not stop after context cancellation")
	}
}

func TestMarkSeenReportsCommandStage(t *testing.T) {
	tests := []struct {
		name    string
		session *fakeIMAPSession
		want    string
	}{
		{name: "login", session: &fakeIMAPSession{loginErr: errors.New("login failed")}, want: "login IMAP account to mark messages seen"},
		{name: "select", session: &fakeIMAPSession{uidValidity: 9001, selectErr: errors.New("select failed")}, want: "select INBOX read-write to mark messages seen"},
		{name: "store", session: &fakeIMAPSession{uidValidity: 9001, uidNext: 20, storeErr: errors.New("store failed")}, want: "mark IMAP messages seen with UID STORE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := testFetcher(test.session, time.Time{}).MarkSeen(
				context.Background(),
				testAccount(),
				"app-password",
				9001,
				[]uint32{3},
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("MarkSeen() error = %v, want containing %q", err, test.want)
			}
		})
	}
}
