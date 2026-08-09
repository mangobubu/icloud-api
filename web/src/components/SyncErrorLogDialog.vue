<template>
  <span v-if="hasLog" class="sync-error-log-dialog">
    <el-button
      class="sync-error-log-dialog__trigger"
      link
      type="danger"
      @click="dialogVisible = true"
    >
      点击查看错误日志
    </el-button>

    <el-dialog
      v-model="dialogVisible"
      class="sync-error-log-dialog__window"
      :title="title"
      width="min(720px, calc(100vw - 28px))"
      append-to-body
      destroy-on-close
    >
      <div class="sync-error-log-dialog__toolbar">
        <span class="sync-error-log-dialog__feedback" role="status">
          {{ copyFeedback }}
        </span>
        <el-tooltip content="复制错误日志" placement="top">
          <el-button
            :icon="CopyDocument"
            circle
            :loading="copying"
            aria-label="复制错误日志"
            @click="copyLog"
          />
        </el-tooltip>
      </div>

      <pre class="sync-error-log-dialog__content">{{ logText }}</pre>

      <template #footer>
        <div class="dialog-actions dialog-actions--spread">
          <el-button :icon="Tickets" @click="openAllLogs">
            查看全部日志
          </el-button>
          <el-button @click="dialogVisible = false">关闭</el-button>
        </div>
      </template>
    </el-dialog>
  </span>
</template>

<script setup>
import { CopyDocument, Tickets } from "@element-plus/icons-vue";
import { computed, onBeforeUnmount, ref } from "vue";
import { useRouter } from "vue-router";

import { copyText } from "../utils/clipboard.js";

const props = defineProps({
  log: { type: String, default: "" },
  title: { type: String, default: "同步错误日志" },
  accountId: { type: [Number, String], default: null },
});

const router = useRouter();

const dialogVisible = ref(false);
const copying = ref(false);
const copyFeedback = ref("");
let feedbackTimer = null;

const logText = computed(() => String(props.log ?? ""));
const hasLog = computed(() => Boolean(logText.value.trim()));
const accountFilter = computed(() => {
  const value = String(props.accountId ?? "").trim();
  return /^\d+$/.test(value) && Number(value) > 0 ? value : "";
});

function openAllLogs() {
  dialogVisible.value = false;
  const query = accountFilter.value
    ? { account_id: accountFilter.value }
    : {};
  void router.push({ name: "logs", query });
}

async function copyLog() {
  if (copying.value || !hasLog.value) return;
  copying.value = true;
  const copied = await copyText(logText.value);
  copying.value = false;

  const message = copied
    ? "错误日志已复制"
    : "错误日志复制失败，请手动复制";
  copyFeedback.value = message;

  window.clearTimeout(feedbackTimer);
  feedbackTimer = window.setTimeout(() => {
    copyFeedback.value = "";
  }, 1800);
}

onBeforeUnmount(() => {
  window.clearTimeout(feedbackTimer);
});
</script>

<style scoped>
.sync-error-log-dialog {
  display: inline-flex;
  align-items: center;
  min-width: 0;
}

.sync-error-log-dialog__trigger {
  height: auto;
  padding: 0;
  font-size: inherit;
  vertical-align: baseline;
}

.sync-error-log-dialog__toolbar {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  min-width: 0;
  min-height: 32px;
  margin-bottom: 10px;
  gap: 8px;
}

.sync-error-log-dialog__feedback {
  min-width: 0;
  color: var(--text-secondary, #5f6b76);
  font-size: 12px;
  overflow-wrap: anywhere;
}

.sync-error-log-dialog__content {
  box-sizing: border-box;
  width: 100%;
  max-width: 100%;
  max-height: min(58vh, 520px);
  margin: 0;
  padding: 14px;
  overflow: auto;
  color: var(--text-primary, #20262d);
  font-family: ui-monospace, SFMono-Regular, Consolas, "Liberation Mono", monospace;
  font-size: 12px;
  line-height: 1.6;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  word-break: break-word;
  background: var(--surface-muted, #f6f7f8);
  border: 1px solid var(--border, #dfe3e6);
  border-radius: 4px;
}

@media (max-width: 600px) {
  .sync-error-log-dialog__content {
    max-height: 55vh;
    max-height: 55dvh;
    padding: 10px;
    font-size: 11px;
  }
}
</style>
