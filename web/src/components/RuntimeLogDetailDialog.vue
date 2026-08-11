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
        <el-tooltip v-if="hasFlow" :content="refreshTooltip" placement="top">
          <el-button
            :icon="Refresh"
            circle
            :loading="flowLoading"
            :aria-label="refreshTooltip"
            @click="emit('retry-flow')"
          />
        </el-tooltip>
        <el-tooltip :content="copyTooltip" placement="top">
          <el-button
            :icon="CopyDocument"
            circle
            :loading="copying"
            :disabled="hasFlow && flowLoading"
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
        <div v-if="hasFlow" class="runtime-log-detail__meta-wide">
          <dt>{{ flowIdentifierLabel }}</dt>
          <dd><code>{{ flowRunId }}</code></dd>
        </div>
        <div v-else class="runtime-log-detail__meta-wide">
          <dt>请求编号</dt>
          <dd><code>{{ log.requestId || "-" }}</code></dd>
        </div>
      </dl>

      <section
        v-if="hasFlow"
        class="runtime-log-detail__section runtime-log-flow-section"
        aria-labelledby="runtime-log-flow-title"
      >
        <div class="runtime-log-detail__section-heading">
          <h3 id="runtime-log-flow-title">{{ flowSectionTitle }}</h3>
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
          <span class="sr-only">{{ flowLoadingText }}</span>
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
            :title="flowRunningText"
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
                'runtime-log-flow__item--deferred': isDeferredFlowEntry(entry),
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
                    {{ flowStageLabel(entry) }}
                  </el-tag>
                  <span v-if="flowPercent(entry) !== null" class="runtime-log-flow__metric">
                    {{ Math.round(flowPercent(entry)) }}%
                  </span>
                  <span
                    v-if="entry.elapsedMs !== null && entry.elapsedMs !== undefined"
                    class="runtime-log-flow__metric"
                  >
                    已耗时 {{ elapsedLabel(entry.elapsedMs) }}
                  </span>
                  <span
                    v-if="batchElapsedMs(entry) !== null"
                    class="runtime-log-flow__metric"
                  >
                    批次耗时 {{ elapsedLabel(batchElapsedMs(entry)) }}
                  </span>
                  <span
                    v-if="hasSyncFlow && entry.syncBatch !== null"
                    class="runtime-log-flow__metric"
                  >
                    第 {{ entry.syncBatch }} 批
                  </span>
                  <span v-if="entry.failedStage" class="runtime-log-flow__failed-stage">
                    失败于 {{ stageLabel(entry.failedStage) }}
                  </span>
                </header>

                <p class="runtime-log-flow__message">{{ entry.message || "-" }}</p>

                <div class="runtime-log-flow__source">
                  <span>{{ runtimeLogLevelMeta(entry.level).label }}</span>
                  <code>{{ entry.source || "system" }}</code>
                  <code v-if="entry.requestId">请求 {{ entry.requestId }}</code>
                </div>

                <dl
                  v-if="hasDiagnosticDetails(entry)"
                  class="runtime-log-flow__diagnostics"
                >
                  <div v-if="entry.errorCode">
                    <dt>诊断码</dt>
                    <dd><code>{{ entry.errorCode }}</code></dd>
                  </div>
                  <div v-if="entry.errorClass">
                    <dt>错误类别</dt>
                    <dd>{{ errorClassLabel(entry.errorClass) }}</dd>
                  </div>
                  <div v-if="entry.causeCategory">
                    <dt>原因分类</dt>
                    <dd>{{ causeCategoryLabel(entry.causeCategory) }}</dd>
                  </div>
                  <div v-if="entry.errorContext">
                    <dt>失败原因</dt>
                    <dd>{{ entry.errorContext }}</dd>
                  </div>
                  <div v-if="entry.failedStage">
                    <dt>失败步骤</dt>
                    <dd>{{ stageLabel(entry.failedStage) }}</dd>
                  </div>
                  <div v-if="entry.failedOperation">
                    <dt>失败操作</dt>
                    <dd><code>{{ entry.failedOperation }}</code></dd>
                  </div>
                  <div v-if="entry.operation">
                    <dt>Apple 操作</dt>
                    <dd><code>{{ entry.operation }}</code></dd>
                  </div>
                  <div v-if="entry.httpStatus !== null && entry.httpStatus !== undefined">
                    <dt>HTTP 状态</dt>
                    <dd><code>{{ entry.httpStatus }}</code></dd>
                  </div>
                  <div
                    v-if="
                      (entry.retryable !== null && entry.retryable !== undefined) ||
                      (entry.upstreamRetryable !== null && entry.upstreamRetryable !== undefined)
                    "
                  >
                    <dt>可重试</dt>
                    <dd>{{ retryLabel(entry) }}</dd>
                  </div>
                  <div v-if="entry.elapsedMs !== null && entry.elapsedMs !== undefined">
                    <dt>已耗时</dt>
                    <dd>{{ elapsedLabel(entry.elapsedMs) }}</dd>
                  </div>
                  <div v-if="entry.scheduleAction">
                    <dt>计划动作</dt>
                    <dd>{{ scheduleActionLabel(entry.scheduleAction) }}</dd>
                  </div>
                  <div v-if="entry.scheduledFor">
                    <dt>计划时间</dt>
                    <dd>{{ timestampLabel(entry.scheduledFor) }}</dd>
                  </div>
                  <div v-if="entry.attemptedAt">
                    <dt>实际执行</dt>
                    <dd>{{ timestampLabel(entry.attemptedAt) }}</dd>
                  </div>
                  <div v-if="entry.nextRunAt">
                    <dt>下次执行</dt>
                    <dd>{{ timestampLabel(entry.nextRunAt) }}</dd>
                  </div>
                  <div
                    v-if="entry.remoteSideEffectPossible !== null && entry.remoteSideEffectPossible !== undefined"
                  >
                    <dt>可能已产生远端变更</dt>
                    <dd>{{ booleanLabel(entry.remoteSideEffectPossible) }}</dd>
                  </div>
                  <div
                    v-if="entry.pendingConfirmation !== null && entry.pendingConfirmation !== undefined"
                  >
                    <dt>待确认</dt>
                    <dd>{{ booleanLabel(entry.pendingConfirmation) }}</dd>
                  </div>
                  <div
                    v-if="entry.failureStateRecorded !== null && entry.failureStateRecorded !== undefined"
                  >
                    <dt>失败状态</dt>
                    <dd>{{ booleanLabel(entry.failureStateRecorded) }}</dd>
                  </div>
                  <div
                    v-if="entry.autoCreationDisabled !== null && entry.autoCreationDisabled !== undefined"
                  >
                    <dt>自动创建</dt>
                    <dd>{{ entry.autoCreationDisabled ? "已关闭" : "仍启用" }}</dd>
                  </div>
                  <div v-if="resultStateRecorded(entry) !== null">
                    <dt>计划状态写回</dt>
                    <dd>{{ booleanLabel(resultStateRecorded(entry)) }}</dd>
                  </div>
                  <div
                    v-if="entry.confirmationAttempt !== null && entry.confirmationAttempt !== undefined"
                  >
                    <dt>确认尝试</dt>
                    <dd>第 {{ entry.confirmationAttempt }} 次</dd>
                  </div>
                  <div v-if="entry.serviceCodeFingerprint">
                    <dt>服务码指纹</dt>
                    <dd><code>{{ entry.serviceCodeFingerprint }}</code></dd>
                  </div>
                </dl>

                <div
                  v-if="entry.errorDetail || entry.failedOperation"
                  class="runtime-log-flow__error-context"
                >
                  <template v-if="entry.errorDetail">
                    <strong>错误详情</strong>
                    <pre>{{ entry.errorDetail }}</pre>
                  </template>
                  <template
                    v-if="entry.errorContext && entry.errorContext !== entry.errorDetail"
                  >
                    <strong>失败原因</strong>
                    <pre>{{ entry.errorContext }}</pre>
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

          <el-empty v-else :description="flowEmptyText" :image-size="72" />
        </template>
      </section>

      <template v-else>
        <section class="runtime-log-detail__section" aria-labelledby="runtime-log-message-title">
          <h3 id="runtime-log-message-title">消息</h3>
          <pre>{{ log.message || "-" }}</pre>
        </section>

        <section
          v-if="diagnosticText(log)"
          class="runtime-log-detail__section"
          aria-labelledby="runtime-log-diagnostics-title"
        >
          <h3 id="runtime-log-diagnostics-title">失败诊断</h3>
          <pre>{{ diagnosticText(log) }}</pre>
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
  runtimeLogAutoCreateStageLabel,
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

const hasAutoCreateFlow = computed(() => Boolean(props.log?.autoCreateRunId));
const hasSyncFlow = computed(() =>
  Boolean(props.log?.syncRunId) && !hasAutoCreateFlow.value,
);
const hasFlow = computed(() => hasAutoCreateFlow.value || hasSyncFlow.value);
const dialogTitle = computed(() => {
  if (hasAutoCreateFlow.value) return "自动创建流程详情";
  return hasSyncFlow.value ? "同步流程详情" : "日志详情";
});
const flowRunId = computed(() =>
  hasAutoCreateFlow.value ? props.log?.autoCreateRunId : props.log?.syncRunId,
);
const flowIdentifierLabel = computed(() =>
  hasAutoCreateFlow.value ? "创建编号" : "同步编号",
);
const flowSectionTitle = computed(() =>
  hasAutoCreateFlow.value ? "自动创建流程" : "同步流程",
);
const refreshTooltip = computed(() =>
  hasAutoCreateFlow.value ? "刷新自动创建流程" : "刷新同步流程",
);
const flowLoadingText = computed(() =>
  hasAutoCreateFlow.value
    ? "正在加载完整自动创建流程"
    : "正在加载完整同步流程",
);
const flowRunningText = computed(() =>
  hasAutoCreateFlow.value
    ? "该次自动创建尚未结束，重新打开或点击重新加载可查看后续步骤。"
    : "该次同步尚未结束，重新打开或点击重新加载可查看后续步骤。",
);
const flowEmptyText = computed(() =>
  hasAutoCreateFlow.value
    ? "该次自动创建暂无可用的流程日志"
    : "该次同步暂无可用的流程日志",
);
const copyTooltip = computed(() => {
  if (hasFlow.value && props.flowLoading) return "流程加载完成后可复制";
  if (hasAutoCreateFlow.value) return "复制完整自动创建流程";
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
  const parts = [
    `${orderedFlowLogs.value.length} 条记录`,
    hasAutoCreateFlow.value ? "自动创建" : syncTriggerLabel.value,
  ];
  if (flowDuration.value) parts.push(`耗时 ${flowDuration.value}`);
  return parts.join(" · ");
});
const flowIsPartial = computed(() => {
  if (props.flowError || !orderedFlowLogs.value.length) return false;
  const firstEntry = orderedFlowLogs.value[0];
  const firstEvent = flowEvent(firstEntry);
  const hasStart = ["started", "run_started", "run_queued"].includes(firstEvent);
  const isDeferredOnlyStart =
    firstEvent === "run_deferred" || flowStage(firstEntry) === "deferred";
  return !hasStart && !isDeferredOnlyStart;
});
const flowIsRunning = computed(() => {
  if (props.flowError || flowIsPartial.value || !orderedFlowLogs.value.length) return false;
  const lastEntry = orderedFlowLogs.value.at(-1);
  const lastEvent = flowEvent(lastEntry);
  const hasTerminal = [
    "completed",
    "run_completed",
    "run_completed_with_warning",
    "run_deferred",
    "failed",
    "run_failed",
    "cancelled",
    "run_cancelled",
  ].includes(lastEvent) || ["completed", "deferred", "failed", "cancelled"].includes(flowStage(lastEntry));
  return !hasTerminal;
});

function flowStage(entry) {
  return hasAutoCreateFlow.value
    ? entry?.autoCreateStage
    : entry?.syncStage;
}

function flowEvent(entry) {
  return hasAutoCreateFlow.value
    ? entry?.autoCreateEvent
    : entry?.syncEvent;
}

function flowPercent(entry) {
  const value = hasAutoCreateFlow.value
    ? entry?.autoCreatePercent
    : entry?.syncPercent;
  return value === null || value === undefined ? null : Number(value);
}

function stageLabel(stage) {
  return hasAutoCreateFlow.value
    ? runtimeLogAutoCreateStageLabel(stage)
    : runtimeLogSyncStageLabel(stage);
}

function flowStageLabel(entry) {
  return stageLabel(flowStage(entry));
}

function flowContextText(entry) {
  return runtimeLogFlowContextText(entry);
}

function booleanLabel(value) {
  if (value === true) return "是";
  if (value === false) return "否";
  return "-";
}

function diagnosticAttributeName(value) {
  return String(value ?? "")
    .trim()
    .replace(/([A-Z]+)([A-Z][a-z])/g, "$1_$2")
    .replace(/([a-z0-9])([A-Z])/g, "$1_$2")
    .replaceAll("-", "_")
    .toLowerCase();
}

function diagnosticAttributeLeaf(value) {
  const text = String(value ?? "");
  let leafStart = 0;
  let escaped = false;
  for (let index = 0; index < text.length; index += 1) {
    const character = text[index];
    if (escaped) {
      escaped = false;
      continue;
    }
    if (character === "\\") {
      escaped = true;
      continue;
    }
    if (character === ".") leafStart = index + 1;
  }
  return text
    .slice(leafStart)
    .replaceAll("\\.", ".")
    .replaceAll("\\\\", "\\");
}

function hasDiagnosticAttributeValue(value) {
  return !(
    value === undefined ||
    value === null ||
    (typeof value === "string" && value.trim() === "")
  );
}

function diagnosticAttribute(entry, ...names) {
  for (const name of names) {
    const direct = entry?.[name];
    if (hasDiagnosticAttributeValue(direct)) return direct;
  }

  const normalizedNames = new Set(names.map(diagnosticAttributeName));
  for (const attributes of [entry?.attributes, entry?.fields]) {
    if (!attributes || typeof attributes !== "object" || Array.isArray(attributes)) {
      continue;
    }
    for (const [key, value] of Object.entries(attributes)) {
      const attributeName = diagnosticAttributeName(diagnosticAttributeLeaf(key));
      if (
        normalizedNames.has(attributeName) &&
        hasDiagnosticAttributeValue(value)
      ) {
        return value;
      }
    }
  }
  return undefined;
}

function diagnosticBoolean(entry, ...names) {
  const value = diagnosticAttribute(entry, ...names);
  if (value === null || value === undefined || value === "") return null;
  if (typeof value === "boolean") return value;
  const normalized = String(value).trim().toLowerCase();
  if (["true", "1", "yes"].includes(normalized)) return true;
  if (["false", "0", "no"].includes(normalized)) return false;
  return null;
}

function resultStateRecorded(entry) {
  return diagnosticBoolean(
    entry,
    "resultStateRecorded",
    "result_state_recorded",
    "ResultStateRecorded",
  );
}

function batchElapsedMs(entry) {
  const value = diagnosticAttribute(
    entry,
    "batchElapsedMs",
    "batch_elapsed_ms",
    "BatchElapsedMs",
  );
  if (
    value === null ||
    value === undefined ||
    value === "" ||
    typeof value === "boolean"
  ) {
    return null;
  }
  const milliseconds = Number(value);
  if (!Number.isFinite(milliseconds) || milliseconds < 0) return null;
  return Math.trunc(milliseconds);
}

function retryLabel(entry) {
  const upstream = entry?.upstreamRetryable;
  const retryable = entry?.retryable;
  if (upstream !== null && upstream !== undefined) {
    return `${booleanLabel(retryable)}（Apple：${booleanLabel(upstream)}）`;
  }
  return booleanLabel(retryable);
}

function elapsedLabel(value) {
  const milliseconds = Number(value);
  if (!Number.isFinite(milliseconds) || milliseconds < 0) return "-";
  if (milliseconds < 1000) return `${Math.round(milliseconds)} 毫秒`;
  const seconds = milliseconds / 1000;
  if (seconds < 60) return `${seconds.toFixed(seconds < 10 ? 1 : 0)} 秒`;
  const minutes = Math.floor(seconds / 60);
  const remainder = Math.round(seconds % 60);
  return remainder ? `${minutes} 分 ${remainder} 秒` : `${minutes} 分钟`;
}

function timestampLabel(value) {
  const formatted = formatTime(value, { seconds: true });
  return formatted === "-" ? String(value || "-") : formatted;
}

function scheduleActionLabel(value) {
  switch (String(value || "").toLowerCase()) {
    case "continue":
      return "继续按计划执行";
    case "disabled":
      return "已关闭自动创建";
    case "none":
      return "没有后续计划";
    default:
      return String(value || "-");
  }
}

function errorClassLabel(value) {
  const normalized = String(value || "").toLowerCase();
  const labels = {
    account_state: "主号状态",
    apple_upstream: "Apple 服务",
    capacity: "容量限制",
    context: "任务上下文",
    crypto: "本地密钥",
    internal: "本地处理",
    persistence: "本地持久化",
    schedule: "计划调度",
    session: "Apple 会话",
  };
  const label = labels[normalized];
  return label ? `${label}（${normalized}）` : String(value || "-");
}

function causeCategoryLabel(value) {
  const normalized = String(value || "").toLowerCase();
  const labels = {
    account_state: "主号或归属状态",
    apple_upstream: "Apple 服务响应",
    capacity: "容量限制",
    context: "任务取消或超时",
    crypto: "本地密钥处理",
    internal: "本地处理",
    persistence: "数据库写入",
    plan_conflict: "计划并发冲突",
    schedule: "计划调度",
    session: "Apple 登录会话",
  };
  const label = labels[normalized];
  return label ? `${label}（${normalized}）` : String(value || "-");
}

function diagnosticLines(entry) {
  if (!entry) return [];
  const lines = [];
  if (entry.errorCode) lines.push(`诊断码: ${entry.errorCode}`);
  if (entry.errorClass) lines.push(`错误类别: ${errorClassLabel(entry.errorClass)}`);
  if (entry.causeCategory) lines.push(`原因分类: ${causeCategoryLabel(entry.causeCategory)}`);
  if (entry.errorContext) lines.push(`失败原因: ${entry.errorContext}`);
  if (entry.failedStage) lines.push(`失败步骤: ${stageLabel(entry.failedStage)}`);
  if (entry.failedOperation) lines.push(`失败操作: ${entry.failedOperation}`);
  if (entry.operation) lines.push(`Apple 操作: ${entry.operation}`);
  if (entry.httpStatus !== null && entry.httpStatus !== undefined) {
    lines.push(`HTTP 状态: ${entry.httpStatus}`);
  }
  if (entry.retryable !== null && entry.retryable !== undefined) {
    lines.push(`可重试: ${retryLabel(entry)}`);
  } else if (entry.upstreamRetryable !== null && entry.upstreamRetryable !== undefined) {
    lines.push(`Apple 可重试: ${booleanLabel(entry.upstreamRetryable)}`);
  }
  if (entry.elapsedMs !== null && entry.elapsedMs !== undefined) {
    lines.push(`已耗时: ${elapsedLabel(entry.elapsedMs)}`);
  }
  const batchElapsed = batchElapsedMs(entry);
  if (batchElapsed !== null) lines.push(`批次耗时: ${elapsedLabel(batchElapsed)}`);
  if (entry.scheduleAction) {
    lines.push(`计划动作: ${scheduleActionLabel(entry.scheduleAction)}`);
  }
  if (entry.scheduledFor) lines.push(`计划时间: ${timestampLabel(entry.scheduledFor)}`);
  if (entry.attemptedAt) lines.push(`实际执行: ${timestampLabel(entry.attemptedAt)}`);
  if (entry.nextRunAt) lines.push(`下次执行: ${timestampLabel(entry.nextRunAt)}`);
  if (entry.remoteSideEffectPossible !== null && entry.remoteSideEffectPossible !== undefined) {
    lines.push(`可能已产生远端变更: ${booleanLabel(entry.remoteSideEffectPossible)}`);
  }
  if (entry.pendingConfirmation !== null && entry.pendingConfirmation !== undefined) {
    lines.push(`待确认: ${booleanLabel(entry.pendingConfirmation)}`);
  }
  if (entry.failureStateRecorded !== null && entry.failureStateRecorded !== undefined) {
    lines.push(`失败状态已写入: ${booleanLabel(entry.failureStateRecorded)}`);
  }
  if (entry.autoCreationDisabled !== null && entry.autoCreationDisabled !== undefined) {
    lines.push(`自动创建计划: ${entry.autoCreationDisabled ? "已关闭" : "仍启用"}`);
  }
  const resultRecorded = resultStateRecorded(entry);
  if (resultRecorded !== null) {
    lines.push(`计划状态写回: ${booleanLabel(resultRecorded)}`);
  }
  if (entry.confirmationAttempt !== null && entry.confirmationAttempt !== undefined) {
    lines.push(`确认尝试: 第 ${entry.confirmationAttempt} 次`);
  }
  if (entry.serviceCodeFingerprint) {
    lines.push(`服务码指纹: ${entry.serviceCodeFingerprint}`);
  }
  return lines;
}

function diagnosticText(entry) {
  return diagnosticLines(entry).join("\n");
}

function hasDiagnosticDetails(entry) {
  if (!entry) return false;
  return Boolean(
    entry.errorCode ||
      entry.errorClass ||
      entry.causeCategory ||
      entry.errorContext ||
      entry.failedStage ||
      entry.failedOperation ||
      entry.operation ||
      (entry.httpStatus !== null && entry.httpStatus !== undefined) ||
      (entry.retryable !== null && entry.retryable !== undefined) ||
      (entry.upstreamRetryable !== null && entry.upstreamRetryable !== undefined) ||
      entry.scheduleAction ||
      entry.scheduledFor ||
      entry.attemptedAt ||
      entry.nextRunAt ||
      batchElapsedMs(entry) !== null ||
      (entry.remoteSideEffectPossible !== null && entry.remoteSideEffectPossible !== undefined) ||
      (entry.pendingConfirmation !== null && entry.pendingConfirmation !== undefined) ||
      (entry.failureStateRecorded !== null && entry.failureStateRecorded !== undefined) ||
      (entry.autoCreationDisabled !== null && entry.autoCreationDisabled !== undefined) ||
      resultStateRecorded(entry) !== null ||
      (entry.confirmationAttempt !== null && entry.confirmationAttempt !== undefined) ||
      entry.serviceCodeFingerprint,
  );
}

function isFailedFlowEntry(entry) {
  return (
    entry?.level === "error" ||
    ["failed", "run_failed"].includes(flowEvent(entry)) ||
    flowStage(entry) === "failed" ||
    Boolean(entry?.errorDetail)
  );
}

function isCompletedFlowEntry(entry) {
  return (
    ["completed", "run_completed", "run_completed_with_warning"].includes(
      flowEvent(entry),
    ) || flowStage(entry) === "completed"
  );
}

function isDeferredFlowEntry(entry) {
  return flowEvent(entry) === "run_deferred" || flowStage(entry) === "deferred";
}

function flowLogText() {
  const lines = hasAutoCreateFlow.value
    ? [
        "自动创建流程",
        `创建编号: ${flowRunId.value}`,
        `主号: ${accountDisplayLabel.value}`,
        `流程记录: ${orderedFlowLogs.value.length} 条`,
      ]
    : [
        `同步编号: ${flowRunId.value}`,
        `主号: ${accountDisplayLabel.value}`,
        `触发方式: ${syncTriggerLabel.value}`,
        `流程记录: ${orderedFlowLogs.value.length} 条`,
      ];
  if (flowDuration.value) lines.push(`耗时: ${flowDuration.value}`);

  for (const entry of orderedFlowLogs.value) {
    const stage = flowStageLabel(entry);
    const metrics = [];
    const percent = flowPercent(entry);
    if (percent !== null) metrics.push(`${Math.round(percent)}%`);
    if (hasSyncFlow.value && entry.syncBatch !== null) {
      metrics.push(`第 ${entry.syncBatch} 批`);
    }
    lines.push(
      "",
      `[${formatTime(entry.time, { seconds: true })}] ${stage}${metrics.length ? ` (${metrics.join("，")})` : ""}`,
      `级别: ${String(entry.level || "info").toUpperCase()}`,
      `来源: ${entry.source || "system"}`,
      `消息: ${entry.message || "-"}`,
    );
    if (entry.requestId) lines.push(`请求编号: ${entry.requestId}`);
    if (entry.failedStage) {
      lines.push(`失败步骤: ${stageLabel(entry.failedStage)}`);
    }
    if (entry.errorDetail) lines.push(`错误详情: ${entry.errorDetail}`);
    if (entry.failedOperation) lines.push(`操作位置: ${entry.failedOperation}`);
    const diagnostics = diagnosticLines(entry).filter(
      (line) => !line.startsWith("失败步骤:") && !line.startsWith("失败操作:"),
    );
    if (diagnostics.length) lines.push("失败诊断:", ...diagnostics);
    const context = flowContextText(entry);
    if (context) lines.push("步骤上下文:", context);
  }
  return lines.join("\n");
}

const fullLogText = computed(() => {
  if (!props.log) return "";
  if (hasFlow.value) return flowLogText();

  const lines = [
    `时间: ${formatTime(props.log.time, { seconds: true })}`,
    `级别: ${String(props.log.level || "info").toUpperCase()}`,
    `来源: ${props.log.source || "system"}`,
  ];
  if (props.log.accountId) lines.push(`主号: #${props.log.accountId}`);
  if (props.log.requestId) lines.push(`请求编号: ${props.log.requestId}`);
  lines.push("", "消息:", props.log.message || "-");
  const diagnostics = diagnosticText(props.log);
  if (diagnostics) lines.push("", "失败诊断:", diagnostics);
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
    ? hasAutoCreateFlow.value
      ? "完整自动创建流程已复制"
      : hasSyncFlow.value
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

.runtime-log-flow__item--deferred .runtime-log-flow__marker {
  border-color: var(--warning);
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

.runtime-log-flow__diagnostics {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 0 12px;
  margin: 10px 0 0;
  padding: 8px 10px;
  background: var(--surface-subtle);
  border: 1px solid var(--border);
  border-radius: 4px;
}

.runtime-log-flow__diagnostics > div {
  display: grid;
  min-width: 0;
  grid-template-columns: max-content minmax(0, 1fr);
  gap: 8px;
  align-items: baseline;
  padding: 4px 0;
}

.runtime-log-flow__diagnostics dt {
  color: var(--text-secondary);
  font-size: 11px;
  white-space: nowrap;
}

.runtime-log-flow__diagnostics dd {
  min-width: 0;
  margin: 0;
  overflow-wrap: anywhere;
  font-size: 12px;
}

.runtime-log-flow__diagnostics code {
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

  .runtime-log-flow__diagnostics {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
