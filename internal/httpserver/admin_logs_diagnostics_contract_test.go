package httpserver

import (
	"log/slog"
	"testing"

	"icloud-api/internal/applog"
)

// Automatic-creation failure fields are carried in attributes so the API can
// evolve the diagnostic set without changing the top-level log envelope.
func TestAdminAPIApplicationLogPreservesAutoCreateFailureDiagnostics(t *testing.T) {
	entry := applog.Entry{
		ID:      17,
		Level:   slog.LevelError,
		Message: "automatic alias creation failed",
		Source:  "internal/autocreate/flow_log.go:247",
		Fields: map[string]string{
			"account_id":                  "3",
			"auto_create_run_id":          "auto-create-run-1",
			"auto_create_stage":           "failed",
			"auto_create_percent":         "65",
			"auto_create_event":           "run_failed",
			"failed_stage":                "reserving",
			"failed_operation":            "reserve_alias",
			"error_code":                  "APPLE_RATE_LIMITED",
			"error_class":                 "apple_upstream",
			"error_context":               "Apple rate limit reached",
			"error":                       "Apple rate limit reached",
			"http_status":                 "429",
			"retryable":                   "true",
			"upstream_retryable":          "true",
			"schedule_action":             "continue",
			"next_run_at":                 "2026-08-09T10:00:00Z",
			"remote_side_effect_possible": "true",
		},
	}

	dto := adminAPIApplicationLogFromEntry(entry)
	if dto.AutoCreateRunID != "auto-create-run-1" {
		t.Fatalf("auto-create run id = %q", dto.AutoCreateRunID)
	}
	if dto.AccountID == nil || *dto.AccountID != 3 {
		t.Fatalf("account id = %#v", dto.AccountID)
	}

	want := map[string]string{
		"auto_create_stage":           "failed",
		"auto_create_event":           "run_failed",
		"failed_stage":                "reserving",
		"failed_operation":            "reserve_alias",
		"error_code":                  "APPLE_RATE_LIMITED",
		"error_class":                 "apple_upstream",
		"error_context":               "Apple rate limit reached",
		"error":                       "Apple rate limit reached",
		"http_status":                 "429",
		"retryable":                   "true",
		"schedule_action":             "continue",
		"next_run_at":                 "2026-08-09T10:00:00Z",
		"remote_side_effect_possible": "true",
	}
	for key, expected := range want {
		if got := dto.Attributes[key]; got != expected {
			t.Errorf("attributes[%q] = %q, want %q; attributes=%#v", key, got, expected, dto.Attributes)
		}
	}
}
