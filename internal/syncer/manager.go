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

// ErrSyncQueued means a manual sync is running in the background, or an
// equivalent request for the same account is already queued.
var ErrSyncQueued = errors.New("mailbox sync queued")

const maxPersistedSyncErrorRunes = 8000

type accountLock struct {
	token chan struct{}
	refs  int
}

type activeMailboxSync struct {
	progress    domain.MailboxSyncProgress
	uidValidity uint32
	initialUID  uint32
	targetUID   uint32
	cursorSet   bool
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

	manualMu         sync.Mutex
	manualJobs       map[int64]struct{}
	manualDone       chan struct{}
	manualDoneClosed bool

	progressMu sync.RWMutex
	progress   map[int64]activeMailboxSync
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
		manualJobs:   make(map[int64]struct{}),
		manualDone:   make(chan struct{}),
		progress:     make(map[int64]activeMailboxSync),
	}
}

// AccountProgress returns a snapshot of the current process-local sync state.
// Completed and failed runs are removed; callers then use the persisted account
// sync status and timestamp for the terminal result.
func (m *Manager) AccountProgress(accountID int64) (domain.MailboxSyncProgress, bool) {
	m.progressMu.RLock()
	active, ok := m.progress[accountID]
	m.progressMu.RUnlock()
	if !ok {
		return domain.MailboxSyncProgress{}, false
	}
	return active.progress, true
}

func (m *Manager) SetSyncTimeout(timeout time.Duration) {
	if timeout > 0 {
		m.syncTimeout = timeout
	}
}

// BeginShutdown prevents cancellation caused by process shutdown from being
// persisted as an account synchronization failure.
func (m *Manager) BeginShutdown() {
	m.manualMu.Lock()
	m.stopping.Store(true)
	m.closeManualDoneLocked()
	m.manualMu.Unlock()
}

func (m *Manager) Run(ctx context.Context) {
	defer m.waitForManualJobs()
	defer m.clearProgress(domain.MailboxSyncTriggerAutomatic)
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

func (m *Manager) ensureProgress(accountID int64, trigger domain.MailboxSyncTrigger, phase domain.MailboxSyncPhase, percent int) {
	now := time.Now().UTC()
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	active, ok := m.progress[accountID]
	if ok && active.progress.Trigger != trigger {
		return
	}
	if !ok {
		active.progress = domain.MailboxSyncProgress{
			AccountID: accountID,
			Trigger:   trigger,
			StartedAt: now,
		}
	}
	active.progress.Phase = phase
	if percent > active.progress.Percent {
		active.progress.Percent = normalizedActivePercent(percent)
	}
	active.progress.UpdatedAt = now
	m.progress[accountID] = active
}

func (m *Manager) beginProgress(accountID int64, trigger domain.MailboxSyncTrigger, phase domain.MailboxSyncPhase, percent int) {
	now := time.Now().UTC()
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	active, ok := m.progress[accountID]
	if !ok || active.progress.Trigger != trigger {
		active = activeMailboxSync{progress: domain.MailboxSyncProgress{
			AccountID: accountID,
			Trigger:   trigger,
			StartedAt: now,
		}}
	}
	active.progress.Phase = phase
	if percent > active.progress.Percent {
		active.progress.Percent = normalizedActivePercent(percent)
	}
	active.progress.UpdatedAt = now
	m.progress[accountID] = active
}

func (m *Manager) reportProgress(accountID int64, trigger domain.MailboxSyncTrigger, update domain.MailboxSyncProgressUpdate) {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	active, ok := m.progress[accountID]
	if !ok || active.progress.Trigger != trigger {
		return
	}
	active.progress.Phase = update.Phase
	if update.Percent > active.progress.Percent {
		active.progress.Percent = normalizedActivePercent(update.Percent)
	}
	active.progress.UpdatedAt = time.Now().UTC()
	m.progress[accountID] = active
}

func (m *Manager) reportCursorProgress(
	accountID int64,
	trigger domain.MailboxSyncTrigger,
	previous *domain.IMAPSyncState,
	result domain.MailboxSyncResult,
) {
	now := time.Now().UTC()
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	active, ok := m.progress[accountID]
	if !ok || active.progress.Trigger != trigger {
		return
	}
	if !active.cursorSet || active.uidValidity != result.State.UIDValidity {
		active.uidValidity = result.State.UIDValidity
		active.initialUID = 0
		if previous != nil && previous.UIDValidity == result.State.UIDValidity {
			active.initialUID = previous.LastUID
		}
		active.targetUID = result.TargetUID
		active.cursorSet = true
	} else if result.TargetUID > active.targetUID {
		active.targetUID = result.TargetUID
	}

	percent := 95
	if result.HasMore && active.targetUID > active.initialUID {
		current := result.State.LastUID
		if current < active.initialUID {
			current = active.initialUID
		}
		completed := uint64(current - active.initialUID)
		total := uint64(active.targetUID - active.initialUID)
		if completed > total {
			completed = total
		}
		percent = 25 + int(completed*70/total)
	}
	if percent > active.progress.Percent {
		active.progress.Percent = normalizedActivePercent(percent)
	}
	active.progress.Phase = domain.MailboxSyncPhaseSaving
	active.progress.UpdatedAt = now
	m.progress[accountID] = active
}

func (m *Manager) finishProgress(accountID int64, trigger domain.MailboxSyncTrigger) {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	if active, ok := m.progress[accountID]; ok && active.progress.Trigger == trigger {
		delete(m.progress, accountID)
	}
}

func (m *Manager) clearProgress(trigger domain.MailboxSyncTrigger) {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	for accountID, active := range m.progress {
		if active.progress.Trigger == trigger {
			delete(m.progress, accountID)
		}
	}
}

func normalizedActivePercent(percent int) int {
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
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
		for accountID := range continuations {
			m.finishProgress(accountID, domain.MailboxSyncTriggerAutomatic)
		}
		return pending
	}
	if continuations != nil {
		enabled := make(accountIDSet, len(accounts))
		for _, account := range accounts {
			enabled[account.ID] = struct{}{}
		}
		for accountID := range continuations {
			if _, ok := enabled[accountID]; !ok {
				m.finishProgress(accountID, domain.MailboxSyncTriggerAutomatic)
			}
		}
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
				m.ensureProgress(account.ID, domain.MailboxSyncTriggerAutomatic, domain.MailboxSyncPhaseWaiting, 2)
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
					m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
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
			m.beginProgress(account.ID, domain.MailboxSyncTriggerAutomatic, domain.MailboxSyncPhaseWaiting, 2)

			waitCtx, cancelWait := m.withTimeout(ctx, m.syncTimeout)
			defer cancelWait()
			releaseSlot, err := m.acquireSyncSlot(waitCtx)
			if err != nil {
				m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
				markResult(account.ID, err)
				if !errors.Is(err, context.Canceled) {
					m.logger.Warn("主号同步失败", "account_id", account.ID, "error", err)
				}
				return
			}
			defer releaseSlot()
			if err := waitCtx.Err(); err != nil {
				m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
				markResult(account.ID, err)
				if !errors.Is(err, context.Canceled) {
					m.logger.Warn("主号同步失败", "account_id", account.ID, "error", err)
				}
				return
			}
			cancelWait()

			syncCtx, cancelSync := m.withTimeout(ctx, m.syncTimeout)
			defer cancelSync()
			syncErr := m.syncAccountLocked(syncCtx, account.ID, domain.MailboxSyncTriggerAutomatic)
			markResult(account.ID, syncErr)
			if !errors.Is(syncErr, ErrSyncPending) {
				m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
			}
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
	m.ensureProgress(accountID, domain.MailboxSyncTriggerManual, domain.MailboxSyncPhaseWaiting, 2)
	defer m.finishProgress(accountID, domain.MailboxSyncTriggerManual)
	waitCtx, cancelWait := m.withTimeout(ctx, m.syncTimeout)
	defer cancelWait()
	return m.WithAccountIMAPSlot(waitCtx, accountID, func() error {
		cancelWait()
		m.beginProgress(accountID, domain.MailboxSyncTriggerManual, domain.MailboxSyncPhaseWaiting, 2)
		syncCtx, cancelSync := m.withTimeout(ctx, m.syncTimeout)
		defer cancelSync()
		return m.syncAccountLocked(syncCtx, accountID, domain.MailboxSyncTriggerManual)
	})
}

// QueueAccountSync starts a deduplicated manual sync outside the HTTP request
// lifetime. Pending batches continue until the account catches up or the
// worker context ends.
func (m *Manager) QueueAccountSync(ctx context.Context, accountID int64) error {
	if ctx == nil {
		return errors.New("manual sync context must not be nil")
	}
	if accountID < 1 {
		return errors.New("manual sync account ID must be positive")
	}

	m.manualMu.Lock()
	if m.stopping.Load() {
		m.manualMu.Unlock()
		return context.Canceled
	}
	if _, exists := m.manualJobs[accountID]; exists {
		m.manualMu.Unlock()
		return ErrSyncQueued
	}
	if err := ctx.Err(); err != nil {
		m.manualMu.Unlock()
		return err
	}
	m.manualJobs[accountID] = struct{}{}
	m.manualMu.Unlock()
	m.ensureProgress(accountID, domain.MailboxSyncTriggerManual, domain.MailboxSyncPhaseQueued, 0)

	go m.runQueuedAccountSync(ctx, accountID)
	return ErrSyncQueued
}

func (m *Manager) runQueuedAccountSync(ctx context.Context, accountID int64) {
	defer m.finishManualJob(accountID)
	defer m.finishProgress(accountID, domain.MailboxSyncTriggerManual)
	for {
		syncErr := m.syncQueuedAccount(ctx, accountID)
		if errors.Is(syncErr, ErrSyncPending) {
			continue
		}
		if syncErr != nil && !errors.Is(syncErr, context.Canceled) {
			m.logger.Warn("queued account sync failed", "account_id", accountID, "error", syncErr)
		}
		return
	}
}

// syncQueuedAccount waits for the account and global IMAP slot until the
// worker context is canceled. Once both resources are held, mailbox work gets
// its own bounded timeout so a slow server cannot block shutdown forever.
func (m *Manager) syncQueuedAccount(ctx context.Context, accountID int64) error {
	m.ensureProgress(accountID, domain.MailboxSyncTriggerManual, domain.MailboxSyncPhaseWaiting, 2)
	return m.WithAccountIMAPSlot(ctx, accountID, func() error {
		m.beginProgress(accountID, domain.MailboxSyncTriggerManual, domain.MailboxSyncPhaseWaiting, 2)
		syncCtx, cancelSync := m.withTimeout(ctx, m.syncTimeout)
		defer cancelSync()
		return m.syncAccountLocked(syncCtx, accountID, domain.MailboxSyncTriggerManual)
	})
}

func (m *Manager) finishManualJob(accountID int64) {
	m.manualMu.Lock()
	delete(m.manualJobs, accountID)
	m.closeManualDoneLocked()
	m.manualMu.Unlock()
}

func (m *Manager) closeManualDoneLocked() {
	if !m.stopping.Load() || len(m.manualJobs) != 0 || m.manualDoneClosed {
		return
	}
	close(m.manualDone)
	m.manualDoneClosed = true
}

func (m *Manager) waitForManualJobs() {
	m.manualMu.Lock()
	if !m.stopping.Load() {
		m.manualMu.Unlock()
		return
	}
	done := m.manualDone
	m.manualMu.Unlock()
	<-done
}

func (m *Manager) SyncAccount(ctx context.Context, accountID int64) error {
	m.ensureProgress(accountID, domain.MailboxSyncTriggerManual, domain.MailboxSyncPhaseWaiting, 2)
	defer m.finishProgress(accountID, domain.MailboxSyncTriggerManual)
	return m.WithAccountIMAPSlot(ctx, accountID, func() error {
		m.beginProgress(accountID, domain.MailboxSyncTriggerManual, domain.MailboxSyncPhaseWaiting, 2)
		return m.syncAccountLocked(ctx, accountID, domain.MailboxSyncTriggerManual)
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

func (m *Manager) syncAccountLocked(
	ctx context.Context,
	accountID int64,
	trigger domain.MailboxSyncTrigger,
) (syncErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	account, err := m.repo.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if !account.Enabled {
		return store.ErrAccountDisabled
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
	fetchCtx := domain.WithMailboxSyncProgressReporter(ctx, func(update domain.MailboxSyncProgressUpdate) {
		m.reportProgress(accountID, trigger, update)
	})
	result, err := m.fetcher.FetchIncremental(fetchCtx, account, password, enabled, previousState, nil)
	password = ""
	if err != nil {
		return fmt.Errorf("fetch IMAP mailbox increment: %w", err)
	}
	m.reportCursorProgress(accountID, trigger, previousState, result)
	now := time.Now().UTC()
	if err := m.repo.ApplyMailboxSync(ctx, accountID, account.UpdatedAt, enabled, result, now); err != nil {
		return fmt.Errorf("批量保存 IMAP 同步结果: %w", err)
	}
	if result.HasMore {
		return ErrSyncPending
	}
	m.reportProgress(accountID, trigger, domain.MailboxSyncProgressUpdate{
		Phase:   domain.MailboxSyncPhaseSaving,
		Percent: 100,
	})
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
	if len(messageRunes) > maxPersistedSyncErrorRunes {
		messageRunes = messageRunes[:maxPersistedSyncErrorRunes]
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
