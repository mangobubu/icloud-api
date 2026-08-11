<template>
  <section
    class="page-stack virtual-list-page"
    aria-labelledby="aliases-section-title"
  >
    <SectionHeader
      id="aliases-section-title"
      title="全部隐私邮箱"
      description="查看每个地址的主号归属、Key 状态和最近使用情况。"
    >
      <template #actions>
        <div class="alias-filter" role="group" aria-label="按主号筛选">
          <span class="alias-filter__label">所属主号</span>
          <el-select
            v-model="selectedAccountId"
            class="alias-filter__select"
            filterable
            remote
            reserve-keyword
            clearable
            :disabled="accountsLoading || accounts.length === 0"
            :loading="accountsLoading"
            :remote-method="searchAccounts"
            placeholder="全部主号"
            aria-label="按所属主号筛选"
            no-data-text="暂无主号"
            no-match-text="没有匹配的主号"
            @change="handleAccountFilterChange"
          >
            <el-option label="全部主号" value="" />
            <el-option
              v-for="account in accounts"
              :key="account.id"
              :label="account.email"
              :value="account.id"
            >
              <div class="primary-stack">
                <strong>{{ account.email }}</strong>
                <small>{{ account.name || "未填写备注" }}</small>
              </div>
            </el-option>
          </el-select>
        </div>
        <el-button
          :icon="Download"
          :disabled="selectedAliases.length === 0"
          @click="exportSelectedAliases"
        >
          导出勾选<span v-if="selectedAliases.length">
            （{{ selectedAliases.length }}）
          </span>
        </el-button>
        <el-button
          type="primary"
          :icon="Download"
          :loading="exportingAll"
          :disabled="total === 0 || exportingAll"
          @click="exportAllAliases"
        >
          全部导出
        </el-button>
        <el-tooltip content="刷新隐私邮箱列表" placement="bottom">
          <el-button
            :icon="Refresh"
            circle
            :loading="loading"
            aria-label="刷新隐私邮箱列表"
            @click="loadAliases"
          />
        </el-tooltip>
      </template>
    </SectionHeader>

    <RequestAlert
      v-if="accountsLoadError"
      :error="accountsLoadError"
      closable
      @close="accountsLoadError = null"
    />

    <div v-if="loading && aliases.length === 0" class="data-panel loading-panel">
      <el-skeleton :rows="6" animated />
    </div>

    <div v-else-if="loadError && aliases.length === 0" class="load-failed">
      <RequestAlert :error="loadError" />
      <el-button :icon="Refresh" @click="loadAliases">重新加载</el-button>
    </div>

    <EmptyState
      v-else-if="aliases.length === 0"
      :title="selectedAccountId ? '该主号暂无隐私邮箱' : '还没有隐私邮箱'"
      :description="
        selectedAccountId
          ? '请选择其他主号，或进入该主号详情页添加地址。'
          : '进入某个主号详情页添加地址。'
      "
    >
      <el-button type="primary" :icon="Setting" @click="openAccounts">
        查看主号
      </el-button>
    </EmptyState>

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
          :columns="aliasColumns"
          :data="aliases"
          row-key="id"
          fill-height
          :row-height="64"
          :loading="loading"
        >
          <template #header-cell="{ column }">
            <el-checkbox
              v-if="column.key === 'selection'"
              :model-value="allExportableAliasesSelected"
              :indeterminate="someExportableAliasesSelected"
              :disabled="exportableAliases.length === 0"
              aria-label="勾选本页可导出的隐私邮箱"
              @change="setAllAliasesSelected"
            />
            <template v-else>{{ column.title }}</template>
          </template>
          <template #cell="{ column, row }">
            <el-checkbox
              v-if="column.key === 'selection'"
              :model-value="isAliasSelected(row.id)"
              :disabled="!isAliasExportable(row)"
              :aria-label="`勾选 ${row.address}`"
              @change="setAliasSelected(row, $event)"
            />
            <template v-else-if="column.key === 'address'">
              <div class="primary-stack">
                <strong>{{ row.address }}</strong>
                <small>{{ row.label || "未填写用途备注" }}</small>
              </div>
            </template>
            <template v-else-if="column.key === 'account'">
              <el-button
                class="account-link"
                link
                type="primary"
                @click="openAccount(row.accountId)"
              >
                {{ row.accountEmail || "查看主号" }}
              </el-button>
            </template>
            <template v-else-if="column.key === 'apiKey'">
              <code class="key-prefix">{{ keyPrefix(row) }}</code>
            </template>
            <template v-else-if="column.key === 'lastAccessedAt'">
              {{ formatTime(row.lastAccessedAt) }}
            </template>
            <template v-else-if="column.key === 'latestReceivedAt'">
              {{ formatTime(row.latestReceivedAt) }}
            </template>
            <template v-else-if="column.key === 'status'">
              <el-tag
                v-if="isAliasConfirmationPending(row)"
                type="warning"
                effect="plain"
                size="small"
              >
                等待目录确认
              </el-tag>
              <SyncStatus v-else :item="row" details />
            </template>
            <template v-else-if="column.key === 'actions'">
              <div class="icon-action-row">
                <el-tooltip
                  v-if="isAliasExportable(row)"
                  content="复制邮件 API 直达链接"
                  placement="top"
                >
                  <el-button
                    :icon="CopyDocument"
                    circle
                    :loading="Boolean(copyLoading[row.id])"
                    :disabled="!row.directLinkPath"
                    :aria-label="`复制 ${row.address} 的邮件 API 直达链接`"
                    @click="copyAliasDirectLink(row)"
                  />
                </el-tooltip>
                <el-button
                  link
                  type="primary"
                  :icon="Setting"
                  @click="openAccount(row.accountId)"
                >
                  管理
                </el-button>
              </div>
            </template>
          </template>
        </VirtualDataTable>
      </div>

      <div v-if="pageSize <= 100" class="mobile-record-list" :aria-busy="loading">
        <article v-for="alias in aliases" :key="alias.id" class="mobile-record">
          <header class="mobile-record__header">
            <div class="mobile-alias-selection">
              <el-checkbox
                class="mobile-alias-selection__checkbox"
                :model-value="isAliasSelected(alias.id)"
                :disabled="!isAliasExportable(alias)"
                :aria-label="`勾选 ${alias.address}`"
                @change="setAliasSelected(alias, $event)"
              />
              <div class="primary-stack">
                <strong>{{ alias.address }}</strong>
                <small>{{ alias.label || "未填写用途备注" }}</small>
              </div>
            </div>
            <el-tag
              v-if="isAliasConfirmationPending(alias)"
              type="warning"
              effect="plain"
              size="small"
            >
              等待目录确认
            </el-tag>
            <SyncStatus v-else :item="alias" details />
          </header>
          <dl class="mobile-kv-list">
            <div>
              <dt>所属主号</dt>
              <dd>{{ alias.accountEmail || "-" }}</dd>
            </div>
            <div>
              <dt>API Key</dt>
              <dd><code class="key-prefix">{{ keyPrefix(alias) }}</code></dd>
            </div>
            <div>
              <dt>最近调用</dt>
              <dd>{{ formatTime(alias.lastAccessedAt) }}</dd>
            </div>
            <div>
              <dt>最新邮件</dt>
              <dd>{{ formatTime(alias.latestReceivedAt) }}</dd>
            </div>
          </dl>
          <footer class="mobile-record__actions mobile-record__actions--direct-link">
            <el-button
              v-if="isAliasExportable(alias)"
              :icon="CopyDocument"
              :loading="Boolean(copyLoading[alias.id])"
              :disabled="!alias.directLinkPath"
              :aria-label="`复制 ${alias.address} 的邮件 API 直达链接`"
              @click="copyAliasDirectLink(alias)"
            >
              复制邮件 API 直达链接
            </el-button>
            <el-button
              :icon="Setting"
              :aria-label="`管理 ${alias.address} 所属主号`"
              @click="openAccount(alias.accountId)"
            >
              管理所属主号
            </el-button>
          </footer>
        </article>
      </div>

      <ListPagination
        :page="currentPage"
        :page-size="pageSize"
        :total="total"
        :loading="loading"
        aria-label="隐私邮箱列表分页"
        @change="handlePageChange"
        @size-change="handlePageSizeChange"
      />
    </template>
  </section>
</template>

<script setup>
import {
  CopyDocument,
  Download,
  Refresh,
  Setting,
} from "@element-plus/icons-vue";
import { ElMessage } from "element-plus";
import { computed, onBeforeUnmount, onMounted, reactive, ref } from "vue";
import { useRouter } from "vue-router";

import { getAccountPage, getAliasPage, getAllAliases } from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import ListPagination from "../components/ListPagination.vue";
import RequestAlert from "../components/RequestAlert.vue";
import SectionHeader from "../components/SectionHeader.vue";
import SyncStatus from "../components/SyncStatus.vue";
import VirtualDataTable from "../components/VirtualDataTable.vue";
import {
  createActionLock,
  createLatestRequestGate,
} from "../utils/asyncState.js";
import { buildAliasExportText } from "../utils/aliasExport.js";
import {
  buildRecentMailDirectLink,
  copyText,
} from "../utils/clipboard.js";
import { showRequestError, successMessage } from "../utils/feedback.js";
import { formatTime } from "../utils/format.js";
import { createLiveRefresh } from "../utils/liveRefresh.js";
import {
  ALL_PAGE_SIZE,
  DEFAULT_PAGE_SIZE,
  normalizePageSize,
} from "../utils/pagination.js";

const router = useRouter();
const ACCOUNT_OPTION_LIMIT = 50;
const pageSize = ref(DEFAULT_PAGE_SIZE);
const aliasColumns = [
  { key: "selection", title: "", width: 52, align: "center", fixed: "left" },
  { key: "address", title: "隐私邮箱", width: 220, flexGrow: 2 },
  { key: "account", title: "所属主号", width: 190, flexGrow: 1 },
  { key: "apiKey", title: "API Key", width: 120 },
  { key: "lastAccessedAt", title: "最近调用", width: 150, flexGrow: 1 },
  { key: "latestReceivedAt", title: "最新邮件", width: 150, flexGrow: 1 },
  { key: "status", title: "状态", width: 134, flexGrow: 1 },
  { key: "actions", title: "操作", width: 152, align: "right", fixed: "right" },
];
const aliases = ref([]);
const accounts = ref([]);
const selectedAccountId = ref("");
const currentPage = ref(1);
const total = ref(0);
const selectedAliasIds = ref([]);
const loading = ref(false);
const loadError = ref(null);
const accountsLoading = ref(false);
const accountsLoadError = ref(null);
const exportingAll = ref(false);
const copyLoading = reactive({});
const copyLock = createActionLock();
const aliasLoadGate = createLatestRequestGate();
const accountsLoadGate = createLatestRequestGate();
let accountSearchTimer = null;
let aliasAbortController = null;
let viewActive = true;

const selectedAliases = computed(() => {
  const selectedIds = new Set(selectedAliasIds.value);
  return aliases.value.filter(
    (alias) => selectedIds.has(alias.id) && isAliasExportable(alias),
  );
});

const exportableAliases = computed(() => aliases.value.filter(isAliasExportable));

const allExportableAliasesSelected = computed(() =>
  exportableAliases.value.length > 0 &&
  exportableAliases.value.every((alias) => isAliasSelected(alias.id)),
);

const someExportableAliasesSelected = computed(() => {
  const selectedCount = exportableAliases.value.filter((alias) =>
    isAliasSelected(alias.id),
  ).length;
  return selectedCount > 0 && selectedCount < exportableAliases.value.length;
});

function isAliasConfirmationPending(alias) {
  return (
    !alias?.enabled &&
    String(alias?.lastSyncError || "").trim() ===
      "APPLE_ALIAS_CONFIRMATION_PENDING"
  );
}

function isAliasExportable(alias) {
  return !isAliasConfirmationPending(alias) && Boolean(alias?.directLinkPath);
}

function keyPrefix(alias) {
  return alias.apiKeyPrefix ? `${alias.apiKeyPrefix}…` : "-";
}

async function loadAccounts({ silent = false, query = "" } = {}) {
  const normalizedQuery = String(query || "").trim();
  const ticket = accountsLoadGate.begin(normalizedQuery);
  if (!silent) {
    accountsLoading.value = true;
    accountsLoadError.value = null;
  }
  try {
    const result = await getAccountPage({
      limit: ACCOUNT_OPTION_LIMIT,
      offset: 0,
      query: normalizedQuery,
    });
    if (!accountsLoadGate.isCurrent(ticket, normalizedQuery)) return;
    const nextAccounts = Array.isArray(result?.items) ? result.items : [];
    const selected = accounts.value.find(
      (account) => String(account.id) === String(selectedAccountId.value),
    );
    accounts.value =
      selected &&
      !nextAccounts.some(
        (account) => String(account.id) === String(selected.id),
      )
        ? [selected, ...nextAccounts]
        : nextAccounts;
    accountsLoadError.value = null;
  } catch (error) {
    if (
      accountsLoadGate.isCurrent(ticket, normalizedQuery) &&
      !silent
    ) {
      accountsLoadError.value = error;
    }
  } finally {
    if (accountsLoadGate.isCurrent(ticket, normalizedQuery)) {
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

async function loadAliases({ silent = false } = {}) {
  const accountId = selectedAccountId.value;
  const page = currentPage.value;
  const selectedPageSize = pageSize.value;
  const requestKey = `${accountId}\u0000${page}\u0000${selectedPageSize}`;
  const ticket = aliasLoadGate.begin(requestKey);
  aliasAbortController?.abort();
  const abortController = new AbortController();
  aliasAbortController = abortController;
  if (!silent) {
    loading.value = true;
    loadError.value = null;
  }
  try {
    const result = selectedPageSize === ALL_PAGE_SIZE
      ? {
          items: await getAllAliases(accountId, {
            signal: abortController.signal,
          }),
        }
      : await getAliasPage(accountId, {
          limit: selectedPageSize,
          offset: (page - 1) * selectedPageSize,
          signal: abortController.signal,
        });
    const currentKey = `${selectedAccountId.value}\u0000${currentPage.value}\u0000${pageSize.value}`;
    if (!aliasLoadGate.isCurrent(ticket, currentKey)) return;
    const nextTotal = Math.max(0, Number(result?.total) || 0);
    const nextAliases = Array.isArray(result?.items) ? result.items : [];
    const allItems = selectedPageSize === ALL_PAGE_SIZE;
    const resolvedTotal = allItems ? nextAliases.length : nextTotal;
    const lastPage = allItems
      ? 1
      : Math.max(1, Math.ceil(resolvedTotal / selectedPageSize));
    if (!allItems && page > lastPage) {
      currentPage.value = lastPage;
      aliases.value = [];
      total.value = resolvedTotal;
      void loadAliases();
      return;
    }
    aliases.value = nextAliases;
    total.value = resolvedTotal;
    const availableAliasIds = new Set(
      nextAliases.filter(isAliasExportable).map((alias) => alias.id),
    );
    selectedAliasIds.value = selectedAliasIds.value.filter((id) =>
      availableAliasIds.has(id),
    );
    loadError.value = null;
  } catch (error) {
    if (
      error?.name !== "AbortError" &&
      aliasLoadGate.isCurrent(
        ticket,
        `${selectedAccountId.value}\u0000${currentPage.value}\u0000${pageSize.value}`,
      ) &&
      !silent
    ) {
      loadError.value = error;
    }
  } finally {
    if (
      aliasAbortController === abortController &&
      aliasLoadGate.isCurrent(
        ticket,
        `${selectedAccountId.value}\u0000${currentPage.value}\u0000${pageSize.value}`,
      )
    ) {
      loading.value = false;
    }
    if (aliasAbortController === abortController) {
      aliasAbortController = null;
    }
  }
}

function clearAliasSelection() {
  selectedAliasIds.value = [];
}

function handleAccountFilterChange(value) {
  selectedAccountId.value = value == null ? "" : value;
  currentPage.value = 1;
  clearAliasSelection();
  aliases.value = [];
  total.value = 0;
  loadAliases();
}

function handlePageChange(page) {
  if (pageSize.value === ALL_PAGE_SIZE) return;
  const nextPage = Math.max(1, Number(page) || 1);
  if (nextPage === currentPage.value) return;
  currentPage.value = nextPage;
  clearAliasSelection();
  aliases.value = [];
  loadError.value = null;
  void loadAliases();
}

function handlePageSizeChange(value) {
  const nextPageSize = normalizePageSize(value);
  if (nextPageSize === pageSize.value) return;
  pageSize.value = nextPageSize;
  currentPage.value = 1;
  clearAliasSelection();
  aliases.value = [];
  total.value = 0;
  loadError.value = null;
  aliasLoadGate.invalidate();
  void loadAliases();
}

const liveRefresh = createLiveRefresh(() => {
  return loadAliases({ silent: true });
});

function isAliasSelected(id) {
  return selectedAliasIds.value.includes(id);
}

function setAliasSelected(alias, selected) {
  if (!isAliasExportable(alias)) return;
  const selectedIds = new Set(selectedAliasIds.value);
  if (selected) {
    selectedIds.add(alias.id);
  } else {
    selectedIds.delete(alias.id);
  }
  selectedAliasIds.value = [...selectedIds];
}

function setAllAliasesSelected(selected) {
  selectedAliasIds.value = selected
    ? exportableAliases.value.map((alias) => alias.id)
    : [];
}

function exportAliases(items, scope) {
  const exportableItems = items.filter(isAliasExportable);
  if (!exportableItems.length) return;

  let url = "";
  let link = null;
  try {
    const content = buildAliasExportText(exportableItems);
    const blob = new Blob([content], { type: "text/plain;charset=utf-8" });
    url = URL.createObjectURL(blob);
    link = document.createElement("a");
    const timestamp = new Date().toISOString().replace(/[:.]/g, "-");
    link.href = url;
    link.download = `icloud-aliases-${scope}-${timestamp}.txt`;
    document.body.appendChild(link);
    link.click();
    successMessage(`已导出 ${exportableItems.length} 个邮箱。`);
  } catch {
    ElMessage({
      type: "error",
      message: "邮箱导出失败，请刷新页面后重试。",
      grouping: true,
    });
  } finally {
    link?.remove();
    if (url) {
      window.setTimeout(() => URL.revokeObjectURL(url), 0);
    }
  }
}

function exportSelectedAliases() {
  exportAliases(selectedAliases.value, "selected");
}

function exportAllAliases() {
  if (exportingAll.value) return;
  exportingAll.value = true;
  getAllAliases(selectedAccountId.value)
    .then((items) => {
      if (viewActive) {
        exportAliases(items, selectedAccountId.value || "all");
      }
    })
    .catch((error) => {
      if (viewActive) {
        showRequestError(error, "导出隐私邮箱失败，请稍后重试。");
      }
    })
    .finally(() => {
      exportingAll.value = false;
    });
}

async function copyAliasDirectLink(alias) {
  if (!isAliasExportable(alias) || !copyLock.acquire(alias.id)) return;
  copyLoading[alias.id] = true;
  try {
    const directLink = buildRecentMailDirectLink(alias.directLinkPath);
    const copied = await copyText(directLink);
    if (!viewActive) return;
    if (!copied) {
      ElMessage({
        type: "error",
        message: "直达链接复制失败，请检查浏览器剪切板权限后重试。",
        grouping: true,
      });
      return;
    }
    successMessage("邮件 API 直达链接已复制。");
  } catch {
    if (!viewActive) return;
    ElMessage({
      type: "error",
      message: "直达链接复制失败，请刷新页面后重试。",
      grouping: true,
    });
  } finally {
    delete copyLoading[alias.id];
    copyLock.release(alias.id);
  }
}

function openAccounts() {
  router.push({ name: "accounts" });
}

function openAccount(id) {
  router.push({ name: "account-detail", params: { id } });
}

onMounted(() => {
  loadAccounts();
  loadAliases();
  liveRefresh.start({ immediate: false });
});

onBeforeUnmount(() => {
  viewActive = false;
  if (accountSearchTimer !== null) {
    window.clearTimeout(accountSearchTimer);
    accountSearchTimer = null;
  }
  aliasLoadGate.deactivate();
  accountsLoadGate.deactivate();
  aliasAbortController?.abort();
  liveRefresh.stop();
});
</script>

<style scoped>
.alias-filter {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: 8px;
}

.alias-filter__label {
  flex: 0 0 auto;
  color: var(--text-secondary);
  font-size: 13px;
  white-space: nowrap;
}

.alias-filter__select {
  width: min(280px, 30vw);
}

.account-link {
  max-width: 100%;
  justify-content: flex-start;
}

.account-link :deep(span) {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.mobile-alias-selection {
  display: flex;
  width: 100%;
  min-width: 0;
  max-width: 100%;
  flex: 1 1 auto;
  align-items: flex-start;
  gap: 10px;
}

.mobile-alias-selection .primary-stack {
  min-width: 0;
  flex: 1 1 auto;
}

.mobile-alias-selection__checkbox {
  flex: 0 0 auto;
  margin-top: 1px;
}

@media (max-width: 720px) {
  .alias-filter {
    width: 100%;
  }

  .alias-filter__select {
    min-width: 0;
    flex: 1 1 auto;
    width: auto;
  }
}
</style>
