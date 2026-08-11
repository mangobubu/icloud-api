import { apiRequest } from "./client.js";
import {
  buildRuntimeLogQuery,
  chronologicalRuntimeLogs,
  mergeRuntimeLogs,
  normalizeRuntimeLogPage,
} from "../utils/runtimeLogs.js";

function firstDefined(object, ...keys) {
  for (const key of keys) {
    if (object && object[key] !== undefined) {
      return object[key];
    }
  }
  return undefined;
}

function listFrom(data, ...keys) {
  if (Array.isArray(data)) {
    return data;
  }
  for (const key of keys) {
    if (Array.isArray(data?.[key])) {
      return data[key];
    }
  }
  return [];
}

function integerAtLeast(value, minimum, fallback) {
  const number = Number(value);
  return Number.isFinite(number) && number >= minimum
    ? Math.trunc(number)
    : fallback;
}

function normalizeListPage(data, normalizer, keys = [], options = {}) {
  const rawItems = listFrom(data, ...keys);
  const pagination =
    data && !Array.isArray(data) && typeof data.pagination === "object"
      ? data.pagination || {}
      : {};
  const offset = integerAtLeast(
    firstDefined(pagination, "offset", "Offset") ??
      firstDefined(data, "offset", "Offset"),
    0,
    integerAtLeast(options.offset, 0, 0),
  );
  const limit = integerAtLeast(
    firstDefined(pagination, "limit", "Limit") ??
      firstDefined(data, "limit", "Limit"),
    1,
    integerAtLeast(options.limit, 1, rawItems.length || 50),
  );
  const explicitTotal = Number(
    firstDefined(pagination, "total", "Total") ??
      firstDefined(data, "total", "Total"),
  );
  const explicitHasMore =
    firstDefined(pagination, "has_more", "hasMore", "HasMore") ??
    firstDefined(data, "has_more", "hasMore", "HasMore");
  const total =
    Number.isFinite(explicitTotal) && explicitTotal >= 0
      ? Math.trunc(explicitTotal)
      : offset + rawItems.length + (explicitHasMore ? 1 : 0);

  return {
    items: rawItems.map(normalizer),
    total,
    limit,
    offset,
    hasMore:
      explicitHasMore === undefined
        ? offset + rawItems.length < total
        : Boolean(explicitHasMore),
  };
}

function listQuery(options = {}, extra = {}) {
  const parameters = new URLSearchParams();
  const limit = Math.min(200, integerAtLeast(options.limit, 1, 50));
  const offset = Math.min(1_000_000, integerAtLeast(options.offset, 0, 0));
  parameters.set("limit", String(limit));
  parameters.set("offset", String(offset));
  for (const [key, rawValue] of Object.entries(extra)) {
    const value = String(rawValue ?? "").trim();
    if (value) parameters.set(key, value);
  }
  return parameters.toString();
}

export function normalizeAccount(raw = {}) {
  const lastSyncError =
    firstDefined(raw, "last_sync_error", "lastSyncError", "LastSyncError") ||
    "";
  const lastSyncErrorLog = firstDefined(
    raw,
    "last_sync_error_log",
    "lastSyncErrorLog",
    "LastSyncErrorLog",
  );

  return {
    id: firstDefined(raw, "id", "ID"),
    name: firstDefined(raw, "name", "Name") || "",
    email: firstDefined(raw, "email", "Email") || "",
    imapHost:
      firstDefined(raw, "imap_host", "imapHost", "IMAPHost") ||
      "imap.mail.me.com",
    imapPort: firstDefined(raw, "imap_port", "imapPort", "IMAPPort") || 993,
    imapUsername:
      firstDefined(raw, "imap_username", "imapUsername", "IMAPUsername") ||
      "",
    enabled: Boolean(firstDefined(raw, "enabled", "Enabled")),
    lastSyncStatus:
      firstDefined(raw, "last_sync_status", "lastSyncStatus", "LastSyncStatus") ||
      "pending",
    lastSyncError,
    lastSyncErrorLog:
      lastSyncErrorLog === undefined ? lastSyncError : lastSyncErrorLog || "",
    lastSyncedAt:
      firstDefined(raw, "last_synced_at", "lastSyncedAt", "LastSyncedAt") ||
      null,
    syncProgress: normalizeSyncProgress(
      firstDefined(raw, "sync_progress", "syncProgress", "SyncProgress"),
    ),
    aliasCount:
      Number(firstDefined(raw, "alias_count", "aliasCount", "AliasCount")) || 0,
  };
}

function normalizeSyncProgress(raw) {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
    return null;
  }

  const percentageRaw = firstDefined(raw, "percentage", "Percentage");
  const percentageValue =
    typeof percentageRaw === "string" ? percentageRaw.trim() : percentageRaw;
  const percentageNumber =
    percentageValue === undefined ||
    percentageValue === null ||
    percentageValue === ""
      ? Number.NaN
      : Number(percentageValue);

  return {
    active: Boolean(firstDefined(raw, "active", "Active")),
    source: firstDefined(raw, "source", "Source") || "",
    stage: firstDefined(raw, "stage", "Stage") || "",
    percentage: Number.isFinite(percentageNumber)
      ? Math.min(100, Math.max(0, percentageNumber))
      : null,
    startedAt:
      firstDefined(raw, "started_at", "startedAt", "StartedAt") || null,
    updatedAt:
      firstDefined(raw, "updated_at", "updatedAt", "UpdatedAt") || null,
  };
}

export function normalizeAlias(raw = {}) {
  const lastSyncError =
    firstDefined(raw, "last_sync_error", "lastSyncError", "LastSyncError") ||
    "";
  const lastSyncErrorLog = firstDefined(
    raw,
    "last_sync_error_log",
    "lastSyncErrorLog",
    "LastSyncErrorLog",
  );

  return {
    id: firstDefined(raw, "id", "ID"),
    accountId: firstDefined(raw, "account_id", "accountId", "AccountID"),
    accountEmail:
      firstDefined(raw, "account_email", "accountEmail", "AccountEmail") || "",
    address: firstDefined(raw, "address", "Address") || "",
    label: firstDefined(raw, "label", "Label") || "",
    apiKeyPrefix:
      firstDefined(raw, "api_key_prefix", "apiKeyPrefix", "APIKeyPrefix") || "",
    directLinkPath:
      firstDefined(
        raw,
        "direct_link_path",
        "directLinkPath",
        "DirectLinkPath",
      ) || "",
    enabled: Boolean(firstDefined(raw, "enabled", "Enabled")),
    lastSyncStatus:
      firstDefined(raw, "last_sync_status", "lastSyncStatus", "LastSyncStatus") ||
      "pending",
    lastSyncError,
    lastSyncErrorLog:
      lastSyncErrorLog === undefined ? lastSyncError : lastSyncErrorLog || "",
    lastSyncedAt:
      firstDefined(raw, "last_synced_at", "lastSyncedAt", "LastSyncedAt") ||
      null,
    lastAccessedAt:
      firstDefined(raw, "last_accessed_at", "lastAccessedAt", "LastAccessedAt") ||
      null,
    latestReceivedAt:
      firstDefined(
        raw,
        "latest_received_at",
        "latestReceivedAt",
        "LatestReceivedAt",
      ) || null,
  };
}

export function normalizeAppleSession(raw) {
  if (!raw || typeof raw !== "object") {
    return null;
  }
  return {
    status:
      firstDefined(raw, "status", "Status", "auth_state", "authState", "AuthState") ||
      (firstDefined(raw, "authenticated", "Authenticated")
        ? "authenticated"
        : ""),
    appleId:
      firstDefined(raw, "apple_id", "appleId", "AppleID") || "",
    region: firstDefined(raw, "region", "Region") || "global",
    authenticatedAt:
      firstDefined(
        raw,
        "authenticated_at",
        "authenticatedAt",
        "AuthenticatedAt",
      ) || null,
    expiresAt:
      firstDefined(raw, "expires_at", "expiresAt", "ExpiresAt") || null,
  };
}

export function normalizeAutoCreation(raw = {}) {
  const value = raw && typeof raw === "object" ? raw : {};
  const plannedTimesRaw = firstDefined(
    value,
    "planned_times",
    "plannedTimes",
    "PlannedTimes",
  );
  const plannedTimes = Array.isArray(plannedTimesRaw)
    ? plannedTimesRaw
        .filter((planned) => planned !== null && planned !== undefined)
        .map((planned) => String(planned).trim())
        .filter(Boolean)
    : [];
  const plannedAt =
    firstDefined(value, "planned_at", "plannedAt", "PlannedAt") ||
    plannedTimes[0] ||
    null;
  const pendingRaw = firstDefined(
    value,
    "pending_key_count",
    "pending_auto_created_key_count",
    "pendingKeyCount",
    "PendingKeyCount",
  );
  const pendingNumber = Number(pendingRaw);
  const pendingKeyCount =
    Number.isFinite(pendingNumber) && pendingNumber >= 0
      ? Math.floor(pendingNumber)
      : 0;

  return {
    enabled: Boolean(firstDefined(value, "enabled", "Enabled")),
    status: firstDefined(value, "status", "Status") || "",
    nextRunAt:
      firstDefined(value, "next_run_at", "nextRunAt", "NextRunAt") || null,
    plannedAt,
    plannedTimes,
    lastAttemptedAt:
      firstDefined(
        value,
        "last_attempted_at",
        "lastAttemptedAt",
        "LastAttemptedAt",
      ) || null,
    lastCreatedAt:
      firstDefined(
        value,
        "last_created_at",
        "lastCreatedAt",
        "LastCreatedAt",
      ) || null,
    lastAliasAddress:
      firstDefined(
        value,
        "last_alias_address",
        "lastAliasAddress",
        "LastAliasAddress",
      ) || "",
    lastError:
      firstDefined(value, "last_error", "lastError", "LastError") || "",
    pendingKeyCount,
  };
}

export function normalizeAuditLog(raw = {}) {
  return {
    id: firstDefined(raw, "id", "ID"),
    username: firstDefined(raw, "username", "Username") || "",
    action: firstDefined(raw, "action", "Action") || "",
    resourceType:
      firstDefined(raw, "resource_type", "resourceType", "ResourceType") || "",
    resourceId:
      firstDefined(raw, "resource_id", "resourceId", "ResourceID") || "",
    result: firstDefined(raw, "result", "Result") || "",
    requestId:
      firstDefined(raw, "request_id", "requestId", "RequestID") || "",
    createdAt:
      firstDefined(raw, "created_at", "createdAt", "CreatedAt") || null,
  };
}

function authData(data = {}) {
  return {
    username: data.admin?.username || data.username || "",
    csrfToken: data.csrf_token || data.csrfToken || "",
    expiresAt: data.expires_at || data.expiresAt || null,
  };
}

export async function getLoginCsrf() {
  const data = await apiRequest("/auth/csrf", { handleUnauthorized: false });
  return data?.csrf_token || data?.csrfToken || "";
}

export async function login(username, password, csrfToken) {
  const data = await apiRequest("/auth/login", {
    method: "POST",
    body: { username, password },
    csrfToken,
    handleUnauthorized: false,
  });
  return authData(data);
}

export async function getSession() {
  return authData(
    await apiRequest("/auth/session", { handleUnauthorized: false }),
  );
}

export function logout(csrfToken) {
  return apiRequest("/auth/logout", { method: "POST", csrfToken });
}

export function updatePassword(payload, csrfToken) {
  return apiRequest("/auth/password", {
    method: "PUT",
    body: payload,
    csrfToken,
  });
}

export async function getAccounts() {
  const data = await apiRequest("/accounts");
  return listFrom(data, "accounts", "items").map(normalizeAccount);
}

export async function getAccountPage(options = {}) {
  const query = listQuery(options, { query: options.query });
  const data = await apiRequest(`/accounts?${query}`);
  return normalizeListPage(data, normalizeAccount, ["accounts", "items"], options);
}

export async function getAccount(id) {
  const data = await apiRequest(`/accounts/${encodeURIComponent(id)}`);
  return normalizeAccountDetail(data);
}

function normalizeAccountDetail(data = {}) {
  const accountRaw = data?.account || data || {};
  return {
    account: normalizeAccount(accountRaw),
    aliases: listFrom(data, "aliases").map(normalizeAlias),
    appleSession: normalizeAppleSession(
      firstDefined(data, "apple_session", "appleSession", "AppleSession"),
    ),
    autoCreation: normalizeAutoCreation(
      firstDefined(data, "auto_creation", "autoCreation", "AutoCreation"),
    ),
    syncPending: Boolean(
      firstDefined(data, "sync_pending", "syncPending", "SyncPending"),
    ),
  };
}

export async function createAccount(payload, csrfToken) {
  const data = await apiRequest("/accounts", {
    method: "POST",
    body: payload,
    csrfToken,
  });
  return normalizeAccount(data?.account || data || {});
}

export async function updateAccount(id, payload, csrfToken) {
  const data = await apiRequest(`/accounts/${encodeURIComponent(id)}`, {
    method: "PUT",
    body: payload,
    csrfToken,
  });
  return normalizeAccount(data?.account || data || {});
}

export function deleteAccount(id, csrfToken) {
  return apiRequest(`/accounts/${encodeURIComponent(id)}`, {
    method: "DELETE",
    csrfToken,
  });
}

export async function syncAccount(id, csrfToken) {
  const data = await apiRequest(`/accounts/${encodeURIComponent(id)}/sync`, {
    method: "POST",
    csrfToken,
  });
  return normalizeAccountDetail(data);
}

function appleSessionResult(data = {}) {
  const appleSession = normalizeAppleSession(
    firstDefined(data, "apple_session", "appleSession", "AppleSession"),
  );
  return {
    status:
      firstDefined(data, "status", "Status") || appleSession?.status || "",
    appleSession,
    flow: firstDefined(data, "flow", "Flow") || "",
    challengeId:
      firstDefined(data, "challenge_id", "challengeId", "ChallengeID") || "",
  };
}

export async function loginAppleSession(accountId, payload, csrfToken) {
  const data = await apiRequest(
    `/accounts/${encodeURIComponent(accountId)}/apple-auth`,
    {
      method: "POST",
      body: payload,
      csrfToken,
    },
  );
  return appleSessionResult(data);
}

export async function verifyAppleSession(accountId, payload, csrfToken) {
  const data = await apiRequest(
    `/accounts/${encodeURIComponent(accountId)}/apple-auth/verify`,
    {
      method: "POST",
      body: payload,
      csrfToken,
    },
  );
  return appleSessionResult(data);
}

export function deleteAppleSession(accountId, csrfToken) {
  return apiRequest(
    `/accounts/${encodeURIComponent(accountId)}/apple-auth`,
    {
      method: "DELETE",
      csrfToken,
    },
  );
}

function normalizeSyncSummary(raw = {}) {
  return {
    total:
      Number(firstDefined(raw, "total", "Total", "discovered", "Discovered")) ||
      0,
    createdCount:
      Number(
        firstDefined(
          raw,
          "created_count",
          "createdCount",
          "CreatedCount",
          "created",
          "Created",
        ),
      ) ||
      0,
    existingCount:
      Number(
        firstDefined(
          raw,
          "existing_count",
          "existingCount",
          "ExistingCount",
          "existing",
          "Existing",
        ),
      ) || 0,
    inactiveCount:
      Number(
        firstDefined(
          raw,
          "inactive_count",
          "inactiveCount",
          "InactiveCount",
          "inactive",
          "Inactive",
        ),
      ) || 0,
    importedDisabledCount:
      Number(
        firstDefined(
          raw,
          "imported_disabled_count",
          "importedDisabledCount",
          "ImportedDisabledCount",
          "imported_disabled",
          "ImportedDisabled",
          "filtered_out_count",
          "filteredOutCount",
          "FilteredOutCount",
        ),
      ) || 0,
    conflictCount:
      Number(
        firstDefined(
          raw,
          "conflict_count",
          "conflictCount",
          "ConflictCount",
          "conflicts",
          "Conflicts",
        ),
      ) || 0,
  };
}

function normalizeCreatedAlias(raw = {}) {
  const aliasRaw = firstDefined(raw, "alias", "Alias") || {};
  return {
    alias: normalizeAlias(
      typeof aliasRaw === "string" ? { address: aliasRaw } : aliasRaw,
    ),
    apiKey: firstDefined(raw, "api_key", "apiKey", "APIKey") || "",
    mailApiDirectLink:
      firstDefined(
        raw,
        "mail_api_direct_link",
        "mailApiDirectLink",
        "MailAPIDirectLink",
      ) || "",
  };
}

function normalizeAutoCreationResult(data) {
  const nested = firstDefined(
    data,
    "auto_creation",
    "autoCreation",
    "AutoCreation",
  );
  return normalizeAutoCreation(nested === undefined ? data : nested);
}

export async function setAliasAutoCreation(accountId, enabled, csrfToken) {
  const data = await apiRequest(
    `/accounts/${encodeURIComponent(accountId)}/aliases/auto-create`,
    {
      method: "PUT",
      body: { enabled },
      csrfToken,
    },
  );
  return normalizeAutoCreationResult(data);
}

export async function getAliasAutoCreationKeys(accountId) {
  const data = await apiRequest(
    `/accounts/${encodeURIComponent(accountId)}/aliases/auto-create/keys`,
  );
  return {
    created: listFrom(data, "created", "Created").map(normalizeCreatedAlias),
  };
}

export function clearAliasAutoCreationKeys(accountId, aliasIds, csrfToken) {
  const hasAliasIDs = Array.isArray(aliasIds);
  const requestCSRF = hasAliasIDs ? csrfToken : csrfToken ?? aliasIds;
  const options = {
    method: "DELETE",
    csrfToken: requestCSRF,
  };
  if (hasAliasIDs) {
    options.body = {
      alias_ids: aliasIds,
    };
  }
  return apiRequest(
    `/accounts/${encodeURIComponent(accountId)}/aliases/auto-create/keys`,
    options,
  );
}

export async function syncAccountAliases(accountId, csrfToken) {
  const data =
    (await apiRequest(
      `/accounts/${encodeURIComponent(accountId)}/aliases/sync`,
      {
        method: "POST",
        csrfToken,
      },
    )) || {};
  return {
    ...normalizeAccountDetail(data),
    summary: normalizeSyncSummary(data.summary),
    created: listFrom(data, "created").map(normalizeCreatedAlias),
  };
}

function aliasMutationResult(data = {}) {
  return {
    alias: normalizeAlias(data.alias || data),
    apiKey: data.api_key || data.apiKey || "",
  };
}

export async function createAlias(accountId, payload, csrfToken) {
  return aliasMutationResult(
    await apiRequest(`/accounts/${encodeURIComponent(accountId)}/aliases`, {
      method: "POST",
      body: payload,
      csrfToken,
    }),
  );
}

export async function getAliases(accountId = "") {
  const normalizedAccountId = String(accountId ?? "").trim();
  const query = normalizedAccountId
    ? `?account_id=${encodeURIComponent(normalizedAccountId)}`
    : "";
  const data = await apiRequest(`/aliases${query}`);
  return listFrom(data, "aliases", "items").map(normalizeAlias);
}

export async function getAliasPage(accountId = "", options = {}) {
  const query = listQuery(options, { account_id: accountId });
  const data = await apiRequest(`/aliases?${query}`);
  return normalizeListPage(data, normalizeAlias, ["aliases", "items"], options);
}

export async function getAllAliases(accountId = "") {
  const pageSize = 200;
  const aliases = [];
  let offset = 0;

  while (true) {
    const page = await getAliasPage(accountId, { limit: pageSize, offset });
    aliases.push(...page.items);
    offset += page.items.length;
    if (!page.hasMore || page.items.length === 0 || offset >= page.total) break;
  }
  return aliases;
}

export async function rotateAlias(id, csrfToken) {
  return aliasMutationResult(
    await apiRequest(`/aliases/${encodeURIComponent(id)}/rotate-key`, {
      method: "POST",
      csrfToken,
    }),
  );
}

export async function setAliasEnabled(id, enabled, csrfToken) {
  const data = await apiRequest(`/aliases/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: { enabled },
    csrfToken,
  });
  return normalizeAlias(data?.alias || data || {});
}

export function deleteAlias(id, csrfToken) {
  return apiRequest(`/aliases/${encodeURIComponent(id)}`, {
    method: "DELETE",
    csrfToken,
  });
}

export async function getAuditLogs(options = {}) {
  const query = listQuery(options);
  const data = await apiRequest(`/audit?${query}`);
  return normalizeListPage(
    data,
    normalizeAuditLog,
    ["audit", "audit_logs", "logs", "items"],
    options,
  );
}

export async function getRuntimeLogs(options = {}) {
  const query = buildRuntimeLogQuery(options);
  const data = await apiRequest(`/logs${query ? `?${query}` : ""}`, {
    signal: options.signal,
  });
  return normalizeRuntimeLogPage(data || {});
}

async function getRuntimeLogFlow(runFilter, options = {}) {
  if (!Object.values(runFilter).some(Boolean)) return [];
  const maximum = 2000;
  const seenCursors = new Set();
  let beforeId = "";
  let items = [];

  while (items.length < maximum) {
    const page = await getRuntimeLogs({
      accountId: options.accountId,
      beforeId,
      limit: 200,
      signal: options.signal,
      ...runFilter,
    });
    items = mergeRuntimeLogs(items, page.items).slice(0, maximum);

    const nextCursor = String(page.nextBeforeId ?? "").trim();
    if (!page.hasMore || !nextCursor || seenCursors.has(nextCursor)) break;
    seenCursors.add(nextCursor);
    beforeId = nextCursor;
  }

  return chronologicalRuntimeLogs(items);
}

export function getRuntimeLogRun(syncRunId, options = {}) {
  const normalizedRunId = String(syncRunId ?? "").trim();
  return getRuntimeLogFlow({ syncRunId: normalizedRunId }, options);
}

export function getAutoCreateLogRun(autoCreateRunId, options = {}) {
  const normalizedRunId = String(autoCreateRunId ?? "").trim();
  return getRuntimeLogFlow({ autoCreateRunId: normalizedRunId }, options);
}
