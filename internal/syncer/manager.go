package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"icloud-api/internal/domain"
)

type Repository interface {
	ListEnabledAccounts(context.Context) ([]domain.Account, error)
	GetAccount(context.Context, int64) (domain.Account, error)
	ListAliasesByAccount(context.Context, int64) ([]domain.Alias, error)
	ReplaceLatestMessage(context.Context, domain.LatestMessage) error
	DeleteLatestMessage(context.Context, int64) error
	DeleteLatestMessageFromOtherUIDValidity(context.Context, int64, uint32) error
	UpdateAliasSyncStatus(context.Context, int64, string, string, *time.Time) error
	UpdateAccountSyncStatus(context.Context, int64, string, string, *time.Time) error
}

type CredentialCipher interface {
	Decrypt(string) (string, error)
}

type MailFetcher interface {
	FetchLatest(context.Context, domain.Account, string, []domain.Alias) (map[int64]domain.LatestMessage, error)
}

type accountLock struct {
	token chan struct{}
	refs  int
}

type Manager struct {
	repo         Repository
	cipher       CredentialCipher
	fetcher      MailFetcher
	logger       *slog.Logger
	interval     time.Duration
	syncTimeout  time.Duration
	concurrency  int
	withTimeout  func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	waitInterval func(context.Context, time.Duration) bool

	locksMu sync.Mutex
	locks   map[int64]*accountLock
}

func New(repo Repository, cipher CredentialCipher, fetcher MailFetcher, logger *slog.Logger, interval time.Duration, concurrency int) *Manager {
	return &Manager{
		repo:         repo,
		cipher:       cipher,
		fetcher:      fetcher,
		logger:       logger,
		interval:     interval,
		syncTimeout:  2 * time.Minute,
		concurrency:  concurrency,
		withTimeout:  context.WithTimeout,
		waitInterval: waitForInterval,
		locks:        make(map[int64]*accountLock),
	}
}

func (m *Manager) SetSyncTimeout(timeout time.Duration) {
	if timeout > 0 {
		m.syncTimeout = timeout
	}
}

func (m *Manager) Run(ctx context.Context) {
	for {
		m.syncAll(ctx)
		if !m.waitInterval(ctx, m.interval) {
			return
		}
	}
}

func waitForInterval(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (m *Manager) syncAll(ctx context.Context) {
	accounts, err := m.repo.ListEnabledAccounts(ctx)
	if err != nil {
		m.logger.Error("读取待同步主号失败", "error", err)
		return
	}
	semaphore := make(chan struct{}, m.concurrency)
	var wg sync.WaitGroup
	for _, account := range accounts {
		account := account
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			if err := m.SyncAccountWithTimeout(ctx, account.ID); err != nil && !errors.Is(err, context.Canceled) {
				m.logger.Warn("主号同步失败", "account_id", account.ID, "error", err)
			}
		}()
	}
	wg.Wait()
}

// SyncAccountWithTimeout applies the same configured total limit to periodic
// and manually requested account syncs.
func (m *Manager) SyncAccountWithTimeout(ctx context.Context, accountID int64) error {
	syncCtx, cancel := m.withTimeout(ctx, m.syncTimeout)
	defer cancel()
	return m.SyncAccount(syncCtx, accountID)
}

func (m *Manager) SyncAccount(ctx context.Context, accountID int64) error {
	release, err := m.acquireAccount(ctx, accountID)
	if err != nil {
		return err
	}
	defer release()
	failures := newFailureRecorder(m, ctx, accountID)
	defer failures.close()

	account, err := m.repo.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if !account.Enabled {
		return errors.New("主号已停用")
	}
	aliases, err := m.repo.ListAliasesByAccount(ctx, accountID)
	if err != nil {
		return err
	}
	enabled := aliases[:0]
	for _, alias := range aliases {
		if alias.Enabled {
			enabled = append(enabled, alias)
		}
	}

	password, err := m.cipher.Decrypt(account.PasswordCiphertext)
	if err != nil {
		failures.record(enabled, err)
		return fmt.Errorf("解密 IMAP 凭据: %w", err)
	}
	messages, err := m.fetcher.FetchLatest(ctx, account, password, enabled)
	password = ""
	if err != nil {
		failures.record(enabled, err)
		return err
	}
	enabledIDs := make(map[int64]domain.Alias, len(enabled))
	for _, alias := range enabled {
		enabledIDs[alias.ID] = alias
	}
	now := time.Now().UTC()
	for aliasID := range messages {
		if _, ok := enabledIDs[aliasID]; !ok {
			err := fmt.Errorf("IMAP 结果包含未授权隐私邮箱 %d", aliasID)
			failures.record(enabled, err)
			return err
		}
	}
	unknownCount := 0
	processable := make(map[int64]bool, len(enabled))
	var issues []error
	snapshotTime := func(message domain.LatestMessage) time.Time {
		if message.SyncedAt.IsZero() {
			return now
		}
		return message.SyncedAt.UTC()
	}

	// Withdraw every affected alias before publishing any new snapshot. This
	// keeps later aliases fail-closed if an earlier database operation fails.
	for _, alias := range enabled {
		if err := ctx.Err(); err != nil {
			failures.record(enabled, err)
			return err
		}
		aliasID := alias.ID
		message, ok := messages[aliasID]
		status := domain.SyncStatusPending
		statusError := ""
		var statusAt *time.Time
		if !ok {
			unknownCount++
			status = domain.SyncStatusError
			statusError = "IMAP 未返回该隐私邮箱的同步状态"
			statusAt = &now
		} else {
			at := snapshotTime(message)
			switch message.SnapshotState {
			case domain.SnapshotEmpty:
			case domain.SnapshotUnknown:
				unknownCount++
				status = domain.SyncStatusError
				statusError = "最新邮件状态未能确认"
				statusAt = &at
			case domain.SnapshotFound:
				if message.UIDValidity == 0 || message.UID == 0 {
					unknownCount++
					status = domain.SyncStatusError
					statusError = "已找到的最新邮件缺少有效 IMAP 标识"
					statusAt = &at
				}
			default:
				unknownCount++
				status = domain.SyncStatusError
				statusError = "IMAP 返回了无效的快照状态"
				statusAt = &at
			}
		}
		if err := m.repo.UpdateAliasSyncStatus(ctx, aliasID, status, statusError, statusAt); err != nil {
			wrapped := fmt.Errorf("撤下隐私邮箱 %d 的旧快照状态: %w", aliasID, err)
			issues = append(issues, wrapped)
			failures.record([]domain.Alias{alias}, wrapped)
			continue
		}
		processable[aliasID] = true
	}

	for _, alias := range enabled {
		if err := ctx.Err(); err != nil {
			failures.record(enabled, err)
			return err
		}
		aliasID := alias.ID
		message, ok := messages[aliasID]
		if !ok || !processable[aliasID] {
			continue
		}
		at := snapshotTime(message)
		switch message.SnapshotState {
		case domain.SnapshotEmpty:
			if err := m.repo.DeleteLatestMessage(ctx, aliasID); err != nil {
				wrapped := fmt.Errorf("清理隐私邮箱 %d 的旧邮件快照: %w", aliasID, err)
				issues = append(issues, wrapped)
				failures.record([]domain.Alias{alias}, wrapped)
				continue
			}
		case domain.SnapshotUnknown:
			if message.UIDValidity != 0 {
				if err := m.repo.DeleteLatestMessageFromOtherUIDValidity(ctx, aliasID, message.UIDValidity); err != nil {
					wrapped := fmt.Errorf("清理隐私邮箱 %d 的旧 IMAP 世代快照: %w", aliasID, err)
					issues = append(issues, wrapped)
					failures.record([]domain.Alias{alias}, wrapped)
				}
			}
			continue
		case domain.SnapshotFound:
			if message.UIDValidity == 0 || message.UID == 0 {
				continue
			}
		default:
			continue
		}
		if message.SnapshotState == domain.SnapshotFound {
			message.AliasID = aliasID
			if err := m.repo.ReplaceLatestMessage(ctx, message); err != nil {
				wrapped := fmt.Errorf("保存隐私邮箱 %d 的最新邮件: %w", aliasID, err)
				issues = append(issues, wrapped)
				failures.record([]domain.Alias{alias}, wrapped)
				continue
			}
		}
		if err := m.repo.UpdateAliasSyncStatus(ctx, aliasID, domain.SyncStatusOK, "", &at); err != nil {
			wrapped := fmt.Errorf("发布隐私邮箱 %d 的同步状态: %w", aliasID, err)
			issues = append(issues, wrapped)
			failures.record([]domain.Alias{alias}, wrapped)
		}
	}
	if unknownCount > 0 {
		issues = append(issues, fmt.Errorf("%d 个隐私邮箱的最新邮件状态未能确认", unknownCount))
	}
	if len(issues) > 0 {
		joined := errors.Join(issues...)
		failures.record(nil, joined)
		return joined
	}
	if err := m.repo.UpdateAccountSyncStatus(ctx, accountID, domain.SyncStatusOK, "", &now); err != nil {
		return err
	}
	return nil
}

// WithAccountLock serializes account configuration changes with IMAP syncs.
// The service intentionally runs as a single process, so this lock also forms
// the publication boundary for credentials and alias snapshots.
func (m *Manager) WithAccountLock(ctx context.Context, accountID int64, operation func() error) error {
	if operation == nil {
		return errors.New("主号操作不能为空")
	}
	release, err := m.acquireAccount(ctx, accountID)
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	return operation()
}

type failureRecorder struct {
	manager   *Manager
	parent    context.Context
	accountID int64
	fallback  context.Context
	cancel    context.CancelFunc
}

func newFailureRecorder(manager *Manager, parent context.Context, accountID int64) *failureRecorder {
	return &failureRecorder{manager: manager, parent: parent, accountID: accountID}
}

func (r *failureRecorder) close() {
	if r.cancel != nil {
		r.cancel()
	}
}

func (r *failureRecorder) statusContext() (context.Context, bool) {
	if r.parent.Err() == nil {
		return r.parent, false
	}
	if r.fallback == nil {
		r.fallback, r.cancel = r.manager.withTimeout(context.WithoutCancel(r.parent), 3*time.Second)
	}
	return r.fallback, true
}

func (r *failureRecorder) update(operation func(context.Context) error) error {
	statusCtx, fallback := r.statusContext()
	err := operation(statusCtx)
	if err != nil && !fallback && r.parent.Err() != nil {
		statusCtx, _ = r.statusContext()
		err = operation(statusCtx)
	}
	return err
}

func (r *failureRecorder) record(aliases []domain.Alias, syncErr error) {
	message := strings.TrimSpace(syncErr.Error())
	if len(message) > 240 {
		message = message[:240]
	}
	now := time.Now().UTC()
	for _, alias := range aliases {
		if err := r.update(func(ctx context.Context) error {
			return r.manager.repo.UpdateAliasSyncStatus(ctx, alias.ID, domain.SyncStatusError, message, &now)
		}); err != nil {
			r.manager.logger.Error("记录隐私邮箱同步状态失败", "alias_id", alias.ID, "error", err)
		}
	}
	if err := r.update(func(ctx context.Context) error {
		return r.manager.repo.UpdateAccountSyncStatus(ctx, r.accountID, domain.SyncStatusError, message, &now)
	}); err != nil {
		r.manager.logger.Error("记录同步状态失败", "account_id", r.accountID, "error", err)
	}
}

func (m *Manager) acquireAccount(ctx context.Context, accountID int64) (func(), error) {
	m.locksMu.Lock()
	lock, ok := m.locks[accountID]
	if !ok {
		lock = &accountLock{token: make(chan struct{}, 1)}
		m.locks[accountID] = lock
	}
	lock.refs++
	m.locksMu.Unlock()
	select {
	case lock.token <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-lock.token
				m.releaseAccountLock(accountID, lock)
			})
		}, nil
	case <-ctx.Done():
		m.releaseAccountLock(accountID, lock)
		return nil, ctx.Err()
	}
}

func (m *Manager) releaseAccountLock(accountID int64, lock *accountLock) {
	m.locksMu.Lock()
	defer m.locksMu.Unlock()
	lock.refs--
	if lock.refs == 0 && m.locks[accountID] == lock {
		delete(m.locks, accountID)
	}
}
