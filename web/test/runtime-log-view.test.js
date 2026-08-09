import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const viewPath = new URL("../src/views/LogsView.vue", import.meta.url);
const detailPath = new URL(
  "../src/components/RuntimeLogDetailDialog.vue",
  import.meta.url,
);
const routerPath = new URL("../src/router/index.js", import.meta.url);
const layoutPath = new URL("../src/layouts/AdminLayout.vue", import.meta.url);

test("all logs view is routed and exposed in admin navigation", async () => {
  const [router, layout] = await Promise.all([
    readFile(routerPath, "utf8"),
    readFile(layoutPath, "utf8"),
  ]);

  assert.match(router, /path:\s*"logs"[\s\S]{0,100}name:\s*"logs"/);
  assert.match(router, /import\("\.\.\/views\/LogsView\.vue"\)/);
  assert.match(layout, /to:\s*"\/admin\/logs"[\s\S]{0,100}label:\s*"全部日志"/);
});

test("all logs view supports filters, cursor paging, live refresh, and responsive records", async () => {
  const source = await readFile(viewPath, "utf8");

  assert.match(source, /v-model="filters\.level"/);
  assert.match(source, /v-model="filters\.accountId"/);
  assert.match(source, /v-model="keywordDraft"/);
  assert.match(source, /route\.query\.account_id/);
  assert.match(source, /route\.query\.level/);
  assert.match(source, /route\.query\.query/);
  assert.match(source, /getRuntimeLogs\(currentRequestOptions\(cursor\)\)/);
  assert.match(source, /nextBeforeId/);
  assert.match(source, /加载更多/);
  assert.match(source, /MAX_VISIBLE_LOGS\s*=\s*2000/);
  assert.match(
    source,
    /if \(autoRefreshEnabled\.value\)[\s\S]{0,100}autoRefreshEnabled\.value = false/,
  );
  assert.match(source, /createLiveRefresh\(\(\) => loadLatestLogs\(\{ silent: true \}\)\)/);
  assert.match(source, /v-model="autoRefreshEnabled"/);
  assert.match(source, /class="data-panel desktop-data-table"/);
  assert.match(source, /class="mobile-record-list"/);
  assert.match(source, /<RuntimeLogDetailDialog/);
  assert.match(source, /@media \(max-width: 720px\)/);
});

test("reenabling live refresh cancels an in-flight historical page before loading latest logs", async () => {
  const source = await readFile(viewPath, "utf8");
  const watcherStart = source.indexOf("watch(autoRefreshEnabled");
  const watcherEnd = source.indexOf("\n});", watcherStart);
  const watcher = source.slice(watcherStart, watcherEnd);
  const operations = [
    "logRequestGate.invalidate();",
    "loadMoreGeneration += 1;",
    "loadingMore.value = false;",
    "loadLatestLogs({ force: true });",
    "liveRefresh.start({ immediate: false });",
  ];

  assert.notEqual(watcherStart, -1);
  assert.notEqual(watcherEnd, -1);
  let previousIndex = -1;
  for (const operation of operations) {
    const operationIndex = watcher.indexOf(operation);
    assert.ok(operationIndex > previousIndex, `${operation} must run in order`);
    previousIndex = operationIndex;
  }
});

test("runtime log details are escaped, copyable, and mobile safe", async () => {
  const source = await readFile(detailPath, "utf8");

  assert.match(source, /<pre>\{\{ log\.message \|\| "-" \}\}<\/pre>/);
  assert.match(source, /<pre>\{\{ attributesText \}\}<\/pre>/);
  assert.doesNotMatch(source, /v-html|innerHTML/);
  assert.match(source, /await copyText\(fullLogText\.value\)/);
  assert.match(source, /完整日志已复制/);
  assert.match(source, /role="status"[\s\S]{0,100}aria-live="polite"/);
  assert.doesNotMatch(source, /runtime-log-detail__announcement/);
  assert.match(source, /width="min\(760px, calc\(100vw - 28px\)\)"/);
  assert.match(source, /overflow-wrap:\s*anywhere/);
  assert.match(source, /@media \(max-width: 600px\)/);
});
