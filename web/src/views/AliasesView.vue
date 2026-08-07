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
          <el-table-column label="隐私邮箱" min-width="240">
            <template #default="{ row }">
              <div class="primary-stack">
                <strong>{{ row.address }}</strong>
                <small>{{ row.label || "未填写用途备注" }}</small>
              </div>
            </template>
          </el-table-column>
          <el-table-column label="所属主号" min-width="210">
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
          <el-table-column label="API Key" min-width="126">
            <template #default="{ row }">
              <code class="key-prefix">{{ keyPrefix(row) }}</code>
            </template>
          </el-table-column>
          <el-table-column label="最近调用" min-width="170">
            <template #default="{ row }">
              {{ formatTime(row.lastAccessedAt) }}
            </template>
          </el-table-column>
          <el-table-column label="最新邮件" min-width="170">
            <template #default="{ row }">
              {{ formatTime(row.latestReceivedAt) }}
            </template>
          </el-table-column>
          <el-table-column label="状态" min-width="124">
            <template #default="{ row }">
              <SyncStatus :item="row" details />
            </template>
          </el-table-column>
          <el-table-column label="操作" width="112" align="right">
            <template #default="{ row }">
              <el-button
                link
                type="primary"
                :icon="Setting"
                @click="openAccount(row.accountId)"
              >
                管理
              </el-button>
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
          <footer class="mobile-record__actions">
            <el-button :icon="Setting" @click="openAccount(alias.accountId)">
              管理所属主号
            </el-button>
          </footer>
        </article>
      </div>
    </template>
  </section>
</template>

<script setup>
import { Refresh, Setting } from "@element-plus/icons-vue";
import { onMounted, ref } from "vue";
import { useRouter } from "vue-router";

import { getAliases } from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import RequestAlert from "../components/RequestAlert.vue";
import SectionHeader from "../components/SectionHeader.vue";
import SyncStatus from "../components/SyncStatus.vue";
import { formatTime } from "../utils/format.js";

const router = useRouter();
const aliases = ref([]);
const loading = ref(false);
const loadError = ref(null);

function keyPrefix(alias) {
  return alias.apiKeyPrefix ? `${alias.apiKeyPrefix}…` : "-";
}

async function loadAliases() {
  if (loading.value) return;
  loading.value = true;
  loadError.value = null;
  try {
    aliases.value = await getAliases();
  } catch (error) {
    loadError.value = error;
  } finally {
    loading.value = false;
  }
}

function openAccounts() {
  router.push({ name: "accounts" });
}

function openAccount(id) {
  router.push({ name: "account-detail", params: { id } });
}

onMounted(loadAliases);
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
