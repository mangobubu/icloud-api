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
  assert.match(source, /getRuntimeLogRun\(syncRunId,\s*\{/);
  assert.match(source, /getAutoCreateLogRun\(autoCreateRunId,\s*\{/);
  assert.match(source, /const autoCreateRunId = String\(log\?\.autoCreateRunId/);
  assert.match(source, /if \(autoCreateRunId\) return `auto-create:\$\{autoCreateRunId\}`/);
  assert.match(source, /detailFlowAbortController\?\.abort\(\)/);
  assert.match(source, /:flow-loading="detailFlowLoading"/);
  assert.match(source, /:flow-error="detailFlowError"/);
  assert.match(source, /@retry-flow="loadSelectedLogFlow"/);
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

test("runtime log details show loadable, copyable sync and automatic creation timelines", async () => {
  const source = await readFile(detailPath, "utf8");

  assert.match(source, /<pre>\{\{ log\.message \|\| "-" \}\}<\/pre>/);
  assert.match(source, /<pre>\{\{ attributesText \}\}<\/pre>/);
  assert.doesNotMatch(source, /v-html|innerHTML/);
  assert.match(source, /v-if="flowLoading"[\s\S]{0,300}<el-skeleton/);
  assert.match(source, /v-if="flowError"[\s\S]{0,300}<RequestAlert/);
  assert.match(source, /重新加载完整流程/);
  assert.match(source, /v-if="hasFlow"[\s\S]{0,250}@click="emit\('retry-flow'\)"/);
  assert.match(source, /"刷新自动创建流程"\s*:\s*"刷新同步流程"/);
  assert.match(source, /v-for="entry in orderedFlowLogs"/);
  assert.match(source, /失败于 \{\{ stageLabel\(entry\.failedStage\) \}\}/);
  assert.match(source, /<strong>错误详情<\/strong>[\s\S]{0,100}entry\.errorDetail/);
  assert.match(source, /entry\.errorCode/);
  assert.match(source, /entry\.errorClass/);
  assert.match(source, /entry\.causeCategory/);
  assert.match(source, /原因分类/);
  assert.match(source, /schedule:\s*"计划调度"/);
  assert.match(source, /entry\.errorContext/);
  assert.match(source, /entry\.httpStatus/);
  assert.match(source, /entry\.retryable/);
  assert.match(source, /entry\.elapsedMs/);
  assert.match(source, /batchElapsedMs\(entry\)/);
  assert.match(source, /batch_elapsed_ms/);
  assert.match(source, /entry\.scheduleAction/);
  assert.match(source, /可能已产生远端变更/);
  assert.match(source, /resultStateRecorded\(entry\)/);
  assert.match(source, /result_state_recorded/);
  assert.match(source, /计划状态写回/);
  assert.match(source, /function diagnosticLines\(entry\)/);
  assert.match(source, /失败诊断/);
  assert.match(source, /操作位置：[\s\S]{0,100}entry\.failedOperation/);
  assert.match(source, /当前仅展示日志缓冲区内保留的部分流程/);
  assert.match(source, /\["started", "run_started", "run_queued"\]/);
  assert.match(source, /v-else-if="flowIsRunning"/);
  assert.match(source, /该次同步尚未结束/);
  assert.match(source, /该次自动创建尚未结束/);
  assert.match(source, /Boolean\(props\.log\?\.autoCreateRunId\)/);
  assert.match(source, /自动创建流程详情/);
  assert.match(source, /创建编号/);
  assert.match(source, /自动创建流程/);
  assert.match(source, /runtimeLogAutoCreateStageLabel\(stage\)/);
  assert.match(source, /entry\?\.autoCreateStage/);
  assert.match(source, /entry\?\.autoCreatePercent/);
  assert.match(source, /entry\?\.autoCreateEvent/);
  assert.match(source, /await copyText\(fullLogText\.value\)/);
  assert.match(source, /完整日志已复制/);
  assert.match(source, /完整同步流程已复制/);
  assert.match(source, /完整自动创建流程已复制/);
  assert.match(source, /复制完整自动创建流程/);
  assert.match(source, /function flowLogText\(\)/);
  assert.match(source, /role="status"[\s\S]{0,100}aria-live="polite"/);
  assert.doesNotMatch(source, /runtime-log-detail__announcement/);
  assert.match(source, /width="min\(900px, calc\(100vw - 28px\)\)"/);
  assert.match(source, /overflow-wrap:\s*anywhere/);
  assert.match(source, /max-height:\s*52dvh/);
  assert.match(source, /@media \(max-width: 600px\)/);
});
