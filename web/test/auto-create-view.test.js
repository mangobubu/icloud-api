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
  assert.match(source, /autoCreation\.plannedTimes\?\.length/);
  assert.match(source, /autoCreation\.plannedTimes/);
  assert.match(source, /autoCreation\.plannedAt/);

  const formatAutoCreationError = autoCreationErrorFormatter(source);
  assert.equal(
    formatAutoCreationError("APPLE_ACCOUNT_MISMATCH"),
    "Apple 登录账户或隐藏邮件地址的默认转发目标与当前主号不匹配，请确认登录了正确的 Apple 账户，并在 iCloud 设置中把‘转发到’改为当前主号后重新开启",
  );
  assert.equal(
    formatAutoCreationError("APPLE_FORWARDING_TARGET_MISSING"),
    "Apple 未能确认隐私邮箱的默认转发目标，本次没有发起创建；请确认当前主号可作为转发邮箱，或先在 iCloud 手动创建一个隐私邮箱后重新同步",
  );
  assert.equal(
    formatAutoCreationError("APPLE_SESSION_EXPIRED"),
    "Apple 登录已过期，请点击“同步隐私邮箱”并重新登录后重试",
  );
  assert.equal(
    formatAutoCreationError("APPLE_ACCOUNT_ACTION_REQUIRED"),
    "Apple 账户需要完成条款确认或其他账户操作，请前往 Apple 官网处理后重试",
  );
  assert.equal(
    formatAutoCreationError("APPLE_RATE_LIMITED"),
    "Apple 请求过于频繁，请稍后再试；自动创建会按计划继续执行",
  );
  assert.equal(
    formatAutoCreationError("APPLE_UPSTREAM_ERROR"),
    "Apple 服务暂时异常，请稍后再试；自动创建会按计划继续执行",
  );
  assert.equal(
    formatAutoCreationError("APPLE_ALIAS_CONFIRMATION_PENDING"),
    "Apple 已创建隐私邮箱，正在等待目录确认；后续自动创建计划只会继续确认，不会重复创建",
  );
  assert.equal(
    formatAutoCreationError("ALIAS_LIMIT_REACHED"),
    "当前主号已达到隐私邮箱容量上限，请确认自动创建计划状态",
  );
  assert.equal(
    formatAutoCreationError(" unknown upstream detail "),
    " unknown upstream detail ",
  );
  assert.equal(formatAutoCreationError("constructor"), "constructor");

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
  assert.equal(
    isAliasConfirmationPending({
      enabled: false,
      lastSyncError: "APPLE_UPSTREAM_ERROR",
    }),
    false,
  );

  const desktopStart = source.indexOf(
    '<div v-if="aliases.length" class="data-panel desktop-data-table">',
  );
  const mobileStart = source.indexOf(
    '<div v-if="aliases.length" class="mobile-record-list">',
  );
  const aliasFormStart = source.indexOf("<el-form", mobileStart);
  assert.ok(desktopStart >= 0 && mobileStart > desktopStart);
  assert.ok(aliasFormStart > mobileStart);
  const desktopAliases = source.slice(desktopStart, mobileStart);
  const mobileAliases = source.slice(mobileStart, aliasFormStart);

  assert.match(
    desktopAliases,
    /v-if="isAliasConfirmationPending\(row\)"[\s\S]{0,180}等待目录确认/,
  );
  assert.match(
    desktopAliases,
    /<el-switch\s+v-if="!isAliasConfirmationPending\(row\)"/,
  );
  assert.match(
    desktopAliases,
    /<div\s+v-if="!isAliasConfirmationPending\(row\)"\s+class="icon-action-row"/,
  );
  assert.match(
    mobileAliases,
    /v-if="isAliasConfirmationPending\(alias\)"[\s\S]{0,180}等待目录确认/,
  );
  assert.match(
    mobileAliases,
    /<div v-if="!isAliasConfirmationPending\(alias\)">\s*<dt>启用<\/dt>/,
  );
  assert.match(
    mobileAliases,
    /<footer\s+v-if="!isAliasConfirmationPending\(alias\)"\s+class="mobile-record__actions mobile-record__actions--three"/,
  );

  for (const signature of [
    "async function rotateKey",
    "async function copyAliasDirectLink",
    "async function toggleAlias",
    "async function removeAlias",
  ]) {
    assert.match(
      functionBody(source, signature),
      /isAliasConfirmationPending\(alias\)/,
      `${signature} must reject a pending alias`,
    );
  }

  const acknowledge = functionBody(
    source,
    "async function acknowledgeAndCloseBatchSecrets",
  );
  assert.match(acknowledge, /clearAliasAutoCreationKeys\(/);
  assert.match(acknowledge, /aliasIDAcknowledgementBatches\(aliasIds\)/);
  assert.match(acknowledge, /for \(const aliasIDBatch of aliasIDBatches\)/);
  assert.match(acknowledge, /clearBatchSecrets\(\)/);

  const batchBody = functionBody(
    source,
    "function aliasIDAcknowledgementBatches",
  );
  const aliasIDAcknowledgementBatches = Function(
    "AUTO_CREATION_KEY_ACK_BATCH_SIZE",
    `"use strict"; return function (aliasIds) ${batchBody}`,
  )(1000);
  const moreThanOneBatch = Array.from({ length: 1001 }, (_, index) => index + 1);
  const batches = aliasIDAcknowledgementBatches(moreThanOneBatch);
  assert.deepEqual(
    batches.map((batch) => batch.length),
    [1000, 1],
  );
  assert.deepEqual(batches.flat(), moreThanOneBatch);

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
