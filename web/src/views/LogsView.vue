<template>
  <section class="page-stack" aria-labelledby="logs-section-title">
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
          clearable
          :loading="accountsLoading"
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

      <div class="data-panel desktop-data-table" :aria-busy="loading || loadingMore">
        <el-table :data="logs" row-key="id" style="width: 100%">
          <el-table-column label="时间" min-width="178">
            <template #default="{ row }">
              {{ formatTime(row.time, { seconds: true }) }}
            </template>
          </el-table-column>
          <el-table-column label="级别" width="92">
            <template #default="{ row }">
              <el-tag
                class="runtime-log-level"
                :type="runtimeLogLevelMeta(row.level).type"
                effect="plain"
                size="small"
              >
                {{ runtimeLogLevelMeta(row.level).label }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="来源" min-width="150">
            <template #default="{ row }">
              <code class="runtime-log-source">{{ row.source || "system" }}</code>
            </template>
          </el-table-column>
          <el-table-column label="消息" min-width="360">
            <template #default="{ row }">
              <p class="runtime-log-message">{{ row.message || "-" }}</p>
            </template>
          </el-table-column>
          <el-table-column label="主号" min-width="170">
            <template #default="{ row }">
              {{ accountLabel(row.accountId) }}
            </template>
          </el-table-column>
          <el-table-column label="请求编号" min-width="180">
            <template #default="{ row }">
              <code class="runtime-log-request-id">{{ row.requestId || "-" }}</code>
            </template>
          </el-table-column>
          <el-table-column label="详情" width="70" align="right" fixed="right">
            <template #default="{ row }">
              <el-tooltip content="查看日志详情" placement="top">
                <el-button
                  :icon="View"
                  circle
                  :aria-label="`查看日志 ${row.id} 的详情`"
                  @click="openLogDetail(row)"
                />
              </el-tooltip>
            </template>
          </el-table-column>
        </el-table>
        <div v-if="loading" class="table-loading-mask" aria-hidden="true"></div>
      </div>

      <div class="mobile-record-list" :aria-busy="loading || loadingMore">
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

      <div class="runtime-log-pagination" aria-live="polite">
        <span>已显示 {{ logs.length }} 条</span>
        <el-button
          v-if="hasMore"
          :loading="loadingMore"
          :disabled="loading"
          @click="loadMoreLogs"
        >
          加载更多
        </el-button>
        <span v-else>已显示全部日志</span>
      </div>
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
  getAccounts,
  getRuntimeLogRun,
  getRuntimeLogs,
} from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import RequestAlert from "../components/RequestAlert.vue";
import RuntimeLogDetailDialog from "../components/RuntimeLogDetailDialog.vue";
import SectionHeader from "../components/SectionHeader.vue";
import { createLatestRequestGate } from "../utils/asyncState.js";
import { formatTime } from "../utils/format.js";
import { createLiveRefresh } from "../utils/liveRefresh.js";
import {
  appendRuntimeLogPage,
  normalizeRuntimeLogLevel,
  runtimeLogLevelMeta,
} from "../utils/runtimeLogs.js";

const PAGE_SIZE = 50;
const MAX_VISIBLE_LOGS = 2000;
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
const loadingMore = ref(false);
const loadError = ref(null);
const accountsLoading = ref(false);
const accountsLoadError = ref(null);
const hasMore = ref(false);
const nextBeforeId = ref(null);
const autoRefreshEnabled = ref(true);
const detailVisible = ref(false);
const selectedLog = ref(null);
const detailFlowLogs = ref([]);
const detailFlowLoading = ref(false);
const detailFlowError = ref(null);
const logRequestGate = createLatestRequestGate();
const detailRequestGate = createLatestRequestGate();
let latestRequest = null;
let latestRequestKey = "";
let loadMoreGeneration = 0;
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

async function loadAccounts() {
  accountsLoading.value = true;
  accountsLoadError.value = null;
  try {
    const nextAccounts = await getAccounts();
    if (!viewActive) return;
    accounts.value = nextAccounts;
  } catch (error) {
    if (viewActive) accountsLoadError.value = error;
  } finally {
    if (viewActive) accountsLoading.value = false;
  }
}

function currentRequestOptions(beforeId = "") {
  return {
    level: filters.level,
    query: appliedKeyword.value,
    accountId: filters.accountId,
    limit: PAGE_SIZE,
    beforeId,
  };
}

function loadLatestLogs({ silent = false, force = false } = {}) {
  const filterKey = currentFilterKey.value;
  if (!force && latestRequest && latestRequestKey === filterKey) {
    return latestRequest;
  }
  if (loadingMore.value) return Promise.resolve(false);

  const ticket = logRequestGate.begin(filterKey);
  if (!silent) {
    loading.value = true;
    loadError.value = null;
  }

  const request = (async () => {
    try {
      const result = await getRuntimeLogs(currentRequestOptions());
      if (!logRequestGate.isCurrent(ticket, currentFilterKey.value)) return false;

      logs.value = result.items.slice(0, MAX_VISIBLE_LOGS);
      hasMore.value = Boolean(result.hasMore && result.nextBeforeId != null);
      nextBeforeId.value = result.nextBeforeId;
      loadError.value = null;
      return true;
    } catch (error) {
      if (logRequestGate.isCurrent(ticket, currentFilterKey.value)) {
        loadError.value = error;
      }
      return false;
    } finally {
      if (latestRequest === request) {
        latestRequest = null;
        latestRequestKey = "";
      }
      if (!silent && logRequestGate.isCurrent(ticket, currentFilterKey.value)) {
        loading.value = false;
      }
    }
  })();

  latestRequest = request;
  latestRequestKey = filterKey;
  return request;
}

async function loadMoreLogs() {
  if (
    loadingMore.value ||
    loading.value ||
    latestRequest ||
    !hasMore.value ||
    nextBeforeId.value == null
  ) {
    return;
  }

  if (autoRefreshEnabled.value) {
    autoRefreshEnabled.value = false;
  }

  const filterKey = currentFilterKey.value;
  const cursor = nextBeforeId.value;
  const ticket = logRequestGate.begin(filterKey);
  const generation = ++loadMoreGeneration;
  loadingMore.value = true;
  try {
    const result = await getRuntimeLogs(currentRequestOptions(cursor));
    if (!logRequestGate.isCurrent(ticket, currentFilterKey.value)) return;
    const nextPage = appendRuntimeLogPage(
      logs.value,
      result,
      MAX_VISIBLE_LOGS,
    );
    logs.value = nextPage.items;
    hasMore.value = nextPage.hasMore;
    nextBeforeId.value = nextPage.nextBeforeId;
    loadError.value = null;
  } catch (error) {
    if (logRequestGate.isCurrent(ticket, currentFilterKey.value)) {
      loadError.value = error;
    }
  } finally {
    if (generation === loadMoreGeneration) {
      loadingMore.value = false;
    }
  }
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
  loadMoreGeneration += 1;
  loadingMore.value = false;
  hasMore.value = false;
  nextBeforeId.value = null;
  logs.value = [];
  loadError.value = null;
  if (updateRoute) updateRouteQuery();
  void loadLatestLogs({ force: true });
}

function refreshLatestLogs() {
  return loadLatestLogs({ force: true });
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

async function loadSelectedLogFlow() {
  const log = selectedLog.value;
  const syncRunId = String(log?.syncRunId || "").trim();
  if (!syncRunId || !detailVisible.value) return;

  detailFlowAbortController?.abort();
  detailFlowAbortController = new AbortController();
  const ticket = detailRequestGate.begin(syncRunId);
  detailFlowLoading.value = true;
  detailFlowError.value = null;

  try {
    const nextLogs = await getRuntimeLogRun(syncRunId, {
      accountId: log.accountId,
      signal: detailFlowAbortController.signal,
    });
    if (
      detailRequestGate.isCurrent(ticket, selectedLog.value?.syncRunId) &&
      detailVisible.value
    ) {
      detailFlowLogs.value = nextLogs;
    }
  } catch (error) {
    if (
      error?.name !== "AbortError" &&
      detailRequestGate.isCurrent(ticket, selectedLog.value?.syncRunId) &&
      detailVisible.value
    ) {
      detailFlowError.value = error;
    }
  } finally {
    if (detailRequestGate.isCurrent(ticket, selectedLog.value?.syncRunId)) {
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
  detailFlowLoading.value = Boolean(log?.syncRunId);
  detailVisible.value = true;
  if (log?.syncRunId) void loadSelectedLogFlow();
}

const liveRefresh = createLiveRefresh(() => loadLatestLogs({ silent: true }));

watch(autoRefreshEnabled, (enabled) => {
  if (enabled) {
    logRequestGate.invalidate();
    loadMoreGeneration += 1;
    loadingMore.value = false;
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
  logRequestGate.deactivate();
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

.runtime-log-pagination {
  display: flex;
  min-height: 38px;
  align-items: center;
  justify-content: center;
  flex-wrap: wrap;
  gap: 12px;
  color: var(--text-secondary);
  font-size: 13px;
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
