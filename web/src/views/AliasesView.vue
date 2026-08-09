<template>
  <section class="page-stack" aria-labelledby="aliases-section-title">
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
            clearable
            :disabled="accountsLoading || accounts.length === 0"
            :loading="accountsLoading"
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
          :disabled="exportableAliases.length === 0"
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

      <div class="data-panel desktop-data-table" :aria-busy="loading">
        <el-table
          ref="aliasTable"
          :data="aliases"
          row-key="id"
          style="width: 100%"
          @selection-change="handleSelectionChange"
        >
          <el-table-column
            type="selection"
            width="52"
            reserve-selection
            :selectable="isAliasExportable"
          />
          <el-table-column label="隐私邮箱" min-width="220">
            <template #default="{ row }">
              <div class="primary-stack">
                <strong>{{ row.address }}</strong>
                <small>{{ row.label || "未填写用途备注" }}</small>
              </div>
            </template>
          </el-table-column>
          <el-table-column label="所属主号" min-width="190">
            <template #default="{ row }">
              <el-button
                class="account-link"
                link
                type="primary"
                @click="openAccount(row.accountId)"
              >
                {{ row.accountEmail || "查看主号" }}
              </el-button>
            </template>
          </el-table-column>
          <el-table-column label="API Key" min-width="120">
            <template #default="{ row }">
              <code class="key-prefix">{{ keyPrefix(row) }}</code>
            </template>
          </el-table-column>
          <el-table-column label="最近调用" min-width="150">
            <template #default="{ row }">
              {{ formatTime(row.lastAccessedAt) }}
            </template>
          </el-table-column>
          <el-table-column label="最新邮件" min-width="150">
            <template #default="{ row }">
              {{ formatTime(row.latestReceivedAt) }}
            </template>
          </el-table-column>
          <el-table-column label="状态" min-width="124">
            <template #default="{ row }">
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
          </el-table-column>
          <el-table-column label="操作" width="152" align="right" fixed="right">
            <template #default="{ row }">
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
          </el-table-column>
        </el-table>
        <div v-if="loading" class="table-loading-mask" aria-hidden="true"></div>
      </div>

      <div class="mobile-record-list" :aria-busy="loading">
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

import { getAccounts, getAliases } from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import RequestAlert from "../components/RequestAlert.vue";
import SectionHeader from "../components/SectionHeader.vue";
import SyncStatus from "../components/SyncStatus.vue";
import {
  createActionLock,
  createLatestRequestGate,
} from "../utils/asyncState.js";
import { buildAliasExportText } from "../utils/aliasExport.js";
import {
  buildRecentMailDirectLink,
  copyText,
} from "../utils/clipboard.js";
import { successMessage } from "../utils/feedback.js";
import { formatTime } from "../utils/format.js";
import { createLiveRefresh } from "../utils/liveRefresh.js";

const router = useRouter();
const aliases = ref([]);
const accounts = ref([]);
const selectedAccountId = ref("");
const aliasTable = ref(null);
const selectedAliasIds = ref([]);
const loading = ref(false);
const loadError = ref(null);
const accountsLoading = ref(false);
const accountsLoadError = ref(null);
const copyLoading = reactive({});
const copyLock = createActionLock();
const aliasLoadGate = createLatestRequestGate();
const accountsLoadGate = createLatestRequestGate();
let viewActive = true;

const selectedAliases = computed(() => {
  const selectedIds = new Set(selectedAliasIds.value);
  return aliases.value.filter(
    (alias) => selectedIds.has(alias.id) && isAliasExportable(alias),
  );
});
const exportableAliases = computed(() => aliases.value.filter(isAliasExportable));

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

async function loadAccounts({ silent = false } = {}) {
  const ticket = accountsLoadGate.begin("accounts");
  if (!silent) {
    accountsLoading.value = true;
    accountsLoadError.value = null;
  }
  try {
    const nextAccounts = await getAccounts();
    if (!accountsLoadGate.isCurrent(ticket)) return;
    accounts.value = nextAccounts;
    accountsLoadError.value = null;

    if (
      selectedAccountId.value &&
      !nextAccounts.some(
        (account) => String(account.id) === String(selectedAccountId.value),
      )
    ) {
      selectedAccountId.value = "";
      clearAliasSelection();
      void loadAliases({ silent });
    }
  } catch (error) {
    if (accountsLoadGate.isCurrent(ticket) && !silent) {
      accountsLoadError.value = error;
    }
  } finally {
    if (accountsLoadGate.isCurrent(ticket)) {
      accountsLoading.value = false;
    }
  }
}

async function loadAliases({ silent = false } = {}) {
  const accountId = selectedAccountId.value;
  const ticket = aliasLoadGate.begin(accountId);
  if (!silent) {
    loading.value = true;
    loadError.value = null;
  }
  try {
    const nextAliases = await getAliases(accountId);
    if (!aliasLoadGate.isCurrent(ticket, selectedAccountId.value)) return;
    aliases.value = nextAliases;
    const availableAliasIds = new Set(
      nextAliases.filter(isAliasExportable).map((alias) => alias.id),
    );
    selectedAliasIds.value = selectedAliasIds.value.filter((id) =>
      availableAliasIds.has(id),
    );
    loadError.value = null;
  } catch (error) {
    if (aliasLoadGate.isCurrent(ticket, selectedAccountId.value) && !silent) {
      loadError.value = error;
    }
  } finally {
    if (aliasLoadGate.isCurrent(ticket, selectedAccountId.value)) {
      loading.value = false;
    }
  }
}

function clearAliasSelection() {
  selectedAliasIds.value = [];
  aliasTable.value?.clearSelection();
}

function handleAccountFilterChange(value) {
  selectedAccountId.value = value == null ? "" : value;
  clearAliasSelection();
  loadAliases();
}

const liveRefresh = createLiveRefresh(() => {
  return Promise.all([
    loadAliases({ silent: true }),
    loadAccounts({ silent: true }),
  ]);
});

function handleSelectionChange(selection) {
  selectedAliasIds.value = selection
    .filter(isAliasExportable)
    .map((alias) => alias.id);
}

function isAliasSelected(id) {
  return selectedAliasIds.value.includes(id);
}

function setAliasSelected(alias, selected) {
  if (!isAliasExportable(alias)) return;
  if (aliasTable.value) {
    aliasTable.value.toggleRowSelection(alias, selected);
    return;
  }

  const selectedIds = new Set(selectedAliasIds.value);
  if (selected) {
    selectedIds.add(alias.id);
  } else {
    selectedIds.delete(alias.id);
  }
  selectedAliasIds.value = [...selectedIds];
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
  exportAliases(exportableAliases.value, "all");
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
  aliasLoadGate.deactivate();
  accountsLoadGate.deactivate();
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
