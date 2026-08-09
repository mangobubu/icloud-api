package domain

import (
	"context"
	"time"
)

type MailboxSyncTrigger string

const (
	MailboxSyncTriggerManual    MailboxSyncTrigger = "manual"
	MailboxSyncTriggerAutomatic MailboxSyncTrigger = "automatic"
)

type MailboxSyncPhase string

const (
	MailboxSyncPhaseQueued         MailboxSyncPhase = "queued"
	MailboxSyncPhaseWaiting        MailboxSyncPhase = "waiting"
	MailboxSyncPhasePreparing      MailboxSyncPhase = "preparing"
	MailboxSyncPhaseConnecting     MailboxSyncPhase = "connecting"
	MailboxSyncPhaseAuthenticating MailboxSyncPhase = "authenticating"
	MailboxSyncPhaseScanning       MailboxSyncPhase = "scanning"
	MailboxSyncPhaseReading        MailboxSyncPhase = "reading"
	MailboxSyncPhaseValidating     MailboxSyncPhase = "validating"
	MailboxSyncPhaseSaving         MailboxSyncPhase = "saving"
)

// MailboxSyncProgress is the current process-local state of one account sync.
// Completed runs are removed from the manager and represented by the account's
// persisted last-sync fields instead.
type MailboxSyncProgress struct {
	AccountID int64
	Trigger   MailboxSyncTrigger
	Phase     MailboxSyncPhase
	Percent   int
	StartedAt time.Time
	UpdatedAt time.Time
}

// MailboxSyncProgressUpdate is emitted by mailbox fetchers while one bounded
// batch moves through its network and parsing stages.
type MailboxSyncProgressUpdate struct {
	Phase   MailboxSyncPhase
	Percent int
}

type mailboxSyncProgressReporterKey struct{}

// WithMailboxSyncProgressReporter attaches a lightweight synchronous progress
// callback to a fetch context. Reporters must return promptly.
func WithMailboxSyncProgressReporter(
	ctx context.Context,
	reporter func(MailboxSyncProgressUpdate),
) context.Context {
	if reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, mailboxSyncProgressReporterKey{}, reporter)
}

// ReportMailboxSyncProgress reports a fetch stage when the caller installed a
// reporter. Percent is normalized so a malformed producer cannot leak an
// invalid value into the public manager state.
func ReportMailboxSyncProgress(ctx context.Context, phase MailboxSyncPhase, percent int) {
	if ctx == nil {
		return
	}
	reporter, _ := ctx.Value(mailboxSyncProgressReporterKey{}).(func(MailboxSyncProgressUpdate))
	if reporter == nil {
		return
	}
	if percent < 0 {
		percent = 0
	} else if percent > 100 {
		percent = 100
	}
	reporter(MailboxSyncProgressUpdate{Phase: phase, Percent: percent})
}
