import assert from "node:assert/strict";
import test from "node:test";

import {
  ApiError,
  apiRequest,
  setUnauthorizedHandler,
} from "../src/api/client.js";

function errorResponse(status, code) {
  return new Response(
    JSON.stringify({
      error: {
        code,
        message: "test error",
        request_id: "request-test-id",
      },
    }),
    {
      status,
      headers: { "Content-Type": "application/json" },
    },
  );
}

test("current-password errors do not invalidate the session", async () => {
  let invalidations = 0;
  setUnauthorizedHandler(() => {
    invalidations += 1;
  });
  globalThis.fetch = async () =>
    errorResponse(401, "CURRENT_PASSWORD_INVALID");

  await assert.rejects(
    apiRequest("/auth/password", {
      method: "PUT",
      body: {},
      csrfToken: "csrf-test-token",
    }),
    (error) =>
      error instanceof ApiError &&
      error.code === "CURRENT_PASSWORD_INVALID" &&
      error.requestId === "request-test-id",
  );
  assert.equal(invalidations, 0);
});

test("expired sessions invoke the global unauthorized handler", async () => {
  let invalidations = 0;
  setUnauthorizedHandler(() => {
    invalidations += 1;
  });
  globalThis.fetch = async () => errorResponse(401, "SESSION_EXPIRED");

  await assert.rejects(apiRequest("/accounts"), {
    name: "ApiError",
    code: "SESSION_EXPIRED",
  });
  assert.equal(invalidations, 1);
  setUnauthorizedHandler(null);
});

test("HTML gateway timeouts are normalized without exposing the proxy page", async () => {
  const proxyPage =
    "<!doctype html><html><body><h1>504 Gateway Time-out</h1></body></html>";
  globalThis.fetch = async () =>
    new Response(proxyPage, {
      status: 504,
      headers: { "Content-Type": "text/html" },
    });

  await assert.rejects(
    apiRequest("/accounts/12/sync", { method: "POST" }),
    (error) => {
      assert.ok(error instanceof ApiError);
      assert.equal(error.status, 504);
      assert.equal(error.code, "GATEWAY_TIMEOUT");
      assert.equal(error.message, "网关等待服务响应超时，请稍后重试。");
      assert.equal(error.message.includes("<html>"), false);
      return true;
    },
  );
});
