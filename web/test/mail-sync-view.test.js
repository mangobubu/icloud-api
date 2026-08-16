import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const syncStatusPath = new URL(
  "../src/components/SyncStatus.vue",
  import.meta.url,
);
const accountDetailPath = new URL(
  "../src/views/AccountDetailView.vue",
  import.meta.url,
);
const accountsViewPath = new URL(
  "../src/views/AccountsView.vue",
  import.meta.url,
);

test("shared sync status renders manual and automatic progress", async () => {
  const source = await readFile(syncStatusPath, "utf8");

  assert.match(source, /syncProgressPresentation\(props\.item\.syncProgress\)/);
  assert.match(source, /v-if="progress\.active"/);
  assert.match(source, /<el-progress/);
  assert.match(source, /:indeterminate="progress\.indeterminate"/);
  assert.match(
    source,
    /:aria-hidden="progress\.indeterminate \? 'true' : undefined"/,
  );
  assert.match(source, /progress\.stageLabel/);
  assert.match(source, /progress\.percentage/);
});

test("account list and detail share server-driven sync progress", async () => {
  const [detailSource, listSource] = await Promise.all([
    readFile(accountDetailPath, "utf8"),
    readFile(accountsViewPath, "utf8"),
  ]);

  assert.match(detailSource, /const syncActive = computed\(/);
  assert.match(detailSource, /account\.value\?\.syncProgress\?\.active/);
  assert.match(detailSource, /:loading="syncLoading \|\| syncActive"/);
  assert.match(
    detailSource,
    /if \(syncLoading\.value \|\| syncActive\.value \|\| randomAliasLoading\.value\) return/,
  );
  assert.ok(
    (listSource.match(/<SyncStatus :item=/g) || []).length >= 2,
    "desktop and mobile account lists must use the shared sync status",
  );
});

test("existing sync error summaries open the reusable full log dialog", async () => {
  const [statusSource, detailSource] = await Promise.all([
    readFile(syncStatusPath, "utf8"),
    readFile(accountDetailPath, "utf8"),
  ]);

  assert.match(
    statusSource,
    /错误：\{\{ compactRunes\(item\.lastSyncError\) \}\}[\s\S]*<SyncErrorLogDialog/,
  );
  assert.match(
    detailSource,
    /\{\{ account\.lastSyncError \}\}[\s\S]*<SyncErrorLogDialog/,
  );
  assert.match(
    detailSource,
    /account\.lastSyncErrorLog \|\| account\.lastSyncError/,
  );
});
