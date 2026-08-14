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

function autoCreationPanel(source) {
  const start = source.indexOf('class="auto-creation-panel"');
  assert.notEqual(start, -1, "missing automatic creation panel");
  const end = source.indexOf(
    'class="data-panel desktop-data-table account-alias-table"',
    start,
  );
  assert.ok(end > start, "missing alias table after automatic creation panel");
  return source.slice(start, end);
}

test("automatic creation panel displays the current alias count", async () => {
  const source = await readFile(viewPath, "utf8");
  assert.match(
    autoCreationPanel(source),
    /\{\{[^}]*\b(?:aliasCount|aliases\.length)\b[^}]*\}\}/,
  );
});

test("directory synchronization publishes the authoritative credential-bearing alias list", async () => {
  const source = await readFile(viewPath, "utf8");
  const syncBody = functionBody(source, "async function performAliasesSync");

  assert.match(syncBody, /const result = await syncAccountAliases/);
  assert.match(syncBody, /aliases\.value = result\.aliases/);
  assert.match(syncBody, /syncAccountAliasCount\(\)/);
  assert.match(syncBody, /完整凭证已显示在列表中/);
  assert.doesNotMatch(syncBody, /pending|acknowledge|oneTime|batchSecrets/i);
});

test("alias count synchronizer derives the value from the visible list", async () => {
  const source = await readFile(viewPath, "utf8");
  const body = functionBody(source, "function syncAccountAliasCount");
  const account = { value: { id: 12, aliasCount: 99 } };
  const aliases = { value: [{ id: 1 }, { id: 2 }, { id: 3 }] };
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

test("successful alias creation and deletion update the displayed alias count", async () => {
  const source = await readFile(viewPath, "utf8");
  const addAlias = functionBody(source, "async function addAlias");
  const removeAlias = functionBody(source, "async function removeAlias");

  assert.match(addAlias, /aliases\.value = \[\.\.\.aliases\.value, result\.alias\]\.sort/);
  assert.match(addAlias, /syncAccountAliasCount\(\)/);
  assert.match(addAlias, /整套凭证已签发并常驻显示/);
  assert.match(removeAlias, /aliases\.value\s*=\s*aliases\.value\.filter/);
  assert.match(removeAlias, /syncAccountAliasCount\(\)/);
});
