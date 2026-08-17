import { apiRequest } from "./client.js";
import {
  buildRuntimeLogQuery,
  chronologicalRuntimeLogs,
  mergeRuntimeLogs,
  normalizeRuntimeLogPage,
} from "../utils/runtimeLogs.js";
import {
  DEFAULT_PAGE_SIZE,
  MAX_PAGE_SIZE,
} from "../utils/pagination.js";
import { normalizeIMAPEndpoint } from "../utils/imap.js";

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
    integerAtLeast(options.limit, 1, rawItems.length || DEFAULT_PAGE_SIZE),
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
  const limit = Math.min(
    MAX_PAGE_SIZE,
    integerAtLeast(options.limit, 1, DEFAULT_PAGE_SIZE),
  );
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

  const imapEndpoint = normalizeIMAPEndpoint(
    firstDefined(raw, "imap_host", "imapHost", "IMAPHost"),
    firstDefined(raw, "imap_port", "imapPort", "IMAPPort"),
  );

  return {
    id: firstDefined(raw, "id", "ID"),
    name: firstDefined(raw, "name", "Name") || "",
    email: firstDefined(raw, "email", "Email") || "",
    mailboxType:
      firstDefined(
        raw,
        "mailbox_type",
        "mailboxType",
        "account_type",
        "accountType",
        "provider",
        "Provider",
        "MailboxType",
      ) || "icloud",
    provider:
      firstDefined(
        raw,
        "provider",
        "Provider",
        "mailbox_type",
        "mailboxType",
      ) || "icloud",
    emailSuffix:
      firstDefined(raw, "email_suffix", "emailSuffix", "EmailSuffix") || "",
    imapHost: imapEndpoint.host,
    imapPort: imapEndpoint.port,
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
  const groupIDRaw = firstDefined(raw, "group_id", "groupId", "GroupID");
  const groupIDNumber = Number(groupIDRaw);

  return {
    id: firstDefined(raw, "id", "ID"),
    accountId: firstDefined(raw, "account_id", "accountId", "AccountID"),
    accountEmail:
      firstDefined(raw, "account_email", "accountEmail", "AccountEmail") || "",
    address: firstDefined(raw, "address", "Address") || "",
    label: firstDefined(raw, "label", "Label") || "",
    groupId:
      groupIDRaw === null || groupIDRaw === undefined || !Number.isFinite(groupIDNumber) || groupIDNumber < 1
        ? null
        : Math.trunc(groupIDNumber),
    groupName:
      firstDefined(raw, "group_name", "groupName", "GroupName") || "",
    apiKey: firstDefined(raw, "api_key", "apiKey", "APIKey") || "",
    apiKeyPrefix:
      firstDefined(raw, "api_key_prefix", "apiKeyPrefix", "APIKeyPrefix") ||
      "",
    imapPassword:
      firstDefined(raw, "imap_password", "imapPassword", "IMAPPassword") || "",
    clientId: firstDefined(raw, "client_id", "clientId", "ClientID") || "",
    refreshToken:
      firstDefined(raw, "refresh_token", "refreshToken", "RefreshToken") || "",
    otpUrlPath:
      firstDefined(
        raw,
        "otp_url_path",
        "otpUrlPath",
        "OTPURLPath",
      ) || "",
    directLinkPath:
      firstDefined(
        raw,
        "direct_link_path",
        "directLinkPath",
        "DirectLinkPath",
        "legacy_direct_link_path",
        "legacyDirectLinkPath",
        "LegacyDirectLink",
      ) || "",
    legacyDirectLinkPath:
      firstDefined(
        raw,
        "legacy_direct_link_path",
        "legacyDirectLinkPath",
        "LegacyDirectLink",
        "direct_link_path",
        "directLinkPath",
        "DirectLinkPath",
      ) || "",
    credentialMode:
      firstDefined(raw, "credential_mode", "credentialMode", "CredentialMode") ||
      "",
    credentialVersion:
      Number(
        firstDefined(
          raw,
          "credential_version",
          "credentialVersion",
          "CredentialVersion",
        ),
      ) || 0,
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

export function getAccounts(options = {}) {
  return getAllAccounts(options);
}

export async function getAccountPage(options = {}) {
  const query = listQuery(options, { query: options.query });
  const data = await apiRequest(`/accounts?${query}`, {
    signal: options.signal,
  });
  return normalizeListPage(data, normalizeAccount, ["accounts", "items"], options);
}

async function collectOffsetPages(fetchPage, options = {}) {
  const items = [];
  let offset = 0;
  let total = 0;
  const maximumPages = 10000;

  for (let pageNumber = 0; pageNumber < maximumPages; pageNumber += 1) {
    const page = await fetchPage({
      ...options,
      limit: MAX_PAGE_SIZE,
      offset,
    });
    const pageItems = Array.isArray(page?.items) ? page.items : [];
    items.push(...pageItems);
    const reportedTotal = Number(page?.total);
    if (Number.isFinite(reportedTotal) && reportedTotal >= 0) {
      total = Math.trunc(reportedTotal);
    }

    const pageOffset = Number(page?.offset);
    const nextOffset =
      Number.isFinite(pageOffset) && pageOffset >= 0
        ? Math.trunc(pageOffset) + pageItems.length
        : offset + pageItems.length;
    if (
      !page?.hasMore ||
      pageItems.length === 0 ||
      nextOffset <= offset ||
      (total > 0 && nextOffset >= total)
    ) {
      break;
    }
    offset = nextOffset;
  }

  return items;
}

export function getAllAccounts(options = {}) {
  return collectOffsetPages(getAccountPage, options);
}

export async function getMailGroups() {
  const data = await apiRequest("/groups");
  return listFrom(data, "groups", "items").map(normalizeMailGroup);
}

export async function createMailGroup(name, csrfToken) {
  const data = await apiRequest("/groups", {
    method: "POST",
    body: { name },
    csrfToken,
  });
  return normalizeMailGroup(data?.group || data || {});
}

export async function updateMailGroup(id, name, csrfToken) {
  const data = await apiRequest(`/groups/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: { name },
    csrfToken,
  });
  return normalizeMailGroup(data?.group || data || {});
}

export function deleteMailGroup(id, csrfToken) {
  return apiRequest(`/groups/${encodeURIComponent(id)}`, {
    method: "DELETE",
    csrfToken,
  });
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

export function normalizeMailGroup(raw = {}) {
  return {
    id: firstDefined(raw, "id", "ID"),
    name: firstDefined(raw, "name", "Name") || "",
    aliasCount:
      Number(firstDefined(raw, "alias_count", "aliasCount", "AliasCount")) ||
      0,
    createdAt:
      firstDefined(raw, "created_at", "createdAt", "CreatedAt") || null,
    updatedAt:
      firstDefined(raw, "updated_at", "updatedAt", "UpdatedAt") || null,
  };
}

function normalizeRandomAliasResult(data = {}) {
  const rawCreated = listFrom(data, "created", "items");
  const rawAliases = listFrom(data, "aliases");
  const created = rawCreated.map((item) => {
    const rawAlias = firstDefined(item, "alias", "Alias") || item;
    const alias = normalizeAlias(
      typeof rawAlias === "string" ? { address: rawAlias } : rawAlias,
    );
    return {
      alias,
      apiKey:
        firstDefined(item, "api_key", "apiKey", "APIKey") || alias.apiKey,
      otpUrlPath:
        firstDefined(
          item,
          "otp_url_path",
          "otpUrlPath",
          "OTPURLPath",
          "mail_api_direct_link",
          "mailApiDirectLink",
          "MailAPIDirectLink",
        ) || alias.otpUrlPath || alias.directLinkPath,
    };
  });
  return {
    created,
    aliases: (rawAliases.length ? rawAliases : created.map((item) => item.alias)).map(
      normalizeAlias,
    ),
    count:
      Number(firstDefined(data, "count", "Count")) || created.length,
  };
}

export async function createRandomAliases(accountId, payload, csrfToken) {
  const data = await apiRequest(
    `/accounts/${encodeURIComponent(accountId)}/aliases/random`,
    {
      method: "POST",
      body: payload,
      csrfToken,
    },
  );
  return normalizeRandomAliasResult(data?.data || data || {});
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
  const alias = normalizeAlias(
    typeof aliasRaw === "string" ? { address: aliasRaw } : aliasRaw,
  );
  return {
    alias,
    apiKey:
      firstDefined(raw, "api_key", "apiKey", "APIKey") || alias.apiKey,
    otpUrlPath:
      firstDefined(
        raw,
        "otp_url_path",
        "otpUrlPath",
        "OTPURLPath",
        "mail_api_direct_link",
        "mailApiDirectLink",
        "MailAPIDirectLink",
      ) || alias.otpUrlPath || alias.directLinkPath,
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
  const alias = normalizeAlias(data.alias || data);
  return {
    alias,
    apiKey:
      firstDefined(data, "api_key", "apiKey", "APIKey") || alias.apiKey,
    otpUrlPath:
      firstDefined(
        data,
        "otp_url_path",
        "otpUrlPath",
        "OTPURLPath",
        "mail_api_direct_link",
        "mailApiDirectLink",
        "MailAPIDirectLink",
      ) || alias.otpUrlPath || alias.directLinkPath,
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

export function getAliases(accountId = "", options = {}) {
  return getAllAliases(accountId, options);
}

export async function getAliasPage(accountId = "", options = {}) {
  const query = listQuery(options, {
    account_id: accountId,
    group_id: options.groupId,
    query: options.query,
  });
  const data = await apiRequest(`/aliases?${query}`, {
    signal: options.signal,
  });
  return normalizeListPage(data, normalizeAlias, ["aliases", "items"], options);
}

export async function moveAliasToGroup(id, groupId, csrfToken) {
  const data = await apiRequest(`/aliases/${encodeURIComponent(id)}/group`, {
    method: "PATCH",
    body: { group_id: groupId == null ? null : Number(groupId) },
    csrfToken,
  });
  return normalizeAlias(data?.alias || data || {});
}

export function moveAliasesToGroup(ids, groupId, csrfToken) {
  return apiRequest("/aliases/group", {
    method: "PATCH",
    body: {
      alias_ids: ids,
      group_id: groupId == null ? null : Number(groupId),
    },
    csrfToken,
  });
}

export function getAllAliases(accountId = "", options = {}) {
  return collectOffsetPages(
    (pageOptions) => getAliasPage(accountId, pageOptions),
    options,
  );
}

export async function rotateAlias(id, csrfToken, credentialMode = "") {
  const operation =
    credentialMode === "v2" ? "rotate-credentials" : "rotate-key";
  return aliasMutationResult(
    await apiRequest(`/aliases/${encodeURIComponent(id)}/${operation}`, {
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

export async function updateAliasGroup(id, groupId, csrfToken) {
  const data = await apiRequest(`/aliases/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: { group_id: groupId == null ? null : Number(groupId) },
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
  const data = await apiRequest(`/audit?${query}`, {
    signal: options.signal,
  });
  return normalizeListPage(
    data,
    normalizeAuditLog,
    ["audit", "audit_logs", "logs", "items"],
    options,
  );
}

export function getAllAuditLogs(options = {}) {
  return collectOffsetPages(getAuditLogs, options);
}

export async function getRuntimeLogs(options = {}) {
  const query = buildRuntimeLogQuery(options);
  const data = await apiRequest(`/logs${query ? `?${query}` : ""}`, {
    signal: options.signal,
  });
  return normalizeRuntimeLogPage(data || {});
}

export function getAllRuntimeLogs(options = {}) {
  return collectOffsetPages(
    (pageOptions) => getRuntimeLogs({ ...options, ...pageOptions }),
    options,
  );
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
      limit: MAX_PAGE_SIZE,
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
