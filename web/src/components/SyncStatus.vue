<template>
  <div class="sync-status">
    <el-tag :type="status.type" effect="plain" size="small" round>
      <span
        class="sync-status__dot"
        :class="{ 'sync-status__dot--active': progress.active }"
        aria-hidden="true"
      ></span>
      {{ status.label }}
    </el-tag>
    <div
      v-if="progress.active"
      class="sync-status__progress"
      role="status"
      :aria-label="progressAriaLabel"
    >
      <div class="sync-status__progress-meta">
        <small>{{ progress.stageLabel }}</small>
        <small v-if="progress.percentage !== null">
          {{ Math.round(progress.percentage) }}%
        </small>
      </div>
      <el-progress
        :percentage="progress.percentage ?? 100"
        :indeterminate="progress.indeterminate"
        :aria-hidden="progress.indeterminate ? 'true' : undefined"
        :duration="2"
        :show-text="false"
        :stroke-width="4"
      />
    </div>
    <template v-if="showDetails">
      <small v-if="item.lastSyncError" class="sync-status__error">
        <span>错误：{{ compactRunes(item.lastSyncError) }}</span>
        <SyncErrorLogDialog
          :log="fullErrorLog"
          :account-id="item.accountId || item.id"
        />
      </small>
      <small v-if="item.lastSyncedAt">
        {{ item.lastSyncStatus === "error" ? "尝试于" : "同步于" }}
        {{ formatTime(item.lastSyncedAt, { seconds: true }) }}
      </small>
    </template>
  </div>
</template>

<script setup>
import { computed } from "vue";

import SyncErrorLogDialog from "./SyncErrorLogDialog.vue";
import { compactRunes, formatTime } from "../utils/format.js";
import { syncProgressPresentation } from "../utils/syncProgress.js";

const props = defineProps({
  item: { type: Object, required: true },
  details: { type: Boolean, default: false },
});

const progress = computed(() =>
  syncProgressPresentation(props.item.syncProgress),
);
const fullErrorLog = computed(
  () => props.item.lastSyncErrorLog || props.item.lastSyncError || "",
);
const progressAriaLabel = computed(() => {
  const parts = [progress.value.label, progress.value.stageLabel];
  if (progress.value.percentage !== null) {
    parts.push(`完成 ${Math.round(progress.value.percentage)}%`);
  }
  return parts.filter(Boolean).join("，");
});

const status = computed(() => {
  if (!props.item.enabled) {
    return { label: "已停用", type: "info" };
  }
  if (progress.value.active) {
    const stage = String(props.item.syncProgress?.stage || "").toLowerCase();
    return {
      label: progress.value.label,
      type: stage === "queued" || stage === "waiting" ? "warning" : "primary",
    };
  }
  if (props.item.lastSyncStatus === "ok") {
    return { label: "正常", type: "success" };
  }
  if (props.item.lastSyncStatus === "error") {
    return { label: "同步异常", type: "danger" };
  }
  return { label: "待同步", type: "warning" };
});

const showDetails = computed(
  () =>
    props.details &&
    props.item.enabled &&
    (props.item.lastSyncStatus === "ok" || props.item.lastSyncStatus === "error"),
);
</script>
