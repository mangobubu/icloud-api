<template>
  <section class="page-stack" aria-labelledby="accounts-section-title">
    <SectionHeader
      id="accounts-section-title"
      title="iCloud 主号"
      description="隐私邮箱通过所属主号的 IMAP 收取邮件。"
    >
      <template #actions>
        <el-tooltip content="刷新主号列表" placement="bottom">
          <el-button
            :icon="Refresh"
            circle
            :loading="loading"
            aria-label="刷新主号列表"
            @click="loadAccounts"
          />
        </el-tooltip>
        <el-button type="primary" :icon="Plus" @click="openNewAccount">
          添加主号
        </el-button>
      </template>
    </SectionHeader>

    <div v-if="loading && accounts.length === 0" class="data-panel loading-panel">
      <el-skeleton :rows="5" animated />
    </div>

    <div v-else-if="loadError && accounts.length === 0" class="load-failed">
      <RequestAlert :error="loadError" />
      <el-button :icon="Refresh" @click="loadAccounts">重新加载</el-button>
    </div>

    <EmptyState
      v-else-if="accounts.length === 0"
      title="还没有主号"
      description="添加一个 iCloud 主号及其 App 专用密码，然后登记隐私邮箱。"
    >
      <el-button type="primary" :icon="Plus" @click="openNewAccount">
        添加第一个主号
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
        class="data-panel desktop-data-table"
        :class="{
          'desktop-data-table--force': pageSize > 100 || pageSize === ALL_PAGE_SIZE,
        }"
        :aria-busy="loading"
      >
        <VirtualDataTable
          :columns="accountColumns"
          :data="accounts"
          row-key="id"
          :loading="loading"
        >
          <template #cell="{ column, row }">
            <template v-if="column.key === 'account'">
              <div class="primary-stack">
                <strong>{{ row.email }}</strong>
                <small>{{ row.name || "未填写备注" }}</small>
              </div>
            </template>
            <template v-else-if="column.key === 'status'">
              <SyncStatus :item="row" />
            </template>
            <template v-else-if="column.key === 'aliasCount'">
              {{ row.aliasCount }}
            </template>
            <template v-else-if="column.key === 'lastSyncedAt'">
              {{ formatTime(row.lastSyncedAt, { seconds: true }) }}
            </template>
            <template v-else-if="column.key === 'actions'">
              <el-button
                link
                type="primary"
                :icon="Setting"
                @click="openAccount(row.id)"
              >
                管理
              </el-button>
            </template>
          </template>
        </VirtualDataTable>
      </div>

      <div v-if="pageSize <= 100" class="mobile-record-list" :aria-busy="loading">
        <article v-for="account in accounts" :key="account.id" class="mobile-record">
          <header class="mobile-record__header">
            <div class="primary-stack">
              <strong>{{ account.email }}</strong>
              <small>{{ account.name || "未填写备注" }}</small>
            </div>
            <SyncStatus :item="account" />
          </header>
          <dl class="mobile-kv-list">
            <div>
              <dt>隐私邮箱</dt>
              <dd>{{ account.aliasCount }}</dd>
            </div>
            <div>
              <dt>最近同步</dt>
              <dd>{{ formatTime(account.lastSyncedAt, { seconds: true }) }}</dd>
            </div>
          </dl>
          <footer class="mobile-record__actions">
            <el-button :icon="Setting" @click="openAccount(account.id)">
              管理主号
            </el-button>
          </footer>
        </article>
      </div>

      <ListPagination
        :page="currentPage"
        :page-size="pageSize"
        :total="total"
        :loading="loading"
        aria-label="主号列表分页"
        @change="handlePageChange"
        @size-change="handlePageSizeChange"
      />
    </template>
  </section>
</template>

<script setup>
import { Plus, Refresh, Setting } from "@element-plus/icons-vue";
import { onBeforeUnmount, onMounted, ref } from "vue";
import { useRouter } from "vue-router";

import { getAccountPage, getAllAccounts } from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import ListPagination from "../components/ListPagination.vue";
import RequestAlert from "../components/RequestAlert.vue";
import SectionHeader from "../components/SectionHeader.vue";
import SyncStatus from "../components/SyncStatus.vue";
import VirtualDataTable from "../components/VirtualDataTable.vue";
import { formatTime } from "../utils/format.js";
import { createLatestRequestGate } from "../utils/asyncState.js";
import { createLiveRefresh } from "../utils/liveRefresh.js";
import {
  ALL_PAGE_SIZE,
  DEFAULT_PAGE_SIZE,
  normalizePageSize,
} from "../utils/pagination.js";

const router = useRouter();
const pageSize = ref(DEFAULT_PAGE_SIZE);
const accountColumns = [
  { key: "account", title: "主号", width: 250, flexGrow: 3 },
  { key: "status", title: "状态", width: 140, flexGrow: 1 },
  { key: "aliasCount", title: "隐私邮箱", width: 112, align: "center" },
  { key: "lastSyncedAt", title: "最近同步", width: 180, flexGrow: 1 },
  { key: "actions", title: "操作", width: 118, align: "right", fixed: "right" },
];
const accounts = ref([]);
const currentPage = ref(1);
const total = ref(0);
const loading = ref(false);
const loadError = ref(null);
const loadGate = createLatestRequestGate();
let listAbortController = null;
let viewActive = true;

async function loadAccounts({ silent = false } = {}) {
  const page = currentPage.value;
  const selectedPageSize = pageSize.value;
  const requestKey = `${page}\u0000${selectedPageSize}`;
  const ticket = loadGate.begin(requestKey);
  listAbortController?.abort();
  const abortController = new AbortController();
  listAbortController = abortController;
  if (!silent) {
    loading.value = true;
    loadError.value = null;
  }
  try {
    const result = selectedPageSize === ALL_PAGE_SIZE
      ? {
          items: await getAllAccounts({ signal: abortController.signal }),
        }
      : await getAccountPage({
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
      accounts.value = [];
      total.value = resolvedTotal;
      void loadAccounts();
      return;
    }
    accounts.value = nextItems;
    total.value = resolvedTotal;
    loadError.value = null;
  } catch (error) {
    if (
      error?.name !== "AbortError" &&
      viewActive &&
      loadGate.isCurrent(ticket, `${currentPage.value}\u0000${pageSize.value}`) &&
      !silent
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
  accounts.value = [];
  loadError.value = null;
  void loadAccounts();
}

function handlePageSizeChange(value) {
  const nextPageSize = normalizePageSize(value);
  if (nextPageSize === pageSize.value) return;
  pageSize.value = nextPageSize;
  currentPage.value = 1;
  accounts.value = [];
  total.value = 0;
  loadError.value = null;
  loadGate.invalidate();
  void loadAccounts();
}

const liveRefresh = createLiveRefresh(() => loadAccounts({ silent: true }));

function openNewAccount() {
  router.push({ name: "account-new" });
}

function openAccount(id) {
  router.push({ name: "account-detail", params: { id } });
}

onMounted(() => {
  loadAccounts();
  liveRefresh.start({ immediate: false });
});

onBeforeUnmount(() => {
  viewActive = false;
  loadGate.deactivate();
  listAbortController?.abort();
  liveRefresh.stop();
});
</script>
