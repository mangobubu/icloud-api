const LEVEL_ALIASES = {
  warning: "warn",
  fatal: "error",
  trace: "debug",
};

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

  if (level && options.level) parameters.set("level", level);
  if (query) parameters.set("query", query);
  if (accountId) parameters.set("account_id", accountId);
  parameters.set("limit", String(limit));
  if (beforeId) parameters.set("before_id", beforeId);
  return parameters.toString();
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
