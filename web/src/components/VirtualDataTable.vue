<template>
  <div
    class="virtual-data-table"
    :style="{ height: `${height}px` }"
    :aria-busy="loading"
  >
    <el-auto-resizer :on-resize="handleResize">
      <template #default="{ width }">
        <el-table-v2
          v-if="width > 0"
          fixed
          :columns="resolvedColumns"
          :data="data"
          :row-key="rowKey"
          :row-height="rowHeight"
          :header-height="headerHeight"
          :width="Math.max(Math.floor(width), 1)"
          :height="height"
        >
          <template #cell="scope">
            <div
              class="virtual-data-table__cell"
              :class="{
                'is-center': scope.column.align === 'center',
                'is-right': scope.column.align === 'right',
              }"
            >
              <slot name="cell" v-bind="scope" :row="scope.rowData">
                {{ scope.cellData ?? "" }}
              </slot>
            </div>
          </template>

          <template #header-cell="scope">
            <div
              class="virtual-data-table__cell virtual-data-table__header-cell"
              :class="{
                'is-center': scope.column.align === 'center',
                'is-right': scope.column.align === 'right',
              }"
            >
              <slot name="header-cell" v-bind="scope">
                {{ scope.column.title ?? "" }}
              </slot>
            </div>
          </template>

          <template #empty>
            <slot name="empty" />
          </template>
        </el-table-v2>
      </template>
    </el-auto-resizer>

    <div
      v-if="loading"
      class="virtual-data-table__loading"
      role="status"
      aria-live="polite"
    >
      <el-icon class="is-loading" :size="22">
        <Loading />
      </el-icon>
      <span>加载中</span>
    </div>
  </div>
</template>

<script setup>
import { Loading } from "@element-plus/icons-vue";
import { computed, ref } from "vue";

const props = defineProps({
  columns: { type: Array, required: true },
  data: { type: Array, required: true },
  rowKey: { type: [String, Number, Symbol], default: "id" },
  height: { type: Number, default: 520 },
  rowHeight: { type: Number, default: 56 },
  headerHeight: { type: Number, default: 48 },
  loading: { type: Boolean, default: false },
});

const measuredWidth = ref(0);
const TABLE_SCROLLBAR_SIZE = 6;

const resolvedColumns = computed(() => {
  const columns = props.columns.map((column, index) => ({
    ...column,
    key: column.key ?? column.dataKey ?? index,
    width: Math.max(Number(column.width) || Number(column.minWidth) || 120, 1),
  }));
  const baseWidth = columns.reduce((total, column) => total + column.width, 0);
  const availableWidth = Math.max(
    Math.floor(measuredWidth.value) - TABLE_SCROLLBAR_SIZE,
    baseWidth,
  );
  const extraWidth = availableWidth - baseWidth;
  const totalFlexGrow = columns.reduce(
    (total, column) => total + Math.max(Number(column.flexGrow) || 0, 0),
    0,
  );

  if (extraWidth <= 0 || totalFlexGrow <= 0) return columns;

  let allocatedWidth = 0;
  let remainingFlexColumns = columns.filter(
    (column) => Number(column.flexGrow) > 0,
  ).length;

  return columns.map((column) => {
    const flexGrow = Math.max(Number(column.flexGrow) || 0, 0);
    if (!flexGrow) return column;

    remainingFlexColumns -= 1;
    const addition = remainingFlexColumns === 0
      ? extraWidth - allocatedWidth
      : Math.floor((extraWidth * flexGrow) / totalFlexGrow);
    allocatedWidth += addition;

    return { ...column, width: column.width + addition };
  });
});

function handleResize({ width }) {
  measuredWidth.value = width;
}
</script>

<style scoped>
.virtual-data-table {
  position: relative;
  width: 100%;
  min-width: 0;
  overflow: hidden;
  font-size: 13px;
}

.virtual-data-table__cell {
  display: flex;
  width: 100%;
  min-width: 0;
  align-items: center;
}

.virtual-data-table__cell.is-center {
  justify-content: center;
}

.virtual-data-table__cell.is-right {
  justify-content: flex-end;
}

.virtual-data-table__header-cell {
  height: 100%;
  font: inherit;
}

.virtual-data-table__loading {
  position: absolute;
  inset: 0;
  z-index: 3;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-direction: column;
  gap: 8px;
  color: var(--text-secondary);
  font-size: 12px;
  background: rgb(255 255 255 / 72%);
}

:deep(.el-table-v2) {
  --el-table-header-bg-color: #f7f8f9;
  --el-table-row-hover-bg-color: #f7faff;
  --el-table-border-color: #e5e8eb;
  font-size: 13px;
}

:deep(.el-table-v2__header-cell) {
  color: #5d6670;
  font-size: 12px;
  font-weight: 650;
}

:deep(.el-table-v2__row-cell) {
  padding: 0 12px;
}
</style>
