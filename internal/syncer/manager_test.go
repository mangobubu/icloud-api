package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

type fakeRepo struct {
	account        domain.Account
	aliases        []domain.Alias
	messages       []domain.LatestMessage
	deleted        []int64
	status         string
	aliasStatus    map[int64]string
	aliasSyncedAt  map[int64]*time.Time
	operations     []string
	replaceErr     error
	deleteErr      error
	deleteOtherErr error
}

func (f *fakeRepo) ListEnabledAccounts(context.Context) ([]domain.Account, error) {
	return []domain.Account{f.account}, nil
}
func (f *fakeRepo) GetAccount(context.Context, int64) (domain.Account, error) { return f.account, nil }
func (f *fakeRepo) ListAliasesByAccount(context.Context, int64) ([]domain.Alias, error) {
	return f.aliases, nil
}
func (f *fakeRepo) ReplaceLatestMessage(_ context.Context, msg domain.LatestMessage) error {
	f.operations = append(f.operations, "replace")
	if f.replaceErr != nil {
		return f.replaceErr
	}
	f.messages = append(f.messages, msg)
	return nil
}
func (f *fakeRepo) DeleteLatestMessage(_ context.Context, aliasID int64) error {
	f.operations = append(f.operations, "delete")
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, aliasID)
	return nil
}
func (f *fakeRepo) DeleteLatestMessageFromOtherUIDValidity(_ context.Context, _ int64, _ uint32) error {
	f.operations = append(f.operations, "delete-other")
	if f.deleteOtherErr != nil {
		return f.deleteOtherErr
	}
	return nil
}
func (f *fakeRepo) UpdateAliasSyncStatus(_ context.Context, id int64, status, _ string, syncedAt *time.Time) error {
	f.operations = append(f.operations, "status:"+status)
	if f.aliasStatus == nil {
		f.aliasStatus = make(map[int64]string)
	}
	if f.aliasSyncedAt == nil {
		f.aliasSyncedAt = make(map[int64]*time.Time)
	}
	f.aliasStatus[id] = status
	if syncedAt == nil {
		f.aliasSyncedAt[id] = nil
	} else {
		value := *syncedAt
		f.aliasSyncedAt[id] = &value
	}
	return nil
}
func (f *fakeRepo) UpdateAccountSyncStatus(_ context.Context, _ int64, status, _ string, _ *time.Time) error {
	f.status = status
	return nil
}

type fakeCipher struct{}

func (fakeCipher) Decrypt(string) (string, error) { return "password", nil }

type fakeFetcher struct {
	mu      sync.Mutex
	aliases []domain.Alias
	empty   bool
	results map[int64]domain.LatestMessage
}

type fetcherFunc func(context.Context, domain.Account, string, []domain.Alias) (map[int64]domain.LatestMessage, error)

func (f fetcherFunc) FetchLatest(ctx context.Context, account domain.Account, password string, aliases []domain.Alias) (map[int64]domain.LatestMessage, error) {
	return f(ctx, account, password, aliases)
}

type managerTestRepo struct {
	accounts      []domain.Account
	aliases       map[int64][]domain.Alias
	updateAlias   func(context.Context, int64, string, string, *time.Time) error
	updateAccount func(context.Context, int64, string, string, *time.Time) error
}

func (r *managerTestRepo) ListEnabledAccounts(context.Context) ([]domain.Account, error) {
	return append([]domain.Account(nil), r.accounts...), nil
}

func (r *managerTestRepo) GetAccount(_ context.Context, accountID int64) (domain.Account, error) {
	for _, account := range r.accounts {
		if account.ID == accountID {
			return account, nil
		}
	}
	return domain.Account{}, errors.New("account not found")
}

func (r *managerTestRepo) ListAliasesByAccount(_ context.Context, accountID int64) ([]domain.Alias, error) {
	return append([]domain.Alias(nil), r.aliases[accountID]...), nil
}

func (*managerTestRepo) ReplaceLatestMessage(context.Context, domain.LatestMessage) error {
	return nil
}

func (*managerTestRepo) DeleteLatestMessage(context.Context, int64) error {
	return nil
}

func (*managerTestRepo) DeleteLatestMessageFromOtherUIDValidity(context.Context, int64, uint32) error {
	return nil
}

func (r *managerTestRepo) UpdateAliasSyncStatus(ctx context.Context, aliasID int64, status, message string, syncedAt *time.Time) error {
	if r.updateAlias != nil {
		return r.updateAlias(ctx, aliasID, status, message, syncedAt)
	}
	return nil
}

func (r *managerTestRepo) UpdateAccountSyncStatus(ctx context.Context, accountID int64, status, message string, syncedAt *time.Time) error {
	if r.updateAccount != nil {
		return r.updateAccount(ctx, accountID, status, message, syncedAt)
	}
	return nil
}

func (f *fakeFetcher) FetchLatest(_ context.Context, _ domain.Account, _ string, aliases []domain.Alias) (map[int64]domain.LatestMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aliases = append([]domain.Alias(nil), aliases...)
	if f.results != nil {
		return f.results, nil
	}
	if f.empty {
		result := make(map[int64]domain.LatestMessage, len(aliases))
		for _, alias := range aliases {
			result[alias.ID] = domain.LatestMessage{AliasID: alias.ID, SnapshotState: domain.SnapshotEmpty}
		}
		return result, nil
	}
	return map[int64]domain.LatestMessage{aliases[0].ID: {UID: 9, UIDValidity: 3, SnapshotState: domain.SnapshotFound}}, nil
}

func TestSyncAccountFiltersDisabledAliasAndStoresLatest(t *testing.T) {
	repo := &fakeRepo{
		account: domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"},
		aliases: []domain.Alias{{ID: 10, Enabled: true}, {ID: 20, Enabled: false}},
	}
	fetcher := &fakeFetcher{}
	manager := New(repo, fakeCipher{}, fetcher, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 1)
	if err := manager.SyncAccount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(fetcher.aliases) != 1 || fetcher.aliases[0].ID != 10 {
		t.Fatalf("传给 IMAP 的别名不正确: %#v", fetcher.aliases)
	}
	if len(repo.messages) != 1 || repo.messages[0].AliasID != 10 {
		t.Fatalf("最新邮件未正确保存: %#v", repo.messages)
	}
	if repo.status != domain.SyncStatusOK {
		t.Fatalf("同步状态为 %q", repo.status)
	}
	if repo.aliasStatus[10] != domain.SyncStatusOK {
		t.Fatalf("隐私邮箱同步状态为 %q", repo.aliasStatus[10])
	}
}

func TestSyncDisabledAccount(t *testing.T) {
	repo := &fakeRepo{account: domain.Account{ID: 1, Enabled: false}}
	manager := New(repo, fakeCipher{}, &fakeFetcher{}, slog.Default(), time.Minute, 1)
	if err := manager.SyncAccount(context.Background(), 1); err == nil || !errors.Is(err, errors.New("主号已停用")) && err.Error() != "主号已停用" {
		t.Fatalf("预期停用错误，得到 %v", err)
	}
}

func TestSyncAccountClearsSnapshotWhenMailboxHasNoMatch(t *testing.T) {
	repo := &fakeRepo{
		account: domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"},
		aliases: []domain.Alias{{ID: 10, Enabled: true}},
	}
	manager := New(repo, fakeCipher{}, &fakeFetcher{empty: true}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 1)
	if err := manager.SyncAccount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != 10 {
		t.Fatalf("未清理旧快照: %#v", repo.deleted)
	}
	if repo.aliasStatus[10] != domain.SyncStatusOK {
		t.Fatalf("空邮箱状态为 %q", repo.aliasStatus[10])
	}
}

func TestSyncAccountUnknownAliasDoesNotBlockOtherSnapshots(t *testing.T) {
	repo := &fakeRepo{
		account: domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"},
		aliases: []domain.Alias{
			{ID: 10, Enabled: true},
			{ID: 20, Enabled: true},
			{ID: 30, Enabled: true},
		},
	}
	fetcher := &fakeFetcher{results: map[int64]domain.LatestMessage{
		10: {UIDValidity: 4, SnapshotState: domain.SnapshotUnknown},
		20: {UIDValidity: 4, UID: 12, Subject: "found", SnapshotState: domain.SnapshotFound},
		30: {UIDValidity: 4, SnapshotState: domain.SnapshotEmpty},
	}}
	manager := New(repo, fakeCipher{}, fetcher, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 1)

	if err := manager.SyncAccount(context.Background(), 1); err == nil {
		t.Fatal("包含 unknown 别名的同步应返回汇总错误")
	}
	if repo.aliasStatus[10] != domain.SyncStatusError {
		t.Fatalf("unknown 别名同步状态为 %q", repo.aliasStatus[10])
	}
	if repo.aliasStatus[20] != domain.SyncStatusOK {
		t.Fatalf("found 别名同步状态为 %q", repo.aliasStatus[20])
	}
	if repo.aliasStatus[30] != domain.SyncStatusOK {
		t.Fatalf("empty 别名同步状态为 %q", repo.aliasStatus[30])
	}
	if len(repo.messages) != 1 || repo.messages[0].AliasID != 20 || repo.messages[0].Subject != "found" {
		t.Fatalf("found 别名未独立提交: %#v", repo.messages)
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != 30 {
		t.Fatalf("empty 别名未独立清理: %#v", repo.deleted)
	}
	if repo.status != domain.SyncStatusError {
		t.Fatalf("主号汇总同步状态为 %q", repo.status)
	}
}

func TestAccountLockSerializesConfigurationWithSync(t *testing.T) {
	repo := &fakeRepo{
		account: domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"},
		aliases: []domain.Alias{{ID: 10, Enabled: true}},
	}
	manager := New(repo, fakeCipher{}, &fakeFetcher{}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 1)
	lockEntered := make(chan struct{})
	releaseLock := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- manager.WithAccountLock(context.Background(), 1, func() error {
			close(lockEntered)
			<-releaseLock
			return nil
		})
	}()
	<-lockEntered

	syncDone := make(chan error, 1)
	go func() { syncDone <- manager.SyncAccount(context.Background(), 1) }()
	select {
	case err := <-syncDone:
		t.Fatalf("同步越过了配置锁: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseLock)
	if err := <-mutationDone; err != nil {
		t.Fatalf("配置操作失败: %v", err)
	}
	if err := <-syncDone; err != nil {
		t.Fatalf("锁释放后的同步失败: %v", err)
	}
}

func TestSnapshotMutationFailuresLeaveAliasUnavailable(t *testing.T) {
	tests := []struct {
		name       string
		snapshot   domain.LatestMessage
		configure  func(*fakeRepo)
		wantPrefix []string
	}{
		{
			name:       "empty delete fails",
			snapshot:   domain.LatestMessage{UIDValidity: 5, SnapshotState: domain.SnapshotEmpty},
			configure:  func(repo *fakeRepo) { repo.deleteErr = errors.New("delete failed") },
			wantPrefix: []string{"status:pending", "delete", "status:error"},
		},
		{
			name:       "unknown generation cleanup fails",
			snapshot:   domain.LatestMessage{UIDValidity: 5, SnapshotState: domain.SnapshotUnknown},
			configure:  func(repo *fakeRepo) { repo.deleteOtherErr = errors.New("cleanup failed") },
			wantPrefix: []string{"status:error", "delete-other", "status:error"},
		},
		{
			name:       "found replace fails",
			snapshot:   domain.LatestMessage{UIDValidity: 5, UID: 9, SnapshotState: domain.SnapshotFound},
			configure:  func(repo *fakeRepo) { repo.replaceErr = errors.New("replace failed") },
			wantPrefix: []string{"status:pending", "replace", "status:error"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepo{
				account: domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"},
				aliases: []domain.Alias{{ID: 10, Enabled: true}},
			}
			tt.configure(repo)
			fetcher := &fakeFetcher{results: map[int64]domain.LatestMessage{10: tt.snapshot}}
			manager := New(repo, fakeCipher{}, fetcher, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 1)

			if err := manager.SyncAccount(context.Background(), 1); err == nil {
				t.Fatal("快照变更失败时同步仍返回成功")
			}
			if repo.aliasStatus[10] != domain.SyncStatusError {
				t.Fatalf("失败后的隐私邮箱状态为 %q", repo.aliasStatus[10])
			}
			if len(repo.operations) < len(tt.wantPrefix) {
				t.Fatalf("操作序列过短: %#v", repo.operations)
			}
			for index, want := range tt.wantPrefix {
				if repo.operations[index] != want {
					t.Fatalf("操作 %d = %q, want %q; all=%#v", index, repo.operations[index], want, repo.operations)
				}
			}
		})
	}
}

func TestSyncContinuesAfterOneAliasMutationFails(t *testing.T) {
	repo := &fakeRepo{
		account:   domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"},
		aliases:   []domain.Alias{{ID: 10, Enabled: true}, {ID: 20, Enabled: true}, {ID: 30, Enabled: true}},
		deleteErr: errors.New("delete failed"),
	}
	fetcher := &fakeFetcher{results: map[int64]domain.LatestMessage{
		10: {UIDValidity: 8, UID: 10, Subject: "first", SnapshotState: domain.SnapshotFound},
		20: {UIDValidity: 8, SnapshotState: domain.SnapshotEmpty},
		30: {UIDValidity: 8, UID: 30, Subject: "third", SnapshotState: domain.SnapshotFound},
	}}
	manager := New(repo, fakeCipher{}, fetcher, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 1)

	if err := manager.SyncAccount(context.Background(), 1); err == nil {
		t.Fatal("部分快照写入失败时同步仍返回成功")
	}
	if repo.aliasStatus[10] != domain.SyncStatusOK || repo.aliasStatus[20] != domain.SyncStatusError || repo.aliasStatus[30] != domain.SyncStatusOK {
		t.Fatalf("部分失败后的 alias 状态 = %#v", repo.aliasStatus)
	}
	if len(repo.messages) != 2 || repo.messages[0].AliasID != 10 || repo.messages[1].AliasID != 30 {
		t.Fatalf("健康 alias 未继续提交: %#v", repo.messages)
	}
	if repo.status != domain.SyncStatusError {
		t.Fatalf("部分失败后的主号状态 = %q", repo.status)
	}
}

func TestAliasFreshnessUsesFetcherSnapshotTime(t *testing.T) {
	snapshotAt := time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC)
	repo := &fakeRepo{
		account: domain.Account{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"},
		aliases: []domain.Alias{{ID: 10, Enabled: true}},
	}
	fetcher := &fakeFetcher{results: map[int64]domain.LatestMessage{
		10: {
			UIDValidity: 9, UID: 11, SyncedAt: snapshotAt,
			SnapshotState: domain.SnapshotFound,
		},
	}}
	manager := New(repo, fakeCipher{}, fetcher, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 1)

	if err := manager.SyncAccount(context.Background(), 1); err != nil {
		t.Fatalf("sync account: %v", err)
	}
	if repo.aliasSyncedAt[10] == nil || !repo.aliasSyncedAt[10].Equal(snapshotAt) {
		t.Fatalf("alias freshness = %v, want %v", repo.aliasSyncedAt[10], snapshotAt)
	}
}

func TestSyncAllAppliesConfiguredTimeoutToEveryAccount(t *testing.T) {
	repo := &managerTestRepo{accounts: []domain.Account{
		{ID: 1, Enabled: true, PasswordCiphertext: "encrypted-1"},
		{ID: 2, Enabled: true, PasswordCiphertext: "encrypted-2"},
	}}
	fetched := make(chan int64, len(repo.accounts))
	manager := New(repo, fakeCipher{}, fetcherFunc(func(_ context.Context, account domain.Account, _ string, _ []domain.Alias) (map[int64]domain.LatestMessage, error) {
		fetched <- account.ID
		return map[int64]domain.LatestMessage{}, nil
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, len(repo.accounts))
	const wantTimeout = 17 * time.Second
	manager.SetSyncTimeout(wantTimeout)
	timeouts := make(chan time.Duration, len(repo.accounts))
	manager.withTimeout = func(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
		timeouts <- timeout
		return context.WithCancel(ctx)
	}

	manager.syncAll(context.Background())

	seenAccounts := make(map[int64]bool, len(repo.accounts))
	for range repo.accounts {
		if timeout := <-timeouts; timeout != wantTimeout {
			t.Fatalf("单主号同步时限 = %v, want %v", timeout, wantTimeout)
		}
		seenAccounts[<-fetched] = true
	}
	if !seenAccounts[1] || !seenAccounts[2] {
		t.Fatalf("未同步全部主号: %#v", seenAccounts)
	}
}

func TestSyncAccountWithTimeoutCancelsFetcher(t *testing.T) {
	repo := &managerTestRepo{accounts: []domain.Account{{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}}}
	manager := New(repo, fakeCipher{}, fetcherFunc(func(ctx context.Context, _ domain.Account, _ string, _ []domain.Alias) (map[int64]domain.LatestMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 1)
	manager.SetSyncTimeout(20 * time.Millisecond)

	err := manager.SyncAccountWithTimeout(context.Background(), 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("同步超时错误 = %v, want %v", err, context.DeadlineExceeded)
	}
}

func TestRunStartsIntervalAfterRoundCompletes(t *testing.T) {
	repo := &managerTestRepo{accounts: []domain.Account{{ID: 1, Enabled: true, PasswordCiphertext: "encrypted"}}}
	started := make(chan int, 2)
	releaseFirst := make(chan struct{})
	var calls int
	var callsMu sync.Mutex
	manager := New(repo, fakeCipher{}, fetcherFunc(func(_ context.Context, _ domain.Account, _ string, _ []domain.Alias) (map[int64]domain.LatestMessage, error) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		started <- call
		if call == 1 {
			<-releaseFirst
		}
		return map[int64]domain.LatestMessage{}, nil
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), 45*time.Second, 1)
	waitCalled := make(chan time.Duration, 2)
	continueWaiting := make(chan bool)
	manager.waitInterval = func(ctx context.Context, interval time.Duration) bool {
		waitCalled <- interval
		select {
		case again := <-continueWaiting:
			return again
		case <-ctx.Done():
			return false
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		manager.Run(ctx)
		close(runDone)
	}()

	select {
	case call := <-started:
		if call != 1 {
			t.Fatalf("首次同步序号 = %d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("首次同步未启动")
	}
	select {
	case interval := <-waitCalled:
		t.Fatalf("首轮仍在运行时已开始等待下一轮: %v", interval)
	default:
	}
	close(releaseFirst)

	select {
	case interval := <-waitCalled:
		if interval != 45*time.Second {
			t.Fatalf("轮询间隔 = %v", interval)
		}
	case <-time.After(time.Second):
		t.Fatal("首轮完成后未开始等待下一轮")
	}
	continueWaiting <- true
	select {
	case call := <-started:
		if call != 2 {
			t.Fatalf("第二次同步序号 = %d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("第二轮同步未启动")
	}
	select {
	case <-waitCalled:
	case <-time.After(time.Second):
		t.Fatal("第二轮完成后未重新开始计时")
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("调度器未响应取消")
	}
}

func TestFailureRecorderReusesFallbackContextAfterParentCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	var calls []context.Context
	var fallback context.Context
	created := 0
	repo := &managerTestRepo{}
	repo.updateAlias = func(ctx context.Context, _ int64, _, _ string, _ *time.Time) error {
		calls = append(calls, ctx)
		if len(calls) == 1 {
			cancelParent()
			return context.Canceled
		}
		return nil
	}
	repo.updateAccount = func(ctx context.Context, _ int64, _, _ string, _ *time.Time) error {
		calls = append(calls, ctx)
		return nil
	}
	manager := New(repo, fakeCipher{}, &fakeFetcher{}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 1)
	manager.withTimeout = func(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
		if timeout != 3*time.Second {
			t.Fatalf("失败状态兜底时限 = %v", timeout)
		}
		created++
		fallback, cancelParent = context.WithCancel(ctx)
		return fallback, cancelParent
	}
	recorder := newFailureRecorder(manager, parent, 1)
	defer recorder.close()

	recorder.record([]domain.Alias{{ID: 10}, {ID: 20}}, errors.New("sync failed"))
	recorder.record([]domain.Alias{{ID: 30}}, errors.New("still failed"))

	if created != 1 {
		t.Fatalf("兜底 context 创建次数 = %d, want 1", created)
	}
	if len(calls) != 6 {
		t.Fatalf("状态写入次数 = %d, want 6", len(calls))
	}
	for index, ctx := range calls[1:] {
		if ctx != fallback {
			t.Fatalf("第 %d 次兜底写入未复用 context", index+2)
		}
	}
}

func TestAccountLocksAreReclaimed(t *testing.T) {
	manager := New(&managerTestRepo{}, fakeCipher{}, &fakeFetcher{}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 1)
	for accountID := int64(1); accountID <= 128; accountID++ {
		if err := manager.WithAccountLock(context.Background(), accountID, func() error { return nil }); err != nil {
			t.Fatalf("锁定主号 %d: %v", accountID, err)
		}
	}
	manager.locksMu.Lock()
	defer manager.locksMu.Unlock()
	if len(manager.locks) != 0 {
		t.Fatalf("已释放主号仍留在锁表中: %d", len(manager.locks))
	}
}

func TestCanceledAccountLockWaiterDoesNotPreventReclamation(t *testing.T) {
	manager := New(&managerTestRepo{}, fakeCipher{}, &fakeFetcher{}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 1)
	release, err := manager.acquireAccount(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.acquireAccount(canceled, 7); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消等待者错误 = %v", err)
	}

	manager.locksMu.Lock()
	remaining := len(manager.locks)
	manager.locksMu.Unlock()
	if remaining != 1 {
		t.Fatalf("持有期间锁表大小 = %d, want 1", remaining)
	}
	release()
	release()
	manager.locksMu.Lock()
	defer manager.locksMu.Unlock()
	if len(manager.locks) != 0 {
		t.Fatalf("最后持有者释放后锁表大小 = %d", len(manager.locks))
	}
}
