<template>
  <div class="section-header">
    <div class="section-header__copy">
      <component :is="level" class="section-header__title">{{ title }}</component>
      <p v-if="description">{{ description }}</p>
    </div>
    <div v-if="$slots.actions" class="section-header__actions">
      <slot name="actions" />
    </div>
  </div>
</template>

<script setup>
defineProps({
  title: { type: String, required: true },
  description: { type: String, default: "" },
  level: { type: String, default: "h2" },
});
</script>
