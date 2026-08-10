// Package autocreate schedules bounded, rate-limited Hide My Email creation
// attempts for individual iCloud accounts.
package autocreate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	randv2 "math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

const (
	// CreationsPerCycle is the number of attempts in one hourly schedule.
	CreationsPerCycle = 5

	// MinimumInterval is the smallest permitted interval between attempts.
	MinimumInterval = 5 * time.Minute

	// CycleDuration is the duration covered by one generated plan.
	CycleDuration = time.Hour

	// appleRateLimitCooldown clears Apple's observed 30-60 minute reserve
	// batch window before another generated address is submitted.
	appleRateLimitCooldown = 61 * time.Minute

	defaultPollInterval                  = 30 * time.Second
	defaultOverdueGrace                  = 2 * time.Minute
	terminalStatePersistTimeout          = 5 * time.Second
	appleServiceCodeFingerprintHexLength = 16
)

const minimumCycleDuration = MinimumInterval * CreationsPerCycle

var (
	errNilRepository               = errors.New("alias creation repository is required")
	errNilCreator                  = errors.New("alias creator is required")
	errInvalidAccountID            = errors.New("account ID must be positive")
	errInvalidRandom               = errors.New("random source returned an invalid index")
	errEmptyAliasAddress           = errors.New("alias creator returned an empty address")
	errAliasCreationPlanCorrection = errors.New("automatic alias creation plan correction failed")
	errAliasCreationPlanConflict   = errors.New("automatic alias creation plan changed before correction")
	// ErrCapacityReached tells the worker that the account's provider-side
	// alias limit has been reached permanently for this schedule. The current
	// slot is recorded as failed and the opt-in is disabled to prevent further
	// remote creation attempts.
	ErrCapacityReached = errors.New("automatic alias capacity reached")
)

// Repository is the persistence boundary for an automatic alias schedule.
//
// EnableAliasCreation and DisableAliasCreation are idempotent mutations.
// RescheduleAliasCreation and ClaimAliasCreation compare expectedNext with
// the persisted next run and return false when another worker changed it.
// planned is the complete replacement plan, ordered by time; an empty plan is
// not used by Manager.
type Repository interface {
	GetAliasCreationSchedule(context.Context, int64) (domain.AliasCreationSchedule, error)
	ListDueAliasCreationSchedules(context.Context, time.Time) ([]domain.AliasCreationSchedule, error)
	EnableAliasCreation(context.Context, int64, []time.Time, time.Time) error
	DisableAliasCreation(context.Context, int64, time.Time) error
	RescheduleAliasCreation(context.Context, int64, time.Time, []time.Time, time.Time) (bool, error)
	ClaimAliasCreation(context.Context, int64, time.Time, []time.Time, time.Time) (bool, error)
	RecordAliasCreationSuccess(context.Context, int64, time.Time, string) error
	RecordAliasCreationFailure(context.Context, int64, time.Time, string) error
}

// Creator performs one remote creation and returns the locally persisted
// alias. It should not return a raw API key in an error or log message.
type Creator func(context.Context, int64) (domain.Alias, error)

// RandomSource is deliberately small so tests can provide a deterministic
// source. It is used for scheduling only, not for credentials or security.
type RandomSource interface {
	IntN(int) int
}

// WaitFunc waits for a duration or until the context is canceled. Returning
// false stops the manager. The production implementation uses a timer.
type WaitFunc func(context.Context, time.Duration) bool

// Option configures a Manager.
type Option func(*Manager) error

// WithClock injects the source of UTC wall-clock time.
func WithClock(clock func() time.Time) Option {
	return func(manager *Manager) error {
		if clock == nil {
			return errors.New("clock is required")
		}
		manager.clock = clock
		return nil
	}
}

// WithRandom injects the scheduling random source.
func WithRandom(source RandomSource) Option {
	return func(manager *Manager) error {
		if source == nil {
			return errors.New("random source is required")
		}
		manager.random = source
		return nil
	}
}

// WithRandomFunc is a convenience adapter for tests and small integrations.
func WithRandomFunc(source func(int) int) Option {
	if source == nil {
		return func(*Manager) error { return errors.New("random function is required") }
	}
	return WithRandom(randomFunc(source))
}

type randomFunc func(int) int

func (f randomFunc) IntN(n int) int { return f(n) }

// WithWait injects the wait primitive used by Run.
func WithWait(wait WaitFunc) Option {
	return func(manager *Manager) error {
		if wait == nil {
			return errors.New("wait function is required")
		}
		manager.wait = wait
		return nil
	}
}

// WithPollInterval changes how often Run asks the repository for due work.
// A zero or negative interval is rejected because it would create a busy loop.
func WithPollInterval(interval time.Duration) Option {
	return func(manager *Manager) error {
		if interval <= 0 {
			return errors.New("poll interval must be positive")
		}
		manager.pollInterval = interval
		return nil
	}
}

// WithOverdueGrace controls how late a planned slot may be executed. A slot
// later than this grace is skipped and a fresh cycle is anchored at now.
func WithOverdueGrace(grace time.Duration) Option {
	return func(manager *Manager) error {
		if grace < 0 {
			return errors.New("overdue grace must not be negative")
		}
		manager.overdueGrace = grace
		return nil
	}
}

// Manager owns the in-process polling loop. Durability and cross-process
// coordination are provided by Repository's compare-and-swap methods.
type Manager struct {
	repo    Repository
	creator Creator
	logger  *slog.Logger

	clock        func() time.Time
	random       RandomSource
	wait         WaitFunc
	pollInterval time.Duration
	overdueGrace time.Duration
	randomMu     sync.Mutex
	runSeq       atomic.Uint64
}

type aliasCreationScheduleDiagnosticError struct {
	cause error
}

func (e aliasCreationScheduleDiagnosticError) Error() string {
	return "automatic alias schedule operation failed"
}

func (e aliasCreationScheduleDiagnosticError) DiagnosticCode() string {
	return "AUTO_CREATE_SCHEDULE_ERROR"
}

func (e aliasCreationScheduleDiagnosticError) Unwrap() error { return e.cause }

// New constructs an automatic alias creation manager.
func New(repo Repository, creator Creator, logger *slog.Logger, options ...Option) (*Manager, error) {
	if repo == nil {
		return nil, errNilRepository
	}
	if creator == nil {
		return nil, errNilCreator
	}
	if logger == nil {
		logger = slog.Default()
	}
	manager := &Manager{
		repo:         repo,
		creator:      creator,
		logger:       logger,
		clock:        func() time.Time { return time.Now().UTC() },
		random:       defaultRandom{},
		wait:         waitForInterval,
		pollInterval: defaultPollInterval,
		overdueGrace: defaultOverdueGrace,
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(manager); err != nil {
			return nil, fmt.Errorf("configure alias creation manager: %w", err)
		}
	}
	return manager, nil
}

// Run polls until ctx is canceled or the injected wait function returns false.
// A poll handles each due schedule at most once; CAS in the repository makes
// concurrent manager instances harmless.
func (m *Manager) Run(ctx context.Context) {
	if m == nil || ctx == nil {
		return
	}
	for ctx.Err() == nil {
		m.runDue(ctx)
		if ctx.Err() != nil || !m.wait(ctx, m.pollInterval) {
			return
		}
	}
}

// GetSchedule returns a persisted schedule. An absent row means the feature
// has never been enabled and is represented as a disabled default.
func (m *Manager) GetSchedule(ctx context.Context, accountID int64) (domain.AliasCreationSchedule, error) {
	if err := validateAccountID(accountID); err != nil {
		return domain.AliasCreationSchedule{}, err
	}
	if ctx == nil {
		return domain.AliasCreationSchedule{}, errors.New("context is required")
	}
	schedule, err := m.repo.GetAliasCreationSchedule(ctx, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AliasCreationSchedule{AccountID: accountID}, nil
	}
	if err != nil {
		return domain.AliasCreationSchedule{}, fmt.Errorf("read alias creation schedule: %w", err)
	}
	return schedule, nil
}

// SetEnabled enables or disables automatic creation for one account. Enabling
// seeds a fresh absolute plan beginning at now; disabling leaves no future
// work. Repeating the same operation is idempotent and does not reshuffle an
// already enabled plan.
func (m *Manager) SetEnabled(ctx context.Context, accountID int64, enabled bool) (domain.AliasCreationSchedule, error) {
	if err := validateAccountID(accountID); err != nil {
		return domain.AliasCreationSchedule{}, err
	}
	if ctx == nil {
		return domain.AliasCreationSchedule{}, errors.New("context is required")
	}
	current, err := m.repo.GetAliasCreationSchedule(ctx, accountID)
	if err == nil && current.Enabled == enabled {
		return current, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.AliasCreationSchedule{}, fmt.Errorf("read alias creation schedule: %w", err)
	}
	now := m.now()
	if enabled {
		planned, planErr := m.newPlan(now)
		if planErr != nil {
			return domain.AliasCreationSchedule{}, planErr
		}
		if err := m.repo.EnableAliasCreation(ctx, accountID, planned, now); err != nil {
			return domain.AliasCreationSchedule{}, fmt.Errorf("enable alias creation: %w", err)
		}
	} else if err := m.repo.DisableAliasCreation(ctx, accountID, now); err != nil {
		return domain.AliasCreationSchedule{}, fmt.Errorf("disable alias creation: %w", err)
	}
	return m.GetSchedule(ctx, accountID)
}

func (m *Manager) runDue(ctx context.Context) {
	now := m.now()
	schedules, err := m.repo.ListDueAliasCreationSchedules(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			m.logAutomaticScheduleError(ctx, 0, "list_due_schedules", domain.AliasCreationPhasePreparing, now, err)
		}
		return
	}
	for _, schedule := range schedules {
		if ctx.Err() != nil {
			return
		}
		m.processDue(ctx, schedule)
	}
}

func (m *Manager) processDue(ctx context.Context, schedule domain.AliasCreationSchedule) {
	if !schedule.Enabled || schedule.AccountID < 1 || schedule.NextRunAt == nil {
		return
	}
	expected := schedule.NextRunAt.UTC()
	now := m.now()
	if now.Before(expected) {
		return
	}
	if now.Sub(expected) > m.overdueGrace {
		planned, err := m.newPlan(now)
		if err != nil {
			m.logAutomaticScheduleError(ctx, schedule.AccountID, "build_overdue_schedule", domain.AliasCreationPhasePreparing, expected, err)
			return
		}
		if _, err := m.repo.RescheduleAliasCreation(ctx, schedule.AccountID, expected, planned, now); err != nil {
			if ctx.Err() != nil {
				m.logAutomaticScheduleCancellation(ctx, schedule.AccountID, expected, now)
			} else {
				m.logAutomaticScheduleError(ctx, schedule.AccountID, "reschedule_overdue_schedule", domain.AliasCreationPhasePreparing, expected, err)
			}
		}
		return
	}

	if schedule.LastAttemptedAt != nil {
		earliest := schedule.LastAttemptedAt.UTC().Add(MinimumInterval)
		if now.Before(earliest) {
			planned, err := m.shiftCurrentPlan(schedule, expected, earliest)
			if err != nil {
				m.logAutomaticScheduleError(ctx, schedule.AccountID, "build_minimum_interval_schedule", domain.AliasCreationPhasePreparing, expected, err)
				return
			}
			if _, err := m.repo.RescheduleAliasCreation(ctx, schedule.AccountID, expected, planned, now); err != nil {
				if ctx.Err() != nil {
					m.logAutomaticScheduleCancellation(ctx, schedule.AccountID, expected, now)
				} else {
					m.logAutomaticScheduleError(ctx, schedule.AccountID, "reschedule_minimum_interval", domain.AliasCreationPhasePreparing, expected, err)
				}
			}
			return
		}
	}

	claimAt := now
	planned, err := m.planAfterClaim(schedule, expected, claimAt)
	if err != nil {
		m.logAutomaticScheduleError(ctx, schedule.AccountID, "build_next_schedule", domain.AliasCreationPhasePreparing, expected, err)
		return
	}
	claimed, err := m.repo.ClaimAliasCreation(ctx, schedule.AccountID, expected, planned, claimAt)
	if err != nil {
		if ctx.Err() == nil {
			m.logAutomaticScheduleError(ctx, schedule.AccountID, "claim_schedule_slot", domain.AliasCreationPhasePreparing, expected, err)
		} else {
			m.logAutomaticScheduleCancellation(ctx, schedule.AccountID, expected, claimAt)
		}
		return
	}
	if !claimed {
		return
	}
	// Claim time is persisted before the remote side effect. Re-read the clock
	// after the CAS so database latency cannot leave the next deadline inside
	// five minutes of the post-claim start boundary.
	attemptedAt := claimAt
	actualAt := m.now()
	flow := m.newAliasCreationFlow(actualAt)
	var nextRunAt *time.Time
	if len(planned) > 0 {
		next := planned[0].UTC()
		nextRunAt = &next
	}
	flow.markStarted()
	startAttributes := aliasCreationTimingAttrs(expected, attemptedAt, nextRunAt)
	startAttributes = append(startAttributes, slog.String("schedule_action", aliasCreationScheduleAction(false, nextRunAt)))
	m.logAliasCreationFlow(
		context.WithoutCancel(ctx),
		slog.LevelDebug,
		"自动创建隐私邮箱开始",
		schedule.AccountID,
		flow,
		"run_started",
		startAttributes...,
	)
	if ctx.Err() != nil {
		m.logAliasCreationCancellationWithError(
			context.WithoutCancel(ctx),
			schedule.AccountID,
			flow,
			flow.currentStage(),
			ctx.Err(),
			expected,
			attemptedAt,
			nextRunAt,
		)
		return
	}
	if actualAt.After(attemptedAt) {
		corrected, correctedPlan, correctionErr := m.correctPlanAfterClaim(ctx, schedule.AccountID, planned, actualAt)
		if correctionErr != nil || !corrected {
			if ctx.Err() != nil {
				m.logAliasCreationCancellationWithError(
					context.WithoutCancel(ctx),
					schedule.AccountID,
					flow,
					flow.currentStage(),
					ctx.Err(),
					expected,
					attemptedAt,
					nextRunAt,
				)
				return
			}
			correctionCause := error(errAliasCreationPlanConflict)
			if correctionErr != nil {
				correctionCause = fmt.Errorf("%w: %w", errAliasCreationPlanCorrection, correctionErr)
			}
			failureRecorded := true
			if recordErr := m.repo.RecordAliasCreationFailure(ctx, schedule.AccountID, actualAt, aliasCreationErrorReason("AUTO_CREATE_PLAN_CORRECTION_FAILED")); recordErr != nil {
				failureRecorded = false
				m.logAliasCreationStateError(
					ctx,
					"记录自动创建间隔修正失败状态失败",
					schedule.AccountID,
					flow,
					"record_plan_correction_failure",
					recordErr,
				)
			}
			m.logAliasCreationFailureWithOperation(
				context.WithoutCancel(ctx),
				schedule.AccountID,
				flow,
				domain.AliasCreationPhasePreparing,
				"correct_schedule_after_claim",
				correctionCause,
				expected,
				attemptedAt,
				nextRunAt,
				failureRecorded,
				false,
			)
			return
		}
		planned = correctedPlan
		attemptedAt = actualAt
	}

	nextRunAt = nil
	if len(planned) > 0 {
		next := planned[0].UTC()
		nextRunAt = &next
	}
	flow.startedAt = attemptedAt
	creatorContext := domain.WithAliasCreationProgressReporter(ctx, func(update domain.AliasCreationProgressUpdate) {
		m.logAliasCreationProgress(ctx, schedule.AccountID, &flow, update)
	})
	alias, createErr := m.creator(creatorContext, schedule.AccountID)
	address := strings.TrimSpace(alias.Address)
	if createErr == nil && address == "" {
		createErr = errEmptyAliasAddress
	}
	if createErr != nil {
		failedStage := flow.currentStage()
		if ctx.Err() != nil {
			info := diagnoseAliasCreationError(createErr)
			if info.code == "CONTEXT_CANCELED" || info.code == "CONTEXT_DEADLINE_EXCEEDED" {
				m.logAliasCreationCancellationWithError(
					context.WithoutCancel(ctx),
					schedule.AccountID,
					flow,
					failedStage,
					createErr,
					expected,
					attemptedAt,
					nextRunAt,
				)
				return
			}
		}
		logContext := context.WithoutCancel(ctx)
		failureStateContext, cancelFailureStateContext := context.WithTimeout(logContext, terminalStatePersistTimeout)
		failureRecorded := true
		if recordErr := m.repo.RecordAliasCreationFailure(failureStateContext, schedule.AccountID, attemptedAt, failureMessage(createErr)); recordErr != nil {
			failureRecorded = false
			m.logAliasCreationStateError(
				logContext,
				"记录自动创建失败状态失败",
				schedule.AccountID,
				flow,
				"record_creation_failure",
				recordErr,
			)
		}
		cancelFailureStateContext()
		if aliasCreationRequiresRateLimitCooldown(createErr) && nextRunAt != nil {
			// Start the cooldown at the observed failure time. The claim timestamp
			// can precede Apple's response by several seconds, which would otherwise
			// shorten the provider's rolling window.
			cooldownPlan, cooldownPlanErr := m.newPlanStartingAt(m.now().Add(appleRateLimitCooldown))
			if cooldownPlanErr == nil {
				expectedNext := nextRunAt.UTC()
				cooldownContext, cancelCooldownContext := context.WithTimeout(logContext, terminalStatePersistTimeout)
				var changed bool
				changed, cooldownPlanErr = m.repo.RescheduleAliasCreation(
					cooldownContext,
					schedule.AccountID,
					expectedNext,
					cooldownPlan,
					m.now(),
				)
				cancelCooldownContext()
				if cooldownPlanErr == nil && !changed {
					cooldownPlanErr = errAliasCreationPlanConflict
				}
			}
			if cooldownPlanErr != nil {
				m.logAliasCreationStateError(
					logContext,
					"Apple 限流后推迟自动创建计划失败",
					schedule.AccountID,
					flow,
					"reschedule_rate_limit_cooldown",
					cooldownPlanErr,
				)
			} else {
				next := cooldownPlan[0].UTC()
				nextRunAt = &next
			}
		}
		creationDisabled := false
		disableOperation := ""
		disableMessage := ""
		switch {
		case errors.Is(createErr, ErrCapacityReached):
			disableOperation = "disable_creation_schedule"
			disableMessage = "达到容量上限后关闭自动创建失败"
		case aliasCreationUntrackedRemoteSideEffect(createErr):
			disableOperation = "pause_after_untracked_remote_side_effect"
			disableMessage = "远端地址可能已创建但本地候选未保存，暂停自动创建失败"
		}
		if disableOperation != "" {
			disableContext, cancelDisableContext := context.WithTimeout(logContext, terminalStatePersistTimeout)
			disableErr := m.repo.DisableAliasCreation(disableContext, schedule.AccountID, m.now())
			cancelDisableContext()
			if disableErr != nil {
				m.logAliasCreationStateError(
					logContext,
					disableMessage,
					schedule.AccountID,
					flow,
					disableOperation,
					disableErr,
				)
			} else {
				creationDisabled = true
				nextRunAt = nil
			}
		}
		m.logAliasCreationFailure(
			logContext,
			schedule.AccountID,
			flow,
			failedStage,
			createErr,
			expected,
			attemptedAt,
			nextRunAt,
			failureRecorded,
			creationDisabled,
		)
		return
	}
	logContext := context.WithoutCancel(ctx)
	stateContext, cancelStateContext := context.WithTimeout(logContext, terminalStatePersistTimeout)
	defer cancelStateContext()
	resultRecorded := true
	if recordErr := m.repo.RecordAliasCreationSuccess(stateContext, schedule.AccountID, attemptedAt, address); recordErr != nil {
		resultRecorded = false
		m.logAliasCreationStateError(
			logContext,
			"记录自动创建成功状态失败",
			schedule.AccountID,
			flow,
			"record_creation_success",
			recordErr,
		)
	}
	m.logAliasCreationCompleted(
		logContext,
		schedule.AccountID,
		flow,
		alias.ID,
		expected,
		attemptedAt,
		nextRunAt,
		resultRecorded,
	)
}

func (m *Manager) correctPlanAfterClaim(
	ctx context.Context,
	accountID int64,
	planned []time.Time,
	actualAt time.Time,
) (bool, []time.Time, error) {
	if len(planned) == 0 {
		return true, nil, nil
	}
	earliest := actualAt.Add(MinimumInterval)
	if !planned[0].Before(earliest) {
		return true, append([]time.Time(nil), planned...), nil
	}
	shifted := append([]time.Time(nil), planned...)
	shift := earliest.Sub(shifted[0])
	for index := range shifted {
		shifted[index] = shifted[index].Add(shift)
	}
	changed, err := m.repo.RescheduleAliasCreation(ctx, accountID, planned[0], shifted, actualAt)
	if err != nil || !changed {
		return changed, nil, err
	}
	return true, shifted, nil
}

func (m *Manager) planAfterClaim(schedule domain.AliasCreationSchedule, expected, attemptedAt time.Time) ([]time.Time, error) {
	remaining := futurePlan(schedule.PlannedAt, expected)
	if len(remaining) > CreationsPerCycle-1 || !validPlan(remaining) {
		remaining = nil
	}
	if len(remaining) == 0 {
		anchor := expected
		if attemptedAt.After(anchor) {
			anchor = attemptedAt
		}
		return m.newPlan(anchor)
	}
	earliest := attemptedAt.Add(MinimumInterval)
	if remaining[0].Before(earliest) {
		shift := earliest.Sub(remaining[0])
		for index := range remaining {
			remaining[index] = remaining[index].Add(shift)
		}
	}
	return remaining, nil
}

func (m *Manager) shiftCurrentPlan(schedule domain.AliasCreationSchedule, expected, earliest time.Time) ([]time.Time, error) {
	planned := append([]time.Time(nil), schedule.PlannedAt...)
	if len(planned) == 0 {
		return m.newPlan(earliest.Add(-MinimumInterval))
	}
	planned = futurePlanIncludingCurrent(planned, expected)
	if len(planned) == 0 || len(planned) > CreationsPerCycle || !validPlan(planned) {
		return m.newPlan(earliest.Add(-MinimumInterval))
	}
	if planned[0].Before(earliest) {
		shift := earliest.Sub(planned[0])
		for index := range planned {
			planned[index] = planned[index].Add(shift)
		}
	}
	return planned, nil
}

func futurePlan(planned []time.Time, expected time.Time) []time.Time {
	result := make([]time.Time, 0, len(planned))
	for _, value := range planned {
		value = value.UTC()
		if value.After(expected) {
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Before(result[j]) })
	return result
}

func futurePlanIncludingCurrent(planned []time.Time, expected time.Time) []time.Time {
	result := make([]time.Time, 0, len(planned))
	for _, value := range planned {
		value = value.UTC()
		if !value.Before(expected) {
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Before(result[j]) })
	return result
}

func validPlan(planned []time.Time) bool {
	if len(planned) == 0 {
		return true
	}
	for index := 1; index < len(planned); index++ {
		if planned[index].Sub(planned[index-1]) < MinimumInterval {
			return false
		}
	}
	return true
}

func (m *Manager) newPlan(anchor time.Time) ([]time.Time, error) {
	m.randomMu.Lock()
	defer m.randomMu.Unlock()
	return generatePlan(anchor, m.random)
}

func (m *Manager) newPlanStartingAt(first time.Time) ([]time.Time, error) {
	planned, err := m.newPlan(first)
	if err != nil {
		return nil, err
	}
	// Keep five attempts in the following hour, but make the first deadline the
	// exact end of the cooldown instead of adding another random scheduling gap.
	planned[0] = first.UTC()
	return planned, nil
}

// generatePlan creates five cumulative deadlines. The five random gaps are
// integer minutes, each at least five minutes, and add up to one hour.
func generatePlan(anchor time.Time, random RandomSource) ([]time.Time, error) {
	if random == nil {
		return nil, errors.New("random source is required")
	}
	if CycleDuration < minimumCycleDuration {
		return nil, errors.New("cycle duration is shorter than minimum intervals")
	}
	gaps := [CreationsPerCycle]time.Duration{}
	for index := range gaps {
		gaps[index] = MinimumInterval
	}
	remaining := int((CycleDuration - minimumCycleDuration) / time.Minute)
	for index := 0; index < remaining; index++ {
		bucket := random.IntN(CreationsPerCycle)
		if bucket < 0 || bucket >= CreationsPerCycle {
			return nil, fmt.Errorf("%w: %d", errInvalidRandom, bucket)
		}
		gaps[bucket] += time.Minute
	}
	planned := make([]time.Time, CreationsPerCycle)
	next := anchor.UTC()
	for index, gap := range gaps {
		next = next.Add(gap)
		planned[index] = next
	}
	return planned, nil
}

type defaultRandom struct{}

func (defaultRandom) IntN(n int) int { return randv2.IntN(n) }

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

func (m *Manager) now() time.Time {
	return m.clock().UTC()
}

func validateAccountID(accountID int64) error {
	if accountID < 1 {
		return errInvalidAccountID
	}
	return nil
}

func failureMessage(err error) string {
	return diagnoseAliasCreationError(err).reason
}

func aliasCreationRequiresRateLimitCooldown(err error) bool {
	// Keep the stable diagnostic-code path for callers that only expose a
	// classified error, but also inspect wrapped Apple causes. A reserve can
	// return a rate-limit response while a session checkpoint fails, in which
	// case hmesync joins the persistence error before APPLE_RATE_LIMITED.
	return diagnoseAliasCreationError(err).code == "APPLE_RATE_LIMITED" || apple.IsRateLimited(err)
}

func (m *Manager) logAutomaticScheduleError(
	ctx context.Context,
	accountID int64,
	operation string,
	stage domain.AliasCreationPhase,
	scheduledFor time.Time,
	err error,
) {
	attemptedAt := m.now()
	flow := m.newAliasCreationFlow(attemptedAt)
	flow.markStarted()
	var nextRunAt *time.Time
	if accountID > 0 && !scheduledFor.IsZero() {
		next := scheduledFor.UTC()
		nextRunAt = &next
	}
	startAttributes := aliasCreationTimingAttrs(scheduledFor, attemptedAt, nextRunAt)
	startAttributes = append(startAttributes, slog.String("schedule_action", aliasCreationScheduleAction(false, nextRunAt)))
	m.logAliasCreationFlow(
		context.WithoutCancel(ctx),
		slog.LevelDebug,
		"自动创建隐私邮箱计划处理开始",
		accountID,
		flow,
		"run_started",
		startAttributes...,
	)
	wrapped := aliasCreationScheduleDiagnosticError{cause: err}
	m.logAliasCreationFailureWithOperation(
		context.WithoutCancel(ctx),
		accountID,
		flow,
		stage,
		operation,
		wrapped,
		scheduledFor,
		attemptedAt,
		nextRunAt,
		false,
		false,
	)
}

func (m *Manager) logAutomaticScheduleCancellation(
	ctx context.Context,
	accountID int64,
	scheduledFor, attemptedAt time.Time,
) {
	flow := m.newAliasCreationFlow(m.now())
	nextRunAt := scheduledFor.UTC()
	m.logAliasCreationCancellationWithError(
		context.WithoutCancel(ctx),
		accountID,
		flow,
		domain.AliasCreationPhasePreparing,
		ctx.Err(),
		scheduledFor,
		attemptedAt,
		&nextRunAt,
	)
}

func safeAppleOperation(value string) string {
	value = strings.TrimSpace(value)
	switch value {
	case "sign in",
		"initialize sign in",
		"initialize SRP",
		"derive SRP proof",
		"complete SRP sign in",
		"decode SRP challenge",
		"decode SRP salt",
		"decode SRP public value",
		"federate Apple ID",
		"request two-factor code",
		"verify two-factor code",
		"complete two-factor authentication",
		"trust Apple session",
		"exchange Apple session token",
		"decode Apple account",
		"validate Apple session",
		"decode Apple session",
		"list Hide My Email aliases",
		"decode Hide My Email list",
		"update Hide My Email forwarding target",
		"decode update Hide My Email forwarding target",
		"generate Hide My Email alias",
		"decode generate Hide My Email alias",
		"decode generated Hide My Email alias",
		"reserve Hide My Email alias",
		"decode reserve Hide My Email alias",
		"decode reserved Hide My Email alias",
		"deactivate Hide My Email alias",
		"decode deactivate Hide My Email alias",
		"delete Hide My Email alias",
		"decode delete Hide My Email alias",
		"create Hide My Email alias",
		"reserve alias":
		return value
	default:
		return "unknown"
	}
}

func appleServiceCodeFingerprint(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])[:appleServiceCodeFingerprintHexLength], true
}
