<template>
  <nav
    v-if="total > 0"
    class="list-pagination"
    :aria-label="ariaLabel"
    aria-live="polite"
  >
    <span class="list-pagination__summary">共 {{ total }} 条</span>
    <el-select
      class="list-pagination__size"
      :model-value="pageSize"
      :disabled="loading"
      aria-label="每页条数"
      @change="handleSizeChange"
    >
      <el-option
        v-for="option in pageSizeOptions"
        :key="option.value"
        :label="option.label"
        :value="option.value"
      />
    </el-select>
    <el-pagination
      v-if="pageSize > 0"
      background
      layout="prev, pager, next"
      :pager-count="5"
      :current-page="page"
      :page-size="pageSize"
      :total="total"
      :disabled="loading"
      @current-change="emit('change', $event)"
    />
  </nav>
</template>

<script setup>
const pageSizeOptions = [
  { label: "20 条/页", value: 20 },
  { label: "50 条/页", value: 50 },
  { label: "100 条/页", value: 100 },
  { label: "500 条/页", value: 500 },
  { label: "1000 条/页", value: 1000 },
  { label: "全部显示", value: 0 },
];
const pageSizeValues = new Set(pageSizeOptions.map((option) => option.value));

defineProps({
  page: { type: Number, required: true },
  pageSize: { type: Number, default: 20 },
  total: { type: Number, required: true },
  loading: { type: Boolean, default: false },
  ariaLabel: { type: String, default: "列表分页" },
});

const emit = defineEmits(["change", "size-change"]);

function handleSizeChange(value) {
  const nextPageSize = Number(value);
  if (pageSizeValues.has(nextPageSize)) {
    emit("size-change", nextPageSize);
  }
}
</script>
