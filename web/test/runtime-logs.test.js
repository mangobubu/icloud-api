import assert from "node:assert/strict";
import test from "node:test";

import {
  appendRuntimeLogPage,
  buildRuntimeLogQuery,
  chronologicalRuntimeLogs,
  mergeRuntimeLogs,
  normalizeRuntimeLog,
  normalizeRuntimeLogPage,
  runtimeLogAttributesText,
  runtimeLogAutoCreateStageLabel,
  runtimeLogFlowContextText,
  runtimeLogLevelMeta,
  runtimeLogSyncStageLabel,
  runtimeLogSyncTriggerLabel,
} from "../src/utils/runtimeLogs.js";

test("runtime logs normalize timestamps, levels, attributes, and promoted context", () => {
  assert.deepEqual(
    normalizeRuntimeLog({
      id: 42,
      created_at: "2026-08-09T08:00:00Z",
      level: "WARNING",
      message: "主号同步失败",
      source: "syncer.manager",
      account_id: 12,
      request_id: "req-123",
      attributes: { attempt: 2, error: "connection closed" },
    }),
    {
      id: 42,
      time: "2026-08-09T08:00:00Z",
      level: "warn",
      message: "主号同步失败",
      source: "syncer.manager",
      accountId: 12,
      requestId: "req-123",
      syncRunId: "",
      syncStage: "",
      syncBatch: null,
      syncPercent: null,
      syncEvent: "",
      autoCreateRunId: "",
      autoCreateStage: "",
      autoCreatePercent: null,
      autoCreateEvent: "",
      syncTrigger: "",
      errorContext: "",
      failedStage: "",
      errorDetail: "connection closed",
      failedOperation: "",
      errorCode: "",
      errorClass: "",
      causeCategory: "",
      httpStatus: null,
      retryable: null,
      upstreamRetryable: null,
      elapsedMs: null,
      batchElapsedMs: null,
      scheduleAction: "",
      scheduledFor: "",
      attemptedAt: "",
      nextRunAt: "",
      remoteSideEffectPossible: null,
      pendingConfirmation: null,
      failureStateRecorded: null,
      autoCreationDisabled: null,
      resultStateRecorded: null,
      operation: "",
      serviceCodePresent: null,
      serviceCodeFingerprint: "",
      confirmationAttempt: null,
      attributes: { attempt: 2, error: "connection closed" },
    },
  );

  const fieldsLog = normalizeRuntimeLog({
    ID: 43,
    Time: "2026-08-09T08:01:00Z",
    Level: "fatal",
    Message: "request failed",
    Fields: { account_id: 13, request_id: "req-456" },
  });
  assert.equal(fieldsLog.level, "error");
  assert.equal(fieldsLog.accountId, 13);
  assert.equal(fieldsLog.requestId, "req-456");
});

test("runtime log pages normalize page totals while preserving flow cursors", () => {
  assert.deepEqual(
    normalizeRuntimeLogPage({
      items: [{ id: 9, timestamp: "2026-08-09T08:00:00Z", level: "INFO" }],
      pagination: { total: 91, limit: 50, offset: 50, has_more: true },
      next_before_id: 9,
    }),
    {
      items: [
        {
          id: 9,
          time: "2026-08-09T08:00:00Z",
          level: "info",
          message: "",
          source: "system",
          accountId: null,
          requestId: "",
          syncRunId: "",
          syncStage: "",
          syncBatch: null,
          syncPercent: null,
          syncEvent: "",
          autoCreateRunId: "",
          autoCreateStage: "",
          autoCreatePercent: null,
          autoCreateEvent: "",
          syncTrigger: "",
          errorContext: "",
          failedStage: "",
          errorDetail: "",
          failedOperation: "",
          errorCode: "",
          errorClass: "",
          causeCategory: "",
          httpStatus: null,
          retryable: null,
          upstreamRetryable: null,
          elapsedMs: null,
          batchElapsedMs: null,
          scheduleAction: "",
          scheduledFor: "",
          attemptedAt: "",
          nextRunAt: "",
          remoteSideEffectPossible: null,
          pendingConfirmation: null,
          failureStateRecorded: null,
          autoCreationDisabled: null,
          resultStateRecorded: null,
          operation: "",
          serviceCodePresent: null,
          serviceCodeFingerprint: "",
          confirmationAttempt: null,
          attributes: {},
        },
      ],
      hasMore: true,
      nextBeforeId: 9,
      total: 91,
      limit: 50,
      offset: 50,
    },
  );
});

test("runtime log queries trim filters, encode values, and clamp limits", () => {
  const query = buildRuntimeLogQuery({
    level: "ERROR",
    query: " connection closed ",
    accountId: 12,
    syncRunId: "sync-run-7",
    autoCreateRunId: "auto-run-7",
    limit: 500,
    offset: 175,
    beforeId: 91,
  });
  const parameters = new URLSearchParams(query);

  assert.deepEqual(Object.fromEntries(parameters), {
    level: "error",
    query: "connection closed",
    account_id: "12",
    sync_run_id: "sync-run-7",
    auto_create_run_id: "auto-run-7",
    limit: "500",
    offset: "175",
    before_id: "91",
  });
  assert.equal(buildRuntimeLogQuery({ limit: "invalid" }), "limit=20");
});

test("sync flow fields normalize from structured attributes", () => {
  const log = normalizeRuntimeLog({
    id: 91,
    created_at: "2026-08-09T08:02:00Z",
    level: "WARN",
    message: "邮件同步失败",
    attributes: {
      account_id: "12",
      sync_run_id: "sync-run-7",
      sync_stage: "FAILED",
      sync_batch: "2",
      sync_percent: "104",
      sync_event: "RUN_FAILED",
      trigger: "AUTOMATIC",
      failed_stage: "READING",
      failed_operation: "FETCH_INCREMENTAL",
      error_context: "fetch mailbox: connection closed",
      error: "fetch mailbox: connection closed",
      elapsed_ms: "3821",
    },
  });

  assert.equal(log.syncRunId, "sync-run-7");
  assert.equal(log.syncStage, "failed");
  assert.equal(log.syncBatch, 2);
  assert.equal(log.syncPercent, 100);
  assert.equal(log.syncEvent, "run_failed");
  assert.equal(log.syncTrigger, "automatic");
  assert.equal(log.failedStage, "reading");
  assert.equal(log.failedOperation, "fetch_incremental");
  assert.equal(log.errorDetail, "fetch mailbox: connection closed");
  assert.equal(log.errorContext, "fetch mailbox: connection closed");
  assert.equal(log.elapsedMs, 3821);
  assert.equal(runtimeLogFlowContextText(log), "");
  assert.equal(
    normalizeRuntimeLog({
      attributes: { error_context: "save result: database timeout" },
    }).errorDetail,
    "save result: database timeout",
  );
});

test("automatic creation flow fields normalize from top-level and grouped attributes", () => {
  const topLevelLog = normalizeRuntimeLog({
    auto_create_run_id: "auto-run-top",
    auto_create_stage: "CHECKING-ACCOUNT",
    auto_create_percent: "8",
    auto_create_event: "RUN-STARTED",
  });
  assert.equal(topLevelLog.autoCreateRunId, "auto-run-top");
  assert.equal(topLevelLog.autoCreateStage, "checking_account");
  assert.equal(topLevelLog.autoCreatePercent, 8);
  assert.equal(topLevelLog.autoCreateEvent, "run_started");

  const log = normalizeRuntimeLog({
    id: 92,
    created_at: "2026-08-09T08:03:00Z",
    level: "WARN",
    message: "自动创建隐私邮箱失败",
    attributes: {
      account_id: "12",
      "autocreate.auto_create_run_id": "auto-run-7",
      "autocreate.auto_create_stage": "FAILED",
      "autocreate.auto_create_percent": "104",
      "autocreate.auto_create_event": "RUN_FAILED",
      failed_stage: "RESERVING",
      failed_operation: "RESERVE_ALIAS",
      error: "Apple 请求被限流",
      error_code: "APPLE_RATE_LIMITED",
      error_class: "APPLE_UPSTREAM",
      cause_category: "APPLE_UPSTREAM",
      http_status: "429",
      retryable: "true",
      upstream_retryable: "true",
      elapsed_ms: "3821",
      schedule_action: "CONTINUE",
      scheduled_for: "2026-08-09T08:02:00Z",
      attempted_at: "2026-08-09T08:02:03Z",
      next_run_at: "2026-08-09T09:02:03Z",
      remote_side_effect_possible: "true",
      pending_confirmation: "false",
      service_code_present: "true",
      service_code_fingerprint: "deadbeef01234567",
    },
  });

  assert.equal(log.autoCreateRunId, "auto-run-7");
  assert.equal(log.autoCreateStage, "failed");
  assert.equal(log.autoCreatePercent, 100);
  assert.equal(log.autoCreateEvent, "run_failed");
  assert.equal(log.failedStage, "reserving");
  assert.equal(log.failedOperation, "reserve_alias");
  assert.equal(log.errorDetail, "Apple 请求被限流");
  assert.equal(log.errorCode, "APPLE_RATE_LIMITED");
  assert.equal(log.errorClass, "apple_upstream");
  assert.equal(log.causeCategory, "apple_upstream");
  assert.equal(log.httpStatus, 429);
  assert.equal(log.retryable, true);
  assert.equal(log.upstreamRetryable, true);
  assert.equal(log.elapsedMs, 3821);
  assert.equal(log.scheduleAction, "continue");
  assert.equal(log.scheduledFor, "2026-08-09T08:02:00Z");
  assert.equal(log.attemptedAt, "2026-08-09T08:02:03Z");
  assert.equal(log.nextRunAt, "2026-08-09T09:02:03Z");
  assert.equal(log.remoteSideEffectPossible, true);
  assert.equal(log.pendingConfirmation, false);
  assert.equal(log.serviceCodePresent, true);
  assert.equal(log.serviceCodeFingerprint, "deadbeef01234567");
  assert.equal(runtimeLogFlowContextText(log), "");
});

test("automatic creation diagnostics normalize status, retry, timing, and schedule fields", () => {
  const log = normalizeRuntimeLog({
    attributes: {
      error_code: "apple_rate_limited",
      error_class: "APPLE_UPSTREAM",
      cause_category: "PLAN_CONFLICT",
      http_status: "503.9",
      retryable: "FALSE",
      upstream_retryable: "yes",
      elapsed_ms: "-9",
      batch_elapsed_ms: "17.9",
      schedule_action: "DISABLED",
      remote_side_effect_possible: "not-a-bool",
      result_state_recorded: "false",
      confirmation_attempt: "2.8",
    },
  });

  assert.equal(log.errorCode, "APPLE_RATE_LIMITED");
  assert.equal(log.errorClass, "apple_upstream");
  assert.equal(log.causeCategory, "plan_conflict");
  assert.equal(log.httpStatus, 503);
  assert.equal(log.retryable, false);
  assert.equal(log.upstreamRetryable, true);
  assert.equal(log.elapsedMs, 0);
  assert.equal(log.batchElapsedMs, 17);
  assert.equal(log.scheduleAction, "disabled");
  assert.equal(log.remoteSideEffectPossible, null);
  assert.equal(log.resultStateRecorded, false);
  assert.equal(log.confirmationAttempt, 2);
});

test("runtime diagnostic aliases use non-empty attribute fallbacks and hide camelCase standard fields", () => {
  const log = normalizeRuntimeLog({
    cause_category: "",
    schedule_action: "",
    scheduled_for: "",
    attempted_at: "",
    next_run_at: "",
    remote_side_effect_possible: "",
    result_state_recorded: "",
    batch_elapsed_ms: "",
    attributes: {
      causeCategory: "PLAN_CONFLICT",
      scheduleAction: "CONTINUE",
      scheduledFor: "2026-08-09T08:02:00Z",
      attemptedAt: "2026-08-09T08:02:03Z",
      nextRunAt: "2026-08-09T09:02:03Z",
      remoteSideEffectPossible: "false",
      resultStateRecorded: "false",
      batchElapsedMs: "12",
      "other\\.cause_category": "literal-dot-context",
    },
  });

  assert.equal(log.causeCategory, "plan_conflict");
  assert.equal(log.scheduleAction, "continue");
  assert.equal(log.scheduledFor, "2026-08-09T08:02:00Z");
  assert.equal(log.attemptedAt, "2026-08-09T08:02:03Z");
  assert.equal(log.nextRunAt, "2026-08-09T09:02:03Z");
  assert.equal(log.remoteSideEffectPossible, false);
  assert.equal(log.resultStateRecorded, false);
  assert.equal(log.batchElapsedMs, 12);
  assert.equal(
    runtimeLogFlowContextText(log),
    '{\n  "other\\\\.cause_category": "literal-dot-context"\n}',
  );

  const groupedFallback = normalizeRuntimeLog({
    attributes: {
      cause_category: "",
      "flow.cause_category": "SCHEDULE",
    },
  });
  assert.equal(groupedFallback.causeCategory, "schedule");

  const explicitFalse = normalizeRuntimeLog({
    remote_side_effect_possible: false,
    attributes: { remote_side_effect_possible: "true" },
  });
  assert.equal(explicitFalse.remoteSideEffectPossible, false);

  const literalDot = normalizeRuntimeLog({
    attributes: { "other\\.cause_category": "literal-dot-context" },
  });
  assert.equal(literalDot.causeCategory, "");
  assert.match(runtimeLogFlowContextText(literalDot), /literal-dot-context/);
});

test("runtime log pages merge in order without duplicate IDs", () => {
  const latest = [{ id: 4 }, { id: 3 }];
  const existing = [{ id: 3, stale: true }, { id: 2 }, { id: 1 }];
  assert.deepEqual(
    mergeRuntimeLogs(latest, existing).map((item) => item.id),
    [4, 3, 2, 1],
  );
  assert.equal(mergeRuntimeLogs(latest, existing)[1].stale, undefined);
});

test("sync flow logs are deduplicated and ordered from start to finish", () => {
  const result = chronologicalRuntimeLogs([
    { id: 4, time: "2026-08-09T08:00:04Z" },
    { id: 2, time: "2026-08-09T08:00:02Z" },
    { id: 3, time: "2026-08-09T08:00:03Z" },
    { id: 2, time: "2026-08-09T08:00:02Z", duplicate: true },
  ]);

  assert.deepEqual(result.map((item) => item.id), [2, 3, 4]);
  assert.equal(result[0].duplicate, undefined);
});

test("loaded runtime log history is capped to the in-memory server window", () => {
  const current = Array.from({ length: 1990 }, (_, index) => ({
    id: 2100 - index,
  }));
  const page = {
    items: Array.from({ length: 50 }, (_, index) => ({ id: 110 - index })),
    hasMore: true,
    nextBeforeId: 61,
  };

  const result = appendRuntimeLogPage(current, page, 2000);

  assert.equal(result.items.length, 2000);
  assert.equal(result.items[0].id, 2100);
  assert.equal(result.items.at(-1).id, 101);
  assert.equal(result.hasMore, false);
  assert.equal(result.nextBeforeId, null);
});

test("runtime log display metadata and attributes remain predictable", () => {
  assert.deepEqual(runtimeLogLevelMeta("ERROR"), {
    label: "错误",
    type: "danger",
  });
  assert.equal(runtimeLogLevelMeta("custom").label, "CUSTOM");
  assert.equal(runtimeLogAttributesText(null), "");
  assert.equal(
    runtimeLogAttributesText({ error: "connection closed" }),
    '{\n  "error": "connection closed"\n}',
  );
  assert.equal(runtimeLogSyncStageLabel("preparing"), "准备同步数据");
  assert.equal(runtimeLogSyncStageLabel("failed"), "同步失败");
  assert.equal(runtimeLogAutoCreateStageLabel("preparing"), "准备自动创建");
  assert.equal(
    runtimeLogAutoCreateStageLabel("checking_forwarding"),
    "核对隐私邮箱转发目标",
  );
  assert.equal(
    runtimeLogAutoCreateStageLabel("initializing_forwarding"),
    "初始化隐私邮箱转发目标",
  );
  assert.equal(
    runtimeLogAutoCreateStageLabel("reconciling"),
    "等待并核对 Apple 隐私邮箱目录",
  );
  assert.equal(runtimeLogAutoCreateStageLabel("failed"), "自动创建失败");
  assert.equal(runtimeLogSyncTriggerLabel("manual"), "手动同步");
  assert.equal(runtimeLogSyncTriggerLabel("automatic"), "自动同步");
});
