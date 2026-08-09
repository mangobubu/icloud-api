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

const SYNC_TRIGGER_LABELS = Object.freeze({
  manual: "手动同步",
  automatic: "自动同步",
});

const SYNC_FLOW_ATTRIBUTE_NAMES = new Set([
  "account_id",
  "request_id",
  "sync_run_id",
  "sync_batch",
  "sync_stage",
  "sync_percent",
  "sync_event",
  "trigger",
  "error_context",
  "error",
  "error_detail",
  "failed_stage",
  "failed_operation",
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

function attributeValue(attributes, ...names) {
  for (const name of names) {
    const exact = firstDefined(attributes, name);
    if (exact !== undefined) return exact;

    const normalizedName = String(name).toLowerCase();
    const suffix = `.${normalizedName}`;
    const matchingKey = Object.keys(attributes).find((key) => {
      const normalizedKey = String(key).toLowerCase();
      return normalizedKey === normalizedName || normalizedKey.endsWith(suffix);
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
      accountId ??
      firstDefined(attributes, "account_id", "accountId", "AccountID") ??
      null,
    requestId: String(
      requestId ??
        firstDefined(attributes, "request_id", "requestId", "RequestID") ??
        "",
    ),
    syncRunId: String(
      syncRunId ??
        attributeValue(attributes, "sync_run_id", "syncRunId", "SyncRunID") ??
        "",
    ),
    syncStage: normalizedToken(
      syncStage ??
        attributeValue(attributes, "sync_stage", "syncStage", "SyncStage"),
    ),
    syncBatch: normalizedNullableNumber(
      syncBatch ??
        attributeValue(attributes, "sync_batch", "syncBatch", "SyncBatch"),
      { minimum: 1 },
    ),
    syncPercent: normalizedNullableNumber(
      syncPercent ??
        attributeValue(
          attributes,
          "sync_percent",
          "syncPercent",
          "SyncPercent",
        ),
      { minimum: 0, maximum: 100 },
    ),
    syncEvent: normalizedToken(
      syncEvent ??
        attributeValue(attributes, "sync_event", "syncEvent", "SyncEvent"),
    ),
    syncTrigger: normalizedToken(
      syncTrigger ?? attributeValue(attributes, "trigger", "Trigger"),
    ),
    errorContext: String(
      errorContext ??
        attributeValue(
          attributes,
          "error_context",
          "errorContext",
          "ErrorContext",
        ) ??
        "",
    ),
    failedStage: normalizedToken(
      failedStage ??
        attributeValue(attributes, "failed_stage", "failedStage", "FailedStage"),
    ),
    errorDetail: String(
      errorDetail ??
        attributeValue(
          attributes,
          "error",
          "Error",
          "error_detail",
          "errorDetail",
          "ErrorDetail",
        ) ??
        errorContext ??
        attributeValue(
          attributes,
          "error_context",
          "errorContext",
          "ErrorContext",
        ) ??
        "",
    ),
    failedOperation: normalizedToken(
      failedOperation ??
        attributeValue(
          attributes,
          "failed_operation",
          "failedOperation",
          "FailedOperation",
        ),
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

  if (level && options.level) parameters.set("level", level);
  if (query) parameters.set("query", query);
  if (accountId) parameters.set("account_id", accountId);
  if (syncRunId) parameters.set("sync_run_id", syncRunId);
  parameters.set("limit", String(limit));
  if (beforeId) parameters.set("before_id", beforeId);
  return parameters.toString();
}

export function runtimeLogSyncStageLabel(stage) {
  const normalized = normalizedToken(stage);
  if (!normalized) return "同步步骤";
  return SYNC_STAGE_LABELS[normalized] || normalized.replaceAll("_", " ");
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
    const normalizedKey = String(key).toLowerCase();
    const attributeName = normalizedKey.split(".").at(-1);
    if (!SYNC_FLOW_ATTRIBUTE_NAMES.has(attributeName)) {
      context[key] = value;
    }
  }
  return runtimeLogAttributesText(context);
}
