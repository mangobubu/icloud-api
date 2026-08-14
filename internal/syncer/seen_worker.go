package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/mail"
)

const (
	defaultSeenTaskBatchSize    = 256
	defaultSeenAccountWorkers   = 16
	defaultSeenPollInterval     = time.Minute
	defaultSeenAcquireTimeout   = 30 * time.Second
	defaultSeenOperationTimeout = 2 * time.Minute
)

// SeenTaskRepository persists the durable handoff between a successful API
// read and the asynchronous IMAP flag update.
type SeenTaskRepository interface {
	GetAccount(context.Context, int64) (domain.Account, error)
	ListSeenTaskAccountIDs(context.Context) ([]int64, error)
	ListSeenTasks(context.Context, int64, int) ([]domain.SeenTask, error)
	DeleteSeenTasks(context.Context, int64, uint32, []uint32) error
}

type SeenMarker interface {
	MarkSeen(context.Context, domain.Account, string, uint32, []uint32) error
}

type SeenAccountLocker interface {
	WithAccountIMAPSlot(context.Context, int64, func() error) error
}

type SeenWorker struct {
	repo             SeenTaskRepository
	cipher           CredentialCipher
	marker           SeenMarker
	locker           SeenAccountLocker
	logger           *slog.Logger
	interval         time.Duration
	batchSize        int
	accountWorkers   int
	acquireTimeout   time.Duration
	operationTimeout time.Duration
	notifications    chan struct{}
	retryAfter       map[int64]time.Time
}

func NewSeenWorker(
	repo SeenTaskRepository,
	cipher CredentialCipher,
	marker SeenMarker,
	locker SeenAccountLocker,
	logger *slog.Logger,
	interval time.Duration,
) *SeenWorker {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = defaultSeenPollInterval
	}
	return &SeenWorker{
		repo:             repo,
		cipher:           cipher,
		marker:           marker,
		locker:           locker,
		logger:           logger,
		interval:         interval,
		batchSize:        defaultSeenTaskBatchSize,
		accountWorkers:   defaultSeenAccountWorkers,
		acquireTimeout:   defaultSeenAcquireTimeout,
		operationTimeout: defaultSeenOperationTimeout,
		notifications:    make(chan struct{}, 1),
		retryAfter:       make(map[int64]time.Time),
	}
}

// SetAcquireTimeout limits how long one account may wait for its account lock
// and a global IMAP slot. The operation receives a separate full budget after
// both resources have been acquired.
func (w *SeenWorker) SetAcquireTimeout(timeout time.Duration) {
	if timeout > 0 {
		w.acquireTimeout = timeout
	}
}

// SetOperationTimeout sets the total budget for one account after it has
// acquired both its account lock and a global IMAP slot.
func (w *SeenWorker) SetOperationTimeout(timeout time.Duration) {
	if timeout > 0 {
		w.operationTimeout = timeout
	}
}

// Notify wakes the worker after a request commits a new task. Notifications
// coalesce so an API request never waits for the worker.
func (w *SeenWorker) Notify() {
	select {
	case w.notifications <- struct{}{}:
	default:
	}
}

// Run drains work immediately at startup, then wakes on either a notification
// or the polling interval. A completely successful full batch is followed by
// another batch without waiting.
func (w *SeenWorker) Run(ctx context.Context) {
	for {
		more, err := w.processPending(ctx)
		if err != nil && ctx.Err() == nil {
			w.logger.Warn("process pending IMAP seen tasks", "error", err)
		}
		if ctx.Err() != nil {
			return
		}
		if more {
			continue
		}
		if !w.wait(ctx) {
			return
		}
	}
}

func (w *SeenWorker) wait(ctx context.Context) bool {
	delay := w.interval
	now := time.Now()
	for _, retryAt := range w.retryAfter {
		untilRetry := retryAt.Sub(now)
		if untilRetry <= 0 {
			delay = 0
			break
		}
		if untilRetry < delay {
			delay = untilRetry
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-w.notifications:
		return true
	case <-timer.C:
		return true
	}
}

func (w *SeenWorker) processPending(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	accountIDs, err := w.repo.ListSeenTaskAccountIDs(ctx)
	if err != nil {
		return false, fmt.Errorf("list accounts with pending IMAP seen tasks: %w", err)
	}
	if len(accountIDs) == 0 {
		clear(w.retryAfter)
		return false, nil
	}
	present := make(map[int64]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		present[accountID] = struct{}{}
	}
	for accountID := range w.retryAfter {
		if _, exists := present[accountID]; !exists {
			delete(w.retryAfter, accountID)
		}
	}

	type pendingAccount struct {
		work      *seenAccountBatch
		taskCount int
	}
	type accountResult struct {
		accountID int64
		taskCount int
		err       error
	}

	var processErrors []error
	pendingAccounts := make([]pendingAccount, 0, len(accountIDs))
	considered := make(map[int64]struct{}, len(accountIDs))
	now := time.Now()
	for _, accountID := range accountIDs {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, duplicate := considered[accountID]; duplicate {
			continue
		}
		considered[accountID] = struct{}{}
		if retryAt, deferred := w.retryAfter[accountID]; deferred && now.Before(retryAt) {
			continue
		}
		delete(w.retryAfter, accountID)
		tasks, err := w.repo.ListSeenTasks(ctx, accountID, w.batchSize)
		if err != nil {
			processErrors = append(processErrors, fmt.Errorf("account %d: list tasks: %w", accountID, err))
			w.deferAccount(accountID)
			continue
		}
		if len(tasks) == 0 {
			continue
		}
		account, err := groupSeenTasks(accountID, tasks)
		if err != nil {
			processErrors = append(processErrors, err)
			w.deferAccount(accountID)
			continue
		}
		pendingAccounts = append(pendingAccounts, pendingAccount{
			work:      account,
			taskCount: len(tasks),
		})
	}

	// Use a bounded worker pool so a large pending-account set cannot create one
	// goroutine per account. The locker remains responsible for the per-account
	// lock and global IMAP connection limit.
	results := make([]accountResult, len(pendingAccounts))
	workerCount := min(w.accountWorkers, len(pendingAccounts))
	if workerCount < 1 {
		workerCount = 1
	}
	jobs := make(chan int, workerCount)
	var wg sync.WaitGroup
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				pending := pendingAccounts[index]
				results[index] = accountResult{
					accountID: pending.work.id,
					taskCount: pending.taskCount,
					err:       w.processAccount(ctx, pending.work),
				}
			}
		}()
	}
	for index := range pendingAccounts {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return false, err
	}

	more := false
	for _, result := range results {
		if result.err != nil {
			processErrors = append(
				processErrors,
				fmt.Errorf("account %d: %w", result.accountID, result.err),
			)
			w.deferAccount(result.accountID)
			continue
		}
		if result.taskCount >= w.batchSize {
			more = true
		}
	}
	if len(processErrors) != 0 {
		return more, errors.Join(processErrors...)
	}
	return more, nil
}

func (w *SeenWorker) deferAccount(accountID int64) {
	w.retryAfter[accountID] = time.Now().Add(w.interval)
}

type seenUIDBatch struct {
	uidValidity uint32
	uids        []uint32
	seen        map[uint32]struct{}
}

type seenAccountBatch struct {
	id      int64
	batches []*seenUIDBatch
}

func groupSeenTasks(accountID int64, tasks []domain.SeenTask) (*seenAccountBatch, error) {
	account := &seenAccountBatch{id: accountID}
	batchIndexes := make(map[uint32]int)
	for _, task := range tasks {
		if task.AccountID != accountID {
			return nil, fmt.Errorf(
				"account %d: listed task belongs to account %d",
				accountID, task.AccountID,
			)
		}
		batchIndex, ok := batchIndexes[task.UIDValidity]
		if !ok {
			batchIndex = len(account.batches)
			batchIndexes[task.UIDValidity] = batchIndex
			account.batches = append(account.batches, &seenUIDBatch{
				uidValidity: task.UIDValidity,
				seen:        make(map[uint32]struct{}),
			})
		}
		batch := account.batches[batchIndex]
		if _, duplicate := batch.seen[task.UID]; duplicate {
			continue
		}
		batch.seen[task.UID] = struct{}{}
		batch.uids = append(batch.uids, task.UID)
	}
	return account, nil
}

func (w *SeenWorker) processAccount(ctx context.Context, work *seenAccountBatch) error {
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, w.acquireTimeout)
	defer cancelAcquire()
	return w.locker.WithAccountIMAPSlot(acquireCtx, work.id, func() error {
		operationCtx, cancel := context.WithTimeout(ctx, w.operationTimeout)
		defer cancel()

		account, err := w.repo.GetAccount(operationCtx, work.id)
		if err != nil {
			return fmt.Errorf("get account: %w", err)
		}
		if !account.Enabled {
			return errors.New("account is disabled")
		}
		password, err := w.cipher.Decrypt(account.PasswordCiphertext)
		if err != nil {
			return fmt.Errorf("decrypt IMAP credential: %w", err)
		}
		defer func() { password = "" }()

		for _, batch := range work.batches {
			err := w.marker.MarkSeen(operationCtx, account, password, batch.uidValidity, batch.uids)
			if err != nil {
				var mismatch *mail.UIDValidityMismatchError
				if !errors.As(err, &mismatch) {
					return fmt.Errorf("mark UIDs seen for UIDVALIDITY %d: %w", batch.uidValidity, err)
				}
				w.logger.Info(
					"discard obsolete IMAP seen tasks",
					"account_id", work.id,
					"expected_uid_validity", mismatch.Expected,
					"actual_uid_validity", mismatch.Actual,
					"count", len(batch.uids),
				)
			}
			if err := w.repo.DeleteSeenTasks(operationCtx, work.id, batch.uidValidity, batch.uids); err != nil {
				return fmt.Errorf("delete completed IMAP seen tasks for UIDVALIDITY %d: %w", batch.uidValidity, err)
			}
		}
		return nil
	})
}
