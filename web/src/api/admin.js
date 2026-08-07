import { apiRequest } from "./client.js";

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

export function normalizeAccount(raw = {}) {
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
    lastSyncError:
      firstDefined(raw, "last_sync_error", "lastSyncError", "LastSyncError") ||
      "",
    lastSyncedAt:
      firstDefined(raw, "last_synced_at", "lastSyncedAt", "LastSyncedAt") ||
      null,
    aliasCount:
      Number(firstDefined(raw, "alias_count", "aliasCount", "AliasCount")) || 0,
  };
}

export function normalizeAlias(raw = {}) {
  return {
    id: firstDefined(raw, "id", "ID"),
    accountId: firstDefined(raw, "account_id", "accountId", "AccountID"),
    accountEmail:
      firstDefined(raw, "account_email", "accountEmail", "AccountEmail") || "",
    address: firstDefined(raw, "address", "Address") || "",
    label: firstDefined(raw, "label", "Label") || "",
    apiKeyPrefix:
      firstDefined(raw, "api_key_prefix", "apiKeyPrefix", "APIKeyPrefix") || "",
    enabled: Boolean(firstDefined(raw, "enabled", "Enabled")),
    lastSyncStatus:
      firstDefined(raw, "last_sync_status", "lastSyncStatus", "LastSyncStatus") ||
      "pending",
    lastSyncError:
      firstDefined(raw, "last_sync_error", "lastSyncError", "LastSyncError") ||
      "",
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

export async function getAccount(id) {
  const data = await apiRequest(`/accounts/${encodeURIComponent(id)}`);
  return normalizeAccountDetail(data);
}

function normalizeAccountDetail(data = {}) {
  const accountRaw = data?.account || data || {};
  return {
    account: normalizeAccount(accountRaw),
    aliases: listFrom(data, "aliases").map(normalizeAlias),
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

export async function getAliases() {
  const data = await apiRequest("/aliases");
  return listFrom(data, "aliases", "items").map(normalizeAlias);
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

export async function getAuditLogs() {
  const data = await apiRequest("/audit");
  return listFrom(data, "audit", "audit_logs", "logs", "items").map(
    normalizeAuditLog,
  );
}
