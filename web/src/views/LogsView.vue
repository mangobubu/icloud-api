<template>
  <section
    class="page-stack virtual-list-page"
    aria-labelledby="logs-section-title"
  >
    <SectionHeader
      id="logs-section-title"
      title="全部日志"
      description="查看当前进程最近 2000 条服务运行、同步与后台请求日志，定位异常发生的环节。"
    >
      <template #actions>
        <div class="runtime-log-auto-refresh">
          <span>自动刷新</span>
          <el-switch
            v-model="autoRefreshEnabled"
            aria-label="每 5 秒自动刷新日志"
          />
        </div>
        <el-tooltip content="刷新全部日志" placement="bottom">
          <el-button
            :icon="Refresh"
            circle
            :loading="loading"
            aria-label="刷新全部日志"
            @click="refreshLatestLogs"
          />
        </el-tooltip>
      </template>
    </SectionHeader>

    <div class="runtime-log-filters" role="search" aria-label="筛选全部日志">
      <label class="runtime-log-filter">
        <span>级别</span>
        <el-select
          v-model="filters.level"
          aria-label="按日志级别筛选"
          @change="applyFilters"
        >
          <el-option label="全部级别" value="" />
          <el-option label="错误" value="error" />
          <el-option label="警告" value="warn" />
          <el-option label="信息" value="info" />
          <el-option label="调试" value="debug" />
        </el-select>
      </label>

      <label class="runtime-log-filter">
        <span>主号</span>
        <el-select
          v-model="filters.accountId"
          filterable
          remote
          reserve-keyword
          clearable
          :loading="accountsLoading"
          :remote-method="searchAccounts"
          aria-label="按主号筛选日志"
          placeholder="全部主号"
          no-data-text="暂无主号"
          no-match-text="没有匹配的主号"
          @change="applyFilters"
        >
          <el-option label="全部主号" value="" />
          <el-option
            v-for="account in accounts"
            :key="account.id"
            :label="account.email"
            :value="String(account.id)"
          />
        </el-select>
      </label>

      <label class="runtime-log-filter runtime-log-filter--query">
        <span>关键词</span>
        <el-input
          v-model="keywordDraft"
          clearable
          maxlength="200"
          aria-label="搜索日志关键词"
          placeholder="消息、来源或请求编号"
          @keyup.enter="applyFilters"
          @clear="applyFilters"
        />
      </label>

      <div class="runtime-log-filter-actions">
        <el-button type="primary" :icon="Search" @click="applyFilters">
          查询
        </el-button>
        <el-button
          :icon="RefreshLeft"
          :disabled="!hasActiveFilters"
          @click="resetFilters"
        >
          重置
        </el-button>
      </div>
    </div>

    <RequestAlert
      v-if="accountsLoadError"
      :error="accountsLoadError"
      closable
      @close="accountsLoadError = null"
    />

    <div v-if="loading && logs.length === 0" class="data-panel loading-panel">
      <el-skeleton :rows="7" animated />
    </div>

    <div v-else-if="loadError && logs.length === 0" class="load-failed">
      <RequestAlert :error="loadError" />
      <el-button :icon="Refresh" @click="refreshLatestLogs">重新加载</el-button>
    </div>

    <EmptyState
      v-else-if="logs.length === 0"
      :title="hasAppliedFilters ? '没有匹配的日志' : '暂无运行日志'"
      :description="
        hasAppliedFilters
          ? '调整筛选条件后重新查询。'
          : '服务产生新的运行日志后会显示在这里。'
      "
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
          :columns="runtimeLogColumns"
          :data="logs"
          row-key="id"
          fill-height
          :row-height="68"
          :loading="loading"
        >
          <template #cell="{ column, row }">
            <template v-if="column.key === 'time'">
              {{ formatTime(row.time, { seconds: true }) }}
            </template>
            <template v-else-if="column.key === 'level'">
              <el-tag
                class="runtime-log-level"
                :type="runtimeLogLevelMeta(row.level).type"
                effect="plain"
                size="small"
              >
                {{ runtimeLogLevelMeta(row.level).label }}
              </el-tag>
            </template>
            <template v-else-if="column.key === 'source'">
              <code class="runtime-log-source">{{ row.source || "system" }}</code>
            </template>
            <template v-else-if="column.key === 'message'">
              <p class="runtime-log-message">{{ row.message || "-" }}</p>
            </template>
            <template v-else-if="column.key === 'accountId'">
              {{ accountLabel(row.accountId) }}
            </template>
            <template v-else-if="column.key === 'requestId'">
              <code class="runtime-log-request-id">{{ row.requestId || "-" }}</code>
            </template>
            <template v-else-if="column.key === 'actions'">
              <el-tooltip content="查看日志详情" placement="top">
                <el-button
                  :icon="View"
                  circle
                  :aria-label="`查看日志 ${row.id} 的详情`"
                  @click="openLogDetail(row)"
                />
              </el-tooltip>
            </template>
          </template>
        </VirtualDataTable>
      </div>

      <div v-if="pageSize <= 100" class="mobile-record-list" :aria-busy="loading">
        <article v-for="log in logs" :key="log.id" class="mobile-record">
          <header class="mobile-record__header">
            <div class="primary-stack">
              <strong class="runtime-log-source">{{ log.source || "system" }}</strong>
              <small>{{ formatTime(log.time, { seconds: true }) }}</small>
            </div>
            <el-tag
              class="runtime-log-level"
              :type="runtimeLogLevelMeta(log.level).type"
              effect="plain"
              size="small"
            >
              {{ runtimeLogLevelMeta(log.level).label }}
            </el-tag>
          </header>
          <p class="runtime-log-mobile-message">{{ log.message || "-" }}</p>
          <dl class="mobile-kv-list">
            <div>
              <dt>主号</dt>
              <dd>{{ accountLabel(log.accountId) }}</dd>
            </div>
            <div>
              <dt>请求编号</dt>
              <dd><code class="runtime-log-request-id">{{ log.requestId || "-" }}</code></dd>
            </div>
          </dl>
          <footer class="mobile-record__actions">
            <el-button :icon="View" @click="openLogDetail(log)">
              查看详情
            </el-button>
          </footer>
        </article>
      </div>

      <ListPagination
        :page="currentPage"
        :page-size="pageSize"
        :total="total"
        :loading="loading"
        aria-label="全部日志分页"
        @change="handlePageChange"
        @size-change="handlePageSizeChange"
      />
    </template>

    <RuntimeLogDetailDialog
      v-model="detailVisible"
      :log="selectedLog"
      :flow-logs="detailFlowLogs"
      :flow-loading="detailFlowLoading"
      :flow-error="detailFlowError"
      :account-label="accountLabel(selectedLog?.accountId)"
      @retry-flow="loadSelectedLogFlow"
    />
  </section>
</template>

<script setup>
import {
  Refresh,
  RefreshLeft,
  Search,
  View,
} from "@element-plus/icons-vue";
import {
  computed,
  onBeforeUnmount,
  onMounted,
  reactive,
  ref,
  watch,
} from "vue";
import { useRoute, useRouter } from "vue-router";

import {
  getAutoCreateLogRun,
  getAccountPage,
  getAllRuntimeLogs,
  getRuntimeLogRun,
  getRuntimeLogs,
} from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import ListPagination from "../components/ListPagination.vue";
import RequestAlert from "../components/RequestAlert.vue";
import RuntimeLogDetailDialog from "../components/RuntimeLogDetailDialog.vue";
import SectionHeader from "../components/SectionHeader.vue";
import VirtualDataTable from "../components/VirtualDataTable.vue";
import { createLatestRequestGate } from "../utils/asyncState.js";
import { formatTime } from "../utils/format.js";
import { createLiveRefresh } from "../utils/liveRefresh.js";
import {
  normalizeRuntimeLogLevel,
  runtimeLogLevelMeta,
} from "../utils/runtimeLogs.js";
import {
  ALL_PAGE_SIZE,
  DEFAULT_PAGE_SIZE,
  normalizePageSize,
} from "../utils/pagination.js";

const ACCOUNT_OPTION_LIMIT = 50;
const runtimeLogColumns = [
  { key: "time", title: "时间", width: 178, flexGrow: 1 },
  { key: "level", title: "级别", width: 92 },
  { key: "source", title: "来源", width: 150, flexGrow: 1 },
  { key: "message", title: "消息", width: 360, flexGrow: 3 },
  { key: "accountId", title: "主号", width: 170, flexGrow: 1 },
  { key: "requestId", title: "请求编号", width: 180, flexGrow: 1 },
  { key: "actions", title: "详情", width: 70, align: "right", fixed: "right" },
];
const selectableLevels = new Set(["debug", "info", "warn", "error"]);
const route = useRoute();
const router = useRouter();

function queryValue(value) {
  return String(Array.isArray(value) ? value[0] || "" : value || "").trim();
}

function routeFilterState() {
  const rawLevel = queryValue(route.query.level);
  const level = rawLevel ? normalizeRuntimeLogLevel(rawLevel) : "";
  const accountId = queryValue(route.query.account_id);
  return {
    level: selectableLevels.has(level) ? level : "",
    accountId: /^\d+$/.test(accountId) && Number(accountId) > 0 ? accountId : "",
    query: queryValue(route.query.query),
  };
}

const initialFilters = routeFilterState();
const logs = ref([]);
const accounts = ref([]);
const filters = reactive({
  level: initialFilters.level,
  accountId: initialFilters.accountId,
});
const keywordDraft = ref(initialFilters.query);
const appliedKeyword = ref(initialFilters.query);
const loading = ref(false);
const loadError = ref(null);
const accountsLoading = ref(false);
const accountsLoadError = ref(null);
const currentPage = ref(1);
const pageSize = ref(DEFAULT_PAGE_SIZE);
const total = ref(0);
const autoRefreshEnabled = ref(true);
const detailVisible = ref(false);
const selectedLog = ref(null);
const detailFlowLogs = ref([]);
const detailFlowLoading = ref(false);
const detailFlowError = ref(null);
const logRequestGate = createLatestRequestGate();
const detailRequestGate = createLatestRequestGate();
const accountLoadGate = createLatestRequestGate();
let latestRequest = null;
let latestRequestKey = "";
let listAbortController = null;
let accountSearchTimer = null;
let detailFlowAbortController = null;
let viewActive = true;

const currentFilterKey = computed(() =>
  [filters.level, filters.accountId, appliedKeyword.value].join("\u0000"),
);
const hasActiveFilters = computed(() =>
  Boolean(filters.level || filters.accountId || keywordDraft.value.trim()),
);
const hasAppliedFilters = computed(() =>
  Boolean(filters.level || filters.accountId || appliedKeyword.value),
);

function accountLabel(accountId) {
  if (accountId === null || accountId === undefined || accountId === "") return "-";
  const account = accounts.value.find(
    (item) => String(item.id) === String(accountId),
  );
  return account?.email || `主号 #${accountId}`;
}

async function loadAccounts({ query = "" } = {}) {
  const normalizedQuery = String(query || "").trim();
  const ticket = accountLoadGate.begin(normalizedQuery);
  accountsLoading.value = true;
  accountsLoadError.value = null;
  try {
    const result = await getAccountPage({
      limit: ACCOUNT_OPTION_LIMIT,
      offset: 0,
      query: normalizedQuery,
    });
    if (!viewActive || !accountLoadGate.isCurrent(ticket, normalizedQuery)) {
      return;
    }
    const nextAccounts = Array.isArray(result?.items) ? result.items : [];
    const selected = accounts.value.find(
      (account) => String(account.id) === String(filters.accountId),
    );
    accounts.value =
      selected &&
      !nextAccounts.some(
        (account) => String(account.id) === String(selected.id),
      )
        ? [selected, ...nextAccounts]
        : nextAccounts;
  } catch (error) {
    if (viewActive && accountLoadGate.isCurrent(ticket, normalizedQuery)) {
      accountsLoadError.value = error;
    }
  } finally {
    if (accountLoadGate.isCurrent(ticket, normalizedQuery)) {
      accountsLoading.value = false;
    }
  }
}

function searchAccounts(query) {
  if (accountSearchTimer !== null) {
    window.clearTimeout(accountSearchTimer);
  }
  accountSearchTimer = window.setTimeout(() => {
    accountSearchTimer = null;
    void loadAccounts({ query });
  }, query ? 220 : 0);
}

function currentRequestOptions(page = currentPage.value) {
  return {
    level: filters.level,
    query: appliedKeyword.value,
    accountId: filters.accountId,
    limit: pageSize.value,
    offset: (page - 1) * pageSize.value,
  };
}

function loadLatestLogs({ silent = false, force = false } = {}) {
  const filterKey = currentFilterKey.value;
  const page = currentPage.value;
  const selectedPageSize = pageSize.value;
  const requestKey = `${filterKey}\u0000${page}\u0000${selectedPageSize}`;
  if (!force && latestRequest && latestRequestKey === requestKey) {
    return latestRequest;
  }

  const ticket = logRequestGate.begin(requestKey);
  listAbortController?.abort();
  const abortController = new AbortController();
  listAbortController = abortController;
  if (!silent) {
    loading.value = true;
    loadError.value = null;
  }

  const request = (async () => {
    try {
      const requestOptions = {
        ...currentRequestOptions(page),
        signal: abortController.signal,
      };
      const result = selectedPageSize === ALL_PAGE_SIZE
        ? {
            items: await getAllRuntimeLogs(requestOptions),
          }
        : await getRuntimeLogs(requestOptions);
      if (
        !logRequestGate.isCurrent(
          ticket,
          `${currentFilterKey.value}\u0000${currentPage.value}\u0000${pageSize.value}`,
        )
      ) {
        return false;
      }

      const items = Array.isArray(result?.items) ? result.items : [];
      const reportedTotal = Number(result?.total);
      const allItems = selectedPageSize === ALL_PAGE_SIZE;
      const nextTotal = allItems
        ? items.length
        : Number.isFinite(reportedTotal)
          ? Math.max(0, reportedTotal)
          : (page - 1) * selectedPageSize + items.length + (result?.hasMore ? 1 : 0);
      const lastPage = allItems
        ? 1
        : Math.max(1, Math.ceil(nextTotal / selectedPageSize));
      if (!allItems && page > lastPage) {
        currentPage.value = lastPage;
        logs.value = [];
        total.value = nextTotal;
        void loadLatestLogs({ force: true });
        return false;
      }
      logs.value = items;
      total.value = nextTotal;
      loadError.value = null;
      return true;
    } catch (error) {
      if (
        error?.name !== "AbortError" &&
        logRequestGate.isCurrent(
          ticket,
          `${currentFilterKey.value}\u0000${currentPage.value}\u0000${pageSize.value}`,
        )
      ) {
        loadError.value = error;
      }
      return false;
    } finally {
      if (latestRequest === request) {
        latestRequest = null;
        latestRequestKey = "";
      }
      if (
        listAbortController === abortController &&
        logRequestGate.isCurrent(
          ticket,
          `${currentFilterKey.value}\u0000${currentPage.value}\u0000${pageSize.value}`,
        )
      ) {
        loading.value = false;
      }
      if (listAbortController === abortController) {
        listAbortController = null;
      }
    }
  })();

  latestRequest = request;
  latestRequestKey = requestKey;
  return request;
}

function updateRouteQuery() {
  const query = {};
  if (filters.level) query.level = filters.level;
  if (filters.accountId) query.account_id = filters.accountId;
  if (appliedKeyword.value) query.query = appliedKeyword.value;
  void router.replace({ name: "logs", query });
}

function reloadForFilters({ updateRoute = true } = {}) {
  logRequestGate.invalidate();
  currentPage.value = 1;
  logs.value = [];
  total.value = 0;
  loadError.value = null;
  if (updateRoute) updateRouteQuery();
  void loadLatestLogs({ force: true });
}

function refreshLatestLogs() {
  return loadLatestLogs({ force: true });
}

function handlePageChange(page) {
  if (pageSize.value === ALL_PAGE_SIZE) return;
  const nextPage = Math.max(1, Number(page) || 1);
  if (nextPage === currentPage.value) return;
  if (nextPage > 1 && autoRefreshEnabled.value) {
    autoRefreshEnabled.value = false;
  }
  currentPage.value = nextPage;
  logs.value = [];
  total.value = 0;
  loadError.value = null;
  void loadLatestLogs({ force: true });
}

function handlePageSizeChange(value) {
  const nextPageSize = normalizePageSize(value);
  if (nextPageSize === pageSize.value) return;
  pageSize.value = nextPageSize;
  currentPage.value = 1;
  logs.value = [];
  total.value = 0;
  loadError.value = null;
  logRequestGate.invalidate();
  void loadLatestLogs({ force: true });
}

function applyFilters() {
  appliedKeyword.value = keywordDraft.value.trim();
  reloadForFilters();
}

function resetFilters() {
  filters.level = "";
  filters.accountId = "";
  keywordDraft.value = "";
  appliedKeyword.value = "";
  reloadForFilters();
}

function logFlowKey(log) {
  const autoCreateRunId = String(log?.autoCreateRunId || "").trim();
  if (autoCreateRunId) return `auto-create:${autoCreateRunId}`;
  const syncRunId = String(log?.syncRunId || "").trim();
  return syncRunId ? `sync:${syncRunId}` : "";
}

async function loadSelectedLogFlow() {
  const log = selectedLog.value;
  const autoCreateRunId = String(log?.autoCreateRunId || "").trim();
  const syncRunId = String(log?.syncRunId || "").trim();
  const flowKey = logFlowKey(log);
  if (!flowKey || !detailVisible.value) return;

  detailFlowAbortController?.abort();
  detailFlowAbortController = new AbortController();
  const ticket = detailRequestGate.begin(flowKey);
  detailFlowLoading.value = true;
  detailFlowError.value = null;

  try {
    const nextLogs = autoCreateRunId
      ? await getAutoCreateLogRun(autoCreateRunId, {
          accountId: log.accountId,
          signal: detailFlowAbortController.signal,
        })
      : await getRuntimeLogRun(syncRunId, {
          accountId: log.accountId,
          signal: detailFlowAbortController.signal,
        });
    if (
      detailRequestGate.isCurrent(ticket, logFlowKey(selectedLog.value)) &&
      detailVisible.value
    ) {
      detailFlowLogs.value = nextLogs;
    }
  } catch (error) {
    if (
      error?.name !== "AbortError" &&
      detailRequestGate.isCurrent(ticket, logFlowKey(selectedLog.value)) &&
      detailVisible.value
    ) {
      detailFlowError.value = error;
    }
  } finally {
    if (detailRequestGate.isCurrent(ticket, logFlowKey(selectedLog.value))) {
      detailFlowLoading.value = false;
    }
  }
}

function openLogDetail(log) {
  detailFlowAbortController?.abort();
  detailRequestGate.invalidate();
  selectedLog.value = log;
  detailFlowLogs.value = [];
  detailFlowError.value = null;
  detailFlowLoading.value = Boolean(logFlowKey(log));
  detailVisible.value = true;
  if (logFlowKey(log)) void loadSelectedLogFlow();
}

const liveRefresh = createLiveRefresh(() => loadLatestLogs({ silent: true }));

watch(autoRefreshEnabled, (enabled) => {
  if (enabled) {
    logRequestGate.invalidate();
    void loadLatestLogs({ force: true });
    void liveRefresh.start({ immediate: false });
  } else {
    liveRefresh.stop();
  }
});

watch(detailVisible, (visible) => {
  if (visible) return;
  detailFlowAbortController?.abort();
  detailFlowAbortController = null;
  detailRequestGate.invalidate();
  detailFlowLoading.value = false;
});

watch(
  () => [route.query.level, route.query.account_id, route.query.query],
  () => {
    const next = routeFilterState();
    if (
      next.level === filters.level &&
      next.accountId === filters.accountId &&
      next.query === appliedKeyword.value
    ) {
      return;
    }
    filters.level = next.level;
    filters.accountId = next.accountId;
    keywordDraft.value = next.query;
    appliedKeyword.value = next.query;
    reloadForFilters({ updateRoute: false });
  },
);

onMounted(() => {
  void loadAccounts();
  void loadLatestLogs({ force: true });
  if (autoRefreshEnabled.value) {
    void liveRefresh.start({ immediate: false });
  }
});

onBeforeUnmount(() => {
  viewActive = false;
  if (accountSearchTimer !== null) {
    window.clearTimeout(accountSearchTimer);
    accountSearchTimer = null;
  }
  logRequestGate.deactivate();
  listAbortController?.abort();
  accountLoadGate.deactivate();
  detailFlowAbortController?.abort();
  detailRequestGate.deactivate();
  liveRefresh.stop();
});
</script>

<style scoped>
.runtime-log-auto-refresh {
  display: inline-flex;
  min-height: 32px;
  align-items: center;
  gap: 8px;
  color: var(--text-secondary);
  font-size: 13px;
  white-space: nowrap;
}

.runtime-log-filters {
  display: grid;
  grid-template-columns:
    minmax(120px, 0.55fr)
    minmax(190px, 0.8fr)
    minmax(260px, 1.5fr)
    auto;
  align-items: end;
  gap: 14px;
  padding: 16px;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: 6px;
}

.runtime-log-filter {
  display: grid;
  min-width: 0;
  gap: 6px;
}

.runtime-log-filter > span {
  color: var(--text-secondary);
  font-size: 12px;
  font-weight: 600;
}

.runtime-log-filter-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}

.runtime-log-level {
  min-width: 48px;
  justify-content: center;
}

.runtime-log-source,
.runtime-log-request-id {
  overflow-wrap: anywhere;
}

.runtime-log-message {
  display: -webkit-box;
  overflow: hidden;
  line-height: 1.55;
  overflow-wrap: anywhere;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 2;
}

.runtime-log-mobile-message {
  margin: 0;
  padding: 14px;
  color: var(--text);
  line-height: 1.55;
  overflow-wrap: anywhere;
  border-top: 1px solid var(--border);
  border-bottom: 1px solid var(--border);
}

@media (max-width: 1080px) {
  .runtime-log-filters {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }

  .runtime-log-filter--query {
    grid-column: 1 / -1;
  }

  .runtime-log-filter-actions {
    grid-column: 1 / -1;
    justify-content: flex-end;
  }
}

@media (max-width: 720px) {
  .runtime-log-filters {
    grid-template-columns: minmax(0, 1fr);
    padding: 14px;
  }

  .runtime-log-filter--query,
  .runtime-log-filter-actions {
    grid-column: auto;
  }

  .runtime-log-filter-actions > .el-button {
    min-width: 0;
    flex: 1 1 0;
  }
}
</style>
