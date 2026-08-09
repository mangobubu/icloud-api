<template>
  <el-dialog
    :model-value="modelValue"
    class="runtime-log-detail"
    title="日志详情"
    width="min(760px, calc(100vw - 28px))"
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
        <el-tooltip content="复制完整日志" placement="top">
          <el-button
            :icon="CopyDocument"
            circle
            :loading="copying"
            aria-label="复制完整日志"
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
          <dd>{{ log.accountId ? `#${log.accountId}` : "-" }}</dd>
        </div>
        <div class="runtime-log-detail__meta-wide">
          <dt>请求编号</dt>
          <dd><code>{{ log.requestId || "-" }}</code></dd>
        </div>
      </dl>

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

    <template #footer>
      <el-button @click="emit('update:modelValue', false)">关闭</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { CopyDocument } from "@element-plus/icons-vue";
import { computed, onBeforeUnmount, ref, watch } from "vue";

import { copyText } from "../utils/clipboard.js";
import { formatTime } from "../utils/format.js";
import {
  runtimeLogAttributesText,
  runtimeLogLevelMeta,
} from "../utils/runtimeLogs.js";

const props = defineProps({
  modelValue: { type: Boolean, default: false },
  log: { type: Object, default: null },
});
const emit = defineEmits(["update:modelValue"]);

const copying = ref(false);
const copyFeedback = ref("");
let feedbackTimer = null;

const levelMeta = computed(() => runtimeLogLevelMeta(props.log?.level));
const attributesText = computed(() =>
  runtimeLogAttributesText(props.log?.attributes),
);
const fullLogText = computed(() => {
  if (!props.log) return "";
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
  if (copying.value || !fullLogText.value) return;
  copying.value = true;
  const copied = await copyText(fullLogText.value);
  copying.value = false;

  const message = copied ? "完整日志已复制" : "日志复制失败，请手动复制";
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

.runtime-log-detail__section h3 {
  margin: 0 0 7px;
  font-size: 13px;
  font-weight: 700;
}

.runtime-log-detail__section pre {
  box-sizing: border-box;
  width: 100%;
  max-width: 100%;
  max-height: min(42vh, 380px);
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

  .runtime-log-detail__section pre {
    max-height: 34vh;
    max-height: 34dvh;
    padding: 10px;
    font-size: 11px;
  }
}
</style>
