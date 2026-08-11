import assert from "node:assert/strict";
import test from "node:test";

import {
  clearAliasAutoCreationKeys,
  deleteAlias,
  deleteAppleSession,
  getAliasAutoCreationKeys,
  getAutoCreateLogRun,
  getAccount,
  getAccountPage,
  getAccounts,
  getAliasPage,
  getAllAliases,
  getAliases,
  getAuditLogs,
  getRuntimeLogRun,
  getRuntimeLogs,
  loginAppleSession,
  normalizeAutoCreation,
  setAliasAutoCreation,
  syncAccount,
  syncAccountAliases,
  verifyAppleSession,
} from "../src/api/admin.js";

function jsonResponse(data, status = 200) {
  return new Response(JSON.stringify({ data }), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

test("account list normalizes sync progress and detailed error logs", async () => {
  globalThis.fetch = async () =>
    jsonResponse([
      {
        id: 12,
        email: "owner@icloud.com",
        last_sync_error: "同步失败",
        last_sync_error_log: "fetch IMAP mailbox increment: connection closed",
        sync_progress: {
          active: true,
          source: "automatic",
          stage: "fetching",
          percentage: 0,
          started_at: "2026-08-09T08:00:00Z",
          updated_at: "2026-08-09T08:00:01Z",
        },
      },
      { id: 13, email: "missing@icloud.com" },
      { id: 14, email: "idle@icloud.com", sync_progress: null },
    ]);

  const accounts = await getAccounts();

  assert.deepEqual(accounts[0].syncProgress, {
    active: true,
    source: "automatic",
    stage: "fetching",
    percentage: 0,
    startedAt: "2026-08-09T08:00:00Z",
    updatedAt: "2026-08-09T08:00:01Z",
  });
  assert.equal(
    accounts[0].lastSyncErrorLog,
    "fetch IMAP mailbox increment: connection closed",
  );
  assert.equal(accounts[1].syncProgress, null);
  assert.equal(accounts[1].lastSyncErrorLog, "");
  assert.equal(accounts[2].syncProgress, null);
});

test("account pages send server-side search and normalize pagination metadata", async () => {
  let request;
  globalThis.fetch = async (url, options) => {
    request = { url, options };
    return jsonResponse({
      items: [{ id: 12, email: "owner@icloud.com", alias_count: 7 }],
      pagination: { total: 73, limit: 50, offset: 50, has_more: false },
    });
  };

  const page = await getAccountPage({
    limit: 50,
    offset: 50,
    query: " owner ",
  });
  const url = new URL(request.url, "https://admin.invalid");

  assert.equal(url.pathname, "/admin/api/v1/accounts");
  assert.deepEqual(Object.fromEntries(url.searchParams), {
    limit: "50",
    offset: "50",
    query: "owner",
  });
  assert.equal(request.options.method, "GET");
  assert.deepEqual(
    {
      ids: page.items.map((account) => account.id),
      total: page.total,
      limit: page.limit,
      offset: page.offset,
      hasMore: page.hasMore,
    },
    { ids: [12], total: 73, limit: 50, offset: 50, hasMore: false },
  );
});

test("account detail accepts camelCase progress and falls back to the sync error", async () => {
  globalThis.fetch = async () =>
    jsonResponse({
      account: {
        id: 12,
        email: "owner@icloud.com",
        lastSyncError: "连接被远端关闭",
        syncProgress: {
          active: true,
          source: "manual",
          stage: "saving",
          percentage: 125,
          startedAt: "2026-08-09T08:00:00Z",
          updatedAt: "2026-08-09T08:00:02Z",
        },
      },
      aliases: [],
    });

  const detail = await getAccount(12);

  assert.deepEqual(detail.account.syncProgress, {
    active: true,
    source: "manual",
    stage: "saving",
    percentage: 100,
    startedAt: "2026-08-09T08:00:00Z",
    updatedAt: "2026-08-09T08:00:02Z",
  });
  assert.equal(detail.account.lastSyncErrorLog, "连接被远端关闭");
});

test("mail sync accepts PascalCase progress and normalizes percentage bounds", async () => {
  const progressCases = [
    {
      Percentage: -20,
      expectedPercentage: 0,
    },
    {
      Percentage: "unknown",
      expectedPercentage: null,
    },
  ];
  const responseQueue = [...progressCases];
  globalThis.fetch = async () => {
    const { expectedPercentage: _expectedPercentage, ...progress } =
      responseQueue.shift();
    return jsonResponse(
      {
        account: {
          ID: 12,
          Email: "owner@icloud.com",
          LastSyncError: "同步失败",
          LastSyncErrorLog: "完整错误日志",
          SyncProgress: {
            Active: true,
            Source: "manual",
            Stage: "connecting",
            StartedAt: "2026-08-09T08:00:00Z",
            UpdatedAt: "2026-08-09T08:00:03Z",
            ...progress,
          },
        },
        aliases: [],
        SyncPending: true,
      },
      202,
    );
  };

  for (const progressCase of progressCases) {
    const detail = await syncAccount(12, "csrf-token");
    assert.equal(
      detail.account.syncProgress.percentage,
      progressCase.expectedPercentage,
    );
    assert.equal(detail.account.lastSyncErrorLog, "完整错误日志");
    assert.equal(detail.syncPending, true);
  }
});

test("account detail includes the normalized Apple session", async () => {
  globalThis.fetch = async () =>
    jsonResponse({
      account: {
        id: 12,
        email: "owner@icloud.com",
        enabled: true,
        alias_count: 1,
      },
      aliases: [{ id: 8, address: "private@icloud.com", enabled: true }],
      apple_session: {
        status: "authenticated",
        apple_id: "owner@icloud.com",
        region: "cn",
        authenticated_at: "2026-08-07T08:00:00Z",
      },
    });

  const detail = await getAccount(12);

  assert.equal(detail.account.id, 12);
  assert.equal(detail.aliases[0].address, "private@icloud.com");
  assert.deepEqual(detail.appleSession, {
    status: "authenticated",
    appleId: "owner@icloud.com",
    region: "cn",
    authenticatedAt: "2026-08-07T08:00:00Z",
    expiresAt: null,
  });
});

test("account detail normalizes automatic alias creation state", async () => {
  globalThis.fetch = async () =>
    jsonResponse({
      account: { id: 12, email: "owner@icloud.com" },
      aliases: [],
      apple_session: null,
      auto_creation: {
        enabled: true,
        status: "scheduled",
        next_run_at: "2026-08-08T09:00:00Z",
        planned_at: "2026-08-08T09:00:00Z",
        planned_times: [
          "2026-08-08T09:00:00Z",
          "2026-08-08T09:15:00Z",
          "2026-08-08T09:35:00Z",
        ],
        last_attempted_at: "2026-08-08T08:00:00Z",
        last_created_at: "2026-08-08T07:00:00Z",
        last_alias_address: "new@icloud.com",
        last_error: "",
        pending_key_count: 5,
      },
    });

  const detail = await getAccount(12);

  assert.deepEqual(detail.autoCreation, {
    enabled: true,
    status: "scheduled",
    nextRunAt: "2026-08-08T09:00:00Z",
    plannedAt: "2026-08-08T09:00:00Z",
    plannedTimes: [
      "2026-08-08T09:00:00Z",
      "2026-08-08T09:15:00Z",
      "2026-08-08T09:35:00Z",
    ],
    lastAttemptedAt: "2026-08-08T08:00:00Z",
    lastCreatedAt: "2026-08-08T07:00:00Z",
    lastAliasAddress: "new@icloud.com",
    lastError: "",
    pendingKeyCount: 5,
  });

  assert.deepEqual(
    normalizeAutoCreation({
      Enabled: true,
      Status: "ready",
      NextRunAt: "2026-08-08T10:00:00Z",
      PlannedAt: "2026-08-08T10:00:00Z",
      PlannedTimes: [
        "2026-08-08T10:00:00Z",
        "2026-08-08T10:20:00Z",
      ],
      LastAttemptedAt: "2026-08-08T09:00:00Z",
      LastCreatedAt: "2026-08-08T08:00:00Z",
      LastAliasAddress: "pascal@icloud.com",
      LastError: "temporary failure",
      PendingKeyCount: -2,
    }),
    {
      enabled: true,
      status: "ready",
      nextRunAt: "2026-08-08T10:00:00Z",
      plannedAt: "2026-08-08T10:00:00Z",
      plannedTimes: [
        "2026-08-08T10:00:00Z",
        "2026-08-08T10:20:00Z",
      ],
      lastAttemptedAt: "2026-08-08T09:00:00Z",
      lastCreatedAt: "2026-08-08T08:00:00Z",
      lastAliasAddress: "pascal@icloud.com",
      lastError: "temporary failure",
      pendingKeyCount: 0,
    },
  );
});

test("automatic alias creation toggle uses an encoded account URL and CSRF", async () => {
  let request;
  globalThis.fetch = async (url, options) => {
    request = { url, options };
    return jsonResponse({
      auto_creation: {
        enabled: true,
        status: "scheduled",
        pending_key_count: 0,
      },
    });
  };

  const result = await setAliasAutoCreation("account/12", true, "csrf-token");

  assert.equal(
    request.url,
    "/admin/api/v1/accounts/account%2F12/aliases/auto-create",
  );
  assert.equal(request.options.method, "PUT");
  assert.equal(request.options.headers.get("X-CSRF-Token"), "csrf-token");
  assert.deepEqual(JSON.parse(request.options.body), { enabled: true });
  assert.deepEqual(result, {
    enabled: true,
    status: "scheduled",
    nextRunAt: null,
    plannedAt: null,
    plannedTimes: [],
    lastAttemptedAt: null,
    lastCreatedAt: null,
    lastAliasAddress: "",
    lastError: "",
    pendingKeyCount: 0,
  });
});

test("automatic alias creation key retrieval normalizes created entries", async () => {
  let request;
  globalThis.fetch = async (url, options) => {
    request = { url, options };
    return jsonResponse({
      created: [
        {
          alias: {
            id: 91,
            account_id: 12,
            account_email: "owner@icloud.com",
            address: "queued@icloud.com",
            api_key_prefix: "icm_queued",
            direct_link_path: "/api/v1/mail/recent?api_key=derived",
            enabled: true,
          },
          api_key: "icm_one-time-secret",
          mail_api_direct_link: "/api/v1/mail/recent?api_key=derived",
        },
      ],
    });
  };

  const result = await getAliasAutoCreationKeys("account/12");

  assert.equal(
    request.url,
    "/admin/api/v1/accounts/account%2F12/aliases/auto-create/keys",
  );
  assert.equal(request.options.method, "GET");
  assert.equal(request.options.headers.get("X-CSRF-Token"), null);
  assert.deepEqual(result.created[0], {
    alias: {
      id: 91,
      accountId: 12,
      accountEmail: "owner@icloud.com",
      address: "queued@icloud.com",
      label: "",
      apiKeyPrefix: "icm_queued",
      directLinkPath: "/api/v1/mail/recent?api_key=derived",
      enabled: true,
      lastSyncStatus: "pending",
      lastSyncError: "",
      lastSyncErrorLog: "",
      lastSyncedAt: null,
      lastAccessedAt: null,
      latestReceivedAt: null,
    },
    apiKey: "icm_one-time-secret",
    mailApiDirectLink: "/api/v1/mail/recent?api_key=derived",
  });
});

test("automatic alias creation key acknowledgement sends DELETE with IDs and CSRF", async () => {
  let request;
  globalThis.fetch = async (url, options) => {
    request = { url, options };
    return new Response(null, { status: 204 });
  };

  const result = await clearAliasAutoCreationKeys(
    "account/12",
    [91, 92],
    "csrf-token",
  );

  assert.equal(result, null);
  assert.equal(
    request.url,
    "/admin/api/v1/accounts/account%2F12/aliases/auto-create/keys",
  );
  assert.equal(request.options.method, "DELETE");
  assert.equal(request.options.headers.get("X-CSRF-Token"), "csrf-token");
  assert.deepEqual(JSON.parse(request.options.body), { alias_ids: [91, 92] });
});

test("alias deletion sends an authenticated DELETE without a request body", async () => {
  let request;
  globalThis.fetch = async (url, options) => {
    request = { url, options };
    return new Response(null, { status: 204 });
  };

  const result = await deleteAlias("alias/91", "csrf-token");

  assert.equal(result, null);
  assert.equal(request.url, "/admin/api/v1/aliases/alias%2F91");
  assert.equal(request.options.method, "DELETE");
  assert.equal(request.options.headers.get("X-CSRF-Token"), "csrf-token");
  assert.equal(request.options.body, undefined);
});

test("alias directory forwards the optional primary-account filter", async () => {
  const requests = [];
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    return jsonResponse([
      {
        id: 8,
        account_id: 12,
        account_email: "owner@icloud.com",
        address: "private@icloud.com",
        enabled: true,
        last_sync_error: "连接失败",
        last_sync_error_log: "fetch IMAP mailbox increment: connection closed",
      },
      {
        id: 9,
        account_id: 12,
        account_email: "owner@icloud.com",
        address: "fallback@icloud.com",
        enabled: true,
        last_sync_error: "仅有错误摘要",
      },
    ]);
  };

  const filtered = await getAliases(12);
  const all = await getAliases();

  assert.equal(requests[0].url, "/admin/api/v1/aliases?account_id=12");
  assert.equal(requests[1].url, "/admin/api/v1/aliases");
  assert.equal(filtered[0].accountId, 12);
  assert.equal(
    filtered[0].lastSyncErrorLog,
    "fetch IMAP mailbox increment: connection closed",
  );
  assert.equal(filtered[1].lastSyncErrorLog, "仅有错误摘要");
  assert.equal(all[0].address, "private@icloud.com");
});

test("alias pages apply the primary-account filter before server pagination", async () => {
  let request;
  globalThis.fetch = async (url, options) => {
    request = { url, options };
    return jsonResponse({
      items: [
        {
          id: 8,
          account_id: 12,
          address: "private@icloud.com",
          enabled: true,
        },
      ],
      pagination: { total: 81, limit: 50, offset: 50, has_more: false },
    });
  };

  const page = await getAliasPage(12, { limit: 50, offset: 50 });
  const url = new URL(request.url, "https://admin.invalid");

  assert.deepEqual(Object.fromEntries(url.searchParams), {
    limit: "50",
    offset: "50",
    account_id: "12",
  });
  assert.equal(page.items[0].accountId, 12);
  assert.deepEqual(
    { total: page.total, limit: page.limit, offset: page.offset, hasMore: page.hasMore },
    { total: 81, limit: 50, offset: 50, hasMore: false },
  );
});

test("full alias export follows every server page for the selected account", async () => {
  const requests = [];
  const pages = [
    {
      items: [{ id: 2, account_id: 12, address: "second@icloud.com" }],
      pagination: { total: 2, limit: 200, offset: 0, has_more: true },
    },
    {
      items: [{ id: 1, account_id: 12, address: "first@icloud.com" }],
      pagination: { total: 2, limit: 200, offset: 1, has_more: false },
    },
  ];
  globalThis.fetch = async (url) => {
    requests.push(new URL(url, "https://admin.invalid"));
    return jsonResponse(pages.shift());
  };

  const aliases = await getAllAliases(12);

  assert.deepEqual(aliases.map((alias) => alias.id), [2, 1]);
  assert.equal(requests.length, 2);
  assert.equal(requests[0].searchParams.get("offset"), "0");
  assert.equal(requests[1].searchParams.get("offset"), "1");
  for (const request of requests) {
    assert.equal(request.searchParams.get("limit"), "200");
    assert.equal(request.searchParams.get("account_id"), "12");
  }
});

test("audit pages preserve total, limit, offset, and has-more metadata", async () => {
  let request;
  globalThis.fetch = async (url, options) => {
    request = { url, options };
    return jsonResponse({
      items: [{ id: 31, action: "update", created_at: "2026-08-09T08:00:00Z" }],
      pagination: { total: 131, limit: 50, offset: 100, has_more: true },
    });
  };

  const page = await getAuditLogs({ limit: 50, offset: 100 });
  const url = new URL(request.url, "https://admin.invalid");

  assert.deepEqual(Object.fromEntries(url.searchParams), {
    limit: "50",
    offset: "100",
  });
  assert.equal(page.items[0].createdAt, "2026-08-09T08:00:00Z");
  assert.deepEqual(
    { total: page.total, limit: page.limit, offset: page.offset, hasMore: page.hasMore },
    { total: 131, limit: 50, offset: 100, hasMore: true },
  );
});

test("pending mail sync accepts HTTP 202 and exposes continuation state", async () => {
  let request;
  globalThis.fetch = async (url, options) => {
    request = { url, options };
    return jsonResponse(
      {
        account: {
          id: 12,
          email: "owner@icloud.com",
          enabled: true,
          last_sync_status: "pending",
        },
        aliases: [],
        apple_session: null,
        sync_pending: true,
      },
      202,
    );
  };

  const detail = await syncAccount(12, "csrf-token");

  assert.equal(request.url, "/admin/api/v1/accounts/12/sync");
  assert.equal(request.options.method, "POST");
  assert.equal(request.options.headers.get("X-CSRF-Token"), "csrf-token");
  assert.equal(detail.account.lastSyncStatus, "pending");
  assert.equal(detail.syncPending, true);
});

test("Apple session endpoints send credentials and verification only in request bodies", async () => {
  const requests = [];
  const responses = [
    jsonResponse({
      status: "verification_required",
      challenge_id: "challenge-123",
      apple_session: {
        status: "verification_required",
        apple_id: "owner@icloud.com",
        region: "global",
      },
    }),
    jsonResponse({
      status: "authenticated",
      apple_session: {
        status: "authenticated",
        apple_id: "owner@icloud.com",
        region: "global",
      },
    }),
    new Response(null, { status: 204 }),
  ];
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    return responses.shift();
  };

  const loginResult = await loginAppleSession(
    "account/12",
    {
      apple_id: "owner@icloud.com",
      password: "apple-password",
      region: "global",
    },
    "csrf-token",
  );
  const verifyResult = await verifyAppleSession(
    "account/12",
    { challenge_id: loginResult.challengeId, code: "123456" },
    "csrf-token",
  );
  await deleteAppleSession("account/12", "csrf-token");

  assert.equal(loginResult.status, "verification_required");
  assert.equal(verifyResult.status, "authenticated");
  assert.deepEqual(
    requests.map(({ url, options }) => ({
      url,
      method: options.method,
      csrf: options.headers.get("X-CSRF-Token"),
      body: options.body ? JSON.parse(options.body) : null,
    })),
    [
      {
        url: "/admin/api/v1/accounts/account%2F12/apple-auth",
        method: "POST",
        csrf: "csrf-token",
        body: {
          apple_id: "owner@icloud.com",
          password: "apple-password",
          region: "global",
        },
      },
      {
        url: "/admin/api/v1/accounts/account%2F12/apple-auth/verify",
        method: "POST",
        csrf: "csrf-token",
        body: { challenge_id: "challenge-123", code: "123456" },
      },
      {
        url: "/admin/api/v1/accounts/account%2F12/apple-auth",
        method: "DELETE",
        csrf: "csrf-token",
        body: null,
      },
    ],
  );
});

test("alias directory sync normalizes its summary and one-time keys", async () => {
  let request;
  globalThis.fetch = async (url, options) => {
    request = { url, options };
    return jsonResponse({
      account: {
        id: 12,
        email: "owner@icloud.com",
        enabled: true,
        alias_count: 2,
      },
      aliases: [
        { id: 8, address: "existing@icloud.com", enabled: true },
        { id: 9, address: "new@icloud.com", enabled: true },
      ],
      apple_session: {
        status: "authenticated",
        apple_id: "owner@icloud.com",
        region: "global",
      },
      summary: {
        total: 4,
        created_count: 1,
        existing_count: 1,
        inactive_count: 1,
        imported_disabled_count: 1,
        conflict_count: 1,
      },
      created: [
        {
          alias: {
            id: 9,
            address: "new@icloud.com",
            api_key_prefix: "icm_new",
            direct_link_path: "/api/v1/mail/recent/?api_key=derived",
            enabled: true,
          },
          api_key: "icm_one-time-secret",
          mail_api_direct_link: "/api/v1/mail/recent/?api_key=derived",
        },
      ],
    });
  };

  const result = await syncAccountAliases(12, "csrf-token");

  assert.equal(request.url, "/admin/api/v1/accounts/12/aliases/sync");
  assert.equal(request.options.method, "POST");
  assert.equal(request.options.headers.get("X-CSRF-Token"), "csrf-token");
  assert.deepEqual(result.summary, {
    total: 4,
    createdCount: 1,
    existingCount: 1,
    inactiveCount: 1,
    importedDisabledCount: 1,
    conflictCount: 1,
  });
  assert.deepEqual(result.created[0], {
    alias: {
      id: 9,
      accountId: undefined,
      accountEmail: "",
      address: "new@icloud.com",
      label: "",
      apiKeyPrefix: "icm_new",
      directLinkPath: "/api/v1/mail/recent/?api_key=derived",
      enabled: true,
      lastSyncStatus: "pending",
      lastSyncError: "",
      lastSyncErrorLog: "",
      lastSyncedAt: null,
      lastAccessedAt: null,
      latestReceivedAt: null,
    },
    apiKey: "icm_one-time-secret",
    mailApiDirectLink: "/api/v1/mail/recent/?api_key=derived",
  });
});

test("runtime log pages use offset filters and normalize pagination metadata", async () => {
  let request;
  globalThis.fetch = async (url, options) => {
    request = { url, options };
    return jsonResponse({
      items: [
        {
          id: 81,
          created_at: "2026-08-09T08:00:00Z",
          level: "ERROR",
          message: "主号同步失败",
          source: "syncer.manager",
          account_id: 12,
          request_id: "req-log-1",
          auto_create_run_id: "auto-run-1",
          attributes: {
            auto_create_stage: "failed",
            auto_create_percent: "100",
            auto_create_event: "run_failed",
            error: "connection closed",
            error_code: "APPLE_RATE_LIMITED",
            error_class: "apple_upstream",
            cause_category: "apple_upstream",
            error_context: "Apple 请求被限流",
            failed_stage: "reserving",
            failed_operation: "reserve_alias",
            http_status: "429",
            retryable: "true",
            elapsed_ms: "3821",
            schedule_action: "continue",
          },
        },
      ],
      pagination: { total: 481, limit: 50, offset: 100, has_more: true },
    });
  };

  const result = await getRuntimeLogs({
    level: "error",
    query: "同步失败",
    accountId: 12,
    autoCreateRunId: "auto-run-1",
    limit: 50,
    offset: 100,
  });
  const url = new URL(request.url, "https://admin.invalid");

  assert.equal(url.pathname, "/admin/api/v1/logs");
  assert.deepEqual(Object.fromEntries(url.searchParams), {
    level: "error",
    query: "同步失败",
    account_id: "12",
    auto_create_run_id: "auto-run-1",
    limit: "50",
    offset: "100",
  });
  assert.equal(request.options.method, "GET");
  assert.equal(result.items[0].time, "2026-08-09T08:00:00Z");
  assert.equal(result.items[0].level, "error");
  assert.equal(result.items[0].accountId, 12);
  assert.equal(result.items[0].requestId, "req-log-1");
  assert.equal(result.items[0].autoCreateRunId, "auto-run-1");
  assert.equal(result.items[0].autoCreateStage, "failed");
  assert.equal(result.items[0].autoCreatePercent, 100);
  assert.equal(result.items[0].autoCreateEvent, "run_failed");
  assert.equal(result.items[0].errorCode, "APPLE_RATE_LIMITED");
  assert.equal(result.items[0].errorClass, "apple_upstream");
  assert.equal(result.items[0].causeCategory, "apple_upstream");
  assert.equal(result.items[0].errorContext, "Apple 请求被限流");
  assert.equal(result.items[0].failedStage, "reserving");
  assert.equal(result.items[0].failedOperation, "reserve_alias");
  assert.equal(result.items[0].httpStatus, 429);
  assert.equal(result.items[0].retryable, true);
  assert.equal(result.items[0].elapsedMs, 3821);
  assert.equal(result.items[0].scheduleAction, "continue");
  assert.equal(result.hasMore, true);
  assert.equal(result.nextBeforeId, null);
  assert.equal(result.total, 481);
  assert.equal(result.limit, 50);
  assert.equal(result.offset, 100);
});

test("sync run logs follow every cursor without inheriting list filters", async () => {
  const requests = [];
  const pages = [
    {
      items: [
        { id: 6, created_at: "2026-08-09T08:00:06Z", attributes: { sync_run_id: "run-42", sync_event: "run_failed" } },
        { id: 5, created_at: "2026-08-09T08:00:05Z", attributes: { sync_run_id: "run-42", sync_event: "progress" } },
      ],
      has_more: true,
      next_before_id: 5,
    },
    {
      items: [
        { id: 4, created_at: "2026-08-09T08:00:04Z", attributes: { sync_run_id: "run-42", sync_event: "progress" } },
        { id: 3, created_at: "2026-08-09T08:00:03Z", attributes: { sync_run_id: "run-42", sync_event: "progress" } },
      ],
      has_more: true,
      next_before_id: 3,
    },
    {
      items: [
        { id: 2, created_at: "2026-08-09T08:00:02Z", attributes: { sync_run_id: "run-42", sync_event: "progress" } },
        { id: 1, created_at: "2026-08-09T08:00:01Z", attributes: { sync_run_id: "run-42", sync_event: "run_started" } },
      ],
      has_more: false,
      next_before_id: 0,
    },
  ];

  globalThis.fetch = async (url) => {
    requests.push(new URL(url, "https://admin.invalid"));
    return jsonResponse(pages.shift());
  };

  const result = await getRuntimeLogRun(" run-42 ", { accountId: 12 });

  assert.deepEqual(result.map((item) => item.id), [1, 2, 3, 4, 5, 6]);
  assert.equal(result[0].syncEvent, "run_started");
  assert.equal(result.at(-1).syncEvent, "run_failed");
  assert.equal(requests.length, 3);
  assert.deepEqual(Object.fromEntries(requests[0].searchParams), {
    account_id: "12",
    sync_run_id: "run-42",
    limit: "200",
  });
  assert.equal(requests[1].searchParams.get("before_id"), "5");
  assert.equal(requests[2].searchParams.get("before_id"), "3");
  for (const request of requests) {
    assert.equal(request.searchParams.has("level"), false);
    assert.equal(request.searchParams.has("query"), false);
  }
});

test("automatic creation run logs follow every cursor with their own run filter", async () => {
  const requests = [];
  const pages = [
    {
      items: [
        {
          id: 4,
          created_at: "2026-08-09T08:00:04Z",
          attributes: {
            auto_create_run_id: "auto-run-42",
            auto_create_event: "run_failed",
          },
        },
        {
          id: 3,
          created_at: "2026-08-09T08:00:03Z",
          attributes: {
            auto_create_run_id: "auto-run-42",
            auto_create_event: "stage_started",
          },
        },
      ],
      has_more: true,
      next_before_id: 3,
    },
    {
      items: [
        {
          id: 2,
          created_at: "2026-08-09T08:00:02Z",
          attributes: {
            auto_create_run_id: "auto-run-42",
            auto_create_event: "stage_started",
          },
        },
        {
          id: 1,
          created_at: "2026-08-09T08:00:01Z",
          attributes: {
            auto_create_run_id: "auto-run-42",
            auto_create_event: "run_started",
          },
        },
      ],
      has_more: false,
      next_before_id: 0,
    },
  ];

  globalThis.fetch = async (url) => {
    requests.push(new URL(url, "https://admin.invalid"));
    return jsonResponse(pages.shift());
  };

  const result = await getAutoCreateLogRun(" auto-run-42 ", {
    accountId: 12,
  });

  assert.deepEqual(result.map((item) => item.id), [1, 2, 3, 4]);
  assert.equal(result[0].autoCreateEvent, "run_started");
  assert.equal(result.at(-1).autoCreateEvent, "run_failed");
  assert.equal(requests.length, 2);
  assert.deepEqual(Object.fromEntries(requests[0].searchParams), {
    account_id: "12",
    auto_create_run_id: "auto-run-42",
    limit: "200",
  });
  assert.equal(requests[1].searchParams.get("before_id"), "3");
  for (const request of requests) {
    assert.equal(request.searchParams.has("sync_run_id"), false);
    assert.equal(request.searchParams.has("level"), false);
    assert.equal(request.searchParams.has("query"), false);
  }
});
