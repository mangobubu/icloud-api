<template>
  <section class="page-stack" aria-labelledby="audit-section-title">
    <SectionHeader
      id="audit-section-title"
      title="最近操作"
      description="记录后台登录与配置变更，不包含密码、完整 Key 或邮件内容。"
    >
      <template #actions>
        <el-tooltip content="刷新操作记录" placement="bottom">
          <el-button
            :icon="Refresh"
            circle
            :loading="loading"
            aria-label="刷新操作记录"
            @click="loadAuditLogs"
          />
        </el-tooltip>
      </template>
    </SectionHeader>

    <div v-if="loading && logs.length === 0" class="data-panel loading-panel">
      <el-skeleton :rows="7" animated />
    </div>

    <div v-else-if="loadError && logs.length === 0" class="load-failed">
      <RequestAlert :error="loadError" />
      <el-button :icon="Refresh" @click="loadAuditLogs">重新加载</el-button>
    </div>

    <EmptyState
      v-else-if="logs.length === 0"
      title="暂无操作记录"
      description="新的登录与配置操作会显示在这里。"
    />

    <template v-else>
      <RequestAlert
        v-if="loadError"
        :error="loadError"
        closable
        @close="loadError = null"
      />

      <div class="data-panel desktop-data-table" :aria-busy="loading">
        <el-table :data="logs" row-key="id" style="width: 100%">
          <el-table-column label="时间" min-width="176">
            <template #default="{ row }">
              {{ formatTime(row.createdAt, { seconds: true }) }}
            </template>
          </el-table-column>
          <el-table-column label="管理员" min-width="130">
            <template #default="{ row }">{{ row.username || "系统" }}</template>
          </el-table-column>
          <el-table-column label="操作" min-width="142">
            <template #default="{ row }">
              <strong class="audit-action">{{ actionLabel(row.action) }}</strong>
            </template>
          </el-table-column>
          <el-table-column label="对象" min-width="150">
            <template #default="{ row }">
              {{ resourceLabel(row.resourceType, row.resourceId) }}
            </template>
          </el-table-column>
          <el-table-column label="结果" width="92">
            <template #default="{ row }">
              <el-tag
                class="audit-result"
                :type="resultMeta(row.result).type"
                effect="plain"
                size="small"
              >
                {{ resultMeta(row.result).label }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="请求编号" min-width="180">
            <template #default="{ row }">
              <code class="audit-request-id">{{ row.requestId || "-" }}</code>
            </template>
          </el-table-column>
        </el-table>
        <div v-if="loading" class="table-loading-mask" aria-hidden="true"></div>
      </div>

      <div class="mobile-record-list" :aria-busy="loading">
        <article v-for="log in logs" :key="log.id" class="mobile-record">
          <header class="mobile-record__header">
            <div class="primary-stack">
              <strong>{{ actionLabel(log.action) }}</strong>
              <small>{{ formatTime(log.createdAt, { seconds: true }) }}</small>
            </div>
            <el-tag
              class="audit-result"
              :type="resultMeta(log.result).type"
              effect="plain"
              size="small"
            >
              {{ resultMeta(log.result).label }}
            </el-tag>
          </header>
          <dl class="mobile-kv-list">
            <div>
              <dt>管理员</dt>
              <dd>{{ log.username || "系统" }}</dd>
            </div>
            <div>
              <dt>对象</dt>
              <dd>{{ resourceLabel(log.resourceType, log.resourceId) }}</dd>
            </div>
            <div>
              <dt>请求编号</dt>
              <dd><code class="audit-request-id">{{ log.requestId || "-" }}</code></dd>
            </div>
          </dl>
        </article>
      </div>
    </template>
  </section>
</template>

<script setup>
import { Refresh } from "@element-plus/icons-vue";
import { onMounted, ref } from "vue";

import { getAuditLogs } from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import RequestAlert from "../components/RequestAlert.vue";
import SectionHeader from "../components/SectionHeader.vue";
import { formatTime } from "../utils/format.js";

const actionLabels = {
  login: "登录后台",
  logout: "退出登录",
  change_password: "修改登录密码",
  create: "创建",
  update: "更新",
  delete: "删除",
  sync: "同步主号",
  rotate_key: "轮换 API Key",
  toggle: "切换启用状态",
};

const resourceLabels = {
  admin: "管理员",
  account: "主号",
  alias: "隐私邮箱",
};

const resultLabels = {
  success: { label: "成功", type: "success" },
  failed: { label: "失败", type: "danger" },
};

const logs = ref([]);
const loading = ref(false);
const loadError = ref(null);

function actionLabel(action) {
  return actionLabels[action] || action || "未知操作";
}

function resourceLabel(type, id) {
  const typeLabel = resourceLabels[type] || type || "系统";
  return id ? `${typeLabel} #${id}` : typeLabel;
}

function resultMeta(result) {
  return resultLabels[result] || {
    label: result || "未知",
    type: "info",
  };
}

async function loadAuditLogs() {
  if (loading.value) return;
  loading.value = true;
  loadError.value = null;
  try {
    logs.value = await getAuditLogs();
  } catch (error) {
    loadError.value = error;
  } finally {
    loading.value = false;
  }
}

onMounted(loadAuditLogs);
</script>

<style scoped>
.audit-action {
  color: var(--el-text-color-primary);
  font-weight: 600;
}

.audit-result {
  min-width: 48px;
  justify-content: center;
}

.audit-request-id {
  overflow-wrap: anywhere;
}
</style>
