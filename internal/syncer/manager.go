package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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

var errRetryWindowElapsed = errors.New("automatic mailbox retry window elapsed")

const (
	maxPersistedSyncErrorRunes = 8000
	// The default sync budget is 70s. Keep transient recovery within a 15s
	// window so the default automatic cycle stays inside the external 90s limit.
	automaticIMAPRetryBudget = 15 * time.Second
)

var defaultAutomaticIMAPRetryDelays = []time.Duration{time.Second, 3 * time.Second}

type accountLock struct {
	token chan struct{}
	refs  int
}

type activeMailboxSync struct {
	progress    domain.MailboxSyncProgress
	runID       string
	batch       int
	batchStart  time.Time
	uidValidity uint32
	initialUID  uint32
	targetUID   uint32
	cursorSet   bool
}

type syncFlowSeed struct {
	runID     string
	startedAt time.Time
}

type syncFlowSnapshot struct {
	runID        string
	batch        int
	stage        domain.MailboxSyncPhase
	percent      int
	startedAt    time.Time
	batchStarted time.Time
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
	retryDelays  []time.Duration
	retryBudget  time.Duration

	locksMu  sync.Mutex
	locks    map[int64]*accountLock
	stopping atomic.Bool

	manualMu         sync.Mutex
	manualJobs       map[int64]syncFlowSeed
	manualDone       chan struct{}
	manualDoneClosed bool

	progressMu sync.RWMutex
	progress   map[int64]activeMailboxSync
	runSeq     atomic.Uint64
}

func New(repo Repository, cipher CredentialCipher, fetcher MailFetcher, logger *slog.Logger, interval time.Duration, concurrency int) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
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
		retryDelays:  append([]time.Duration(nil), defaultAutomaticIMAPRetryDelays...),
		retryBudget:  automaticIMAPRetryBudget,
		locks:        make(map[int64]*accountLock),
		manualJobs:   make(map[int64]syncFlowSeed),
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
	var retryQueue accountIDSet
	var retryCtx context.Context
	var cancelRetry context.CancelFunc
	retryAttempt := 0
	clearRetryCycle := func() {
		if cancelRetry != nil {
			cancelRetry()
		}
		retryQueue = nil
		retryCtx = nil
		cancelRetry = nil
		retryAttempt = 0
	}
	defer clearRetryCycle()
	for {
		workCtx := ctx
		if retryCtx != nil {
			workCtx = retryCtx
		}
		// The worker context owns resource-wait timeouts. A short retry context
		// can stop a queued retry early and bounds mailbox recovery work.
		result := m.syncAllRoundDetailedWithContexts(ctx, workCtx, retryCtx, continuations)
		if ctx.Err() != nil {
			if cancelRetry != nil {
				cancelRetry()
			}
			return
		}
		retryQueue = mergeAccountIDSets(retryQueue, result.retryable)
		retryWindowDone := retryCtx != nil && retryWindowExpiredFromContext(retryCtx)
		if len(result.pending) > 0 && !retryWindowDone {
			continuations = result.pending
			continue
		}
		if retryWindowDone {
			m.deferContinuations(ctx, result.pending, "continue_batch")
		}
		continuations = nil

		if !retryWindowDone && len(retryQueue) > 0 && retryAttempt < len(m.retryDelays) {
			if retryCtx == nil {
				retryCtx, cancelRetry = context.WithTimeoutCause(ctx, m.retryBudget, errRetryWindowElapsed)
			}
			delay := m.retryDelays[retryAttempt]
			m.logger.Info(
				"瞬时 IMAP 连接失败，正在安排自动重试",
				"attempt", retryAttempt+1,
				"delay", delay,
				"account_count", len(retryQueue),
			)
			if m.waitInterval(retryCtx, delay) {
				retryAttempt++
				continuations = retryQueue
				retryQueue = nil
				continue
			}
			if ctx.Err() != nil || retryCtx.Err() == nil {
				if cancelRetry != nil {
					cancelRetry()
				}
				return
			}
		}

		clearRetryCycle()
		if !m.waitInterval(ctx, m.interval) {
			if cancelRetry != nil {
				cancelRetry()
			}
			return
		}
	}
}

func (m *Manager) deferContinuations(ctx context.Context, continuations accountIDSet, operation string) {
	for accountID := range continuations {
		flow := m.ensureProgress(
			accountID,
			domain.MailboxSyncTriggerAutomatic,
			domain.MailboxSyncPhaseWaiting,
			2,
			m.newSyncFlowSeed(),
		)
		m.logSyncDeferred(
			ctx,
			accountID,
			domain.MailboxSyncTriggerAutomatic,
			flow,
			operation,
		)
		m.finishProgress(accountID, domain.MailboxSyncTriggerAutomatic)
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

func (m *Manager) newSyncFlowSeed() syncFlowSeed {
	now := time.Now().UTC()
	sequence := m.runSeq.Add(1)
	return syncFlowSeed{
		runID:     fmt.Sprintf("sync-%016x-%08x", uint64(now.UnixNano()), sequence),
		startedAt: now,
	}
}

func (m *Manager) ensureProgress(
	accountID int64,
	trigger domain.MailboxSyncTrigger,
	phase domain.MailboxSyncPhase,
	percent int,
	seed syncFlowSeed,
) syncFlowSnapshot {
	now := time.Now().UTC()
	m.progressMu.Lock()
	active, ok := m.progress[accountID]
	if ok && active.progress.Trigger != trigger {
		m.progressMu.Unlock()
		return syncFlowSnapshot{
			runID:     seed.runID,
			batch:     1,
			stage:     phase,
			percent:   normalizedActivePercent(percent),
			startedAt: seed.startedAt,
		}
	}
	if !ok {
		if seed.runID == "" {
			seed = m.newSyncFlowSeed()
		}
		if seed.startedAt.IsZero() {
			seed.startedAt = now
		}
		active = activeMailboxSync{
			runID: seed.runID,
			progress: domain.MailboxSyncProgress{
				AccountID: accountID,
				Trigger:   trigger,
				StartedAt: seed.startedAt,
			},
		}
	}
	active.progress.Phase = phase
	if percent > active.progress.Percent {
		active.progress.Percent = normalizedActivePercent(percent)
	}
	active.progress.UpdatedAt = now
	m.progress[accountID] = active
	snapshot := syncFlowSnapshotFromActive(active, true)
	m.progressMu.Unlock()
	return snapshot
}

func (m *Manager) beginProgress(
	accountID int64,
	trigger domain.MailboxSyncTrigger,
	phase domain.MailboxSyncPhase,
	percent int,
	seed syncFlowSeed,
) syncFlowSnapshot {
	now := time.Now().UTC()
	m.progressMu.Lock()
	active, ok := m.progress[accountID]
	if !ok || active.progress.Trigger != trigger {
		if seed.runID == "" {
			seed = m.newSyncFlowSeed()
		}
		if seed.startedAt.IsZero() {
			seed.startedAt = now
		}
		active = activeMailboxSync{progress: domain.MailboxSyncProgress{
			AccountID: accountID,
			Trigger:   trigger,
			StartedAt: seed.startedAt,
		}, runID: seed.runID}
	}
	active.batch++
	active.batchStart = now
	active.progress.Phase = phase
	if percent > active.progress.Percent {
		active.progress.Percent = normalizedActivePercent(percent)
	}
	active.progress.UpdatedAt = now
	m.progress[accountID] = active
	snapshot := syncFlowSnapshotFromActive(active, false)
	m.progressMu.Unlock()
	return snapshot
}

func (m *Manager) reportProgress(accountID int64, trigger domain.MailboxSyncTrigger, update domain.MailboxSyncProgressUpdate) {
	m.progressMu.Lock()
	active, ok := m.progress[accountID]
	if !ok || active.progress.Trigger != trigger {
		m.progressMu.Unlock()
		return
	}
	active.progress.Phase = update.Phase
	if update.Percent > active.progress.Percent {
		active.progress.Percent = normalizedActivePercent(update.Percent)
	}
	active.progress.UpdatedAt = time.Now().UTC()
	m.progress[accountID] = active
	snapshot := syncFlowSnapshotFromActive(active, false)
	m.progressMu.Unlock()
	m.logSyncFlow(
		context.Background(),
		slog.LevelDebug,
		syncStageMessage(update.Phase),
		accountID,
		trigger,
		"stage_started",
		snapshot,
	)
}

func (m *Manager) reportCursorProgress(
	accountID int64,
	trigger domain.MailboxSyncTrigger,
	previous *domain.IMAPSyncState,
	result domain.MailboxSyncResult,
) (syncFlowSnapshot, bool) {
	now := time.Now().UTC()
	m.progressMu.Lock()
	active, ok := m.progress[accountID]
	if !ok || active.progress.Trigger != trigger {
		m.progressMu.Unlock()
		return syncFlowSnapshot{}, false
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
	snapshot := syncFlowSnapshotFromActive(active, false)
	m.progressMu.Unlock()
	return snapshot, true
}

func syncFlowSnapshotFromActive(active activeMailboxSync, nextBatch bool) syncFlowSnapshot {
	batch := active.batch
	batchStarted := active.batchStart
	if nextBatch {
		batch++
		batchStarted = time.Time{}
	} else if batch < 1 {
		batch = 1
	}
	return syncFlowSnapshot{
		runID:        active.runID,
		batch:        batch,
		stage:        active.progress.Phase,
		percent:      active.progress.Percent,
		startedAt:    active.progress.StartedAt,
		batchStarted: batchStarted,
	}
}

func (m *Manager) currentSyncFlow(accountID int64, trigger domain.MailboxSyncTrigger) (syncFlowSnapshot, bool) {
	m.progressMu.RLock()
	active, ok := m.progress[accountID]
	m.progressMu.RUnlock()
	if !ok || active.progress.Trigger != trigger {
		return syncFlowSnapshot{}, false
	}
	return syncFlowSnapshotFromActive(active, false), true
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

type syncAllRoundResult struct {
	pending   accountIDSet
	retryable accountIDSet
}

func mergeAccountIDSets(destination, source accountIDSet) accountIDSet {
	if len(source) == 0 {
		return destination
	}
	if destination == nil {
		destination = make(accountIDSet, len(source))
	}
	for accountID := range source {
		destination[accountID] = struct{}{}
	}
	return destination
}

func retryWindowExpired(queueCtx context.Context, err error) bool {
	return queueCtx.Err() == nil && errors.Is(err, errRetryWindowElapsed)
}

func resourceWaitExpired(queueCtx context.Context, err error) bool {
	return queueCtx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
}

func retryWindowExpiredFromQueue(queueCtx, workCtx context.Context) bool {
	return queueCtx.Err() == nil && workCtx != nil &&
		errors.Is(context.Cause(workCtx), errRetryWindowElapsed)
}

func retryWindowExpiredFromContext(ctx context.Context) bool {
	return ctx != nil && errors.Is(context.Cause(ctx), errRetryWindowElapsed)
}

func retryWindowInterrupted(ctx context.Context, err error) bool {
	if !retryWindowExpiredFromContext(ctx) {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, errRetryWindowElapsed)
}

// syncAllRound processes at most one batch per eligible account. A previous
// continuation waits for its account lock with a bounded budget so manual and
// seen work neither lose the continuation nor cause a busy retry loop.
func (m *Manager) syncAllRound(ctx context.Context, continuations accountIDSet) accountIDSet {
	return m.syncAllRoundDetailed(ctx, continuations).pending
}

func (m *Manager) syncAllRoundDetailed(ctx context.Context, continuations accountIDSet) syncAllRoundResult {
	return m.syncAllRoundDetailedWithContexts(ctx, ctx, nil, continuations)
}

// syncAllRoundDetailedWithContexts keeps resource queueing separate from the
// context that bounds mailbox work. retryCtx is also used as an early-stop
// signal for a fast retry that is still waiting for a resource; that attempt
// is deferred to the normal poll instead of being reported as a sync failure.
func (m *Manager) syncAllRoundDetailedWithContexts(
	queueCtx context.Context,
	workCtx context.Context,
	retryCtx context.Context,
	continuations accountIDSet,
) syncAllRoundResult {
	result := syncAllRoundResult{
		pending:   make(accountIDSet),
		retryable: make(accountIDSet),
	}
	accounts, err := m.repo.ListEnabledAccounts(workCtx)
	if err != nil {
		if queueCtx.Err() == nil && retryWindowInterrupted(workCtx, err) {
			m.deferContinuations(queueCtx, continuations, "list_enabled_accounts")
			return result
		}
		m.logger.Error("读取待同步主号失败", "error", err)
		for accountID := range continuations {
			if flow, ok := m.currentSyncFlow(accountID, domain.MailboxSyncTriggerAutomatic); ok {
				m.logSyncFailure(
					queueCtx,
					accountID,
					domain.MailboxSyncTriggerAutomatic,
					flow,
					"list_enabled_accounts",
					err,
				)
			}
			m.finishProgress(accountID, domain.MailboxSyncTriggerAutomatic)
		}
		return result
	}
	ctx := queueCtx
	if continuations != nil {
		enabled := make(accountIDSet, len(accounts))
		for _, account := range accounts {
			enabled[account.ID] = struct{}{}
		}
		for accountID := range continuations {
			if _, ok := enabled[accountID]; !ok {
				if flow, active := m.currentSyncFlow(accountID, domain.MailboxSyncTriggerAutomatic); active {
					m.logSyncCancellation(
						ctx,
						accountID,
						domain.MailboxSyncTriggerAutomatic,
						flow,
						flow.stage,
						"account_no_longer_enabled",
						"账号已停用或已从自动同步列表移除",
					)
				}
				m.finishProgress(accountID, domain.MailboxSyncTriggerAutomatic)
			}
		}
	}
	var statusMu sync.Mutex
	markResult := func(accountID int64, syncErr error) {
		statusMu.Lock()
		defer statusMu.Unlock()
		switch {
		case errors.Is(syncErr, ErrSyncPending):
			result.pending[accountID] = struct{}{}
		case isTransientIMAPSyncFailure(syncErr):
			result.retryable[accountID] = struct{}{}
		}
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

			seed := m.newSyncFlowSeed()
			if active, ok := m.currentSyncFlow(account.ID, domain.MailboxSyncTriggerAutomatic); ok {
				seed = syncFlowSeed{runID: active.runID, startedAt: active.startedAt}
			}
			var release func()
			if continuing {
				waiting := m.ensureProgress(
					account.ID,
					domain.MailboxSyncTriggerAutomatic,
					domain.MailboxSyncPhaseWaiting,
					2,
					seed,
				)
				m.logSyncFlow(
					ctx,
					slog.LevelDebug,
					"邮件同步正在等待账号锁",
					account.ID,
					domain.MailboxSyncTriggerAutomatic,
					"waiting",
					waiting,
					slog.String("wait_for", "account_lock"),
				)
				lockCtx, cancelLock := m.withTimeout(queueCtx, m.syncTimeout)
				var err error
				release, err = m.acquireAccountUntil(lockCtx, retryCtx, account.ID)
				if err == nil {
					err = lockCtx.Err()
				}
				cancelLock()
				if err != nil {
					if release != nil {
						release()
					}
					if retryWindowExpired(queueCtx, err) {
						m.logSyncDeferred(
							ctx,
							account.ID,
							domain.MailboxSyncTriggerAutomatic,
							waiting,
							"wait_account_lock",
						)
						m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
						return
					}
					if resourceWaitExpired(queueCtx, err) {
						m.logSyncDeferredWithReason(
							ctx,
							account.ID,
							domain.MailboxSyncTriggerAutomatic,
							waiting,
							"wait_account_lock",
							"resource_wait_timeout",
						)
						m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
						return
					}
					m.logSyncFailure(
						ctx,
						account.ID,
						domain.MailboxSyncTriggerAutomatic,
						waiting,
						"wait_account_lock",
						err,
					)
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
			waiting := m.ensureProgress(
				account.ID,
				domain.MailboxSyncTriggerAutomatic,
				domain.MailboxSyncPhaseWaiting,
				2,
				seed,
			)
			m.logSyncFlow(
				ctx,
				slog.LevelDebug,
				"邮件同步正在等待全局连接名额",
				account.ID,
				domain.MailboxSyncTriggerAutomatic,
				"waiting",
				waiting,
				slog.String("wait_for", "global_imap_slot"),
			)

			waitCtx, cancelWait := m.withTimeout(queueCtx, m.syncTimeout)
			defer cancelWait()
			releaseSlot, err := m.acquireSyncSlotUntil(waitCtx, retryCtx)
			if err != nil {
				if retryWindowExpired(queueCtx, err) {
					m.logSyncDeferred(
						ctx,
						account.ID,
						domain.MailboxSyncTriggerAutomatic,
						waiting,
						"wait_global_imap_slot",
					)
					m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
					return
				}
				if resourceWaitExpired(queueCtx, err) {
					m.logSyncDeferredWithReason(
						ctx,
						account.ID,
						domain.MailboxSyncTriggerAutomatic,
						waiting,
						"wait_global_imap_slot",
						"resource_wait_timeout",
					)
					m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
					return
				}
				m.logSyncFailure(
					ctx,
					account.ID,
					domain.MailboxSyncTriggerAutomatic,
					waiting,
					"wait_global_imap_slot",
					err,
				)
				m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
				markResult(account.ID, err)
				return
			}
			defer releaseSlot()
			if err := waitCtx.Err(); err != nil {
				if resourceWaitExpired(queueCtx, err) {
					m.logSyncDeferredWithReason(
						ctx,
						account.ID,
						domain.MailboxSyncTriggerAutomatic,
						waiting,
						"wait_global_imap_slot",
						"resource_wait_timeout",
					)
					m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
					return
				}
				m.logSyncFailure(
					ctx,
					account.ID,
					domain.MailboxSyncTriggerAutomatic,
					waiting,
					"wait_global_imap_slot",
					err,
				)
				m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
				markResult(account.ID, err)
				return
			}
			cancelWait()
			if err := workCtx.Err(); err != nil {
				if retryWindowExpiredFromQueue(queueCtx, workCtx) {
					m.logSyncDeferred(
						ctx,
						account.ID,
						domain.MailboxSyncTriggerAutomatic,
						waiting,
						"start_retry_sync",
					)
					m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
					return
				}
				m.logSyncFailure(
					ctx,
					account.ID,
					domain.MailboxSyncTriggerAutomatic,
					waiting,
					"start_sync",
					err,
				)
				m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
				markResult(account.ID, err)
				return
			}

			flow := m.beginProgress(
				account.ID,
				domain.MailboxSyncTriggerAutomatic,
				domain.MailboxSyncPhasePreparing,
				3,
				seed,
			)
			m.logSyncBatchStarted(ctx, account.ID, domain.MailboxSyncTriggerAutomatic, flow)
			syncCtx, cancelSync := m.withTimeout(workCtx, m.syncTimeout)
			defer cancelSync()
			syncErr := m.syncAccountLocked(syncCtx, account.ID, domain.MailboxSyncTriggerAutomatic, flow)
			markResult(account.ID, syncErr)
			if !errors.Is(syncErr, ErrSyncPending) {
				m.finishProgress(account.ID, domain.MailboxSyncTriggerAutomatic)
			}
		}()
	}
	wg.Wait()
	return result
}

// SyncAccountWithTimeout bounds queueing and mailbox work separately so a busy
// account or IMAP slot does not consume the mailbox operation's full budget.
func (m *Manager) SyncAccountWithTimeout(ctx context.Context, accountID int64) error {
	seed := m.newSyncFlowSeed()
	waiting := m.ensureProgress(
		accountID,
		domain.MailboxSyncTriggerManual,
		domain.MailboxSyncPhaseWaiting,
		2,
		seed,
	)
	m.logSyncFlow(
		ctx,
		slog.LevelDebug,
		"邮件同步正在等待执行资源",
		accountID,
		domain.MailboxSyncTriggerManual,
		"waiting",
		waiting,
		slog.String("wait_for", "account_lock_and_global_imap_slot"),
	)
	defer m.finishProgress(accountID, domain.MailboxSyncTriggerManual)
	waitCtx, cancelWait := m.withTimeout(ctx, m.syncTimeout)
	defer cancelWait()
	started := false
	err := m.WithAccountIMAPSlot(waitCtx, accountID, func() error {
		cancelWait()
		flow := m.beginProgress(
			accountID,
			domain.MailboxSyncTriggerManual,
			domain.MailboxSyncPhasePreparing,
			3,
			seed,
		)
		started = true
		m.logSyncBatchStarted(ctx, accountID, domain.MailboxSyncTriggerManual, flow)
		syncCtx, cancelSync := m.withTimeout(ctx, m.syncTimeout)
		defer cancelSync()
		return m.syncAccountLocked(syncCtx, accountID, domain.MailboxSyncTriggerManual, flow)
	})
	if err != nil && !started {
		m.logSyncFailure(ctx, accountID, domain.MailboxSyncTriggerManual, waiting, "wait_sync_resources", err)
	}
	return err
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
	seed := m.newSyncFlowSeed()
	m.manualJobs[accountID] = seed
	m.manualMu.Unlock()
	queued := m.ensureProgress(
		accountID,
		domain.MailboxSyncTriggerManual,
		domain.MailboxSyncPhaseQueued,
		0,
		seed,
	)
	m.logSyncFlow(
		ctx,
		slog.LevelInfo,
		"邮件同步已排队",
		accountID,
		domain.MailboxSyncTriggerManual,
		"run_queued",
		queued,
	)

	go m.runQueuedAccountSync(ctx, accountID, seed)
	return ErrSyncQueued
}

func (m *Manager) runQueuedAccountSync(ctx context.Context, accountID int64, seed syncFlowSeed) {
	defer m.finishManualJob(accountID)
	defer m.finishProgress(accountID, domain.MailboxSyncTriggerManual)
	for {
		syncErr := m.syncQueuedAccount(ctx, accountID, seed)
		if errors.Is(syncErr, ErrSyncPending) {
			continue
		}
		return
	}
}

// syncQueuedAccount waits for the account and global IMAP slot until the
// worker context is canceled. Once both resources are held, mailbox work gets
// its own bounded timeout so a slow server cannot block shutdown forever.
func (m *Manager) syncQueuedAccount(ctx context.Context, accountID int64, seed syncFlowSeed) error {
	waiting := m.ensureProgress(
		accountID,
		domain.MailboxSyncTriggerManual,
		domain.MailboxSyncPhaseWaiting,
		2,
		seed,
	)
	m.logSyncFlow(
		ctx,
		slog.LevelDebug,
		"邮件同步正在等待执行资源",
		accountID,
		domain.MailboxSyncTriggerManual,
		"waiting",
		waiting,
		slog.String("wait_for", "account_lock_and_global_imap_slot"),
	)
	started := false
	err := m.WithAccountIMAPSlot(ctx, accountID, func() error {
		flow := m.beginProgress(
			accountID,
			domain.MailboxSyncTriggerManual,
			domain.MailboxSyncPhasePreparing,
			3,
			seed,
		)
		started = true
		m.logSyncBatchStarted(ctx, accountID, domain.MailboxSyncTriggerManual, flow)
		syncCtx, cancelSync := m.withTimeout(ctx, m.syncTimeout)
		defer cancelSync()
		return m.syncAccountLocked(syncCtx, accountID, domain.MailboxSyncTriggerManual, flow)
	})
	if err != nil && !started {
		m.logSyncFailure(ctx, accountID, domain.MailboxSyncTriggerManual, waiting, "wait_sync_resources", err)
	}
	return err
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
	seed := m.newSyncFlowSeed()
	waiting := m.ensureProgress(
		accountID,
		domain.MailboxSyncTriggerManual,
		domain.MailboxSyncPhaseWaiting,
		2,
		seed,
	)
	m.logSyncFlow(
		ctx,
		slog.LevelDebug,
		"邮件同步正在等待执行资源",
		accountID,
		domain.MailboxSyncTriggerManual,
		"waiting",
		waiting,
		slog.String("wait_for", "account_lock_and_global_imap_slot"),
	)
	defer m.finishProgress(accountID, domain.MailboxSyncTriggerManual)
	started := false
	err := m.WithAccountIMAPSlot(ctx, accountID, func() error {
		flow := m.beginProgress(
			accountID,
			domain.MailboxSyncTriggerManual,
			domain.MailboxSyncPhasePreparing,
			3,
			seed,
		)
		started = true
		m.logSyncBatchStarted(ctx, accountID, domain.MailboxSyncTriggerManual, flow)
		return m.syncAccountLocked(ctx, accountID, domain.MailboxSyncTriggerManual, flow)
	})
	if err != nil && !started {
		m.logSyncFailure(ctx, accountID, domain.MailboxSyncTriggerManual, waiting, "wait_sync_resources", err)
	}
	return err
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
	flow syncFlowSnapshot,
) (syncErr error) {
	failedOperation := "check_context"
	sensitiveValues := make([]string, 0, 4)
	defer func() {
		if syncErr != nil && !errors.Is(syncErr, ErrSyncPending) &&
			retryWindowInterrupted(ctx, syncErr) {
			m.logSyncDeferred(ctx, accountID, trigger, flow, failedOperation)
			return
		}
		m.logSyncFailure(ctx, accountID, trigger, flow, failedOperation, syncErr, sensitiveValues...)
	}()
	if err := ctx.Err(); err != nil {
		return err
	}

	failedOperation = "load_account"
	account, err := m.repo.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	sensitiveValues = append(sensitiveValues, account.Email, account.PasswordCiphertext)
	failedOperation = "validate_account_enabled"
	if !account.Enabled {
		return store.ErrAccountDisabled
	}
	m.logSyncFlow(
		ctx,
		slog.LevelDebug,
		"已读取邮件同步账号配置",
		accountID,
		trigger,
		"account_loaded",
		flow,
	)
	failures := newFailureRecorder(m, ctx, accountID, account.UpdatedAt)
	defer failures.close()
	defer func() {
		if syncErr != nil && !errors.Is(syncErr, ErrSyncPending) &&
			!retryWindowInterrupted(ctx, syncErr) {
			failures.record(syncErr)
		}
	}()

	failedOperation = "load_enabled_aliases"
	enabled, err := m.repo.ListEnabledAliasesByAccount(ctx, accountID)
	if err != nil {
		return err
	}
	for _, alias := range enabled {
		sensitiveValues = append(sensitiveValues, alias.Address)
	}
	failedOperation = "load_incremental_cursor"
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
	preparedAttrs := []slog.Attr{
		slog.Int("enabled_alias_count", len(enabled)),
		slog.Bool("has_previous_cursor", previousState != nil),
	}
	if previousState != nil {
		preparedAttrs = append(
			preparedAttrs,
			slog.Uint64("uid_validity", uint64(previousState.UIDValidity)),
			slog.Uint64("cursor_uid", uint64(previousState.LastUID)),
		)
	}
	m.logSyncFlow(
		ctx,
		slog.LevelDebug,
		"邮件同步范围准备完成",
		accountID,
		trigger,
		"prepared",
		flow,
		preparedAttrs...,
	)

	failedOperation = "decrypt_credentials"
	password, err := m.cipher.Decrypt(account.PasswordCiphertext)
	if err != nil {
		return fmt.Errorf("解密 IMAP 凭据: %w", err)
	}
	sensitiveValues = append(sensitiveValues, password)
	fetchCtx := domain.WithMailboxSyncProgressReporter(ctx, func(update domain.MailboxSyncProgressUpdate) {
		m.reportProgress(accountID, trigger, update)
	})
	failedOperation = "fetch_incremental"
	result, err := m.fetcher.FetchIncremental(fetchCtx, account, password, enabled, previousState, nil)
	password = ""
	if err != nil {
		wrapped := fmt.Errorf("fetch IMAP mailbox increment: %w", err)
		if isTransientIMAPTransportError(err) {
			return &transientIMAPSyncError{cause: wrapped}
		}
		return wrapped
	}
	saving, ok := m.reportCursorProgress(accountID, trigger, previousState, result)
	if !ok {
		saving = flow
		saving.stage = domain.MailboxSyncPhaseSaving
	}
	m.logSyncFlow(
		ctx,
		slog.LevelDebug,
		"正在保存同步结果",
		accountID,
		trigger,
		"stage_started",
		saving,
		slog.Int("result_count", len(result.Messages)),
		slog.Uint64("cursor_uid", uint64(result.State.LastUID)),
		slog.Uint64("target_uid", uint64(result.TargetUID)),
		slog.Bool("has_more", result.HasMore),
		slog.Bool("cursor_reset", result.Reset),
	)
	failedOperation = "save_result"
	now := time.Now().UTC()
	if err := m.repo.ApplyMailboxSync(ctx, accountID, account.UpdatedAt, enabled, result, now); err != nil {
		return fmt.Errorf("批量保存 IMAP 同步结果: %w", err)
	}
	m.logSyncFlow(
		ctx,
		slog.LevelDebug,
		"邮件同步批次已保存",
		accountID,
		trigger,
		"batch_saved",
		saving,
		slog.Int("result_count", len(result.Messages)),
		slog.Uint64("cursor_uid", uint64(result.State.LastUID)),
		slog.Uint64("target_uid", uint64(result.TargetUID)),
		slog.Bool("has_more", result.HasMore),
	)
	if result.HasMore {
		m.logSyncFlow(
			ctx,
			slog.LevelInfo,
			"邮件同步批次完成，仍有邮件等待后续批次",
			accountID,
			trigger,
			"batch_completed",
			saving,
			slog.Int("next_batch", saving.batch+1),
			slog.Uint64("cursor_uid", uint64(result.State.LastUID)),
			slog.Uint64("target_uid", uint64(result.TargetUID)),
		)
		return ErrSyncPending
	}
	failedOperation = "complete_run"
	m.reportProgress(accountID, trigger, domain.MailboxSyncProgressUpdate{
		Phase:   domain.MailboxSyncPhaseSaving,
		Percent: 100,
	})
	completed, ok := m.currentSyncFlow(accountID, trigger)
	if !ok {
		completed = saving
		completed.percent = 100
	}
	completed.stage = domain.MailboxSyncPhase("completed")
	m.logSyncFlow(
		ctx,
		slog.LevelInfo,
		"邮件同步完成",
		accountID,
		trigger,
		"run_completed",
		completed,
		slog.Int("result_count", len(result.Messages)),
		slog.Uint64("cursor_uid", uint64(result.State.LastUID)),
		slog.Uint64("target_uid", uint64(result.TargetUID)),
	)
	return nil
}

type transientIMAPSyncError struct {
	cause error
}

func (e *transientIMAPSyncError) Error() string { return e.cause.Error() }

func (e *transientIMAPSyncError) Unwrap() error { return e.cause }

func isTransientIMAPSyncFailure(err error) bool {
	var transient *transientIMAPSyncError
	return errors.As(err, &transient)
}

func isTransientIMAPTransportError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary()) {
		return true
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		message := strings.ToLower(strings.TrimSpace(cause.Error()))
		switch message {
		case "imap: connection closed", "imap: connection closed during command execution":
			return true
		}
		if strings.Contains(message, "imap: connection closed") ||
			strings.Contains(message, "unexpected eof") ||
			strings.Contains(message, "i/o timeout") ||
			strings.Contains(message, "connection timed out") ||
			strings.Contains(message, "operation timed out") ||
			strings.Contains(message, "connection reset") ||
			strings.Contains(message, "broken pipe") ||
			strings.Contains(message, "forcibly closed") ||
			strings.Contains(message, "connection was aborted") ||
			strings.Contains(message, "wsasend") ||
			strings.Contains(message, "wsarecv") {
			return true
		}
	}
	return false
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
	return m.acquireAccountUntil(ctx, nil, accountID)
}

func (m *Manager) acquireAccountUntil(ctx, stopCtx context.Context, accountID int64) (func(), error) {
	m.locksMu.Lock()
	lock, ok := m.locks[accountID]
	if !ok {
		lock = &accountLock{token: make(chan struct{}, 1)}
		m.locks[accountID] = lock
	}
	lock.refs++
	m.locksMu.Unlock()
	var stopDone <-chan struct{}
	if stopCtx != nil {
		stopDone = stopCtx.Done()
	}
	select {
	case lock.token <- struct{}{}:
		return m.accountLockRelease(accountID, lock), nil
	case <-ctx.Done():
		m.releaseAccountLock(accountID, lock)
		return nil, contextTerminationError(ctx)
	case <-stopDone:
		m.releaseAccountLock(accountID, lock)
		if ctx.Err() != nil {
			return nil, contextTerminationError(ctx)
		}
		return nil, contextTerminationError(stopCtx)
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
	return m.acquireSyncSlotUntil(ctx, nil)
}

func (m *Manager) acquireSyncSlotUntil(ctx, stopCtx context.Context) (func(), error) {
	var stopDone <-chan struct{}
	if stopCtx != nil {
		stopDone = stopCtx.Done()
	}
	select {
	case m.syncSlots <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-m.syncSlots })
		}, nil
	case <-ctx.Done():
		return nil, contextTerminationError(ctx)
	case <-stopDone:
		if ctx.Err() != nil {
			return nil, contextTerminationError(ctx)
		}
		return nil, contextTerminationError(stopCtx)
	}
}

func contextTerminationError(ctx context.Context) error {
	if errors.Is(context.Cause(ctx), errRetryWindowElapsed) {
		return errRetryWindowElapsed
	}
	return ctx.Err()
}

func (m *Manager) releaseAccountLock(accountID int64, lock *accountLock) {
	m.locksMu.Lock()
	defer m.locksMu.Unlock()
	lock.refs--
	if lock.refs == 0 && m.locks[accountID] == lock {
		delete(m.locks, accountID)
	}
}
