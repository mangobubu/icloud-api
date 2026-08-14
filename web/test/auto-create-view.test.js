import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const viewPath = new URL("../src/views/AccountDetailView.vue", import.meta.url);

function functionBody(source, signature) {
  const start = source.indexOf(signature);
  assert.notEqual(start, -1, `missing ${signature}`);
  const opening = source.indexOf("{", start);
  assert.notEqual(opening, -1, `missing body for ${signature}`);
  let depth = 0;
  for (let index = opening; index < source.length; index += 1) {
    if (source[index] === "{") depth += 1;
    if (source[index] === "}") {
      depth -= 1;
      if (depth === 0) return source.slice(opening, index + 1);
    }
  }
  assert.fail(`unterminated body for ${signature}`);
}

function autoCreationErrorFormatter(source) {
  const messagesMatch = source.match(
    /const AUTO_CREATION_ERROR_MESSAGES = Object\.freeze\((\{[\s\S]*?\})\);/,
  );
  assert.ok(messagesMatch, "missing automatic creation error messages");
  const messages = Function(`"use strict"; return (${messagesMatch[1]});`)();
  const body = functionBody(source, "function autoCreationErrorMessage");
  return Function(
    "AUTO_CREATION_ERROR_MESSAGES",
    `"use strict"; return function (value) ${body}`,
  )(messages);
}

test("account detail exposes automatic alias creation with persistent credential handling", async () => {
  const source = await readFile(viewPath, "utf8");

  assert.match(source, /<el-switch[\s\S]*自动创建隐私邮箱/);
  assert.match(source, /resumeAutoCreationAfterAuth/);
  assert.match(source, /autoCreation\.plannedTimes\?\.length/);
  assert.match(source, /autoCreation\.plannedAt/);
  assert.doesNotMatch(source, /getAliasAutoCreationKeys/);
  assert.doesNotMatch(source, /clearAliasAutoCreationKeys/);
  assert.doesNotMatch(source, /pendingAutoKeys|batchSecrets|OneTimeSecret/);

  for (const field of ["apiKey", "imapPassword", "clientId", "refreshToken"]) {
    assert.match(source, new RegExp(`\\{\\{ (?:row|alias)\\.${field} \\}\\}`));
  }
  assert.match(source, /copyAliasCredentials\(row, ALIAS_EXPORT_OTP\)/);
  assert.match(source, /copyAliasCredentials\(row, ALIAS_EXPORT_IMAP\)/);
  assert.match(
    functionBody(source, "async function rotateKey"),
    /旧 API Key、取码链接、IMAP 密码、refresh token 和访问令牌会同时失效/,
  );
  assert.match(
    functionBody(source, "async function rotateKey"),
    /邮件消费状态和 IMAP 已读状态保持不变/,
  );
  assert.match(
    functionBody(source, "async function rotateKey"),
    /alias\.credentialMode/,
  );

  const formatAutoCreationError = autoCreationErrorFormatter(source);
  assert.equal(
    formatAutoCreationError("APPLE_SESSION_EXPIRED"),
    "Apple 登录已过期，请点击“同步隐私邮箱”并重新登录后重试",
  );
  assert.equal(
    formatAutoCreationError("APPLE_RATE_LIMITED"),
    "Apple 请求过于频繁，自动创建已进入冷却，冷却后会继续执行",
  );
  assert.equal(
    formatAutoCreationError(" unknown upstream detail "),
    " unknown upstream detail ",
  );
});

test("directory-confirmation aliases remain visibly gated without a key-claim queue", async () => {
  const source = await readFile(viewPath, "utf8");
  const confirmationBody = functionBody(
    source,
    "function isAliasConfirmationPending",
  );
  const isAliasConfirmationPending = Function(
    `"use strict"; return function (item) ${confirmationBody}`,
  )();

  assert.equal(
    isAliasConfirmationPending({
      enabled: false,
      lastSyncError: "APPLE_ALIAS_CONFIRMATION_PENDING",
    }),
    true,
  );
  assert.equal(
    isAliasConfirmationPending({
      enabled: true,
      lastSyncError: "APPLE_ALIAS_CONFIRMATION_PENDING",
    }),
    false,
  );
  for (const signature of [
    "async function rotateKey",
    "async function copyAliasCredentials",
    "async function toggleAlias",
    "async function removeAlias",
  ]) {
    assert.match(
      functionBody(source, signature),
      /isAliasConfirmationPending\(alias\)/,
      `${signature} must reject a directory-confirmation alias`,
    );
  }
});

test("alias deletion is presented as an irreversible iCloud operation", async () => {
  const source = await readFile(viewPath, "utf8");
  const remove = functionBody(source, "async function removeAlias");

  assert.match(source, /从 iCloud 永久删除隐私邮箱/);
  assert.match(remove, /将从 iCloud 永久删除/);
  assert.match(remove, /且无法恢复/);
  assert.match(remove, /await deleteAlias\(alias\.id/);
  assert.match(remove, /aliases\.value = aliases\.value\.filter/);
  assert.match(remove, /isAppleSessionInvalid\(error\)/);
  assert.match(remove, /openAppleLogin\(\{ error \}\)/);
  assert.match(remove, /本地记录已保留/);
});
