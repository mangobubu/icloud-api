package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

type applyCall struct {
	accountID int64
	version   time.Time
	aliases   []domain.Alias
	result    domain.MailboxSyncResult
	syncedAt  time.Time
}

type failureCall struct {
	ctx       context.Context
	accountID int64
	version   time.Time
	message   string
	at        time.Time
}

type fakeRepo struct {
	mu sync.Mutex

	accounts   []domain.Account
	aliases    map[int64][]domain.Alias
	states     map[int64]domain.IMAPSyncState
	stateErrs  map[int64]error
	getErrs    map[int64]error
	applyErr   error
	failureErr error
	failureFn  func(context.Context, int64, time.Time, string, time.Time) error
	applies    []applyCall
	failures   []failureCall
	getCalls   int
}

func newFakeRepo(accounts ...domain.Account) *fakeRepo {
	return &fakeRepo{
		accounts:  accounts,
		aliases:   make(map[int64][]domain.Alias),
		states:    make(map[int64]domain.IMAPSyncState),
		stateErrs: make(map[int64]error),
		getErrs:   make(map[int64]error),
	}
}

func (f *fakeRepo) ListEnabledAccounts(context.Context) ([]domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Account(nil), f.accounts...), nil
}

func (f *fakeRepo) GetAccount(_ context.Context, accountID int64) (domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if err := f.getErrs[accountID]; err != nil {
		return domain.Account{}, err
	}
	for _, account := range f.accounts {
		if account.ID == accountID {
			return account, nil
		}
	}
	return domain.Account{}, store.ErrNotFound
}

func (f *fakeRepo) ListEnabledAliasesByAccount(_ context.Context, accountID int64) ([]domain.Alias, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	enabled := make([]domain.Alias, 0, len(f.aliases[accountID]))
	for _, alias := range f.aliases[accountID] {
		if alias.Enabled {
			enabled = append(enabled, alias)
		}
	}
	return enabled, nil
}

func (f *fakeRepo) GetIMAPSyncState(_ context.Context, accountID int64) (domain.IMAPSyncState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.stateErrs[accountID]; err != nil {
		return domain.IMAPSyncState{}, err
	}
	state, ok := f.states[accountID]
	if !ok {
		return domain.IMAPSyncState{}, store.ErrNotFound
	}
	return state, nil
}

func (f *fakeRepo) ApplyMailboxSync(_ context.Context, accountID int64, version time.Time, aliases []domain.Alias, result domain.MailboxSyncResult, syncedAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applies = append(f.applies, applyCall{
		accountID: accountID,
		version:   version,
		aliases:   append([]domain.Alias(nil), aliases...),
		result:    result,
		syncedAt:  syncedAt,
	})
	return f.applyErr
}

func (f *fakeRepo) RecordMailboxSyncFailure(ctx context.Context, accountID int64, version time.Time, message string, at time.Time) error {
	f.mu.Lock()
	f.failures = append(f.failures, failureCall{ctx: ctx, accountID: accountID, version: version, message: message, at: at})
	failureFn := f.failureFn
	failureErr := f.failureErr
	f.mu.Unlock()
	if failureFn != nil {
		return failureFn(ctx, accountID, version, message, at)
	}
	return failureErr
}

func (f *fakeRepo) applyCalls() []applyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]applyCall(nil), f.applies...)
}

func (f *fakeRepo) failureCalls() []failureCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]failureCall(nil), f.failures...)
}

func (f *fakeRepo) getAccountCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls
}

type cipherFunc func(string) (string, error)

func (f cipherFunc) Decrypt(value string) (string, error) { return f(value) }

type fetcherFunc func(context.Context, domain.Account, string, []domain.Alias, *domain.IMAPSyncState, map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error)

func (f fetcherFunc) FetchIncremental(ctx context.Context, account domain.Account, password string, aliases []domain.Alias, state *domain.IMAPSyncState, positions map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
	return f(ctx, account, password, aliases, state, positions)
}

func fixedCipher(_ string) (string, error) { return "app-password", nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSyncAccountPassesCursorAndAppliesOnce(t *testing.T) {
	accountVersion := time.Date(2026, 8, 8, 9, 0, 0, 123, time.UTC)
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted", UpdatedAt: accountVersion}
	repo := newFakeRepo(account)
	repo.aliases[account.ID] = []domain.Alias{
		{ID: 10, AccountID: account.ID, Address: "enabled@icloud.com", Enabled: true},
		{ID: 20, AccountID: account.ID, Address: "disabled@icloud.com", Enabled: false},
	}
	wantState := domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 7, LastUID: 41}
	repo.states[account.ID] = wantState
	wantResult := domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{
			10: {AliasID: 10, UIDValidity: 7, UID: 42, SnapshotState: domain.SnapshotFound},
		},
		State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 7, LastUID: 42},
	}
	fetchCalls := 0
	fetcher := fetcherFunc(func(_ context.Context, gotAccount domain.Account, password string, aliases []domain.Alias, state *domain.IMAPSyncState, positions map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		fetchCalls++
		if gotAccount.ID != account.ID || password != "app-password" {
			t.Fatalf("抓取参数错误: account=%#v password=%q", gotAccount, password)
		}
		if len(aliases) != 1 || aliases[0].ID != 10 {
			t.Fatalf("传给抓取器的启用隐私邮箱错误: %#v", aliases)
		}
		if state == nil || !reflect.DeepEqual(*state, wantState) {
			t.Fatalf("增量游标 = %#v, want %#v", state, wantState)
		}
		if positions != nil {
			t.Fatalf("mailbox snapshot positions = %#v, want nil", positions)
		}
		return wantResult, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	if err := manager.SyncAccount(context.Background(), account.ID); err != nil {
		t.Fatalf("同步主号: %v", err)
	}
	if fetchCalls != 1 {
		t.Fatalf("抓取次数 = %d, want 1", fetchCalls)
	}
	applies := repo.applyCalls()
	if len(applies) != 1 {
		t.Fatalf("批量提交次数 = %d, want 1", len(applies))
	}
	if applies[0].accountID != account.ID || !applies[0].version.Equal(accountVersion) ||
		len(applies[0].aliases) != 1 || applies[0].aliases[0].ID != 10 {
		t.Fatalf("批量提交目标错误: %#v", applies[0])
	}
	if !reflect.DeepEqual(applies[0].result, wantResult) {
		t.Fatalf("批量提交结果 = %#v, want %#v", applies[0].result, wantResult)
	}
	if applies[0].syncedAt.IsZero() || applies[0].syncedAt.Location() != time.UTC {
		t.Fatalf("批量提交时间无效: %v", applies[0].syncedAt)
	}
	if len(repo.failureCalls()) != 0 {
		t.Fatalf("成功同步不应记录失败: %#v", repo.failureCalls())
	}
}

func TestSyncAccountPassesNilCursorWhenStateIsMissing(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	stateWasNil := false
	fetcher := fetcherFunc(func(_ context.Context, _ domain.Account, _ string, _ []domain.Alias, state *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		stateWasNil = state == nil
		return domain.MailboxSyncResult{State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 3, LastUID: 9}}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	if err := manager.SyncAccount(context.Background(), account.ID); err != nil {
		t.Fatalf("首次同步主号: %v", err)
	}
	if !stateWasNil {
		t.Fatal("游标不存在时抓取器应收到 nil")
	}
	if len(repo.applyCalls()) != 1 {
		t.Fatalf("首次同步批量提交次数 = %d, want 1", len(repo.applyCalls()))
	}
}

func TestSyncAccountAppliesEmptyIncrementalResultForFreshness(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	state := domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 8, LastUID: 99}
	repo.states[account.ID] = state
	fetcher := fetcherFunc(func(_ context.Context, _ domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		return domain.MailboxSyncResult{Messages: map[int64]domain.LatestMessage{}, State: state}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	if err := manager.SyncAccount(context.Background(), account.ID); err != nil {
		t.Fatalf("无新增邮件同步: %v", err)
	}
	applies := repo.applyCalls()
	if len(applies) != 1 {
		t.Fatalf("无新增邮件仍应提交 freshness，实际次数 %d", len(applies))
	}
	if applies[0].syncedAt.IsZero() {
		t.Fatal("无新增邮件的提交缺少 freshness 时间")
	}
}

func TestSyncAccountReturnsPendingAfterCommittingOneBoundedBatch(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	previous := domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 8, LastUID: 99}
	repo.states[account.ID] = previous
	result := domain.MailboxSyncResult{
		Messages: map[int64]domain.LatestMessage{},
		State:    domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 8, LastUID: 112},
		HasMore:  true,
	}
	fetchCalls := 0
	fetcher := fetcherFunc(func(_ context.Context, _ domain.Account, _ string, _ []domain.Alias, state *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		fetchCalls++
		if state == nil || !reflect.DeepEqual(*state, previous) {
			t.Fatalf("cursor = %#v, want %#v", state, previous)
		}
		return result, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	err := manager.SyncAccount(context.Background(), account.ID)
	if !errors.Is(err, ErrSyncPending) {
		t.Fatalf("sync error = %v, want ErrSyncPending", err)
	}
	if fetchCalls != 1 {
		t.Fatalf("fetch calls = %d, want one IMAP batch", fetchCalls)
	}
	applies := repo.applyCalls()
	if len(applies) != 1 || !reflect.DeepEqual(applies[0].result, result) {
		t.Fatalf("committed batches = %#v, want one pending batch", applies)
	}
	if failures := repo.failureCalls(); len(failures) != 0 {
		t.Fatalf("pending continuation recorded as failure: %#v", failures)
	}
}

func TestQueueAccountSyncReturnsImmediatelyAndDeduplicates(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	fetchCalls := 0
	fetcher := fetcherFunc(func(context.Context, domain.Account, string, []domain.Alias, *domain.IMAPSyncState, map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		fetchCalls++
		if fetchCalls == 1 {
			close(started)
		}
		<-release
		return domain.MailboxSyncResult{
			Messages: map[int64]domain.LatestMessage{},
			State:    domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1},
		}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	queueResult := make(chan error, 1)
	go func() {
		queueResult <- manager.QueueAccountSync(context.Background(), account.ID)
	}()
	select {
	case err := <-queueResult:
		if !errors.Is(err, ErrSyncQueued) {
			t.Fatalf("first queue result = %v, want ErrSyncQueued", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queue call blocked on mailbox synchronization")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("queued sync did not start")
	}
	if err := manager.QueueAccountSync(context.Background(), account.ID); !errors.Is(err, ErrSyncQueued) {
		t.Fatalf("duplicate queue result = %v, want ErrSyncQueued", err)
	}

	releaseOnce.Do(func() { close(release) })
	manager.BeginShutdown()
	manager.waitForManualJobs()
	if fetchCalls != 1 {
		t.Fatalf("deduplicated fetch calls = %d, want 1", fetchCalls)
	}
	if applies := repo.applyCalls(); len(applies) != 1 {
		t.Fatalf("deduplicated apply calls = %d, want 1", len(applies))
	}
	if err := manager.QueueAccountSync(context.Background(), account.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("queue during shutdown = %v, want context canceled", err)
	}
}

func TestQueueAccountSyncShutdownWaitsForWorker(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	fetcher := fetcherFunc(func(context.Context, domain.Account, string, []domain.Alias, *domain.IMAPSyncState, map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		close(started)
		<-release
		return domain.MailboxSyncResult{
			Messages: map[int64]domain.LatestMessage{},
			State:    domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1},
		}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	if err := manager.QueueAccountSync(context.Background(), account.ID); !errors.Is(err, ErrSyncQueued) {
		t.Fatalf("queue result = %v, want ErrSyncQueued", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("queued sync did not start")
	}
	manager.BeginShutdown()
	waitDone := make(chan struct{})
	go func() {
		manager.waitForManualJobs()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		t.Fatal("shutdown wait returned before the queued sync released its resources")
	case <-time.After(25 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown wait did not return after the queued sync finished")
	}
}

func TestQueueAccountSyncWaitsForResourcesWithoutDropping(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	started := make(chan struct{})
	fetcher := fetcherFunc(func(context.Context, domain.Account, string, []domain.Alias, *domain.IMAPSyncState, map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		close(started)
		return domain.MailboxSyncResult{
			Messages: map[int64]domain.LatestMessage{},
			State:    domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1},
		}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)
	manager.SetSyncTimeout(20 * time.Millisecond)
	releaseBusy, err := manager.acquireAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var releaseBusyOnce sync.Once
	releaseBusyLock := func() { releaseBusyOnce.Do(releaseBusy) }
	defer releaseBusyLock()

	if err := manager.QueueAccountSync(context.Background(), account.ID); !errors.Is(err, ErrSyncQueued) {
		t.Fatalf("queue result = %v, want ErrSyncQueued", err)
	}
	select {
	case <-started:
		t.Fatal("queued sync bypassed the busy account lock")
	case <-time.After(60 * time.Millisecond):
	}
	releaseBusyLock()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("queued sync was dropped after waiting for the account lock")
	}
	manager.BeginShutdown()
	manager.waitForManualJobs()
	if calls := len(repo.applyCalls()); calls != 1 {
		t.Fatalf("queued sync apply calls = %d, want 1", calls)
	}
}

func TestQueueAccountSyncContinuesPendingBatches(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	fetchCalls := 0
	fetcher := fetcherFunc(func(context.Context, domain.Account, string, []domain.Alias, *domain.IMAPSyncState, map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		fetchCalls++
		return domain.MailboxSyncResult{
			Messages: map[int64]domain.LatestMessage{},
			State:    domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1, LastUID: uint32(fetchCalls)},
			HasMore:  fetchCalls == 1,
		}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	if err := manager.QueueAccountSync(context.Background(), account.ID); !errors.Is(err, ErrSyncQueued) {
		t.Fatalf("queue result = %v, want ErrSyncQueued", err)
	}
	manager.BeginShutdown()
	manager.waitForManualJobs()
	if fetchCalls != 2 {
		t.Fatalf("pending continuation fetch calls = %d, want 2", fetchCalls)
	}
	if applies := repo.applyCalls(); len(applies) != 2 || !applies[0].result.HasMore || applies[1].result.HasMore {
		t.Fatalf("pending continuation applies = %#v", applies)
	}
}

func TestQueuedManualSyncProgressSpansPendingBatches(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseSecond) })
	var manager *Manager
	var callsMu sync.Mutex
	calls := 0
	firstProgress := make(chan domain.MailboxSyncProgress, 1)
	fetcher := fetcherFunc(func(ctx context.Context, _ domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		domain.ReportMailboxSyncProgress(ctx, domain.MailboxSyncPhaseConnecting, 5)
		if call == 1 {
			progress, _ := manager.AccountProgress(account.ID)
			firstProgress <- progress
			return domain.MailboxSyncResult{
				State:     domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1, LastUID: 25},
				HasMore:   true,
				TargetUID: 100,
			}, nil
		}
		close(secondStarted)
		<-releaseSecond
		return domain.MailboxSyncResult{
			State:     domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1, LastUID: 100},
			TargetUID: 100,
		}, nil
	})
	manager = New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	if err := manager.QueueAccountSync(context.Background(), account.ID); !errors.Is(err, ErrSyncQueued) {
		t.Fatalf("queue result = %v, want ErrSyncQueued", err)
	}
	first := <-firstProgress
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second manual batch did not start")
	}
	second, ok := manager.AccountProgress(account.ID)
	if !ok {
		t.Fatal("manual continuation omitted active progress")
	}
	if first.Trigger != domain.MailboxSyncTriggerManual || second.Trigger != domain.MailboxSyncTriggerManual {
		t.Fatalf("manual progress triggers = %q/%q", first.Trigger, second.Trigger)
	}
	if !second.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("manual continuation restarted progress: first=%v second=%v", first.StartedAt, second.StartedAt)
	}
	if second.Percent < first.Percent || second.Percent < 25 || second.Phase != domain.MailboxSyncPhaseConnecting {
		t.Fatalf("manual continuation progress = %#v after %#v", second, first)
	}
	releaseOnce.Do(func() { close(releaseSecond) })
	manager.BeginShutdown()
	manager.waitForManualJobs()
	if progress, ok := manager.AccountProgress(account.ID); ok {
		t.Fatalf("completed manual progress was retained: %#v", progress)
	}
}

func TestSyncAccountFailureUsesOneBulkRecord(t *testing.T) {
	accountVersion := time.Date(2026, 8, 8, 9, 30, 0, 456, time.UTC)
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted", UpdatedAt: accountVersion}
	repo := newFakeRepo(account)
	for id := int64(1); id <= 165; id++ {
		repo.aliases[account.ID] = append(repo.aliases[account.ID], domain.Alias{ID: id, AccountID: account.ID, Enabled: true})
	}
	wantErr := errors.New("context deadline exceeded")
	fetcher := fetcherFunc(func(context.Context, domain.Account, string, []domain.Alias, *domain.IMAPSyncState, map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		return domain.MailboxSyncResult{}, wantErr
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	err := manager.SyncAccount(context.Background(), account.ID)
	if !errors.Is(err, wantErr) {
		t.Fatalf("同步错误 = %v, want %v", err, wantErr)
	}
	wantMessage := "fetch IMAP mailbox increment: " + wantErr.Error()
	if err.Error() != wantMessage {
		t.Fatalf("sync error = %q, want %q", err, wantMessage)
	}
	if len(repo.applyCalls()) != 0 {
		t.Fatalf("抓取失败后不应提交结果: %#v", repo.applyCalls())
	}
	failures := repo.failureCalls()
	if len(failures) != 1 {
		t.Fatalf("165 个隐私邮箱的失败写入次数 = %d, want 1", len(failures))
	}
	if failures[0].accountID != account.ID || !failures[0].version.Equal(accountVersion) ||
		failures[0].message != wantMessage || failures[0].at.IsZero() {
		t.Fatalf("批量失败记录错误: %#v", failures[0])
	}
}

func TestFailureRecorderSkipsProcessShutdownCancellation(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, UpdatedAt: time.Unix(10, 0).UTC()}
	repo := newFakeRepo(account)
	manager := New(repo, cipherFunc(fixedCipher), nil, discardLogger(), time.Minute, 1)
	manager.BeginShutdown()
	shutdownContext, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := newFailureRecorder(manager, shutdownContext, account.ID, account.UpdatedAt)
	recorder.record(context.Canceled)
	if failures := repo.failureCalls(); len(failures) != 0 {
		t.Fatalf("shutdown cancellation recorded failures = %#v, want none", failures)
	}
}

func TestSyncAccountApplyFailureUsesBulkRecord(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	repo.applyErr = errors.New("transaction failed")
	fetcher := fetcherFunc(func(context.Context, domain.Account, string, []domain.Alias, *domain.IMAPSyncState, map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		return domain.MailboxSyncResult{State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 2, LastUID: 4}}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	err := manager.SyncAccount(context.Background(), account.ID)
	if err == nil || !errors.Is(err, repo.applyErr) {
		t.Fatalf("批量提交错误 = %v, want wrapped %v", err, repo.applyErr)
	}
	if len(repo.applyCalls()) != 1 || len(repo.failureCalls()) != 1 {
		t.Fatalf("提交/失败记录次数错误: apply=%d failure=%d", len(repo.applyCalls()), len(repo.failureCalls()))
	}
}

func TestSyncAccountCursorReadFailureUsesBulkRecord(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	wantErr := errors.New("read cursor failed")
	repo.stateErrs[account.ID] = wantErr
	fetchCalls := 0
	fetcher := fetcherFunc(func(context.Context, domain.Account, string, []domain.Alias, *domain.IMAPSyncState, map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		fetchCalls++
		return domain.MailboxSyncResult{}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	err := manager.SyncAccount(context.Background(), account.ID)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("游标读取错误 = %v, want wrapped %v", err, wantErr)
	}
	if fetchCalls != 0 || len(repo.applyCalls()) != 0 {
		t.Fatalf("游标读取失败后仍执行后续操作: fetch=%d apply=%d", fetchCalls, len(repo.applyCalls()))
	}
	if len(repo.failureCalls()) != 1 {
		t.Fatalf("游标读取失败的批量记录次数 = %d, want 1", len(repo.failureCalls()))
	}
}

func TestSyncAccountGetFailureDoesNotRecordWithoutAccountVersion(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	wantErr := errors.New("read account failed")
	repo.getErrs[account.ID] = wantErr
	manager := New(repo, cipherFunc(fixedCipher), nil, discardLogger(), time.Minute, 1)

	err := manager.SyncAccount(context.Background(), account.ID)
	if !errors.Is(err, wantErr) {
		t.Fatalf("读取主号错误 = %v, want %v", err, wantErr)
	}
	failures := repo.failureCalls()
	if len(failures) != 0 {
		t.Fatalf("读取主号失败时缺少 CAS 版本，不应写入失败状态: %#v", failures)
	}
}

func TestSyncAccountWithTimeoutCancelsFetcherAndRecordsFailure(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	recordContextActive := false
	repo.failureFn = func(ctx context.Context, _ int64, _ time.Time, _ string, _ time.Time) error {
		recordContextActive = ctx.Err() == nil
		return nil
	}
	fetcher := fetcherFunc(func(ctx context.Context, _ domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		<-ctx.Done()
		return domain.MailboxSyncResult{}, ctx.Err()
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)
	manager.SetSyncTimeout(20 * time.Millisecond)

	err := manager.SyncAccountWithTimeout(context.Background(), account.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("同步超时错误 = %v, want %v", err, context.DeadlineExceeded)
	}
	wantMessage := "fetch IMAP mailbox increment: " + context.DeadlineExceeded.Error()
	if err.Error() != wantMessage {
		t.Fatalf("sync timeout error = %q, want %q", err, wantMessage)
	}
	failures := repo.failureCalls()
	if len(failures) != 1 || failures[0].message != wantMessage || !recordContextActive {
		t.Fatalf("超时后的批量失败记录错误: calls=%#v active=%v", failures, recordContextActive)
	}
}

func TestSyncAccountWithTimeoutStartsFreshWorkBudgetAfterQueueing(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	remainingBudget := make(chan time.Duration, 1)
	fetcher := fetcherFunc(func(ctx context.Context, _ domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			remainingBudget <- 0
		} else {
			remainingBudget <- time.Until(deadline)
		}
		return domain.MailboxSyncResult{State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1}}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)
	const syncTimeout = 300 * time.Millisecond
	manager.SetSyncTimeout(syncTimeout)

	var timeoutMu sync.Mutex
	timeoutCalls := 0
	waitBudgetStarted := make(chan struct{})
	manager.withTimeout = func(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
		timeoutMu.Lock()
		timeoutCalls++
		call := timeoutCalls
		timeoutMu.Unlock()
		if call == 1 {
			close(waitBudgetStarted)
		}
		return context.WithTimeout(ctx, timeout)
	}

	releaseAccount, err := manager.acquireAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var releaseAccountOnce sync.Once
	releaseAccountLock := func() { releaseAccountOnce.Do(releaseAccount) }
	defer releaseAccountLock()
	releaseSlot, err := manager.acquireSyncSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var releaseSlotOnce sync.Once
	releaseGlobalSlot := func() { releaseSlotOnce.Do(releaseSlot) }
	defer releaseGlobalSlot()

	done := make(chan error, 1)
	go func() { done <- manager.SyncAccountWithTimeout(context.Background(), account.ID) }()
	select {
	case <-waitBudgetStarted:
	case <-time.After(time.Second):
		t.Fatal("manual sync did not start its queue wait budget")
	}

	time.Sleep(60 * time.Millisecond)
	releaseAccountLock()
	time.Sleep(60 * time.Millisecond)
	timeoutMu.Lock()
	callsWhileWaitingForSlot := timeoutCalls
	timeoutMu.Unlock()
	if callsWhileWaitingForSlot != 1 {
		t.Fatalf("timeout budgets before global slot acquisition = %d, want one shared wait budget", callsWhileWaitingForSlot)
	}
	releaseGlobalSlot()

	var remaining time.Duration
	select {
	case remaining = <-remainingBudget:
	case <-time.After(time.Second):
		t.Fatal("fetch did not start after account lock and global slot were released")
	}
	if err := <-done; err != nil {
		t.Fatalf("manual sync after queueing: %v", err)
	}
	if remaining < 240*time.Millisecond {
		t.Fatalf("work budget after queueing = %v, want at least 240ms of %v", remaining, syncTimeout)
	}
	timeoutMu.Lock()
	finalTimeoutCalls := timeoutCalls
	timeoutMu.Unlock()
	if finalTimeoutCalls != 2 {
		t.Fatalf("manual sync timeout budgets = %d, want wait and work budgets", finalTimeoutCalls)
	}
}

func TestSyncAllStartsFreshWorkBudgetAfterSlotQueueing(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	remainingBudget := make(chan time.Duration, 1)
	fetcher := fetcherFunc(func(ctx context.Context, _ domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			remainingBudget <- 0
		} else {
			remainingBudget <- time.Until(deadline)
		}
		return domain.MailboxSyncResult{State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1}}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)
	const syncTimeout = 300 * time.Millisecond
	manager.SetSyncTimeout(syncTimeout)

	var timeoutMu sync.Mutex
	timeoutCalls := 0
	waitBudgetStarted := make(chan struct{})
	manager.withTimeout = func(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
		timeoutMu.Lock()
		timeoutCalls++
		call := timeoutCalls
		timeoutMu.Unlock()
		if call == 1 {
			close(waitBudgetStarted)
		}
		return context.WithTimeout(ctx, timeout)
	}

	releaseSlot, err := manager.acquireSyncSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var releaseSlotOnce sync.Once
	releaseGlobalSlot := func() { releaseSlotOnce.Do(releaseSlot) }
	defer releaseGlobalSlot()

	roundDone := make(chan struct{})
	go func() {
		manager.syncAll(context.Background())
		close(roundDone)
	}()
	select {
	case <-waitBudgetStarted:
	case <-time.After(time.Second):
		t.Fatal("periodic sync did not start its slot wait budget")
	}
	time.Sleep(120 * time.Millisecond)
	releaseGlobalSlot()

	var remaining time.Duration
	select {
	case remaining = <-remainingBudget:
	case <-time.After(time.Second):
		t.Fatal("periodic fetch did not start after the global slot was released")
	}
	select {
	case <-roundDone:
	case <-time.After(time.Second):
		t.Fatal("periodic sync round did not finish")
	}
	if remaining < 240*time.Millisecond {
		t.Fatalf("periodic work budget after slot queueing = %v, want at least 240ms of %v", remaining, syncTimeout)
	}
	timeoutMu.Lock()
	finalTimeoutCalls := timeoutCalls
	timeoutMu.Unlock()
	if finalTimeoutCalls != 2 {
		t.Fatalf("periodic sync timeout budgets = %d, want wait and work budgets", finalTimeoutCalls)
	}
}

func TestFailureRecorderRetriesWithOneThreeSecondFallback(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	repo := newFakeRepo()
	var contexts []context.Context
	repo.failureFn = func(ctx context.Context, _ int64, _ time.Time, _ string, _ time.Time) error {
		contexts = append(contexts, ctx)
		if len(contexts) == 1 {
			cancelParent()
			return context.Canceled
		}
		return nil
	}
	manager := New(repo, cipherFunc(fixedCipher), nil, discardLogger(), time.Minute, 1)
	created := 0
	var fallback context.Context
	manager.withTimeout = func(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
		if timeout != 3*time.Second {
			t.Fatalf("失败状态兜底时限 = %v, want 3s", timeout)
		}
		created++
		var cancel context.CancelFunc
		fallback, cancel = context.WithCancel(ctx)
		return fallback, cancel
	}
	recorder := newFailureRecorder(manager, parent, 1, time.Unix(0, 1).UTC())
	defer recorder.close()

	recorder.record(errors.New("sync failed"))
	if created != 1 {
		t.Fatalf("兜底 context 创建次数 = %d, want 1", created)
	}
	if len(contexts) != 2 || contexts[0] != parent || contexts[1] != fallback {
		t.Fatalf("失败记录 context 序列错误: %#v", contexts)
	}
}

func TestFailureRecorderKeepsDetailedUnicodeErrorWithinBound(t *testing.T) {
	repo := newFakeRepo()
	manager := New(repo, cipherFunc(fixedCipher), nil, discardLogger(), time.Minute, 1)
	recorder := newFailureRecorder(manager, context.Background(), 1, time.Unix(0, 1).UTC())
	defer recorder.close()

	recorder.record(errors.New(strings.Repeat("错", maxPersistedSyncErrorRunes+100)))
	failures := repo.failureCalls()
	if len(failures) != 1 {
		t.Fatalf("失败记录次数 = %d, want 1", len(failures))
	}
	if !utf8.ValidString(failures[0].message) {
		t.Fatalf("截断后的错误文本不是有效 UTF-8: %q", failures[0].message)
	}
	if got := utf8.RuneCountInString(failures[0].message); got != maxPersistedSyncErrorRunes {
		t.Fatalf("截断后字符数 = %d, want %d", got, maxPersistedSyncErrorRunes)
	}
}

func TestAccountLockSerializesConfigurationWithSync(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	fetchStarted := make(chan struct{}, 1)
	fetcher := fetcherFunc(func(context.Context, domain.Account, string, []domain.Alias, *domain.IMAPSyncState, map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		fetchStarted <- struct{}{}
		return domain.MailboxSyncResult{}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)
	lockEntered := make(chan struct{})
	releaseLock := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- manager.WithAccountLock(context.Background(), account.ID, func() error {
			close(lockEntered)
			<-releaseLock
			return nil
		})
	}()
	<-lockEntered

	syncDone := make(chan error, 1)
	go func() { syncDone <- manager.SyncAccount(context.Background(), account.ID) }()
	select {
	case <-fetchStarted:
		t.Fatal("同步越过了主号配置锁")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseLock)
	if err := <-mutationDone; err != nil {
		t.Fatalf("配置操作失败: %v", err)
	}
	select {
	case <-fetchStarted:
	case <-time.After(time.Second):
		t.Fatal("释放配置锁后同步未启动")
	}
	if err := <-syncDone; err != nil {
		t.Fatalf("释放配置锁后的同步失败: %v", err)
	}
}

func TestSyncAllSkipsBusyAccountWithoutBlockingOtherAccounts(t *testing.T) {
	busy := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted-1"}
	other := domain.Account{ID: 2, Enabled: true, PasswordCiphertext: "encrypted-2"}
	repo := newFakeRepo(busy, other)
	fetched := make(chan int64, 2)
	fetcher := fetcherFunc(func(_ context.Context, account domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		fetched <- account.ID
		return domain.MailboxSyncResult{
			Messages: map[int64]domain.LatestMessage{},
			State:    domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1},
		}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	manualEntered := make(chan struct{})
	releaseManual := make(chan struct{})
	manualDone := make(chan error, 1)
	go func() {
		manualDone <- manager.WithAccountLock(context.Background(), busy.ID, func() error {
			close(manualEntered)
			<-releaseManual
			return nil
		})
	}()
	<-manualEntered

	roundDone := make(chan struct{})
	go func() {
		manager.syncAll(context.Background())
		close(roundDone)
	}()
	select {
	case <-roundDone:
	case <-time.After(time.Second):
		close(releaseManual)
		<-manualDone
		t.Fatal("周期同步等待了已被手动操作占用的主号")
	}

	select {
	case accountID := <-fetched:
		if accountID != other.ID {
			t.Fatalf("周期同步抓取了忙碌主号 %d", accountID)
		}
	default:
		t.Fatal("空闲主号未执行周期同步")
	}
	select {
	case accountID := <-fetched:
		t.Fatalf("周期同步发生额外抓取: account_id=%d", accountID)
	default:
	}
	if applies := repo.applyCalls(); len(applies) != 1 || applies[0].accountID != other.ID {
		t.Fatalf("周期同步提交 = %#v", applies)
	}

	close(releaseManual)
	if err := <-manualDone; err != nil {
		t.Fatalf("手动主号操作失败: %v", err)
	}
}

func TestManualAndPeriodicSyncsShareGlobalConcurrencyWithoutBlockingOnAccountLock(t *testing.T) {
	busy := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted-1"}
	other := domain.Account{ID: 2, Enabled: true, PasswordCiphertext: "encrypted-2"}
	repo := newFakeRepo(busy, other)

	periodicStarted := make(chan struct{})
	releasePeriodic := make(chan struct{})
	manualStarted := make(chan struct{})
	var activeMu sync.Mutex
	active := 0
	maxActive := 0
	fetcher := fetcherFunc(func(_ context.Context, account domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		activeMu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		activeMu.Unlock()
		defer func() {
			activeMu.Lock()
			active--
			activeMu.Unlock()
		}()

		switch account.ID {
		case busy.ID:
			close(manualStarted)
		case other.ID:
			close(periodicStarted)
			<-releasePeriodic
		}
		return domain.MailboxSyncResult{
			Messages: map[int64]domain.LatestMessage{},
			State:    domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1},
		}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Minute, 1)

	releaseConfiguration, err := manager.acquireAccount(context.Background(), busy.ID)
	if err != nil {
		t.Fatal(err)
	}
	var releaseConfigurationOnce sync.Once
	releaseConfigurationLock := func() { releaseConfigurationOnce.Do(releaseConfiguration) }
	defer releaseConfigurationLock()

	manualCtx, cancelManual := context.WithCancel(context.Background())
	defer cancelManual()
	manualDone := make(chan error, 1)
	go func() { manualDone <- manager.SyncAccount(manualCtx, busy.ID) }()
	deadline := time.Now().Add(time.Second)
	for {
		manager.locksMu.Lock()
		lock := manager.locks[busy.ID]
		waiterRegistered := lock != nil && lock.refs >= 2
		manager.locksMu.Unlock()
		if waiterRegistered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("手动同步未进入主号锁等待队列")
		}
		time.Sleep(time.Millisecond)
	}

	roundDone := make(chan struct{})
	go func() {
		manager.syncAll(context.Background())
		close(roundDone)
	}()
	select {
	case <-periodicStarted:
	case <-time.After(time.Second):
		t.Fatal("等待主号锁的手动同步占用了全局并发槽")
	}
	select {
	case <-manualStarted:
		t.Fatal("手动同步越过了主号配置锁")
	default:
	}

	releaseConfigurationLock()
	select {
	case <-manualStarted:
		t.Fatal("手动同步在周期同步释放全局并发槽前启动")
	case <-time.After(30 * time.Millisecond):
	}
	close(releasePeriodic)

	select {
	case <-manualStarted:
	case <-time.After(time.Second):
		t.Fatal("周期同步释放并发槽后手动同步未启动")
	}
	if err := <-manualDone; err != nil {
		t.Fatalf("手动同步失败: %v", err)
	}
	select {
	case <-roundDone:
	case <-time.After(time.Second):
		t.Fatal("周期同步轮次未结束")
	}
	activeMu.Lock()
	observedMax := maxActive
	activeMu.Unlock()
	if observedMax != 1 {
		t.Fatalf("手动和周期 IMAP 同步最大并发 = %d, want 1", observedMax)
	}
}

func TestWithAccountIMAPSlotAcquiresAccountLockBeforeGlobalSlot(t *testing.T) {
	manager := New(newFakeRepo(), cipherFunc(fixedCipher), nil, discardLogger(), time.Minute, 1)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- manager.WithAccountIMAPSlot(context.Background(), 1, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- manager.WithAccountIMAPSlot(context.Background(), 2, func() error {
			close(secondEntered)
			return nil
		})
	}()

	deadline := time.Now().Add(time.Second)
	for {
		manager.locksMu.Lock()
		secondHoldsAccountLock := manager.locks[2] != nil
		manager.locksMu.Unlock()
		if secondHoldsAccountLock {
			break
		}
		if time.Now().After(deadline) {
			close(releaseFirst)
			t.Fatal("second IMAP operation did not acquire its account lock while waiting for the global slot")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-secondEntered:
		close(releaseFirst)
		t.Fatal("second IMAP operation exceeded the global concurrency limit")
	case <-time.After(30 * time.Millisecond):
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first IMAP operation: %v", err)
	}
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second IMAP operation did not start after the global slot was released")
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second IMAP operation: %v", err)
	}
}

func TestWithAccountIMAPSlotRejectsNilOperation(t *testing.T) {
	manager := New(newFakeRepo(), cipherFunc(fixedCipher), nil, discardLogger(), time.Minute, 1)
	if err := manager.WithAccountIMAPSlot(context.Background(), 1, nil); err == nil {
		t.Fatal("nil account IMAP operation succeeded")
	}
}

func TestCanceledAccountLockWaiterDoesNotPreventReclamation(t *testing.T) {
	manager := New(newFakeRepo(), cipherFunc(fixedCipher), nil, discardLogger(), time.Minute, 1)
	release, err := manager.acquireAccount(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.acquireAccount(canceled, 7); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消等待者错误 = %v, want %v", err, context.Canceled)
	}
	release()
	release()

	manager.locksMu.Lock()
	remaining := len(manager.locks)
	manager.locksMu.Unlock()
	if remaining != 0 {
		t.Fatalf("最后持有者释放后锁表大小 = %d, want 0", remaining)
	}
}

func TestCanceledContextDoesNotEnterAccountPipeline(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	manager := New(repo, cipherFunc(fixedCipher), nil, discardLogger(), time.Minute, 1)

	for attempt := 0; attempt < 64; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := manager.SyncAccount(ctx, account.ID)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("第 %d 次取消同步错误 = %v, want %v", attempt+1, err, context.Canceled)
		}
	}
	if calls := repo.getAccountCalls(); calls != 0 {
		t.Fatalf("已取消同步仍读取主号 %d 次", calls)
	}
	manager.locksMu.Lock()
	remaining := len(manager.locks)
	manager.locksMu.Unlock()
	if remaining != 0 {
		t.Fatalf("已取消同步后锁表大小 = %d, want 0", remaining)
	}
}

func TestRunDrainsPendingBatchesBeforeWaiting(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	callEvents := make(chan int, 4)
	var callsMu sync.Mutex
	calls := 0
	fetcher := fetcherFunc(func(_ context.Context, got domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		callEvents <- call
		return domain.MailboxSyncResult{
			State:   domain.IMAPSyncState{AccountID: got.ID, UIDValidity: 1, LastUID: uint32(call)},
			HasMore: call < 3,
		}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), 10*time.Minute, 1)
	waitEvents := make(chan time.Duration, 2)
	manager.waitInterval = func(_ context.Context, interval time.Duration) bool {
		waitEvents <- interval
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		manager.Run(ctx)
		close(done)
	}()

	for want := 1; want <= 3; want++ {
		select {
		case got := <-callEvents:
			if got != want {
				t.Fatalf("batch call = %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("waiting for batch %d", want)
		}
	}
	select {
	case interval := <-waitEvents:
		if interval != 10*time.Minute {
			t.Fatalf("wait interval = %v, want %v", interval, 10*time.Minute)
		}
	case <-time.After(time.Second):
		t.Fatal("final batch did not enter the interval wait")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("periodic sync did not stop after the interval hook returned false")
	}
	callsMu.Lock()
	gotCalls := calls
	callsMu.Unlock()
	if gotCalls != 3 {
		t.Fatalf("batch calls = %d, want 3", gotCalls)
	}
}

func TestRunAutomaticProgressSpansPendingBatches(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseSecond) })
	var manager *Manager
	var callsMu sync.Mutex
	calls := 0
	firstProgress := make(chan domain.MailboxSyncProgress, 1)
	fetcher := fetcherFunc(func(ctx context.Context, _ domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		domain.ReportMailboxSyncProgress(ctx, domain.MailboxSyncPhaseScanning, 15)
		if call == 1 {
			progress, _ := manager.AccountProgress(account.ID)
			firstProgress <- progress
			return domain.MailboxSyncResult{
				State:     domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 2, LastUID: 40},
				HasMore:   true,
				TargetUID: 100,
			}, nil
		}
		close(secondStarted)
		<-releaseSecond
		return domain.MailboxSyncResult{
			State:     domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 2, LastUID: 100},
			TargetUID: 100,
		}, nil
	})
	manager = New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Hour, 1)
	manager.waitInterval = func(context.Context, time.Duration) bool { return false }
	done := make(chan struct{})
	go func() {
		manager.Run(context.Background())
		close(done)
	}()

	first := <-firstProgress
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second automatic batch did not start")
	}
	second, ok := manager.AccountProgress(account.ID)
	if !ok {
		t.Fatal("automatic continuation omitted active progress")
	}
	if first.Trigger != domain.MailboxSyncTriggerAutomatic || second.Trigger != domain.MailboxSyncTriggerAutomatic {
		t.Fatalf("automatic progress triggers = %q/%q", first.Trigger, second.Trigger)
	}
	if !second.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("automatic continuation restarted progress: first=%v second=%v", first.StartedAt, second.StartedAt)
	}
	if second.Percent < first.Percent || second.Percent < 25 || second.Phase != domain.MailboxSyncPhaseScanning {
		t.Fatalf("automatic continuation progress = %#v after %#v", second, first)
	}
	releaseOnce.Do(func() { close(releaseSecond) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("automatic sync did not finish")
	}
	if progress, ok := manager.AccountProgress(account.ID); ok {
		t.Fatalf("completed automatic progress was retained: %#v", progress)
	}
}

func TestRunDrainsPendingBatchesFairlyAcrossAccounts(t *testing.T) {
	accounts := []domain.Account{
		{ID: 1, Enabled: true, PasswordCiphertext: "encrypted-1"},
		{ID: 2, Enabled: true, PasswordCiphertext: "encrypted-2"},
	}
	repo := newFakeRepo(accounts...)
	var callsMu sync.Mutex
	calls := make(map[int64]int)
	order := make([]int64, 0, 4)
	fetcher := fetcherFunc(func(_ context.Context, account domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		callsMu.Lock()
		calls[account.ID]++
		call := calls[account.ID]
		order = append(order, account.ID)
		callsMu.Unlock()
		return domain.MailboxSyncResult{
			State:   domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1, LastUID: uint32(call)},
			HasMore: call == 1,
		}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), 10*time.Minute, 1)
	waitCalls := 0
	manager.waitInterval = func(_ context.Context, _ time.Duration) bool {
		waitCalls++
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		manager.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fair periodic sync did not finish")
	}

	callsMu.Lock()
	gotCalls := map[int64]int{1: calls[1], 2: calls[2]}
	gotOrder := append([]int64(nil), order...)
	callsMu.Unlock()
	if !reflect.DeepEqual(gotCalls, map[int64]int{1: 2, 2: 2}) {
		t.Fatalf("per-account batch calls = %#v, want two each", gotCalls)
	}
	if len(gotOrder) != 4 || gotOrder[0] == gotOrder[1] {
		t.Fatalf("batch order = %#v, want one batch for each account before round two", gotOrder)
	}
	if gotOrder[2] == gotOrder[3] {
		t.Fatalf("batch order = %#v, want one batch for each account in round two", gotOrder)
	}
	if waitCalls != 1 {
		t.Fatalf("interval waits = %d, want one after all pending batches drain", waitCalls)
	}
}

func TestRunDoesNotResyncCaughtUpAccountDuringContinuation(t *testing.T) {
	accounts := []domain.Account{
		{ID: 1, Enabled: true, PasswordCiphertext: "encrypted-1"},
		{ID: 2, Enabled: true, PasswordCiphertext: "encrypted-2"},
	}
	repo := newFakeRepo(accounts...)
	var callsMu sync.Mutex
	calls := make(map[int64]int)
	fetcher := fetcherFunc(func(_ context.Context, account domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		callsMu.Lock()
		calls[account.ID]++
		call := calls[account.ID]
		callsMu.Unlock()
		return domain.MailboxSyncResult{
			State:   domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1, LastUID: uint32(call)},
			HasMore: account.ID == 1 && call == 1,
		}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Hour, 1)
	waitCalls := 0
	manager.waitInterval = func(_ context.Context, _ time.Duration) bool {
		waitCalls++
		return false
	}
	done := make(chan struct{})
	go func() {
		manager.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sync run did not finish after the pending account caught up")
	}
	callsMu.Lock()
	gotCalls := map[int64]int{1: calls[1], 2: calls[2]}
	callsMu.Unlock()
	if !reflect.DeepEqual(gotCalls, map[int64]int{1: 2, 2: 1}) {
		t.Fatalf("per-account batch calls = %#v, want only the pending account in the continuation round", gotCalls)
	}
	if waitCalls != 1 {
		t.Fatalf("interval waits = %d, want one after the pending account caught up", waitCalls)
	}
}

func TestRunContinuesOtherPendingAfterOrdinaryFailure(t *testing.T) {
	accounts := []domain.Account{
		{ID: 1, Enabled: true, PasswordCiphertext: "encrypted-1"},
		{ID: 2, Enabled: true, PasswordCiphertext: "encrypted-2"},
	}
	repo := newFakeRepo(accounts...)
	var callsMu sync.Mutex
	calls := make(map[int64]int)
	fetcher := fetcherFunc(func(_ context.Context, account domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		callsMu.Lock()
		calls[account.ID]++
		call := calls[account.ID]
		callsMu.Unlock()
		if account.ID == 2 {
			return domain.MailboxSyncResult{}, errors.New("transient IMAP failure")
		}
		return domain.MailboxSyncResult{
			State:   domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1, LastUID: uint32(call)},
			HasMore: call == 1,
		}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Hour, 2)
	waitCalls := 0
	manager.waitInterval = func(_ context.Context, _ time.Duration) bool {
		waitCalls++
		return false
	}
	done := make(chan struct{})
	go func() {
		manager.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sync run did not stop after the interval hook returned false")
	}
	callsMu.Lock()
	gotCalls := map[int64]int{1: calls[1], 2: calls[2]}
	callsMu.Unlock()
	if !reflect.DeepEqual(gotCalls, map[int64]int{1: 2, 2: 1}) {
		t.Fatalf("calls after mixed pending/error round = %#v, want pending account to continue alone", gotCalls)
	}
	if waitCalls != 1 {
		t.Fatalf("interval waits after healthy continuation drained = %d, want 1", waitCalls)
	}
}

func TestRunPreservesPendingContinuationWhileAccountBusy(t *testing.T) {
	t.Run("lock released within queue budget", func(t *testing.T) {
		account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
		repo := newFakeRepo(account)
		firstFetchStarted := make(chan struct{})
		releaseFirstFetch := make(chan struct{})
		var releaseFirstOnce sync.Once
		releaseFirst := func() { releaseFirstOnce.Do(func() { close(releaseFirstFetch) }) }
		defer releaseFirst()
		secondFetchStarted := make(chan struct{})
		var callsMu sync.Mutex
		calls := 0
		fetcher := fetcherFunc(func(_ context.Context, got domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
			callsMu.Lock()
			calls++
			call := calls
			callsMu.Unlock()
			switch call {
			case 1:
				close(firstFetchStarted)
				<-releaseFirstFetch
			case 2:
				close(secondFetchStarted)
			}
			return domain.MailboxSyncResult{
				State:   domain.IMAPSyncState{AccountID: got.ID, UIDValidity: 1, LastUID: uint32(call)},
				HasMore: call == 1,
			}, nil
		})
		manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Hour, 1)
		manager.SetSyncTimeout(time.Second)
		waitEvents := make(chan struct{}, 2)
		manager.waitInterval = func(_ context.Context, _ time.Duration) bool {
			waitEvents <- struct{}{}
			return false
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			manager.Run(ctx)
			close(done)
		}()
		select {
		case <-firstFetchStarted:
		case <-time.After(time.Second):
			t.Fatal("first pending batch did not start")
		}

		manualEntered := make(chan struct{})
		releaseManual := make(chan struct{})
		var releaseManualOnce sync.Once
		releaseManualLock := func() { releaseManualOnce.Do(func() { close(releaseManual) }) }
		defer releaseManualLock()
		manualDone := make(chan error, 1)
		go func() {
			manualDone <- manager.WithAccountLock(ctx, account.ID, func() error {
				close(manualEntered)
				<-releaseManual
				return nil
			})
		}()
		deadline := time.Now().Add(time.Second)
		for {
			manager.locksMu.Lock()
			lock := manager.locks[account.ID]
			manualQueued := lock != nil && lock.refs >= 2
			manager.locksMu.Unlock()
			if manualQueued {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("manual operation did not queue behind the first batch")
			}
			time.Sleep(time.Millisecond)
		}
		releaseFirst()
		select {
		case <-manualEntered:
		case <-time.After(time.Second):
			t.Fatal("manual operation did not acquire the account lock")
		}
		select {
		case <-secondFetchStarted:
			t.Fatal("pending continuation bypassed the busy account lock")
		case <-time.After(30 * time.Millisecond):
		}
		select {
		case <-waitEvents:
			t.Fatal("busy pending continuation waited for the full poll interval")
		default:
		}

		releaseManualLock()
		if err := <-manualDone; err != nil {
			t.Fatalf("manual account operation: %v", err)
		}
		select {
		case <-secondFetchStarted:
		case <-time.After(time.Second):
			t.Fatal("pending continuation did not resume after the account lock was released")
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("sync run did not finish after the continuation drained")
		}
		callsMu.Lock()
		gotCalls := calls
		callsMu.Unlock()
		if gotCalls != 2 {
			t.Fatalf("batch calls = %d, want 2", gotCalls)
		}
		if len(waitEvents) != 1 {
			t.Fatalf("interval waits = %d, want one final wait", len(waitEvents))
		}
	})

	t.Run("queue timeout defers to normal poll", func(t *testing.T) {
		account := domain.Account{
			ID:                 1,
			Enabled:            true,
			PasswordCiphertext: "encrypted",
			LastSyncStatus:     domain.SyncStatusPending,
		}
		repo := newFakeRepo(account)
		fetchCalls := 0
		fetcher := fetcherFunc(func(_ context.Context, got domain.Account, _ string, _ []domain.Alias, _ *domain.IMAPSyncState, _ map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
			fetchCalls++
			return domain.MailboxSyncResult{
				State: domain.IMAPSyncState{AccountID: got.ID, UIDValidity: 1, LastUID: 1},
			}, nil
		})
		manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Hour, 1)
		manager.SetSyncTimeout(40 * time.Millisecond)
		releaseBusy, err := manager.acquireAccount(context.Background(), account.ID)
		if err != nil {
			t.Fatal(err)
		}
		var releaseBusyOnce sync.Once
		releaseBusyLock := func() { releaseBusyOnce.Do(releaseBusy) }
		defer releaseBusyLock()
		waitCalls := 0
		manager.waitInterval = func(_ context.Context, _ time.Duration) bool {
			waitCalls++
			if waitCalls == 1 {
				releaseBusyLock()
				return true
			}
			return false
		}
		done := make(chan struct{})
		go func() {
			manager.Run(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("deferred continuation did not retry on the normal poll")
		}
		if fetchCalls != 1 {
			t.Fatalf("fetch calls after busy timeout = %d, want one normal-poll retry", fetchCalls)
		}
		if waitCalls != 2 {
			t.Fatalf("interval waits after busy timeout = %d, want defer and final waits", waitCalls)
		}
		if failures := repo.failureCalls(); len(failures) != 0 {
			t.Fatalf("account-lock timeout recorded as sync failure: %#v", failures)
		}
	})
}

func TestRunStopsOnCanceledContextWithoutWaiting(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}
	repo := newFakeRepo(account)
	fetchCalls := 0
	fetcher := fetcherFunc(func(context.Context, domain.Account, string, []domain.Alias, *domain.IMAPSyncState, map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error) {
		fetchCalls++
		return domain.MailboxSyncResult{}, nil
	})
	manager := New(repo, cipherFunc(fixedCipher), fetcher, discardLogger(), time.Hour, 1)
	waitCalls := 0
	manager.waitInterval = func(_ context.Context, _ time.Duration) bool {
		waitCalls++
		return true
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		manager.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled sync run did not stop")
	}
	if fetchCalls != 0 {
		t.Fatalf("fetch calls for canceled run = %d, want 0", fetchCalls)
	}
	if waitCalls != 0 {
		t.Fatalf("interval waits for canceled run = %d, want 0", waitCalls)
	}
}

func TestSyncDisabledAccount(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: false}
	repo := newFakeRepo(account)
	manager := New(repo, cipherFunc(fixedCipher), nil, discardLogger(), time.Minute, 1)

	err := manager.SyncAccount(context.Background(), account.ID)
	if !errors.Is(err, store.ErrAccountDisabled) {
		t.Fatalf("停用主号错误 = %v", err)
	}
}
