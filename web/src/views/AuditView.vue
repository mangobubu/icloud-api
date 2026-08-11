<template>
  <section
    class="page-stack virtual-list-page"
    aria-labelledby="audit-section-title"
  >
    <SectionHeader
      id="audit-section-title"
      title="操作记录"
      description="记录后台登录与配置变更，不包含密码、完整 Key 或邮件内容。"
    >
      <template #actions>
        <el-tooltip content="刷新操作记录" placement="bottom">
          <el-button
            :icon="Refresh"
            circle
            :loading="loading"
            aria-label="刷新操作记录"
            @click="loadAuditLogs"
          />
        </el-tooltip>
      </template>
    </SectionHeader>

    <div v-if="loading && logs.length === 0" class="data-panel loading-panel">
      <el-skeleton :rows="7" animated />
    </div>

    <div v-else-if="loadError && logs.length === 0" class="load-failed">
      <RequestAlert :error="loadError" />
      <el-button :icon="Refresh" @click="loadAuditLogs">重新加载</el-button>
    </div>

    <EmptyState
      v-else-if="logs.length === 0"
      title="暂无操作记录"
      description="新的登录与配置操作会显示在这里。"
    />

    <template v-else>
      <RequestAlert
        v-if="loadError"
        :error="loadError"
        closable
        @close="loadError = null"
      />

      <div
        class="data-panel desktop-data-table virtual-list-table"
        :class="{
          'desktop-data-table--force': pageSize > 100 || pageSize === ALL_PAGE_SIZE,
        }"
        :aria-busy="loading"
      >
        <VirtualDataTable
          :columns="auditColumns"
          :data="logs"
          row-key="id"
          fill-height
          :loading="loading"
        >
          <template #cell="{ column, row }">
            <template v-if="column.key === 'createdAt'">
              {{ formatTime(row.createdAt, { seconds: true }) }}
            </template>
            <template v-else-if="column.key === 'username'">
              {{ row.username || "系统" }}
            </template>
            <template v-else-if="column.key === 'action'">
              <strong class="audit-action">{{ actionLabel(row.action) }}</strong>
            </template>
            <template v-else-if="column.key === 'resource'">
              {{ resourceLabel(row.resourceType, row.resourceId) }}
            </template>
            <template v-else-if="column.key === 'result'">
              <el-tag
                class="audit-result"
                :type="resultMeta(row.result).type"
                effect="plain"
                size="small"
              >
                {{ resultMeta(row.result).label }}
              </el-tag>
            </template>
            <template v-else-if="column.key === 'requestId'">
              <code class="audit-request-id">{{ row.requestId || "-" }}</code>
            </template>
          </template>
        </VirtualDataTable>
      </div>

      <div v-if="pageSize <= 100" class="mobile-record-list" :aria-busy="loading">
        <article v-for="log in logs" :key="log.id" class="mobile-record">
          <header class="mobile-record__header">
            <div class="primary-stack">
              <strong>{{ actionLabel(log.action) }}</strong>
              <small>{{ formatTime(log.createdAt, { seconds: true }) }}</small>
            </div>
            <el-tag
              class="audit-result"
              :type="resultMeta(log.result).type"
              effect="plain"
              size="small"
            >
              {{ resultMeta(log.result).label }}
            </el-tag>
          </header>
          <dl class="mobile-kv-list">
            <div>
              <dt>管理员</dt>
              <dd>{{ log.username || "系统" }}</dd>
            </div>
            <div>
              <dt>对象</dt>
              <dd>{{ resourceLabel(log.resourceType, log.resourceId) }}</dd>
            </div>
            <div>
              <dt>请求编号</dt>
              <dd><code class="audit-request-id">{{ log.requestId || "-" }}</code></dd>
            </div>
          </dl>
        </article>
      </div>

      <ListPagination
        :page="currentPage"
        :page-size="pageSize"
        :total="total"
        :loading="loading"
        aria-label="操作记录分页"
        @change="handlePageChange"
        @size-change="handlePageSizeChange"
      />
    </template>
  </section>
</template>

<script setup>
import { Refresh } from "@element-plus/icons-vue";
import { onBeforeUnmount, onMounted, ref } from "vue";

import { getAllAuditLogs, getAuditLogs } from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import ListPagination from "../components/ListPagination.vue";
import RequestAlert from "../components/RequestAlert.vue";
import SectionHeader from "../components/SectionHeader.vue";
import VirtualDataTable from "../components/VirtualDataTable.vue";
import { createLatestRequestGate } from "../utils/asyncState.js";
import { formatTime } from "../utils/format.js";
import {
  ALL_PAGE_SIZE,
  DEFAULT_PAGE_SIZE,
  normalizePageSize,
} from "../utils/pagination.js";

const actionLabels = {
  login: "登录后台",
  logout: "退出登录",
  change_password: "修改登录密码",
  create: "创建",
  update: "更新",
  delete: "删除",
  sync: "同步主号",
  rotate_key: "轮换 API Key",
  toggle: "切换启用状态",
};

const resourceLabels = {
  admin: "管理员",
  account: "主号",
  alias: "隐私邮箱",
};

const resultLabels = {
  success: { label: "成功", type: "success" },
  failed: { label: "失败", type: "danger" },
};

const pageSize = ref(DEFAULT_PAGE_SIZE);
const auditColumns = [
  { key: "createdAt", title: "时间", width: 176, flexGrow: 1 },
  { key: "username", title: "管理员", width: 130, flexGrow: 1 },
  { key: "action", title: "操作", width: 142, flexGrow: 1 },
  { key: "resource", title: "对象", width: 150, flexGrow: 1 },
  { key: "result", title: "结果", width: 92 },
  { key: "requestId", title: "请求编号", width: 180, flexGrow: 1 },
];
const logs = ref([]);
const currentPage = ref(1);
const total = ref(0);
const loading = ref(false);
const loadError = ref(null);
const loadGate = createLatestRequestGate();
let listAbortController = null;
let viewActive = true;

function actionLabel(action) {
  return actionLabels[action] || action || "未知操作";
}

function resourceLabel(type, id) {
  const typeLabel = resourceLabels[type] || type || "系统";
  return id ? `${typeLabel} #${id}` : typeLabel;
}

function resultMeta(result) {
  return resultLabels[result] || {
    label: result || "未知",
    type: "info",
  };
}

async function loadAuditLogs() {
  const page = currentPage.value;
  const selectedPageSize = pageSize.value;
  const requestKey = `${page}\u0000${selectedPageSize}`;
  const ticket = loadGate.begin(requestKey);
  listAbortController?.abort();
  const abortController = new AbortController();
  listAbortController = abortController;
  loading.value = true;
  loadError.value = null;
  try {
    const result = selectedPageSize === ALL_PAGE_SIZE
      ? { items: await getAllAuditLogs({ signal: abortController.signal }) }
      : await getAuditLogs({
          limit: selectedPageSize,
          offset: (page - 1) * selectedPageSize,
          signal: abortController.signal,
        });
    if (
      !viewActive ||
      !loadGate.isCurrent(ticket, `${currentPage.value}\u0000${pageSize.value}`)
    ) return;
    const nextTotal = Math.max(0, Number(result?.total) || 0);
    const nextItems = Array.isArray(result?.items) ? result.items : [];
    const allItems = selectedPageSize === ALL_PAGE_SIZE;
    const resolvedTotal = allItems ? nextItems.length : nextTotal;
    const lastPage = allItems
      ? 1
      : Math.max(1, Math.ceil(resolvedTotal / selectedPageSize));
    if (!allItems && page > lastPage) {
      currentPage.value = lastPage;
      logs.value = [];
      total.value = resolvedTotal;
      void loadAuditLogs();
      return;
    }
    logs.value = nextItems;
    total.value = resolvedTotal;
  } catch (error) {
    if (
      error?.name !== "AbortError" &&
      viewActive &&
      loadGate.isCurrent(ticket, `${currentPage.value}\u0000${pageSize.value}`)
    ) {
      loadError.value = error;
    }
  } finally {
    if (
      listAbortController === abortController &&
      loadGate.isCurrent(ticket, `${currentPage.value}\u0000${pageSize.value}`)
    ) {
      loading.value = false;
    }
    if (listAbortController === abortController) listAbortController = null;
  }
}

function handlePageChange(page) {
  if (pageSize.value === ALL_PAGE_SIZE) return;
  const nextPage = Math.max(1, Number(page) || 1);
  if (nextPage === currentPage.value) return;
  currentPage.value = nextPage;
  logs.value = [];
  loadError.value = null;
  void loadAuditLogs();
}

function handlePageSizeChange(value) {
  const nextPageSize = normalizePageSize(value);
  if (nextPageSize === pageSize.value) return;
  pageSize.value = nextPageSize;
  currentPage.value = 1;
  logs.value = [];
  total.value = 0;
  loadError.value = null;
  loadGate.invalidate();
  void loadAuditLogs();
}

onMounted(loadAuditLogs);

onBeforeUnmount(() => {
  viewActive = false;
  loadGate.deactivate();
  listAbortController?.abort();
});
</script>

<style scoped>
.audit-action {
  color: var(--el-text-color-primary);
  font-weight: 600;
}

.audit-result {
  min-width: 48px;
  justify-content: center;
}

.audit-request-id {
  overflow-wrap: anywhere;
}
</style>
