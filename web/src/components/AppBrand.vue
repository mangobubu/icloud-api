<template>
  <div class="app-brand">
    <span class="app-brand__mark" aria-hidden="true">iC</span>
    <span class="app-brand__copy">
      <strong>{{ title }}</strong>
      <small>{{ subtitle }}</small>
    </span>
  </div>
</template>

<script setup>
defineProps({
  title: { type: String, default: "隐私邮箱" },
  subtitle: { type: String, default: "收件 API 管理" },
});
</script>
