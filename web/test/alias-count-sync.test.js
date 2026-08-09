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

function autoCreationPanel(source) {
  const start = source.indexOf('class="auto-creation-panel"');
  assert.notEqual(start, -1, "missing automatic creation panel");
  const end = source.indexOf(
    '<div v-if="aliases.length" class="data-panel desktop-data-table">',
    start,
  );
  assert.ok(end > start, "missing alias table after automatic creation panel");
  return source.slice(start, end);
}

test("automatic creation panel displays the current alias count", async () => {
  const source = await readFile(viewPath, "utf8");
  const panel = autoCreationPanel(source);

  // Keep the count tied to reactive account/list state so it changes after a refresh.
  assert.match(
    panel,
    /\{\{[^}]*\b(?:aliasCount|aliases\.length)\b[^}]*\}\}/,
  );
});

test("automatic creation results merge into the alias list and count immediately", async () => {
  const source = await readFile(viewPath, "utf8");
  const openPending = functionBody(
    source,
    "async function openPendingAutoCreationKeys",
  );
  const mergeBody = functionBody(source, "function mergeAliases");
  const orderBody = functionBody(source, "function aliasAddressOrder");
  const countSync = functionBody(source, "function syncAccountAliasCount");

  assert.match(openPending, /result(?:\?\.|\.)created/);
  assert.match(openPending, /mergeAliases\([\s\S]*?result(?:\?\.|\.)created/);

  const account = { value: { id: 12, aliasCount: 2 } };
  const aliases = {
    value: [
      { id: 1, address: "first@icloud.com", label: "first" },
      { id: 2, address: "second@icloud.com", label: "old" },
    ],
  };
  const sync = Function(
    "account",
    "aliases",
    `"use strict"; return function () ${countSync}`,
  )(account, aliases);
  const order = Function(
    `"use strict"; return function (left, right) ${orderBody}`,
  )();
  const merge = Function(
    "aliases",
    "aliasAddressOrder",
    "syncAccountAliasCount",
    `"use strict"; return function (items) ${mergeBody}`,
  )(aliases, order, sync);

  merge([
    { id: 2, address: "second@icloud.com", label: "updated" },
    { id: 3, address: "alpha@icloud.com", label: "new" },
  ]);

  assert.deepEqual(aliases.value.map((alias) => alias.id), [3, 1, 2]);
  assert.equal(aliases.value.find((alias) => alias.id === 2).label, "updated");
  assert.equal(account.value.aliasCount, 3);
});

test("alias count synchronizer derives the value from the visible list", async () => {
  const source = await readFile(viewPath, "utf8");
  const body = functionBody(source, "function syncAccountAliasCount");
  const account = {
    value: { id: 12, aliasCount: 99 },
  };
  const aliases = {
    value: [{ id: 1 }, { id: 2 }, { id: 3 }],
  };
  const sync = Function(
    "account",
    "aliases",
    `"use strict"; return function () ${body}`,
  )(account, aliases);

  sync();
  assert.equal(account.value.aliasCount, 3);

  aliases.value = [];
  sync();
  assert.equal(account.value.aliasCount, 0);
});

test("successful alias deletion updates the displayed alias count", async () => {
  const source = await readFile(viewPath, "utf8");
  const removeAlias = functionBody(source, "async function removeAlias");

  assert.match(removeAlias, /aliases\.value\s*=\s*aliases\.value\.filter/);
  assert.match(removeAlias, /syncAccountAliasCount\(\)/);
});

test("closing automatic key results refreshes the authoritative detail", async () => {
  const source = await readFile(viewPath, "utf8");
  const acknowledge = functionBody(
    source,
    "async function acknowledgeAndCloseBatchSecrets",
  );
  const dismiss = functionBody(source, "function dismissBatchSecrets");

  assert.match(
    acknowledge,
    /pendingAutoKeysClearing\.value = false;[\s\S]*loadDetail\(\{ silent: true \}\)/,
  );
  assert.match(
    dismiss,
    /clearBatchSecrets\(\)[\s\S]*loadDetail\(\{ silent: true \}\)/,
  );
});
