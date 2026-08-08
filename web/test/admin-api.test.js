import assert from "node:assert/strict";
import test from "node:test";

import {
  deleteAppleSession,
  getAccount,
  loginAppleSession,
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
      lastSyncedAt: null,
      lastAccessedAt: null,
      latestReceivedAt: null,
    },
    apiKey: "icm_one-time-secret",
    mailApiDirectLink: "/api/v1/mail/recent/?api_key=derived",
  });
});
