package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap"

	"icloud-api/internal/domain"
)

type fakeIMAPSession struct {
	searchResults   [][]uint32
	searchErrors    []error
	searchIndex     int
	searchCriteria  []*imap.SearchCriteria
	uidOnlyResults  []uint32
	uidOnlySeqNums  []uint32
	headerByUID     map[uint32][]byte
	bodyByUID       map[uint32][]byte
	internalDates   map[uint32]time.Time
	fetchItems      [][]imap.FetchItem
	fetchSeqSets    []string
	fetchErrors     []error
	username        string
	password        string
	selected        string
	readOnly        bool
	messages        uint32
	uidNext         uint32
	loggedOut       bool
	terminated      bool
	zeroUIDValidity bool
}

func (f *fakeIMAPSession) Login(username, password string) error {
	f.username = username
	f.password = password
	return nil
}

func (f *fakeIMAPSession) Select(name string, readOnly bool) (*imap.MailboxStatus, error) {
	f.selected = name
	f.readOnly = readOnly
	uidValidity := uint32(4242)
	if f.zeroUIDValidity {
		uidValidity = 0
	}
	return &imap.MailboxStatus{
		Name: name, UidValidity: uidValidity, UidNext: f.uidNext, Messages: f.messages,
	}, nil
}

func (f *fakeIMAPSession) UidSearch(criteria *imap.SearchCriteria) ([]uint32, error) {
	f.searchCriteria = append(f.searchCriteria, criteria)
	call := f.searchIndex
	f.searchIndex++
	if call < len(f.searchErrors) && f.searchErrors[call] != nil {
		return nil, f.searchErrors[call]
	}
	if call >= len(f.searchResults) {
		return nil, fmt.Errorf("unexpected search %d", call)
	}
	return f.searchResults[call], nil
}

func (f *fakeIMAPSession) Fetch(seqset *imap.SeqSet, items []imap.FetchItem, ch chan *imap.Message) error {
	return f.fetch(seqset, items, ch)
}

func (f *fakeIMAPSession) UidFetch(seqset *imap.SeqSet, items []imap.FetchItem, ch chan *imap.Message) error {
	return f.fetch(seqset, items, ch)
}

func (f *fakeIMAPSession) fetch(seqset *imap.SeqSet, items []imap.FetchItem, ch chan *imap.Message) error {
	defer close(ch)
	call := len(f.fetchItems)
	f.fetchItems = append(f.fetchItems, append([]imap.FetchItem(nil), items...))
	f.fetchSeqSets = append(f.fetchSeqSets, seqset.String())
	if call < len(f.fetchErrors) && f.fetchErrors[call] != nil {
		return f.fetchErrors[call]
	}
	if len(items) == 1 && items[0] == imap.FetchUid {
		first := uint32(1)
		if seqset != nil && len(seqset.Set) > 0 && seqset.Set[0].Start != 0 {
			first = seqset.Set[0].Start
		}
		for index, uid := range f.uidOnlyResults {
			seqNum := first + uint32(index)
			if index < len(f.uidOnlySeqNums) {
				seqNum = f.uidOnlySeqNums[index]
			}
			ch <- &imap.Message{SeqNum: seqNum, Uid: uid}
		}
		return nil
	}
	headerFetch := false
	var requested *imap.BodySectionName
	for _, item := range items {
		if strings.Contains(string(item), "HEADER.FIELDS") {
			headerFetch = true
		}
		if section, err := imap.ParseBodySectionName(item); err == nil {
			requested = section
		}
	}
	if requested == nil {
		return errors.New("missing body section")
	}

	source := f.bodyByUID
	if headerFetch {
		source = f.headerByUID
	}
	for uid, raw := range source {
		responseSection := *requested
		responseSection.Peek = false
		if len(responseSection.Partial) == 2 {
			responseSection.Partial = []int{responseSection.Partial[0]}
		}
		ch <- &imap.Message{
			Uid:          uid,
			Size:         uint32(len(raw)),
			InternalDate: f.internalDates[uid],
			Body: map[*imap.BodySectionName]imap.Literal{
				&responseSection: bytes.NewBuffer(raw),
			},
		}
	}
	return nil
}

func (f *fakeIMAPSession) Logout() error {
	f.loggedOut = true
	return nil
}

func (f *fakeIMAPSession) Terminate() error {
	f.terminated = true
	return nil
}

type blockingLogoutSession struct {
	*fakeIMAPSession
	logoutStarted    chan struct{}
	logoutRelease    chan struct{}
	terminatedSignal chan struct{}
	terminateOnce    sync.Once
}

func (s *blockingLogoutSession) Logout() error {
	s.loggedOut = true
	close(s.logoutStarted)
	select {
	case <-s.logoutRelease:
	case <-s.terminatedSignal:
	}
	return nil
}

func (s *blockingLogoutSession) Terminate() error {
	s.terminated = true
	s.terminateOnce.Do(func() { close(s.terminatedSignal) })
	return nil
}

func TestFetcherFetchLatest(t *testing.T) {
	fixedNow := time.Date(2026, 8, 6, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	firstDate := fixedNow.Add(-2 * time.Hour)
	secondDate := fixedNow.Add(-time.Hour)
	session := &fakeIMAPSession{
		searchResults: [][]uint32{{9, 10, 11}, {20}},
		headerByUID: map[uint32][]byte{
			9:  []byte("To: alias@example.com\r\n\r\n"),
			10: []byte("X-Original-To: Alias@Example.com\r\n\r\n"),
			11: []byte("X-Original-To: notalias@example.com\r\n\r\n"),
			20: []byte("Delivered-To: second@example.com\r\n\r\n"),
		},
		bodyByUID: map[uint32][]byte{
			10: []byte("Message-ID: <ten@example.com>\r\nSubject: Ten\r\nTo: alias@example.com\r\n\r\nbody ten"),
			20: []byte("Message-ID: <twenty@example.com>\r\nSubject: Twenty\r\nTo: second@example.com\r\n\r\nbody twenty"),
		},
		internalDates: map[uint32]time.Time{10: firstDate, 20: secondDate},
	}

	fetcher := NewFetcher()
	fetcher.MaxCandidatesPerAlias = 3
	fetcher.now = func() time.Time { return fixedNow }
	var dialAddress, dialServerName string
	fetcher.dial = func(_ context.Context, address, serverName string, _ time.Duration) (imapSession, error) {
		dialAddress = address
		dialServerName = serverName
		return session, nil
	}

	account := domain.Account{
		ID:           7,
		Email:        "owner@icloud.com",
		IMAPUsername: "imap-user",
		Enabled:      true,
	}
	aliases := []domain.Alias{
		{ID: 1, AccountID: 7, Address: "alias@example.com", Enabled: true},
		{ID: 2, AccountID: 7, Address: "second@example.com", Enabled: true},
		{ID: 3, AccountID: 7, Address: "disabled@example.com", Enabled: false},
	}

	got, err := fetcher.FetchLatest(context.Background(), account, "app-password", aliases)
	if err != nil {
		t.Fatal(err)
	}
	if dialAddress != "imap.mail.me.com:993" || dialServerName != "imap.mail.me.com" {
		t.Fatalf("dial = %q server name %q", dialAddress, dialServerName)
	}
	if session.username != "imap-user" || session.password != "app-password" {
		t.Fatalf("login = %q / %q", session.username, session.password)
	}
	if session.selected != "INBOX" || !session.readOnly {
		t.Fatalf("select = %q read-only=%v", session.selected, session.readOnly)
	}
	if session.loggedOut || !session.terminated {
		t.Fatalf("cleanup = logout=%v terminate=%v", session.loggedOut, session.terminated)
	}
	if len(got) != 2 {
		t.Fatalf("messages = %#v", got)
	}
	if got[1].UID != 10 || got[1].Subject != "Ten" || got[1].TextBody != "body ten" {
		t.Fatalf("alias 1 message = %#v", got[1])
	}
	if got[1].SnapshotState != domain.SnapshotFound || got[2].SnapshotState != domain.SnapshotFound {
		t.Fatalf("snapshot states = %q / %q", got[1].SnapshotState, got[2].SnapshotState)
	}
	if got[2].UID != 20 || got[2].Subject != "Twenty" || got[2].TextBody != "body twenty" {
		t.Fatalf("alias 2 message = %#v", got[2])
	}
	for aliasID, message := range got {
		if message.AliasID != aliasID || message.UIDValidity != 4242 || !message.SyncedAt.Equal(fixedNow.UTC()) {
			t.Fatalf("alias %d identity = %#v", aliasID, message)
		}
	}
	if len(session.fetchItems) != 2 {
		t.Fatalf("fetch calls = %d, want 2", len(session.fetchItems))
	}
	for call, items := range session.fetchItems {
		foundPeek := false
		for _, item := range items {
			if strings.HasPrefix(string(item), "BODY.PEEK[") {
				foundPeek = true
			}
		}
		if !foundPeek {
			t.Fatalf("fetch call %d did not use BODY.PEEK: %#v", call, items)
		}
	}
	for _, criteria := range session.searchCriteria {
		headerNames := make(map[string]struct{})
		collectSearchHeaderNames(criteria, headerNames)
		for _, weak := range weakRecipientHeaderFields {
			if _, exists := headerNames[weak]; exists {
				t.Fatalf("default search included weak header %q", weak)
			}
		}
		for _, strong := range strongRecipientHeaderFields {
			if _, exists := headerNames[strong]; !exists {
				t.Fatalf("default search omitted strong header %q", strong)
			}
		}
	}
}

func collectSearchHeaderNames(criteria *imap.SearchCriteria, names map[string]struct{}) {
	for name := range criteria.Header {
		names[name] = struct{}{}
	}
	for _, pair := range criteria.Or {
		collectSearchHeaderNames(pair[0], names)
		collectSearchHeaderNames(pair[1], names)
	}
}

func TestFetcherFallsBackToRecentUIDScanWhenRecipientSearchFails(t *testing.T) {
	session := &fakeIMAPSession{
		searchResults:  [][]uint32{nil},
		searchErrors:   []error{errors.New("Unexpected exception")},
		messages:       20,
		uidNext:        13,
		uidOnlyResults: []uint32{10, 11, 12, 13},
		headerByUID: map[uint32][]byte{
			10: []byte("X-Original-To: alias@example.com\r\n\r\n"),
			11: []byte("Delivered-To: alias@example.com\r\n\r\n"),
			12: []byte("X-Original-To: other@example.com\r\n\r\n"),
			13: []byte("X-Original-To: other@example.com\r\n\r\n"),
		},
		bodyByUID: map[uint32][]byte{
			11: []byte("Subject: fallback winner\r\n\r\nbody"),
		},
		internalDates: map[uint32]time.Time{
			11: time.Date(2026, 8, 7, 7, 0, 0, 0, time.UTC),
		},
	}
	fetcher := NewFetcher()
	fetcher.MaxCandidates = 4
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID: 1, Email: "owner@icloud.com", Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if message := got[5]; message.SnapshotState != domain.SnapshotFound || message.UID != 11 || message.Subject != "fallback winner" {
		t.Fatalf("fallback snapshot = %#v, want UID 11 Found", message)
	}
	if len(session.searchCriteria) != 1 || len(session.searchCriteria[0].Or) == 0 {
		t.Fatalf("SEARCH calls = %#v, want one combined recipient search", session.searchCriteria)
	}
	if len(session.fetchItems) != 3 || len(session.fetchItems[0]) != 1 || session.fetchItems[0][0] != imap.FetchUid {
		t.Fatalf("fallback FETCH calls = %#v, want UID discovery, headers, body", session.fetchItems)
	}
	if got := session.fetchSeqSets[0]; got != "17:20" {
		t.Fatalf("fallback sequence range = %q, want 17:20", got)
	}
}

func TestFetcherFallbackUIDScanRemainsUnknownWhenWindowIsIncomplete(t *testing.T) {
	session := &fakeIMAPSession{
		searchResults:  [][]uint32{nil},
		searchErrors:   []error{errors.New("Unexpected exception")},
		messages:       10,
		uidNext:        100,
		uidOnlyResults: []uint32{98, 99},
		headerByUID: map[uint32][]byte{
			98: []byte("X-Original-To: other@example.com\r\n\r\n"),
			99: []byte("Delivered-To: other@example.com\r\n\r\n"),
		},
	}
	fetcher := NewFetcher()
	fetcher.MaxCandidates = 2
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID: 1, Email: "owner@icloud.com", Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if message := got[5]; message.SnapshotState != domain.SnapshotUnknown || message.UID != 0 {
		t.Fatalf("incomplete fallback snapshot = %#v, want Unknown", message)
	}
}

func TestFetcherFallbackUIDScanCanConfirmEmptyMailboxBinding(t *testing.T) {
	session := &fakeIMAPSession{
		searchResults:  [][]uint32{nil},
		searchErrors:   []error{errors.New("Unexpected exception")},
		messages:       2,
		uidNext:        100,
		uidOnlyResults: []uint32{98, 99},
		headerByUID: map[uint32][]byte{
			98: []byte("X-Original-To: other@example.com\r\n\r\n"),
			99: []byte("Delivered-To: other@example.com\r\n\r\n"),
		},
	}
	fetcher := NewFetcher()
	fetcher.MaxCandidates = 2
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID: 1, Email: "owner@icloud.com", Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if message := got[5]; message.SnapshotState != domain.SnapshotEmpty {
		t.Fatalf("complete fallback snapshot = %#v, want Empty", message)
	}
}

func TestFetcherFallbackUsesSequenceWindowForSparseUIDs(t *testing.T) {
	session := &fakeIMAPSession{
		searchResults:  [][]uint32{nil},
		searchErrors:   []error{errors.New("Unexpected exception")},
		messages:       3,
		uidNext:        900000,
		uidOnlyResults: []uint32{3, 11, 700000},
		headerByUID: map[uint32][]byte{
			700000: []byte("X-Original-To: other@example.com\r\n\r\n"),
			11:     []byte("X-Original-To: alias@example.com\r\n\r\n"),
			3:      []byte("X-Original-To: other@example.com\r\n\r\n"),
		},
		bodyByUID: map[uint32][]byte{
			11: []byte("Subject: sparse UID winner\r\n\r\nbody"),
		},
	}
	fetcher := NewFetcher()
	fetcher.MaxCandidates = 3
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID: 1, Email: "owner@icloud.com", Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if message := got[5]; message.SnapshotState != domain.SnapshotFound || message.UID != 11 {
		t.Fatalf("sparse UID fallback = %#v, want UID 11 Found", message)
	}
	if got := session.fetchSeqSets[0]; got != "1:3" {
		t.Fatalf("fallback sequence range = %q, want 1:3", got)
	}
}

func TestFetcherFallbackRejectsMalformedUIDDiscovery(t *testing.T) {
	tests := []struct {
		name    string
		uids    []uint32
		seqNums []uint32
	}{
		{name: "duplicate UID", uids: []uint32{10, 10}},
		{name: "zero UID", uids: []uint32{0, 11}},
		{name: "out of range sequence", uids: []uint32{10, 11}, seqNums: []uint32{1, 99}},
		{name: "missing newer sequence", uids: []uint32{10}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &fakeIMAPSession{
				searchResults:  [][]uint32{nil},
				searchErrors:   []error{errors.New("Unexpected exception")},
				messages:       2,
				uidNext:        100,
				uidOnlyResults: test.uids,
				uidOnlySeqNums: test.seqNums,
				headerByUID:    map[uint32][]byte{10: []byte("X-Original-To: alias@example.com\r\n\r\n"), 11: []byte("X-Original-To: alias@example.com\r\n\r\n")},
			}
			fetcher := NewFetcher()
			fetcher.MaxCandidates = 2
			fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
				return session, nil
			}

			got, err := fetcher.FetchLatest(context.Background(), domain.Account{
				ID: 1, Email: "owner@icloud.com", Enabled: true,
			}, "password", []domain.Alias{
				{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got[5].SnapshotState != domain.SnapshotUnknown {
				t.Fatalf("malformed fallback snapshot = %#v, want Unknown", got[5])
			}
			if len(session.fetchItems) != 1 {
				t.Fatalf("malformed fallback FETCH calls = %d, want discovery only", len(session.fetchItems))
			}
		})
	}
}

func TestFetcherFallbackConfirmsEmptyZeroMessageMailbox(t *testing.T) {
	session := &fakeIMAPSession{
		searchResults: [][]uint32{nil},
		searchErrors:  []error{errors.New("Unexpected exception")},
		messages:      0,
		uidNext:       1,
	}
	fetcher := NewFetcher()
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID: 1, Email: "owner@icloud.com", Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[5].SnapshotState != domain.SnapshotEmpty || len(session.fetchItems) != 0 {
		t.Fatalf("zero-message fallback = %#v, FETCH calls=%d; want Empty without FETCH", got[5], len(session.fetchItems))
	}
}

func TestFetcherReturnsFallbackUIDScanError(t *testing.T) {
	searchErr := errors.New("Unexpected exception")
	fallbackErr := errors.New("sequence range failed")
	session := &fakeIMAPSession{
		searchResults: [][]uint32{nil},
		searchErrors:  []error{searchErr},
		messages:      1,
		uidNext:       2,
		fetchErrors:   []error{fallbackErr},
	}
	fetcher := NewFetcher()
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	_, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID: 1, Email: "owner@icloud.com", Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if err == nil || !strings.Contains(err.Error(), "Unexpected exception") || !strings.Contains(err.Error(), "sequence range failed") || !errors.Is(err, searchErr) || !errors.Is(err, fallbackErr) {
		t.Fatalf("fallback error = %v, want combined SEARCH and UID scan errors", err)
	}
}

func TestFetcherRejectsAliasFromAnotherAccount(t *testing.T) {
	fetcher := NewFetcher()
	_, err := fetcher.FetchLatest(context.Background(), domain.Account{ID: 1, Enabled: true}, "password", []domain.Alias{
		{ID: 2, AccountID: 99, Address: "alias@example.com", Enabled: true},
	})
	if !errors.Is(err, ErrAliasAccountMismatch) {
		t.Fatalf("error = %v, want ErrAliasAccountMismatch", err)
	}
}

func TestFetcherHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetcher := NewFetcher()
	dialed := false
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		dialed = true
		return nil, errors.New("unexpected dial")
	}
	_, err := fetcher.FetchLatest(ctx, domain.Account{ID: 1, Enabled: true}, "password", nil)
	if !errors.Is(err, context.Canceled) || dialed {
		t.Fatalf("error = %v, dialed = %v", err, dialed)
	}
}

func TestFetcherValidatesIMAPWithNoAliases(t *testing.T) {
	session := &fakeIMAPSession{}
	fetcher := NewFetcher()
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID:      1,
		Email:   "owner@icloud.com",
		Enabled: true,
	}, "app-password", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("messages = %#v", got)
	}
	if session.username != "owner@icloud.com" || session.selected != "INBOX" || !session.readOnly {
		t.Fatalf("IMAP validation = username %q mailbox %q read-only=%v", session.username, session.selected, session.readOnly)
	}
	if session.loggedOut || !session.terminated {
		t.Fatalf("cleanup = logout=%v terminate=%v", session.loggedOut, session.terminated)
	}
}

func TestFetcherRejectsZeroUIDValidity(t *testing.T) {
	session := &fakeIMAPSession{zeroUIDValidity: true}
	fetcher := NewFetcher()
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	_, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID:      1,
		Email:   "owner@icloud.com",
		Enabled: true,
	}, "app-password", nil)
	if err == nil || !strings.Contains(err.Error(), "UIDVALIDITY is zero") {
		t.Fatalf("error = %v, want zero UIDVALIDITY rejection", err)
	}
	if !session.terminated {
		t.Fatal("session was not terminated after invalid UIDVALIDITY")
	}
}

func TestFetcherCancellationCannotBeBlockedByLogout(t *testing.T) {
	base := &fakeIMAPSession{}
	session := &blockingLogoutSession{
		fakeIMAPSession:  base,
		logoutStarted:    make(chan struct{}),
		logoutRelease:    make(chan struct{}),
		terminatedSignal: make(chan struct{}),
	}
	fetcher := NewFetcher()
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := fetcher.FetchLatest(ctx, domain.Account{
			ID:      1,
			Email:   "owner@icloud.com",
			Enabled: true,
		}, "app-password", nil)
		done <- err
	}()

	select {
	case err := <-done:
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	case <-session.logoutStarted:
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			close(session.logoutRelease)
			<-done
			t.Fatal("cancellation did not release a blocking Logout")
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("fetch did not finish or enter cleanup")
	}
	if !session.terminated {
		t.Fatal("session was not terminated during cleanup")
	}
}

func TestFetcherSnapshotStates(t *testing.T) {
	tests := []struct {
		name          string
		searchResults []uint32
		header        []byte
		body          []byte
		want          domain.SnapshotState
	}{
		{name: "authoritative empty", searchResults: nil, want: domain.SnapshotEmpty},
		{
			name:          "candidate fails recipient validation",
			searchResults: []uint32{10},
			header:        []byte("To: alias@example.com\r\n\r\n"),
			want:          domain.SnapshotUnknown,
		},
		{
			name:          "winner body missing",
			searchResults: []uint32{10},
			header:        []byte("X-Original-To: alias@example.com\r\n\r\n"),
			want:          domain.SnapshotUnknown,
		},
		{
			name:          "winner MIME malformed",
			searchResults: []uint32{10},
			header:        []byte("X-Original-To: alias@example.com\r\n\r\n"),
			body:          []byte("malformed header without colon\r\n\r\nbody"),
			want:          domain.SnapshotUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &fakeIMAPSession{
				searchResults: [][]uint32{test.searchResults},
				headerByUID:   map[uint32][]byte{},
				bodyByUID:     map[uint32][]byte{},
			}
			if test.header != nil {
				session.headerByUID[10] = test.header
			}
			if test.body != nil {
				session.bodyByUID[10] = test.body
			}
			fetcher := NewFetcher()
			fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
				return session, nil
			}

			got, err := fetcher.FetchLatest(context.Background(), domain.Account{
				ID:      1,
				Email:   "owner@icloud.com",
				Enabled: true,
			}, "password", []domain.Alias{
				{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			message, exists := got[5]
			if !exists {
				t.Fatalf("alias state missing from result: %#v", got)
			}
			if message.SnapshotState != test.want || message.UIDValidity != 4242 {
				t.Fatalf("snapshot = state %q UIDVALIDITY %d, want %q / 4242", message.SnapshotState, message.UIDValidity, test.want)
			}
			if test.want != domain.SnapshotFound && message.UID != 0 {
				t.Fatalf("non-found snapshot UID = %d", message.UID)
			}
		})
	}
}

func TestHigherUnresolvedCandidateBlocksLowerWinner(t *testing.T) {
	session := &fakeIMAPSession{
		searchResults: [][]uint32{{10, 11}},
		headerByUID: map[uint32][]byte{
			10: []byte("X-Original-To: alias@example.com\r\n\r\n"),
			// UID 11 is deliberately missing from the FETCH response.
		},
		bodyByUID: map[uint32][]byte{
			10: []byte("Subject: older\r\n\r\nbody"),
		},
	}
	fetcher := NewFetcher()
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID:      1,
		Email:   "owner@icloud.com",
		Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[5].SnapshotState != domain.SnapshotUnknown || got[5].UID != 0 {
		t.Fatalf("snapshot = %#v, want Unknown without lower-UID fallback", got[5])
	}
	if len(session.fetchItems) != 1 {
		t.Fatalf("FETCH calls = %d, lower winner body should not be fetched", len(session.fetchItems))
	}
}

func TestHigherAmbiguousCandidateBlocksLowerWinner(t *testing.T) {
	session := &fakeIMAPSession{
		searchResults: [][]uint32{{10, 11}},
		headerByUID: map[uint32][]byte{
			10: []byte("X-Original-To: alias@example.com\r\n\r\n"),
			11: []byte("X-Original-To: alias@example.com\r\nX-Original-To: other@example.com\r\n\r\n"),
		},
		bodyByUID: map[uint32][]byte{
			10: []byte("Subject: older\r\n\r\nbody"),
		},
	}
	fetcher := NewFetcher()
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID:      1,
		Email:   "owner@icloud.com",
		Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[5].SnapshotState != domain.SnapshotUnknown || got[5].UID != 0 {
		t.Fatalf("snapshot = %#v, want Unknown without lower-UID fallback", got[5])
	}
}

func TestCompleteFalsePositiveCandidatesAreAuthoritativelyEmpty(t *testing.T) {
	session := &fakeIMAPSession{
		searchResults: [][]uint32{{10}},
		headerByUID: map[uint32][]byte{
			10: []byte("X-Original-To: other@example.com\r\n\r\n"),
		},
	}
	fetcher := NewFetcher()
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID:      1,
		Email:   "owner@icloud.com",
		Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[5].SnapshotState != domain.SnapshotEmpty {
		t.Fatalf("snapshot = %#v, want authoritative Empty", got[5])
	}
}

func TestPerAliasCandidateTruncationStaysUnknown(t *testing.T) {
	session := &fakeIMAPSession{
		searchResults: [][]uint32{{10, 11}},
		headerByUID: map[uint32][]byte{
			11: []byte("X-Original-To: other@example.com\r\n\r\n"),
		},
	}
	fetcher := NewFetcher()
	fetcher.MaxCandidatesPerAlias = 1
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID:      1,
		Email:   "owner@icloud.com",
		Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[5].SnapshotState != domain.SnapshotUnknown {
		t.Fatalf("snapshot = %#v, want Unknown after per-alias truncation", got[5])
	}
}

func TestGlobalCandidateTruncationStaysUnknown(t *testing.T) {
	session := &fakeIMAPSession{
		searchResults: [][]uint32{{10, 11}, {20, 21}},
		headerByUID: map[uint32][]byte{
			11: []byte("X-Original-To: other@example.com\r\n\r\n"),
			21: []byte("X-Original-To: other@example.com\r\n\r\n"),
		},
	}
	fetcher := NewFetcher()
	fetcher.MaxCandidates = 2
	fetcher.MaxCandidatesPerAlias = 2
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID:      1,
		Email:   "owner@icloud.com",
		Enabled: true,
	}, "password", []domain.Alias{
		{ID: 1, AccountID: 1, Address: "a@example.com", Enabled: true},
		{ID: 2, AccountID: 1, Address: "b@example.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[1].SnapshotState != domain.SnapshotUnknown || got[2].SnapshotState != domain.SnapshotUnknown {
		t.Fatalf("snapshots = %#v / %#v, want Unknown after global truncation", got[1], got[2])
	}
}

func TestInvalidZeroUIDKeepsFalsePositiveResultUnknown(t *testing.T) {
	session := &fakeIMAPSession{
		searchResults: [][]uint32{{0, 10}},
		headerByUID: map[uint32][]byte{
			10: []byte("X-Original-To: other@example.com\r\n\r\n"),
		},
	}
	fetcher := NewFetcher()
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID:      1,
		Email:   "owner@icloud.com",
		Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[5].SnapshotState != domain.SnapshotUnknown {
		t.Fatalf("snapshot = %#v, want Unknown after invalid UID 0", got[5])
	}
}

func TestFetcherReturnsIMAPFetchCommandError(t *testing.T) {
	commandErr := errors.New("fetch command failed")
	session := &fakeIMAPSession{
		searchResults: [][]uint32{{10}},
		headerByUID: map[uint32][]byte{
			10: []byte("X-Original-To: alias@example.com\r\n\r\n"),
		},
		bodyByUID: map[uint32][]byte{
			10: []byte("Subject: latest\r\n\r\nbody"),
		},
		fetchErrors: []error{nil, commandErr},
	}
	fetcher := NewFetcher()
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	_, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID:      1,
		Email:   "owner@icloud.com",
		Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if !errors.Is(err, commandErr) {
		t.Fatalf("error = %v, want command error", err)
	}
}

func TestFetchMessagesBatchesUIDs(t *testing.T) {
	session := &fakeIMAPSession{bodyByUID: make(map[uint32][]byte)}
	uids := make([]uint32, 0, messageFetchBatch+1)
	for uid := uint32(1); uid <= uint32(messageFetchBatch+1); uid++ {
		uids = append(uids, uid)
		session.bodyByUID[uid] = []byte(fmt.Sprintf("Subject: message %d\r\n\r\nbody", uid))
	}

	aliasesByUID := make(map[uint32][]int64, len(uids))
	for _, uid := range uids {
		aliasesByUID[uid] = []int64{int64(uid)}
	}
	messages, err := fetchMessages(
		session,
		uids,
		1024,
		defaultMIMELimits(128, 1024),
		aliasesByUID,
		1<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != len(uids) || len(session.fetchItems) != 2 {
		t.Fatalf("messages = %d, FETCH calls = %d", len(messages), len(session.fetchItems))
	}
}

func TestTruncatedMessageHeaderStillReturnsFoundSnapshot(t *testing.T) {
	raw := []byte("Subject: " + strings.Repeat("x", 1024) + "\r\n\r\nbody")
	session := &fakeIMAPSession{
		searchResults: [][]uint32{{10}},
		headerByUID: map[uint32][]byte{
			10: []byte("X-Original-To: alias@example.com\r\n\r\n"),
		},
		bodyByUID: map[uint32][]byte{10: raw},
	}
	fetcher := NewFetcher()
	fetcher.MaxMessageBytes = 64
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}

	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID:      1,
		Email:   "owner@icloud.com",
		Enabled: true,
	}, "password", []domain.Alias{
		{ID: 5, AccountID: 1, Address: "alias@example.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[5].SnapshotState != domain.SnapshotFound || got[5].UID != 10 || !got[5].BodyTruncated {
		t.Fatalf("snapshot = %#v, want truncated Found", got[5])
	}
}

func TestFetchResultBudgetPreservesAllWinners(t *testing.T) {
	const aliasCount = 4
	const dynamicBytesPerAlias = 100
	session := &fakeIMAPSession{
		searchResults: make([][]uint32, 0, aliasCount),
		headerByUID:   make(map[uint32][]byte, aliasCount),
		bodyByUID:     make(map[uint32][]byte, aliasCount),
	}
	aliases := make([]domain.Alias, 0, aliasCount)
	for i := 1; i <= aliasCount; i++ {
		uid := uint32(i)
		address := fmt.Sprintf("alias%d@example.com", i)
		session.searchResults = append(session.searchResults, []uint32{uid})
		session.headerByUID[uid] = []byte("X-Original-To: " + address + "\r\n\r\n")
		session.bodyByUID[uid] = []byte(fmt.Sprintf("Message-ID: <%d@example.com>\r\nSubject: message %d\r\n\r\n", i, i) + strings.Repeat("x", 1024))
		aliases = append(aliases, domain.Alias{ID: int64(i), AccountID: 1, Address: address, Enabled: true})
	}

	resultBudget := aliasCount*parsedMessageBaseBytes + aliasCount*dynamicBytesPerAlias
	fetcher := NewFetcher()
	fetcher.MaxBodyBytes = 1024
	fetcher.MaxFetchResultBytes = resultBudget
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		return session, nil
	}
	got, err := fetcher.FetchLatest(context.Background(), domain.Account{
		ID:      1,
		Email:   "owner@icloud.com",
		Enabled: true,
	}, "password", aliases)
	if err != nil {
		t.Fatal(err)
	}

	var retained int64
	for _, alias := range aliases {
		message := got[alias.ID]
		if message.SnapshotState != domain.SnapshotFound || !message.BodyTruncated {
			t.Fatalf("alias %d snapshot = %#v, want truncated Found", alias.ID, message)
		}
		retained += parsedMessageResultBytes(parsedMessage{
			messageID:     message.MessageID,
			from:          message.From,
			to:            message.To,
			cc:            message.CC,
			subject:       message.Subject,
			textBody:      message.TextBody,
			htmlBody:      message.HTMLBody,
			attachments:   message.Attachments,
			bodyTruncated: message.BodyTruncated,
		})
	}
	if retained > int64(resultBudget) {
		t.Fatalf("retained result bytes = %d, budget = %d", retained, resultBudget)
	}
	if settings := NewFetcher().settings(); settings.maxFetchResultBytes != 64<<20 {
		t.Fatalf("default result budget = %d, want 64 MiB", settings.maxFetchResultBytes)
	}
}

func TestAccountEndpointDefaultsToICloudTLSPort(t *testing.T) {
	host, address, username, err := accountEndpoint(domain.Account{Email: "owner@icloud.com"})
	if err != nil {
		t.Fatal(err)
	}
	if host != "imap.mail.me.com" || address != "imap.mail.me.com:993" || username != "owner@icloud.com" {
		t.Fatalf("endpoint = %q %q %q", host, address, username)
	}
}

func TestAccountEndpointRejectsOtherServer(t *testing.T) {
	_, _, _, err := accountEndpoint(domain.Account{
		Email:    "owner@icloud.com",
		IMAPHost: "internal.example.test",
		IMAPPort: 143,
	})
	if !errors.Is(err, ErrInvalidIMAPConfig) {
		t.Fatalf("error = %v, want ErrInvalidIMAPConfig", err)
	}
}

func TestFairCandidateUIDs(t *testing.T) {
	got := fairCandidateUIDs([][]uint32{{10, 9, 8}, {20, 19}, {10, 7}}, 5)
	want := []uint32{20, 19, 10, 9, 7}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("fairCandidateUIDs() = %v, want %v", got, want)
	}
}

func TestNewestUIDsUsesBoundedTopKSemantics(t *testing.T) {
	tests := []struct {
		name          string
		uids          []uint32
		limit         int
		want          []uint32
		wantTruncated bool
	}{
		{name: "deduplicates without truncation", uids: []uint32{0, 3, 3, 2}, limit: 3, want: []uint32{3, 2}, wantTruncated: true},
		{name: "keeps newest values", uids: []uint32{1, 5, 2, 4, 3, 5}, limit: 3, want: []uint32{5, 4, 3}, wantTruncated: true},
		{name: "zero limit", uids: []uint32{1}, limit: 0, want: nil, wantTruncated: true},
		{name: "empty", uids: nil, limit: 3, want: nil, wantTruncated: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, truncated := newestUIDs(test.uids, test.limit)
			if fmt.Sprint(got) != fmt.Sprint(test.want) || truncated != test.wantTruncated {
				t.Fatalf("newestUIDs(%v, %d) = %v/%v, want %v/%v", test.uids, test.limit, got, truncated, test.want, test.wantTruncated)
			}
		})
	}
}
