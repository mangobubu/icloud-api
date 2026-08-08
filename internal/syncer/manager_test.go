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
	positions  map[int64]map[int64]domain.MailboxSnapshotPosition
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
		positions: make(map[int64]map[int64]domain.MailboxSnapshotPosition),
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

func (f *fakeRepo) ListMailboxSnapshotPositions(_ context.Context, accountID int64) (map[int64]domain.MailboxSnapshotPosition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make(map[int64]domain.MailboxSnapshotPosition, len(f.positions[accountID]))
	for aliasID, position := range f.positions[accountID] {
		result[aliasID] = position
	}
	return result, nil
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
	wantPositions := map[int64]domain.MailboxSnapshotPosition{
		10: {AliasID: 10, UIDValidity: 7, UID: 40},
	}
	repo.positions[account.ID] = wantPositions
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
		if !reflect.DeepEqual(positions, wantPositions) {
			t.Fatalf("mailbox snapshot positions = %#v, want %#v", positions, wantPositions)
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
	if len(repo.applyCalls()) != 0 {
		t.Fatalf("抓取失败后不应提交结果: %#v", repo.applyCalls())
	}
	failures := repo.failureCalls()
	if len(failures) != 1 {
		t.Fatalf("165 个隐私邮箱的失败写入次数 = %d, want 1", len(failures))
	}
	if failures[0].accountID != account.ID || !failures[0].version.Equal(accountVersion) ||
		failures[0].message != wantErr.Error() || failures[0].at.IsZero() {
		t.Fatalf("批量失败记录错误: %#v", failures[0])
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
	if len(repo.failureCalls()) != 1 || !recordContextActive {
		t.Fatalf("超时后的批量失败记录未使用独立 context: calls=%d active=%v", len(repo.failureCalls()), recordContextActive)
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

func TestFailureRecorderTruncatesUnicodeWithoutBreakingUTF8(t *testing.T) {
	repo := newFakeRepo()
	manager := New(repo, cipherFunc(fixedCipher), nil, discardLogger(), time.Minute, 1)
	recorder := newFailureRecorder(manager, context.Background(), 1, time.Unix(0, 1).UTC())
	defer recorder.close()

	recorder.record(errors.New(strings.Repeat("错", 300)))
	failures := repo.failureCalls()
	if len(failures) != 1 {
		t.Fatalf("失败记录次数 = %d, want 1", len(failures))
	}
	if !utf8.ValidString(failures[0].message) {
		t.Fatalf("截断后的错误文本不是有效 UTF-8: %q", failures[0].message)
	}
	if got := utf8.RuneCountInString(failures[0].message); got != 240 {
		t.Fatalf("截断后字符数 = %d, want 240", got)
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

func TestSyncDisabledAccount(t *testing.T) {
	account := domain.Account{ID: 1, Enabled: false}
	repo := newFakeRepo(account)
	manager := New(repo, cipherFunc(fixedCipher), nil, discardLogger(), time.Minute, 1)

	err := manager.SyncAccount(context.Background(), account.ID)
	if err == nil || err.Error() != "主号已停用" {
		t.Fatalf("停用主号错误 = %v", err)
	}
}
