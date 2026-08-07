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

      <div class="data-panel desktop-data-table" :aria-busy="loading">
        <el-table :data="accounts" row-key="id" style="width: 100%">
          <el-table-column label="主号" min-width="250">
            <template #default="{ row }">
              <div class="primary-stack">
                <strong>{{ row.email }}</strong>
                <small>{{ row.name || "未填写备注" }}</small>
              </div>
            </template>
          </el-table-column>
          <el-table-column label="状态" min-width="120">
            <template #default="{ row }">
              <SyncStatus :item="row" />
            </template>
          </el-table-column>
          <el-table-column label="隐私邮箱" width="112" align="center">
            <template #default="{ row }">{{ row.aliasCount }}</template>
          </el-table-column>
          <el-table-column label="最近同步" min-width="170">
            <template #default="{ row }">{{ formatTime(row.lastSyncedAt) }}</template>
          </el-table-column>
          <el-table-column label="操作" width="118" align="right">
            <template #default="{ row }">
              <el-button
                link
                type="primary"
                :icon="Setting"
                @click="openAccount(row.id)"
              >
                管理
              </el-button>
            </template>
          </el-table-column>
        </el-table>
        <div v-if="loading" class="table-loading-mask" aria-hidden="true"></div>
      </div>

      <div class="mobile-record-list" :aria-busy="loading">
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
              <dd>{{ formatTime(account.lastSyncedAt) }}</dd>
            </div>
          </dl>
          <footer class="mobile-record__actions">
            <el-button :icon="Setting" @click="openAccount(account.id)">
              管理主号
            </el-button>
          </footer>
        </article>
      </div>
    </template>
  </section>
</template>

<script setup>
import { Plus, Refresh, Setting } from "@element-plus/icons-vue";
import { onMounted, ref } from "vue";
import { useRouter } from "vue-router";

import { getAccounts } from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import RequestAlert from "../components/RequestAlert.vue";
import SectionHeader from "../components/SectionHeader.vue";
import SyncStatus from "../components/SyncStatus.vue";
import { formatTime } from "../utils/format.js";

const router = useRouter();
const accounts = ref([]);
const loading = ref(false);
const loadError = ref(null);

async function loadAccounts() {
  if (loading.value) return;
  loading.value = true;
  loadError.value = null;
  try {
    accounts.value = await getAccounts();
  } catch (error) {
    loadError.value = error;
  } finally {
    loading.value = false;
  }
}

function openNewAccount() {
  router.push({ name: "account-new" });
}

function openAccount(id) {
  router.push({ name: "account-detail", params: { id } });
}

onMounted(loadAccounts);
</script>
