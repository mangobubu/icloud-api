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
