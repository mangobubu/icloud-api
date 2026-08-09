package syncer

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

const syncLogRedactedValue = "[REDACTED]"

func syncStageMessage(stage domain.MailboxSyncPhase) string {
	switch stage {
	case domain.MailboxSyncPhaseQueued:
		return "邮件同步已排队"
	case domain.MailboxSyncPhaseWaiting:
		return "邮件同步正在等待执行资源"
	case domain.MailboxSyncPhasePreparing:
		return "正在准备邮件同步"
	case domain.MailboxSyncPhaseConnecting:
		return "正在连接 IMAP 服务器"
	case domain.MailboxSyncPhaseAuthenticating:
		return "正在验证 IMAP 账号"
	case domain.MailboxSyncPhaseScanning:
		return "正在扫描邮箱"
	case domain.MailboxSyncPhaseReading:
		return "正在读取邮件"
	case domain.MailboxSyncPhaseValidating:
		return "正在校验同步结果"
	case domain.MailboxSyncPhaseSaving:
		return "正在保存同步结果"
	default:
		return "邮件同步阶段已更新"
	}
}

func (m *Manager) logSyncFlow(
	ctx context.Context,
	level slog.Level,
	message string,
	accountID int64,
	trigger domain.MailboxSyncTrigger,
	event string,
	flow syncFlowSnapshot,
	extra ...slog.Attr,
) {
	if m == nil || m.logger == nil || flow.runID == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now().UTC()
	batch := flow.batch
	if batch < 1 {
		batch = 1
	}
	attrs := []slog.Attr{
		slog.Int64("account_id", accountID),
		slog.String("trigger", string(trigger)),
		slog.String("sync_run_id", flow.runID),
		slog.Int("sync_batch", batch),
		slog.String("sync_stage", string(flow.stage)),
		slog.Int("sync_percent", normalizedActivePercent(flow.percent)),
		slog.String("sync_event", event),
		slog.Int64("elapsed_ms", elapsedMilliseconds(flow.startedAt, now)),
	}
	if !flow.batchStarted.IsZero() {
		attrs = append(attrs, slog.Int64("batch_elapsed_ms", elapsedMilliseconds(flow.batchStarted, now)))
	}
	attrs = append(attrs, extra...)
	m.logger.LogAttrs(ctx, level, message, attrs...)
}

func (m *Manager) logSyncBatchStarted(
	ctx context.Context,
	accountID int64,
	trigger domain.MailboxSyncTrigger,
	flow syncFlowSnapshot,
) {
	event := "batch_started"
	message := "邮件同步续跑批次开始"
	if flow.batch <= 1 {
		event = "run_started"
		message = "邮件同步开始"
	}
	m.logSyncFlow(ctx, slog.LevelInfo, message, accountID, trigger, event, flow)
}

func (m *Manager) logSyncFailure(
	ctx context.Context,
	accountID int64,
	trigger domain.MailboxSyncTrigger,
	fallback syncFlowSnapshot,
	failedOperation string,
	err error,
	sensitiveValues ...string,
) {
	if err == nil || errors.Is(err, ErrSyncPending) {
		return
	}
	flow, ok := m.currentSyncFlow(accountID, trigger)
	if !ok || fallback.batch > flow.batch {
		flow = fallback
	}
	failedStage := flow.stage
	errorText := redactSyncLogText(err.Error(), sensitiveValues...)
	if errors.Is(err, context.Canceled) {
		m.logSyncCancellation(ctx, accountID, trigger, flow, failedStage, failedOperation, errorText)
		return
	}
	flow.stage = domain.MailboxSyncPhase("failed")
	m.logSyncFlow(
		ctx,
		slog.LevelWarn,
		"邮件同步失败",
		accountID,
		trigger,
		"run_failed",
		flow,
		slog.String("failed_stage", string(failedStage)),
		slog.String("error_context", errorText),
		slog.String("failed_operation", failedOperation),
		slog.String("error", errorText),
	)
}

func (m *Manager) logSyncCancellation(
	ctx context.Context,
	accountID int64,
	trigger domain.MailboxSyncTrigger,
	flow syncFlowSnapshot,
	previousStage domain.MailboxSyncPhase,
	failedOperation string,
	detail string,
) {
	flow.stage = domain.MailboxSyncPhase("cancelled")
	m.logSyncFlow(
		ctx,
		slog.LevelInfo,
		"邮件同步已取消",
		accountID,
		trigger,
		"run_cancelled",
		flow,
		slog.String("failed_stage", string(previousStage)),
		slog.String("error_context", detail),
		slog.String("failed_operation", failedOperation),
		slog.String("error", detail),
	)
}

func elapsedMilliseconds(start, end time.Time) int64 {
	if start.IsZero() || !end.After(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

func redactSyncLogText(value string, sensitiveValues ...string) string {
	value = strings.TrimSpace(value)
	for _, sensitive := range sensitiveValues {
		sensitive = strings.TrimSpace(sensitive)
		if len(sensitive) < 3 {
			continue
		}
		value = strings.ReplaceAll(value, sensitive, syncLogRedactedValue)
	}
	return value
}
