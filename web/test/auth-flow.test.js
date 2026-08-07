import assert from "node:assert/strict";
import test from "node:test";

import {
  AUTH_REQUIRED,
  SESSION_EXPIRED,
  buildLoginRedirect,
  loginNoticeMessage,
} from "../src/utils/authFlow.js";

test("fresh unauthenticated visits redirect to login without an expiry notice", () => {
  assert.deepEqual(buildLoginRedirect(AUTH_REQUIRED, "/admin/"), {
    name: "login",
    query: { redirect: "/admin/" },
  });
});

test("only expired sessions receive the expiry notice", () => {
  assert.deepEqual(buildLoginRedirect(SESSION_EXPIRED, "/admin/aliases"), {
    name: "login",
    query: {
      notice: "session_expired",
      redirect: "/admin/aliases",
    },
  });
  assert.deepEqual(buildLoginRedirect("NETWORK_ERROR", "/admin/audit"), {
    name: "login",
    query: {
      notice: "session_error",
      redirect: "/admin/audit",
    },
  });
});

test("starting a new login hides stale session notices", () => {
  assert.equal(
    loginNoticeMessage({ notice: "session_expired" }),
    "登录会话已过期，请重新登录。",
  );
  assert.equal(
    loginNoticeMessage({ notice: "session_expired", dismissed: true }),
    "",
  );
  assert.equal(
    loginNoticeMessage({ sessionCheckFailed: true, dismissed: true }),
    "",
  );
});
