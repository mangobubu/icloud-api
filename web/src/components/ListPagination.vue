<template>
  <nav
    v-if="total > 0"
    class="list-pagination"
    :aria-label="ariaLabel"
    aria-live="polite"
  >
    <span class="list-pagination__summary">共 {{ total }} 条</span>
    <el-pagination
      background
      layout="prev, pager, next"
      :current-page="page"
      :page-size="pageSize"
      :total="total"
      :disabled="loading"
      @current-change="emit('change', $event)"
    />
  </nav>
</template>

<script setup>
defineProps({
  page: { type: Number, required: true },
  pageSize: { type: Number, default: 50 },
  total: { type: Number, required: true },
  loading: { type: Boolean, default: false },
  ariaLabel: { type: String, default: "列表分页" },
});

const emit = defineEmits(["change"]);
</script>
