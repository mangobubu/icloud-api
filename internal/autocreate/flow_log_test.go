package autocreate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/applog"
	"icloud-api/internal/domain"
)

type testDiagnosticError struct {
	code   string
	detail string
}

func (e testDiagnosticError) Error() string          { return e.detail }
func (e testDiagnosticError) DiagnosticCode() string { return e.code }

type testPendingConfirmationError struct {
	err error
}

func (e testPendingConfirmationError) Error() string             { return e.err.Error() }
func (e testPendingConfirmationError) Unwrap() error             { return e.err }
func (e testPendingConfirmationError) PendingConfirmation() bool { return true }

type testDeadlineContext struct {
	context.Context
	done chan struct{}
}

func newTestDeadlineContext() (*testDeadlineContext, func()) {
	ctx := &testDeadlineContext{Context: context.Background(), done: make(chan struct{})}
	return ctx, func() {
		select {
		case <-ctx.done:
		default:
			close(ctx.done)
		}
	}
}

func (c *testDeadlineContext) Done() <-chan struct{} { return c.done }
func (c *testDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func newFlowLogManager(
	t *testing.T,
	repo Repository,
	clock *testClock,
	creator Creator,
) (*Manager, *applog.Handler) {
	t.Helper()
	logs := applog.New(100)
	manager, err := New(
		repo,
		creator,
		slog.New(logs),
		WithClock(clock.Now),
		WithRandom(fixedRandom(0)),
	)
	if err != nil {
		t.Fatalf("new flow log manager: %v", err)
	}
	return manager, logs
}

type separateTerminalStateContextRepository struct {
	*fakeRepository
	failureContext       context.Context
	disableContext       context.Context
	freshContextObserved bool
}

func (r *separateTerminalStateContextRepository) RecordAliasCreationFailure(
	ctx context.Context,
	_ int64,
	_ time.Time,
	_ string,
) error {
	r.failureContext = ctx
	return errors.New("record failure state unavailable")
}

func (r *separateTerminalStateContextRepository) DisableAliasCreation(
	ctx context.Context,
	accountID int64,
	now time.Time,
) error {
	r.disableContext = ctx
	if r.failureContext != nil && ctx == r.failureContext {
		return errors.New("disable reused failure state context")
	}
	if !errors.Is(r.failureContext.Err(), context.Canceled) {
		return errors.New("failure state context was not cancelled")
	}
	if ctx.Err() != nil {
		return errors.New("disable received an expired context")
	}
	r.freshContextObserved = true
	return r.fakeRepository.DisableAliasCreation(ctx, accountID, now)
}

func autoCreateLogEntries(t *testing.T, logs *applog.Handler) []applog.Entry {
	t.Helper()
	page := logs.List(applog.Filter{Limit: 100})
	entries := append([]applog.Entry(nil), page.Items...)
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	return entries
}

func requireAutoCreateEvent(t *testing.T, logs *applog.Handler, event string) applog.Entry {
	t.Helper()
	var matches []applog.Entry
	for _, entry := range autoCreateLogEntries(t, logs) {
		if entry.Fields["auto_create_event"] == event {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("auto-create event %q count = %d, want 1; entries=%#v", event, len(matches), autoCreateLogEntries(t, logs))
	}
	return matches[0]
}

func assertAutoCreateEventsAbsent(t *testing.T, logs *applog.Handler, events ...string) {
	t.Helper()
	for _, entry := range autoCreateLogEntries(t, logs) {
		for _, event := range events {
			if entry.Fields["auto_create_event"] == event {
				t.Fatalf("unexpected auto-create event %q: %#v", event, entry)
			}
		}
	}
}

func assertFlowLogsDoNotContain(t *testing.T, logs *applog.Handler, sensitiveValues ...string) {
	t.Helper()
	encoded, err := json.Marshal(autoCreateLogEntries(t, logs))
	if err != nil {
		t.Fatalf("encode captured flow logs: %v", err)
	}
	for _, sensitive := range sensitiveValues {
		if sensitive != "" && strings.Contains(string(encoded), sensitive) {
			t.Fatalf("sensitive value %q leaked into flow logs: %s", sensitive, encoded)
		}
	}
}

func TestAliasCreationSuccessLogsCorrelatedFlowWithoutAddress(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	const address = "flow-success-secret@example.com"
	manager, logs := newFlowLogManager(t, repo, clock, func(ctx context.Context, accountID int64) (domain.Alias, error) {
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseCheckingAccount, 20, 0)
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReserving, 65, 0)
		return domain.Alias{ID: 71, AccountID: accountID, Address: address}, nil
	})
	schedule := enableForTest(t, manager, 71)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	started := requireAutoCreateEvent(t, logs, "run_started")
	checking := requireAutoCreateStage(t, logs, domain.AliasCreationPhaseCheckingAccount)
	reserving := requireAutoCreateStage(t, logs, domain.AliasCreationPhaseReserving)
	completed := requireAutoCreateEvent(t, logs, "run_completed")
	runID := started.Fields["auto_create_run_id"]
	if runID == "" {
		t.Fatalf("run_started has empty run id: %#v", started)
	}
	for _, entry := range []applog.Entry{checking, reserving, completed} {
		if entry.Fields["auto_create_run_id"] != runID {
			t.Fatalf("flow run id = %q, want %q; entry=%#v", entry.Fields["auto_create_run_id"], runID, entry)
		}
	}
	if started.Level != slog.LevelDebug || checking.Level != slog.LevelDebug || reserving.Level != slog.LevelDebug {
		t.Fatalf("debug flow levels = %v/%v/%v, want DEBUG", started.Level, checking.Level, reserving.Level)
	}
	if completed.Level != slog.LevelInfo || completed.Fields["auto_create_stage"] != string(domain.AliasCreationPhaseCompleted) ||
		completed.Fields["auto_create_percent"] != "100" || completed.Fields["alias_id"] != "71" {
		t.Fatalf("completed flow entry = %#v", completed)
	}
	assertAutoCreateEventsAbsent(t, logs, "run_failed", "run_cancelled")
	assertFlowLogsDoNotContain(t, logs, address)
}

func requireAutoCreateStage(t *testing.T, logs *applog.Handler, stage domain.AliasCreationPhase) applog.Entry {
	t.Helper()
	var matches []applog.Entry
	for _, entry := range autoCreateLogEntries(t, logs) {
		if entry.Fields["auto_create_event"] == "stage_started" && entry.Fields["auto_create_stage"] == string(stage) {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("auto-create stage %q count = %d, want 1; entries=%#v", stage, len(matches), autoCreateLogEntries(t, logs))
	}
	return matches[0]
}

func TestAliasCreationFailureLogsStableDiagnosticCode(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	const sensitive = "coded-flow-secret@example.com"
	manager, logs := newFlowLogManager(t, repo, clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseConfirming, 84, 2)
		return domain.Alias{}, fmt.Errorf("wrapped %s: %w", sensitive, testDiagnosticError{
			code:   "APPLE_ALIAS_CONFIRMATION_PENDING",
			detail: "diagnostic detail for " + sensitive,
		})
	})
	schedule := enableForTest(t, manager, 73)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	failed := requireAutoCreateEvent(t, logs, "run_failed")
	if failed.Fields["error_code"] != "APPLE_ALIAS_CONFIRMATION_PENDING" ||
		failed.Fields["failed_stage"] != string(domain.AliasCreationPhaseConfirming) ||
		failed.Fields["pending_confirmation"] != "true" {
		t.Fatalf("coded failure entry = %#v", failed)
	}
	assertFlowLogsDoNotContain(t, logs, sensitive, "diagnostic detail")
}

func TestAliasCreationTransportFailureOmitsHTTPStatus(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	manager, logs := newFlowLogManager(t, repo, clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReserving, 65, 0)
		return domain.Alias{}, &apple.Error{
			Op:         "reserve alias",
			Kind:       apple.ErrService,
			StatusCode: 0,
			Retryable:  true,
		}
	})
	schedule := enableForTest(t, manager, 76)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	failed := requireAutoCreateEvent(t, logs, "run_failed")
	if _, exists := failed.Fields["http_status"]; exists {
		t.Fatalf("transport failure unexpectedly logged an HTTP status: %#v", failed)
	}
	if failed.Fields["retryable"] != "true" || failed.Fields["operation"] != "reserve alias" {
		t.Fatalf("transport failure diagnostics = %#v", failed)
	}
}

func TestAliasCreationCancellationLogsTerminalEvent(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	ctx, cancel := context.WithCancel(context.Background())
	manager, logs := newFlowLogManager(t, repo, clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseValidatingSession, 38, 0)
		cancel()
		return domain.Alias{}, ctx.Err()
	})
	schedule := enableForTest(t, manager, 74)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(ctx)

	cancelled := requireAutoCreateEvent(t, logs, "run_cancelled")
	if cancelled.Fields["auto_create_stage"] != string(domain.AliasCreationPhaseCancelled) ||
		cancelled.Fields["failed_stage"] != string(domain.AliasCreationPhaseValidatingSession) ||
		cancelled.Fields["error_code"] != "CONTEXT_CANCELED" {
		t.Fatalf("cancelled flow entry = %#v", cancelled)
	}
	assertAutoCreateEventsAbsent(t, logs, "run_failed", "run_completed")
}

func TestAliasCreationCancellationReportsRemotePendingCandidate(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	ctx, cancel := context.WithCancel(context.Background())
	manager, logs := newFlowLogManager(t, repo, clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReconciling, 82, 2)
		cancel()
		return domain.Alias{}, testPendingConfirmationError{err: ctx.Err()}
	})
	schedule := enableForTest(t, manager, 79)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(ctx)

	cancelled := requireAutoCreateEvent(t, logs, "run_cancelled")
	if cancelled.Fields["remote_side_effect_possible"] != "true" ||
		cancelled.Fields["pending_confirmation"] != "true" ||
		cancelled.Fields["cause_category"] != "context" {
		t.Fatalf("pending cancellation diagnostics = %#v", cancelled)
	}
}

func TestAliasCreationRealFailureWinsCancellationRaceAndIsRecorded(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	ctx, cancel := context.WithCancel(context.Background())
	manager, logs := newFlowLogManager(t, repo, clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReserving, 65, 0)
		cancel()
		return domain.Alias{}, testDiagnosticError{code: "APPLE_UPSTREAM_ERROR", detail: "sensitive upstream detail"}
	})
	schedule := enableForTest(t, manager, 80)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(ctx)

	failed := requireAutoCreateEvent(t, logs, "run_failed")
	if failed.Fields["error_code"] != "APPLE_UPSTREAM_ERROR" ||
		failed.Fields["failure_state_recorded"] != "true" ||
		failed.Fields["cause_category"] != "apple_upstream" {
		t.Fatalf("cancellation race failure diagnostics = %#v", failed)
	}
	if len(repo.failures) != 1 || strings.Contains(repo.failures[0].message, "sensitive upstream detail") {
		t.Fatalf("cancellation race failure state = %#v", repo.failures)
	}
	assertAutoCreateEventsAbsent(t, logs, "run_cancelled")
}

func TestAliasCreationDeadlineLogsTimeoutReason(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	ctx, expire := newTestDeadlineContext()
	defer expire()
	repo.claimHook = expire
	manager, logs := newFlowLogManager(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		t.Fatal("creator should not run after the deadline")
		return domain.Alias{}, nil
	})
	schedule := enableForTest(t, manager, 77)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(ctx)

	cancelled := requireAutoCreateEvent(t, logs, "run_cancelled")
	if cancelled.Fields["error_code"] != "CONTEXT_DEADLINE_EXCEEDED" ||
		cancelled.Message != "自动创建隐私邮箱超时" || cancelled.Fields["error_context"] != aliasCreationErrorReason("CONTEXT_DEADLINE_EXCEEDED") {
		t.Fatalf("deadline flow entry = %#v", cancelled)
	}
}

func TestAliasCreationPlanCorrectionFailureHasCorrelatedDiagnostics(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	repo.claimHook = func() { clock.Advance(time.Minute) }
	repo.rescheduleErr = errors.New("database correction failed")
	manager, logs := newFlowLogManager(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		t.Fatal("creator should not run when plan correction fails")
		return domain.Alias{}, nil
	})
	schedule := enableForTest(t, manager, 78)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	failed := requireAutoCreateEvent(t, logs, "run_failed")
	if failed.Fields["auto_create_run_id"] == "" ||
		failed.Fields["failed_operation"] != "correct_schedule_after_claim" ||
		failed.Fields["error_code"] != "AUTO_CREATE_PLAN_CORRECTION_FAILED" ||
		failed.Fields["failure_state_recorded"] != "true" ||
		failed.Fields["cause_category"] != "persistence" {
		t.Fatalf("plan correction failure entry = %#v", failed)
	}
	if len(repo.failures) != 1 || repo.failures[0].message != aliasCreationErrorReason("AUTO_CREATE_PLAN_CORRECTION_FAILED") {
		t.Fatalf("plan correction failure state = %#v", repo.failures)
	}
}

func TestAliasCreationCapacityFailureDisablesSchedule(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	const sensitive = "capacity-flow-secret@example.com"
	manager, logs := newFlowLogManager(t, repo, clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseCheckingCapacity, 25, 0)
		return domain.Alias{}, fmt.Errorf("capacity check for %s: %w", sensitive, ErrCapacityReached)
	})
	schedule := enableForTest(t, manager, 75)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	failed := requireAutoCreateEvent(t, logs, "run_failed")
	if failed.Fields["error_code"] != "ALIAS_LIMIT_REACHED" ||
		failed.Fields["failed_stage"] != string(domain.AliasCreationPhaseCheckingCapacity) ||
		failed.Fields["schedule_action"] != "disabled" ||
		failed.Fields["auto_creation_disabled"] != "true" {
		t.Fatalf("capacity failure entry = %#v", failed)
	}
	current, err := manager.GetSchedule(context.Background(), 75)
	if err != nil {
		t.Fatalf("read disabled schedule: %v", err)
	}
	if current.Enabled || current.NextRunAt != nil {
		t.Fatalf("capacity schedule was not disabled: %#v", current)
	}
	if current.LastError != aliasCreationErrorReason("ALIAS_LIMIT_REACHED") {
		t.Fatalf("capacity failure state = %q", current.LastError)
	}
	assertFlowLogsDoNotContain(t, logs, sensitive)
}

func TestAliasCreationCapacityFailureReportsSchedulePauseFailure(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	repo.disableErr = errors.New("database disable unavailable")
	manager, logs := newFlowLogManager(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		return domain.Alias{}, ErrCapacityReached
	})
	schedule := enableForTest(t, manager, 79)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	failed := requireAutoCreateEvent(t, logs, "run_failed")
	if failed.Fields["error_code"] != "ALIAS_LIMIT_REACHED" ||
		failed.Fields["auto_creation_disabled"] != "false" ||
		failed.Fields["schedule_action"] != "continue" ||
		failed.Fields["next_run_at"] == "" ||
		!strings.Contains(failed.Fields["error_context"], "关闭自动创建计划失败") {
		t.Fatalf("capacity pause failure diagnostics = %#v", failed.Fields)
	}
	stateFailure := requireAutoCreateEvent(t, logs, "state_persist_failed")
	if stateFailure.Fields["failed_operation"] != "disable_creation_schedule" ||
		stateFailure.Fields["error_code"] != "AUTO_CREATION_PERSISTENCE_ERROR" {
		t.Fatalf("capacity pause state failure = %#v", stateFailure.Fields)
	}
	current, err := manager.GetSchedule(context.Background(), 79)
	if err != nil {
		t.Fatalf("read schedule after pause failure: %v", err)
	}
	if !current.Enabled || current.NextRunAt == nil {
		t.Fatalf("schedule state after pause failure = %#v", current)
	}
}

func TestAliasCreationFailureUsesFreshContextForSchedulePause(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC))
	repo := &separateTerminalStateContextRepository{fakeRepository: newFakeRepository()}
	manager, logs := newFlowLogManager(t, repo, clock, func(context.Context, int64) (domain.Alias, error) {
		return domain.Alias{}, ErrCapacityReached
	})
	schedule := enableForTest(t, manager, 76)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	if repo.failureContext == nil || repo.disableContext == nil || repo.failureContext == repo.disableContext || !repo.freshContextObserved {
		t.Fatalf("terminal state operations reused a context: failure=%#v disable=%#v", repo.failureContext, repo.disableContext)
	}
	current, err := manager.GetSchedule(context.Background(), 76)
	if err != nil {
		t.Fatalf("read paused schedule: %v", err)
	}
	if current.Enabled || current.NextRunAt != nil {
		t.Fatalf("capacity schedule was not paused after failure-state write error: %#v", current)
	}
	failed := requireAutoCreateEvent(t, logs, "run_failed")
	if failed.Fields["failure_state_recorded"] != "false" || failed.Fields["auto_creation_disabled"] != "true" {
		t.Fatalf("terminal state diagnostics = %#v", failed.Fields)
	}
}
