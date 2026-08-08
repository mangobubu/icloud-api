package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap"

	"icloud-api/internal/domain"
)

type fetchCall struct {
	seqSet string
	items  []imap.FetchItem
}

type storeCall struct {
	seqSet string
	item   imap.StoreItem
	value  interface{}
}

type fakeIMAPSession struct {
	mu sync.Mutex

	uidValidity uint32
	uidNext     uint32
	messages    uint32
	mailboxUIDs []uint32

	headerByUID        map[uint32][]byte
	bodyByUID          map[uint32][]byte
	internalDates      map[uint32]time.Time
	seenUIDs           map[uint32]struct{}
	fetchErrors        []error
	duplicateUID       uint32
	expungeBeforeFetch map[int][]uint32

	username           string
	password           string
	selected           string
	readOnly           bool
	loginCalls         int
	selectCalls        int
	searchCalls        int
	searchUIDSets      []string
	searchWithoutFlags [][]string
	fetchCalls         []fetchCall
	storeCalls         []storeCall
	activeCommands     int
	maxActive          int
	terminated         bool

	loginErr  error
	selectErr error
	storeErr  error

	storeStarted        chan struct{}
	storeUntilTerminate chan struct{}
	terminateOnce       sync.Once
}

func (f *fakeIMAPSession) Login(username, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.username = username
	f.password = password
	f.loginCalls++
	return f.loginErr
}

func (f *fakeIMAPSession) Select(name string, readOnly bool) (*imap.MailboxStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selected = name
	f.readOnly = readOnly
	f.selectCalls++
	if f.selectErr != nil {
		return nil, f.selectErr
	}
	messages := f.messages
	if messages == 0 {
		messages = uint32(len(f.mailboxUIDListLocked()))
	}
	return &imap.MailboxStatus{
		Name:        name,
		UidValidity: f.uidValidity,
		UidNext:     f.uidNext,
		Messages:    messages,
	}, nil
}

func (f *fakeIMAPSession) UidSearch(criteria *imap.SearchCriteria) ([]uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchCalls++
	if criteria == nil || criteria.Uid == nil {
		return nil, errors.New("expected account-level UID range search")
	}
	f.searchUIDSets = append(f.searchUIDSets, criteria.Uid.String())
	f.searchWithoutFlags = append(f.searchWithoutFlags, append([]string(nil), criteria.WithoutFlags...))
	uids := make([]uint32, 0)
	for _, uid := range f.mailboxUIDListLocked() {
		if !criteria.Uid.Contains(uid) || !f.hasAllFlagsLocked(uid, criteria.WithFlags) || f.hasAnyFlagLocked(uid, criteria.WithoutFlags) {
			continue
		}
		uids = append(uids, uid)
	}
	return uids, nil
}

func (f *fakeIMAPSession) hasAnyFlagLocked(uid uint32, flags []string) bool {
	if len(flags) == 0 {
		return false
	}
	for _, want := range flags {
		if testHasIMAPFlag(f.flagsForUIDLocked(uid), want) {
			return true
		}
	}
	return false
}

func (f *fakeIMAPSession) hasAllFlagsLocked(uid uint32, flags []string) bool {
	for _, want := range flags {
		if !testHasIMAPFlag(f.flagsForUIDLocked(uid), want) {
			return false
		}
	}
	return true
}

func testHasIMAPFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if imap.CanonicalFlag(flag) == imap.CanonicalFlag(want) {
			return true
		}
	}
	return false
}

func (f *fakeIMAPSession) flagsForUIDLocked(uid uint32) []string {
	if _, seen := f.seenUIDs[uid]; seen {
		return []string{imap.SeenFlag}
	}
	return []string{}
}

func (f *fakeIMAPSession) flagsForUID(uid uint32) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flagsForUIDLocked(uid)
}

func (f *fakeIMAPSession) Fetch(seqset *imap.SeqSet, items []imap.FetchItem, ch chan *imap.Message) error {
	commandErr := f.beginFetch(seqset, items)
	defer func() {
		close(ch)
		f.endFetch()
	}()
	if commandErr != nil {
		return commandErr
	}
	if !isUIDFlagsFetch(items) && (len(items) != 1 || items[0] != imap.FetchUid) {
		return errors.New("unexpected sequence fetch items")
	}
	for index, uid := range f.mailboxUIDList() {
		sequence := uint32(index + 1)
		if !seqset.Contains(sequence) {
			continue
		}
		message := &imap.Message{SeqNum: sequence, Uid: uid}
		if isUIDFlagsFetch(items) {
			message.Flags = f.flagsForUID(uid)
		}
		ch <- message
	}
	return nil
}

// midResponseExpungeSession models an EXPUNGE arriving after the boundary
// sentinel has already been emitted by a sequence FETCH. Real IMAP servers can
// interleave unsolicited EXPUNGE responses with a selected-mailbox command.
type midResponseExpungeSession struct {
	*fakeIMAPSession
	targetCall int
	expungeUID uint32
	responses  []*imap.Message
}

func (s *midResponseExpungeSession) Fetch(seqset *imap.SeqSet, items []imap.FetchItem, ch chan *imap.Message) error {
	s.mu.Lock()
	callIndex := len(s.fetchCalls)
	s.mu.Unlock()
	if callIndex != s.targetCall {
		return s.fakeIMAPSession.Fetch(seqset, items, ch)
	}

	commandErr := s.beginFetch(seqset, items)
	defer func() {
		close(ch)
		s.endFetch()
	}()
	if commandErr != nil {
		return commandErr
	}
	if !isUIDFlagsFetch(items) {
		return errors.New("mid-response test requires UID/FLAGS FETCH")
	}

	responses := s.responses
	if len(responses) == 0 {
		responses = []*imap.Message{
			{SeqNum: 2, Uid: 2},
			{SeqNum: 3, Uid: 4},
			{SeqNum: 4, Uid: 5},
		}
	}
	// The first message is the old sentinel. Expunging it shifts UID 3 into
	// the sentinel sequence slot; the remaining scripted responses deliberately
	// retain the old sequence numbers to reproduce a crossed response.
	for index, message := range responses {
		ch <- message
		if index != 0 {
			continue
		}
		s.mu.Lock()
		s.expungeUIDsLocked([]uint32{s.expungeUID})
		s.mu.Unlock()
	}
	return nil
}

func (f *fakeIMAPSession) UidFetch(seqset *imap.SeqSet, items []imap.FetchItem, ch chan *imap.Message) error {
	return f.fetch(seqset, items, ch)
}

func (f *fakeIMAPSession) UidStore(seqset *imap.SeqSet, item imap.StoreItem, value interface{}, ch chan *imap.Message) error {
	f.mu.Lock()
	if f.selected == "" {
		f.mu.Unlock()
		return errors.New("UID STORE without selected mailbox")
	}
	if f.readOnly {
		f.mu.Unlock()
		return errors.New("UID STORE with read-only mailbox")
	}
	f.storeCalls = append(f.storeCalls, storeCall{
		seqSet: seqset.String(),
		item:   item,
		value:  value,
	})
	storeErr := f.storeErr
	if op, _, parseErr := imap.ParseFlagsOp(item); parseErr == nil && op == imap.AddFlags {
		if values, ok := value.([]interface{}); ok {
			for _, rawFlag := range values {
				if flag, ok := rawFlag.(string); ok && imap.CanonicalFlag(flag) == imap.SeenFlag {
					if f.seenUIDs == nil {
						f.seenUIDs = make(map[uint32]struct{})
					}
					for _, uid := range f.mailboxUIDListLocked() {
						if seqset.Contains(uid) {
							f.seenUIDs[uid] = struct{}{}
						}
					}
				}
			}
		}
	}
	started := f.storeStarted
	untilTerminate := f.storeUntilTerminate
	f.mu.Unlock()
	if started != nil {
		close(started)
	}
	if untilTerminate != nil {
		<-untilTerminate
	}
	if ch != nil {
		close(ch)
	}
	return storeErr
}

func (f *fakeIMAPSession) fetch(seqset *imap.SeqSet, items []imap.FetchItem, ch chan *imap.Message) error {
	commandErr := f.beginFetch(seqset, items)

	defer func() {
		close(ch)
		f.endFetch()
	}()
	if commandErr != nil {
		return commandErr
	}
	if isUIDFlagsFetch(items) {
		for _, uid := range f.mailboxUIDList() {
			if seqset.Contains(uid) {
				ch <- &imap.Message{Uid: uid, Flags: f.flagsForUID(uid)}
			}
		}
		return nil
	}
	if len(items) == 1 && items[0] == imap.FetchUid {
		for _, uid := range f.mailboxUIDList() {
			if seqset.Contains(uid) {
				ch <- &imap.Message{Uid: uid}
			}
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
	uids := make([]uint32, 0, len(source))
	for uid := range source {
		if seqset.Contains(uid) {
			uids = append(uids, uid)
		}
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	for _, uid := range uids {
		f.sendMessage(ch, requested, uid, source[uid])
		if headerFetch && uid == f.duplicateUID {
			f.sendMessage(ch, requested, uid, source[uid])
		}
	}
	return nil
}

func (f *fakeIMAPSession) beginFetch(seqset *imap.SeqSet, items []imap.FetchItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	callIndex := len(f.fetchCalls)
	if uids := f.expungeBeforeFetch[callIndex]; len(uids) > 0 {
		f.expungeUIDsLocked(uids)
	}
	f.fetchCalls = append(f.fetchCalls, fetchCall{
		seqSet: seqset.String(),
		items:  append([]imap.FetchItem(nil), items...),
	})
	f.activeCommands++
	if f.activeCommands > f.maxActive {
		f.maxActive = f.activeCommands
	}
	var commandErr error
	if callIndex < len(f.fetchErrors) {
		commandErr = f.fetchErrors[callIndex]
	}
	return commandErr
}

func (f *fakeIMAPSession) expungeUIDsLocked(uids []uint32) {
	expunged := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		expunged[uid] = struct{}{}
	}
	mailboxUIDs := f.mailboxUIDListLocked()
	kept := mailboxUIDs[:0]
	for _, uid := range mailboxUIDs {
		if _, remove := expunged[uid]; !remove {
			kept = append(kept, uid)
		}
	}
	f.mailboxUIDs = make([]uint32, len(kept))
	copy(f.mailboxUIDs, kept)
	for uid := range expunged {
		delete(f.headerByUID, uid)
		delete(f.bodyByUID, uid)
		delete(f.internalDates, uid)
		delete(f.seenUIDs, uid)
	}
}

func (f *fakeIMAPSession) endFetch() {
	f.mu.Lock()
	f.activeCommands--
	f.mu.Unlock()
}

func (f *fakeIMAPSession) mailboxUIDListLocked() []uint32 {
	if f.mailboxUIDs != nil {
		return append([]uint32(nil), f.mailboxUIDs...)
	}
	uids := make([]uint32, 0, len(f.headerByUID))
	for uid := range f.headerByUID {
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	return uids
}

func (f *fakeIMAPSession) mailboxUIDList() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mailboxUIDListLocked()
}

func (f *fakeIMAPSession) sendMessage(ch chan *imap.Message, requested *imap.BodySectionName, uid uint32, raw []byte) {
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

func (f *fakeIMAPSession) Logout() error {
	return nil
}

func (f *fakeIMAPSession) Terminate() error {
	f.mu.Lock()
	f.terminated = true
	untilTerminate := f.storeUntilTerminate
	f.mu.Unlock()
	if untilTerminate != nil {
		f.terminateOnce.Do(func() { close(untilTerminate) })
	}
	return nil
}

func (f *fakeIMAPSession) calls() []fetchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]fetchCall, len(f.fetchCalls))
	copy(result, f.fetchCalls)
	return result
}

func (f *fakeIMAPSession) stores() []storeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]storeCall, len(f.storeCalls))
	copy(result, f.storeCalls)
	return result
}

func (f *fakeIMAPSession) counters() (login, selectCount, search, maxActive int, terminated bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCalls, f.selectCalls, f.searchCalls, f.maxActive, f.terminated
}

func (f *fakeIMAPSession) searches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.searchUIDSets...)
}

func (f *fakeIMAPSession) searchFlags() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([][]string, len(f.searchWithoutFlags))
	for index, flags := range f.searchWithoutFlags {
		result[index] = append([]string(nil), flags...)
	}
	return result
}

func testAccount() domain.Account {
	return domain.Account{
		ID:           7,
		Email:        "owner@icloud.com",
		IMAPUsername: "imap-user",
		Enabled:      true,
	}
}

func testAlias(id int64, address string) domain.Alias {
	return domain.Alias{ID: id, AccountID: 7, Address: address, Enabled: true}
}

func testFetcher(session imapSession, now time.Time) *Fetcher {
	fetcher := NewFetcher()
	fetcher.now = func() time.Time { return now }
	fetcher.dial = func(_ context.Context, address, serverName string, _ time.Duration) (imapSession, error) {
		if address != "imap.mail.me.com:993" || serverName != "imap.mail.me.com" {
			return nil, fmt.Errorf("unexpected endpoint %q / %q", address, serverName)
		}
		return session, nil
	}
	return fetcher
}

func TestNewFetcherUsesSeparateCandidateDefaults(t *testing.T) {
	fetcher := NewFetcher()
	if fetcher.MaxCandidates != 1024 || fetcher.MaxIncrementalCandidates != 256 {
		t.Fatalf(
			"candidate defaults recent=%d incremental=%d, want 1024/256",
			fetcher.MaxCandidates,
			fetcher.MaxIncrementalCandidates,
		)
	}
	settings := (&Fetcher{}).settings()
	if settings.maxCandidates != 1024 || settings.maxIncrementalCandidates != 256 {
		t.Fatalf(
			"zero-value settings recent=%d incremental=%d, want 1024/256",
			settings.maxCandidates,
			settings.maxIncrementalCandidates,
		)
	}
	bounded := (&Fetcher{
		MaxCandidates:            int(^uint(0) >> 1),
		MaxIncrementalCandidates: int(^uint(0) >> 1),
	}).settings()
	if bounded.maxCandidates != defaultMaxCandidates ||
		bounded.maxIncrementalCandidates != defaultMaxIncrementalCandidates {
		t.Fatalf(
			"oversized candidate settings=%d/%d, want bounded %d/%d",
			bounded.maxCandidates,
			bounded.maxIncrementalCandidates,
			defaultMaxCandidates,
			defaultMaxIncrementalCandidates,
		)
	}
}

func aliasHeader(address string) []byte {
	return []byte("X-Original-To: " + address + "\r\n\r\n")
}

func ordinaryHeader() []byte {
	return []byte("From: sender@example.com\r\nTo: owner@icloud.com\r\n\r\n")
}

func rawMessage(uid uint32) []byte {
	return []byte(fmt.Sprintf(
		"Message-ID: <%d@example.com>\r\nSubject: message %d\r\n\r\nbody %d",
		uid,
		uid,
		uid,
	))
}

func isHeaderFetch(call fetchCall) bool {
	for _, item := range call.items {
		if strings.Contains(string(item), "HEADER.FIELDS") {
			return true
		}
	}
	return false
}

func isUIDOnlyFetch(call fetchCall) bool {
	return (len(call.items) == 1 && call.items[0] == imap.FetchUid) || isUIDFlagsFetch(call.items)
}

func isUIDFlagsFetch(items []imap.FetchItem) bool {
	if len(items) != 2 {
		return false
	}
	seenUID, seenFlags := false, false
	for _, item := range items {
		switch item {
		case imap.FetchUid:
			seenUID = true
		case imap.FetchFlags:
			seenFlags = true
		}
	}
	return seenUID && seenFlags
}

func uidFlagsFetchCalls(calls []fetchCall) []fetchCall {
	result := make([]fetchCall, 0)
	for _, call := range calls {
		if isUIDFlagsFetch(call.items) {
			result = append(result, call)
		}
	}
	return result
}

func TestFetchIncrementalScans165AliasesWithOneAccountUIDSearch(t *testing.T) {
	const aliasCount = 165
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	session := &fakeIMAPSession{
		uidValidity:   9001,
		uidNext:       aliasCount + 1,
		headerByUID:   make(map[uint32][]byte, aliasCount),
		bodyByUID:     make(map[uint32][]byte, aliasCount),
		internalDates: make(map[uint32]time.Time, aliasCount),
	}
	aliases := make([]domain.Alias, 0, aliasCount)
	for index := 1; index <= aliasCount; index++ {
		uid := uint32(index)
		address := fmt.Sprintf("alias-%03d@icloud.com", index)
		aliases = append(aliases, testAlias(int64(index), address))
		session.headerByUID[uid] = aliasHeader(address)
		session.bodyByUID[uid] = rawMessage(uid)
		session.internalDates[uid] = now.Add(-time.Duration(aliasCount-index) * time.Minute)
	}

	result, err := testFetcher(session, now).FetchIncremental(
		context.Background(),
		testAccount(),
		"app-password",
		aliases,
		&domain.IMAPSyncState{AccountID: 7, UIDValidity: 9001, LastUID: 0},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reset || result.HasMore || result.State.AccountID != 7 || result.State.UIDValidity != 9001 ||
		result.State.LastUID != aliasCount || !result.State.UpdatedAt.Equal(now) {
		t.Fatalf("state = %#v", result)
	}
	if len(result.Messages) != aliasCount {
		t.Fatalf("messages = %d, want %d", len(result.Messages), aliasCount)
	}
	for _, alias := range aliases {
		message := result.Messages[alias.ID]
		if message.SnapshotState != domain.SnapshotFound || message.UID != uint32(alias.ID) {
			t.Fatalf("alias %d message = %#v", alias.ID, message)
		}
	}

	login, selectCount, search, maxActive, terminated := session.counters()
	if login != 1 || selectCount != 1 || search != 1 || maxActive != 1 || !terminated {
		t.Fatalf(
			"commands login=%d select=%d search=%d max-active=%d terminated=%v",
			login,
			selectCount,
			search,
			maxActive,
			terminated,
		)
	}
	if searches := session.searches(); !reflect.DeepEqual(searches, []string{"1:165"}) {
		t.Fatalf("account-level UID searches = %v", searches)
	}
	headerCalls := 0
	for _, call := range session.calls() {
		if isHeaderFetch(call) {
			headerCalls++
		}
	}
	_, headerFetchBatch := boundedHeaderFetchLimits(defaultMaxHeaderBytes)
	wantHeaderCalls := (aliasCount + headerFetchBatch - 1) / headerFetchBatch
	if headerCalls != wantHeaderCalls {
		t.Fatalf("shared header FETCH calls = %d, want %d", headerCalls, wantHeaderCalls)
	}
}

func TestFetchIncrementalNoNewUIDDoesNotFetch(t *testing.T) {
	now := time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)
	session := &fakeIMAPSession{uidValidity: 77, uidNext: 21}
	previous := domain.IMAPSyncState{
		AccountID: 7, UIDValidity: 77, LastUID: 20, UpdatedAt: now.Add(-time.Hour),
	}

	result, err := testFetcher(session, now).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		&previous,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reset || len(result.Messages) != 0 || len(session.calls()) != 0 {
		t.Fatalf("no-new result = %#v, FETCH calls = %#v", result, session.calls())
	}
	if result.State.LastUID != 20 || !result.State.UpdatedAt.Equal(now) {
		t.Fatalf("state = %#v", result.State)
	}
	_, _, search, _, _ := session.counters()
	if search != 0 {
		t.Fatalf("UID SEARCH calls = %d", search)
	}
}

func TestFetchIncrementalValidatesExistingSnapshotsBeforeScanAndPublish(t *testing.T) {
	now := time.Date(2026, 8, 7, 13, 30, 0, 0, time.UTC)
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     31,
		mailboxUIDs: []uint32{20, 30},
	}
	aliases := []domain.Alias{
		testAlias(1, "one@icloud.com"),
		testAlias(2, "two@icloud.com"),
	}
	positions := map[int64]domain.MailboxSnapshotPosition{
		1: {AliasID: 1, UIDValidity: 77, UID: 20},
		2: {AliasID: 2, UIDValidity: 77, UID: 30},
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 30}

	result, err := testFetcher(session, now).FetchIncremental(
		context.Background(), testAccount(), "password", aliases, &previous, positions,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reset || result.HasMore || len(result.Messages) != 0 || result.State.LastUID != 30 {
		t.Fatalf("stable snapshot result = %#v", result)
	}
	calls := session.calls()
	if len(calls) != 2 || calls[0].seqSet != "20,30" || !isUIDOnlyFetch(calls[0]) ||
		calls[1].seqSet != "20,30" || !isUIDOnlyFetch(calls[1]) {
		t.Fatalf("shared snapshot validation calls = %#v", calls)
	}
}

func TestFetchIncrementalReconcilesExpungedLatestSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 7, 13, 45, 0, 0, time.UTC)
	aliases := []domain.Alias{testAlias(1, "one@icloud.com")}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 30}
	positions := map[int64]domain.MailboxSnapshotPosition{
		1: {AliasID: 1, UIDValidity: 77, UID: 30},
	}

	t.Run("falls back to next newest message in shared window", func(t *testing.T) {
		session := &fakeIMAPSession{
			uidValidity: 77,
			uidNext:     31,
			mailboxUIDs: []uint32{10, 20},
			headerByUID: map[uint32][]byte{10: ordinaryHeader(), 20: aliasHeader("one@icloud.com")},
			bodyByUID:   map[uint32][]byte{20: rawMessage(20)},
			seenUIDs:    map[uint32]struct{}{20: {}},
		}
		result, err := testFetcher(session, now).FetchIncremental(
			context.Background(), testAccount(), "password", aliases, &previous, positions,
		)
		if err != nil {
			t.Fatal(err)
		}
		message := result.Messages[1]
		if message.SnapshotState != domain.SnapshotFound || message.UID != 20 || result.State.LastUID != 30 {
			t.Fatalf("expunged snapshot fallback = %#v", result)
		}
		if _, _, searches, maxActive, _ := session.counters(); searches != 0 || maxActive != 1 {
			t.Fatalf("expunge reconciliation searches=%d max-active=%d", searches, maxActive)
		}
		calls := session.calls()
		if len(calls) != 5 || calls[0].seqSet != "30" || !isUIDOnlyFetch(calls[0]) ||
			calls[1].seqSet != "1:2" || !isUIDOnlyFetch(calls[1]) ||
			calls[2].seqSet != "10,20" || !isHeaderFetch(calls[2]) || calls[3].seqSet != "20" ||
			calls[4].seqSet != "20" || !isUIDOnlyFetch(calls[4]) {
			t.Fatalf("shared expunge reconciliation calls = %#v", calls)
		}
	})

	t.Run("deletes snapshot when shared window has no fallback", func(t *testing.T) {
		session := &fakeIMAPSession{
			uidValidity: 77,
			uidNext:     31,
			mailboxUIDs: []uint32{10},
			headerByUID: map[uint32][]byte{10: ordinaryHeader()},
		}
		result, err := testFetcher(session, now).FetchIncremental(
			context.Background(), testAccount(), "password", aliases, &previous, positions,
		)
		if err != nil {
			t.Fatal(err)
		}
		message := result.Messages[1]
		if message.SnapshotState != domain.SnapshotEmpty || message.AliasID != 1 || result.State.LastUID != 30 {
			t.Fatalf("expunged snapshot deletion = %#v", result)
		}
		if searches := session.searches(); len(searches) != 0 {
			t.Fatalf("expunge deletion performed UID SEARCH: %v", searches)
		}
	})
}

func TestFetchIncrementalResetValidatesPreservedSameGenerationSnapshots(t *testing.T) {
	now := time.Date(2026, 8, 7, 13, 50, 0, 0, time.UTC)
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     31,
		mailboxUIDs: []uint32{5, 20, 30},
		headerByUID: map[uint32][]byte{
			5:  aliasHeader("one@icloud.com"),
			20: aliasHeader("two@icloud.com"),
			30: ordinaryHeader(),
		},
		bodyByUID: map[uint32][]byte{20: rawMessage(20)},
	}
	aliases := []domain.Alias{
		testAlias(1, "one@icloud.com"),
		testAlias(2, "two@icloud.com"),
	}
	positions := map[int64]domain.MailboxSnapshotPosition{
		1: {AliasID: 1, UIDValidity: 77, UID: 5},
		2: {AliasID: 2, UIDValidity: 77, UID: 25},
	}
	fetcher := testFetcher(session, now)
	fetcher.MaxCandidates = 2

	result, err := fetcher.FetchIncremental(
		context.Background(), testAccount(), "password", aliases, nil, positions,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reset || result.State.LastUID != 30 {
		t.Fatalf("reset result = %#v", result)
	}
	if _, exists := result.Messages[1]; exists {
		t.Fatalf("validated snapshot outside reset window was replaced: %#v", result.Messages[1])
	}
	if message := result.Messages[2]; message.SnapshotState != domain.SnapshotFound || message.UID != 20 {
		t.Fatalf("expunged reset snapshot fallback = %#v", message)
	}
	calls := session.calls()
	if len(calls) != 5 || calls[0].seqSet != "2:3" || !isUIDOnlyFetch(calls[0]) ||
		calls[1].seqSet != "5,25" || !isUIDOnlyFetch(calls[1]) ||
		calls[2].seqSet != "20,30" || !isHeaderFetch(calls[2]) || calls[3].seqSet != "20" ||
		calls[4].seqSet != "5,20" || !isUIDOnlyFetch(calls[4]) {
		t.Fatalf("bounded reset validation calls = %#v", calls)
	}
}

func TestFetchIncrementalFinalValidationRejectsExpungeDuringScan(t *testing.T) {
	incrementalPrevious := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 10}
	fallbackPrevious := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 30}
	tests := []struct {
		name          string
		session       *fakeIMAPSession
		aliases       []domain.Alias
		previous      *domain.IMAPSyncState
		positions     map[int64]domain.MailboxSnapshotPosition
		maxCandidates int
		wantCalls     int
		wantFinalUIDs string
		wantSearches  int
	}{
		{
			name: "new winner expunged after body fetch",
			session: &fakeIMAPSession{
				uidValidity:        77,
				uidNext:            12,
				mailboxUIDs:        []uint32{11},
				headerByUID:        map[uint32][]byte{11: aliasHeader("one@icloud.com")},
				bodyByUID:          map[uint32][]byte{11: rawMessage(11)},
				expungeBeforeFetch: map[int][]uint32{2: {11}},
			},
			aliases:       []domain.Alias{testAlias(1, "one@icloud.com")},
			previous:      &incrementalPrevious,
			wantCalls:     3,
			wantFinalUIDs: "11",
			wantSearches:  1,
		},
		{
			name: "same generation reset snapshot expunged after replacement body fetch",
			session: &fakeIMAPSession{
				uidValidity: 77,
				uidNext:     31,
				mailboxUIDs: []uint32{5, 20, 30},
				headerByUID: map[uint32][]byte{
					5:  aliasHeader("one@icloud.com"),
					20: aliasHeader("two@icloud.com"),
					30: ordinaryHeader(),
				},
				bodyByUID:          map[uint32][]byte{20: rawMessage(20)},
				expungeBeforeFetch: map[int][]uint32{4: {5}},
			},
			aliases: []domain.Alias{
				testAlias(1, "one@icloud.com"),
				testAlias(2, "two@icloud.com"),
			},
			positions: map[int64]domain.MailboxSnapshotPosition{
				1: {AliasID: 1, UIDValidity: 77, UID: 5},
				2: {AliasID: 2, UIDValidity: 77, UID: 25},
			},
			maxCandidates: 2,
			wantCalls:     5,
			wantFinalUIDs: "5,20",
		},
		{
			name: "incremental fallback expunged after body fetch",
			session: &fakeIMAPSession{
				uidValidity:        77,
				uidNext:            31,
				mailboxUIDs:        []uint32{10, 20},
				headerByUID:        map[uint32][]byte{10: ordinaryHeader(), 20: aliasHeader("one@icloud.com")},
				bodyByUID:          map[uint32][]byte{20: rawMessage(20)},
				expungeBeforeFetch: map[int][]uint32{4: {20}},
			},
			aliases:  []domain.Alias{testAlias(1, "one@icloud.com")},
			previous: &fallbackPrevious,
			positions: map[int64]domain.MailboxSnapshotPosition{
				1: {AliasID: 1, UIDValidity: 77, UID: 30},
			},
			wantCalls:     5,
			wantFinalUIDs: "20",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fetcher := testFetcher(test.session, time.Now())
			if test.maxCandidates > 0 {
				fetcher.MaxCandidates = test.maxCandidates
			}
			result, err := fetcher.FetchIncremental(
				context.Background(), testAccount(), "password", test.aliases, test.previous, test.positions,
			)
			if err == nil || !strings.Contains(err.Error(), "expunged before publish") {
				t.Fatalf("result = %#v, error = %v, want final EXPUNGE error", result, err)
			}
			wantState := domain.IMAPSyncState{}
			if test.previous != nil {
				wantState = *test.previous
			}
			if !reflect.DeepEqual(result.State, wantState) || len(result.Messages) != 0 {
				t.Fatalf("failed validation returned publishable result: %#v, want state %#v", result, wantState)
			}
			calls := test.session.calls()
			if len(calls) != test.wantCalls {
				t.Fatalf("FETCH calls = %#v, want %d", calls, test.wantCalls)
			}
			finalCall := calls[len(calls)-1]
			if finalCall.seqSet != test.wantFinalUIDs || !isUIDOnlyFetch(finalCall) {
				t.Fatalf("final shared validation = %#v, want UID FETCH %s", finalCall, test.wantFinalUIDs)
			}
			login, selectCount, searches, maxActive, terminated := test.session.counters()
			if login != 1 || selectCount != 1 || searches != test.wantSearches || maxActive != 1 || !terminated {
				t.Fatalf(
					"commands login=%d select=%d search=%d max-active=%d terminated=%v",
					login, selectCount, searches, maxActive, terminated,
				)
			}
		})
	}
}

func TestFetchIncrementalReadsOnlyNewUIDsAndLatestWinnerBody(t *testing.T) {
	now := time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC)
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     14,
		headerByUID: map[uint32][]byte{
			11: ordinaryHeader(),
			12: aliasHeader("one@icloud.com"),
			13: aliasHeader("one@icloud.com"),
		},
		bodyByUID: map[uint32][]byte{
			12: rawMessage(12),
			13: rawMessage(13),
		},
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 10}

	result, err := testFetcher(session, now).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{
			testAlias(1, "one@icloud.com"),
			testAlias(2, "two@icloud.com"),
		},
		&previous,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reset || result.State.LastUID != 13 || len(result.Messages) != 1 {
		t.Fatalf("result = %#v", result)
	}
	message := result.Messages[1]
	if message.SnapshotState != domain.SnapshotFound || message.UID != 13 || message.Subject != "message 13" {
		t.Fatalf("winner = %#v", message)
	}
	if _, exists := result.Messages[2]; exists {
		t.Fatalf("incremental miss unexpectedly replaced alias 2: %#v", result.Messages[2])
	}
	calls := session.calls()
	if len(calls) != 3 || calls[0].seqSet != "11:13" || !isHeaderFetch(calls[0]) ||
		calls[1].seqSet != "13" || isHeaderFetch(calls[1]) ||
		calls[2].seqSet != "13" || !isUIDOnlyFetch(calls[2]) {
		t.Fatalf("FETCH calls = %#v", calls)
	}
	if searches := session.searches(); !reflect.DeepEqual(searches, []string{"11:13"}) {
		t.Fatalf("account-level UID searches = %v", searches)
	}
}

func TestFetchIncrementalOrdinaryAccountMailDoesNotBlockAlias(t *testing.T) {
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     13,
		headerByUID: map[uint32][]byte{
			11: ordinaryHeader(),
			12: aliasHeader("one@icloud.com"),
		},
		bodyByUID: map[uint32][]byte{12: rawMessage(12)},
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 10}

	result, err := testFetcher(session, time.Now()).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		&previous,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[1].UID != 12 || result.State.LastUID != 12 {
		t.Fatalf("result = %#v", result)
	}
}

func TestFetchIncrementalInitialMissIsEmptyButIncrementalMissIsOmitted(t *testing.T) {
	aliases := []domain.Alias{testAlias(1, "one@icloud.com")}

	initialSession := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     3,
		headerByUID: map[uint32][]byte{1: ordinaryHeader(), 2: ordinaryHeader()},
	}
	initial, err := testFetcher(initialSession, time.Now()).FetchIncremental(
		context.Background(), testAccount(), "password", aliases, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !initial.Reset || initial.Messages[1].SnapshotState != domain.SnapshotEmpty {
		t.Fatalf("initial result = %#v", initial)
	}

	incrementalSession := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     3,
		headerByUID: map[uint32][]byte{1: ordinaryHeader(), 2: ordinaryHeader()},
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 0}
	incremental, err := testFetcher(incrementalSession, time.Now()).FetchIncremental(
		context.Background(), testAccount(), "password", aliases, &previous, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if incremental.Reset || len(incremental.Messages) != 0 || incremental.State.LastUID != 2 {
		t.Fatalf("incremental result = %#v", incremental)
	}
}

func TestFetchIncrementalUIDValidityChangeReturnsResetSnapshot(t *testing.T) {
	session := &fakeIMAPSession{
		uidValidity: 88,
		uidNext:     4,
		headerByUID: map[uint32][]byte{
			1: aliasHeader("one@icloud.com"),
			2: ordinaryHeader(),
			3: aliasHeader("two@icloud.com"),
		},
		bodyByUID: map[uint32][]byte{1: rawMessage(1), 3: rawMessage(3)},
		seenUIDs:  map[uint32]struct{}{1: {}, 2: {}, 3: {}},
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 999}

	result, err := testFetcher(session, time.Now()).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{
			testAlias(1, "one@icloud.com"),
			testAlias(2, "two@icloud.com"),
			testAlias(3, "three@icloud.com"),
		},
		&previous,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reset || result.State.UIDValidity != 88 || result.State.LastUID != 3 {
		t.Fatalf("reset result = %#v", result)
	}
	if result.Messages[1].UID != 1 || result.Messages[2].UID != 3 ||
		result.Messages[3].SnapshotState != domain.SnapshotEmpty {
		t.Fatalf("reset messages = %#v", result.Messages)
	}
	if searches := session.searches(); len(searches) != 0 {
		t.Fatalf("UIDVALIDITY reset unexpectedly used UNSEEN search: %v", searches)
	}
}

func TestFetchIncrementalProcessesLargeSparseBacklogInOldestBatches(t *testing.T) {
	newSession := func() *fakeIMAPSession {
		return &fakeIMAPSession{
			uidValidity: 77,
			uidNext:     9001,
			mailboxUIDs: []uint32{1, 5, 100, 1000, 2000, 5000, 9000},
			headerByUID: map[uint32][]byte{
				1:    ordinaryHeader(),
				5:    ordinaryHeader(),
				100:  aliasHeader("one@icloud.com"),
				1000: ordinaryHeader(),
				2000: ordinaryHeader(),
				5000: aliasHeader("one@icloud.com"),
				9000: ordinaryHeader(),
			},
			bodyByUID: map[uint32][]byte{100: rawMessage(100), 5000: rawMessage(5000)},
		}
	}
	now := time.Now()
	session := newSession()
	fetcher := testFetcher(session, now)
	fetcher.MaxIncrementalCandidates = 3
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 10}

	result, err := fetcher.FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		&previous,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.LastUID != 2000 || !result.HasMore || result.Messages[1].UID != 100 {
		t.Fatalf("result = %#v", result)
	}
	calls := session.calls()
	discoveryCalls := uidFlagsFetchCalls(calls)
	if len(discoveryCalls) != 1 || discoveryCalls[0].seqSet != "2:5" {
		t.Fatalf("first bounded discovery FETCH=%#v", discoveryCalls)
	}
	if len(calls) < 3 || calls[len(calls)-3].seqSet != "100,1000,2000" ||
		!isHeaderFetch(calls[len(calls)-3]) || calls[len(calls)-2].seqSet != "100" ||
		calls[len(calls)-1].seqSet != "100" || !isUIDOnlyFetch(calls[len(calls)-1]) {
		t.Fatalf("first bounded batch FETCH=%#v", calls)
	}
	if searches := session.searches(); len(searches) != 0 {
		t.Fatalf("first sparse batch performed UID SEARCH: %v", searches)
	}

	secondSession := newSession()
	secondFetcher := testFetcher(secondSession, now.Add(time.Minute))
	secondFetcher.MaxIncrementalCandidates = 3
	second, err := secondFetcher.FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		&result.State,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.State.LastUID != 9000 || second.HasMore || second.Messages[1].UID != 5000 {
		t.Fatalf("second result = %#v", second)
	}
	secondCalls := secondSession.calls()
	secondDiscoveryCalls := uidFlagsFetchCalls(secondCalls)
	if len(secondDiscoveryCalls) != 1 || secondDiscoveryCalls[0].seqSet != "5:7" {
		t.Fatalf("second bounded discovery FETCH=%#v", secondDiscoveryCalls)
	}
	if len(secondCalls) < 3 || secondCalls[len(secondCalls)-3].seqSet != "5000,9000" ||
		!isHeaderFetch(secondCalls[len(secondCalls)-3]) || secondCalls[len(secondCalls)-2].seqSet != "5000" ||
		secondCalls[len(secondCalls)-1].seqSet != "5000" || !isUIDOnlyFetch(secondCalls[len(secondCalls)-1]) {
		t.Fatalf("second bounded batch FETCH=%#v", secondCalls)
	}
	if searches := secondSession.searches(); len(searches) != 0 {
		t.Fatalf("second sparse batch performed UID SEARCH: %v", searches)
	}
}

func TestFetchIncrementalDefaultLimitProcesses257MessagesInTwoBatches(t *testing.T) {
	const messageCount = defaultMaxIncrementalCandidates + 1
	newSession := func() *fakeIMAPSession {
		uids := make([]uint32, 0, messageCount)
		headers := make(map[uint32][]byte, messageCount)
		for uid := uint32(1); uid <= messageCount; uid++ {
			uids = append(uids, uid)
			headers[uid] = ordinaryHeader()
		}
		return &fakeIMAPSession{
			uidValidity: 77,
			uidNext:     messageCount + 1,
			mailboxUIDs: uids,
			headerByUID: headers,
		}
	}

	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77}
	firstSession := newSession()
	first, err := testFetcher(firstSession, time.Now()).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		&previous,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.State.LastUID != defaultMaxIncrementalCandidates || !first.HasMore {
		t.Fatalf("first default batch state = %#v, want cursor 256 with more work", first)
	}
	firstHeaders := 0
	for _, call := range firstSession.calls() {
		if isHeaderFetch(call) {
			firstHeaders++
		}
	}
	_, headerFetchBatch := boundedHeaderFetchLimits(defaultMaxHeaderBytes)
	wantHeaderCalls := (defaultMaxIncrementalCandidates + headerFetchBatch - 1) / headerFetchBatch
	if firstHeaders != wantHeaderCalls {
		t.Fatalf("first default batch header FETCH calls = %d, want %d", firstHeaders, wantHeaderCalls)
	}
	if searches := firstSession.searches(); len(searches) != 0 {
		t.Fatalf("first default batch performed UID SEARCH: %v", searches)
	}
	if discoveryCalls := uidFlagsFetchCalls(firstSession.calls()); len(discoveryCalls) != 1 || discoveryCalls[0].seqSet != "1:256" {
		t.Fatalf("first default discovery FETCH=%#v", discoveryCalls)
	}

	secondSession := newSession()
	second, err := testFetcher(secondSession, time.Now().Add(time.Minute)).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		&first.State,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.State.LastUID != messageCount || second.HasMore {
		t.Fatalf("second default batch state = %#v, want cursor 257 caught up", second)
	}
	secondHeaders := 0
	for _, call := range secondSession.calls() {
		if isHeaderFetch(call) {
			secondHeaders++
		}
	}
	if secondHeaders != 1 {
		t.Fatalf("second default batch header FETCH calls = %d, want 1", secondHeaders)
	}
	if searches := secondSession.searches(); !reflect.DeepEqual(searches, []string{"257"}) {
		t.Fatalf("second default UID SEARCH ranges = %v", searches)
	}
	if flags := secondSession.searchFlags(); len(flags) != 1 || !reflect.DeepEqual(flags[0], []string{imap.SeenFlag}) {
		t.Fatalf("second default UID SEARCH WithoutFlags = %#v", flags)
	}
}

func TestFetchIncrementalBacklogReconcilesExpungedSnapshotBeforeLaterReplacement(t *testing.T) {
	newSession := func() *fakeIMAPSession {
		return &fakeIMAPSession{
			uidValidity: 77,
			uidNext:     51,
			mailboxUIDs: []uint32{5, 20, 30, 40, 50},
			headerByUID: map[uint32][]byte{
				5:  ordinaryHeader(),
				20: ordinaryHeader(),
				30: ordinaryHeader(),
				40: aliasHeader("one@icloud.com"),
				50: ordinaryHeader(),
			},
			bodyByUID: map[uint32][]byte{40: rawMessage(40)},
		}
	}
	aliases := []domain.Alias{
		testAlias(1, "one@icloud.com"),
		testAlias(2, "two@icloud.com"),
	}
	positions := map[int64]domain.MailboxSnapshotPosition{
		1: {AliasID: 1, UIDValidity: 77, UID: 10},
		2: {AliasID: 2, UIDValidity: 77, UID: 5},
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 10}

	firstSession := newSession()
	firstFetcher := testFetcher(firstSession, time.Now())
	firstFetcher.MaxIncrementalCandidates = 2
	first, err := firstFetcher.FetchIncremental(
		context.Background(), testAccount(), "password", aliases, &previous, positions,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.State.LastUID != 30 || !first.HasMore {
		t.Fatalf("first backlog state = %#v", first)
	}
	if message := first.Messages[1]; message.SnapshotState != domain.SnapshotEmpty {
		t.Fatalf("first missing snapshot reconciliation = %#v", message)
	}
	if _, replaced := first.Messages[2]; replaced {
		t.Fatalf("valid retained snapshot was replaced: %#v", first.Messages[2])
	}
	assertUIDOnlyFetchBeforeHeaders(t, firstSession.calls(), "5,10")
	assertFinalUIDOnlyFetch(t, firstSession.calls(), "5")
	if searches := firstSession.searches(); len(searches) != 0 {
		t.Fatalf("first backlog batch performed UID SEARCH: %v", searches)
	}
	if discoveryCalls := uidFlagsFetchCalls(firstSession.calls()); len(discoveryCalls) != 1 || discoveryCalls[0].seqSet != "1:3" {
		t.Fatalf("first backlog discovery FETCH=%#v", discoveryCalls)
	}

	secondSession := newSession()
	secondFetcher := testFetcher(secondSession, time.Now().Add(time.Minute))
	secondFetcher.MaxIncrementalCandidates = 2
	remainingPositions := map[int64]domain.MailboxSnapshotPosition{
		2: {AliasID: 2, UIDValidity: 77, UID: 5},
	}
	second, err := secondFetcher.FetchIncremental(
		context.Background(), testAccount(), "password", aliases, &first.State, remainingPositions,
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.State.LastUID != 50 || second.HasMore {
		t.Fatalf("second backlog state = %#v", second)
	}
	if message := second.Messages[1]; message.SnapshotState != domain.SnapshotFound || message.UID != 40 {
		t.Fatalf("later replacement = %#v", message)
	}
	if _, replaced := second.Messages[2]; replaced {
		t.Fatalf("valid retained snapshot was replaced: %#v", second.Messages[2])
	}
	assertUIDOnlyFetchBeforeHeaders(t, secondSession.calls(), "5")
	assertFinalUIDOnlyFetch(t, secondSession.calls(), "5,40")
	if searches := secondSession.searches(); len(searches) != 0 {
		t.Fatalf("second backlog batch performed UID SEARCH: %v", searches)
	}
	if discoveryCalls := uidFlagsFetchCalls(secondSession.calls()); len(discoveryCalls) != 1 || discoveryCalls[0].seqSet != "3:5" {
		t.Fatalf("second backlog discovery FETCH=%#v", discoveryCalls)
	}
}

func assertUIDOnlyFetchBeforeHeaders(t *testing.T, calls []fetchCall, seqSet string) {
	t.Helper()
	for index, call := range calls {
		if !isHeaderFetch(call) {
			continue
		}
		if index == 0 {
			t.Fatalf("header FETCH had no shared UID validation before it: %#v", calls)
		}
		if calls[index-1].seqSet != seqSet || !isUIDOnlyFetch(calls[index-1]) {
			t.Fatalf("shared UID validation before headers = %#v, want UID FETCH %s; all FETCH calls = %#v", calls[index-1], seqSet, calls)
		}
		return
	}
	t.Fatalf("missing header FETCH after shared UID validation %s: %#v", seqSet, calls)
}

func assertFinalUIDOnlyFetch(t *testing.T, calls []fetchCall, seqSet string) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatalf("missing final shared UID validation %s", seqSet)
	}
	last := calls[len(calls)-1]
	if last.seqSet != seqSet || !isUIDOnlyFetch(last) {
		t.Fatalf("final shared UID validation = %#v, want UID FETCH %s; all FETCH calls = %#v", last, seqSet, calls)
	}
}

func TestFetchIncrementalResetUsesNewestActualSparseMessages(t *testing.T) {
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     1001,
		mailboxUIDs: []uint32{5, 400, 900, 1000},
		headerByUID: map[uint32][]byte{
			5:    aliasHeader("two@icloud.com"),
			400:  aliasHeader("one@icloud.com"),
			900:  ordinaryHeader(),
			1000: ordinaryHeader(),
		},
		bodyByUID: map[uint32][]byte{400: rawMessage(400)},
	}
	fetcher := testFetcher(session, time.Now())
	fetcher.MaxCandidates = 3

	result, err := fetcher.FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com"), testAlias(2, "two@icloud.com")},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	calls := session.calls()
	if !result.Reset || result.HasMore || result.State.LastUID != 1000 || result.Messages[1].UID != 400 {
		t.Fatalf("sparse reset result = %#v", result)
	}
	if _, exists := result.Messages[2]; exists {
		t.Fatalf("bounded reset replaced alias outside recent window: %#v", result.Messages[2])
	}
	if len(calls) != 4 || calls[0].seqSet != "2:4" || calls[1].seqSet != "400,900,1000" ||
		calls[2].seqSet != "400" || calls[3].seqSet != "400" || !isUIDOnlyFetch(calls[3]) {
		t.Fatalf("sparse reset calls = %#v", calls)
	}
	if searches := session.searches(); len(searches) != 0 {
		t.Fatalf("reset performed UID SEARCH: %v", searches)
	}
}

func TestFetchIncrementalResetHasBoundedCommandCountAtProductionLimits(t *testing.T) {
	const (
		mailboxMessages        = defaultMaxCandidates + 1
		winnerCount            = defaultMaxAliases
		connectionTimeoutSteps = 3 // TCP dial, TLS handshake, and IMAP greeting.
		loginSelectCommands    = 2
	)
	if maxContentFetchLiteralBytes != 12<<20 {
		t.Fatalf("content FETCH aggregate literal budget = %d, want 12 MiB", maxContentFetchLiteralBytes)
	}
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     mailboxMessages + 1,
		headerByUID: make(map[uint32][]byte, mailboxMessages),
		bodyByUID:   make(map[uint32][]byte, winnerCount),
	}
	aliases := make([]domain.Alias, 0, winnerCount)
	positions := make(map[int64]domain.MailboxSnapshotPosition, winnerCount)
	firstWinnerUID := uint32(mailboxMessages - winnerCount + 1)
	for uid := uint32(1); uid <= mailboxMessages; uid++ {
		session.headerByUID[uid] = ordinaryHeader()
	}
	for index := 0; index < winnerCount; index++ {
		aliasID := int64(index + 1)
		uid := firstWinnerUID + uint32(index)
		address := fmt.Sprintf("winner-%d@icloud.com", aliasID)
		aliases = append(aliases, testAlias(aliasID, address))
		positions[aliasID] = domain.MailboxSnapshotPosition{
			AliasID:     aliasID,
			UIDValidity: session.uidValidity,
			UID:         uid,
		}
		session.headerByUID[uid] = aliasHeader(address)
		session.bodyByUID[uid] = rawMessage(uid)
		if index == 0 {
			session.bodyByUID[uid] = []byte(fmt.Sprintf(
				"Message-ID: <%d@example.com>\r\nSubject: large message\r\n\r\n%s",
				uid,
				strings.Repeat("x", maxContentFetchLiteralBytes/defaultMaxIncrementalCandidates),
			))
		}
	}

	result, err := testFetcher(session, time.Now()).FetchIncremental(
		context.Background(), testAccount(), "password", aliases, nil, positions,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reset || result.HasMore || result.State.LastUID != mailboxMessages || len(result.Messages) != winnerCount {
		t.Fatalf("bounded reset result = reset:%v more:%v cursor:%d messages:%d", result.Reset, result.HasMore, result.State.LastUID, len(result.Messages))
	}
	if !result.Messages[1].BodyTruncated {
		t.Fatal("large body in a full winner batch was not marked truncated")
	}

	calls := session.calls()
	headerCalls := make([]fetchCall, 0)
	for _, call := range calls {
		if isHeaderFetch(call) {
			headerCalls = append(headerCalls, call)
		}
	}
	bodyCalls := bodyFetchCalls(calls)
	headerBytes, headerFetchBatch := boundedHeaderFetchLimits(defaultMaxHeaderBytes)
	if headerBytes != defaultMaxHeaderBytes || headerFetchBatch != 95 {
		t.Fatalf("default header limits = %d bytes/%d UIDs, want %d/95", headerBytes, headerFetchBatch, defaultMaxHeaderBytes)
	}
	wantHeaderCommands := (defaultMaxCandidates + headerFetchBatch - 1) / headerFetchBatch
	wantBodyCommands := (winnerCount + messageFetchBatch - 1) / messageFetchBatch
	if len(headerCalls) != wantHeaderCommands || len(bodyCalls) != wantBodyCommands {
		t.Fatalf(
			"content FETCH commands = headers:%d bodies:%d, want %d/%d; all calls=%#v",
			len(headerCalls), len(bodyCalls), wantHeaderCommands, wantBodyCommands, calls,
		)
	}
	for _, group := range [][]fetchCall{headerCalls, bodyCalls} {
		for _, call := range group {
			requested := requestedSequenceCount(t, call.seqSet, mailboxMessages)
			if requested > defaultMaxIncrementalCandidates {
				t.Fatalf("content FETCH exceeds %d UIDs: call=%#v count=%d", defaultMaxIncrementalCandidates, call, requested)
			}
			section := bodyFetchSection(t, call)
			if len(section.Partial) != 2 || section.Partial[0] != 0 || section.Partial[1] <= 0 {
				t.Fatalf("content FETCH has invalid partial section: call=%#v section=%#v", call, section)
			}
			if aggregate := requested * section.Partial[1]; aggregate > maxContentFetchLiteralBytes {
				t.Fatalf("content FETCH literal budget = %d, exceeds %d: call=%#v", aggregate, maxContentFetchLiteralBytes, call)
			}
		}
	}

	// The remaining UID FETCHes are one recent-window discovery, one optional
	// current-generation snapshot validation, and one final publish validation.
	const fixedFetchCommands = 3
	wantFetchCommands := fixedFetchCommands + wantHeaderCommands + wantBodyCommands
	if len(calls) != wantFetchCommands {
		t.Fatalf("reset FETCH commands = %d, want %d: %#v", len(calls), wantFetchCommands, calls)
	}
	loginCalls, selectCalls, _, maxActive, _ := session.counters()
	if loginCalls != 1 || selectCalls != 1 || maxActive != 1 {
		t.Fatalf("session commands = login:%d select:%d max-active:%d", loginCalls, selectCalls, maxActive)
	}
	serialIMAPCommands := loginSelectCommands + wantFetchCommands
	if serialIMAPCommands != 20 {
		t.Fatalf("maximum reset IMAP commands = %d, want 20", serialIMAPCommands)
	}
	defaultWorstCase := time.Duration(connectionTimeoutSteps+serialIMAPCommands) * defaultIMAPTimeout
	const defaultSyncTimeout = 10 * time.Minute
	if defaultWorstCase >= defaultSyncTimeout {
		t.Fatalf("default reset timeout bound = %v, must be below %v", defaultWorstCase, defaultSyncTimeout)
	}
	if defaultWorstCase != 575*time.Second {
		t.Fatalf("default reset timeout bound = %v, want 575s", defaultWorstCase)
	}
	if margin := defaultSyncTimeout - defaultWorstCase; margin != 25*time.Second {
		t.Fatalf("default reset timeout margin = %v, want 25s", margin)
	}
}

func TestFetchCandidateWinnersPreservesLargeHeaderUnderAggregateBudget(t *testing.T) {
	const candidateCount = defaultMaxIncrementalCandidates
	session := &fakeIMAPSession{headerByUID: make(map[uint32][]byte, candidateCount)}
	candidateUIDs := make([]uint32, 0, candidateCount)
	for uid := uint32(1); uid <= candidateCount; uid++ {
		candidateUIDs = append(candidateUIDs, uid)
		session.headerByUID[uid] = ordinaryHeader()
	}
	largeHeader := []byte(
		"X-Original-To: " + strings.Repeat(" ", 60<<10) + "one@icloud.com\r\n\r\n",
	)
	if len(largeHeader) <= maxContentFetchLiteralBytes/candidateCount || len(largeHeader) > defaultMaxHeaderBytes {
		t.Fatalf("large header test fixture size = %d", len(largeHeader))
	}
	session.headerByUID[candidateCount] = largeHeader

	winners, err := fetchCandidateWinners(
		context.Background(),
		session,
		candidateUIDs,
		map[string][]int64{"one@icloud.com": {1}},
		"owner@icloud.com",
		defaultMaxHeaderBytes,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int64]uint32{1: candidateCount}; !reflect.DeepEqual(winners, want) {
		t.Fatalf("large header winners = %#v, want %#v", winners, want)
	}
	calls := session.calls()
	if len(calls) != 3 {
		t.Fatalf("header FETCH calls = %d, want 3", len(calls))
	}
	for _, call := range calls {
		section := bodyFetchSection(t, call)
		if !reflect.DeepEqual(section.Partial, []int{0, defaultMaxHeaderBytes + 1}) {
			t.Fatalf("header partial = %v, want 0.%d", section.Partial, defaultMaxHeaderBytes+1)
		}
		requested := requestedSequenceCount(t, call.seqSet, candidateCount)
		if aggregate := requested * section.Partial[1]; aggregate > maxContentFetchLiteralBytes {
			t.Fatalf("header aggregate = %d, exceeds %d", aggregate, maxContentFetchLiteralBytes)
		}
	}
}

func TestFetchIncrementalFailureDoesNotAdvanceCursor(t *testing.T) {
	previous := domain.IMAPSyncState{
		AccountID:   7,
		UIDValidity: 77,
		LastUID:     10,
		UpdatedAt:   time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name    string
		session *fakeIMAPSession
	}{
		{
			name: "winner body missing",
			session: &fakeIMAPSession{
				uidValidity: 77,
				uidNext:     12,
				headerByUID: map[uint32][]byte{11: aliasHeader("one@icloud.com")},
				bodyByUID:   map[uint32][]byte{},
			},
		},
		{
			name: "duplicate candidate response",
			session: &fakeIMAPSession{
				uidValidity:  77,
				uidNext:      12,
				headerByUID:  map[uint32][]byte{11: aliasHeader("one@icloud.com")},
				duplicateUID: 11,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := testFetcher(test.session, time.Now()).FetchIncremental(
				context.Background(),
				testAccount(),
				"password",
				[]domain.Alias{testAlias(1, "one@icloud.com")},
				&previous,
				nil,
			)
			if err == nil {
				t.Fatalf("result = %#v, want error", result)
			}
			if !reflect.DeepEqual(result.State, previous) {
				t.Fatalf("state advanced on error: got %#v, want %#v", result.State, previous)
			}
		})
	}
}

func TestFetchIncrementalSkipsUnrouteableCandidateAndAdvancesCursor(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     13,
		headerByUID: map[uint32][]byte{
			11: aliasHeader("one@icloud.com"),
			12: []byte("X-Original-To: one@icloud.com\r\nDelivered-To: other@icloud.com\r\n\r\n"),
		},
		bodyByUID: map[uint32][]byte{11: rawMessage(11)},
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 10}

	result, err := testFetcher(session, now).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		&previous,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.LastUID != 12 || result.HasMore || result.Messages[1].UID != 11 {
		t.Fatalf("result = %#v, want cursor 12 and fallback winner UID 11", result)
	}
	calls := session.calls()
	if len(calls) != 3 || calls[0].seqSet != "11:12" || calls[1].seqSet != "11" || calls[2].seqSet != "11" {
		t.Fatalf("FETCH calls = %#v", calls)
	}
}

func TestFetchIncrementalSkipsMalformedAndOversizedCandidateHeaders(t *testing.T) {
	tests := []struct {
		name           string
		header         []byte
		maxHeaderBytes int
	}{
		{name: "malformed", header: []byte("not a header\r\n\r\n")},
		{name: "oversized", header: aliasHeader("one@icloud.com"), maxHeaderBytes: 8},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &fakeIMAPSession{
				uidValidity: 77,
				uidNext:     12,
				headerByUID: map[uint32][]byte{11: test.header},
			}
			fetcher := testFetcher(session, time.Now())
			if test.maxHeaderBytes > 0 {
				fetcher.MaxHeaderBytes = test.maxHeaderBytes
			}
			previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 10}

			result, err := fetcher.FetchIncremental(
				context.Background(),
				testAccount(),
				"password",
				[]domain.Alias{testAlias(1, "one@icloud.com")},
				&previous,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.State.LastUID != 11 || len(result.Messages) != 0 {
				t.Fatalf("result = %#v, want skipped candidate and cursor 11", result)
			}
		})
	}
}

func TestFetchLatestUsesAuthoritativeInitialSemantics(t *testing.T) {
	session := &fakeIMAPSession{uidValidity: 77, uidNext: 1}
	messages, err := testFetcher(session, time.Now()).FetchLatest(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if messages[1].SnapshotState != domain.SnapshotEmpty || len(session.calls()) != 0 {
		t.Fatalf("messages = %#v, calls = %#v", messages, session.calls())
	}
}

func TestFetchIncrementalRejectsInvalidMailboxState(t *testing.T) {
	tests := []struct {
		name        string
		uidValidity uint32
		uidNext     uint32
		want        string
	}{
		{name: "zero UIDVALIDITY", uidValidity: 0, uidNext: 1, want: "UIDVALIDITY is zero"},
		{name: "zero UIDNEXT", uidValidity: 1, uidNext: 0, want: "UIDNEXT is zero"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &fakeIMAPSession{uidValidity: test.uidValidity, uidNext: test.uidNext}
			_, err := testFetcher(session, time.Now()).FetchIncremental(
				context.Background(), testAccount(), "password", nil, nil, nil,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestFetchIncrementalHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetcher := NewFetcher()
	dialed := false
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		dialed = true
		return nil, errors.New("unexpected dial")
	}
	_, err := fetcher.FetchIncremental(ctx, testAccount(), "password", nil, nil, nil)
	if !errors.Is(err, context.Canceled) || dialed {
		t.Fatalf("error = %v, dialed = %v", err, dialed)
	}
}

func TestFetchIncrementalRejectsAliasFromAnotherAccount(t *testing.T) {
	fetcher := NewFetcher()
	_, err := fetcher.FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{{ID: 1, AccountID: 99, Address: "one@icloud.com", Enabled: true}},
		nil,
		nil,
	)
	if !errors.Is(err, ErrAliasAccountMismatch) {
		t.Fatalf("error = %v, want ErrAliasAccountMismatch", err)
	}
}

func TestFetchMessagesBatchesUIDs(t *testing.T) {
	session := &fakeIMAPSession{bodyByUID: make(map[uint32][]byte)}
	uids := make([]uint32, 0, messageFetchBatch+1)
	aliasesByUID := make(map[uint32][]int64)
	for uid := uint32(1); uid <= uint32(messageFetchBatch+1); uid++ {
		uids = append(uids, uid)
		session.bodyByUID[uid] = rawMessage(uid)
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
	if len(messages) != len(uids) || len(session.calls()) != 2 {
		t.Fatalf("messages = %d, FETCH calls = %d", len(messages), len(session.calls()))
	}
}

func TestFetchIncrementalBoundsMultiWinnerBodyReadsByResultBudget(t *testing.T) {
	const (
		winnerCount     = 4
		maxResultBytes  = 4 << 10
		maxMessageBytes = 10 << 20
	)
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     winnerCount + 1,
		headerByUID: make(map[uint32][]byte, winnerCount),
		bodyByUID:   make(map[uint32][]byte, winnerCount),
	}
	aliases := make([]domain.Alias, 0, winnerCount)
	for index := 1; index <= winnerCount; index++ {
		uid := uint32(index)
		address := fmt.Sprintf("winner-%d@icloud.com", index)
		aliases = append(aliases, testAlias(int64(index), address))
		session.headerByUID[uid] = aliasHeader(address)
		session.bodyByUID[uid] = []byte(fmt.Sprintf(
			"Message-ID: <%d@example.com>\r\nSubject: message %d\r\n\r\n%s",
			uid,
			uid,
			strings.Repeat("x", maxResultBytes),
		))
	}

	fetcher := testFetcher(session, time.Now())
	fetcher.MaxMessageBytes = maxMessageBytes
	fetcher.MaxBodyBytes = maxResultBytes
	fetcher.MaxParsedMessageBytes = maxResultBytes
	fetcher.MaxFetchResultBytes = maxResultBytes
	result, err := fetcher.FetchIncremental(
		context.Background(), testAccount(), "password", aliases, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	resultDynamicBudget := int64(maxResultBytes - winnerCount*parsedMessageBaseBytes)
	fairDynamicBytes := resultDynamicBudget / winnerCount
	fairParsedBytes := int64(parsedMessageBaseBytes) + fairDynamicBytes
	fairBodyBytes := fairDynamicBytes
	wantMessageBytes := int(fairParsedBytes + fairBodyBytes)
	bodyCalls := bodyFetchCalls(session.calls())
	if len(bodyCalls) != 1 {
		t.Fatalf("body FETCH calls = %d, want 1; all calls = %#v", len(bodyCalls), session.calls())
	}
	section := bodyFetchSection(t, bodyCalls[0])
	if !section.Peek || !reflect.DeepEqual(section.Partial, []int{0, wantMessageBytes + 1}) {
		t.Fatalf("body section = %#v, want PEEK partial 0.%d", section, wantMessageBytes+1)
	}
	if total := winnerCount * section.Partial[1]; total > 2*maxResultBytes {
		t.Fatalf("bounded body request bytes = %d, result budget = %d", total, maxResultBytes)
	}
	if len(result.Messages) != winnerCount {
		t.Fatalf("messages = %d, want %d", len(result.Messages), winnerCount)
	}
	for aliasID, message := range result.Messages {
		if !message.BodyTruncated {
			t.Errorf("alias %d body was not marked truncated", aliasID)
		}
	}
}

func TestBoundedWinnerMessageBytesKeepsSingleWinnerLimit(t *testing.T) {
	const maxMessageBytes = 10 << 20
	if got := boundedWinnerMessageBytes(maxMessageBytes, 1, 128, 1024, 2048); got != maxMessageBytes {
		t.Fatalf("single-winner message bytes = %d, want %d", got, maxMessageBytes)
	}
	if got := boundedContentLiteralBytes(maxMessageBytes, 1); got != maxMessageBytes {
		t.Fatalf("single-winner aggregate message bytes = %d, want %d", got, maxMessageBytes)
	}
}

func bodyFetchCalls(calls []fetchCall) []fetchCall {
	result := make([]fetchCall, 0)
	for _, call := range calls {
		if isHeaderFetch(call) || isUIDOnlyFetch(call) {
			continue
		}
		for _, item := range call.items {
			if _, err := imap.ParseBodySectionName(item); err == nil {
				result = append(result, call)
				break
			}
		}
	}
	return result
}

func bodyFetchSection(t *testing.T, call fetchCall) *imap.BodySectionName {
	t.Helper()
	for _, item := range call.items {
		section, err := imap.ParseBodySectionName(item)
		if err == nil {
			return section
		}
	}
	t.Fatalf("missing body section in FETCH call %#v", call)
	return nil
}

func TestFetchIncrementalPublishesPlaceholderForMalformedWinnerBody(t *testing.T) {
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     12,
		headerByUID: map[uint32][]byte{11: aliasHeader("one@icloud.com")},
		bodyByUID:   map[uint32][]byte{11: []byte("not a message")},
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 10}

	result, err := testFetcher(session, time.Now()).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		&previous,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	message := result.Messages[1]
	if result.State.LastUID != 11 || message.UID != 11 || !message.BodyTruncated || message.SnapshotState != domain.SnapshotFound {
		t.Fatalf("result = %#v, want truncated placeholder at UID 11", result)
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

func TestFetchRecentMailboxUIDsUsesActualSparseMessages(t *testing.T) {
	session := &fakeIMAPSession{mailboxUIDs: []uint32{5, 400, 900, 1000}}
	uids, err := fetchRecentMailboxUIDs(context.Background(), session, 4, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint32{1000, 900, 400}; !reflect.DeepEqual(uids, want) {
		t.Fatalf("recent mailbox UIDs = %v, want %v", uids, want)
	}
	calls := session.calls()
	if len(calls) != 1 || calls[0].seqSet != "2:4" || isHeaderFetch(calls[0]) || len(calls[0].items) != 1 || calls[0].items[0] != imap.FetchUid {
		t.Fatalf("sequence UID discovery calls = %#v", calls)
	}
}

func TestFetchIncrementalMailboxUIDsSearchExcludesSeenAndAdvancesEmptyRange(t *testing.T) {
	tests := []struct {
		name       string
		seen       []uint32
		wantUIDs   []uint32
		wantCursor uint32
	}{
		{name: "mixed", seen: []uint32{20, 40}, wantUIDs: []uint32{30, 10}, wantCursor: 40},
		{name: "all seen", seen: []uint32{10, 20, 30, 40}, wantUIDs: []uint32{}, wantCursor: 40},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			seen := make(map[uint32]struct{}, len(test.seen))
			for _, uid := range test.seen {
				seen[uid] = struct{}{}
			}
			session := &fakeIMAPSession{
				mailboxUIDs: []uint32{10, 20, 30, 40},
				seenUIDs:    seen,
			}
			uids, processedThrough, hasMore, err := fetchIncrementalMailboxUIDs(
				context.Background(), session, 4, 0, 40, 100,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(uids, test.wantUIDs) || processedThrough != test.wantCursor || hasMore {
				t.Fatalf("result = %v/%d/%v, want %v/%d/false", uids, processedThrough, hasMore, test.wantUIDs, test.wantCursor)
			}
			if searches := session.searches(); !reflect.DeepEqual(searches, []string{"1:40"}) {
				t.Fatalf("UID SEARCH ranges = %v, want [1:40]", searches)
			}
			if flags := session.searchFlags(); len(flags) != 1 || !reflect.DeepEqual(flags[0], []string{imap.SeenFlag}) {
				t.Fatalf("UID SEARCH WithoutFlags = %#v, want [[%q]]", flags, imap.SeenFlag)
			}
		})
	}
}

func TestFetchIncrementalMailboxUIDsRejectsCursorBeyondUpperUID(t *testing.T) {
	const (
		lastUID  = ^uint32(0)
		upperUID = lastUID - 1
	)
	session := &fakeIMAPSession{mailboxUIDs: []uint32{upperUID}}
	uids, processedThrough, hasMore, err := fetchIncrementalMailboxUIDs(
		context.Background(), session, 1, lastUID, upperUID, 1,
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds mailbox upper UID") {
		t.Fatalf("result = %v/%d/%v, error = %v, want cursor-boundary error", uids, processedThrough, hasMore, err)
	}
	if processedThrough != lastUID || hasMore || len(uids) != 0 {
		t.Fatalf("cursor advanced on invalid boundary: %v/%d/%v", uids, processedThrough, hasMore)
	}
	if calls := session.calls(); len(calls) != 0 {
		t.Fatalf("invalid cursor performed FETCH calls: %#v", calls)
	}
}

func TestFetchIncrementalMailboxUIDsHandlesMaximumRepresentableUpperUID(t *testing.T) {
	const upperUID = ^uint32(0) - 1
	session := &fakeIMAPSession{mailboxUIDs: []uint32{upperUID}}
	uids, processedThrough, hasMore, err := fetchIncrementalMailboxUIDs(
		context.Background(), session, 1, 0, upperUID, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint32{upperUID}; !reflect.DeepEqual(uids, want) || processedThrough != upperUID || hasMore {
		t.Fatalf("result = %v/%d/%v, want %v/%d/false", uids, processedThrough, hasMore, want, upperUID)
	}
	calls := uidFlagsFetchCalls(session.calls())
	if len(calls) != 1 || calls[0].seqSet != "1" {
		t.Fatalf("maximum-UID discovery calls = %#v, want singleton sequence FETCH", calls)
	}
}

func TestFetchIncrementalMailboxUIDsLargeRangeSkipsSeenWithoutSkippingUnread(t *testing.T) {
	seen := map[uint32]struct{}{20: {}, 40: {}}
	session := &fakeIMAPSession{
		mailboxUIDs: []uint32{10, 20, 30, 40, 50, 60},
		seenUIDs:    seen,
	}
	uids, processedThrough, hasMore, err := fetchIncrementalMailboxUIDs(
		context.Background(), session, 6, 10, 60, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint32{30}; !reflect.DeepEqual(uids, want) || processedThrough != 30 || !hasMore {
		t.Fatalf("first result = %v/%d/%v, want %v/30/true", uids, processedThrough, hasMore, want)
	}
	if searches := session.searches(); len(searches) != 0 {
		t.Fatalf("large range performed UID SEARCH: %v", searches)
	}
	if discoveryCalls := uidFlagsFetchCalls(session.calls()); len(discoveryCalls) != 1 || discoveryCalls[0].seqSet != "1:3" {
		t.Fatalf("first large-range discovery FETCH=%#v", discoveryCalls)
	}

	second, secondThrough, secondMore, err := fetchIncrementalMailboxUIDs(
		context.Background(), session, 6, processedThrough, 60, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint32{50}; !reflect.DeepEqual(second, want) || secondThrough != 50 || !secondMore {
		t.Fatalf("second result = %v/%d/%v, want %v/50/true", second, secondThrough, secondMore, want)
	}

	third, thirdThrough, thirdMore, err := fetchIncrementalMailboxUIDs(
		context.Background(), session, 6, secondThrough, 60, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint32{60}; !reflect.DeepEqual(third, want) || thirdThrough != 60 || thirdMore {
		t.Fatalf("third result = %v/%d/%v, want %v/60/false", third, thirdThrough, thirdMore, want)
	}
	discoveryCalls := uidFlagsFetchCalls(session.calls())
	if len(discoveryCalls) != 3 || discoveryCalls[0].seqSet != "1:3" ||
		discoveryCalls[1].seqSet != "3:5" || discoveryCalls[2].seqSet != "5:6" {
		t.Fatalf("large-range discovery FETCH calls=%#v", discoveryCalls)
	}
}

func TestFetchIncrementalMailboxUIDsLargeRangeAllSeenStillAdvances(t *testing.T) {
	session := &fakeIMAPSession{
		mailboxUIDs: []uint32{10, 20, 30, 40, 50},
		seenUIDs: map[uint32]struct{}{
			20: {}, 30: {}, 40: {}, 50: {},
		},
	}

	first, firstThrough, firstMore, err := fetchIncrementalMailboxUIDs(
		context.Background(), session, 5, 10, 50, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 0 || firstThrough != 30 || !firstMore {
		t.Fatalf("first all-seen result = %v/%d/%v, want []/30/true", first, firstThrough, firstMore)
	}

	second, secondThrough, secondMore, err := fetchIncrementalMailboxUIDs(
		context.Background(), session, 5, firstThrough, 50, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 || secondThrough != 50 || secondMore {
		t.Fatalf("second all-seen result = %v/%d/%v, want []/50/false", second, secondThrough, secondMore)
	}
	if searches := session.searches(); len(searches) != 0 {
		t.Fatalf("all-seen large range performed UID SEARCH: %v", searches)
	}
	discoveryCalls := uidFlagsFetchCalls(session.calls())
	if len(discoveryCalls) != 2 || discoveryCalls[0].seqSet != "1:3" || discoveryCalls[1].seqSet != "3:5" {
		t.Fatalf("all-seen discovery FETCH calls=%#v", discoveryCalls)
	}
}

func TestFetchIncrementalMailboxUIDsTenThousandUnreadAdvancesInBoundedBatches(t *testing.T) {
	const (
		messageCount = 10_000
		limit        = 256
	)
	mailboxUIDs := make([]uint32, messageCount)
	for index := range mailboxUIDs {
		mailboxUIDs[index] = uint32(index + 1)
	}
	session := &fakeIMAPSession{mailboxUIDs: mailboxUIDs}

	first, firstThrough, firstMore, err := fetchIncrementalMailboxUIDs(
		context.Background(), session, messageCount, 0, messageCount, limit,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != limit || first[0] != limit || first[len(first)-1] != 1 || firstThrough != limit || !firstMore {
		t.Fatalf("first batch len=%d bounds=%d..%d cursor=%d more=%v", len(first), first[0], first[len(first)-1], firstThrough, firstMore)
	}

	second, secondThrough, secondMore, err := fetchIncrementalMailboxUIDs(
		context.Background(), session, messageCount, firstThrough, messageCount, limit,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != limit || second[0] != 2*limit || second[len(second)-1] != limit+1 || secondThrough != 2*limit || !secondMore {
		t.Fatalf("second batch len=%d bounds=%d..%d cursor=%d more=%v", len(second), second[0], second[len(second)-1], secondThrough, secondMore)
	}
	if searches := session.searches(); len(searches) != 0 {
		t.Fatalf("large backlog performed UID SEARCH: %v", searches)
	}
	discoveryCalls := uidFlagsFetchCalls(session.calls())
	if len(discoveryCalls) != 2 || discoveryCalls[0].seqSet != "1:256" || discoveryCalls[1].seqSet != "256:512" {
		t.Fatalf("bounded discovery FETCH calls=%#v", discoveryCalls)
	}
	if maximum := requestedSequenceCount(t, discoveryCalls[1].seqSet, messageCount); maximum != limit+1 {
		t.Fatalf("maximum discovery response = %d, want %d", maximum, limit+1)
	}
	var uidOnlySets []string
	for _, call := range session.calls() {
		requested := requestedSequenceCount(t, call.seqSet, messageCount)
		switch {
		case isUIDFlagsFetch(call.items):
			if requested > limit+1 {
				t.Fatalf("UID/FLAGS discovery response can exceed bound: call=%#v count=%d limit=%d", call, requested, limit)
			}
		case len(call.items) == 1 && call.items[0] == imap.FetchUid:
			uidOnlySets = append(uidOnlySets, call.seqSet)
			if requested != 1 {
				t.Fatalf("binary UID probe is not singleton: call=%#v count=%d", call, requested)
			}
		default:
			t.Fatalf("unexpected discovery FETCH call=%#v", call)
		}
	}
	wantUIDOnlySets := []string{"1", "257", "257", "1", "257", "513", "513", "256"}
	if !reflect.DeepEqual(uidOnlySets, wantUIDOnlySets) {
		t.Fatalf("dense mailbox UID-only probes = %v, want %v", uidOnlySets, wantUIDOnlySets)
	}
}

func TestFetchIncrementalMailboxUIDsExpungeDuringSequenceDiscoveryDoesNotAdvance(t *testing.T) {
	t.Run("expunge crosses the boundary FETCH response", func(t *testing.T) {
		base := &fakeIMAPSession{
			mailboxUIDs: []uint32{1, 2, 3, 4, 5},
		}
		session := &midResponseExpungeSession{
			fakeIMAPSession: base,
			targetCall:      2, // the initial boundary probe plus the leading trailing sentinel precede 2:4
			expungeUID:      2,
		}

		uids, processedThrough, hasMore, err := fetchIncrementalMailboxUIDs(
			context.Background(), session, 5, 2, 5, 2,
		)
		if err == nil || !strings.Contains(err.Error(), "recheck incremental trailing sequence sentinel") {
			t.Fatalf("result = %v/%d/%v, error = %v, want trailing boundary change", uids, processedThrough, hasMore, err)
		}
		if processedThrough != 2 || hasMore || len(uids) != 0 {
			t.Fatalf("crossed-boundary response advanced cursor: %v/%d/%v", uids, processedThrough, hasMore)
		}
		calls := base.calls()
		if len(calls) != 4 || calls[1].seqSet != "5" || calls[2].seqSet != "2:4" ||
			calls[3].seqSet != "5" || !isUIDOnlyFetch(calls[3]) {
			t.Fatalf("crossed-boundary FETCH calls = %#v", calls)
		}
	})

	t.Run("expunge inside the window shifts the trailing sentinel", func(t *testing.T) {
		base := &fakeIMAPSession{
			mailboxUIDs: []uint32{1, 2, 3, 4, 5, 6},
		}
		session := &midResponseExpungeSession{
			fakeIMAPSession: base,
			targetCall:      2, // the initial boundary probe plus the leading trailing sentinel precede 2:4
			expungeUID:      3,
		}

		uids, processedThrough, hasMore, err := fetchIncrementalMailboxUIDs(
			context.Background(), session, 6, 2, 6, 2,
		)
		if err == nil || !strings.Contains(err.Error(), "trailing sequence sentinel") {
			t.Fatalf("result = %v/%d/%v, error = %v, want trailing sentinel change", uids, processedThrough, hasMore, err)
		}
		if processedThrough != 2 || hasMore || len(uids) != 0 {
			t.Fatalf("window EXPUNGE advanced cursor: %v/%d/%v", uids, processedThrough, hasMore)
		}
		calls := base.calls()
		if len(calls) != 4 || calls[1].seqSet != "5" || calls[2].seqSet != "2:4" || calls[3].seqSet != "5" {
			t.Fatalf("window EXPUNGE FETCH calls = %#v", calls)
		}
	})

	t.Run("expunge of the first sequence is caught without a trailing window", func(t *testing.T) {
		base := &fakeIMAPSession{
			mailboxUIDs: []uint32{1, 2, 3, 4, 5},
		}
		session := &midResponseExpungeSession{
			fakeIMAPSession: base,
			targetCall:      1, // the initial boundary probe precedes the 1:4 window
			expungeUID:      1,
			responses: []*imap.Message{
				{SeqNum: 1, Uid: 1},
				{SeqNum: 2, Uid: 3},
				{SeqNum: 3, Uid: 4},
				{SeqNum: 4, Uid: 5},
			},
		}

		uids, processedThrough, hasMore, err := fetchIncrementalMailboxUIDs(
			context.Background(), session, 4, 0, 5, 4,
		)
		if err == nil || !strings.Contains(err.Error(), "mailbox sequence boundary changed after batch fetch") {
			t.Fatalf("result = %v/%d/%v, error = %v, want leading boundary change", uids, processedThrough, hasMore, err)
		}
		if processedThrough != 0 || hasMore || len(uids) != 0 {
			t.Fatalf("first-sequence EXPUNGE advanced cursor: %v/%d/%v", uids, processedThrough, hasMore)
		}
		calls := base.calls()
		if len(calls) != 3 || calls[1].seqSet != "1:4" || calls[2].seqSet != "1" || !isUIDOnlyFetch(calls[2]) {
			t.Fatalf("first-sequence EXPUNGE FETCH calls = %#v", calls)
		}
	})

	t.Run("boundary shifts but response remains full", func(t *testing.T) {
		const (
			messageCount = 20
			lastUID      = 10
		)
		mailboxUIDs := make([]uint32, messageCount)
		for index := range mailboxUIDs {
			mailboxUIDs[index] = uint32(index + 1)
		}
		session := &fakeIMAPSession{
			mailboxUIDs:        mailboxUIDs,
			expungeBeforeFetch: map[int][]uint32{4: {1}},
		}

		uids, processedThrough, hasMore, err := fetchIncrementalMailboxUIDs(
			context.Background(), session, messageCount, lastUID, messageCount, 2,
		)
		if err == nil || !strings.Contains(err.Error(), "sequence boundary changed") {
			t.Fatalf("result = %v/%d/%v, error = %v, want changed sequence boundary", uids, processedThrough, hasMore, err)
		}
		if processedThrough != lastUID || hasMore || len(uids) != 0 {
			t.Fatalf("shifted sequence advanced cursor: %v/%d/%v", uids, processedThrough, hasMore)
		}
		discoveryCalls := uidFlagsFetchCalls(session.calls())
		if len(discoveryCalls) != 1 || discoveryCalls[0].seqSet != "10:12" {
			t.Fatalf("shifted-boundary discovery FETCH calls=%#v", discoveryCalls)
		}
	})

	t.Run("selected sequence range becomes short", func(t *testing.T) {
		const lastUID = 5000
		session := &fakeIMAPSession{
			mailboxUIDs:        []uint32{1, 5, 100, 1000, 2000, 5000, 9000, 10000},
			expungeBeforeFetch: map[int][]uint32{3: {1}},
		}

		uids, processedThrough, hasMore, err := fetchIncrementalMailboxUIDs(
			context.Background(), session, 8, lastUID, 10000, 3,
		)
		if err == nil || !strings.Contains(err.Error(), "unstable mailbox view") {
			t.Fatalf("result = %v/%d/%v, error = %v, want unstable mailbox view", uids, processedThrough, hasMore, err)
		}
		if processedThrough != lastUID || hasMore || len(uids) != 0 {
			t.Fatalf("unstable sequence advanced cursor: %v/%d/%v", uids, processedThrough, hasMore)
		}
		discoveryCalls := uidFlagsFetchCalls(session.calls())
		if len(discoveryCalls) != 1 || discoveryCalls[0].seqSet != "6:8" {
			t.Fatalf("unstable discovery FETCH calls=%#v", discoveryCalls)
		}
	})
}

func TestFetchMailboxSequenceRangeRejectsOversizedResponse(t *testing.T) {
	session := &fakeIMAPSession{}
	_, err := fetchMailboxUIDSequenceRange(
		context.Background(),
		session,
		1,
		maxSequenceFetchMessages+1,
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds bounded response limit") {
		t.Fatalf("oversized sequence range error = %v", err)
	}
	if calls := session.calls(); len(calls) != 0 {
		t.Fatalf("oversized sequence range FETCH calls = %#v, want rejection before FETCH", calls)
	}
}

func requestedSequenceCount(t *testing.T, raw string, maximum uint32) int {
	t.Helper()
	set, err := imap.ParseSeqSet(raw)
	if err != nil {
		t.Fatalf("parse sequence set %q: %v", raw, err)
	}
	count := 0
	for sequence := uint32(1); sequence <= maximum; sequence++ {
		if set.Contains(sequence) {
			count++
		}
	}
	return count
}
