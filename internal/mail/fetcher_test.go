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

func TestFetchIncrementalReportsProgressStagesAndTargetUID(t *testing.T) {
	session := &fakeIMAPSession{uidValidity: 77, uidNext: 5}
	var updates []domain.MailboxSyncProgressUpdate
	ctx := domain.WithMailboxSyncProgressReporter(
		context.Background(),
		func(update domain.MailboxSyncProgressUpdate) {
			updates = append(updates, update)
		},
	)

	result, err := testFetcher(session, time.Now()).FetchIncremental(
		ctx, testAccount(), "password", nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("fetch empty mailbox: %v", err)
	}
	if result.TargetUID != 4 || result.State.LastUID != 4 {
		t.Fatalf("mailbox progress target/state = %d/%d, want 4/4", result.TargetUID, result.State.LastUID)
	}
	want := []domain.MailboxSyncProgressUpdate{
		{Phase: domain.MailboxSyncPhaseConnecting, Percent: 5},
		{Phase: domain.MailboxSyncPhaseAuthenticating, Percent: 10},
		{Phase: domain.MailboxSyncPhaseScanning, Percent: 15},
		{Phase: domain.MailboxSyncPhaseReading, Percent: 20},
		{Phase: domain.MailboxSyncPhaseValidating, Percent: 25},
	}
	if !reflect.DeepEqual(updates, want) {
		t.Fatalf("progress updates = %#v, want %#v", updates, want)
	}
}

func TestNewFetcherUsesSeparateCandidateDefaults(t *testing.T) {
	fetcher := NewFetcher()
	if fetcher.IMAPTimeout != 8*time.Second || fetcher.MaxMessageBytes != 1<<20 ||
		fetcher.MaxBodyBytes != 512<<10 {
		t.Fatalf(
			"fetcher defaults = timeout:%v message:%d body:%d, want 8s/%d/%d",
			fetcher.IMAPTimeout,
			fetcher.MaxMessageBytes,
			fetcher.MaxBodyBytes,
			1<<20,
			512<<10,
		)
	}
	if fetcher.MaxCandidates != 1024 || fetcher.MaxIncrementalCandidates != 256 {
		t.Fatalf(
			"candidate defaults recent=%d incremental=%d, want 1024/256",
			fetcher.MaxCandidates,
			fetcher.MaxIncrementalCandidates,
		)
	}
	settings := (&Fetcher{}).settings()
	if settings.timeout != 8*time.Second || settings.maxMessageBytes != 1<<20 ||
		settings.maxBodyBytes != 512<<10 {
		t.Fatalf(
			"zero-value settings = timeout:%v message:%d body:%d, want 8s/%d/%d",
			settings.timeout,
			settings.maxMessageBytes,
			settings.maxBodyBytes,
			1<<20,
			512<<10,
		)
	}
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

func TestFetchIncrementalDoesNotInspectSnapshotPositions(t *testing.T) {
	now := time.Date(2026, 8, 7, 13, 30, 0, 0, time.UTC)
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     31,
		mailboxUIDs: []uint32{20},
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 30}
	positions := map[int64]domain.MailboxSnapshotPosition{
		1: {AliasID: 1, UIDValidity: 77, UID: 30},
	}

	result, err := testFetcher(session, now).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		&previous,
		positions,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reset || result.HasMore || len(result.Messages) != 0 || result.State.LastUID != 30 {
		t.Fatalf("stable result = %#v", result)
	}
	if calls := session.calls(); len(calls) != 0 {
		t.Fatalf("snapshot positions triggered IMAP FETCH calls: %#v", calls)
	}
	if searches := session.searches(); len(searches) != 0 {
		t.Fatalf("snapshot positions triggered UID SEARCH calls: %v", searches)
	}
}

func TestFetchIncrementalResetCommitsBoundaryWithoutFetchingContent(t *testing.T) {
	now := time.Date(2026, 8, 7, 13, 45, 0, 0, time.UTC)
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     4,
		mailboxUIDs: []uint32{1, 2, 3},
		headerByUID: map[uint32][]byte{
			1: aliasHeader("one@icloud.com"),
			2: ordinaryHeader(),
			3: aliasHeader("one@icloud.com"),
		},
		bodyByUID: map[uint32][]byte{1: rawMessage(1), 3: rawMessage(3)},
		seenUIDs:  map[uint32]struct{}{1: {}},
	}

	result, err := testFetcher(session, now).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reset || !result.HasMore || result.State.LastUID != 0 || result.TargetUID != 3 || len(result.Messages) != 0 {
		t.Fatalf("reset baseline result = %#v", result)
	}
	if calls := session.calls(); len(calls) != 0 {
		t.Fatalf("reset baseline downloaded message content: %#v", calls)
	}
	if searches := session.searches(); len(searches) != 0 {
		t.Fatalf("reset baseline searched message content: %v", searches)
	}
}

func TestFetchIncrementalResetContinuationFetchesOnlyUnseen(t *testing.T) {
	now := time.Date(2026, 8, 7, 13, 50, 0, 0, time.UTC)
	aliases := []domain.Alias{testAlias(1, "one@icloud.com")}
	newSession := func() *fakeIMAPSession {
		return &fakeIMAPSession{
			uidValidity: 77,
			uidNext:     4,
			mailboxUIDs: []uint32{1, 2, 3},
			headerByUID: map[uint32][]byte{
				1: aliasHeader("one@icloud.com"),
				2: ordinaryHeader(),
				3: aliasHeader("one@icloud.com"),
			},
			bodyByUID: map[uint32][]byte{1: rawMessage(1), 3: rawMessage(3)},
			seenUIDs:  map[uint32]struct{}{1: {}},
		}
	}

	initialSession := newSession()
	initial, err := testFetcher(initialSession, now).FetchIncremental(
		context.Background(), testAccount(), "password", aliases, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !initial.Reset || !initial.HasMore || initial.State.LastUID != 0 || len(initial.Messages) != 0 {
		t.Fatalf("initial reset result = %#v", initial)
	}

	continuationSession := newSession()
	continuation, err := testFetcher(continuationSession, now.Add(time.Minute)).FetchIncremental(
		context.Background(), testAccount(), "password", aliases, &initial.State, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	message := continuation.Messages[1]
	if continuation.Reset || continuation.HasMore || continuation.State.LastUID != 3 ||
		message.SnapshotState != domain.SnapshotFound || message.UID != 3 {
		t.Fatalf("reset continuation result = %#v", continuation)
	}
	if searches := continuationSession.searches(); !reflect.DeepEqual(searches, []string{"1:3"}) {
		t.Fatalf("continuation UID searches = %v, want [1:3]", searches)
	}
	if flags := continuationSession.searchFlags(); len(flags) != 1 || !reflect.DeepEqual(flags[0], []string{imap.SeenFlag}) {
		t.Fatalf("continuation UID SEARCH WithoutFlags = %#v", flags)
	}
	calls := continuationSession.calls()
	if len(calls) != 3 || calls[0].seqSet != "2:3" || !isHeaderFetch(calls[0]) ||
		calls[1].seqSet != "3" || isHeaderFetch(calls[1]) ||
		calls[2].seqSet != "3" || !isUIDOnlyFetch(calls[2]) {
		t.Fatalf("continuation FETCH calls = %#v", calls)
	}
}

func TestFetchIncrementalFinalValidationRejectsNewWinnerExpunge(t *testing.T) {
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 10}
	session := &fakeIMAPSession{
		uidValidity:        77,
		uidNext:            12,
		mailboxUIDs:        []uint32{11},
		headerByUID:        map[uint32][]byte{11: aliasHeader("one@icloud.com")},
		bodyByUID:          map[uint32][]byte{11: rawMessage(11)},
		expungeBeforeFetch: map[int][]uint32{2: {11}},
	}

	result, err := testFetcher(session, time.Now()).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		&previous,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "expunged before publish") {
		t.Fatalf("result = %#v, error = %v, want final EXPUNGE error", result, err)
	}
	if !reflect.DeepEqual(result.State, previous) || len(result.Messages) != 0 {
		t.Fatalf("failed validation returned publishable result: %#v, want state %#v", result, previous)
	}
	calls := session.calls()
	if len(calls) != 3 || calls[2].seqSet != "11" || !isUIDOnlyFetch(calls[2]) {
		t.Fatalf("final winner validation calls = %#v", calls)
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

func TestFetchIncrementalResetEmptyMailboxDoesNotCreateAliasSnapshots(t *testing.T) {
	session := &fakeIMAPSession{uidValidity: 77, uidNext: 1}

	result, err := testFetcher(session, time.Now()).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reset || result.HasMore || result.State.LastUID != 0 || result.TargetUID != 0 || len(result.Messages) != 0 {
		t.Fatalf("empty reset result = %#v", result)
	}
	if calls := session.calls(); len(calls) != 0 {
		t.Fatalf("empty reset performed FETCH calls: %#v", calls)
	}
	if searches := session.searches(); len(searches) != 0 {
		t.Fatalf("empty reset performed UID SEARCH calls: %v", searches)
	}
}

func TestFetchIncrementalResetAllSeenAdvancesOnContinuation(t *testing.T) {
	aliases := []domain.Alias{testAlias(1, "one@icloud.com")}
	newSession := func() *fakeIMAPSession {
		return &fakeIMAPSession{
			uidValidity: 77,
			uidNext:     3,
			mailboxUIDs: []uint32{1, 2},
			headerByUID: map[uint32][]byte{
				1: aliasHeader("one@icloud.com"),
				2: aliasHeader("one@icloud.com"),
			},
			bodyByUID: map[uint32][]byte{1: rawMessage(1), 2: rawMessage(2)},
			seenUIDs:  map[uint32]struct{}{1: {}, 2: {}},
		}
	}

	initialSession := newSession()
	initial, err := testFetcher(initialSession, time.Now()).FetchIncremental(
		context.Background(), testAccount(), "password", aliases, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !initial.Reset || !initial.HasMore || initial.State.LastUID != 0 || len(initial.Messages) != 0 {
		t.Fatalf("all-seen reset baseline = %#v", initial)
	}

	continuationSession := newSession()
	continuation, err := testFetcher(continuationSession, time.Now().Add(time.Minute)).FetchIncremental(
		context.Background(), testAccount(), "password", aliases, &initial.State, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if continuation.Reset || continuation.HasMore || continuation.State.LastUID != 2 || len(continuation.Messages) != 0 {
		t.Fatalf("all-seen continuation = %#v", continuation)
	}
	if searches := continuationSession.searches(); !reflect.DeepEqual(searches, []string{"1:2"}) {
		t.Fatalf("all-seen UID searches = %v, want [1:2]", searches)
	}
	if flags := continuationSession.searchFlags(); len(flags) != 1 || !reflect.DeepEqual(flags[0], []string{imap.SeenFlag}) {
		t.Fatalf("all-seen UID SEARCH WithoutFlags = %#v", flags)
	}
	if calls := continuationSession.calls(); len(calls) != 0 {
		t.Fatalf("all-seen continuation downloaded content: %#v", calls)
	}
}

func TestFetchIncrementalUIDValidityChangeStartsUnreadBaseline(t *testing.T) {
	now := time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC)
	aliases := []domain.Alias{
		testAlias(1, "one@icloud.com"),
		testAlias(2, "two@icloud.com"),
	}
	newSession := func() *fakeIMAPSession {
		return &fakeIMAPSession{
			uidValidity: 88,
			uidNext:     4,
			mailboxUIDs: []uint32{1, 2, 3},
			headerByUID: map[uint32][]byte{
				1: aliasHeader("one@icloud.com"),
				2: ordinaryHeader(),
				3: aliasHeader("two@icloud.com"),
			},
			bodyByUID: map[uint32][]byte{1: rawMessage(1), 3: rawMessage(3)},
			seenUIDs:  map[uint32]struct{}{1: {}, 2: {}},
		}
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 999}

	resetSession := newSession()
	reset, err := testFetcher(resetSession, now).FetchIncremental(
		context.Background(), testAccount(), "password", aliases, &previous, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reset.Reset || !reset.HasMore || reset.State.UIDValidity != 88 || reset.State.LastUID != 0 ||
		reset.TargetUID != 3 || len(reset.Messages) != 0 {
		t.Fatalf("UIDVALIDITY reset result = %#v", reset)
	}
	if calls := resetSession.calls(); len(calls) != 0 {
		t.Fatalf("UIDVALIDITY reset downloaded content: %#v", calls)
	}
	if searches := resetSession.searches(); len(searches) != 0 {
		t.Fatalf("UIDVALIDITY reset performed UID SEARCH calls: %v", searches)
	}

	continuationSession := newSession()
	continuation, err := testFetcher(continuationSession, now.Add(time.Minute)).FetchIncremental(
		context.Background(), testAccount(), "password", aliases, &reset.State, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if continuation.Reset || continuation.HasMore || continuation.State.UIDValidity != 88 ||
		continuation.State.LastUID != 3 || continuation.Messages[2].UID != 3 {
		t.Fatalf("UIDVALIDITY continuation result = %#v", continuation)
	}
	calls := continuationSession.calls()
	if len(calls) != 3 || calls[0].seqSet != "3" || !isHeaderFetch(calls[0]) ||
		calls[1].seqSet != "3" || isHeaderFetch(calls[1]) ||
		calls[2].seqSet != "3" || !isUIDOnlyFetch(calls[2]) {
		t.Fatalf("UIDVALIDITY continuation FETCH calls = %#v", calls)
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

func TestFetchIncrementalResetUsesNewestActualSparseWindowBoundary(t *testing.T) {
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
	if !result.Reset || !result.HasMore || result.State.LastUID != 5 || result.TargetUID != 1000 || len(result.Messages) != 0 {
		t.Fatalf("sparse reset result = %#v", result)
	}
	if len(calls) != 3 || calls[0].seqSet != "4" || !isUIDOnlyFetch(calls[0]) ||
		calls[1].seqSet != "1" || !isUIDOnlyFetch(calls[1]) ||
		calls[2].seqSet != "4" || !isUIDOnlyFetch(calls[2]) {
		t.Fatalf("sparse reset calls = %#v", calls)
	}
	if searches := session.searches(); len(searches) != 0 {
		t.Fatalf("reset performed UID SEARCH: %v", searches)
	}
}

func TestFetchIncrementalResetBoundsProductionWindowWithoutContent(t *testing.T) {
	const mailboxMessages = defaultMaxCandidates + 1
	uids := make([]uint32, 0, mailboxMessages)
	for uid := uint32(1); uid <= mailboxMessages; uid++ {
		uids = append(uids, uid)
	}
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     mailboxMessages + 1,
		mailboxUIDs: uids,
	}

	result, err := testFetcher(session, time.Now()).FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reset || !result.HasMore || result.State.LastUID != 1 ||
		result.TargetUID != mailboxMessages || len(result.Messages) != 0 {
		t.Fatalf("bounded reset result = reset:%v more:%v cursor:%d messages:%d", result.Reset, result.HasMore, result.State.LastUID, len(result.Messages))
	}
	calls := session.calls()
	if len(calls) != 3 || calls[0].seqSet != fmt.Sprint(mailboxMessages) || !isUIDOnlyFetch(calls[0]) ||
		calls[1].seqSet != "1" || !isUIDOnlyFetch(calls[1]) ||
		calls[2].seqSet != fmt.Sprint(mailboxMessages) || !isUIDOnlyFetch(calls[2]) {
		t.Fatalf("production reset boundary calls = %#v", calls)
	}
	if searches := session.searches(); len(searches) != 0 {
		t.Fatalf("production reset performed UID SEARCH: %v", searches)
	}
}

func TestFetchIncrementalResetBoundaryExpungeDoesNotAdvance(t *testing.T) {
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 66, LastUID: 999}
	session := &fakeIMAPSession{
		uidValidity:        77,
		uidNext:            1001,
		mailboxUIDs:        []uint32{5, 400, 900, 1000},
		expungeBeforeFetch: map[int][]uint32{2: {5}},
	}
	fetcher := testFetcher(session, time.Now())
	fetcher.MaxCandidates = 3

	result, err := fetcher.FetchIncremental(
		context.Background(),
		testAccount(),
		"password",
		[]domain.Alias{testAlias(1, "one@icloud.com")},
		&previous,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "recheck recent mailbox window anchor") ||
		!strings.Contains(err.Error(), "unstable mailbox view") {
		t.Fatalf("result = %#v, error = %v, want unstable reset boundary", result, err)
	}
	if !reflect.DeepEqual(result.State, previous) || len(result.Messages) != 0 || result.Reset || result.HasMore {
		t.Fatalf("unstable reset boundary advanced state: %#v, want %#v", result, previous)
	}
	calls := session.calls()
	if len(calls) != 3 || calls[0].seqSet != "4" || calls[1].seqSet != "1" || calls[2].seqSet != "4" {
		t.Fatalf("unstable reset boundary calls = %#v", calls)
	}
	for _, call := range calls {
		if !isUIDOnlyFetch(call) {
			t.Fatalf("unstable reset boundary downloaded content: %#v", calls)
		}
	}
	if searches := session.searches(); len(searches) != 0 {
		t.Fatalf("unstable reset boundary performed UID SEARCH: %v", searches)
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

func TestFetchIncrementalReconnectsWithConservativeBatches(t *testing.T) {
	const winnerCount = compatibilityMessageBatch + 1
	if compatibilityMessageBatch >= messageFetchBatch {
		t.Fatalf("retry body batch = %d, want less than initial batch %d", compatibilityMessageBatch, messageFetchBatch)
	}
	newSession := func() *fakeIMAPSession {
		session := &fakeIMAPSession{
			uidValidity: 77,
			uidNext:     winnerCount + 1,
			headerByUID: make(map[uint32][]byte, winnerCount),
			bodyByUID:   make(map[uint32][]byte, winnerCount),
		}
		for uid := uint32(1); uid <= winnerCount; uid++ {
			address := fmt.Sprintf("winner-%d@icloud.com", uid)
			session.headerByUID[uid] = aliasHeader(address)
			session.bodyByUID[uid] = rawMessage(uid)
		}
		return session
	}
	first := newSession()
	first.fetchErrors = []error{nil, errors.New("imap: connection closed")}
	second := newSession()
	sessions := []imapSession{first, second}
	dialCalls := 0
	fetcher := NewFetcher()
	// Exercise the aggregate cap as well as the reconnect divisor. Without the
	// initial-batch cap, switching from two UIDs to one could increase Partial.
	fetcher.MaxMessageBytes = 20 << 20
	fetcher.MaxBodyBytes = 20 << 20
	fetcher.MaxParsedMessageBytes = 20 << 20
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		if dialCalls >= len(sessions) {
			return nil, errors.New("unexpected extra IMAP reconnect")
		}
		session := sessions[dialCalls]
		dialCalls++
		return session, nil
	}
	aliases := make([]domain.Alias, 0, winnerCount)
	for id := int64(1); id <= winnerCount; id++ {
		aliases = append(aliases, testAlias(id, fmt.Sprintf("winner-%d@icloud.com", id)))
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 0}

	result, err := fetcher.FetchIncremental(
		context.Background(), testAccount(), "password", aliases, &previous, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if dialCalls != 2 || len(result.Messages) != winnerCount {
		t.Fatalf("reconnect result = dials:%d messages:%d", dialCalls, len(result.Messages))
	}
	if _, _, _, _, terminated := first.counters(); !terminated {
		t.Fatal("failed IMAP session was not terminated before reconnect")
	}
	firstBodyCalls := bodyFetchCalls(first.calls())
	if len(firstBodyCalls) != 1 {
		t.Fatalf("initial body FETCH calls = %d, want 1; calls=%#v", len(firstBodyCalls), first.calls())
	}
	initialSection := bodyFetchSection(t, firstBodyCalls[0])
	if requested := requestedSequenceCount(t, firstBodyCalls[0].seqSet, winnerCount); requested != messageFetchBatch {
		t.Fatalf("initial body batch requested %d messages, want %d", requested, messageFetchBatch)
	}
	bodyCalls := bodyFetchCalls(second.calls())
	if len(bodyCalls) != winnerCount {
		t.Fatalf("conservative body FETCH calls = %d, want %d; calls=%#v", len(bodyCalls), winnerCount, second.calls())
	}
	for _, call := range bodyCalls {
		if requested := requestedSequenceCount(t, call.seqSet, winnerCount); requested != compatibilityMessageBatch {
			t.Fatalf("retry body batch requested %d messages, want %d", requested, compatibilityMessageBatch)
		}
		section := bodyFetchSection(t, call)
		wantBytes := max(1, (initialSection.Partial[1]-1)/compatibilityMessageByteDivisor)
		if gotBytes := section.Partial[1] - 1; gotBytes != wantBytes {
			t.Fatalf("retry body partial bytes = %d, want %d from initial %d", gotBytes, wantBytes, initialSection.Partial[1]-1)
		}
	}
}

func TestFetchIncrementalReconnectsOnlyOnce(t *testing.T) {
	newDisconnectedSession := func() *fakeIMAPSession {
		return &fakeIMAPSession{
			uidValidity: 77,
			uidNext:     2,
			headerByUID: map[uint32][]byte{1: aliasHeader("one@icloud.com")},
			bodyByUID:   map[uint32][]byte{1: rawMessage(1)},
			fetchErrors: []error{nil, nil, errors.New("imap: connection closed")},
		}
	}
	sessions := []imapSession{newDisconnectedSession(), newDisconnectedSession()}
	dialCalls := 0
	fetcher := NewFetcher()
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		dialCalls++
		if dialCalls > len(sessions) {
			return nil, errors.New("unexpected third IMAP connection")
		}
		return sessions[dialCalls-1], nil
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 0}

	_, err := fetcher.FetchIncremental(
		context.Background(), testAccount(), "password",
		[]domain.Alias{testAlias(1, "one@icloud.com")}, &previous, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "imap: connection closed") {
		t.Fatalf("second disconnect error = %v", err)
	}
	if dialCalls != 2 {
		t.Fatalf("disconnect dial calls = %d, want 2", dialCalls)
	}
}

func TestFetchIncrementalStopsReconnectAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     2,
		headerByUID: map[uint32][]byte{1: aliasHeader("one@icloud.com")},
		bodyByUID:   map[uint32][]byte{1: rawMessage(1)},
		fetchErrors: []error{nil, nil, errors.New("imap: connection closed")},
	}
	dialCalls := 0
	fetcher := NewFetcher()
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		dialCalls++
		if dialCalls > 1 {
			return nil, errors.New("unexpected reconnect after cancellation")
		}
		return first, nil
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 0}

	result := make(chan error, 1)
	go func() {
		_, err := fetcher.FetchIncremental(
			ctx, testAccount(), "password",
			[]domain.Alias{testAlias(1, "one@icloud.com")}, &previous, nil,
		)
		result <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if len(first.calls()) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first IMAP attempt did not reach the disconnect")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled reconnect error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled reconnect did not return")
	}
	if dialCalls != 1 {
		t.Fatalf("canceled reconnect dial calls = %d, want 1", dialCalls)
	}
}

func TestFetchIncrementalDoesNotReconnectForPermanentIMAPError(t *testing.T) {
	session := &fakeIMAPSession{
		uidValidity: 77,
		uidNext:     2,
		headerByUID: map[uint32][]byte{1: aliasHeader("one@icloud.com")},
		bodyByUID:   map[uint32][]byte{1: rawMessage(1)},
		fetchErrors: []error{nil, nil, errors.New("permanent IMAP command failure")},
	}
	dialCalls := 0
	fetcher := testFetcher(session, time.Now())
	fetcher.dial = func(context.Context, string, string, time.Duration) (imapSession, error) {
		dialCalls++
		return session, nil
	}
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 0}

	_, err := fetcher.FetchIncremental(
		context.Background(), testAccount(), "password",
		[]domain.Alias{testAlias(1, "one@icloud.com")}, &previous, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "permanent IMAP command failure") {
		t.Fatalf("permanent error = %v", err)
	}
	if dialCalls != 1 {
		t.Fatalf("permanent error dial calls = %d, want 1", dialCalls)
	}
}

func TestRetryableIMAPDisconnectRecognizesPlatformResetMessages(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "windows receive reset", err: errors.New("wsarecv: An existing connection was forcibly closed by the remote host"), want: true},
		{name: "windows send abort", err: errors.New("wsasend: An established connection was aborted by the software in your host machine"), want: true},
		{name: "unix reset", err: errors.New("write tcp: connection reset by peer"), want: true},
		{name: "protocol failure", err: errors.New("imap: invalid command sequence"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isRetryableIMAPDisconnect(test.err); got != test.want {
				t.Fatalf("retryable(%q) = %v, want %v", test.err, got, test.want)
			}
		})
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
	calls := bodyFetchCalls(session.calls())
	if len(messages) != len(uids) || len(calls) != 2 {
		t.Fatalf("messages = %d, body FETCH calls = %d", len(messages), len(calls))
	}
	for _, call := range calls {
		if requested := requestedSequenceCount(t, call.seqSet, uint32(len(uids))); requested > messageFetchBatch {
			t.Fatalf("initial body batch requested %d messages, limit %d", requested, messageFetchBatch)
		}
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
	previous := domain.IMAPSyncState{AccountID: 7, UIDValidity: 77, LastUID: 0}
	result, err := fetcher.FetchIncremental(
		context.Background(), testAccount(), "password", aliases, &previous, nil,
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
	wantBodyCalls := (winnerCount + messageFetchBatch - 1) / messageFetchBatch
	if len(bodyCalls) != wantBodyCalls {
		t.Fatalf("body FETCH calls = %d, want %d; all calls = %#v", len(bodyCalls), wantBodyCalls, session.calls())
	}
	for _, call := range bodyCalls {
		section := bodyFetchSection(t, call)
		if !section.Peek || !reflect.DeepEqual(section.Partial, []int{0, wantMessageBytes + 1}) {
			t.Fatalf("body section = %#v, want PEEK partial 0.%d", section, wantMessageBytes+1)
		}
		requested := requestedSequenceCount(t, call.seqSet, winnerCount)
		if total := requested * section.Partial[1]; total > 2*maxResultBytes {
			t.Fatalf("bounded body request bytes = %d, result budget = %d", total, maxResultBytes)
		}
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

func TestFetchMessagesBoundsSingleWinnerPartialByConfiguredBudgets(t *testing.T) {
	tests := []struct {
		name            string
		maxMessageBytes int
		maxBodyBytes    int64
		maxParsedBytes  int64
		wantBytes       int
	}{
		{
			name:            "production defaults",
			maxMessageBytes: defaultMaxMessageBytes,
			maxBodyBytes:    defaultMaxBodyBytes,
			maxParsedBytes:  defaultMaxBodyBytes + defaultMetadataResultBytes,
			wantBytes:       min(defaultMaxMessageBytes, 2*defaultMaxBodyBytes+defaultMetadataResultBytes),
		},
		{
			name:            "body and parsed budgets",
			maxMessageBytes: 10 << 20,
			maxBodyBytes:    256 << 10,
			maxParsedBytes:  384 << 10,
			wantBytes:       640 << 10,
		},
		{
			name:            "message budget",
			maxMessageBytes: 512 << 10,
			maxBodyBytes:    1 << 20,
			maxParsedBytes:  (1 << 20) + defaultMetadataResultBytes,
			wantBytes:       512 << 10,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &fakeIMAPSession{bodyByUID: map[uint32][]byte{1: rawMessage(1)}}
			messages, err := fetchMessages(
				session,
				[]uint32{1},
				test.maxMessageBytes,
				defaultMIMELimits(test.maxBodyBytes, test.maxParsedBytes),
				map[uint32][]int64{1: {1}},
				64<<20,
			)
			if err != nil {
				t.Fatal(err)
			}
			calls := bodyFetchCalls(session.calls())
			if len(messages) != 1 || len(calls) != 1 {
				t.Fatalf("messages = %d, body FETCH calls = %d", len(messages), len(calls))
			}
			section := bodyFetchSection(t, calls[0])
			if !section.Peek || !reflect.DeepEqual(section.Partial, []int{0, test.wantBytes + 1}) {
				t.Fatalf("body section = %#v, want PEEK partial 0.%d", section, test.wantBytes+1)
			}
		})
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
