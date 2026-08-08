package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

type Repository interface {
	ListEnabledAccounts(context.Context) ([]domain.Account, error)
	GetAccount(context.Context, int64) (domain.Account, error)
	ListEnabledAliasesByAccount(context.Context, int64) ([]domain.Alias, error)
	ListMailboxSnapshotPositions(context.Context, int64) (map[int64]domain.MailboxSnapshotPosition, error)
	GetIMAPSyncState(context.Context, int64) (domain.IMAPSyncState, error)
	ApplyMailboxSync(context.Context, int64, time.Time, []domain.Alias, domain.MailboxSyncResult, time.Time) error
	RecordMailboxSyncFailure(context.Context, int64, time.Time, string, time.Time) error
}

type CredentialCipher interface {
	Decrypt(string) (string, error)
}

type MailFetcher interface {
	FetchIncremental(context.Context, domain.Account, string, []domain.Alias, *domain.IMAPSyncState, map[int64]domain.MailboxSnapshotPosition) (domain.MailboxSyncResult, error)
}

// ErrSyncPending means one bounded batch committed successfully, while later
// mailbox UIDs remain for a subsequent sync. It is a continuation state, not a
// failed mailbox sync.
var ErrSyncPending = errors.New("mailbox sync batch committed; more messages remain")

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
	syncSlots    chan struct{}
	withTimeout  func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	waitInterval func(context.Context, time.Duration) bool

	locksMu  sync.Mutex
	locks    map[int64]*accountLock
	stopping atomic.Bool
}

func New(repo Repository, cipher CredentialCipher, fetcher MailFetcher, logger *slog.Logger, interval time.Duration, concurrency int) *Manager {
	return &Manager{
		repo:         repo,
		cipher:       cipher,
		fetcher:      fetcher,
		logger:       logger,
		interval:     interval,
		syncTimeout:  2 * time.Minute,
		syncSlots:    make(chan struct{}, concurrency),
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

// BeginShutdown prevents cancellation caused by process shutdown from being
// persisted as an account synchronization failure.
func (m *Manager) BeginShutdown() {
	m.stopping.Store(true)
}

func (m *Manager) Run(ctx context.Context) {
	var continuations accountIDSet
	for {
		continuations = m.syncAllRound(ctx, continuations)
		if ctx.Err() != nil {
			return
		}
		if len(continuations) > 0 {
			continue
		}
		if !m.waitInterval(ctx, m.interval) {
			return
		}
		continuations = nil
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

// syncAll runs one bounded batch for every account that can acquire its
// account lock and reports whether any account committed a pending batch.
func (m *Manager) syncAll(ctx context.Context) bool {
	return len(m.syncAllRound(ctx, nil)) > 0
}

type accountIDSet map[int64]struct{}

// syncAllRound processes at most one batch per eligible account. A previous
// continuation waits for its account lock with a bounded budget so manual and
// seen work neither lose the continuation nor cause a busy retry loop.
func (m *Manager) syncAllRound(ctx context.Context, continuations accountIDSet) accountIDSet {
	pending := make(accountIDSet)
	accounts, err := m.repo.ListEnabledAccounts(ctx)
	if err != nil {
		m.logger.Error("读取待同步主号失败", "error", err)
		return pending
	}
	var statusMu sync.Mutex
	markResult := func(accountID int64, syncErr error) {
		if !errors.Is(syncErr, ErrSyncPending) {
			return
		}
		statusMu.Lock()
		defer statusMu.Unlock()
		pending[accountID] = struct{}{}
	}
	var wg sync.WaitGroup
	for _, account := range accounts {
		account := account
		_, continuing := continuations[account.ID]
		if continuations != nil && !continuing {
			continue
		}
		if continuations == nil {
			continuing = account.LastSyncStatus == domain.SyncStatusPending
		}
		wg.Add(1)
		go func() {
			defer wg.Done()

			var release func()
			if continuing {
				lockCtx, cancelLock := m.withTimeout(ctx, m.syncTimeout)
				var err error
				release, err = m.acquireAccount(lockCtx, account.ID)
				if err == nil {
					err = lockCtx.Err()
				}
				cancelLock()
				if err != nil {
					if release != nil {
						release()
					}
					return
				}
			} else {
				var acquired bool
				release, acquired = m.tryAcquireAccount(account.ID)
				if !acquired {
					return
				}
			}
			defer release()

			waitCtx, cancelWait := m.withTimeout(ctx, m.syncTimeout)
			defer cancelWait()
			releaseSlot, err := m.acquireSyncSlot(waitCtx)
			if err != nil {
				markResult(account.ID, err)
				if !errors.Is(err, context.Canceled) {
					m.logger.Warn("主号同步失败", "account_id", account.ID, "error", err)
				}
				return
			}
			defer releaseSlot()
			if err := waitCtx.Err(); err != nil {
				markResult(account.ID, err)
				if !errors.Is(err, context.Canceled) {
					m.logger.Warn("主号同步失败", "account_id", account.ID, "error", err)
				}
				return
			}
			cancelWait()

			syncCtx, cancelSync := m.withTimeout(ctx, m.syncTimeout)
			defer cancelSync()
			syncErr := m.syncAccountLocked(syncCtx, account.ID)
			markResult(account.ID, syncErr)
			if syncErr != nil &&
				!errors.Is(syncErr, context.Canceled) && !errors.Is(syncErr, ErrSyncPending) {
				m.logger.Warn("主号同步失败", "account_id", account.ID, "error", syncErr)
			}
		}()
	}
	wg.Wait()
	return pending
}

// SyncAccountWithTimeout bounds queueing and mailbox work separately so a busy
// account or IMAP slot does not consume the mailbox operation's full budget.
func (m *Manager) SyncAccountWithTimeout(ctx context.Context, accountID int64) error {
	waitCtx, cancelWait := m.withTimeout(ctx, m.syncTimeout)
	defer cancelWait()
	return m.WithAccountIMAPSlot(waitCtx, accountID, func() error {
		cancelWait()
		syncCtx, cancelSync := m.withTimeout(ctx, m.syncTimeout)
		defer cancelSync()
		return m.syncAccountLocked(syncCtx, accountID)
	})
}

func (m *Manager) SyncAccount(ctx context.Context, accountID int64) error {
	return m.WithAccountIMAPSlot(ctx, accountID, func() error {
		return m.syncAccountLocked(ctx, accountID)
	})
}

// WithAccountIMAPSlot serializes work for one account and includes it in the
// global IMAP connection limit. Keeping this acquisition order prevents a
// worker holding a global slot while it waits for another operation on the
// same account.
func (m *Manager) WithAccountIMAPSlot(ctx context.Context, accountID int64, operation func() error) error {
	if operation == nil {
		return errors.New("account IMAP operation must not be nil")
	}
	release, err := m.acquireAccount(ctx, accountID)
	if err != nil {
		return err
	}
	defer release()
	releaseSlot, err := m.acquireSyncSlot(ctx)
	if err != nil {
		return err
	}
	defer releaseSlot()
	if err := ctx.Err(); err != nil {
		return err
	}
	return operation()
}

func (m *Manager) syncAccountLocked(ctx context.Context, accountID int64) (syncErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	account, err := m.repo.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if !account.Enabled {
		return errors.New("主号已停用")
	}
	failures := newFailureRecorder(m, ctx, accountID, account.UpdatedAt)
	defer failures.close()
	defer func() {
		if syncErr != nil && !errors.Is(syncErr, ErrSyncPending) {
			failures.record(syncErr)
		}
	}()

	enabled, err := m.repo.ListEnabledAliasesByAccount(ctx, accountID)
	if err != nil {
		return err
	}
	snapshotPositions, err := m.repo.ListMailboxSnapshotPositions(ctx, accountID)
	if err != nil {
		return fmt.Errorf("read mailbox snapshot positions: %w", err)
	}
	state, err := m.repo.GetIMAPSyncState(ctx, accountID)
	var previousState *domain.IMAPSyncState
	switch {
	case err == nil:
		previousState = &state
	case errors.Is(err, store.ErrNotFound):
		// A missing state is the expected first-sync path.
	default:
		return fmt.Errorf("读取 IMAP 增量游标: %w", err)
	}

	password, err := m.cipher.Decrypt(account.PasswordCiphertext)
	if err != nil {
		return fmt.Errorf("解密 IMAP 凭据: %w", err)
	}
	result, err := m.fetcher.FetchIncremental(ctx, account, password, enabled, previousState, snapshotPositions)
	password = ""
	if err != nil {
		return fmt.Errorf("fetch IMAP mailbox increment: %w", err)
	}
	now := time.Now().UTC()
	if err := m.repo.ApplyMailboxSync(ctx, accountID, account.UpdatedAt, enabled, result, now); err != nil {
		return fmt.Errorf("批量保存 IMAP 同步结果: %w", err)
	}
	if result.HasMore {
		return ErrSyncPending
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

// AcquireAccountLock exposes the keyed account boundary to operations that
// must keep configuration changes from racing an irreversible remote request.
// Callers must invoke the returned release function exactly once.
func (m *Manager) AcquireAccountLock(ctx context.Context, accountID int64) (func(), error) {
	return m.acquireAccount(ctx, accountID)
}

type failureRecorder struct {
	manager   *Manager
	parent    context.Context
	accountID int64
	version   time.Time
	fallback  context.Context
	cancel    context.CancelFunc
}

func newFailureRecorder(manager *Manager, parent context.Context, accountID int64, version time.Time) *failureRecorder {
	return &failureRecorder{manager: manager, parent: parent, accountID: accountID, version: version}
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

func (r *failureRecorder) record(syncErr error) {
	if r.manager.stopping.Load() {
		return
	}
	messageRunes := []rune(strings.TrimSpace(syncErr.Error()))
	if len(messageRunes) > 240 {
		messageRunes = messageRunes[:240]
	}
	message := string(messageRunes)
	now := time.Now().UTC()
	if err := r.update(func(ctx context.Context) error {
		return r.manager.repo.RecordMailboxSyncFailure(ctx, r.accountID, r.version, message, now)
	}); err != nil {
		r.manager.logger.Error("记录批量同步失败状态失败", "account_id", r.accountID, "error", err)
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
		return m.accountLockRelease(accountID, lock), nil
	case <-ctx.Done():
		m.releaseAccountLock(accountID, lock)
		return nil, ctx.Err()
	}
}

// tryAcquireAccount lets scheduled work coalesce with an already-running
// manual or scheduled sync instead of waiting while it occupies global
// concurrency capacity.
func (m *Manager) tryAcquireAccount(accountID int64) (func(), bool) {
	m.locksMu.Lock()
	if _, busy := m.locks[accountID]; busy {
		m.locksMu.Unlock()
		return nil, false
	}
	lock := &accountLock{token: make(chan struct{}, 1), refs: 1}
	lock.token <- struct{}{}
	m.locks[accountID] = lock
	m.locksMu.Unlock()
	return m.accountLockRelease(accountID, lock), true
}

func (m *Manager) accountLockRelease(accountID int64, lock *accountLock) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			<-lock.token
			m.releaseAccountLock(accountID, lock)
		})
	}
}

func (m *Manager) acquireSyncSlot(ctx context.Context) (func(), error) {
	select {
	case m.syncSlots <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-m.syncSlots })
		}, nil
	case <-ctx.Done():
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
