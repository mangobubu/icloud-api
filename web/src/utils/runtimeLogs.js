const LEVEL_ALIASES = {
  warning: "warn",
  fatal: "error",
  trace: "debug",
};

const SYNC_STAGE_LABELS = Object.freeze({
  queued: "等待开始",
  waiting: "等待同步资源",
  preparing: "准备同步数据",
  connecting: "连接邮箱服务器",
  authenticating: "验证邮箱账户",
  scanning: "扫描邮箱",
  fetching: "获取邮件",
  reading: "读取邮件",
  validating: "核对邮件状态",
  saving: "保存同步结果",
  completed: "同步完成",
  failed: "同步失败",
  cancelled: "同步已取消",
});

const AUTO_CREATE_STAGE_LABELS = Object.freeze({
  preparing: "准备自动创建",
  checking_account: "读取主号状态",
  checking_capacity: "检查容量和待确认任务",
  loading_session: "读取 Apple 登录会话",
  validating_session: "验证 Apple 登录会话",
  checking_forwarding: "核对隐私邮箱转发目标",
  preparing_key: "准备本地 API Key",
  reserving: "请求 Apple 创建隐私邮箱",
  saving_candidate: "保存待确认隐私邮箱",
  confirming: "确认隐私邮箱目录记录",
  reconciling: "等待并核对 Apple 隐私邮箱目录",
  saving_result: "保存自动创建结果",
  completed: "自动创建完成",
  failed: "自动创建失败",
  cancelled: "自动创建已取消",
});

const SYNC_TRIGGER_LABELS = Object.freeze({
  manual: "手动同步",
  automatic: "自动同步",
});

const FLOW_ATTRIBUTE_NAMES = new Set([
  "account_id",
  "request_id",
  "sync_run_id",
  "sync_batch",
  "sync_stage",
  "sync_percent",
  "sync_event",
  "auto_create_run_id",
  "auto_create_stage",
  "auto_create_percent",
  "auto_create_event",
  "trigger",
  "error_context",
  "error",
  "error_detail",
  "failed_stage",
  "failed_operation",
  "error_code",
  "error_class",
  "cause_category",
  "http_status",
  "retryable",
  "upstream_retryable",
  "elapsed_ms",
  "batch_elapsed_ms",
  "schedule_action",
  "scheduled_for",
  "attempted_at",
  "next_run_at",
  "remote_side_effect_possible",
  "pending_confirmation",
  "failure_state_recorded",
  "auto_creation_disabled",
  "result_state_recorded",
  "operation",
  "service_code_present",
  "service_code_fingerprint",
  "confirmation_attempt",
]);

function firstDefined(object, ...keys) {
  for (const key of keys) {
    if (object && object[key] !== undefined) {
      return object[key];
    }
  }
  return undefined;
}

function objectValue(value) {
  return value && typeof value === "object" && !Array.isArray(value)
    ? { ...value }
    : {};
}

function normalizedToken(value) {
  return String(value ?? "")
    .trim()
    .toLowerCase()
    .replaceAll("-", "_");
}

function normalizedAttributeName(value) {
  return String(value ?? "")
    .trim()
    .replace(/([A-Z]+)([A-Z][a-z])/g, "$1_$2")
    .replace(/([a-z0-9])([A-Z])/g, "$1_$2")
    .replaceAll("-", "_")
    .toLowerCase();
}

function attributeLeaf(value) {
  const text = String(value ?? "");
  let leafStart = 0;
  let escaped = false;
  for (let index = 0; index < text.length; index += 1) {
    const character = text[index];
    if (escaped) {
      escaped = false;
      continue;
    }
    if (character === "\\") {
      escaped = true;
      continue;
    }
    if (character === ".") leafStart = index + 1;
  }
  return text
    .slice(leafStart)
    .replaceAll("\\.", ".")
    .replaceAll("\\\\", "\\");
}

function isMeaningful(value) {
  return (
    value !== undefined &&
    value !== null &&
    !(typeof value === "string" && value.trim() === "")
  );
}

function firstMeaningful(...values) {
  for (const value of values) {
    if (!isMeaningful(value)) continue;
    return value;
  }
  return undefined;
}

function valueWithAttributeFallback(value, attributes, ...names) {
  return firstMeaningful(value, attributeValue(attributes, ...names));
}

function attributeValue(attributes, ...names) {
  for (const name of names) {
    const exact = firstDefined(attributes, name);
    if (isMeaningful(exact)) return exact;

    const normalizedName = normalizedAttributeName(name);
    const matchingKey = Object.keys(attributes).find((key) => {
      const leaf = attributeLeaf(key);
      return (
        normalizedAttributeName(leaf) === normalizedName &&
        isMeaningful(attributes[key])
      );
    });
    if (matchingKey !== undefined) return attributes[matchingKey];
  }
  return undefined;
}

function normalizedNullableNumber(value, { minimum = 0, maximum } = {}) {
  if (value === null || value === undefined || value === "" || typeof value === "boolean") {
    return null;
  }
  const number = Number(value);
  if (!Number.isFinite(number)) return null;
  const bounded = Math.max(minimum, number);
  return maximum === undefined ? bounded : Math.min(maximum, bounded);
}

function normalizedNullableInteger(value, options = {}) {
  const number = normalizedNullableNumber(value, options);
  return number === null ? null : Math.trunc(number);
}

function normalizedNullableBoolean(value) {
  if (value === null || value === undefined || value === "") return null;
  if (typeof value === "boolean") return value;
  const normalized = String(value).trim().toLowerCase();
  if (["true", "1", "yes"].includes(normalized)) return true;
  if (["false", "0", "no"].includes(normalized)) return false;
  return null;
}

function normalizedDiagnosticCode(value) {
  const normalized = String(value ?? "").trim();
  return normalized ? normalized.toUpperCase() : "";
}

function normalizedNullableText(value) {
  return value === null || value === undefined ? "" : String(value).trim();
}

export function normalizeRuntimeLogLevel(value) {
  const normalized = String(value || "info").trim().toLowerCase();
  return LEVEL_ALIASES[normalized] || normalized || "info";
}

export function normalizeRuntimeLog(raw = {}) {
  const attributes = objectValue(
    firstDefined(raw, "attributes", "Attributes", "fields", "Fields"),
  );
  const accountId = firstDefined(
    raw,
    "account_id",
    "accountId",
    "AccountID",
  );
  const requestId = firstDefined(
    raw,
    "request_id",
    "requestId",
    "RequestID",
  );
  const syncRunId = firstDefined(
    raw,
    "sync_run_id",
    "syncRunId",
    "SyncRunID",
  );
  const syncStage = firstDefined(raw, "sync_stage", "syncStage", "SyncStage");
  const syncBatch = firstDefined(raw, "sync_batch", "syncBatch", "SyncBatch");
  const syncPercent = firstDefined(
    raw,
    "sync_percent",
    "syncPercent",
    "SyncPercent",
  );
  const syncEvent = firstDefined(raw, "sync_event", "syncEvent", "SyncEvent");
  const autoCreateRunId = firstDefined(
    raw,
    "auto_create_run_id",
    "autoCreateRunId",
    "AutoCreateRunID",
    "AutoCreateRunId",
  );
  const autoCreateStage = firstDefined(
    raw,
    "auto_create_stage",
    "autoCreateStage",
    "AutoCreateStage",
  );
  const autoCreatePercent = firstDefined(
    raw,
    "auto_create_percent",
    "autoCreatePercent",
    "AutoCreatePercent",
  );
  const autoCreateEvent = firstDefined(
    raw,
    "auto_create_event",
    "autoCreateEvent",
    "AutoCreateEvent",
  );
  const syncTrigger = firstDefined(raw, "trigger", "Trigger");
  const errorContext = firstDefined(
    raw,
    "error_context",
    "errorContext",
    "ErrorContext",
  );
  const failedStage = firstDefined(
    raw,
    "failed_stage",
    "failedStage",
    "FailedStage",
  );
  const errorDetail = firstDefined(
    raw,
    "error",
    "Error",
    "error_detail",
    "errorDetail",
    "ErrorDetail",
  );
  const failedOperation = firstDefined(
    raw,
    "failed_operation",
    "failedOperation",
    "FailedOperation",
  );
  const errorCode = firstDefined(raw, "error_code", "errorCode", "ErrorCode");
  const errorClass = firstDefined(
    raw,
    "error_class",
    "errorClass",
    "ErrorClass",
  );
  const causeCategory = firstDefined(
    raw,
    "cause_category",
    "causeCategory",
    "CauseCategory",
  );
  const httpStatus = firstDefined(
    raw,
    "http_status",
    "httpStatus",
    "HTTPStatus",
  );
  const retryable = firstDefined(raw, "retryable", "Retryable");
  const upstreamRetryable = firstDefined(
    raw,
    "upstream_retryable",
    "upstreamRetryable",
    "UpstreamRetryable",
  );
  const elapsedMs = firstDefined(raw, "elapsed_ms", "elapsedMs", "ElapsedMs");
  const batchElapsedMs = firstDefined(
    raw,
    "batch_elapsed_ms",
    "batchElapsedMs",
    "BatchElapsedMs",
  );
  const scheduleAction = firstDefined(
    raw,
    "schedule_action",
    "scheduleAction",
    "ScheduleAction",
  );
  const scheduledFor = firstDefined(
    raw,
    "scheduled_for",
    "scheduledFor",
    "ScheduledFor",
  );
  const attemptedAt = firstDefined(
    raw,
    "attempted_at",
    "attemptedAt",
    "AttemptedAt",
  );
  const nextRunAt = firstDefined(raw, "next_run_at", "nextRunAt", "NextRunAt");
  const remoteSideEffectPossible = firstDefined(
    raw,
    "remote_side_effect_possible",
    "remoteSideEffectPossible",
    "RemoteSideEffectPossible",
  );
  const pendingConfirmation = firstDefined(
    raw,
    "pending_confirmation",
    "pendingConfirmation",
    "PendingConfirmation",
  );
  const failureStateRecorded = firstDefined(
    raw,
    "failure_state_recorded",
    "failureStateRecorded",
    "FailureStateRecorded",
  );
  const autoCreationDisabled = firstDefined(
    raw,
    "auto_creation_disabled",
    "autoCreationDisabled",
    "AutoCreationDisabled",
  );
  const resultStateRecorded = firstDefined(
    raw,
    "result_state_recorded",
    "resultStateRecorded",
    "ResultStateRecorded",
  );
  const operation = firstDefined(raw, "operation", "Operation");
  const serviceCodePresent = firstDefined(
    raw,
    "service_code_present",
    "serviceCodePresent",
    "ServiceCodePresent",
  );
  const serviceCodeFingerprint = firstDefined(
    raw,
    "service_code_fingerprint",
    "serviceCodeFingerprint",
    "ServiceCodeFingerprint",
  );
  const confirmationAttempt = firstDefined(
    raw,
    "confirmation_attempt",
    "confirmationAttempt",
    "ConfirmationAttempt",
  );

  return {
    id: firstDefined(raw, "id", "ID"),
    time:
      firstDefined(
        raw,
        "time",
        "Time",
        "created_at",
        "createdAt",
        "CreatedAt",
        "timestamp",
        "Timestamp",
      ) || null,
    level: normalizeRuntimeLogLevel(firstDefined(raw, "level", "Level")),
    message: String(firstDefined(raw, "message", "Message") || ""),
    source: String(firstDefined(raw, "source", "Source") || "system"),
    accountId:
      valueWithAttributeFallback(
        accountId,
        attributes,
        "account_id",
        "accountId",
        "AccountID",
      ) ?? null,
    requestId: String(
      valueWithAttributeFallback(
        requestId,
        attributes,
        "request_id",
        "requestId",
        "RequestID",
      ) ?? "",
    ),
    syncRunId: String(
      valueWithAttributeFallback(
        syncRunId,
        attributes,
        "sync_run_id",
        "syncRunId",
        "SyncRunID",
      ) ?? "",
    ),
    syncStage: normalizedToken(
      valueWithAttributeFallback(
        syncStage,
        attributes,
        "sync_stage",
        "syncStage",
        "SyncStage",
      ),
    ),
    syncBatch: normalizedNullableNumber(
      valueWithAttributeFallback(
        syncBatch,
        attributes,
        "sync_batch",
        "syncBatch",
        "SyncBatch",
      ),
      { minimum: 1 },
    ),
    syncPercent: normalizedNullableNumber(
      valueWithAttributeFallback(
        syncPercent,
        attributes,
        "sync_percent",
        "syncPercent",
        "SyncPercent",
      ),
      { minimum: 0, maximum: 100 },
    ),
    syncEvent: normalizedToken(
      valueWithAttributeFallback(
        syncEvent,
        attributes,
        "sync_event",
        "syncEvent",
        "SyncEvent",
      ),
    ),
    autoCreateRunId: String(
      valueWithAttributeFallback(
        autoCreateRunId,
        attributes,
        "auto_create_run_id",
        "autoCreateRunId",
        "AutoCreateRunID",
        "AutoCreateRunId",
      ) ?? "",
    ),
    autoCreateStage: normalizedToken(
      valueWithAttributeFallback(
        autoCreateStage,
        attributes,
        "auto_create_stage",
        "autoCreateStage",
        "AutoCreateStage",
      ),
    ),
    autoCreatePercent: normalizedNullableNumber(
      valueWithAttributeFallback(
        autoCreatePercent,
        attributes,
        "auto_create_percent",
        "autoCreatePercent",
        "AutoCreatePercent",
      ),
      { minimum: 0, maximum: 100 },
    ),
    autoCreateEvent: normalizedToken(
      valueWithAttributeFallback(
        autoCreateEvent,
        attributes,
        "auto_create_event",
        "autoCreateEvent",
        "AutoCreateEvent",
      ),
    ),
    syncTrigger: normalizedToken(
      valueWithAttributeFallback(syncTrigger, attributes, "trigger", "Trigger"),
    ),
    errorContext: String(
      valueWithAttributeFallback(
        errorContext,
        attributes,
        "error_context",
        "errorContext",
        "ErrorContext",
      ) ?? "",
    ),
    failedStage: normalizedToken(
      valueWithAttributeFallback(
        failedStage,
        attributes,
        "failed_stage",
        "failedStage",
        "FailedStage",
      ),
    ),
    errorDetail: String(
      firstMeaningful(
        errorDetail,
        attributeValue(
          attributes,
          "error",
          "Error",
          "error_detail",
          "errorDetail",
          "ErrorDetail",
        ),
        errorContext,
        attributeValue(
          attributes,
          "error_context",
          "errorContext",
          "ErrorContext",
        ),
      ) ?? "",
    ),
    failedOperation: normalizedToken(
      valueWithAttributeFallback(
        failedOperation,
        attributes,
        "failed_operation",
        "failedOperation",
        "FailedOperation",
      ),
    ),
    errorCode: normalizedDiagnosticCode(
      valueWithAttributeFallback(
        errorCode,
        attributes,
        "error_code",
        "errorCode",
        "ErrorCode",
      ),
    ),
    errorClass: normalizedToken(
      valueWithAttributeFallback(
        errorClass,
        attributes,
        "error_class",
        "errorClass",
        "ErrorClass",
      ),
    ),
    causeCategory: normalizedToken(
      valueWithAttributeFallback(
        causeCategory,
        attributes,
        "cause_category",
        "causeCategory",
        "CauseCategory",
      ),
    ),
    httpStatus: normalizedNullableInteger(
      valueWithAttributeFallback(
        httpStatus,
        attributes,
        "http_status",
        "httpStatus",
        "HTTPStatus",
      ),
      { minimum: 100, maximum: 599 },
    ),
    retryable: normalizedNullableBoolean(
      valueWithAttributeFallback(retryable, attributes, "retryable", "Retryable"),
    ),
    upstreamRetryable: normalizedNullableBoolean(
      valueWithAttributeFallback(
        upstreamRetryable,
        attributes,
        "upstream_retryable",
        "upstreamRetryable",
        "UpstreamRetryable",
      ),
    ),
    elapsedMs: normalizedNullableInteger(
      valueWithAttributeFallback(
        elapsedMs,
        attributes,
        "elapsed_ms",
        "elapsedMs",
        "ElapsedMs",
      ),
    ),
    batchElapsedMs: normalizedNullableInteger(
      valueWithAttributeFallback(
        batchElapsedMs,
        attributes,
        "batch_elapsed_ms",
        "batchElapsedMs",
        "BatchElapsedMs",
      ),
    ),
    scheduleAction: normalizedToken(
      valueWithAttributeFallback(
        scheduleAction,
        attributes,
        "schedule_action",
        "scheduleAction",
        "ScheduleAction",
      ),
    ),
    scheduledFor: normalizedNullableText(
      valueWithAttributeFallback(
        scheduledFor,
        attributes,
        "scheduled_for",
        "scheduledFor",
        "ScheduledFor",
      ),
    ),
    attemptedAt: normalizedNullableText(
      valueWithAttributeFallback(
        attemptedAt,
        attributes,
        "attempted_at",
        "attemptedAt",
        "AttemptedAt",
      ),
    ),
    nextRunAt: normalizedNullableText(
      valueWithAttributeFallback(
        nextRunAt,
        attributes,
        "next_run_at",
        "nextRunAt",
        "NextRunAt",
      ),
    ),
    remoteSideEffectPossible: normalizedNullableBoolean(
      valueWithAttributeFallback(
        remoteSideEffectPossible,
        attributes,
        "remote_side_effect_possible",
        "remoteSideEffectPossible",
        "RemoteSideEffectPossible",
      ),
    ),
    pendingConfirmation: normalizedNullableBoolean(
      valueWithAttributeFallback(
        pendingConfirmation,
        attributes,
        "pending_confirmation",
        "pendingConfirmation",
        "PendingConfirmation",
      ),
    ),
    failureStateRecorded: normalizedNullableBoolean(
      valueWithAttributeFallback(
        failureStateRecorded,
        attributes,
        "failure_state_recorded",
        "failureStateRecorded",
        "FailureStateRecorded",
      ),
    ),
    autoCreationDisabled: normalizedNullableBoolean(
      valueWithAttributeFallback(
        autoCreationDisabled,
        attributes,
        "auto_creation_disabled",
        "autoCreationDisabled",
        "AutoCreationDisabled",
      ),
    ),
    resultStateRecorded: normalizedNullableBoolean(
      valueWithAttributeFallback(
        resultStateRecorded,
        attributes,
        "result_state_recorded",
        "resultStateRecorded",
        "ResultStateRecorded",
      ),
    ),
    operation: normalizedNullableText(
      valueWithAttributeFallback(operation, attributes, "operation", "Operation"),
    ),
    serviceCodePresent: normalizedNullableBoolean(
      valueWithAttributeFallback(
        serviceCodePresent,
        attributes,
        "service_code_present",
        "serviceCodePresent",
        "ServiceCodePresent",
      ),
    ),
    serviceCodeFingerprint: normalizedNullableText(
      valueWithAttributeFallback(
        serviceCodeFingerprint,
        attributes,
        "service_code_fingerprint",
        "serviceCodeFingerprint",
        "ServiceCodeFingerprint",
      ),
    ),
    confirmationAttempt: normalizedNullableInteger(
      valueWithAttributeFallback(
        confirmationAttempt,
        attributes,
        "confirmation_attempt",
        "confirmationAttempt",
        "ConfirmationAttempt",
      ),
      { minimum: 1 },
    ),
    attributes,
  };
}

export function normalizeRuntimeLogPage(data = {}) {
  const rawItems = Array.isArray(data)
    ? data
    : Array.isArray(data?.items)
      ? data.items
      : [];
  const nextBeforeId = firstDefined(
    data,
    "next_before_id",
    "nextBeforeId",
    "NextBeforeID",
  );
  const hasMore = Boolean(
    firstDefined(data, "has_more", "hasMore", "HasMore"),
  );

  return {
    items: rawItems.map(normalizeRuntimeLog),
    hasMore,
    nextBeforeId: nextBeforeId ?? null,
  };
}

export function buildRuntimeLogQuery(options = {}) {
  const parameters = new URLSearchParams();
  const level = normalizeRuntimeLogLevel(options.level || "");
  const query = String(options.query || "").trim();
  const accountId = String(options.accountId ?? "").trim();
  const rawLimit = Number(options.limit);
  const limit = Number.isFinite(rawLimit)
    ? Math.min(200, Math.max(1, Math.trunc(rawLimit)))
    : 50;
  const beforeId = String(options.beforeId ?? "").trim();
  const syncRunId = String(options.syncRunId ?? "").trim();
  const autoCreateRunId = String(options.autoCreateRunId ?? "").trim();

  if (level && options.level) parameters.set("level", level);
  if (query) parameters.set("query", query);
  if (accountId) parameters.set("account_id", accountId);
  if (syncRunId) parameters.set("sync_run_id", syncRunId);
  if (autoCreateRunId) parameters.set("auto_create_run_id", autoCreateRunId);
  parameters.set("limit", String(limit));
  if (beforeId) parameters.set("before_id", beforeId);
  return parameters.toString();
}

export function runtimeLogSyncStageLabel(stage) {
  const normalized = normalizedToken(stage);
  if (!normalized) return "同步步骤";
  return SYNC_STAGE_LABELS[normalized] || normalized.replaceAll("_", " ");
}

export function runtimeLogAutoCreateStageLabel(stage) {
  const normalized = normalizedToken(stage);
  if (!normalized) return "自动创建步骤";
  return AUTO_CREATE_STAGE_LABELS[normalized] || normalized.replaceAll("_", " ");
}

export function runtimeLogSyncTriggerLabel(trigger) {
  const normalized = normalizedToken(trigger);
  if (!normalized) return "邮件同步";
  return SYNC_TRIGGER_LABELS[normalized] || normalized.replaceAll("_", " ");
}

export function runtimeLogLevelMeta(level) {
  switch (normalizeRuntimeLogLevel(level)) {
    case "error":
      return { label: "错误", type: "danger" };
    case "warn":
      return { label: "警告", type: "warning" };
    case "debug":
      return { label: "调试", type: "info" };
    case "info":
      return { label: "信息", type: "primary" };
    default:
      return { label: String(level || "未知").toUpperCase(), type: "info" };
  }
}

function runtimeLogIdentity(log) {
  if (log?.id !== undefined && log?.id !== null && String(log.id) !== "") {
    return `id:${log.id}`;
  }
  return [log?.time, log?.level, log?.source, log?.message].join("\u0000");
}

export function mergeRuntimeLogs(primary = [], secondary = []) {
  const seen = new Set();
  const merged = [];
  for (const log of [...primary, ...secondary]) {
    const identity = runtimeLogIdentity(log);
    if (seen.has(identity)) continue;
    seen.add(identity);
    merged.push(log);
  }
  return merged;
}

export function chronologicalRuntimeLogs(logs = []) {
  return mergeRuntimeLogs(logs, [])
    .map((log, index) => ({ log, index }))
    .sort((left, right) => {
      const leftTime = Date.parse(left.log?.time || "");
      const rightTime = Date.parse(right.log?.time || "");
      if (Number.isFinite(leftTime) && Number.isFinite(rightTime) && leftTime !== rightTime) {
        return leftTime - rightTime;
      }

      const leftID = Number(left.log?.id);
      const rightID = Number(right.log?.id);
      if (Number.isFinite(leftID) && Number.isFinite(rightID) && leftID !== rightID) {
        return leftID - rightID;
      }
      return left.index - right.index;
    })
    .map(({ log }) => log);
}

export function appendRuntimeLogPage(current = [], page = {}, maximum = 2000) {
  const rawMaximum = Number(maximum);
  const normalizedMaximum = Number.isFinite(rawMaximum)
    ? Math.max(1, Math.trunc(rawMaximum))
    : 2000;
  const items = mergeRuntimeLogs(current, page.items || []).slice(
    0,
    normalizedMaximum,
  );
  const reachedLimit = items.length >= normalizedMaximum;
  const nextBeforeId = reachedLimit ? null : page.nextBeforeId ?? null;

  return {
    items,
    hasMore: Boolean(!reachedLimit && page.hasMore && nextBeforeId != null),
    nextBeforeId,
  };
}

export function runtimeLogAttributesText(attributes) {
  const normalized = objectValue(attributes);
  return Object.keys(normalized).length
    ? JSON.stringify(normalized, null, 2)
    : "";
}

export function runtimeLogFlowContextText(log) {
  const attributes = objectValue(log?.attributes);
  const context = {};
  for (const [key, value] of Object.entries(attributes)) {
    const attributeName = normalizedAttributeName(attributeLeaf(key));
    if (!FLOW_ATTRIBUTE_NAMES.has(attributeName)) {
      context[key] = value;
    }
  }
  return runtimeLogAttributesText(context);
}
