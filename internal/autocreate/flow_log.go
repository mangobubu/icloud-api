package autocreate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

// aliasCreationFlow is intentionally process-local. The persisted schedule is
// the source of truth for retries; this snapshot only correlates the log
// records emitted while one claimed slot is being handled.
type aliasCreationFlow struct {
	runID     string
	startedAt time.Time
	state     *aliasCreationFlowState
}

type aliasCreationFlowState struct {
	mu                       sync.Mutex
	stage                    domain.AliasCreationPhase
	percent                  int
	lastEvent                string
	remoteSideEffectPossible bool
	terminal                 bool
}

func (m *Manager) newAliasCreationFlow(startedAt time.Time) aliasCreationFlow {
	startedAt = startedAt.UTC()
	if startedAt.IsZero() {
		startedAt = m.now()
	}
	sequence := m.runSeq.Add(1)
	return aliasCreationFlow{
		runID:     fmt.Sprintf("auto-create-%016x-%08x", uint64(startedAt.UnixNano()), sequence),
		startedAt: startedAt,
		state: &aliasCreationFlowState{
			stage:   domain.AliasCreationPhasePreparing,
			percent: 5,
		},
	}
}

func (flow aliasCreationFlow) currentStage() domain.AliasCreationPhase {
	if flow.state == nil {
		return domain.AliasCreationPhasePreparing
	}
	flow.state.mu.Lock()
	defer flow.state.mu.Unlock()
	return flow.state.stage
}

func (flow aliasCreationFlow) hasRemoteSideEffectPossible(stage domain.AliasCreationPhase) bool {
	if aliasCreationRemoteSideEffectPossible(stage) {
		return true
	}
	if flow.state == nil {
		return false
	}
	flow.state.mu.Lock()
	defer flow.state.mu.Unlock()
	return flow.state.remoteSideEffectPossible
}

func (flow aliasCreationFlow) hasRecordedRemoteSideEffect() bool {
	if flow.state == nil {
		return false
	}
	flow.state.mu.Lock()
	defer flow.state.mu.Unlock()
	return flow.state.remoteSideEffectPossible
}

func (flow aliasCreationFlow) hasRemoteSideEffectPossibleForError(stage domain.AliasCreationPhase, err error) bool {
	if stage == domain.AliasCreationPhaseReserving && explicitRateLimitRejection(err) {
		// A known HME throttle in a definitive HTTP response rejects the
		// business operation. Preserve an earlier forwarding mutation marker, but
		// do not infer an alias mutation from the reserve stage alone.
		return flow.hasRecordedRemoteSideEffect()
	}
	return flow.hasRemoteSideEffectPossible(stage)
}

func explicitRateLimitRejection(err error) bool {
	if err == nil || !aliasCreationRequiresRateLimitCooldown(err) {
		return false
	}
	var visit func(error) bool
	visit = func(current error) bool {
		if current == nil {
			return false
		}
		if upstream, ok := current.(*apple.Error); ok && upstream != nil {
			if apple.IsRateLimited(upstream) &&
				(upstream.StatusCode == http.StatusTooManyRequests ||
					upstream.StatusCode >= http.StatusOK && upstream.StatusCode < http.StatusMultipleChoices) {
				return true
			}
		}
		switch unwrapped := current.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range unwrapped.Unwrap() {
				if visit(child) {
					return true
				}
			}
		case interface{ Unwrap() error }:
			return visit(unwrapped.Unwrap())
		}
		return false
	}
	return visit(err)
}

func (flow aliasCreationFlow) markStarted() {
	if flow.state == nil {
		return
	}
	flow.state.mu.Lock()
	flow.state.lastEvent = "run_started"
	flow.state.mu.Unlock()
}

func (m *Manager) logAliasCreationFlow(
	ctx context.Context,
	level slog.Level,
	message string,
	accountID int64,
	flow aliasCreationFlow,
	event string,
	extra ...slog.Attr,
) {
	if m == nil || m.logger == nil || flow.runID == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if flow.state == nil {
		return
	}
	flow.state.mu.Lock()
	defer flow.state.mu.Unlock()
	m.logAliasCreationFlowLocked(ctx, level, message, accountID, flow, flow.state.stage, flow.state.percent, event, extra...)
}

func (m *Manager) logAliasCreationFlowLocked(
	ctx context.Context,
	level slog.Level,
	message string,
	accountID int64,
	flow aliasCreationFlow,
	stage domain.AliasCreationPhase,
	percent int,
	event string,
	extra ...slog.Attr,
) {
	now := m.now()
	attrs := make([]slog.Attr, 0, 7+len(extra))
	if accountID > 0 {
		attrs = append(attrs, slog.Int64("account_id", accountID))
	}
	attrs = append(attrs,
		slog.String("auto_create_run_id", flow.runID),
		slog.String("auto_create_stage", safeAliasCreationStage(stage)),
		slog.Int("auto_create_percent", normalizedAliasCreationPercent(percent)),
		slog.String("auto_create_event", safeAliasCreationEvent(event)),
		slog.Int64("elapsed_ms", elapsedAliasCreationMilliseconds(flow.startedAt, now)),
	)
	attrs = append(attrs, extra...)
	m.logger.LogAttrs(ctx, level, message, attrs...)
}

func (m *Manager) logAliasCreationProgress(
	ctx context.Context,
	accountID int64,
	flow *aliasCreationFlow,
	update domain.AliasCreationProgressUpdate,
) {
	if flow == nil {
		return
	}
	// The manager owns terminal records because it also knows whether schedule
	// state was persisted and whether another slot remains. Keep the last
	// service stage here so a failure points to the operation that actually ran.
	switch update.Phase {
	case domain.AliasCreationPhaseCompleted,
		domain.AliasCreationPhaseFailed,
		domain.AliasCreationPhaseCancelled:
		return
	}
	if flow.state == nil {
		return
	}
	flow.state.mu.Lock()
	defer flow.state.mu.Unlock()
	if flow.state.terminal {
		return
	}
	stage := update.Phase
	if stage == "" {
		stage = flow.state.stage
	}
	// Reaching reserve is conservatively treated as an ambiguous remote
	// operation, but merely entering that stage is not itself evidence that a
	// side effect occurred. Keep the recorded marker for mutations that happened
	// before reserve separate so an explicit Apple rejection can be reported as
	// non-mutating without weakening transport-failure diagnostics.
	if stage != domain.AliasCreationPhaseReserving && aliasCreationRemoteSideEffectPossible(stage) {
		flow.state.remoteSideEffectPossible = true
	}
	percent := normalizedAliasCreationPercent(update.Percent)
	if percent == 0 && flow.state.percent > 0 {
		percent = flow.state.percent
	}
	// A producer may report the same stage more than once while refreshing a
	// session. Keep the ring useful by retaining only actual transitions.
	if stage == flow.state.stage && percent == flow.state.percent && update.Attempt == 0 &&
		(flow.state.lastEvent == "run_started" || flow.state.lastEvent == "stage_started") {
		return
	}
	flow.state.stage = stage
	flow.state.percent = percent
	flow.state.lastEvent = "stage_started"
	extra := []slog.Attr{}
	if update.Attempt > 0 {
		extra = append(extra, slog.Int("confirmation_attempt", update.Attempt))
	}
	m.logAliasCreationFlowLocked(ctx, slog.LevelDebug, aliasCreationStageMessage(stage), accountID, *flow, stage, percent, "stage_started", extra...)
}

func (m *Manager) logAliasCreationFailure(
	ctx context.Context,
	accountID int64,
	flow aliasCreationFlow,
	failedStage domain.AliasCreationPhase,
	err error,
	scheduledFor, attemptedAt time.Time,
	nextRunAt *time.Time,
	failureRecorded, creationDisabled bool,
) {
	m.logAliasCreationFailureWithOperation(
		ctx,
		accountID,
		flow,
		failedStage,
		aliasCreationOperation(failedStage),
		err,
		scheduledFor,
		attemptedAt,
		nextRunAt,
		failureRecorded,
		creationDisabled,
	)
}

func (m *Manager) logAliasCreationFailureWithOperation(
	ctx context.Context,
	accountID int64,
	flow aliasCreationFlow,
	failedStage domain.AliasCreationPhase,
	failedOperation string,
	err error,
	scheduledFor, attemptedAt time.Time,
	nextRunAt *time.Time,
	failureRecorded, creationDisabled bool,
) {
	info := diagnoseAliasCreationError(err)
	if info.code == "ALIAS_LIMIT_REACHED" && !creationDisabled {
		info.reason = "主号已达到隐私邮箱容量上限，但关闭自动创建计划失败，请检查数据库状态"
	}
	if aliasCreationUntrackedRemoteSideEffect(err) {
		if creationDisabled {
			info.reason = "Apple 可能已创建地址但本地候选未保存，自动创建已暂停；请先对账 Apple 目录再重新开启"
		} else {
			info.reason = "Apple 可能已创建地址但本地候选未保存，且自动创建暂停失败；请立即检查数据库和 Apple 目录"
		}
	}
	if failedOperation == "" {
		failedOperation = aliasCreationOperation(failedStage)
	}
	causeCategory := aliasCreationCauseCategory(err, info)
	attributes := []slog.Attr{
		slog.String("failed_stage", safeAliasCreationStage(failedStage)),
		slog.String("failed_operation", failedOperation),
		slog.String("error_code", info.code),
		slog.String("error_class", info.class),
		slog.String("cause_category", causeCategory),
		slog.String("error_context", info.reason),
		slog.String("error", info.reason),
		slog.Bool("failure_state_recorded", failureRecorded),
		slog.Bool("auto_creation_disabled", creationDisabled),
		slog.String("schedule_action", aliasCreationScheduleAction(creationDisabled, nextRunAt)),
		slog.Bool("remote_side_effect_possible", flow.hasRemoteSideEffectPossibleForError(failedStage, err)),
		slog.Bool("pending_confirmation", aliasCreationPendingConfirmation(err, info.code)),
	}
	attributes = append(attributes, aliasCreationTimingAttrs(scheduledFor, attemptedAt, nextRunAt)...)
	if info.upstream != nil {
		attributes = append(attributes,
			slog.String("operation", safeAppleOperation(info.upstream.Op)),
			slog.Bool("retryable", info.retryable),
			slog.Bool("upstream_retryable", info.retryable),
		)
		if info.upstream.StatusCode > 0 {
			attributes = append(attributes, slog.Int("http_status", info.upstream.StatusCode))
		}
		if fingerprint, ok := appleServiceCodeFingerprint(info.upstream.ServiceCode); ok {
			attributes = append(attributes,
				slog.Bool("service_code_present", true),
				slog.String("service_code_fingerprint", fingerprint),
			)
		}
	}
	if flow.state == nil {
		return
	}
	flow.state.mu.Lock()
	defer flow.state.mu.Unlock()
	if flow.state.terminal {
		return
	}
	flow.state.stage = domain.AliasCreationPhaseFailed
	flow.state.terminal = true
	m.logAliasCreationFlowLocked(ctx, slog.LevelError, "自动创建隐私邮箱失败", accountID, flow, flow.state.stage, flow.state.percent, "run_failed", attributes...)
}

func (m *Manager) logAliasCreationCancellation(
	ctx context.Context,
	accountID int64,
	flow aliasCreationFlow,
	previousStage domain.AliasCreationPhase,
	scheduledFor, attemptedAt time.Time,
	nextRunAt *time.Time,
) {
	m.logAliasCreationCancellationWithError(ctx, accountID, flow, previousStage, context.Canceled, scheduledFor, attemptedAt, nextRunAt)
}

func (m *Manager) logAliasCreationCancellationWithError(
	ctx context.Context,
	accountID int64,
	flow aliasCreationFlow,
	previousStage domain.AliasCreationPhase,
	cause error,
	scheduledFor, attemptedAt time.Time,
	nextRunAt *time.Time,
) {
	code := "CONTEXT_CANCELED"
	if errors.Is(cause, context.DeadlineExceeded) {
		code = "CONTEXT_DEADLINE_EXCEEDED"
	}
	reason := aliasCreationErrorReason(code)
	attributes := []slog.Attr{
		slog.String("failed_stage", safeAliasCreationStage(previousStage)),
		slog.String("failed_operation", aliasCreationOperation(previousStage)),
		slog.String("error_code", code),
		slog.String("error_class", "context"),
		slog.String("cause_category", "context"),
		slog.String("error_context", reason),
		slog.String("error", reason),
		slog.String("schedule_action", aliasCreationScheduleAction(false, nextRunAt)),
		slog.Bool("remote_side_effect_possible", flow.hasRemoteSideEffectPossibleForError(previousStage, cause)),
		slog.Bool("pending_confirmation", aliasCreationPendingConfirmation(cause, code)),
	}
	attributes = append(attributes, aliasCreationTimingAttrs(scheduledFor, attemptedAt, nextRunAt)...)
	if flow.state == nil {
		return
	}
	flow.state.mu.Lock()
	defer flow.state.mu.Unlock()
	if flow.state.terminal {
		return
	}
	flow.state.stage = domain.AliasCreationPhaseCancelled
	flow.state.terminal = true
	message := "自动创建隐私邮箱已取消"
	if code == "CONTEXT_DEADLINE_EXCEEDED" {
		message = "自动创建隐私邮箱超时"
	}
	m.logAliasCreationFlowLocked(ctx, slog.LevelWarn, message, accountID, flow, flow.state.stage, flow.state.percent, "run_cancelled", attributes...)
}

func (m *Manager) logAliasCreationCompleted(
	ctx context.Context,
	accountID int64,
	flow aliasCreationFlow,
	aliasID int64,
	scheduledFor, attemptedAt time.Time,
	nextRunAt *time.Time,
	resultRecorded bool,
) {
	attributes := []slog.Attr{
		slog.Int64("alias_id", aliasID),
		slog.Bool("result_state_recorded", resultRecorded),
		slog.String("schedule_action", aliasCreationScheduleAction(false, nextRunAt)),
	}
	attributes = append(attributes, aliasCreationTimingAttrs(scheduledFor, attemptedAt, nextRunAt)...)
	level := slog.LevelInfo
	message := "自动创建隐私邮箱完成"
	event := "run_completed"
	if !resultRecorded {
		level = slog.LevelWarn
		message = "自动创建隐私邮箱完成，但计划状态记录失败"
		event = "run_completed_with_warning"
	}
	if flow.state == nil {
		return
	}
	flow.state.mu.Lock()
	defer flow.state.mu.Unlock()
	if flow.state.terminal {
		return
	}
	flow.state.stage = domain.AliasCreationPhaseCompleted
	flow.state.percent = 100
	flow.state.terminal = true
	m.logAliasCreationFlowLocked(ctx, level, message, accountID, flow, flow.state.stage, flow.state.percent, event, attributes...)
}

func (m *Manager) logAliasCreationStateError(
	ctx context.Context,
	message string,
	accountID int64,
	flow aliasCreationFlow,
	operation string,
	err error,
) {
	info := diagnoseAliasCreationError(err)
	info.code = "AUTO_CREATION_PERSISTENCE_ERROR"
	info.class = "persistence"
	info.reason = aliasCreationPersistenceReason(operation)
	m.logAliasCreationFlow(
		ctx,
		slog.LevelError,
		message,
		accountID,
		flow,
		"state_persist_failed",
		slog.String("failed_operation", operation),
		slog.String("error_code", info.code),
		slog.String("error_class", "persistence"),
		slog.String("cause_category", "persistence"),
		slog.String("error_context", info.reason),
		slog.String("error", info.reason),
	)
}

func aliasCreationPersistenceReason(operation string) string {
	switch operation {
	case "record_creation_failure":
		return "自动创建失败原因已确定，但失败状态写入数据库失败"
	case "record_creation_success":
		return "隐私邮箱已创建，但成功结果写入数据库失败"
	case "disable_creation_schedule":
		return "已达到隐私邮箱容量上限，但关闭自动创建计划失败"
	case "pause_after_untracked_remote_side_effect":
		return "Apple 可能已创建地址但本地候选未保存，暂停自动创建计划失败"
	case "reschedule_rate_limit_cooldown":
		return "Apple 已触发创建限流，但推迟后续自动创建计划失败"
	case "record_plan_correction_failure":
		return "认领计划后修正失败状态写入数据库失败"
	default:
		return "自动创建结果已确定，但计划状态写入数据库失败"
	}
}

func aliasCreationTimingAttrs(scheduledFor, attemptedAt time.Time, nextRunAt *time.Time) []slog.Attr {
	attributes := make([]slog.Attr, 0, 3)
	if !scheduledFor.IsZero() {
		attributes = append(attributes, slog.Time("scheduled_for", scheduledFor.UTC()))
	}
	if !attemptedAt.IsZero() {
		attributes = append(attributes, slog.Time("attempted_at", attemptedAt.UTC()))
	}
	if nextRunAt != nil && !nextRunAt.IsZero() {
		attributes = append(attributes, slog.Time("next_run_at", nextRunAt.UTC()))
	}
	return attributes
}

func aliasCreationScheduleAction(disabled bool, nextRunAt *time.Time) string {
	if disabled {
		return "disabled"
	}
	if nextRunAt != nil && !nextRunAt.IsZero() {
		return "continue"
	}
	return "none"
}

func aliasCreationStageMessage(stage domain.AliasCreationPhase) string {
	switch stage {
	case domain.AliasCreationPhasePreparing:
		return "正在准备自动创建隐私邮箱"
	case domain.AliasCreationPhaseCheckingAccount:
		return "正在读取主号状态"
	case domain.AliasCreationPhaseCheckingCapacity:
		return "正在检查隐私邮箱容量和待确认任务"
	case domain.AliasCreationPhaseLoadingSession:
		return "正在读取 Apple 登录会话"
	case domain.AliasCreationPhaseValidatingSession:
		return "正在验证 Apple 登录会话"
	case domain.AliasCreationPhaseCheckingForwarding:
		return "正在核对隐私邮箱转发目标"
	case domain.AliasCreationPhaseInitializingForwarding:
		return "正在初始化隐私邮箱转发目标"
	case domain.AliasCreationPhasePreparingKey:
		return "正在准备本地 API Key"
	case domain.AliasCreationPhaseReserving:
		return "正在向 Apple 请求创建隐私邮箱"
	case domain.AliasCreationPhaseSavingCandidate:
		return "正在保存待确认的隐私邮箱"
	case domain.AliasCreationPhaseConfirming:
		return "正在确认隐私邮箱目录记录"
	case domain.AliasCreationPhaseReconciling:
		return "正在等待并核对 Apple 隐私邮箱目录"
	case domain.AliasCreationPhaseSavingResult:
		return "正在保存自动创建结果"
	case domain.AliasCreationPhaseCompleted:
		return "自动创建隐私邮箱完成"
	case domain.AliasCreationPhaseFailed:
		return "自动创建隐私邮箱失败"
	case domain.AliasCreationPhaseCancelled:
		return "自动创建隐私邮箱已取消"
	default:
		return "自动创建隐私邮箱阶段已更新"
	}
}

func safeAliasCreationStage(value domain.AliasCreationPhase) string {
	value = domain.AliasCreationPhase(strings.TrimSpace(string(value)))
	if !isKnownAliasCreationPhase(value) {
		return "unknown"
	}
	return string(value)
}

func isKnownAliasCreationPhase(value domain.AliasCreationPhase) bool {
	switch value {
	case domain.AliasCreationPhasePreparing,
		domain.AliasCreationPhaseCheckingAccount,
		domain.AliasCreationPhaseCheckingCapacity,
		domain.AliasCreationPhaseLoadingSession,
		domain.AliasCreationPhaseValidatingSession,
		domain.AliasCreationPhaseCheckingForwarding,
		domain.AliasCreationPhaseInitializingForwarding,
		domain.AliasCreationPhasePreparingKey,
		domain.AliasCreationPhaseReserving,
		domain.AliasCreationPhaseSavingCandidate,
		domain.AliasCreationPhaseConfirming,
		domain.AliasCreationPhaseReconciling,
		domain.AliasCreationPhaseSavingResult,
		domain.AliasCreationPhaseCompleted,
		domain.AliasCreationPhaseFailed,
		domain.AliasCreationPhaseCancelled:
		return true
	default:
		return false
	}
}

func safeAliasCreationEvent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !isSafeDiagnosticToken(value) {
		return "stage_started"
	}
	return value
}

func normalizedAliasCreationPercent(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func elapsedAliasCreationMilliseconds(start, end time.Time) int64 {
	if start.IsZero() || !end.After(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

type diagnosticCodeProvider interface {
	DiagnosticCode() string
}

type pendingConfirmationProvider interface {
	PendingConfirmation() bool
}

type remoteSideEffectProvider interface {
	RemoteSideEffectPossible() bool
}

type aliasCreationErrorInfo struct {
	code      string
	class     string
	reason    string
	upstream  *apple.Error
	retryable bool
}

func diagnoseAliasCreationError(err error) aliasCreationErrorInfo {
	info := aliasCreationErrorInfo{
		class:  "internal",
		reason: "自动创建在本地处理阶段失败，请结合失败步骤和部署日志排查",
	}
	if err == nil {
		return info
	}
	if errors.Is(err, ErrCapacityReached) {
		info.code = "ALIAS_LIMIT_REACHED"
	} else {
		var coded diagnosticCodeProvider
		if errors.As(err, &coded) {
			info.code = strings.TrimSpace(coded.DiagnosticCode())
		}
		if !isAllowedAliasCreationErrorCode(info.code) {
			info.code = ""
		}
	}
	if info.code == "" {
		switch {
		case aliasCreationContextOnlyError(err) && errors.Is(err, context.Canceled):
			info.code = "CONTEXT_CANCELED"
		case aliasCreationContextOnlyError(err) && errors.Is(err, context.DeadlineExceeded):
			info.code = "CONTEXT_DEADLINE_EXCEEDED"
		case errors.Is(err, errAliasCreationPlanCorrection), errors.Is(err, errAliasCreationPlanConflict):
			info.code = "AUTO_CREATE_PLAN_CORRECTION_FAILED"
		case errors.Is(err, errEmptyAliasAddress):
			info.code = "APPLE_INVALID_ALIAS_RESPONSE"
		case errors.Is(err, apple.ErrInvalidSession):
			info.code = "APPLE_SESSION_EXPIRED"
		case errors.Is(err, apple.ErrAuthentication):
			info.code = "APPLE_CREDENTIALS_INVALID"
		case errors.Is(err, apple.ErrTermsRequired):
			info.code = "APPLE_ACCOUNT_ACTION_REQUIRED"
		}
	}
	if info.code == "" {
		switch strings.TrimSpace(err.Error()) {
		case "主号已停用":
			info.code = "ACCOUNT_DISABLED"
		case "automatic alias creation persistence is unavailable",
			"automatic alias creation client is unavailable",
			"automatic alias key encryption is unavailable":
			info.code = "AUTO_CREATION_UNAVAILABLE"
		}
	}
	var upstream *apple.Error
	if errors.As(err, &upstream) && upstream != nil {
		info.upstream = upstream
		info.retryable = upstream.Retryable
		if info.code == "" || info.code == "AUTO_CREATE_FAILED" || info.code == "APPLE_UPSTREAM_ERROR" {
			switch {
			case apple.IsRateLimited(err):
				info.code = "APPLE_RATE_LIMITED"
			case errors.Is(upstream, apple.ErrInvalidSession):
				info.code = "APPLE_SESSION_EXPIRED"
			case errors.Is(upstream, apple.ErrAuthentication):
				info.code = "APPLE_CREDENTIALS_INVALID"
			case errors.Is(upstream, apple.ErrTermsRequired):
				info.code = "APPLE_ACCOUNT_ACTION_REQUIRED"
			default:
				info.code = "APPLE_UPSTREAM_ERROR"
			}
		}
	}
	if info.code == "" || !isAllowedAliasCreationErrorCode(info.code) {
		info.code = "AUTO_CREATE_FAILED"
	}
	info.class = aliasCreationErrorClass(info.code)
	info.reason = aliasCreationErrorReason(info.code)
	return info
}

func isAllowedAliasCreationErrorCode(code string) bool {
	switch code {
	case "AUTO_CREATE_FAILED",
		"ALIAS_LIMIT_REACHED",
		"CONTEXT_CANCELED",
		"CONTEXT_DEADLINE_EXCEEDED",
		"APPLE_INVALID_ALIAS_RESPONSE",
		"APPLE_LOGIN_REQUIRED",
		"APPLE_SESSION_EXPIRED",
		"APPLE_CREDENTIALS_INVALID",
		"APPLE_VERIFICATION_INVALID",
		"APPLE_FLOW_EXPIRED",
		"APPLE_ACCOUNT_ACTION_REQUIRED",
		"APPLE_RATE_LIMITED",
		"APPLE_UPSTREAM_ERROR",
		"APPLE_ALIAS_CONFIRMATION_PENDING",
		"APPLE_ACCOUNT_MISMATCH",
		"APPLE_FORWARDING_TARGET_MISSING",
		"ACCOUNT_CHANGED",
		"ALIAS_OWNERSHIP_CONFLICT",
		"ACCOUNT_DISABLED",
		"AUTO_CREATION_UNAVAILABLE",
		"AUTO_CREATE_PLAN_CORRECTION_FAILED",
		"AUTO_CREATE_SCHEDULE_ERROR",
		"AUTO_CREATION_PERSISTENCE_ERROR",
		"AUTO_CREATION_CRYPTO_ERROR":
		return true
	default:
		return false
	}
}

func aliasCreationErrorClass(code string) string {
	switch {
	case code == "ALIAS_LIMIT_REACHED":
		return "capacity"
	case code == "AUTO_CREATE_PLAN_CORRECTION_FAILED" || code == "AUTO_CREATION_PERSISTENCE_ERROR":
		return "persistence"
	case code == "AUTO_CREATE_SCHEDULE_ERROR":
		return "schedule"
	case code == "AUTO_CREATION_CRYPTO_ERROR":
		return "crypto"
	case code == "CONTEXT_CANCELED" || code == "CONTEXT_DEADLINE_EXCEEDED":
		return "context"
	case code == "APPLE_ACCOUNT_ACTION_REQUIRED" || code == "APPLE_ACCOUNT_MISMATCH" ||
		code == "APPLE_FORWARDING_TARGET_MISSING" ||
		code == "ACCOUNT_CHANGED" || code == "ALIAS_OWNERSHIP_CONFLICT" || code == "ACCOUNT_DISABLED":
		return "account_state"
	case strings.HasPrefix(code, "APPLE_SESSION") || strings.HasPrefix(code, "APPLE_LOGIN") ||
		code == "APPLE_CREDENTIALS_INVALID" || code == "APPLE_VERIFICATION_INVALID" || code == "APPLE_FLOW_EXPIRED":
		return "session"
	case strings.HasPrefix(code, "APPLE_"):
		return "apple_upstream"
	case strings.Contains(code, "KEY") || strings.Contains(code, "CRYPTO") || strings.Contains(code, "ENCRYPT"):
		return "crypto"
	case strings.Contains(code, "ACCOUNT"):
		return "account_state"
	default:
		return "internal"
	}
}

func aliasCreationCauseCategory(err error, info aliasCreationErrorInfo) string {
	switch {
	case errors.Is(err, errAliasCreationPlanConflict):
		return "plan_conflict"
	case errors.Is(err, errAliasCreationPlanCorrection):
		return "persistence"
	}
	switch info.class {
	case "capacity", "context", "session", "account_state", "apple_upstream", "crypto", "persistence", "schedule":
		return info.class
	default:
		return "internal"
	}
}

func aliasCreationPendingConfirmation(err error, code string) bool {
	if code == "APPLE_ALIAS_CONFIRMATION_PENDING" {
		return true
	}
	var provider pendingConfirmationProvider
	return errors.As(err, &provider) && provider.PendingConfirmation()
}

func aliasCreationUntrackedRemoteSideEffect(err error) bool {
	var remote remoteSideEffectProvider
	if !errors.As(err, &remote) || !remote.RemoteSideEffectPossible() {
		return false
	}
	var pending pendingConfirmationProvider
	return !errors.As(err, &pending) || !pending.PendingConfirmation()
}

func aliasCreationContextOnlyError(err error) bool {
	if err == nil {
		return false
	}
	if coded, ok := err.(diagnosticCodeProvider); ok {
		code := strings.TrimSpace(coded.DiagnosticCode())
		if code != "" {
			return code == "CONTEXT_CANCELED" || code == "CONTEXT_DEADLINE_EXCEEDED"
		}
	}
	if _, ok := err.(*apple.Error); ok {
		return false
	}
	switch unwrapped := err.(type) {
	case interface{ Unwrap() []error }:
		children := unwrapped.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !aliasCreationContextOnlyError(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		if child := unwrapped.Unwrap(); child != nil {
			return aliasCreationContextOnlyError(child)
		}
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}

func aliasCreationErrorReason(code string) string {
	switch code {
	case "APPLE_LOGIN_REQUIRED":
		return "Apple 账户尚未登录，请先同步隐私邮箱并完成登录"
	case "APPLE_SESSION_EXPIRED":
		return "Apple 登录会话已过期，请重新登录后再执行自动创建"
	case "APPLE_CREDENTIALS_INVALID":
		return "Apple 登录凭据无效，请重新登录 Apple 账户"
	case "APPLE_VERIFICATION_INVALID":
		return "Apple 双重认证状态无效，请重新登录 Apple 账户"
	case "APPLE_FLOW_EXPIRED":
		return "Apple 登录验证流程已过期，请重新登录 Apple 账户"
	case "APPLE_ACCOUNT_ACTION_REQUIRED":
		return "Apple 账户需要完成条款确认或其他账户操作"
	case "APPLE_RATE_LIMITED":
		return "Apple 请求被限流，当前周期剩余计划槽已跳过，冷却后会继续执行"
	case "APPLE_ACCOUNT_MISMATCH":
		return "Apple 登录账户或默认转发目标与当前主号不匹配"
	case "APPLE_FORWARDING_TARGET_MISSING":
		return "Apple 未能确认隐私邮箱的默认转发目标，本次未发起创建；请确认当前主号可作为转发邮箱，或先在 iCloud 手动创建一个隐私邮箱"
	case "APPLE_ALIAS_CONFIRMATION_PENDING":
		return "Apple 地址已创建但目录确认尚未完成，后续计划会继续确认"
	case "ALIAS_LIMIT_REACHED":
		return "主号已达到隐私邮箱容量上限"
	case "ACCOUNT_CHANGED":
		return "主号信息在创建过程中发生变化，本次操作未发布结果"
	case "ALIAS_OWNERSHIP_CONFLICT":
		return "Apple 创建的隐私邮箱归属于其他主号"
	case "ACCOUNT_DISABLED":
		return "当前主号已停用，请先启用主号"
	case "AUTO_CREATION_UNAVAILABLE":
		return "自动创建服务未正确初始化"
	case "AUTO_CREATE_PLAN_CORRECTION_FAILED":
		return "认领计划后无法保存下一次执行时间，本次远端创建未开始，请检查数据库状态"
	case "AUTO_CREATE_SCHEDULE_ERROR":
		return "自动创建计划处理失败，请结合失败操作检查计划计算或数据库状态"
	case "AUTO_CREATION_PERSISTENCE_ERROR":
		return "隐私邮箱已处理，但本地结果或计划状态未能保存，请检查数据库状态"
	case "AUTO_CREATION_CRYPTO_ERROR":
		return "自动创建所需的本地密钥处理失败，请检查密钥配置"
	case "CONTEXT_CANCELED":
		return "自动创建任务被服务停止或请求取消"
	case "CONTEXT_DEADLINE_EXCEEDED":
		return "自动创建操作超时，后续计划会继续执行"
	case "APPLE_INVALID_ALIAS_RESPONSE":
		return "Apple 返回的隐私邮箱地址无效"
	case "APPLE_UPSTREAM_ERROR":
		return "Apple 服务返回异常，请结合 HTTP 状态和服务码指纹排查"
	default:
		return "自动创建在本地处理阶段失败，请结合失败步骤和部署日志排查"
	}
}

func isSafeDiagnosticToken(value string) bool {
	if value == "" || len([]rune(value)) > 96 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func aliasCreationOperation(stage domain.AliasCreationPhase) string {
	switch stage {
	case domain.AliasCreationPhaseCheckingAccount:
		return "load_account"
	case domain.AliasCreationPhaseCheckingCapacity:
		return "check_capacity"
	case domain.AliasCreationPhaseLoadingSession:
		return "load_apple_session"
	case domain.AliasCreationPhaseValidatingSession:
		return "validate_apple_session"
	case domain.AliasCreationPhaseCheckingForwarding:
		return "list_aliases_and_check_forwarding"
	case domain.AliasCreationPhaseInitializingForwarding:
		return "initialize_forwarding_target"
	case domain.AliasCreationPhasePreparingKey:
		return "prepare_api_key"
	case domain.AliasCreationPhaseReserving:
		return "reserve_alias"
	case domain.AliasCreationPhaseSavingCandidate:
		return "persist_pending_alias"
	case domain.AliasCreationPhaseConfirming:
		return "confirm_alias"
	case domain.AliasCreationPhaseReconciling:
		return "reconcile_alias_directory"
	case domain.AliasCreationPhaseSavingResult:
		return "save_creation_result"
	default:
		return "create_alias"
	}
}

func aliasCreationRemoteSideEffectPossible(stage domain.AliasCreationPhase) bool {
	switch stage {
	case domain.AliasCreationPhaseInitializingForwarding,
		domain.AliasCreationPhaseReserving,
		domain.AliasCreationPhaseSavingCandidate,
		domain.AliasCreationPhaseConfirming,
		domain.AliasCreationPhaseReconciling,
		domain.AliasCreationPhaseSavingResult:
		return true
	default:
		return false
	}
}
