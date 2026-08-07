import assert from "node:assert/strict";
import test from "node:test";

import { useAuth } from "../src/stores/auth.js";

function jsonResponse(status, payload) {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function apiError(status, code) {
  return jsonResponse(status, {
    error: { code, message: "test error", request_id: "request-test-id" },
  });
}

const originalFetch = globalThis.fetch;

test.after(() => {
  globalThis.fetch = originalFetch;
});

test("session checks retain unauthenticated and expired reasons without refetching", async () => {
  const auth = useAuth();

  for (const code of ["AUTH_REQUIRED", "SESSION_EXPIRED"]) {
    let requests = 0;
    auth.clearSession({ checked: false });
    globalThis.fetch = async () => {
      requests += 1;
      return apiError(401, code);
    };

    assert.equal(await auth.ensureSession(), false);
    assert.equal(auth.state.lastSessionErrorCode, code);
    assert.equal(await auth.ensureSession(), false);
    assert.equal(requests, 1);
  }
});

test("credential failures preserve the login CSRF token for a valid retry", async () => {
  const auth = useAuth();
  const loginToken = "login-csrf-token-for-retry";
  const loginRequests = [];
  auth.clearSession({ checked: false });

  globalThis.fetch = async (url, options = {}) => {
    if (String(url).endsWith("/auth/csrf")) {
      return jsonResponse(200, { data: { csrf_token: loginToken } });
    }
    if (String(url).endsWith("/auth/login")) {
      loginRequests.push({ url, options });
      if (loginRequests.length === 1) {
        return apiError(401, "INVALID_CREDENTIALS");
      }
      return jsonResponse(200, {
        data: {
          admin: { username: "admin" },
          csrf_token: "authenticated-session-csrf",
          expires_at: "2026-08-07T12:00:00Z",
        },
      });
    }
    throw new Error(`unexpected request: ${url}`);
  };

  await auth.prepareLogin();
  await assert.rejects(auth.login("admin", "wrong-password"), {
    name: "ApiError",
    code: "INVALID_CREDENTIALS",
  });
  assert.equal(auth.state.csrfToken, loginToken);

  await auth.login("admin", "correct-password");
  assert.equal(auth.state.username, "admin");
  assert.equal(auth.state.csrfToken, "authenticated-session-csrf");
  assert.equal(auth.state.lastSessionErrorCode, "");

  assert.equal(loginRequests.length, 2);
  for (const { options } of loginRequests) {
    assert.equal(options.credentials, "same-origin");
    assert.equal(options.headers.get("X-CSRF-Token"), loginToken);
  }
  assert.deepEqual(JSON.parse(loginRequests[0].options.body), {
    username: "admin",
    password: "wrong-password",
  });
  assert.deepEqual(JSON.parse(loginRequests[1].options.body), {
    username: "admin",
    password: "correct-password",
  });

  auth.clearSession({ checked: false });
});
