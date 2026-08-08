import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const viewPath = new URL(
  "../src/views/AccountDetailView.vue",
  import.meta.url,
);

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

test("account detail exposes automatic alias creation and safe key handling", async () => {
  const source = await readFile(viewPath, "utf8");

  assert.match(source, /<el-switch[\s\S]*自动创建隐私邮箱/);
  assert.match(source, /resumeAutoCreationAfterAuth/);
  assert.match(source, /getAliasAutoCreationKeys/);
  assert.match(source, /clearAliasAutoCreationKeys/);
  assert.match(source, /batchSecretsSource/);
  assert.match(source, /aliasId: item\.alias\?\.id/);
  assert.match(source, /@click="acknowledgeAndCloseBatchSecrets"/);
  assert.match(source, /@click="dismissBatchSecrets"/);
  assert.match(
    source,
    /autoCreationErrorMessage\(autoCreation\.lastError\)/,
  );

  const formatAutoCreationError = autoCreationErrorFormatter(source);
  assert.equal(
    formatAutoCreationError("APPLE_ACCOUNT_MISMATCH"),
    "Apple 登录账户或隐藏邮件地址的默认转发目标与当前主号不匹配，请确认登录了正确的 Apple 账户，并在 iCloud 设置中把‘转发到’改为当前主号后重新开启",
  );
  assert.equal(
    formatAutoCreationError("APPLE_SESSION_EXPIRED"),
    "Apple 登录已过期，请点击“同步隐私邮箱”并重新登录后重试",
  );
  assert.equal(
    formatAutoCreationError("APPLE_RATE_LIMITED"),
    "Apple 请求过于频繁，请稍后再试；自动创建会按计划继续执行",
  );
  assert.equal(
    formatAutoCreationError(" unknown upstream detail "),
    " unknown upstream detail ",
  );
  assert.equal(formatAutoCreationError("constructor"), "constructor");

  const acknowledge = functionBody(
    source,
    "async function acknowledgeAndCloseBatchSecrets",
  );
  assert.match(acknowledge, /clearAliasAutoCreationKeys\(/);
  assert.match(acknowledge, /aliasIds/);
  assert.match(acknowledge, /clearBatchSecrets\(\)/);

  const dismiss = functionBody(source, "function dismissBatchSecrets");
  assert.doesNotMatch(dismiss, /clearAliasAutoCreationKeys\(/);

  const close = functionBody(source, "function closeBatchSecrets");
  assert.doesNotMatch(close, /clearAliasAutoCreationKeys\(/);

  const mutationGuard = functionBody(source, "function detailMutationPending");
  assert.match(mutationGuard, /autoCreationLoading\.value/);
  assert.match(mutationGuard, /pendingAutoKeysLoading\.value/);
  assert.match(mutationGuard, /pendingAutoKeysClearing\.value/);

  const navigationGuard = functionBody(
    source,
    "function hasPendingSecretRequest",
  );
  assert.match(navigationGuard, /pendingAutoKeysLoading\.value/);
  assert.match(navigationGuard, /pendingAutoKeysClearing\.value/);
});
