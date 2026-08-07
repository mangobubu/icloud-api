<template>
  <div class="empty-state">
    <el-icon :size="28" aria-hidden="true"><MessageBox /></el-icon>
    <component :is="level" class="empty-state__title">{{ title }}</component>
    <p v-if="description">{{ description }}</p>
    <div v-if="$slots.default" class="empty-state__action"><slot /></div>
  </div>
</template>

<script setup>
import { MessageBox } from "@element-plus/icons-vue";

defineProps({
  title: { type: String, required: true },
  description: { type: String, default: "" },
  level: { type: String, default: "h2" },
});
</script>
