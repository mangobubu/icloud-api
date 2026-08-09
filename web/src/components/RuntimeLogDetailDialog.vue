<template>
  <el-dialog
    :model-value="modelValue"
    class="runtime-log-detail"
    :title="dialogTitle"
    width="min(900px, calc(100vw - 28px))"
    append-to-body
    destroy-on-close
    @update:model-value="emit('update:modelValue', $event)"
  >
    <template v-if="log">
      <div class="runtime-log-detail__toolbar">
        <span
          class="runtime-log-detail__feedback"
          role="status"
          aria-live="polite"
          aria-atomic="true"
        >
          {{ copyFeedback }}
        </span>
        <el-tooltip v-if="hasSyncFlow" content="刷新同步流程" placement="top">
          <el-button
            :icon="Refresh"
            circle
            :loading="flowLoading"
            aria-label="刷新同步流程"
            @click="emit('retry-flow')"
          />
        </el-tooltip>
        <el-tooltip :content="copyTooltip" placement="top">
          <el-button
            :icon="CopyDocument"
            circle
            :loading="copying"
            :disabled="hasSyncFlow && flowLoading"
            :aria-label="copyTooltip"
            @click="copyLog"
          />
        </el-tooltip>
      </div>

      <dl class="runtime-log-detail__meta">
        <div>
          <dt>时间</dt>
          <dd>{{ formatTime(log.time, { seconds: true }) }}</dd>
        </div>
        <div>
          <dt>级别</dt>
          <dd>
            <el-tag :type="levelMeta.type" effect="plain" size="small">
              {{ levelMeta.label }}
            </el-tag>
          </dd>
        </div>
        <div>
          <dt>来源</dt>
          <dd><code>{{ log.source || "system" }}</code></dd>
        </div>
        <div>
          <dt>主号</dt>
          <dd>{{ accountDisplayLabel }}</dd>
        </div>
        <div v-if="hasSyncFlow">
          <dt>触发方式</dt>
          <dd>{{ syncTriggerLabel }}</dd>
        </div>
        <div v-if="hasSyncFlow" class="runtime-log-detail__meta-wide">
          <dt>同步编号</dt>
          <dd><code>{{ log.syncRunId }}</code></dd>
        </div>
        <div v-else class="runtime-log-detail__meta-wide">
          <dt>请求编号</dt>
          <dd><code>{{ log.requestId || "-" }}</code></dd>
        </div>
      </dl>

      <section
        v-if="hasSyncFlow"
        class="runtime-log-detail__section runtime-log-flow-section"
        aria-labelledby="runtime-log-flow-title"
      >
        <div class="runtime-log-detail__section-heading">
          <h3 id="runtime-log-flow-title">同步流程</h3>
          <span v-if="!flowLoading && orderedFlowLogs.length">
            {{ flowSummary }}
          </span>
        </div>

        <div
          v-if="flowLoading"
          class="runtime-log-flow__loading"
          role="status"
          aria-live="polite"
        >
          <el-skeleton :rows="5" animated />
          <span class="sr-only">正在加载完整同步流程</span>
        </div>

        <template v-else>
          <div v-if="flowError" class="runtime-log-flow__load-error">
            <RequestAlert :error="flowError" />
            <p>以下暂时只显示当前日志，重新加载后可查看完整流程。</p>
            <el-button :icon="Refresh" @click="emit('retry-flow')">
              重新加载完整流程
            </el-button>
          </div>

          <el-alert
            v-else-if="flowIsPartial"
            class="runtime-log-flow__partial"
            type="warning"
            title="当前仅展示日志缓冲区内保留的部分流程，较早记录可能已被覆盖。"
            :closable="false"
            show-icon
          />

          <el-alert
            v-else-if="flowIsRunning"
            class="runtime-log-flow__partial"
            type="info"
            title="该次同步尚未结束，重新打开或点击重新加载可查看后续步骤。"
            :closable="false"
            show-icon
          />

          <ol v-if="orderedFlowLogs.length" class="runtime-log-flow">
            <li
              v-for="entry in orderedFlowLogs"
              :key="entry.id ?? `${entry.time}-${entry.message}`"
              class="runtime-log-flow__item"
              :class="{
                'runtime-log-flow__item--failed': isFailedFlowEntry(entry),
                'runtime-log-flow__item--completed': isCompletedFlowEntry(entry),
              }"
            >
              <span class="runtime-log-flow__marker" aria-hidden="true"></span>
              <article class="runtime-log-flow__content">
                <header class="runtime-log-flow__header">
                  <time :datetime="entry.time || undefined">
                    {{ formatTime(entry.time, { seconds: true }) }}
                  </time>
                  <el-tag
                    :type="runtimeLogLevelMeta(entry.level).type"
                    effect="plain"
                    size="small"
                  >
                    {{ runtimeLogSyncStageLabel(entry.syncStage) }}
                  </el-tag>
                  <span v-if="entry.syncPercent !== null" class="runtime-log-flow__metric">
                    {{ Math.round(entry.syncPercent) }}%
                  </span>
                  <span v-if="entry.syncBatch !== null" class="runtime-log-flow__metric">
                    第 {{ entry.syncBatch }} 批
                  </span>
                  <span v-if="entry.failedStage" class="runtime-log-flow__failed-stage">
                    失败于 {{ runtimeLogSyncStageLabel(entry.failedStage) }}
                  </span>
                </header>

                <p class="runtime-log-flow__message">{{ entry.message || "-" }}</p>

                <div class="runtime-log-flow__source">
                  <span>{{ runtimeLogLevelMeta(entry.level).label }}</span>
                  <code>{{ entry.source || "system" }}</code>
                  <code v-if="entry.requestId">请求 {{ entry.requestId }}</code>
                </div>

                <div
                  v-if="entry.errorDetail || entry.failedOperation"
                  class="runtime-log-flow__error-context"
                >
                  <template v-if="entry.errorDetail">
                    <strong>错误详情</strong>
                    <pre>{{ entry.errorDetail }}</pre>
                  </template>
                  <p v-if="entry.failedOperation">
                    操作位置：<code>{{ entry.failedOperation }}</code>
                  </p>
                </div>

                <details v-if="flowContextText(entry)" class="runtime-log-flow__context">
                  <summary>查看该步骤上下文</summary>
                  <pre>{{ flowContextText(entry) }}</pre>
                </details>
              </article>
            </li>
          </ol>

          <el-empty v-else description="该次同步暂无可用的流程日志" :image-size="72" />
        </template>
      </section>

      <template v-else>
        <section class="runtime-log-detail__section" aria-labelledby="runtime-log-message-title">
          <h3 id="runtime-log-message-title">消息</h3>
          <pre>{{ log.message || "-" }}</pre>
        </section>

        <section
          v-if="attributesText"
          class="runtime-log-detail__section"
          aria-labelledby="runtime-log-attributes-title"
        >
          <h3 id="runtime-log-attributes-title">上下文</h3>
          <pre>{{ attributesText }}</pre>
        </section>
      </template>
    </template>

    <template #footer>
      <el-button @click="emit('update:modelValue', false)">关闭</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { CopyDocument, Refresh } from "@element-plus/icons-vue";
import { computed, onBeforeUnmount, ref, watch } from "vue";

import RequestAlert from "./RequestAlert.vue";
import { copyText } from "../utils/clipboard.js";
import { formatTime } from "../utils/format.js";
import {
  chronologicalRuntimeLogs,
  runtimeLogAttributesText,
  runtimeLogFlowContextText,
  runtimeLogLevelMeta,
  runtimeLogSyncStageLabel,
  runtimeLogSyncTriggerLabel,
} from "../utils/runtimeLogs.js";

const props = defineProps({
  modelValue: { type: Boolean, default: false },
  log: { type: Object, default: null },
  flowLogs: { type: Array, default: () => [] },
  flowLoading: { type: Boolean, default: false },
  flowError: { type: Object, default: null },
  accountLabel: { type: String, default: "" },
});
const emit = defineEmits(["update:modelValue", "retry-flow"]);

const copying = ref(false);
const copyFeedback = ref("");
let feedbackTimer = null;

const hasSyncFlow = computed(() => Boolean(props.log?.syncRunId));
const dialogTitle = computed(() =>
  hasSyncFlow.value ? "同步流程详情" : "日志详情",
);
const copyTooltip = computed(() => {
  if (hasSyncFlow.value && props.flowLoading) return "流程加载完成后可复制";
  return hasSyncFlow.value ? "复制完整同步流程" : "复制完整日志";
});
const levelMeta = computed(() => runtimeLogLevelMeta(props.log?.level));
const attributesText = computed(() =>
  runtimeLogAttributesText(props.log?.attributes),
);
const accountDisplayLabel = computed(() => {
  if (props.accountLabel) return props.accountLabel;
  return props.log?.accountId ? `主号 #${props.log.accountId}` : "-";
});
const orderedFlowLogs = computed(() =>
  chronologicalRuntimeLogs([
    ...props.flowLogs,
    ...(props.log ? [props.log] : []),
  ]),
);
const syncTriggerLabel = computed(() => {
  const trigger =
    props.log?.syncTrigger ||
    orderedFlowLogs.value.find((entry) => entry.syncTrigger)?.syncTrigger;
  return runtimeLogSyncTriggerLabel(trigger);
});
const flowDuration = computed(() => {
  if (orderedFlowLogs.value.length < 2) return "";
  const startedAt = Date.parse(orderedFlowLogs.value[0]?.time || "");
  const endedAt = Date.parse(orderedFlowLogs.value.at(-1)?.time || "");
  if (!Number.isFinite(startedAt) || !Number.isFinite(endedAt) || endedAt < startedAt) {
    return "";
  }
  const milliseconds = endedAt - startedAt;
  if (milliseconds < 1000) return `${milliseconds} 毫秒`;
  const seconds = milliseconds / 1000;
  if (seconds < 60) return `${seconds.toFixed(seconds < 10 ? 1 : 0)} 秒`;
  const minutes = Math.floor(seconds / 60);
  const remainder = Math.round(seconds % 60);
  return remainder ? `${minutes} 分 ${remainder} 秒` : `${minutes} 分钟`;
});
const flowSummary = computed(() => {
  const parts = [`${orderedFlowLogs.value.length} 条记录`, syncTriggerLabel.value];
  if (flowDuration.value) parts.push(`耗时 ${flowDuration.value}`);
  return parts.join(" · ");
});
const flowIsPartial = computed(() => {
  if (props.flowError || !orderedFlowLogs.value.length) return false;
  const firstEvent = orderedFlowLogs.value[0]?.syncEvent;
  const hasStart = ["started", "run_started", "run_queued"].includes(firstEvent);
  return !hasStart;
});
const flowIsRunning = computed(() => {
  if (props.flowError || flowIsPartial.value || !orderedFlowLogs.value.length) return false;
  const lastEntry = orderedFlowLogs.value.at(-1);
  const lastEvent = lastEntry?.syncEvent;
  const hasTerminal = [
    "completed",
    "run_completed",
    "failed",
    "run_failed",
    "cancelled",
    "run_cancelled",
  ].includes(lastEvent) || ["completed", "failed", "cancelled"].includes(lastEntry?.syncStage);
  return !hasTerminal;
});

function flowContextText(entry) {
  return runtimeLogFlowContextText(entry);
}

function isFailedFlowEntry(entry) {
  return (
    entry?.level === "error" ||
    ["failed", "run_failed"].includes(entry?.syncEvent) ||
    entry?.syncStage === "failed" ||
    Boolean(entry?.errorDetail)
  );
}

function isCompletedFlowEntry(entry) {
  return (
    ["completed", "run_completed"].includes(entry?.syncEvent) ||
    entry?.syncStage === "completed"
  );
}

function flowLogText() {
  const lines = [
    `同步编号: ${props.log.syncRunId}`,
    `主号: ${accountDisplayLabel.value}`,
    `触发方式: ${syncTriggerLabel.value}`,
    `流程记录: ${orderedFlowLogs.value.length} 条`,
  ];
  if (flowDuration.value) lines.push(`耗时: ${flowDuration.value}`);

  for (const entry of orderedFlowLogs.value) {
    const stage = runtimeLogSyncStageLabel(entry.syncStage);
    const metrics = [];
    if (entry.syncPercent !== null) metrics.push(`${Math.round(entry.syncPercent)}%`);
    if (entry.syncBatch !== null) metrics.push(`第 ${entry.syncBatch} 批`);
    lines.push(
      "",
      `[${formatTime(entry.time, { seconds: true })}] ${stage}${metrics.length ? ` (${metrics.join("，")})` : ""}`,
      `级别: ${String(entry.level || "info").toUpperCase()}`,
      `来源: ${entry.source || "system"}`,
      `消息: ${entry.message || "-"}`,
    );
    if (entry.requestId) lines.push(`请求编号: ${entry.requestId}`);
    if (entry.failedStage) {
      lines.push(`失败步骤: ${runtimeLogSyncStageLabel(entry.failedStage)}`);
    }
    if (entry.errorDetail) lines.push(`错误详情: ${entry.errorDetail}`);
    if (entry.failedOperation) lines.push(`操作位置: ${entry.failedOperation}`);
    const context = flowContextText(entry);
    if (context) lines.push("步骤上下文:", context);
  }
  return lines.join("\n");
}

const fullLogText = computed(() => {
  if (!props.log) return "";
  if (hasSyncFlow.value) return flowLogText();

  const lines = [
    `时间: ${formatTime(props.log.time, { seconds: true })}`,
    `级别: ${String(props.log.level || "info").toUpperCase()}`,
    `来源: ${props.log.source || "system"}`,
  ];
  if (props.log.accountId) lines.push(`主号: #${props.log.accountId}`);
  if (props.log.requestId) lines.push(`请求编号: ${props.log.requestId}`);
  lines.push("", "消息:", props.log.message || "-");
  if (attributesText.value) {
    lines.push("", "上下文:", attributesText.value);
  }
  return lines.join("\n");
});

async function copyLog() {
  if (copying.value || props.flowLoading || !fullLogText.value) return;
  copying.value = true;
  const copied = await copyText(fullLogText.value);
  copying.value = false;

  const message = copied
    ? hasSyncFlow.value
      ? "完整同步流程已复制"
      : "完整日志已复制"
    : "日志复制失败，请手动复制";
  copyFeedback.value = message;

  window.clearTimeout(feedbackTimer);
  feedbackTimer = window.setTimeout(() => {
    copyFeedback.value = "";
  }, 1800);
}

watch(
  () => [props.modelValue, props.log?.id],
  () => {
    copyFeedback.value = "";
  },
);

onBeforeUnmount(() => {
  window.clearTimeout(feedbackTimer);
});
</script>

<style scoped>
.runtime-log-detail__toolbar {
  display: flex;
  min-height: 32px;
  align-items: center;
  justify-content: flex-end;
  gap: 8px;
  margin-bottom: 12px;
}

.runtime-log-detail__feedback {
  min-width: 0;
  color: var(--text-secondary);
  font-size: 12px;
  overflow-wrap: anywhere;
}

.runtime-log-detail__meta {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  margin: 0 0 18px;
  border: 1px solid var(--border);
  border-radius: 5px;
}

.runtime-log-detail__meta > div {
  display: grid;
  min-width: 0;
  grid-template-columns: 86px minmax(0, 1fr);
  align-items: center;
  min-height: 44px;
  padding: 8px 12px;
  border-bottom: 1px solid var(--border);
}

.runtime-log-detail__meta > div:nth-child(odd):not(.runtime-log-detail__meta-wide) {
  border-right: 1px solid var(--border);
}

.runtime-log-detail__meta-wide {
  grid-column: 1 / -1;
  border-bottom: 0 !important;
}

.runtime-log-detail__meta dt {
  color: var(--text-secondary);
  font-size: 12px;
}

.runtime-log-detail__meta dd,
.runtime-log-detail__meta code {
  min-width: 0;
  overflow-wrap: anywhere;
}

.runtime-log-detail__section + .runtime-log-detail__section {
  margin-top: 16px;
}

.runtime-log-detail__section-heading {
  display: flex;
  min-height: 28px;
  align-items: baseline;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 6px 16px;
  margin-bottom: 8px;
}

.runtime-log-detail__section h3 {
  margin: 0 0 7px;
  font-size: 13px;
  font-weight: 700;
}

.runtime-log-detail__section-heading h3 {
  margin: 0;
}

.runtime-log-detail__section-heading > span {
  color: var(--text-secondary);
  font-size: 12px;
}

.runtime-log-detail__section > pre,
.runtime-log-flow pre {
  box-sizing: border-box;
  width: 100%;
  max-width: 100%;
  margin: 0;
  padding: 13px;
  overflow: auto;
  color: var(--text);
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", monospace;
  font-size: 12px;
  line-height: 1.6;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  word-break: break-word;
  background: var(--surface-subtle);
  border: 1px solid var(--border);
  border-radius: 4px;
}

.runtime-log-detail__section > pre {
  max-height: min(42vh, 380px);
}

.runtime-log-flow__loading {
  min-height: 240px;
  padding: 12px 4px;
}

.runtime-log-flow__load-error {
  display: grid;
  justify-items: start;
  gap: 10px;
  margin-bottom: 16px;
}

.runtime-log-flow__partial {
  margin-bottom: 16px;
}

.runtime-log-flow__load-error p {
  margin: 0;
  color: var(--text-secondary);
  font-size: 12px;
}

.runtime-log-flow {
  max-height: min(58vh, 620px);
  margin: 0;
  padding: 4px 2px 4px 20px;
  overflow: auto;
  list-style: none;
}

.runtime-log-flow__item {
  position: relative;
  min-width: 0;
  padding: 0 6px 22px 24px;
  border-left: 2px solid var(--border);
}

.runtime-log-flow__item:last-child {
  padding-bottom: 2px;
  border-left-color: transparent;
}

.runtime-log-flow__marker {
  position: absolute;
  top: 7px;
  left: -7px;
  box-sizing: border-box;
  width: 12px;
  height: 12px;
  background: var(--surface);
  border: 3px solid var(--primary);
  border-radius: 50%;
}

.runtime-log-flow__item--failed .runtime-log-flow__marker {
  border-color: var(--danger);
}

.runtime-log-flow__item--completed .runtime-log-flow__marker {
  border-color: var(--success);
}

.runtime-log-flow__content {
  min-width: 0;
}

.runtime-log-flow__header,
.runtime-log-flow__source {
  display: flex;
  min-width: 0;
  align-items: center;
  flex-wrap: wrap;
  gap: 7px;
}

.runtime-log-flow__header time {
  color: var(--text-secondary);
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", monospace;
  font-size: 11px;
}

.runtime-log-flow__metric {
  color: var(--text-secondary);
  font-size: 11px;
}

.runtime-log-flow__failed-stage {
  color: var(--danger);
  font-size: 11px;
  font-weight: 700;
}

.runtime-log-flow__message {
  margin: 7px 0 5px;
  line-height: 1.55;
  overflow-wrap: anywhere;
}

.runtime-log-flow__source {
  color: var(--text-secondary);
  font-size: 11px;
}

.runtime-log-flow__source code {
  min-width: 0;
  overflow-wrap: anywhere;
}

.runtime-log-flow__error-context {
  margin-top: 10px;
}

.runtime-log-flow__error-context strong {
  display: block;
  margin-bottom: 5px;
  color: var(--danger);
  font-size: 12px;
}

.runtime-log-flow__error-context p {
  margin: 7px 0 0;
  color: var(--text-secondary);
  font-size: 12px;
}

.runtime-log-flow__error-context code {
  color: var(--danger);
  overflow-wrap: anywhere;
}

.runtime-log-flow__error-context pre {
  color: var(--danger);
  background: var(--el-color-danger-light-9, #fef3f2);
  border-color: var(--el-color-danger-light-7, #fecdca);
}

.runtime-log-flow__context {
  margin-top: 9px;
}

.runtime-log-flow__context summary {
  width: fit-content;
  color: var(--primary);
  font-size: 12px;
  cursor: pointer;
}

.runtime-log-flow__context pre {
  max-height: 240px;
  margin-top: 7px;
}

@media (max-width: 600px) {
  .runtime-log-detail__meta {
    grid-template-columns: minmax(0, 1fr);
  }

  .runtime-log-detail__meta > div {
    grid-template-columns: 76px minmax(0, 1fr);
    border-right: 0 !important;
    border-bottom: 1px solid var(--border);
  }

  .runtime-log-detail__meta-wide {
    grid-column: auto;
    border-top: 0;
    border-bottom: 0 !important;
  }

  .runtime-log-detail__section > pre {
    max-height: 34vh;
    max-height: 34dvh;
    padding: 10px;
    font-size: 11px;
  }

  .runtime-log-flow {
    max-height: 52vh;
    max-height: 52dvh;
    padding-left: 9px;
  }

  .runtime-log-flow__item {
    padding-left: 18px;
  }

  .runtime-log-flow__header {
    align-items: flex-start;
  }

  .runtime-log-flow__header time {
    width: 100%;
  }

  .runtime-log-flow pre {
    padding: 10px;
    font-size: 11px;
  }
}
</style>
