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

test("directory synchronization refreshes the current server-backed alias page", async () => {
  const source = await readFile(viewPath, "utf8");
  const syncBody = functionBody(source, "async function performAliasesSync");

  assert.match(syncBody, /const result = await syncAccountAliases/);
  assert.match(syncBody, /const detailLoaded = await loadDetail\(\)/);
  assert.match(syncBody, /if \(!detailLoaded\)/);
  assert.match(syncBody, /邮箱列表刷新失败/);
  assert.doesNotMatch(syncBody, /aliases\.value = result\.aliases/);
  assert.match(syncBody, /可通过列表中的复制操作导出完整凭证/);
  assert.doesNotMatch(syncBody, /pending|acknowledge|oneTime|batchSecrets/i);
});

test("account alias count is never derived from the visible page length", async () => {
  const source = await readFile(viewPath, "utf8");

  assert.doesNotMatch(source, /function syncAccountAliasCount/);
  assert.doesNotMatch(source, /aliasCount:\s*(?:aliases|nextAliases)\.length/);
  assert.doesNotMatch(source, /account\.value\.aliasCount\s*=/);
  assert.match(source, /detail\?\.pagination\?\.total/);
  assert.match(source, /\{\{ account\.aliasCount \}\}/);
});

test("alias creation, random generation, and deletion refresh the current page", async () => {
  const source = await readFile(viewPath, "utf8");
  const addAlias = functionBody(source, "async function addAlias");
  const generateRandomAliases = functionBody(
    source,
    "async function generateRandomAliases",
  );
  const removeAlias = functionBody(source, "async function removeAlias");

  assert.match(addAlias, /await loadDetail\(\)/);
  assert.doesNotMatch(addAlias, /aliases\.value\s*=/);
  assert.match(addAlias, /整套凭证已签发，可通过列表中的复制操作导出/);
  assert.match(generateRandomAliases, /await loadDetail\(\)/);
  assert.doesNotMatch(generateRandomAliases, /mergedByID|aliases\.value\s*=/);
  assert.match(removeAlias, /await loadDetail\(\)/);
  assert.doesNotMatch(removeAlias, /aliases\.value\s*=\s*aliases\.value\.filter/);
});
