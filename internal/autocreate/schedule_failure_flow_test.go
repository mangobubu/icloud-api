package autocreate

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"icloud-api/internal/applog"
	"icloud-api/internal/domain"
)

func TestScheduleOperationFailureIsCorrelatedAndDiagnosable(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	repo.claimErr = errors.New("database claim unavailable")
	logs := applog.New(100)
	manager, err := New(
		repo,
		func(context.Context, int64) (domain.Alias, error) {
			t.Fatal("creator should not run when claiming the schedule fails")
			return domain.Alias{}, nil
		},
		slog.New(logs),
		WithClock(clock.Now),
		WithRandom(fixedRandom(0)),
	)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	schedule, err := manager.SetEnabled(context.Background(), 3, true)
	if err != nil {
		t.Fatalf("enable schedule: %v", err)
	}
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	entries := logs.List(applog.Filter{Limit: 100}).Items
	var failed *applog.Entry
	var started *applog.Entry
	for index := range entries {
		entry := entries[index]
		if entry.Fields["auto_create_event"] == "run_started" {
			started = &entry
		}
		if entry.Fields["auto_create_event"] == "run_failed" {
			failed = &entry
		}
	}
	if failed == nil {
		t.Fatalf("missing correlated schedule failure: %#v", entries)
	}
	if started == nil || started.Fields["auto_create_run_id"] != failed.Fields["auto_create_run_id"] {
		t.Fatalf("schedule failure flow is incomplete: started=%#v failed=%#v", started, failed)
	}
	fields := failed.Fields
	if failed.Level != slog.LevelError || fields["auto_create_run_id"] == "" {
		t.Fatalf("schedule failure correlation = %#v", failed)
	}
	if fields["auto_create_stage"] != string(domain.AliasCreationPhaseFailed) ||
		fields["failed_stage"] != string(domain.AliasCreationPhasePreparing) ||
		fields["failed_operation"] != "claim_schedule_slot" {
		t.Fatalf("schedule failure location = %#v", fields)
	}
	if fields["error_code"] != "AUTO_CREATE_SCHEDULE_ERROR" ||
		fields["error_class"] != "schedule" ||
		fields["cause_category"] != "schedule" ||
		fields["error_context"] == "" ||
		fields["error"] != fields["error_context"] {
		t.Fatalf("schedule failure diagnostics = %#v", fields)
	}
	if fields["schedule_action"] != "continue" ||
		fields["failed_operation"] == "" ||
		fields["next_run_at"] != schedule.NextRunAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("schedule failure follow-up = %#v", fields)
	}
}

func TestScheduleRescheduleCancellationIsNotReportedAsFailure(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC))
	ctx, expire := newTestDeadlineContext()
	defer expire()
	repo := newFakeRepository()
	repo.rescheduleErr = context.DeadlineExceeded
	repo.rescheduleHook = expire
	logs := applog.New(100)
	manager, err := New(
		repo,
		func(context.Context, int64) (domain.Alias, error) {
			t.Fatal("creator should not run while rescheduling an overdue slot")
			return domain.Alias{}, nil
		},
		slog.New(logs),
		WithClock(clock.Now),
		WithRandom(fixedRandom(0)),
		WithOverdueGrace(time.Minute),
	)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	schedule, err := manager.SetEnabled(context.Background(), 4, true)
	if err != nil {
		t.Fatalf("enable schedule: %v", err)
	}
	clock.Set(schedule.NextRunAt.Add(2 * time.Minute))
	manager.runDue(ctx)

	cancelled := requireAutoCreateEvent(t, logs, "run_cancelled")
	if cancelled.Fields["error_code"] != "CONTEXT_DEADLINE_EXCEEDED" ||
		cancelled.Fields["schedule_action"] != "continue" ||
		cancelled.Fields["next_run_at"] != schedule.NextRunAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("reschedule cancellation diagnostics = %#v", cancelled.Fields)
	}
	assertAutoCreateEventsAbsent(t, logs, "run_failed")
}

func TestDueScheduleListFailureHasGlobalStructuredDiagnostics(t *testing.T) {
	clock := newTestClock(time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	repo.listErr = errors.New("database list unavailable")
	logs := applog.New(100)
	manager, err := New(
		repo,
		func(context.Context, int64) (domain.Alias, error) {
			t.Fatal("creator should not run when listing schedules fails")
			return domain.Alias{}, nil
		},
		slog.New(logs),
		WithClock(clock.Now),
		WithRandom(fixedRandom(0)),
	)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	manager.runDue(context.Background())

	entries := logs.List(applog.Filter{Limit: 100}).Items
	var started, failed *applog.Entry
	for index := range entries {
		entry := entries[index]
		switch entry.Fields["auto_create_event"] {
		case "run_started":
			started = &entry
		case "run_failed":
			failed = &entry
		}
	}
	if started == nil || failed == nil || started.Fields["auto_create_run_id"] != failed.Fields["auto_create_run_id"] {
		t.Fatalf("global schedule diagnostics are incomplete: %#v", entries)
	}
	if failed.Fields["failed_operation"] != "list_due_schedules" ||
		failed.Fields["error_code"] != "AUTO_CREATE_SCHEDULE_ERROR" ||
		failed.Fields["cause_category"] != "schedule" {
		t.Fatalf("global schedule diagnostics = %#v", failed)
	}
	if _, exists := failed.Fields["account_id"]; exists {
		t.Fatalf("global schedule failure has a fabricated account ID: %#v", failed)
	}
}
