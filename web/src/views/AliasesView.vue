<template>
  <section
    class="page-stack virtual-list-page"
    aria-labelledby="aliases-section-title"
  >
    <SectionHeader
      id="aliases-section-title"
      title="全部隐私邮箱"
      description="敏感凭证不在列表展示；可通过复制操作导出取码或 IMAP 凭证。"
    >
      <template #actions>
        <el-button
          :icon="CopyDocument"
          :disabled="selectedAliases.length === 0"
          @click="copySelectedAliases(ALIAS_EXPORT_OTP)"
        >
          勾选取码<span v-if="selectedAliases.length">
            （{{ selectedAliases.length }}）
          </span>
        </el-button>
        <el-button
          :icon="CopyDocument"
          :disabled="selectedAliases.length === 0"
          @click="copySelectedAliases(ALIAS_EXPORT_IMAP)"
        >
          勾选 IMAP
        </el-button>
        <el-select
          v-if="selectedAliasIds.length"
          v-model="moveTargetGroupId"
          class="alias-group-bulk-select"
          :loading="groupsLoading || movingAliases"
          :disabled="movingAliases"
          placeholder="移动到分组"
          aria-label="将勾选的隐私邮箱移动到分组"
          @change="moveSelectedAliases"
        >
          <el-option label="未分组" value="none" />
          <el-option
            v-for="group in groups"
            :key="group.id"
            :label="group.name"
            :value="String(group.id)"
          />
        </el-select>
        <el-button :icon="FolderAdd" @click="openGroupDialog()">
          管理分组
        </el-button>
        <el-button
          type="primary"
          :icon="CopyDocument"
          :loading="exportingAll"
          :disabled="total === 0 || exportingAll"
          @click="copyAllAliases(ALIAS_EXPORT_OTP)"
        >
          全部取码
        </el-button>
        <el-button
          type="primary"
          plain
          :icon="CopyDocument"
          :loading="exportingAll"
          :disabled="total === 0 || exportingAll"
          @click="copyAllAliases(ALIAS_EXPORT_IMAP)"
        >
          全部 IMAP
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

    <el-dialog
      v-model="groupDialogVisible"
      title="管理邮箱分组"
      width="min(520px, calc(100vw - 28px))"
      :close-on-click-modal="false"
    >
      <el-form class="dialog-form" @submit.prevent="saveGroup">
        <el-form-item :label="editingGroupId ? '重命名分组' : '新建分组'">
          <el-input
            v-model="groupNameDraft"
            maxlength="100"
            show-word-limit
            placeholder="例如：工作、购物、注册"
            @keyup.enter="saveGroup"
          />
        </el-form-item>
        <div class="dialog-actions dialog-actions--end">
          <el-button
            type="primary"
            :loading="groupSaving"
            :disabled="!groupNameDraft.trim()"
            @click="saveGroup"
          >
            {{ editingGroupId ? "保存名称" : "新建分组" }}
          </el-button>
          <el-button
            v-if="editingGroupId"
            :disabled="groupSaving"
            @click="cancelGroupEdit"
          >
            取消编辑
          </el-button>
        </div>
      </el-form>

      <el-divider />
      <div v-if="groups.length" class="mail-group-list">
        <div v-for="group in groups" :key="group.id" class="mail-group-row">
          <div class="mail-group-row__identity">
            <strong>{{ group.name }}</strong>
            <small>{{ group.aliasCount }} 个邮箱</small>
          </div>
          <div class="mail-group-row__actions">
            <el-button link type="primary" :icon="EditPen" :disabled="groupSaving" @click="editGroup(group)">
              重命名
            </el-button>
            <el-button link type="danger" :icon="Delete" :disabled="groupDeletingId === group.id" @click="removeGroup(group)">
              删除
            </el-button>
          </div>
        </div>
      </div>
      <EmptyState v-else level="h3" title="还没有分组" description="创建分组后即可把隐私邮箱移动进去。" />
    </el-dialog>

    <div class="alias-list-filters" role="search" aria-label="筛选全部隐私邮箱">
      <label class="alias-list-filter alias-list-filter--query">
        <span>关键词</span>
        <el-input
          v-model="keywordDraft"
          clearable
          maxlength="200"
          :prefix-icon="Search"
          aria-label="关键词：模糊搜索隐私邮箱"
          placeholder="邮箱地址或用途备注"
          @keyup.enter="applyAliasSearch"
          @clear="applyAliasSearch"
        />
      </label>

      <label class="alias-list-filter">
        <span>所属主号</span>
        <el-select
          v-model="selectedAccountId"
          filterable
          remote
          reserve-keyword
          clearable
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
            :label="formatAccountIdentity(account)"
            :value="account.id"
          >
            <div class="primary-stack">
              <strong>{{ formatAccountIdentity(account) }}</strong>
              <small>{{ account.name || "未填写备注" }}</small>
            </div>
          </el-option>
        </el-select>
      </label>

      <label class="alias-list-filter">
        <span>邮箱分组</span>
        <el-select
          v-model="selectedGroupFilter"
          clearable
          :loading="groupsLoading"
          placeholder="全部分组"
          aria-label="按邮箱分组筛选"
          @change="handleGroupFilterChange"
        >
          <el-option label="全部分组" value="" />
          <el-option label="未分组" value="none" />
          <el-option
            v-for="group in groups"
            :key="group.id"
            :label="`${group.name}（${group.aliasCount}）`"
            :value="String(group.id)"
          />
        </el-select>
      </label>

      <div class="alias-list-filter-actions">
        <el-button type="primary" :icon="Search" @click="applyAliasSearch">
          查询
        </el-button>
        <el-button
          :icon="RefreshLeft"
          :disabled="!hasActiveFilters && !hasAppliedFilters"
          @click="resetAliasFilters"
        >
          重置
        </el-button>
      </div>
    </div>

      <RequestAlert
        v-if="accountsLoadError || groupsError"
        :error="accountsLoadError || groupsError"
        closable
        @close="accountsLoadError = null; groupsError = null"
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
      :title="
        appliedAliasQuery || appliedGroupId
          ? '没有匹配的隐私邮箱'
          : selectedAccountId
            ? '该主号暂无隐私邮箱'
            : '还没有隐私邮箱'
      "
      :description="
        appliedAliasQuery || appliedGroupId
          ? '请尝试其他关键词，或调整所属主号和邮箱分组筛选。'
          : selectedAccountId
            ? '请选择其他主号，或进入该主号详情页添加地址。'
            : '进入某个主号详情页添加地址。'
      "
    >
      <el-button
        v-if="appliedAliasQuery || appliedGroupId"
        :icon="RefreshLeft"
        @click="resetAliasFilters"
      >
        重置筛选
      </el-button>
      <el-button v-else type="primary" :icon="Setting" @click="openAccounts">
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
              :model-value="allAliasesSelected"
              :indeterminate="someExportableAliasesSelected"
              :disabled="aliases.length === 0"
              aria-label="勾选本页隐私邮箱"
              @change="setAllAliasesSelected"
            />
            <template v-else>{{ column.title }}</template>
          </template>
          <template #cell="{ column, row }">
            <el-checkbox
              v-if="column.key === 'selection'"
              :model-value="isAliasSelected(row.id)"
              :disabled="false"
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
                {{ formatAliasAccountIdentity(row) || "查看主号" }}
              </el-button>
            </template>
            <template v-else-if="column.key === 'group'">
              <el-select
                class="alias-group-select"
                :model-value="row.groupId == null ? '' : String(row.groupId)"
                :loading="Boolean(movingAliasIds[row.id])"
                :disabled="Boolean(movingAliasIds[row.id])"
                aria-label="选择隐私邮箱分组"
                @change="moveAlias(row, $event)"
              >
                <el-option label="未分组" value="" />
                <el-option
                  v-for="group in groups"
                  :key="group.id"
                  :label="group.name"
                  :value="String(group.id)"
                />
              </el-select>
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
                <el-button
                  v-if="isAliasExportable(row)"
                  size="small"
                  :icon="CopyDocument"
                  :loading="Boolean(copyLoading[`${row.id}:otp`])"
                  @click="copyAliasLine(row, ALIAS_EXPORT_OTP)"
                >取码</el-button>
                <el-button
                  v-if="isAliasExportable(row)"
                  size="small"
                  :icon="CopyDocument"
                  :loading="Boolean(copyLoading[`${row.id}:imap`])"
                  @click="copyAliasLine(row, ALIAS_EXPORT_IMAP)"
                >IMAP</el-button>
                <el-button
                  v-if="isAliasReceiveAvailable(row)"
                  size="small"
                  type="primary"
                  plain
                  :icon="Message"
                  @click="openAliasInbox(row)"
                >收件</el-button>
                <el-button
                  v-if="isLegacyDirectLinkAvailable(row)"
                  size="small"
                  :icon="CopyDocument"
                  :loading="Boolean(copyLoading[`${row.id}:legacy-link`])"
                  @click="copyLegacyDirectLink(row)"
                >旧直达</el-button>
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
                :disabled="false"
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
              <dd>{{ formatAliasAccountIdentity(alias) || "-" }}</dd>
            </div>
            <div>
              <dt>最近调用</dt>
              <dd>{{ formatTime(alias.lastAccessedAt) }}</dd>
            </div>
            <div>
              <dt>最新邮件</dt>
              <dd>{{ formatTime(alias.latestReceivedAt) }}</dd>
            </div>
            <div>
              <dt>分组</dt>
              <dd>{{ alias.groupName || "未分组" }}</dd>
            </div>
          </dl>
          <footer class="mobile-record__actions mobile-record__actions--direct-link">
            <el-button
              v-if="isAliasExportable(alias)"
              :icon="CopyDocument"
              :loading="Boolean(copyLoading[`${alias.id}:otp`])"
              @click="copyAliasLine(alias, ALIAS_EXPORT_OTP)"
            >
              复制取码格式
            </el-button>
            <el-button
              v-if="isAliasExportable(alias)"
              :icon="CopyDocument"
              :loading="Boolean(copyLoading[`${alias.id}:imap`])"
              @click="copyAliasLine(alias, ALIAS_EXPORT_IMAP)"
            >
              复制 IMAP 格式
            </el-button>
            <el-button
              v-if="isAliasReceiveAvailable(alias)"
              type="primary"
              plain
              :icon="Message"
              @click="openAliasInbox(alias)"
            >
              收件
            </el-button>
            <el-button
              v-if="isLegacyDirectLinkAvailable(alias)"
              :icon="CopyDocument"
              :loading="Boolean(copyLoading[`${alias.id}:legacy-link`])"
              @click="copyLegacyDirectLink(alias)"
            >
              复制旧直达链接
            </el-button>
            <el-select
              class="mobile-alias-group-select"
              :model-value="alias.groupId == null ? '' : String(alias.groupId)"
              :loading="Boolean(movingAliasIds[alias.id])"
              :disabled="Boolean(movingAliasIds[alias.id])"
              aria-label="选择隐私邮箱分组"
              @change="moveAlias(alias, $event)"
            >
              <el-option label="未分组" value="" />
              <el-option
                v-for="group in groups"
                :key="group.id"
                :label="group.name"
                :value="String(group.id)"
              />
            </el-select>
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
  Delete,
  EditPen,
  FolderAdd,
  Message,
  Refresh,
  RefreshLeft,
  Search,
  Setting,
} from "@element-plus/icons-vue";
import { ElMessage, ElMessageBox } from "element-plus";
import { computed, onBeforeUnmount, onMounted, reactive, ref } from "vue";
import { useRouter } from "vue-router";

import {
  createMailGroup,
  deleteMailGroup,
  getAccountPage,
  getAliasPage,
  getAllAliases,
  getMailGroups,
  moveAliasToGroup,
  moveAliasesToGroup,
  updateMailGroup,
} from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import ListPagination from "../components/ListPagination.vue";
import RequestAlert from "../components/RequestAlert.vue";
import SectionHeader from "../components/SectionHeader.vue";
import SyncStatus from "../components/SyncStatus.vue";
import VirtualDataTable from "../components/VirtualDataTable.vue";
import { useAuth } from "../stores/auth.js";
import {
  createActionLock,
  createLatestRequestGate,
} from "../utils/asyncState.js";
import {
  ALIAS_EXPORT_IMAP,
  ALIAS_EXPORT_OTP,
  buildAliasReceiveLink,
  buildAliasExportText,
} from "../utils/aliasExport.js";
import { buildRecentMailDirectLink, copyText } from "../utils/clipboard.js";
import { showRequestError, successMessage } from "../utils/feedback.js";
import { formatTime } from "../utils/format.js";
import { createLiveRefresh } from "../utils/liveRefresh.js";
import {
  ALL_PAGE_SIZE,
  DEFAULT_PAGE_SIZE,
  normalizePageSize,
} from "../utils/pagination.js";

const router = useRouter();
const auth = useAuth();
const ACCOUNT_OPTION_LIMIT = 50;
const pageSize = ref(DEFAULT_PAGE_SIZE);
const aliasColumns = [
  { key: "selection", title: "", width: 52, align: "center", fixed: "left" },
  { key: "address", title: "隐私邮箱", width: 220, flexGrow: 2 },
  { key: "account", title: "所属主号", width: 190, flexGrow: 1 },
  { key: "group", title: "分组", width: 150, flexGrow: 1 },
  { key: "lastAccessedAt", title: "最近调用", width: 150, flexGrow: 1 },
  { key: "latestReceivedAt", title: "最新邮件", width: 150, flexGrow: 1 },
  { key: "status", title: "状态", width: 134, flexGrow: 1 },
  { key: "actions", title: "复制 / 收件 / 管理", width: 320, align: "right", fixed: "right" },
];
const aliases = ref([]);
const accounts = ref([]);
const selectedAccountId = ref("");
const selectedGroupFilter = ref("");
const appliedGroupId = ref("");
const moveTargetGroupId = ref("");
const groups = ref([]);
const groupsLoading = ref(false);
const groupsError = ref(null);
const groupDialogVisible = ref(false);
const groupNameDraft = ref("");
const editingGroupId = ref(null);
const groupSaving = ref(false);
const groupDeletingId = ref(null);
const movingAliases = ref(false);
const movingAliasIds = reactive({});
const keywordDraft = ref("");
const appliedAliasQuery = ref("");
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
const groupsLoadGate = createLatestRequestGate();
let accountSearchTimer = null;
let aliasAbortController = null;
let viewActive = true;

const selectedAliases = computed(() => {
  const selectedIds = new Set(selectedAliasIds.value);
  return aliases.value.filter(
    (alias) => selectedIds.has(alias.id) && isAliasExportable(alias),
  );
});

const allAliasesSelected = computed(
  () => aliases.value.length > 0 && aliases.value.every((alias) => isAliasSelected(alias.id)),
);

// Keep the established selector name for view/test compatibility. Selection
// now includes every mailbox so legacy aliases can also be moved in bulk.
const someExportableAliasesSelected = computed(() => {
  const selectedCount = aliases.value.filter((alias) => isAliasSelected(alias.id)).length;
  return selectedCount > 0 && selectedCount < aliases.value.length;
});

const hasActiveFilters = computed(() =>
  Boolean(
    selectedAccountId.value ||
      selectedGroupFilter.value ||
      keywordDraft.value.trim(),
  ),
);

const hasAppliedFilters = computed(() =>
  Boolean(selectedAccountId.value || appliedGroupId.value || appliedAliasQuery.value),
);

function isAliasConfirmationPending(alias) {
  return (
    !alias?.enabled &&
    String(alias?.lastSyncError || "").trim() ===
      "APPLE_ALIAS_CONFIRMATION_PENDING"
  );
}

function formatAccountIdentity(account) {
  const email = String(account?.email || account?.accountEmail || "");
  if (account?.mailboxType === "custom") {
    const suffix = String(account?.emailSuffix || "").replace(/^@+/, "");
    if (suffix) return `@${suffix}`;
  }
  if (account?.mailboxType == null && email.startsWith("custom@")) {
    return email.slice("custom".length);
  }
  return email;
}

function formatAliasAccountIdentity(alias) {
  const account = accounts.value.find(
    (item) => String(item.id) === String(alias?.accountId),
  );
  return formatAccountIdentity(account || alias);
}

function isAliasExportable(alias) {
  return Boolean(
    alias?.address &&
      alias?.apiKey &&
      alias?.imapPassword &&
      alias?.clientId &&
      alias?.refreshToken &&
      alias?.otpUrlPath,
  );
}

function isLegacyDirectLinkAvailable(alias) {
  return Boolean(
    !isAliasConfirmationPending(alias) &&
      alias?.credentialMode === "legacy" &&
      alias?.directLinkPath,
  );
}

function isAliasReceiveAvailable(alias) {
  return Boolean(
    !isAliasConfirmationPending(alias) &&
      (alias?.otpUrlPath || alias?.directLinkPath || alias?.legacyDirectLinkPath),
  );
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

async function loadGroups({ silent = false } = {}) {
  const ticket = groupsLoadGate.begin("groups");
  if (!silent) {
    groupsLoading.value = true;
    groupsError.value = null;
  }
  try {
    const nextGroups = await getMailGroups();
    if (!groupsLoadGate.isCurrent(ticket, "groups")) return;
    groups.value = nextGroups;
    groupsError.value = null;
  } catch (error) {
    if (!silent && groupsLoadGate.isCurrent(ticket, "groups")) {
      groupsError.value = error;
    }
  } finally {
    if (groupsLoadGate.isCurrent(ticket, "groups")) {
      groupsLoading.value = false;
    }
  }
}

async function loadAliases({ silent = false } = {}) {
  const accountId = selectedAccountId.value;
  const query = appliedAliasQuery.value;
  const groupId = appliedGroupId.value;
  const page = currentPage.value;
  const selectedPageSize = pageSize.value;
  const requestKey = `${accountId}\u0000${groupId}\u0000${query}\u0000${page}\u0000${selectedPageSize}`;
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
            query,
            groupId,
            signal: abortController.signal,
          }),
        }
      : await getAliasPage(accountId, {
          limit: selectedPageSize,
          offset: (page - 1) * selectedPageSize,
          query,
          groupId,
          signal: abortController.signal,
        });
    const currentKey = `${selectedAccountId.value}\u0000${appliedGroupId.value}\u0000${appliedAliasQuery.value}\u0000${currentPage.value}\u0000${pageSize.value}`;
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
    const availableAliasIds = new Set(nextAliases.map((alias) => alias.id));
    selectedAliasIds.value = selectedAliasIds.value.filter((id) =>
      availableAliasIds.has(id),
    );
    loadError.value = null;
  } catch (error) {
    if (
      error?.name !== "AbortError" &&
      aliasLoadGate.isCurrent(
        ticket,
        `${selectedAccountId.value}\u0000${appliedGroupId.value}\u0000${appliedAliasQuery.value}\u0000${currentPage.value}\u0000${pageSize.value}`,
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
        `${selectedAccountId.value}\u0000${appliedGroupId.value}\u0000${appliedAliasQuery.value}\u0000${currentPage.value}\u0000${pageSize.value}`,
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

function beginAliasMutation() {
  aliasLoadGate.invalidate();
  groupsLoadGate.invalidate();
  aliasAbortController?.abort();
  groupsLoading.value = false;
}

function reloadAliasesForFilters() {
  currentPage.value = 1;
  clearAliasSelection();
  aliases.value = [];
  total.value = 0;
  loadError.value = null;
  void loadAliases();
}

function applyAliasSearch() {
  const query = keywordDraft.value.trim();
  if (query === appliedAliasQuery.value) return;
  appliedAliasQuery.value = query;
  reloadAliasesForFilters();
}

function handleAccountFilterChange(value) {
  selectedAccountId.value = value == null ? "" : value;
  appliedAliasQuery.value = keywordDraft.value.trim();
  reloadAliasesForFilters();
}

function handleGroupFilterChange(value) {
  selectedGroupFilter.value = value == null ? "" : String(value);
  appliedGroupId.value = selectedGroupFilter.value;
  appliedAliasQuery.value = keywordDraft.value.trim();
  reloadAliasesForFilters();
}

function resetAliasFilters() {
  if (!hasActiveFilters.value && !hasAppliedFilters.value) return;
  keywordDraft.value = "";
  appliedAliasQuery.value = "";
  selectedAccountId.value = "";
  selectedGroupFilter.value = "";
  appliedGroupId.value = "";
  reloadAliasesForFilters();
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
  return Promise.all([
    loadAliases({ silent: true }),
    loadGroups({ silent: true }),
  ]);
});

function isAliasSelected(id) {
  return selectedAliasIds.value.includes(id);
}

function setAliasSelected(alias, selected) {
  const selectedIds = new Set(selectedAliasIds.value);
  if (selected) {
    selectedIds.add(alias.id);
  } else {
    selectedIds.delete(alias.id);
  }
  selectedAliasIds.value = [...selectedIds];
}

function setAllAliasesSelected(selected) {
  selectedAliasIds.value = selected ? aliases.value.map((alias) => alias.id) : [];
}

async function copyAliases(items, format, scope) {
  const exportableItems = items.filter(isAliasExportable);
  if (!exportableItems.length) return;

  try {
    const content = buildAliasExportText(exportableItems, format);
    const copied = await copyText(content);
    if (!copied) throw new Error("clipboard rejected copy");
    const label = format === ALIAS_EXPORT_OTP ? "取码链接" : "IMAP/OAuth";
    successMessage(`已复制${scope}${exportableItems.length} 个邮箱的${label}。`);
  } catch {
    ElMessage({
      type: "error",
      message: "邮箱凭证复制失败，请检查浏览器剪切板权限后重试。",
      grouping: true,
    });
  }
}

function copySelectedAliases(format) {
  return copyAliases(selectedAliases.value, format, "勾选的");
}

function copyAllAliases(format) {
  if (exportingAll.value) return;
  exportingAll.value = true;
  getAllAliases(selectedAccountId.value, {
    query: appliedAliasQuery.value,
    groupId: appliedGroupId.value === "none" ? "none" : appliedGroupId.value,
  })
    .then((items) => {
      if (viewActive) {
        return copyAliases(items, format, "全部");
      }
      return undefined;
    })
    .catch((error) => {
      if (viewActive) {
        showRequestError(error, "复制隐私邮箱凭证失败，请稍后重试。");
      }
    })
    .finally(() => {
      exportingAll.value = false;
    });
}

async function copyAliasLine(alias, format) {
  const lockKey = `${alias?.id}:${format}`;
  if (!isAliasExportable(alias) || !copyLock.acquire(lockKey)) return;
  copyLoading[lockKey] = true;
  try {
    const copied = await copyText(buildAliasExportText([alias], format));
    if (!viewActive) return;
    if (!copied) {
      ElMessage({
        type: "error",
        message: "邮箱凭证复制失败，请检查浏览器剪切板权限后重试。",
        grouping: true,
      });
      return;
    }
    successMessage(format === ALIAS_EXPORT_OTP ? "取码链接格式已复制。" : "IMAP/OAuth 格式已复制。");
  } catch {
    if (!viewActive) return;
    ElMessage({
      type: "error",
      message: "邮箱凭证复制失败，请刷新页面后重试。",
      grouping: true,
    });
  } finally {
    delete copyLoading[lockKey];
    copyLock.release(lockKey);
  }
}

async function copyLegacyDirectLink(alias) {
  const lockKey = `${alias?.id}:legacy-link`;
  if (!isLegacyDirectLinkAvailable(alias) || !copyLock.acquire(lockKey)) return;
  copyLoading[lockKey] = true;
  try {
    const directLink = buildRecentMailDirectLink(alias.directLinkPath);
    const copied = await copyText(directLink);
    if (!viewActive) return;
    if (!copied) throw new Error("clipboard rejected copy");
    successMessage("旧邮件 API 直达链接已复制。");
  } catch {
    if (viewActive) {
      ElMessage({
        type: "error",
        message: "旧直达链接复制失败，请刷新页面后重试。",
        grouping: true,
      });
    }
  } finally {
    delete copyLoading[lockKey];
    copyLock.release(lockKey);
  }
}

function openAliasInbox(alias) {
  if (!isAliasReceiveAvailable(alias)) return;
  try {
    const openedWindow = window.open(
      buildAliasReceiveLink(alias),
      "_blank",
      "noopener,noreferrer",
    );
    if (openedWindow) openedWindow.opener = null;
  } catch {
    ElMessage({
      type: "error",
      message: "取码链接打开失败，请刷新页面后重试。",
      grouping: true,
    });
  }
}

function openGroupDialog(group = null) {
  editingGroupId.value = group?.id ?? null;
  groupNameDraft.value = group?.name || "";
  groupDialogVisible.value = true;
}

function editGroup(group) {
  openGroupDialog(group);
}

function cancelGroupEdit() {
  editingGroupId.value = null;
  groupNameDraft.value = "";
}

async function saveGroup() {
  const name = groupNameDraft.value.trim();
  if (!name || groupSaving.value) return;
  beginAliasMutation();
  groupSaving.value = true;
  try {
    if (editingGroupId.value) {
      const updated = await updateMailGroup(
        editingGroupId.value,
        name,
        auth.state.csrfToken,
      );
      groups.value = groups.value
        .map((group) => (group.id === updated.id ? updated : group))
        .sort((left, right) => left.name.localeCompare(right.name));
      successMessage("邮箱分组名称已更新。");
    } else {
      const created = await createMailGroup(name, auth.state.csrfToken);
      groups.value = [...groups.value, created].sort((left, right) =>
        left.name.localeCompare(right.name),
      );
      successMessage("邮箱分组已创建。");
    }
    cancelGroupEdit();
    await loadAliases();
  } catch (error) {
    showRequestError(error, "邮箱分组保存失败，请稍后重试。");
  } finally {
    groupSaving.value = false;
  }
}

async function removeGroup(group) {
  if (!group || groupDeletingId.value) return;
  try {
    await ElMessageBox.confirm(
      group.aliasCount
        ? `删除“${group.name}”后，其中 ${group.aliasCount} 个邮箱会变为未分组。继续吗？`
        : `确定删除分组“${group.name}”吗？`,
      "删除邮箱分组",
      {
        type: "warning",
        confirmButtonText: "删除",
        cancelButtonText: "取消",
        confirmButtonClass: "el-button--danger",
        autofocus: false,
      },
    );
  } catch {
    return;
  }
  groupDeletingId.value = group.id;
  beginAliasMutation();
  try {
    await deleteMailGroup(group.id, auth.state.csrfToken);
    groups.value = groups.value.filter((item) => item.id !== group.id);
    if (editingGroupId.value === group.id) {
      cancelGroupEdit();
    }
    if (selectedGroupFilter.value === String(group.id)) {
      selectedGroupFilter.value = "";
      appliedGroupId.value = "";
      reloadAliasesForFilters();
    } else {
      await loadAliases();
    }
    successMessage("邮箱分组已删除，邮箱已恢复为未分组。");
  } catch (error) {
    showRequestError(error, "邮箱分组删除失败，请稍后重试。");
  } finally {
    groupDeletingId.value = null;
  }
}

async function moveSelectedAliases(groupValue) {
  if (!selectedAliasIds.value.length || movingAliases.value) return;
  beginAliasMutation();
  movingAliases.value = true;
  moveTargetGroupId.value = groupValue == null ? "" : String(groupValue);
  const targetGroupId = moveTargetGroupId.value === "none"
    ? null
    : moveTargetGroupId.value;
  try {
    await moveAliasesToGroup(
      selectedAliasIds.value,
      targetGroupId || null,
      auth.state.csrfToken,
    );
    clearAliasSelection();
    await Promise.all([loadAliases(), loadGroups()]);
    successMessage("已将勾选的隐私邮箱移动到所选分组。");
  } catch (error) {
    showRequestError(error, "邮箱分组移动失败，请稍后重试。");
  } finally {
    movingAliases.value = false;
    moveTargetGroupId.value = "";
  }
}

async function moveAlias(alias, groupValue) {
  if (!alias || movingAliasIds[alias.id]) return;
  beginAliasMutation();
  movingAliasIds[alias.id] = true;
  try {
    const updated = await moveAliasToGroup(
      alias.id,
      groupValue === "" || groupValue == null ? null : groupValue,
      auth.state.csrfToken,
    );
    aliases.value = aliases.value.map((item) =>
      item.id === updated.id ? updated : item,
    );
    await Promise.all([
      loadAliases(),
      loadGroups(),
    ]);
    successMessage("隐私邮箱分组已更新。");
  } catch (error) {
    showRequestError(error, "隐私邮箱分组移动失败，请稍后重试。");
  } finally {
    delete movingAliasIds[alias.id];
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
  loadGroups();
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
  groupsLoadGate.deactivate();
  aliasAbortController?.abort();
  liveRefresh.stop();
});
</script>

<style scoped>
.alias-list-filters {
  display: grid;
  grid-template-columns: minmax(220px, 1.3fr) minmax(180px, 1fr) minmax(180px, 1fr) auto;
  align-items: end;
  gap: 14px;
  padding: 16px;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: 6px;
}

.alias-group-bulk-select {
  width: 150px;
}

.alias-group-select {
  width: 132px;
}

.mobile-alias-group-select {
  min-width: 132px;
}

.mail-group-list {
  display: grid;
  gap: 8px;
}

.mail-group-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: 6px;
}

.mail-group-row__identity {
  display: grid;
  min-width: 0;
  gap: 3px;
}

.mail-group-row__identity strong {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.mail-group-row__identity small {
  color: var(--text-secondary);
  font-size: 12px;
}

.mail-group-row__actions {
  display: flex;
  flex: 0 0 auto;
  gap: 4px;
}

.alias-list-filter {
  display: grid;
  min-width: 0;
  gap: 6px;
}

.alias-list-filter > span {
  color: var(--text-secondary);
  font-size: 12px;
  font-weight: 600;
}

.alias-list-filter-actions {
  display: flex;
  align-items: center;
  gap: 8px;
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

@media (max-width: 1080px) {
  .alias-list-filters {
    grid-template-columns: minmax(220px, 1.25fr) minmax(180px, 1fr);
  }

  .alias-list-filter-actions {
    grid-column: 1 / -1;
    justify-content: flex-end;
  }
}

@media (max-width: 720px) {
  .alias-list-filters {
    grid-template-columns: minmax(0, 1fr);
    padding: 14px;
  }

  .alias-list-filter-actions > .el-button {
    min-width: 0;
    flex: 1 1 0;
  }

  .alias-list-filter-actions {
    grid-column: auto;
  }
}
</style>
