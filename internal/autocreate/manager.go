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

	defaultPollInterval                  = 30 * time.Second
	defaultOverdueGrace                  = 2 * time.Minute
	maxErrorRunes                        = 240
	maxAppleOperationLength              = 96
	appleServiceCodeFingerprintHexLength = 16
)

const minimumCycleDuration = MinimumInterval * CreationsPerCycle

var (
	errNilRepository     = errors.New("alias creation repository is required")
	errNilCreator        = errors.New("alias creator is required")
	errInvalidAccountID  = errors.New("account ID must be positive")
	errInvalidRandom     = errors.New("random source returned an invalid index")
	errEmptyAliasAddress = errors.New("alias creator returned an empty address")
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
}

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
			m.logger.Warn("读取自动创建计划失败")
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
			m.logScheduleError("重排自动创建计划失败", schedule.AccountID)
			return
		}
		if _, err := m.repo.RescheduleAliasCreation(ctx, schedule.AccountID, expected, planned, now); err != nil {
			m.logScheduleError("保存自动创建重排失败", schedule.AccountID)
		}
		return
	}

	if schedule.LastAttemptedAt != nil {
		earliest := schedule.LastAttemptedAt.UTC().Add(MinimumInterval)
		if now.Before(earliest) {
			planned, err := m.shiftCurrentPlan(schedule, expected, earliest)
			if err != nil {
				m.logScheduleError("修正自动创建间隔失败", schedule.AccountID)
				return
			}
			if _, err := m.repo.RescheduleAliasCreation(ctx, schedule.AccountID, expected, planned, now); err != nil {
				m.logScheduleError("保存自动创建间隔修正失败", schedule.AccountID)
			}
			return
		}
	}

	claimAt := now
	planned, err := m.planAfterClaim(schedule, expected, claimAt)
	if err != nil {
		m.logScheduleError("生成下一组自动创建计划失败", schedule.AccountID)
		return
	}
	claimed, err := m.repo.ClaimAliasCreation(ctx, schedule.AccountID, expected, planned, claimAt)
	if err != nil {
		if ctx.Err() == nil {
			m.logScheduleError("认领自动创建计划失败", schedule.AccountID)
		}
		return
	}
	if !claimed || ctx.Err() != nil {
		return
	}
	// Claim time is persisted before the remote side effect. Re-read the clock
	// after the CAS so database latency cannot leave the next deadline inside
	// five minutes of the post-claim start boundary.
	attemptedAt := claimAt
	actualAt := m.now()
	if actualAt.After(attemptedAt) {
		corrected, correctionErr := m.correctPlanAfterClaim(ctx, schedule.AccountID, planned, actualAt)
		if correctionErr != nil || !corrected {
			if ctx.Err() == nil {
				message := errors.New("adjust automatic alias creation interval after claim")
				if correctionErr != nil {
					message = errors.New("persist automatic alias creation interval after claim")
				}
				if recordErr := m.repo.RecordAliasCreationFailure(ctx, schedule.AccountID, actualAt, failureMessage(message)); recordErr != nil {
					m.logScheduleError("记录自动创建间隔修正失败状态失败", schedule.AccountID)
				}
				m.logScheduleError("自动创建间隔修正失败", schedule.AccountID)
			}
			return
		}
		attemptedAt = actualAt
	}

	alias, createErr := m.creator(ctx, schedule.AccountID)
	address := strings.TrimSpace(alias.Address)
	if createErr == nil && address == "" {
		createErr = errEmptyAliasAddress
	}
	if createErr != nil {
		if ctx.Err() != nil {
			return
		}
		if err := m.repo.RecordAliasCreationFailure(ctx, schedule.AccountID, attemptedAt, failureMessage(createErr)); err != nil {
			m.logScheduleError("记录自动创建失败状态失败", schedule.AccountID)
		}
		if errors.Is(createErr, ErrCapacityReached) {
			if err := m.repo.DisableAliasCreation(ctx, schedule.AccountID, m.now()); err != nil {
				m.logScheduleError("达到容量上限后关闭自动创建失败", schedule.AccountID)
			}
		}
		m.logCreationError(schedule.AccountID, createErr)
		return
	}
	if err := m.repo.RecordAliasCreationSuccess(ctx, schedule.AccountID, attemptedAt, address); err != nil {
		m.logScheduleError("记录自动创建成功状态失败", schedule.AccountID)
	}
}

func (m *Manager) correctPlanAfterClaim(ctx context.Context, accountID int64, planned []time.Time, actualAt time.Time) (bool, error) {
	if len(planned) == 0 {
		return true, nil
	}
	earliest := actualAt.Add(MinimumInterval)
	if !planned[0].Before(earliest) {
		return true, nil
	}
	shifted := append([]time.Time(nil), planned...)
	shift := earliest.Sub(shifted[0])
	for index := range shifted {
		shifted[index] = shifted[index].Add(shift)
	}
	changed, err := m.repo.RescheduleAliasCreation(ctx, accountID, planned[0], shifted, actualAt)
	return changed, err
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
	if err == nil {
		return "自动创建失败"
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if message == "" {
		message = "自动创建失败"
	}
	runes := []rune(message)
	if len(runes) > maxErrorRunes {
		runes = runes[:maxErrorRunes]
	}
	return string(runes)
}

func (m *Manager) logScheduleError(message string, accountID int64) {
	// Deliberately omit upstream errors and alias addresses: Apple errors can
	// echo request data, and an address is user data rather than diagnostics.
	m.logger.Warn(message, "account_id", accountID)
}

func (m *Manager) logCreationError(accountID int64, err error) {
	attributes := []any{"account_id", accountID}
	var upstream *apple.Error
	if errors.As(err, &upstream) && upstream != nil {
		attributes = append(attributes,
			"operation", safeAppleOperation(upstream.Op),
			"http_status", upstream.StatusCode,
		)
		if fingerprint, ok := appleServiceCodeFingerprint(upstream.ServiceCode); ok {
			attributes = append(attributes,
				"service_code_present", true,
				"service_code_fingerprint", fingerprint,
			)
		}
		attributes = append(attributes, "retryable", upstream.Retryable)
	}
	m.logger.Warn("自动创建隐私邮箱失败", attributes...)
}

func safeAppleOperation(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxAppleOperationLength || !isSafeAppleOperation(value) {
		return "unknown"
	}
	return value
}

func appleServiceCodeFingerprint(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])[:appleServiceCodeFingerprintHexLength], true
}

func isSafeAppleOperation(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '_' || character == '-' || character == '.' {
			continue
		}
		if character == ' ' || character == '/' || character == ':' {
			continue
		}
		return false
	}
	return true
}
