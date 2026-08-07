<template>
  <el-alert
    v-if="visible"
    class="request-alert"
    :type="type"
    :title="displayMessage"
    :closable="closable"
    show-icon
    role="status"
    @close="$emit('close')"
  >
    <p v-if="requestId" class="request-alert__id">
      请求编号：<code>{{ requestId }}</code>
    </p>
  </el-alert>
</template>

<script setup>
import { computed } from "vue";

const props = defineProps({
  error: { type: Object, default: null },
  message: { type: String, default: "" },
  requestId: { type: String, default: "" },
  type: { type: String, default: "error" },
  closable: { type: Boolean, default: false },
});

defineEmits(["close"]);

const displayMessage = computed(
  () => props.message || props.error?.message || "请求处理失败，请稍后重试。",
);
const requestId = computed(() => props.requestId || props.error?.requestId || "");
const visible = computed(() => Boolean(props.message || props.error));
</script>
