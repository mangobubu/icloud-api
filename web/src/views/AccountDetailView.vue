<template>
  <section class="page-stack">
    <div v-if="loading && !account" class="data-panel loading-panel">
      <el-skeleton :rows="9" animated />
    </div>

    <div v-else-if="loadError && !account" class="load-failed">
      <RequestAlert :error="loadError" />
      <div class="button-row">
        <el-button :icon="Back" @click="router.push({ name: 'accounts' })">
          返回主号列表
        </el-button>
        <el-button type="primary" :icon="Refresh" @click="loadDetail">
          重新加载
        </el-button>
      </div>
    </div>

    <template v-else-if="account">
      <div v-if="apiKey" ref="secretRegion" class="secret-region">
        <OneTimeSecret
          :value="apiKey"
          :direct-link-path="apiDirectLinkPath"
        />
        <el-tooltip content="关闭 Key 提示" placement="left">
          <el-button
            class="secret-region__close"
            :icon="Close"
            circle
            aria-label="关闭 Key 提示"
            @click="clearSecret"
          />
        </el-tooltip>
      </div>

      <RequestAlert
        v-if="loadError"
        :error="loadError"
        closable
        @close="loadError = null"
      />

      <section class="section-block" aria-labelledby="connection-title">
        <SectionHeader
          id="connection-title"
          title="连接配置"
          :description="`${account.imapUsername} · ${account.imapHost}:${account.imapPort}`"
        >
          <template #actions>
            <el-button
              :icon="Refresh"
              :loading="syncLoading"
              @click="syncNow"
            >
              立即同步
            </el-button>
            <el-button :icon="EditPen" @click="editAccount">编辑</el-button>
          </template>
        </SectionHeader>

        <dl class="detail-grid">
          <div>
            <dt>状态</dt>
            <dd><SyncStatus :item="account" /></dd>
          </div>
          <div>
            <dt>最近同步</dt>
            <dd>{{ formatTime(account.lastSyncedAt, { seconds: true }) }}</dd>
          </div>
          <div>
            <dt>主号邮箱</dt>
            <dd>{{ account.email }}</dd>
          </div>
          <div>
            <dt>备注</dt>
            <dd>{{ account.name || "-" }}</dd>
          </div>
        </dl>

        <div v-if="account.lastSyncError" class="inline-error" role="status">
          <strong>最近错误</strong>
          <span>{{ account.lastSyncError }}</span>
        </div>
      </section>

      <section class="section-block" aria-labelledby="aliases-title">
        <SectionHeader
          id="aliases-title"
          title="隐私邮箱"
          description="每个地址使用独立 API Key，只返回自己的最新一封邮件。"
        />

        <div v-if="aliases.length" class="data-panel desktop-data-table">
          <el-table :data="aliases" row-key="id" style="width: 100%">
            <el-table-column label="隐私邮箱" min-width="230">
              <template #default="{ row }">
                <div class="primary-stack">
                  <strong>{{ row.address }}</strong>
                  <small>{{ row.label || "未填写用途备注" }}</small>
                </div>
              </template>
            </el-table-column>
            <el-table-column label="API Key" min-width="126">
              <template #default="{ row }">
                <code class="key-prefix">{{ keyPrefix(row) }}</code>
              </template>
            </el-table-column>
            <el-table-column label="最新邮件" min-width="170">
              <template #default="{ row }">{{ formatTime(row.latestReceivedAt) }}</template>
            </el-table-column>
            <el-table-column label="同步状态" min-width="130">
              <template #default="{ row }"><SyncStatus :item="row" details /></template>
            </el-table-column>
            <el-table-column label="启用" width="82" align="center">
              <template #default="{ row }">
                <el-switch
                  :model-value="row.enabled"
                  :loading="Boolean(toggleLoading[row.id])"
                  :disabled="Boolean(copyLoading[row.id] || rotateLoading[row.id] || deleteLoading[row.id])"
                  :aria-label="`启用隐私邮箱 ${row.address}`"
                  @change="(enabled) => toggleAlias(row, enabled)"
                />
              </template>
            </el-table-column>
            <el-table-column label="操作" width="144" align="right">
              <template #default="{ row }">
                <div class="icon-action-row">
                  <el-tooltip content="复制邮件 API 直达链接" placement="top">
                    <el-button
                      :icon="CopyDocument"
                      circle
                      :loading="Boolean(copyLoading[row.id])"
                      :disabled="Boolean(!row.directLinkPath || toggleLoading[row.id] || rotateLoading[row.id] || deleteLoading[row.id])"
                      :aria-label="`复制 ${row.address} 的邮件 API 直达链接`"
                      @click="copyAliasDirectLink(row)"
                    />
                  </el-tooltip>
                  <el-tooltip content="轮换 API Key" placement="top">
                    <el-button
                      :icon="Key"
                      circle
                      :loading="Boolean(rotateLoading[row.id])"
                      :disabled="Boolean(apiKey || copyLoading[row.id] || toggleLoading[row.id] || deleteLoading[row.id])"
                      :aria-label="`轮换 ${row.address} 的 API Key`"
                      @click="rotateKey(row)"
                    />
                  </el-tooltip>
                  <el-tooltip content="删除隐私邮箱" placement="top">
                    <el-button
                      type="danger"
                      plain
                      :icon="Delete"
                      circle
                      :loading="Boolean(deleteLoading[row.id])"
                      :disabled="Boolean(copyLoading[row.id] || toggleLoading[row.id] || rotateLoading[row.id])"
                      :aria-label="`删除隐私邮箱 ${row.address}`"
                      @click="removeAlias(row)"
                    />
                  </el-tooltip>
                </div>
              </template>
            </el-table-column>
          </el-table>
        </div>

        <div v-if="aliases.length" class="mobile-record-list">
          <article v-for="alias in aliases" :key="alias.id" class="mobile-record">
            <header class="mobile-record__header">
              <div class="primary-stack">
                <strong>{{ alias.address }}</strong>
                <small>{{ alias.label || "未填写用途备注" }}</small>
              </div>
              <SyncStatus :item="alias" details />
            </header>
            <dl class="mobile-kv-list">
              <div>
                <dt>API Key</dt>
                <dd><code class="key-prefix">{{ keyPrefix(alias) }}</code></dd>
              </div>
              <div>
                <dt>最新邮件</dt>
                <dd>{{ formatTime(alias.latestReceivedAt) }}</dd>
              </div>
              <div>
                <dt>启用</dt>
                <dd>
                  <el-switch
                    :model-value="alias.enabled"
                    :loading="Boolean(toggleLoading[alias.id])"
                    :disabled="Boolean(copyLoading[alias.id] || rotateLoading[alias.id] || deleteLoading[alias.id])"
                    :aria-label="`启用隐私邮箱 ${alias.address}`"
                    @change="(enabled) => toggleAlias(alias, enabled)"
                  />
                </dd>
              </div>
            </dl>
            <footer class="mobile-record__actions mobile-record__actions--three">
              <el-button
                :icon="CopyDocument"
                :loading="Boolean(copyLoading[alias.id])"
                :disabled="Boolean(!alias.directLinkPath || toggleLoading[alias.id] || rotateLoading[alias.id] || deleteLoading[alias.id])"
                :aria-label="`复制 ${alias.address} 的邮件 API 直达链接`"
                @click="copyAliasDirectLink(alias)"
              >
                复制邮件 API 直达链接
              </el-button>
              <el-button
                :icon="Key"
                :loading="Boolean(rotateLoading[alias.id])"
                :disabled="Boolean(apiKey || copyLoading[alias.id] || toggleLoading[alias.id] || deleteLoading[alias.id])"
                @click="rotateKey(alias)"
              >
                轮换 Key
              </el-button>
              <el-button
                type="danger"
                plain
                :icon="Delete"
                :loading="Boolean(deleteLoading[alias.id])"
                :disabled="Boolean(copyLoading[alias.id] || toggleLoading[alias.id] || rotateLoading[alias.id])"
                @click="removeAlias(alias)"
              >
                删除
              </el-button>
            </footer>
          </article>
        </div>

        <EmptyState
          v-else
          class="empty-state--compact"
          level="h3"
          title="尚未登记隐私邮箱"
          description="添加一个已经转发到此主号的 Hide My Email 地址。"
        />

        <el-form
          ref="aliasFormRef"
          class="inline-alias-form"
          :model="aliasForm"
          :rules="aliasRules"
          label-position="top"
          :disabled="Boolean(apiKey) || createLoading"
          @submit.prevent="addAlias"
        >
          <RequestAlert
            v-if="aliasFormError"
            class="form-span"
            :error="aliasFormError"
            closable
            @close="aliasFormError = null"
          />
          <el-form-item label="隐私邮箱地址" prop="address">
            <el-input
              v-model="aliasForm.address"
              type="email"
              placeholder="random@icloud.com"
              autocomplete="off"
            />
          </el-form-item>
          <el-form-item label="用途备注" prop="label">
            <el-input
              v-model="aliasForm.label"
              maxlength="100"
              show-word-limit
              placeholder="例如：登录验证码"
              autocomplete="off"
            />
          </el-form-item>
          <el-button
            class="inline-alias-form__submit"
            native-type="submit"
            type="primary"
            :icon="Plus"
            :loading="createLoading"
            :disabled="Boolean(apiKey)"
          >
            添加并生成 Key
          </el-button>
        </el-form>
      </section>

      <section class="danger-zone" aria-labelledby="delete-account-title">
        <div>
          <h2 id="delete-account-title">删除主号</h2>
          <p>同时删除其隐私邮箱、API Key 与本地最新邮件。</p>
        </div>
        <el-button
          type="danger"
          plain
          :icon="Delete"
          :loading="accountDeleteLoading"
          @click="removeAccount"
        >
          删除主号
        </el-button>
      </section>
    </template>
  </section>
</template>

<script setup>
import {
  Back,
  Close,
  CopyDocument,
  Delete,
  EditPen,
  Key,
  Plus,
  Refresh,
} from "@element-plus/icons-vue";
import { ElMessage, ElMessageBox } from "element-plus";
import {
  nextTick,
  onBeforeUnmount,
  onMounted,
  reactive,
  ref,
  watch,
} from "vue";
import {
  onBeforeRouteLeave,
  onBeforeRouteUpdate,
  useRoute,
  useRouter,
} from "vue-router";

import {
  createAlias,
  deleteAccount,
  deleteAlias,
  getAccount,
  rotateAlias,
  setAliasEnabled,
  syncAccount,
} from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import OneTimeSecret from "../components/OneTimeSecret.vue";
import RequestAlert from "../components/RequestAlert.vue";
import SectionHeader from "../components/SectionHeader.vue";
import SyncStatus from "../components/SyncStatus.vue";
import { useAuth } from "../stores/auth.js";
import { setPageHeader } from "../stores/page.js";
import {
  createActionLock,
  createLatestRequestGate,
  oneTimeSecretNavigationMode,
} from "../utils/asyncState.js";
import {
  buildRecentMailDirectLink,
  copyText,
} from "../utils/clipboard.js";
import {
  confirmationCancelled,
  showRequestError,
  successMessage,
} from "../utils/feedback.js";
import { formatTime } from "../utils/format.js";
import { createLiveRefresh } from "../utils/liveRefresh.js";

const route = useRoute();
const router = useRouter();
const auth = useAuth();
const account = ref(null);
const aliases = ref([]);
const apiKey = ref("");
const apiDirectLinkPath = ref("");
const secretRegion = ref(null);
const loading = ref(false);
const loadError = ref(null);
const syncLoading = ref(false);
const createLoading = ref(false);
const accountDeleteLoading = ref(false);
const aliasFormRef = ref(null);
const aliasFormError = ref(null);
const copyLoading = reactive({});
const toggleLoading = reactive({});
const rotateLoading = reactive({});
const deleteLoading = reactive({});
const detailGate = createLatestRequestGate();
const createLock = createActionLock();
const aliasActionLock = createActionLock();
const accountDeleteLock = createActionLock();
const secretNavigationLock = createActionLock();
let viewActive = true;

const aliasForm = reactive({ address: "", label: "" });

function detailRouteKey(id = route.params.id) {
  return String(id || "");
}

function isCurrentAccount(accountId) {
  return (
    viewActive &&
    route.name === "account-detail" &&
    detailRouteKey() === String(accountId || "")
  );
}

function hasPendingSecretRequest() {
  return createLoading.value || Object.keys(rotateLoading).length > 0;
}

function hasProtectedSecret() {
  return secretNavigationMode() !== "allow";
}

function secretNavigationMode() {
  return oneTimeSecretNavigationMode({
    requestPending: hasPendingSecretRequest(),
    keyVisible: Boolean(apiKey.value),
  });
}

function isSessionInvalid(error) {
  return error?.code === "AUTH_REQUIRED" || error?.code === "SESSION_EXPIRED";
}

function redirectExpiredSession() {
  if (!viewActive || route.name === "login") return;
  router.replace({
    name: "login",
    query: { notice: "session_expired", redirect: route.fullPath },
  });
}

function validateAliasAddress(_, value, callback) {
  const normalized = String(value || "").trim();
  if (!normalized) {
    callback(new Error("请填写隐私邮箱地址"));
    return;
  }
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(normalized)) {
    callback(new Error("邮箱地址格式不正确"));
    return;
  }
  callback();
}

function validateAliasLabel(_, value, callback) {
  if (Array.from(String(value || "")).length > 100) {
    callback(new Error("用途备注不能超过 100 个字符"));
    return;
  }
  callback();
}

const aliasRules = {
  address: [{ validator: validateAliasAddress, trigger: ["blur", "change"] }],
  label: [{ validator: validateAliasLabel, trigger: "blur" }],
};

function keyPrefix(alias) {
  return alias.apiKeyPrefix ? `${alias.apiKeyPrefix}…` : "-";
}

function replaceAlias(updated) {
  aliases.value = aliases.value.map((item) =>
    item.id === updated.id ? updated : item,
  );
}

function detailMutationPending() {
  return (
    syncLoading.value ||
    createLoading.value ||
    accountDeleteLoading.value ||
    aliasActionLock.hasAny()
  );
}

function beginDetailMutation() {
  detailGate.invalidate();
}

async function loadDetail({ silent = false } = {}) {
  if (silent && (loading.value || detailMutationPending())) return;
  const accountId = detailRouteKey();
  const ticket = detailGate.begin(accountId);
  if (!silent) {
    loading.value = true;
    loadError.value = null;
  }
  try {
    const detail = await getAccount(accountId);
    if (!detailGate.isCurrent(ticket, detailRouteKey())) return;
    account.value = detail.account;
    aliases.value = detail.aliases;
    loadError.value = null;
    setPageHeader(
      detail.account.email,
      "管理 IMAP 连接、同步状态和所属隐私邮箱",
    );
  } catch (error) {
    if (silent || !detailGate.isCurrent(ticket, detailRouteKey())) return;
    loadError.value = error;
  } finally {
    if (!silent && detailGate.isCurrent(ticket, detailRouteKey())) {
      loading.value = false;
    }
  }
}

const liveRefresh = createLiveRefresh(() => loadDetail({ silent: true }));

async function revealKey(value, directLinkPath, accountId) {
  if (!value || !directLinkPath || !isCurrentAccount(accountId)) return false;
  apiKey.value = value;
  apiDirectLinkPath.value = directLinkPath;
  await nextTick();
  if (!isCurrentAccount(accountId)) {
    clearSecret();
    return false;
  }
  const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  secretRegion.value?.scrollIntoView({
    behavior: reduceMotion ? "auto" : "smooth",
    block: "start",
  });
  return true;
}

async function syncNow() {
  if (syncLoading.value) return;
  beginDetailMutation();
  const accountId = account.value.id;
  syncLoading.value = true;
  try {
    const detail = await syncAccount(accountId, auth.state.csrfToken);
    if (!isCurrentAccount(accountId)) return;
    account.value = detail.account;
    aliases.value = detail.aliases;
    successMessage("同步已完成。");
  } catch (error) {
    if (!isCurrentAccount(accountId)) return;
    showRequestError(error, "同步失败，请检查连接状态。");
    await loadDetail();
  } finally {
    syncLoading.value = false;
  }
}

async function addAlias() {
  if (apiKey.value || !createLock.acquire()) return;
  beginDetailMutation();
  const accountId = account.value.id;
  let sessionInvalid = false;
  createLoading.value = true;
  try {
    aliasFormError.value = null;
    const valid = await aliasFormRef.value?.validate().catch(() => false);
    if (!valid || !isCurrentAccount(accountId)) return;

    const result = await createAlias(
      accountId,
      {
        address: aliasForm.address.trim(),
        label: aliasForm.label.trim(),
      },
      auth.state.csrfToken,
    );
    if (!isCurrentAccount(accountId)) return;
    aliases.value = [...aliases.value, result.alias].sort(
      (left, right) =>
        left.address.localeCompare(right.address) || left.id - right.id,
    );
    account.value = { ...account.value, aliasCount: aliases.value.length };
    Object.assign(aliasForm, { address: "", label: "" });
    aliasFormRef.value?.resetFields();
    if (!(await revealKey(result.apiKey, result.alias.directLinkPath, accountId))) {
      return;
    }
    successMessage("隐私邮箱已添加。");
  } catch (error) {
    sessionInvalid = isSessionInvalid(error);
    if (!isCurrentAccount(accountId)) return;
    aliasFormError.value = error;
  } finally {
    createLoading.value = false;
    createLock.release();
    if (sessionInvalid) redirectExpiredSession();
  }
}

async function rotateKey(alias) {
  if (apiKey.value || !aliasActionLock.acquire(alias.id)) return;
  beginDetailMutation();
  const accountId = account.value.id;
  let sessionInvalid = false;
  rotateLoading[alias.id] = true;
  try {
    await ElMessageBox.confirm(
      "轮换后旧 Key 会立即失效。继续吗？",
      `轮换 ${alias.address} 的 API Key`,
      {
        type: "warning",
        confirmButtonText: "继续轮换",
        cancelButtonText: "取消",
        autofocus: false,
      },
    );
    if (!isCurrentAccount(accountId)) return;
    const result = await rotateAlias(alias.id, auth.state.csrfToken);
    if (!isCurrentAccount(accountId)) return;
    replaceAlias(result.alias);
    if (!(await revealKey(result.apiKey, result.alias.directLinkPath, accountId))) {
      return;
    }
    successMessage("API Key 已轮换，旧 Key 已失效。");
  } catch (error) {
    if (confirmationCancelled(error)) return;
    sessionInvalid = isSessionInvalid(error);
    if (!isCurrentAccount(accountId)) return;
    showRequestError(error, "API Key 轮换失败，请稍后重试。");
  } finally {
    delete rotateLoading[alias.id];
    aliasActionLock.release(alias.id);
    if (sessionInvalid) redirectExpiredSession();
  }
}

async function copyAliasDirectLink(alias) {
  if (!alias.directLinkPath || !aliasActionLock.acquire(alias.id)) return;
  const accountId = account.value.id;
  copyLoading[alias.id] = true;
  try {
    const directLink = buildRecentMailDirectLink(alias.directLinkPath);
    const copied = await copyText(directLink);
    if (!isCurrentAccount(accountId)) return;
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
    if (!isCurrentAccount(accountId)) return;
    ElMessage({
      type: "error",
      message: "直达链接复制失败，请刷新页面后重试。",
      grouping: true,
    });
  } finally {
    delete copyLoading[alias.id];
    aliasActionLock.release(alias.id);
  }
}

async function toggleAlias(alias, enabled) {
  if (alias.enabled === enabled || !aliasActionLock.acquire(alias.id)) return;
  beginDetailMutation();
  const accountId = account.value.id;
  toggleLoading[alias.id] = true;
  try {
    const updated = await setAliasEnabled(
      alias.id,
      Boolean(enabled),
      auth.state.csrfToken,
    );
    if (!isCurrentAccount(accountId)) return;
    replaceAlias(updated);
    successMessage(enabled ? "隐私邮箱已启用。" : "隐私邮箱已停用。");
  } catch (error) {
    if (!isCurrentAccount(accountId)) return;
    showRequestError(error, "隐私邮箱状态更新失败，请稍后重试。");
  } finally {
    delete toggleLoading[alias.id];
    aliasActionLock.release(alias.id);
  }
}

async function removeAlias(alias) {
  if (!aliasActionLock.acquire(alias.id)) return;
  beginDetailMutation();
  const accountId = account.value.id;
  deleteLoading[alias.id] = true;
  try {
    await ElMessageBox.confirm(
      "删除后该地址的 API Key 和最新邮件都会清除。继续吗？",
      `删除 ${alias.address}`,
      {
        type: "warning",
        confirmButtonText: "删除",
        cancelButtonText: "取消",
        confirmButtonClass: "el-button--danger",
        autofocus: false,
      },
    );
    if (!isCurrentAccount(accountId)) return;
    await deleteAlias(alias.id, auth.state.csrfToken);
    if (!isCurrentAccount(accountId)) return;
    aliases.value = aliases.value.filter((item) => item.id !== alias.id);
    account.value = { ...account.value, aliasCount: aliases.value.length };
    successMessage("隐私邮箱已删除。");
  } catch (error) {
    if (confirmationCancelled(error)) return;
    if (!isCurrentAccount(accountId)) return;
    showRequestError(error, "隐私邮箱删除失败，请稍后重试。");
  } finally {
    delete deleteLoading[alias.id];
    aliasActionLock.release(alias.id);
  }
}

async function removeAccount() {
  if (!accountDeleteLock.acquire()) return;
  beginDetailMutation();
  const accountId = account.value.id;
  const accountEmail = account.value.email;
  accountDeleteLoading.value = true;
  try {
    await ElMessageBox.confirm(
      `确定删除主号 ${accountEmail} 及其全部数据吗？`,
      "删除主号",
      {
        type: "warning",
        confirmButtonText: "删除主号",
        cancelButtonText: "取消",
        confirmButtonClass: "el-button--danger",
        autofocus: false,
      },
    );
    if (!isCurrentAccount(accountId)) return;
    await deleteAccount(accountId, auth.state.csrfToken);
    if (!isCurrentAccount(accountId)) return;
    clearSecret();
    successMessage("主号及其全部数据已删除。");
    await router.replace({ name: "accounts" });
  } catch (error) {
    if (confirmationCancelled(error)) return;
    if (!isCurrentAccount(accountId)) return;
    showRequestError(error, "主号删除失败，请稍后重试。");
  } finally {
    accountDeleteLoading.value = false;
    accountDeleteLock.release();
  }
}

function editAccount() {
  router.push({ name: "account-edit", params: { id: account.value.id } });
}

function clearSecret() {
  apiKey.value = "";
  apiDirectLinkPath.value = "";
}

async function confirmSecretNavigation() {
  const mode = secretNavigationMode();
  if (mode === "block") {
    ElMessage({
      type: "warning",
      message: "正在生成 API Key，请等待操作完成。",
      grouping: true,
    });
    return false;
  }
  if (mode === "allow") {
    clearSecret();
    return true;
  }
  if (!secretNavigationLock.acquire()) return false;

  try {
    await ElMessageBox.confirm(
      "完整 API Key 只显示这一次。离开后将无法再次查看，确定离开吗？",
      "尚未关闭 API Key 提示",
      {
        type: "warning",
        confirmButtonText: "仍要离开",
        cancelButtonText: "留在此页",
        autofocus: false,
      },
    );
    clearSecret();
    return true;
  } catch (error) {
    if (!confirmationCancelled(error)) {
      ElMessage.error("无法确认页面导航，请稍后重试。");
    }
    return false;
  } finally {
    secretNavigationLock.release();
  }
}

function protectSecretBeforeUnload(event) {
  if (!hasProtectedSecret()) return;
  event.preventDefault();
  event.returnValue = "";
}

watch(
  () => route.params.id,
  (id, previousId) => {
    if (id && id !== previousId) {
      detailGate.invalidate();
      loading.value = false;
      clearSecret();
      account.value = null;
      aliases.value = [];
      loadDetail();
    }
  },
);

onBeforeRouteLeave(confirmSecretNavigation);
onBeforeRouteUpdate(confirmSecretNavigation);

onMounted(() => {
  window.addEventListener("beforeunload", protectSecretBeforeUnload);
  loadDetail();
  liveRefresh.start({ immediate: false });
});

onBeforeUnmount(() => {
  viewActive = false;
  liveRefresh.stop();
  detailGate.deactivate();
  window.removeEventListener("beforeunload", protectSecretBeforeUnload);
  clearSecret();
});
</script>
