<template>
  <section class="page-stack" aria-labelledby="aliases-section-title">
    <SectionHeader
      id="aliases-section-title"
      title="全部隐私邮箱"
      description="查看每个地址的主号归属、Key 状态和最近使用情况。"
    >
      <template #actions>
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

    <div v-if="loading && aliases.length === 0" class="data-panel loading-panel">
      <el-skeleton :rows="6" animated />
    </div>

    <div v-else-if="loadError && aliases.length === 0" class="load-failed">
      <RequestAlert :error="loadError" />
      <el-button :icon="Refresh" @click="loadAliases">重新加载</el-button>
    </div>

    <EmptyState
      v-else-if="aliases.length === 0"
      title="还没有隐私邮箱"
      description="进入某个主号详情页添加地址。"
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
        <el-table :data="aliases" row-key="id" style="width: 100%">
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
              <SyncStatus :item="row" details />
            </template>
          </el-table-column>
          <el-table-column label="操作" width="152" align="right" fixed="right">
            <template #default="{ row }">
              <div class="icon-action-row">
                <el-tooltip content="复制邮件 API 直达链接" placement="top">
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
            <div class="primary-stack">
              <strong>{{ alias.address }}</strong>
              <small>{{ alias.label || "未填写用途备注" }}</small>
            </div>
            <SyncStatus :item="alias" details />
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
import { CopyDocument, Refresh, Setting } from "@element-plus/icons-vue";
import { ElMessage } from "element-plus";
import { onBeforeUnmount, onMounted, reactive, ref } from "vue";
import { useRouter } from "vue-router";

import { getAliases } from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import RequestAlert from "../components/RequestAlert.vue";
import SectionHeader from "../components/SectionHeader.vue";
import SyncStatus from "../components/SyncStatus.vue";
import { createActionLock } from "../utils/asyncState.js";
import {
  buildRecentMailDirectLink,
  copyText,
} from "../utils/clipboard.js";
import { successMessage } from "../utils/feedback.js";
import { formatTime } from "../utils/format.js";
import { createLiveRefresh } from "../utils/liveRefresh.js";

const router = useRouter();
const aliases = ref([]);
const loading = ref(false);
const loadError = ref(null);
const copyLoading = reactive({});
const copyLock = createActionLock();
let refreshInFlight = false;
let viewActive = true;

function keyPrefix(alias) {
  return alias.apiKeyPrefix ? `${alias.apiKeyPrefix}…` : "-";
}

async function loadAliases({ silent = false } = {}) {
  if (refreshInFlight) return;
  refreshInFlight = true;
  if (!silent) {
    loading.value = true;
    loadError.value = null;
  }
  try {
    const nextAliases = await getAliases();
    if (!viewActive) return;
    aliases.value = nextAliases;
    loadError.value = null;
  } catch (error) {
    if (viewActive && !silent) {
      loadError.value = error;
    }
  } finally {
    refreshInFlight = false;
    if (!silent) {
      loading.value = false;
    }
  }
}

const liveRefresh = createLiveRefresh(() => loadAliases({ silent: true }));

async function copyAliasDirectLink(alias) {
  if (!alias.directLinkPath || !copyLock.acquire(alias.id)) return;
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
  loadAliases();
  liveRefresh.start({ immediate: false });
});

onBeforeUnmount(() => {
  viewActive = false;
  liveRefresh.stop();
});
</script>

<style scoped>
.account-link {
  max-width: 100%;
  justify-content: flex-start;
}

.account-link :deep(span) {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
