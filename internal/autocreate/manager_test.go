package autocreate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

var errCreate = errors.New("upstream alias creation failed for secret@example.com")

type untrackedRemoteSideEffectTestError struct {
	cause error
}

func (e *untrackedRemoteSideEffectTestError) Error() string { return e.cause.Error() }
func (e *untrackedRemoteSideEffectTestError) Unwrap() error { return e.cause }
func (e *untrackedRemoteSideEffectTestError) RemoteSideEffectPossible() bool {
	return true
}

type testRandom struct {
	state uint64
}

func (r *testRandom) IntN(n int) int {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return int(r.state % uint64(n))
}

type fixedRandom int

func (r fixedRandom) IntN(n int) int { return int(r) % n }

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(now time.Time) *testClock { return &testClock{now: now.UTC()} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now.UTC()
	c.mu.Unlock()
}

func (c *testClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}

type claimRecord struct {
	accountID   int64
	expected    time.Time
	planned     []time.Time
	attemptedAt time.Time
}

type rescheduleRecord struct {
	accountID int64
	expected  time.Time
	planned   []time.Time
	now       time.Time
}

type successRecord struct {
	accountID   int64
	attemptedAt time.Time
	address     string
}

type failureRecord struct {
	accountID   int64
	attemptedAt time.Time
	message     string
}

type fakeRepository struct {
	mu sync.Mutex

	schedules   map[int64]domain.AliasCreationSchedule
	claims      []claimRecord
	reschedules []rescheduleRecord
	successes   []successRecord
	failures    []failureRecord

	claimResult      bool
	claimErr         error
	claimHook        func()
	disableErr       error
	rescheduleResult bool
	rescheduleErr    error
	rescheduleHook   func()
	listErr          error
	creatorObserved  bool
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{schedules: make(map[int64]domain.AliasCreationSchedule), claimResult: true, rescheduleResult: true}
}

func (r *fakeRepository) GetAliasCreationSchedule(_ context.Context, accountID int64) (domain.AliasCreationSchedule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	schedule, ok := r.schedules[accountID]
	if !ok {
		return domain.AliasCreationSchedule{}, sql.ErrNoRows
	}
	return cloneSchedule(schedule), nil
}

func (r *fakeRepository) ListDueAliasCreationSchedules(_ context.Context, now time.Time) ([]domain.AliasCreationSchedule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	result := make([]domain.AliasCreationSchedule, 0)
	for _, schedule := range r.schedules {
		if schedule.Enabled && schedule.NextRunAt != nil && !schedule.NextRunAt.After(now) {
			result = append(result, cloneSchedule(schedule))
		}
	}
	return result, nil
}

func (r *fakeRepository) EnableAliasCreation(_ context.Context, accountID int64, planned []time.Time, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	previous := r.schedules[accountID]
	schedule := domain.AliasCreationSchedule{
		AccountID: accountID,
		Enabled:   true,
		PlannedAt: append([]time.Time(nil), planned...),
		CreatedAt: previous.CreatedAt,
		UpdatedAt: now,
	}
	if schedule.CreatedAt.IsZero() {
		schedule.CreatedAt = now
	}
	if len(planned) > 0 {
		next := planned[0]
		schedule.NextRunAt = &next
	}
	r.schedules[accountID] = schedule
	return nil
}

func (r *fakeRepository) DisableAliasCreation(_ context.Context, accountID int64, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disableErr != nil {
		return r.disableErr
	}
	schedule, ok := r.schedules[accountID]
	if !ok {
		return nil
	}
	schedule.Enabled = false
	schedule.PlannedAt = nil
	schedule.NextRunAt = nil
	schedule.UpdatedAt = now
	r.schedules[accountID] = schedule
	return nil
}

func (r *fakeRepository) RescheduleAliasCreation(_ context.Context, accountID int64, expectedNext time.Time, planned []time.Time, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rescheduleHook != nil {
		r.rescheduleHook()
	}
	if r.rescheduleErr != nil {
		return false, r.rescheduleErr
	}
	schedule, ok := r.schedules[accountID]
	if !ok || !schedule.Enabled || schedule.NextRunAt == nil || !schedule.NextRunAt.Equal(expectedNext) {
		return false, nil
	}
	if !r.rescheduleResult {
		return false, nil
	}
	r.reschedules = append(r.reschedules, rescheduleRecord{
		accountID: accountID,
		expected:  expectedNext,
		planned:   append([]time.Time(nil), planned...),
		now:       now,
	})
	applyPlan(&schedule, planned, now)
	r.schedules[accountID] = schedule
	return true, nil
}

func (r *fakeRepository) ClaimAliasCreation(_ context.Context, accountID int64, expectedNext time.Time, planned []time.Time, attemptedAt time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claimErr != nil {
		return false, r.claimErr
	}
	schedule, ok := r.schedules[accountID]
	if !ok || !schedule.Enabled || schedule.NextRunAt == nil || !schedule.NextRunAt.Equal(expectedNext) {
		return false, nil
	}
	if !r.claimResult {
		return false, nil
	}
	r.claims = append(r.claims, claimRecord{
		accountID:   accountID,
		expected:    expectedNext,
		planned:     append([]time.Time(nil), planned...),
		attemptedAt: attemptedAt,
	})
	if r.claimHook != nil {
		r.claimHook()
	}
	schedule.LastAttemptedAt = timePtr(attemptedAt)
	applyPlan(&schedule, planned, attemptedAt)
	r.schedules[accountID] = schedule
	return true, nil
}

func (r *fakeRepository) RecordAliasCreationSuccess(ctx context.Context, accountID int64, attemptedAt time.Time, address string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.successes = append(r.successes, successRecord{accountID: accountID, attemptedAt: attemptedAt, address: address})
	schedule := r.schedules[accountID]
	schedule.LastCreatedAt = timePtr(attemptedAt)
	schedule.LastAliasAddress = address
	schedule.LastError = ""
	r.schedules[accountID] = schedule
	return nil
}

func (r *fakeRepository) RecordAliasCreationFailure(_ context.Context, accountID int64, attemptedAt time.Time, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, failureRecord{accountID: accountID, attemptedAt: attemptedAt, message: message})
	schedule := r.schedules[accountID]
	schedule.LastAttemptedAt = timePtr(attemptedAt)
	schedule.LastError = message
	r.schedules[accountID] = schedule
	return nil
}

func applyPlan(schedule *domain.AliasCreationSchedule, planned []time.Time, updatedAt time.Time) {
	schedule.PlannedAt = append([]time.Time(nil), planned...)
	schedule.UpdatedAt = updatedAt
	if len(planned) == 0 {
		schedule.NextRunAt = nil
		return
	}
	schedule.NextRunAt = timePtr(planned[0])
}

func cloneSchedule(schedule domain.AliasCreationSchedule) domain.AliasCreationSchedule {
	schedule.PlannedAt = append([]time.Time(nil), schedule.PlannedAt...)
	schedule.NextRunAt = cloneTime(schedule.NextRunAt)
	schedule.LastAttemptedAt = cloneTime(schedule.LastAttemptedAt)
	schedule.LastCreatedAt = cloneTime(schedule.LastCreatedAt)
	return schedule
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func timePtr(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}

func newManagerForTest(t *testing.T, repo *fakeRepository, clock *testClock, creator Creator, options ...Option) *Manager {
	t.Helper()
	if creator == nil {
		creator = func(context.Context, int64) (domain.Alias, error) {
			return domain.Alias{Address: "created@example.com"}, nil
		}
	}
	base := []Option{WithClock(clock.Now), WithRandom(fixedRandom(0))}
	base = append(base, options...)
	manager, err := New(repo, creator, slog.New(slog.NewTextHandler(io.Discard, nil)), base...)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return manager
}

func enableForTest(t *testing.T, manager *Manager, accountID int64) domain.AliasCreationSchedule {
	t.Helper()
	schedule, err := manager.SetEnabled(context.Background(), accountID, true)
	if err != nil {
		t.Fatalf("enable schedule: %v", err)
	}
	return schedule
}

func TestGeneratePlanProperties(t *testing.T) {
	anchor := time.Date(2026, 8, 8, 0, 0, 0, 123000000, time.FixedZone("test", 8*60*60))
	for seed := uint64(1); seed <= 1000; seed++ {
		plan, err := generatePlan(anchor, &testRandom{state: seed})
		if err != nil {
			t.Fatalf("seed %d: generate plan: %v", seed, err)
		}
		if len(plan) != CreationsPerCycle {
			t.Fatalf("seed %d: plan length = %d, want %d", seed, len(plan), CreationsPerCycle)
		}
		if !plan[len(plan)-1].Equal(anchor.UTC().Add(CycleDuration)) {
			t.Fatalf("seed %d: final deadline = %v, want %v", seed, plan[len(plan)-1], anchor.UTC().Add(CycleDuration))
		}
		previous := anchor.UTC()
		for index, deadline := range plan {
			if deadline.Sub(previous) < MinimumInterval {
				t.Fatalf("seed %d: gap %d = %v, want >= %v", seed, index, deadline.Sub(previous), MinimumInterval)
			}
			previous = deadline
		}
	}
}

func TestGeneratePlanRejectsInvalidRandomIndex(t *testing.T) {
	_, err := generatePlan(time.Now(), fixedRandom(-1))
	if !errors.Is(err, errInvalidRandom) {
		t.Fatalf("error = %v, want invalid random error", err)
	}
}

func TestWithRandomFuncRejectsNil(t *testing.T) {
	repo := newFakeRepository()
	_, err := New(repo, func(context.Context, int64) (domain.Alias, error) { return domain.Alias{}, nil }, nil, WithRandomFunc(nil))
	if err == nil {
		t.Fatal("nil random function unexpectedly accepted")
	}
}

func TestPlanCrossCycleKeepsMinimumGap(t *testing.T) {
	anchor := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	first, err := generatePlan(anchor, fixedRandom(4))
	if err != nil {
		t.Fatalf("first plan: %v", err)
	}
	second, err := generatePlan(first[len(first)-1], fixedRandom(1))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if gap := second[0].Sub(first[len(first)-1]); gap < MinimumInterval {
		t.Fatalf("cross-cycle gap = %v, want >= %v", gap, MinimumInterval)
	}
	if second[len(second)-1].Sub(first[len(first)-1]) != CycleDuration {
		t.Fatalf("second cycle span = %v, want %v", second[len(second)-1].Sub(first[len(first)-1]), CycleDuration)
	}
}

func TestSetEnabledSeedsAndDisablesIdempotently(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	clock := newTestClock(now)
	repo := newFakeRepository()
	manager := newManagerForTest(t, repo, clock, nil)

	schedule, err := manager.GetSchedule(context.Background(), 7)
	if err != nil || schedule.Enabled || schedule.AccountID != 7 {
		t.Fatalf("missing schedule = %#v, err=%v", schedule, err)
	}
	enabled := enableForTest(t, manager, 7)
	if !enabled.Enabled || len(enabled.PlannedAt) != CreationsPerCycle || enabled.NextRunAt == nil {
		t.Fatalf("enabled schedule = %#v", enabled)
	}
	repeated, err := manager.SetEnabled(context.Background(), 7, true)
	if err != nil {
		t.Fatalf("repeat enable: %v", err)
	}
	if !equalTimes(repeated.PlannedAt, enabled.PlannedAt) {
		t.Fatalf("repeat enable reshuffled plan: got %#v want %#v", repeated.PlannedAt, enabled.PlannedAt)
	}
	disabled, err := manager.SetEnabled(context.Background(), 7, false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if disabled.Enabled || disabled.NextRunAt != nil || len(disabled.PlannedAt) != 0 {
		t.Fatalf("disabled schedule = %#v", disabled)
	}
	disabledAgain, err := manager.SetEnabled(context.Background(), 7, false)
	if err != nil || disabledAgain.Enabled {
		t.Fatalf("repeat disable = %#v, err=%v", disabledAgain, err)
	}
}

func TestRunClaimsBeforeCreatingAndRecordsSuccess(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	clock := newTestClock(now)
	repo := newFakeRepository()
	var manager *Manager
	creatorCalls := 0
	creator := func(_ context.Context, accountID int64) (domain.Alias, error) {
		creatorCalls++
		repo.mu.Lock()
		repo.creatorObserved = len(repo.claims) == 1
		repo.mu.Unlock()
		return domain.Alias{AccountID: accountID, Address: "new@example.com"}, nil
	}
	manager = newManagerForTest(t, repo, clock, creator)
	schedule := enableForTest(t, manager, 3)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if creatorCalls != 1 || len(repo.claims) != 1 || len(repo.successes) != 1 {
		t.Fatalf("calls: creator=%d claims=%d successes=%d", creatorCalls, len(repo.claims), len(repo.successes))
	}
	if !repo.creatorObserved {
		t.Fatal("creator ran before claim was persisted")
	}
	if got := len(repo.claims[0].planned); got != CreationsPerCycle-1 {
		t.Fatalf("remaining plan length = %d, want %d", got, CreationsPerCycle-1)
	}
	if repo.successes[0].address != "new@example.com" {
		t.Fatalf("recorded address = %q", repo.successes[0].address)
	}
}

func TestClaimLatencyCannotShortenActualAttemptGap(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	clock := newTestClock(now)
	repo := newFakeRepository()
	// The hook models a slow database CAS. The first generated plan uses a
	// 40-minute first gap followed by five-minute gaps, so the next slot would
	// otherwise be too close after a one-minute claim delay.
	repo.claimHook = func() { clock.Advance(time.Minute) }
	creatorCalls := 0
	manager := newManagerForTest(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		creatorCalls++
		return domain.Alias{Address: "delayed@example.com"}, nil
	})
	schedule := enableForTest(t, manager, 31)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())
	if creatorCalls != 1 {
		t.Fatalf("creator calls = %d, want 1", creatorCalls)
	}
	current, err := manager.GetSchedule(context.Background(), 31)
	if err != nil {
		t.Fatalf("read delayed schedule: %v", err)
	}
	if current.NextRunAt == nil || current.LastAttemptedAt == nil {
		t.Fatalf("delayed schedule = %#v", current)
	}
	if gap := current.NextRunAt.Sub(*current.LastAttemptedAt); gap < MinimumInterval {
		t.Fatalf("corrected gap = %v, want >= %v", gap, MinimumInterval)
	}
	if len(repo.reschedules) != 1 {
		t.Fatalf("claim-latency correction reschedules = %d, want 1", len(repo.reschedules))
	}
}

func TestRunCompletesCycleAndStartsNextHour(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	creatorCalls := 0
	manager := newManagerForTest(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		creatorCalls++
		return domain.Alias{Address: fmt.Sprintf("alias-%d@example.com", creatorCalls)}, nil
	})
	enableForTest(t, manager, 4)
	for index := 0; index < CreationsPerCycle; index++ {
		current, err := manager.GetSchedule(context.Background(), 4)
		if err != nil {
			t.Fatalf("read schedule %d: %v", index, err)
		}
		clock.Set(*current.NextRunAt)
		manager.runDue(context.Background())
	}
	if creatorCalls != CreationsPerCycle {
		t.Fatalf("creator calls = %d, want %d", creatorCalls, CreationsPerCycle)
	}
	current, err := manager.GetSchedule(context.Background(), 4)
	if err != nil {
		t.Fatalf("read next cycle: %v", err)
	}
	if current.NextRunAt == nil || current.LastAttemptedAt == nil {
		t.Fatalf("next cycle schedule = %#v", current)
	}
	if len(repo.claims) != CreationsPerCycle {
		t.Fatalf("claims = %d, want %d", len(repo.claims), CreationsPerCycle)
	}
	if gap := current.NextRunAt.Sub(*current.LastAttemptedAt); gap < MinimumInterval {
		t.Fatalf("cross-cycle gap = %v, want >= %v", gap, MinimumInterval)
	}
	if gap := current.NextRunAt.Sub(repo.claims[len(repo.claims)-1].attemptedAt); gap > CycleDuration {
		t.Fatalf("next cycle first deadline is too far away: %v", gap)
	}
}

func TestOverdueScheduleIsRescheduledWithoutBurst(t *testing.T) {
	plannedAt := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	clock := newTestClock(plannedAt)
	repo := newFakeRepository()
	creatorCalls := 0
	manager := newManagerForTest(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		creatorCalls++
		return domain.Alias{Address: "unexpected@example.com"}, nil
	}, WithOverdueGrace(time.Minute))
	schedule := enableForTest(t, manager, 5)
	clock.Set(schedule.NextRunAt.Add(2 * time.Minute))
	manager.runDue(context.Background())
	if creatorCalls != 0 {
		t.Fatalf("overdue creator calls = %d, want 0", creatorCalls)
	}
	repo.mu.Lock()
	if len(repo.reschedules) != 1 || len(repo.claims) != 0 {
		repo.mu.Unlock()
		t.Fatalf("reschedules=%d claims=%d, want 1/0", len(repo.reschedules), len(repo.claims))
	}
	rescheduled := repo.reschedules[0]
	repo.mu.Unlock()
	if len(rescheduled.planned) != CreationsPerCycle || !rescheduled.planned[0].After(clock.Now()) {
		t.Fatalf("rescheduled plan = %#v, now=%v", rescheduled.planned, clock.Now())
	}
	manager.runDue(context.Background())
	if creatorCalls != 0 {
		t.Fatal("overdue reschedule caused an immediate catch-up call")
	}
}

func TestFailureAdvancesPlanWithoutImmediateRetry(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	creatorCalls := 0
	manager := newManagerForTest(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		creatorCalls++
		return domain.Alias{}, errCreate
	})
	schedule := enableForTest(t, manager, 6)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())
	if creatorCalls != 1 {
		t.Fatalf("first creator calls = %d, want 1", creatorCalls)
	}
	manager.runDue(context.Background())
	if creatorCalls != 1 {
		t.Fatalf("immediate retry calls = %d, want 1", creatorCalls)
	}
	current, err := manager.GetSchedule(context.Background(), 6)
	if err != nil {
		t.Fatalf("read failed schedule: %v", err)
	}
	if len(repo.failures) != 1 || current.LastError == "" {
		t.Fatalf("failure state: failures=%d schedule=%#v", len(repo.failures), current)
	}
	if stringsContains(current.LastError, "secret@example.com") {
		t.Fatalf("failure message leaked sensitive detail in persistence: %q", current.LastError)
	}
	if current.LastError != failureMessage(errCreate) {
		t.Fatalf("failure message = %q, want stable diagnostic %q", current.LastError, failureMessage(errCreate))
	}
	if current.NextRunAt == nil || current.LastAttemptedAt == nil || current.NextRunAt.Sub(*current.LastAttemptedAt) < MinimumInterval {
		t.Fatalf("next attempt interval = %#v", current)
	}
}

func TestRateLimitedFailureSkipsRemainingCycleAndStartsAfterCooldown(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	creatorCalls := 0
	manager := newManagerForTest(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		creatorCalls++
		if creatorCalls == 1 {
			return domain.Alias{}, testDiagnosticError{code: "APPLE_RATE_LIMITED", detail: "rate limited"}
		}
		return domain.Alias{Address: "after-cooldown@example.com"}, nil
	})
	schedule := enableForTest(t, manager, 60)
	originalPlan := append([]time.Time(nil), schedule.PlannedAt...)
	attemptedAt := schedule.NextRunAt.UTC()
	clock.Set(attemptedAt)
	manager.runDue(context.Background())

	current, err := manager.GetSchedule(context.Background(), 60)
	if err != nil {
		t.Fatalf("read rate-limited schedule: %v", err)
	}
	wantNext := attemptedAt.Add(appleRateLimitCooldown)
	if current.NextRunAt == nil || !current.NextRunAt.Equal(wantNext) ||
		len(current.PlannedAt) != CreationsPerCycle ||
		!current.PlannedAt[len(current.PlannedAt)-1].Equal(wantNext.Add(CycleDuration)) {
		t.Fatalf("rate-limited replacement plan = %#v, want first=%v last=%v", current.PlannedAt, wantNext, wantNext.Add(CycleDuration))
	}
	if len(repo.reschedules) != 1 || !repo.reschedules[0].expected.Equal(originalPlan[1]) {
		t.Fatalf("rate-limit reschedules = %#v, want replacement of next old slot %v", repo.reschedules, originalPlan[1])
	}

	clock.Set(originalPlan[len(originalPlan)-1])
	manager.runDue(context.Background())
	if creatorCalls != 1 {
		t.Fatalf("old cycle executed after rate limit: creator calls=%d", creatorCalls)
	}
	clock.Set(wantNext)
	manager.runDue(context.Background())
	if creatorCalls != 2 || len(repo.successes) != 1 || repo.successes[0].address != "after-cooldown@example.com" {
		t.Fatalf("post-cooldown execution: calls=%d successes=%#v", creatorCalls, repo.successes)
	}
}

func TestRateLimitedAppleCauseBehindPersistenceErrorStillStartsCooldown(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	createErr := errors.Join(
		testDiagnosticError{code: "AUTO_CREATION_PERSISTENCE_ERROR", detail: "checkpoint failed"},
		&apple.Error{
			Op:          "reserve Hide My Email alias",
			Kind:        apple.ErrService,
			StatusCode:  200,
			ServiceCode: "-41015",
		},
	)
	manager := newManagerForTest(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		return domain.Alias{}, createErr
	})
	schedule := enableForTest(t, manager, 62)
	attemptedAt := schedule.NextRunAt.UTC()
	clock.Set(attemptedAt)
	manager.runDue(context.Background())

	current, err := manager.GetSchedule(context.Background(), 62)
	if err != nil {
		t.Fatalf("read joined-error schedule: %v", err)
	}
	wantNext := attemptedAt.Add(appleRateLimitCooldown)
	if current.NextRunAt == nil || !current.NextRunAt.Equal(wantNext) {
		t.Fatalf("joined rate-limit next run = %v, want %v", current.NextRunAt, wantNext)
	}
}

func TestUntrackedRemoteSideEffectPausesScheduleBeforeNextSlot(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	creatorCalls := 0
	manager, logs := newFlowLogManager(t, repo, clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
		creatorCalls++
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseSavingCandidate, 70, 0)
		return domain.Alias{}, &untrackedRemoteSideEffectTestError{cause: errCreate}
	})
	schedule := enableForTest(t, manager, 61)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	current, err := manager.GetSchedule(context.Background(), 61)
	if err != nil {
		t.Fatalf("read paused schedule: %v", err)
	}
	if current.Enabled || current.NextRunAt != nil || len(current.PlannedAt) != 0 {
		t.Fatalf("untracked remote side effect did not pause schedule: %#v", current)
	}
	clock.Advance(CycleDuration)
	manager.runDue(context.Background())
	if creatorCalls != 1 {
		t.Fatalf("creator calls after paused schedule = %d, want 1", creatorCalls)
	}
	failed := requireAutoCreateEvent(t, logs, "run_failed")
	if failed.Fields["auto_creation_disabled"] != "true" ||
		failed.Fields["schedule_action"] != "disabled" ||
		failed.Fields["remote_side_effect_possible"] != "true" ||
		failed.Fields["pending_confirmation"] != "false" {
		t.Fatalf("paused failure diagnostics = %#v", failed.Fields)
	}
}

func TestSuccessfulCreationRecordsTerminalStateAfterCancellation(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	ctx, cancel := context.WithCancel(context.Background())
	manager := newManagerForTest(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		cancel()
		return domain.Alias{ID: 91, Address: "created@example.com"}, nil
	})
	schedule := enableForTest(t, manager, 62)
	clock.Set(*schedule.NextRunAt)
	manager.processDue(ctx, schedule)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.successes) != 1 || repo.successes[0].address != "created@example.com" {
		t.Fatalf("success state after cancellation = %#v", repo.successes)
	}
}

func TestActualAttemptGapIsEnforcedWhenClockPollsEarly(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	clock := newTestClock(now)
	repo := newFakeRepository()
	manager := newManagerForTest(t, repo, clock, nil)
	enableForTest(t, manager, 7)
	lastAttempt := now.Add(-time.Minute)
	current := repo.schedules[7]
	current.LastAttemptedAt = timePtr(lastAttempt)
	current.NextRunAt = timePtr(now)
	current.PlannedAt[0] = now
	repo.schedules[7] = current
	manager.runDue(context.Background())
	if len(repo.claims) != 0 || len(repo.reschedules) != 1 {
		t.Fatalf("claims=%d reschedules=%d, want 0/1", len(repo.claims), len(repo.reschedules))
	}
	if repo.reschedules[0].planned[0].Before(lastAttempt.Add(MinimumInterval)) {
		t.Fatalf("rescheduled first deadline = %v, want >= %v", repo.reschedules[0].planned[0], lastAttempt.Add(MinimumInterval))
	}
}

func TestClaimCASFalseSkipsCreator(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	repo.claimResult = false
	creatorCalls := 0
	manager := newManagerForTest(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		creatorCalls++
		return domain.Alias{Address: "unexpected@example.com"}, nil
	})
	schedule := enableForTest(t, manager, 8)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())
	if creatorCalls != 0 {
		t.Fatalf("creator calls after lost claim = %d, want 0", creatorCalls)
	}
}

func TestRunStopsOnCancellation(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	ctx, cancel := context.WithCancel(context.Background())
	waitStarted := make(chan struct{})
	manager := newManagerForTest(t, repo, clock, nil, WithWait(func(ctx context.Context, _ time.Duration) bool {
		close(waitStarted)
		<-ctx.Done()
		return false
	}))
	done := make(chan struct{})
	go func() {
		manager.Run(ctx)
		close(done)
	}()
	select {
	case <-waitStarted:
	case <-time.After(time.Second):
		t.Fatal("manager did not enter wait")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("manager did not stop after cancellation")
	}
}

func TestRunCancellationPropagatesToCreator(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	ctx, cancel := context.WithCancel(context.Background())
	creatorStarted := make(chan struct{})
	manager := newManagerForTest(t, repo, clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
		close(creatorStarted)
		<-ctx.Done()
		return domain.Alias{}, ctx.Err()
	})
	schedule := enableForTest(t, manager, 21)
	clock.Set(*schedule.NextRunAt)
	done := make(chan struct{})
	go func() {
		manager.Run(ctx)
		close(done)
	}()
	select {
	case <-creatorStarted:
	case <-time.After(time.Second):
		t.Fatal("creator did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("manager did not stop after creator cancellation")
	}
}

func TestOrdinaryFailureLogsSanitizedDiagnostics(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	manager, logs := newFlowLogManager(t, repo, clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReserving, 60, 0)
		return domain.Alias{}, errCreate
	})
	schedule := enableForTest(t, manager, 9)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	failed := requireAutoCreateEvent(t, logs, "run_failed")
	const reason = "自动创建在本地处理阶段失败，请结合失败步骤和部署日志排查"
	if failed.Fields["failed_stage"] != string(domain.AliasCreationPhaseReserving) ||
		failed.Fields["error_code"] != "AUTO_CREATE_FAILED" ||
		failed.Fields["error"] != reason || failed.Fields["error_context"] != reason {
		t.Fatalf("ordinary failure diagnostics = %#v", failed)
	}
	assertFlowLogsDoNotContain(t, logs, "secret@example.com", errCreate.Error())
}

func TestAppleFailureLogsOnlySanitizedStructuredFields(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	upstream := &apple.Error{
		Op:          "reserve Hide My Email alias",
		Kind:        apple.ErrService,
		StatusCode:  503,
		ServiceCode: "HME.TEMPORARY-1_2",
		Retryable:   true,
		Err:         errors.New(`response body {"token":"TOKEN","email":"cause-secret@example.com"}`),
	}
	createErr := fmt.Errorf("outer-secret@example.com: %w", upstream)
	manager, logs := newFlowLogManager(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		return domain.Alias{Address: "alias-secret@example.com"}, createErr
	})
	schedule := enableForTest(t, manager, 41)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	entry := requireAutoCreateEvent(t, logs, "run_failed")
	if entry.Fields["account_id"] != "41" || entry.Fields["operation"] != upstream.Op ||
		entry.Fields["http_status"] != "503" || entry.Fields["retryable"] != "true" ||
		entry.Fields["upstream_retryable"] != "true" || entry.Fields["error_code"] != "APPLE_UPSTREAM_ERROR" {
		t.Fatalf("structured Apple fields = %#v", entry)
	}
	if _, exists := entry.Fields["service_code"]; exists || entry.Fields["service_code_present"] != "true" {
		t.Fatalf("service code fields = %#v", entry)
	}
	fingerprint := entry.Fields["service_code_fingerprint"]
	if len(fingerprint) != appleServiceCodeFingerprintHexLength {
		t.Fatalf("service code fingerprint = %q", fingerprint)
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		t.Fatalf("service code fingerprint is not hex: %q", fingerprint)
	}
	assertFlowLogsDoNotContain(t, logs, upstream.ServiceCode, "outer-secret@example.com", "alias-secret@example.com", "cause-secret@example.com", "TOKEN", "response body")
	if len(repo.failures) != 1 || repo.failures[0].message != failureMessage(createErr) {
		t.Fatalf("persisted failure changed: %#v", repo.failures)
	}
}

func TestAppleFailureFingerprintsServiceCodesAndSanitizesOperation(t *testing.T) {
	tests := []struct {
		name string
		code string
	}{
		{name: "empty"},
		{name: "safe", code: "HME.CODE-1_2"},
		{name: "long token", code: strings.Repeat("A", 512)},
		{name: "space", code: "BAD CODE"},
		{name: "leading space", code: " BAD_CODE"},
		{name: "slash", code: "BAD/CODE"},
		{name: "newline", code: "BAD\nCODE"},
		{name: "non ASCII", code: "错误码"},
		{name: "address", code: "secret@example.com"},
	}
	fingerprints := make(map[string]string, len(tests))
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
			repo := newFakeRepository()
			upstream := &apple.Error{
				Op:          "reserve secret@example.com",
				Kind:        apple.ErrService,
				StatusCode:  502,
				ServiceCode: test.code,
				Retryable:   true,
			}
			manager, logs := newFlowLogManager(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
				return domain.Alias{}, fmt.Errorf("wrapped: %w", upstream)
			})
			schedule := enableForTest(t, manager, int64(50+index))
			clock.Set(*schedule.NextRunAt)
			manager.runDue(context.Background())

			entry := requireAutoCreateEvent(t, logs, "run_failed")
			if entry.Fields["operation"] != "unknown" {
				t.Fatalf("unsafe operation was not sanitized: %#v", entry)
			}
			if _, exists := entry.Fields["service_code"]; exists {
				t.Fatalf("raw service code field was logged: %#v", entry)
			}
			present, presentExists := entry.Fields["service_code_present"]
			fingerprint, fingerprintExists := entry.Fields["service_code_fingerprint"]
			if test.code != "" {
				if !presentExists || present != "true" || !fingerprintExists || len(fingerprint) != appleServiceCodeFingerprintHexLength {
					t.Fatalf("fingerprint fields = %#v", entry)
				}
				if _, err := hex.DecodeString(fingerprint); err != nil {
					t.Fatalf("fingerprint is not hex: %q", fingerprint)
				}
				if previous, duplicate := fingerprints[fingerprint]; duplicate {
					t.Fatalf("different service codes %q and %q share fingerprint %q", previous, test.code, fingerprint)
				}
				fingerprints[fingerprint] = test.code
			} else if presentExists || fingerprintExists {
				t.Fatalf("empty service code produced fingerprint fields: %#v", entry)
			}
			assertFlowLogsDoNotContain(t, logs, "secret@example.com", test.code)
		})
	}
}

func equalTimes(left, right []time.Time) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !left[index].Equal(right[index]) {
			return false
		}
	}
	return true
}

func stringsContains(value, fragment string) bool {
	return len(fragment) == 0 || bytes.Contains([]byte(value), []byte(fragment))
}
