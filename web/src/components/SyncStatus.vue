<template>
  <div class="sync-status">
    <el-tag :type="status.type" effect="plain" size="small" round>
      <span class="sync-status__dot" aria-hidden="true"></span>
      {{ status.label }}
    </el-tag>
    <template v-if="showDetails">
      <small v-if="item.lastSyncError" class="sync-status__error">
        错误：{{ compactRunes(item.lastSyncError) }}
      </small>
      <small v-if="item.lastSyncedAt">
        {{ item.lastSyncStatus === "error" ? "尝试于" : "同步于" }}
        {{ formatTime(item.lastSyncedAt) }}
      </small>
    </template>
  </div>
</template>

<script setup>
import { computed } from "vue";

import { compactRunes, formatTime } from "../utils/format.js";

const props = defineProps({
  item: { type: Object, required: true },
  details: { type: Boolean, default: false },
});

const status = computed(() => {
  if (!props.item.enabled) {
    return { label: "已停用", type: "info" };
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
