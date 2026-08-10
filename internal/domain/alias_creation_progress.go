package domain

import "context"

// AliasCreationPhase identifies one non-sensitive stage of a Hide My Email
// automatic-creation attempt.
type AliasCreationPhase string

const (
	AliasCreationPhasePreparing              AliasCreationPhase = "preparing"
	AliasCreationPhaseCheckingAccount        AliasCreationPhase = "checking_account"
	AliasCreationPhaseCheckingCapacity       AliasCreationPhase = "checking_capacity"
	AliasCreationPhaseLoadingSession         AliasCreationPhase = "loading_session"
	AliasCreationPhaseValidatingSession      AliasCreationPhase = "validating_session"
	AliasCreationPhaseCheckingForwarding     AliasCreationPhase = "checking_forwarding"
	AliasCreationPhaseInitializingForwarding AliasCreationPhase = "initializing_forwarding"
	AliasCreationPhasePreparingKey           AliasCreationPhase = "preparing_key"
	AliasCreationPhaseReserving              AliasCreationPhase = "reserving"
	AliasCreationPhaseSavingCandidate        AliasCreationPhase = "saving_candidate"
	AliasCreationPhaseConfirming             AliasCreationPhase = "confirming"
	AliasCreationPhaseReconciling            AliasCreationPhase = "reconciling"
	AliasCreationPhaseSavingResult           AliasCreationPhase = "saving_result"
	AliasCreationPhaseCompleted              AliasCreationPhase = "completed"
	AliasCreationPhaseFailed                 AliasCreationPhase = "failed"
	AliasCreationPhaseCancelled              AliasCreationPhase = "cancelled"
)

// AliasCreationProgressUpdate contains only process diagnostics. Alias
// addresses, API keys, Apple sessions, and other credentials must never be
// added to this contract.
type AliasCreationProgressUpdate struct {
	Phase   AliasCreationPhase
	Percent int
	Attempt int
}

type aliasCreationProgressReporterKey struct{}

// WithAliasCreationProgressReporter attaches a lightweight synchronous
// callback to an automatic-creation context. Reporters must return promptly.
func WithAliasCreationProgressReporter(
	ctx context.Context,
	reporter func(AliasCreationProgressUpdate),
) context.Context {
	if reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, aliasCreationProgressReporterKey{}, reporter)
}

// ReportAliasCreationProgress reports a creation stage when the caller
// installed a reporter. Percent is normalized before it reaches the consumer.
func ReportAliasCreationProgress(
	ctx context.Context,
	phase AliasCreationPhase,
	percent, attempt int,
) {
	if ctx == nil {
		return
	}
	reporter, _ := ctx.Value(aliasCreationProgressReporterKey{}).(func(AliasCreationProgressUpdate))
	if reporter == nil {
		return
	}
	if percent < 0 {
		percent = 0
	} else if percent > 100 {
		percent = 100
	}
	reporter(AliasCreationProgressUpdate{
		Phase:   phase,
		Percent: percent,
		Attempt: attempt,
	})
}
