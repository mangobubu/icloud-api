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
      <el-dialog
        v-model="appleAuthVisible"
        class="apple-auth-dialog"
        :title="appleAuthStep === 'verification' ? '输入双重认证验证码' : '登录 Apple 账户'"
        width="min(520px, calc(100vw - 28px))"
        :close-on-click-modal="false"
        :close-on-press-escape="false"
        :before-close="closeAppleAuthDialog"
        destroy-on-close
      >
        <RequestAlert
          v-if="appleAuthError"
          :error="appleAuthError"
          closable
          @close="appleAuthError = null"
        />

        <el-form
          v-if="appleAuthStep === 'login'"
          ref="appleLoginFormRef"
          class="dialog-form"
          :model="appleLoginForm"
          :rules="appleLoginRules"
          label-position="top"
          :disabled="appleAuthLoading"
          @submit.prevent="submitAppleLogin"
        >
          <el-form-item label="Apple ID" prop="appleId">
            <el-input
              v-model="appleLoginForm.appleId"
              autocomplete="username"
              placeholder="name@icloud.com"
            />
          </el-form-item>
          <el-form-item label="Apple 账户密码" prop="password">
            <el-input
              v-model="appleLoginForm.password"
              type="password"
              show-password
              autocomplete="current-password"
            />
          </el-form-item>
          <el-form-item label="Apple 服务区域" prop="region">
            <el-select v-model="appleLoginForm.region" style="width: 100%">
              <el-option label="全球" value="global" />
              <el-option label="中国大陆" value="cn" />
            </el-select>
          </el-form-item>
        </el-form>

        <el-form
          v-else
          ref="appleVerificationFormRef"
          class="dialog-form"
          :model="appleVerificationForm"
          :rules="appleVerificationRules"
          label-position="top"
          :disabled="appleAuthLoading"
          @submit.prevent="submitAppleVerification"
        >
          <p class="dialog-form__lead">
            请在受信任的 Apple 设备上查看验证码。
          </p>
          <el-form-item label="6 位验证码" prop="code">
            <el-input
              v-model="appleVerificationForm.code"
              maxlength="6"
              inputmode="numeric"
              autocomplete="one-time-code"
              placeholder="000000"
            />
          </el-form-item>
        </el-form>

        <template #footer>
          <div class="dialog-actions">
            <el-button :disabled="appleAuthLoading" @click="cancelAppleAuth">
              取消
            </el-button>
            <el-button
              v-if="appleAuthStep === 'verification'"
              :disabled="appleAuthLoading"
              @click="returnToAppleLogin"
            >
              返回登录
            </el-button>
            <el-button
              type="primary"
              :loading="appleAuthLoading"
              @click="appleAuthStep === 'verification' ? submitAppleVerification() : submitAppleLogin()"
            >
              {{
                appleAuthStep === "verification"
                  ? appleVerificationActionLabel()
                  : "继续"
              }}
            </el-button>
          </div>
        </template>
      </el-dialog>

      <RequestAlert
        v-if="loadError && aliases.length"
        :error="loadError"
        closable
        @close="loadError = null"
      />
      <RequestAlert
        v-if="groupsError"
        :error="groupsError"
        closable
        @close="groupsError = null"
      />

      <section class="section-block" aria-labelledby="connection-title">
        <SectionHeader
          id="connection-title"
          title="IMAP 邮件同步"
          :description="`${account.imapUsername} · ${formatIMAPEndpoint(account)}`"
        >
          <template #actions>
            <el-button
              :icon="Refresh"
              :loading="syncLoading || syncActive"
              :disabled="!account.enabled || syncActive || randomAliasLoading"
              @click="syncNow"
            >
              {{ syncActive ? "同步处理中" : "同步邮件" }}
            </el-button>
            <el-button :icon="EditPen" @click="editAccount">编辑</el-button>
          </template>
        </SectionHeader>

        <dl class="detail-grid">
          <div>
            <dt>收件规则</dt>
            <dd>{{ receiveRuleLabel }}</dd>
          </div>
          <div>
            <dt>状态</dt>
            <dd><SyncStatus :item="account" /></dd>
          </div>
          <div>
            <dt>最近同步</dt>
            <dd>{{ formatTime(account.lastSyncedAt, { seconds: true }) }}</dd>
          </div>
          <div v-if="!isCustomMailbox">
            <dt>主号邮箱</dt>
            <dd>{{ account.email }}</dd>
          </div>
          <div v-else>
            <dt>邮箱类型</dt>
            <dd>自定义邮箱</dd>
          </div>
          <div v-if="isCustomMailbox">
            <dt>邮箱后缀</dt>
            <dd>@{{ account.emailSuffix }}</dd>
          </div>
          <div>
            <dt>备注</dt>
            <dd>{{ account.name || "-" }}</dd>
          </div>
        </dl>

        <div v-if="account.lastSyncError" class="inline-error" role="status">
          <strong>最近错误</strong>
          <div class="inline-error__content">
            <span>{{ account.lastSyncError }}</span>
            <SyncErrorLogDialog
              :log="account.lastSyncErrorLog || account.lastSyncError"
              :account-id="account.id"
            />
          </div>
        </div>
      </section>

      <section class="section-block" aria-labelledby="aliases-title">
        <SectionHeader
          id="aliases-title"
          title="隐私邮箱"
          :description="isCustomMailbox
            ? '按邮箱后缀生成或手动登记地址；每个地址使用独立的完整凭证包。'
            : '从 Apple 拉取 Hide My Email 地址目录；每个本地地址使用独立的完整凭证包。'"
        >
          <template #actions>
            <el-button
              v-if="!isCustomMailbox && appleSessionAuthenticated"
              :icon="SwitchButton"
              :loading="appleDisconnectLoading"
              :disabled="appleAliasControlsDisabled || aliasesSyncLoading"
              @click="disconnectAppleSession"
            >
              退出 Apple 登录
            </el-button>
            <el-button
              v-if="!isCustomMailbox"
              type="primary"
              :icon="Refresh"
              :loading="aliasesSyncLoading"
              :disabled="appleAliasControlsDisabled || appleDisconnectLoading"
              @click="syncAliasesFromApple"
            >
              同步隐私邮箱
            </el-button>
          </template>
        </SectionHeader>

        <div v-if="!isCustomMailbox" class="apple-session-strip">
          <div class="apple-session-strip__identity">
            <el-tag :type="appleSessionAuthenticated ? 'success' : 'info'" effect="plain">
              {{ appleSessionAuthenticated ? "Apple 已登录" : "Apple 未登录" }}
            </el-tag>
            <span v-if="appleSession?.appleId">{{ appleSession.appleId }}</span>
            <span v-if="appleSessionAuthenticated">
              {{ appleSession.region === "cn" ? "中国大陆" : "全球" }}
            </span>
          </div>
          <div v-if="aliasSyncSummary" class="apple-session-strip__summary">
            上次同步：共 {{ aliasSyncSummary.total }}，新建
            {{ aliasSyncSummary.createdCount }}，已存在
            {{ aliasSyncSummary.existingCount }}，Apple 已停用
            {{ aliasSyncSummary.inactiveCount }}，因本地容量暂未启用
            {{ aliasSyncSummary.importedDisabledCount }}，冲突
            {{ aliasSyncSummary.conflictCount }}
          </div>
        </div>

        <div
          v-if="autoCreation && !isCustomMailbox"
          class="auto-creation-panel"
          aria-labelledby="auto-creation-title"
        >
          <div class="auto-creation-panel__header">
            <div class="auto-creation-panel__copy">
              <div class="auto-creation-panel__title-row">
                <h3 id="auto-creation-title">自动创建隐私邮箱</h3>
                <el-tag
                  :type="autoCreationStatusType(autoCreation)"
                  effect="plain"
                  size="small"
                >
                  {{ autoCreationStatusLabel(autoCreation) }}
                </el-tag>
              </div>
              <p>
                每小时 5 个 · 随机间隔 · 最短 5 分钟。签发后可通过邮箱列表中的复制操作导出完整凭证。
              </p>
            </div>
            <div class="auto-creation-panel__actions">
              <el-switch
                :model-value="Boolean(autoCreation.enabled)"
                :loading="autoCreationLoading"
                :disabled="autoCreationToggleDisabled"
                active-text="开启"
                inactive-text="关闭"
                :aria-label="`${autoCreation.enabled ? '关闭' : '开启'}自动创建隐私邮箱`"
                @change="toggleAutoCreation"
              />
            </div>
          </div>

          <dl class="auto-creation-metrics">
            <div>
              <dt>当前隐私邮箱</dt>
              <dd aria-live="polite" aria-atomic="true">
                {{ account.aliasCount }}
              </dd>
            </div>
            <div>
              <dt>下次执行</dt>
              <dd>{{ formatTime(autoCreation.nextRunAt, { seconds: true }) }}</dd>
            </div>
            <div>
              <dt>计划时间</dt>
              <dd>
                {{
                  formatAutoPlannedAt(
                    autoCreation.plannedTimes?.length
                      ? autoCreation.plannedTimes
                      : autoCreation.plannedAt,
                  )
                }}
              </dd>
            </div>
            <div>
              <dt>最近尝试</dt>
              <dd>{{ formatTime(autoCreation.lastAttemptedAt, { seconds: true }) }}</dd>
            </div>
            <div>
              <dt>最近创建</dt>
              <dd>
                <span>{{ formatTime(autoCreation.lastCreatedAt, { seconds: true }) }}</span>
                <small v-if="autoCreation.lastAliasAddress">
                  {{ autoCreation.lastAliasAddress }}
                </small>
              </dd>
            </div>
          </dl>

          <div v-if="autoCreation.lastError" class="auto-creation-error" role="status">
            <strong>最近错误</strong>
            <span>{{ autoCreationErrorMessage(autoCreation.lastError) }}</span>
          </div>
        </div>

        <div
          v-if="isCustomMailbox"
          class="auto-creation-panel custom-random-panel"
          aria-labelledby="custom-random-title"
        >
          <div class="auto-creation-panel__header">
            <div class="auto-creation-panel__copy">
              <div class="auto-creation-panel__title-row">
                <h3 id="custom-random-title">自动生成随机邮箱</h3>
                <el-tag type="success" effect="plain" size="small">自定义规则</el-tag>
              </div>
              <p>
                按 8–12 位随机英文数字 + @{{ account.emailSuffix || "后缀" }} 生成，不调用 iCloud 规则，且不会重复已有地址。
              </p>
            </div>
            <div class="auto-creation-panel__actions custom-random-panel__actions">
              <el-input-number
                v-model="randomAliasCount"
                :min="1"
                :max="1000"
                :step="1"
                :controls="false"
                aria-label="随机邮箱生成数量"
              />
              <el-button
                type="primary"
                :loading="randomAliasLoading"
                :disabled="randomAliasLoading || syncLoading || syncActive || !account.enabled || !account.emailSuffix"
                @click="generateRandomAliases"
              >
                生成邮箱
              </el-button>
            </div>
          </div>
          <RequestAlert
            v-if="randomAliasError"
            class="custom-random-panel__error"
            :error="randomAliasError"
            closable
            @close="randomAliasError = null"
          />
          <p class="field-help">
            可直接输入生成数量，单次最多 1000 个；可多次分批生成，自定义邮箱主号的累计数量不设上限。系统会拒绝重复地址，可通过下方列表中的复制操作导出完整凭证。
          </p>
        </div>

        <div
          v-if="loading && aliases.length === 0"
          class="data-panel loading-panel"
        >
          <el-skeleton :rows="6" animated />
        </div>

        <div
          v-if="!loading && loadError && aliases.length === 0"
          class="load-failed"
        >
          <RequestAlert :error="loadError" />
          <el-button :icon="Refresh" @click="loadDetail">
            重新加载邮箱列表
          </el-button>
        </div>

        <div
          v-if="aliases.length"
          class="data-panel desktop-data-table account-alias-table"
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
            <template #cell="{ column, rowData: row }">
              <template v-if="column.key === 'address'">
                <div class="primary-stack">
                  <strong>{{ row.address }}</strong>
                  <small>{{ row.label || "未填写用途备注" }}</small>
                </div>
              </template>
              <template v-else-if="column.key === 'group'">
                <el-select
                  class="alias-group-select"
                  :model-value="row.groupId == null ? '' : String(row.groupId)"
                  :loading="groupsLoading || Boolean(groupMoveLoading[row.id])"
                  :disabled="groupsLoading || isAliasActionBusy(row)"
                  aria-label="选择隐私邮箱分组"
                  @change="moveAliasToGroup(row, $event)"
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
              <template v-else-if="column.key === 'latestReceivedAt'">
                {{ formatTime(row.latestReceivedAt) }}
              </template>
              <template v-else-if="column.key === 'syncStatus'">
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
              <template v-else-if="column.key === 'enabled'">
                <el-switch
                  v-if="!isAliasConfirmationPending(row)"
                  :model-value="row.enabled"
                  :loading="Boolean(toggleLoading[row.id])"
                  :disabled="isAliasActionBusy(row)"
                  :aria-label="`启用隐私邮箱 ${row.address}`"
                  @change="(enabled) => toggleAlias(row, enabled)"
                />
              </template>
              <template v-else-if="column.key === 'actions'">
                <div
                  v-if="!isAliasConfirmationPending(row)"
                  class="icon-action-row"
                >
                  <el-button
                    size="small"
                    :icon="CopyDocument"
                    :loading="Boolean(copyLoading[`${row.id}:otp`])"
                    :disabled="isAliasActionBusy(row)"
                    @click="copyAliasCredentials(row, ALIAS_EXPORT_OTP)"
                  >取码</el-button>
                  <el-button
                    size="small"
                    :icon="CopyDocument"
                    :loading="Boolean(copyLoading[`${row.id}:imap`])"
                    :disabled="isAliasActionBusy(row)"
                    @click="copyAliasCredentials(row, ALIAS_EXPORT_IMAP)"
                    v-if="isAliasV2(row)"
                  >IMAP</el-button>
                  <el-button
                    v-else-if="isLegacyDirectLinkAvailable(row)"
                    size="small"
                    :icon="CopyDocument"
                    :loading="Boolean(copyLoading[`${row.id}:legacy-link`])"
                    :disabled="isAliasActionBusy(row)"
                    @click="copyLegacyDirectLink(row)"
                  >旧直达</el-button>
                  <el-tooltip :content="aliasRotationLabel(row)" placement="top">
                    <el-button
                      :icon="Key"
                      circle
                      :loading="Boolean(rotateLoading[row.id])"
                      :disabled="isAliasActionBusy(row)"
                      :aria-label="`${aliasRotationLabel(row)}：${row.address}`"
                      @click="rotateKey(row)"
                    />
                  </el-tooltip>
                  <el-tooltip
                    :content="isCustomMailbox ? '从本地永久删除邮箱' : '从 iCloud 永久删除隐私邮箱'"
                    placement="top"
                  >
                    <el-button
                      type="danger"
                      plain
                      :icon="Delete"
                      circle
                      :loading="Boolean(deleteLoading[row.id])"
                      :disabled="isAliasActionBusy(row)"
                      :aria-label="`${isCustomMailbox ? '从本地永久删除邮箱' : '从 iCloud 永久删除隐私邮箱'} ${row.address}`"
                      @click="removeAlias(row)"
                    />
                  </el-tooltip>
                </div>
              </template>
            </template>
          </VirtualDataTable>
        </div>

        <div
          v-if="aliases.length && pageSize > ALL_PAGE_SIZE && pageSize <= 100"
          class="mobile-record-list"
          :aria-busy="loading"
        >
          <article v-for="alias in aliases" :key="alias.id" class="mobile-record">
            <header class="mobile-record__header">
              <div class="primary-stack">
                <strong>{{ alias.address }}</strong>
                <small>{{ alias.label || "未填写用途备注" }}</small>
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
                <dt>最新邮件</dt>
                <dd>{{ formatTime(alias.latestReceivedAt) }}</dd>
              </div>
              <div>
                <dt>分组</dt>
                <dd>
                  <el-select
                    class="mobile-alias-group-select"
                    :model-value="alias.groupId == null ? '' : String(alias.groupId)"
                    :loading="groupsLoading || Boolean(groupMoveLoading[alias.id])"
                    :disabled="groupsLoading || isAliasActionBusy(alias)"
                    aria-label="选择隐私邮箱分组"
                    @change="moveAliasToGroup(alias, $event)"
                  >
                    <el-option label="未分组" value="" />
                    <el-option
                      v-for="group in groups"
                      :key="group.id"
                      :label="group.name"
                      :value="String(group.id)"
                    />
                  </el-select>
                </dd>
              </div>
              <div v-if="!isAliasConfirmationPending(alias)">
                <dt>启用</dt>
                <dd>
                  <el-switch
                    :model-value="alias.enabled"
                    :loading="Boolean(toggleLoading[alias.id])"
                    :disabled="isAliasActionBusy(alias)"
                    :aria-label="`启用隐私邮箱 ${alias.address}`"
                    @change="(enabled) => toggleAlias(alias, enabled)"
                  />
                </dd>
              </div>
            </dl>
            <footer
              v-if="!isAliasConfirmationPending(alias)"
              class="mobile-record__actions mobile-record__actions--three"
            >
              <el-button
                :icon="CopyDocument"
                :loading="Boolean(copyLoading[`${alias.id}:otp`])"
                :disabled="isAliasActionBusy(alias)"
                @click="copyAliasCredentials(alias, ALIAS_EXPORT_OTP)"
              >
                复制取码格式
              </el-button>
              <el-button
                :icon="CopyDocument"
                :loading="Boolean(copyLoading[`${alias.id}:imap`])"
                :disabled="isAliasActionBusy(alias)"
                @click="copyAliasCredentials(alias, ALIAS_EXPORT_IMAP)"
                v-if="isAliasV2(alias)"
              >
                复制 IMAP 格式
              </el-button>
              <el-button
                v-else-if="isLegacyDirectLinkAvailable(alias)"
                :icon="CopyDocument"
                :loading="Boolean(copyLoading[`${alias.id}:legacy-link`])"
                :disabled="isAliasActionBusy(alias)"
                @click="copyLegacyDirectLink(alias)"
              >
                复制旧直达链接
              </el-button>
              <el-button
                :icon="Key"
                :loading="Boolean(rotateLoading[alias.id])"
                :disabled="isAliasActionBusy(alias)"
                @click="rotateKey(alias)"
              >
                {{ aliasRotationLabel(alias) }}
              </el-button>
              <el-button
                type="danger"
                plain
                :icon="Delete"
                :loading="Boolean(deleteLoading[alias.id])"
                :disabled="isAliasActionBusy(alias)"
                @click="removeAlias(alias)"
              >
                永久删除
              </el-button>
            </footer>
          </article>
        </div>

        <EmptyState
          v-if="!loading && !loadError && aliases.length === 0"
          class="empty-state--compact"
          level="h3"
          :title="isCustomMailbox ? '尚未生成邮箱' : '尚未登记隐私邮箱'"
          :description="isCustomMailbox
            ? '输入生成数量批量创建，或在下方手动登记一个邮箱地址。'
            : '添加一个已经转发到此主号的 Hide My Email 地址。'"
          />

        <ListPagination
          :page="currentPage"
          :page-size="pageSize"
          :total="total"
          :loading="loading"
          aria-label="主号隐私邮箱列表分页"
          @change="handlePageChange"
          @size-change="handlePageSizeChange"
        />

        <el-form
          ref="aliasFormRef"
          class="inline-alias-form"
          :model="aliasForm"
          :rules="aliasRules"
          label-position="top"
          :disabled="createLoading"
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
          >
            添加并签发整套凭证
          </el-button>
        </el-form>
      </section>

      <section class="danger-zone" aria-labelledby="delete-account-title">
        <div>
          <h2 id="delete-account-title">删除主号</h2>
          <p>同时删除其隐私邮箱、完整凭证与全部本地邮件归档。</p>
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
  CopyDocument,
  Delete,
  EditPen,
  Key,
  Plus,
  Refresh,
  SwitchButton,
} from "@element-plus/icons-vue";
import { ElMessage, ElMessageBox } from "element-plus";
import {
  computed,
  nextTick,
  onBeforeUnmount,
  onMounted,
  reactive,
  ref,
  watch,
} from "vue";
import {
  useRoute,
  useRouter,
} from "vue-router";

import {
  createAlias,
  createRandomAliases,
  deleteAccount,
  deleteAlias,
  deleteAppleSession,
  getAccount,
  getAllAliases,
  getMailGroups,
  loginAppleSession,
  moveAliasToGroup as moveAliasToGroupRequest,
  rotateAlias,
  setAliasAutoCreation,
  setAliasEnabled,
  syncAccount,
  syncAccountAliases,
  verifyAppleSession,
} from "../api/admin.js";
import EmptyState from "../components/EmptyState.vue";
import ListPagination from "../components/ListPagination.vue";
import RequestAlert from "../components/RequestAlert.vue";
import SectionHeader from "../components/SectionHeader.vue";
import SyncErrorLogDialog from "../components/SyncErrorLogDialog.vue";
import SyncStatus from "../components/SyncStatus.vue";
import VirtualDataTable from "../components/VirtualDataTable.vue";
import { useAuth } from "../stores/auth.js";
import { setPageHeader } from "../stores/page.js";
import {
  createActionLock,
  createLatestRequestGate,
} from "../utils/asyncState.js";
import {
  ALIAS_EXPORT_IMAP,
  ALIAS_EXPORT_OTP,
  buildAliasExportText,
} from "../utils/aliasExport.js";
import { buildRecentMailDirectLink, copyText } from "../utils/clipboard.js";
import {
  confirmationCancelled,
  showRequestError,
  successMessage,
} from "../utils/feedback.js";
import { formatTime } from "../utils/format.js";
import { formatIMAPEndpoint, mailboxReceiveRule } from "../utils/imap.js";
import { createLiveRefresh } from "../utils/liveRefresh.js";
import {
  ALL_PAGE_SIZE,
  DEFAULT_PAGE_SIZE,
  normalizePageSize,
} from "../utils/pagination.js";

const route = useRoute();
const router = useRouter();
const auth = useAuth();
const account = ref(null);
const aliases = ref([]);
const currentPage = ref(1);
const pageSize = ref(DEFAULT_PAGE_SIZE);
const total = ref(0);
const groups = ref([]);
const groupsLoading = ref(false);
const groupsError = ref(null);
const groupMoveLoading = reactive({});
const appleSession = ref(null);
const loading = ref(false);
const loadError = ref(null);
const syncLoading = ref(false);
const aliasesSyncLoading = ref(false);
const createLoading = ref(false);
const accountDeleteLoading = ref(false);
const appleDisconnectLoading = ref(false);
const aliasFormRef = ref(null);
const aliasFormError = ref(null);
const appleAuthVisible = ref(false);
const appleAuthStep = ref("login");
const appleAuthLoading = ref(false);
const appleAuthError = ref(null);
const appleLoginFormRef = ref(null);
const appleVerificationFormRef = ref(null);
const autoCreation = ref(null);
const autoCreationLoading = ref(false);
const randomAliasCount = ref(1);
const randomAliasLoading = ref(false);
const randomAliasError = ref(null);
const aliasSyncSummary = ref(null);
const copyLoading = reactive({});
const toggleLoading = reactive({});
const rotateLoading = reactive({});
const deleteLoading = reactive({});
const detailGate = createLatestRequestGate();
const groupsLoadGate = createLatestRequestGate();
const createLock = createActionLock();
const aliasActionLock = createActionLock();
const accountDeleteLock = createActionLock();
const appleDisconnectLock = createActionLock();
const autoCreationLock = createActionLock();
const randomAliasLock = createActionLock();
let detailAbortController = null;
let viewActive = true;
let resumeAliasSyncAfterAuth = false;
let resumeAutoCreationAfterAuth = false;

const syncActive = computed(() => Boolean(account.value?.syncProgress?.active));
const isCustomMailbox = computed(
  () => account.value?.mailboxType === "custom",
);
const receiveRuleLabel = computed(() => {
  switch (mailboxReceiveRule(account.value || {})) {
    case "custom":
      return "自定义域名邮箱";
    case "icloud-forwarded":
      return "iCloud 隐私邮箱 + 转发第三方 IMAP";
    default:
      return "iCloud 隐私邮箱 + iCloud IMAP";
  }
});

const aliasForm = reactive({ address: "", label: "" });
const appleLoginForm = reactive({ appleId: "", password: "", region: "global" });
const appleVerificationForm = reactive({ code: "" });
const appleAuthChallenge = reactive({ challengeId: "", flow: "" });
const appleLoginRules = {
  appleId: [{ required: true, message: "请填写 Apple ID", trigger: "blur" }],
  password: [{ required: true, message: "请填写 Apple 账户密码", trigger: "blur" }],
  region: [{ required: true, message: "请选择 Apple 服务区域", trigger: "change" }],
};
const appleVerificationRules = {
  code: [
    { required: true, message: "请填写验证码", trigger: "blur" },
    {
      pattern: /^\d{6}$/,
      message: "请输入 6 位数字验证码",
      trigger: ["blur", "change"],
    },
  ],
};
const aliasColumns = Object.freeze([
  {
    key: "address",
    dataKey: "address",
    title: "隐私邮箱",
    width: 230,
    minWidth: 230,
    flexGrow: 1,
  },
  {
    key: "group",
    title: "分组",
    width: 150,
    minWidth: 150,
    flexGrow: 1,
  },
  {
    key: "latestReceivedAt",
    dataKey: "latestReceivedAt",
    title: "最新邮件",
    width: 170,
    minWidth: 170,
    flexGrow: 1,
  },
  {
    key: "syncStatus",
    title: "同步状态",
    width: 130,
    minWidth: 130,
    flexGrow: 1,
  },
  {
    key: "enabled",
    dataKey: "enabled",
    title: "启用",
    width: 82,
    minWidth: 82,
    align: "center",
  },
  {
    key: "actions",
    title: "复制 / 操作",
    width: 330,
    minWidth: 330,
    align: "right",
  },
]);
const appleSessionAuthenticated = computed(
  () => appleSession.value?.status === "authenticated",
);
function appleVerificationActionLabel() {
  if (resumeAutoCreationAfterAuth) return "验证并开启";
  if (resumeAliasSyncAfterAuth) return "验证并同步";
  return "验证";
}
const autoCreationControlDisabled = computed(
  () =>
    autoCreationLoading.value ||
    randomAliasLoading.value ||
    aliasesSyncLoading.value ||
    appleDisconnectLoading.value ||
    appleAuthLoading.value,
);
const autoCreationToggleDisabled = computed(
  () =>
    autoCreationControlDisabled.value ||
    (!account.value?.enabled && !autoCreation.value?.enabled),
);
const appleAliasControlsDisabled = computed(
  () =>
    autoCreationLoading.value ||
    appleAuthLoading.value,
);

const AUTO_CREATION_ERROR_MESSAGES = Object.freeze({
  APPLE_LOGIN_REQUIRED:
    "Apple 账户尚未登录，请点击“同步隐私邮箱”并完成登录后重试",
  APPLE_SESSION_EXPIRED:
    "Apple 登录已过期，请点击“同步隐私邮箱”并重新登录后重试",
  APPLE_CREDENTIALS_INVALID:
    "Apple 登录凭据无效，请退出 Apple 登录并重新登录后重试",
  APPLE_VERIFICATION_INVALID:
    "Apple 验证码无效，请重新登录 Apple 账户并完成双重认证后重试",
  APPLE_FLOW_EXPIRED:
    "Apple 验证流程已过期，请重新登录 Apple 账户并完成双重认证后重试",
  APPLE_ACCOUNT_ACTION_REQUIRED:
    "Apple 账户需要完成条款确认或其他账户操作，请前往 Apple 官网处理后重试",
  APPLE_RATE_LIMITED:
    "Apple 请求过于频繁，自动创建已进入冷却，冷却后会继续执行",
  APPLE_UPSTREAM_ERROR:
    "Apple 服务暂时异常，请稍后再试；自动创建会按计划继续执行",
  APPLE_ALIAS_CONFIRMATION_PENDING:
    "Apple 已创建隐私邮箱，正在等待目录确认；后续自动创建计划只会继续确认，不会重复创建",
  APPLE_ACCOUNT_MISMATCH:
    "Apple 登录账户或隐藏邮件地址的默认转发目标与当前主号不匹配，请确认登录了正确的 Apple 账户，并在 iCloud 设置中把‘转发到’改为当前主号后重新开启",
  APPLE_FORWARDING_TARGET_MISSING:
    "Apple 未能确认隐私邮箱的默认转发目标，本次没有发起创建；请确认当前主号可作为转发邮箱，或先在 iCloud 手动创建一个隐私邮箱后重新同步",
  ACCOUNT_CHANGED: "主号信息在创建过程中发生变化，请刷新页面并确认主号信息后重试",
  ALIAS_OWNERSHIP_CONFLICT:
    "Apple 创建的隐私邮箱已属于其他主号，请检查主号归属后重试",
  ACCOUNT_DISABLED: "当前主号已停用，请先启用主号后再开启自动创建",
  AUTO_CREATION_UNAVAILABLE: "自动创建服务暂不可用，请稍后重试",
  ALIAS_LIMIT_REACHED: "当前主号已达到隐私邮箱容量上限，请确认自动创建计划状态",
  AUTO_CREATE_PLAN_CORRECTION_FAILED: "认领计划后无法保存下一次执行时间，请检查数据库状态",
  AUTO_CREATE_SCHEDULE_ERROR: "自动创建计划处理失败，请查看失败阶段和操作位置",
  AUTO_CREATION_PERSISTENCE_ERROR: "自动创建结果未能完整写入数据库，请检查数据库状态",
  AUTO_CREATION_CRYPTO_ERROR: "自动创建所需的本地密钥处理失败，请检查主密钥配置",
});

function autoCreationErrorMessage(value) {
  const original = String(value ?? "");
  const code = original.trim();
  return Object.prototype.hasOwnProperty.call(AUTO_CREATION_ERROR_MESSAGES, code)
    ? AUTO_CREATION_ERROR_MESSAGES[code]
    : original;
}

function isAliasConfirmationPending(item) {
  return (
    !item?.enabled &&
    String(item?.lastSyncError || "").trim() ===
      "APPLE_ALIAS_CONFIRMATION_PENDING"
  );
}

function isAliasV2(item) {
  return Boolean(
    item?.credentialMode !== "legacy" &&
      item?.apiKey &&
      item?.imapPassword &&
      item?.clientId &&
      item?.refreshToken,
  );
}

function isLegacyDirectLinkAvailable(item) {
  return Boolean(
    !isAliasConfirmationPending(item) &&
      item?.credentialMode === "legacy" &&
      item?.directLinkPath,
  );
}

function aliasRotationLabel(item) {
  return item?.credentialMode === "v2" ? "轮换整套凭证" : "轮换 API Key";
}

function normalizedAutoCreationStatus(item) {
  return String(item?.status || "")
    .trim()
    .toLowerCase()
    .replaceAll("-", "_");
}

function autoCreationStatusLabel(item) {
  if (account.value && !account.value.enabled) return "主号已停用";
  if (!item?.enabled) return "已关闭";
  switch (normalizedAutoCreationStatus(item)) {
    case "running":
    case "creating":
    case "in_progress":
      return "创建中";
    case "error":
    case "failed":
      return "最近失败";
    case "paused":
      return "已暂停";
    case "login_required":
      return "需要 Apple 登录";
    case "pending":
      return "等待执行";
    case "verification_required":
      return "需验证 Apple";
    case "scheduled":
    case "enabled":
    case "ready":
    case "idle":
    case "":
      return "已开启";
    default:
      return item.status || "已开启";
  }
}

function autoCreationStatusType(item) {
  if (!item?.enabled) return "info";
  switch (normalizedAutoCreationStatus(item)) {
    case "running":
    case "creating":
    case "in_progress":
    case "pending":
      return "warning";
    case "error":
    case "failed":
      return "danger";
    case "paused":
      return "info";
    case "login_required":
      return "warning";
    case "verification_required":
      return "warning";
    default:
      return "success";
  }
}

function formatAutoPlannedAt(value) {
  const planned = Array.isArray(value)
    ? value.filter(Boolean)
    : value
      ? [value]
      : [];
  if (!planned.length) return "-";
  const first = formatTime(planned[0], { seconds: true });
  return planned.length > 1 ? `${first} 等 ${planned.length} 个` : first;
}

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

function isAppleSessionInvalid(error) {
  return (
    error?.code === "APPLE_LOGIN_REQUIRED" ||
    error?.code === "APPLE_AUTH_REQUIRED" ||
    error?.code === "APPLE_SESSION_EXPIRED"
  );
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

function detailRequestKey(
  accountId = detailRouteKey(),
  page = currentPage.value,
  selectedPageSize = pageSize.value,
) {
  return `${accountId}\u0000${page}\u0000${selectedPageSize}`;
}

function replaceAlias(updated) {
  aliases.value = aliases.value.map((item) =>
    item.id === updated.id ? updated : item,
  );
}

function detailMutationPending() {
  return (
    syncLoading.value ||
    aliasesSyncLoading.value ||
    appleAuthLoading.value ||
    appleDisconnectLoading.value ||
    autoCreationLoading.value ||
    randomAliasLoading.value ||
    createLoading.value ||
    accountDeleteLoading.value ||
    Object.keys(groupMoveLoading).length > 0 ||
    aliasActionLock.hasAny() ||
    autoCreationLock.hasAny() ||
    randomAliasLock.hasAny()
  );
}

function beginDetailMutation() {
  detailGate.invalidate();
  detailAbortController?.abort();
  loading.value = false;
}

async function loadMailGroups({ silent = false } = {}) {
  const ticket = groupsLoadGate.begin("groups");
  if (!silent) {
    groupsLoading.value = true;
    groupsError.value = null;
  }
  try {
    const nextGroups = await getMailGroups();
    if (!groupsLoadGate.isCurrent(ticket, "groups")) return false;
    groups.value = nextGroups;
    groupsError.value = null;
    return true;
  } catch (error) {
    if (!silent && groupsLoadGate.isCurrent(ticket, "groups")) {
      groupsError.value = error;
    }
    return false;
  } finally {
    if (groupsLoadGate.isCurrent(ticket, "groups")) {
      groupsLoading.value = false;
    }
  }
}

async function loadDetail({ silent = false } = {}) {
  if (
    silent &&
    (loading.value || detailMutationPending())
  ) {
    return false;
  }
  const accountId = detailRouteKey();
  const page = currentPage.value;
  const selectedPageSize = pageSize.value;
  const requestKey = detailRequestKey(accountId, page, selectedPageSize);
  const ticket = detailGate.begin(requestKey);
  detailAbortController?.abort();
  const abortController = new AbortController();
  detailAbortController = abortController;
  if (!silent) {
    loading.value = true;
    loadError.value = null;
  }
  try {
    let detail;
    let nextAliases;
    if (selectedPageSize === ALL_PAGE_SIZE) {
      [detail, nextAliases] = await Promise.all([
        getAccount(accountId, {
          limit: DEFAULT_PAGE_SIZE,
          offset: 0,
          signal: abortController.signal,
        }),
        getAllAliases(accountId, { signal: abortController.signal }),
      ]);
    } else {
      detail = await getAccount(accountId, {
        limit: selectedPageSize,
        offset: (page - 1) * selectedPageSize,
        signal: abortController.signal,
      });
      nextAliases = Array.isArray(detail?.aliases) ? detail.aliases : [];
    }
    if (
      !viewActive ||
      !detailGate.isCurrent(ticket, detailRequestKey())
    ) {
      return false;
    }
    const allItems = selectedPageSize === ALL_PAGE_SIZE;
    const reportedTotal = Number(detail?.pagination?.total);
    const resolvedTotal = allItems
      ? nextAliases.length
      : Number.isFinite(reportedTotal) && reportedTotal >= 0
        ? Math.trunc(reportedTotal)
        : Math.max(Number(detail?.account?.aliasCount) || 0, nextAliases.length);
    const lastPage = allItems
      ? 1
      : Math.max(1, Math.ceil(resolvedTotal / selectedPageSize));
    if (!allItems && page > lastPage) {
      currentPage.value = lastPage;
      aliases.value = [];
      total.value = resolvedTotal;
      loadError.value = null;
      return await loadDetail({ silent });
    }
    account.value = allItems
      ? { ...detail.account, aliasCount: resolvedTotal }
      : detail.account;
    aliases.value = nextAliases;
    total.value = resolvedTotal;
    void loadMailGroups({ silent });
    appleSession.value = detail.appleSession;
    autoCreation.value = detail.autoCreation || null;
    loadError.value = null;
    setPageHeader(
      detail.account.mailboxType === "custom"
        ? `@${detail.account.emailSuffix}`
        : detail.account.email,
      "管理 IMAP 连接、同步状态和所属隐私邮箱",
    );
    return true;
  } catch (error) {
    abortController.abort();
    if (
      error?.name === "AbortError" ||
      silent ||
      !detailGate.isCurrent(ticket, detailRequestKey())
    ) {
      return false;
    }
    loadError.value = error;
    return false;
  } finally {
    if (
      !silent &&
      detailAbortController === abortController &&
      detailGate.isCurrent(ticket, detailRequestKey())
    ) {
      loading.value = false;
    }
    if (detailAbortController === abortController) {
      detailAbortController = null;
    }
  }
}

function handlePageChange(page) {
  if (pageSize.value === ALL_PAGE_SIZE) return;
  const nextPage = Math.max(1, Number(page) || 1);
  if (nextPage === currentPage.value) return;
  currentPage.value = nextPage;
  aliases.value = [];
  loadError.value = null;
  void loadDetail();
}

function handlePageSizeChange(value) {
  const nextPageSize = normalizePageSize(value);
  if (nextPageSize === pageSize.value) return;
  pageSize.value = nextPageSize;
  currentPage.value = 1;
  aliases.value = [];
  total.value = 0;
  loadError.value = null;
  detailGate.invalidate();
  detailAbortController?.abort();
  void loadDetail();
}

const liveRefresh = createLiveRefresh(() => loadDetail({ silent: true }));

async function syncNow() {
  if (syncLoading.value || syncActive.value || randomAliasLoading.value) return;
  beginDetailMutation();
  const accountId = account.value.id;
  syncLoading.value = true;
  try {
    const detail = await syncAccount(accountId, auth.state.csrfToken);
    if (!isCurrentAccount(accountId)) return;
    account.value = detail.account;
    if (detail.autoCreation) {
      autoCreation.value = detail.autoCreation;
    }
    await loadDetail();
    if (!isCurrentAccount(accountId)) return;
    if (detail.syncPending) {
      ElMessage({ type: "warning", message: "同步已在后台处理。" });
    } else {
      successMessage("同步已完成。");
    }
  } catch (error) {
    if (!isCurrentAccount(accountId)) return;
    showRequestError(error, "同步失败，请检查连接状态。");
    await loadDetail();
  } finally {
    syncLoading.value = false;
  }
}

function resetAppleAuthForm() {
  appleAuthError.value = null;
  appleLoginForm.password = "";
  appleVerificationForm.code = "";
  Object.assign(appleAuthChallenge, { challengeId: "", flow: "" });
}

function openAppleLogin({
  error = null,
  resumeSync = false,
  resumeAutoCreation = false,
} = {}) {
  if (!account.value) return;
  resumeAliasSyncAfterAuth = resumeSync;
  resumeAutoCreationAfterAuth = resumeAutoCreation;
  appleAuthStep.value = "login";
  appleAuthError.value = error;
  appleLoginForm.appleId = appleSession.value?.appleId || account.value.email || "";
  appleLoginForm.password = "";
  appleLoginForm.region = appleSession.value?.region || "global";
  appleVerificationForm.code = "";
  Object.assign(appleAuthChallenge, { challengeId: "", flow: "" });
  appleAuthVisible.value = true;
  nextTick(() => appleLoginFormRef.value?.clearValidate());
}

function cancelAppleAuth() {
  if (appleAuthLoading.value) return;
  resumeAliasSyncAfterAuth = false;
  resumeAutoCreationAfterAuth = false;
  appleAuthVisible.value = false;
  resetAppleAuthForm();
}

function closeAppleAuthDialog(done) {
  if (appleAuthLoading.value) return;
  resumeAliasSyncAfterAuth = false;
  resumeAutoCreationAfterAuth = false;
  resetAppleAuthForm();
  done();
}

function returnToAppleLogin() {
  if (appleAuthLoading.value) return;
  appleAuthStep.value = "login";
  appleAuthError.value = null;
  appleLoginForm.password = "";
  appleVerificationForm.code = "";
  Object.assign(appleAuthChallenge, { challengeId: "", flow: "" });
  nextTick(() => appleLoginFormRef.value?.clearValidate());
}

function mergedAppleSession(result) {
  return {
    status: result.status,
    appleId: result.appleSession?.appleId || appleLoginForm.appleId.trim(),
    region: result.appleSession?.region || appleLoginForm.region,
    authenticatedAt: result.appleSession?.authenticatedAt || null,
    expiresAt: result.appleSession?.expiresAt || null,
  };
}

async function finishAppleAuthentication(result, accountId) {
  if (!isCurrentAccount(accountId)) return;
  appleSession.value = mergedAppleSession(result);
  const shouldResumeSync = resumeAliasSyncAfterAuth;
  const shouldResumeAutoCreation = resumeAutoCreationAfterAuth;
  resumeAliasSyncAfterAuth = false;
  resumeAutoCreationAfterAuth = false;
  appleAuthVisible.value = false;
  resetAppleAuthForm();
  successMessage("Apple 账户已登录。");
  if (shouldResumeAutoCreation) {
    await nextTick();
    await performSetAutoCreation(true);
  }
  if (shouldResumeSync) {
    await nextTick();
    await performAliasesSync();
  }
}

async function submitAppleLogin() {
  if (appleAuthLoading.value || !account.value) return;
  const valid = await appleLoginFormRef.value?.validate().catch(() => false);
  if (!valid) return;
  const accountId = account.value.id;
  beginDetailMutation();
  appleAuthLoading.value = true;
  appleAuthError.value = null;
  try {
    const result = await loginAppleSession(
      accountId,
      {
        apple_id: appleLoginForm.appleId.trim(),
        password: appleLoginForm.password,
        region: appleLoginForm.region,
      },
      auth.state.csrfToken,
    );
    if (!isCurrentAccount(accountId)) return;
    appleSession.value = mergedAppleSession(result);
    appleLoginForm.password = "";
    if (result.status === "verification_required") {
      if (!result.challengeId) {
        throw new Error("Apple 登录未返回验证码挑战标识，请重新登录。");
      }
      Object.assign(appleAuthChallenge, {
        challengeId: result.challengeId,
        flow: result.flow,
      });
      appleAuthStep.value = "verification";
      await nextTick();
      appleVerificationFormRef.value?.clearValidate();
      return;
    }
    if (result.status === "authenticated") {
      await finishAppleAuthentication(result, accountId);
      return;
    }
    throw new Error("Apple 登录返回了未知状态，请重试。");
  } catch (error) {
    if (!isCurrentAccount(accountId)) return;
    appleAuthError.value = error;
  } finally {
    appleAuthLoading.value = false;
  }
}

async function submitAppleVerification() {
  if (appleAuthLoading.value || !account.value) return;
  const valid = await appleVerificationFormRef.value
    ?.validate()
    .catch(() => false);
  if (!valid) return;
  const accountId = account.value.id;
  beginDetailMutation();
  appleAuthLoading.value = true;
  appleAuthError.value = null;
  try {
    const verificationPayload = {
      challenge_id: appleAuthChallenge.challengeId,
      code: appleVerificationForm.code.trim(),
    };
    if (appleAuthChallenge.flow) {
      verificationPayload.flow = appleAuthChallenge.flow;
    }
    const result = await verifyAppleSession(
      accountId,
      verificationPayload,
      auth.state.csrfToken,
    );
    if (!isCurrentAccount(accountId)) return;
    if (result.status !== "authenticated") {
      throw new Error("验证码已提交，但 Apple 登录尚未完成，请重试。");
    }
    await finishAppleAuthentication(result, accountId);
  } catch (error) {
    if (!isCurrentAccount(accountId)) return;
    if (isAppleSessionInvalid(error)) {
      openAppleLogin({
        error,
        resumeSync: resumeAliasSyncAfterAuth,
        resumeAutoCreation: resumeAutoCreationAfterAuth,
      });
      return;
    }
    appleAuthError.value = error;
  } finally {
    appleAuthLoading.value = false;
  }
}

async function toggleAutoCreation(enabled) {
  const desiredEnabled = Boolean(enabled);
  if (
    !account.value ||
    autoCreationControlDisabled.value ||
    Boolean(autoCreation.value?.enabled) === desiredEnabled
  ) {
    return;
  }
  if (desiredEnabled && !account.value.enabled) {
    ElMessage.warning("主号已停用，不能开启自动创建。");
    return;
  }
  if (desiredEnabled && !appleSessionAuthenticated.value) {
    openAppleLogin({ resumeAutoCreation: true });
    return;
  }
  await performSetAutoCreation(desiredEnabled);
}

async function performSetAutoCreation(enabled) {
  if (!account.value || !autoCreationLock.acquire()) return false;
  const accountId = account.value.id;
  const desiredEnabled = Boolean(enabled);
  beginDetailMutation();
  autoCreationLoading.value = true;
  try {
    const updated = await setAliasAutoCreation(
      accountId,
      desiredEnabled,
      auth.state.csrfToken,
    );
    if (!isCurrentAccount(accountId)) return false;
    autoCreation.value = updated;
    successMessage(
      desiredEnabled
        ? "自动创建隐私邮箱已开启。"
        : "自动创建隐私邮箱已关闭。",
    );
    return true;
  } catch (error) {
    if (!isCurrentAccount(accountId)) return false;
    if (desiredEnabled && isAppleSessionInvalid(error)) {
      if (appleSession.value) {
        appleSession.value = { ...appleSession.value, status: "expired" };
      }
      openAppleLogin({ error, resumeAutoCreation: true });
      return false;
    }
    showRequestError(error, "自动创建设置更新失败，请稍后重试。");
    return false;
  } finally {
    autoCreationLoading.value = false;
    autoCreationLock.release();
  }
}

function syncAliasesFromApple() {
  if (
    aliasesSyncLoading.value ||
    autoCreationLoading.value
  ) {
    return;
  }
  if (!appleSessionAuthenticated.value) {
    openAppleLogin({ resumeSync: true });
    return;
  }
  performAliasesSync();
}

async function performAliasesSync() {
  if (
    aliasesSyncLoading.value ||
    autoCreationLoading.value ||
    !account.value
  ) {
    return;
  }
  const accountId = account.value.id;
  beginDetailMutation();
  aliasesSyncLoading.value = true;
  try {
    const result = await syncAccountAliases(accountId, auth.state.csrfToken);
    if (!isCurrentAccount(accountId)) return;
    account.value = result.account;
    appleSession.value = result.appleSession || appleSession.value;
    if (result.autoCreation) {
      autoCreation.value = result.autoCreation;
    }
    aliasSyncSummary.value = result.summary;
    const detailLoaded = await loadDetail();
    if (!isCurrentAccount(accountId)) return;
    if (!detailLoaded) {
      ElMessage({
        type: "warning",
        message: "隐私邮箱同步已完成，但邮箱列表刷新失败，请重新加载邮箱列表。",
      });
      return;
    }
    const capacityNotice = result.summary.importedDisabledCount
      ? `，其中 ${result.summary.importedDisabledCount} 个因本地容量暂未启用`
      : "";
    const createdNotice = result.summary.createdCount
      ? `，新增 ${result.summary.createdCount} 个，可通过列表中的复制操作导出完整凭证`
      : "，没有新增地址";
    successMessage(
      `隐私邮箱同步完成，Apple 共 ${result.summary.total} 个地址${createdNotice}${capacityNotice}。`,
    );
  } catch (error) {
    if (!isCurrentAccount(accountId)) return;
    if (isAppleSessionInvalid(error)) {
      if (appleSession.value) {
        appleSession.value = { ...appleSession.value, status: "expired" };
      }
      openAppleLogin({ error, resumeSync: true });
      return;
    }
    showRequestError(error, "隐私邮箱同步失败，请稍后重试。");
  } finally {
    aliasesSyncLoading.value = false;
  }
}

async function disconnectAppleSession() {
  if (
    !appleSessionAuthenticated.value ||
    autoCreationLoading.value ||
    !appleDisconnectLock.acquire() ||
    !account.value
  ) {
    return;
  }
  const accountId = account.value.id;
  beginDetailMutation();
  appleDisconnectLoading.value = true;
  try {
    await ElMessageBox.confirm(
      "退出后，下次同步隐私邮箱时需要重新登录 Apple 账户。",
      "退出 Apple 登录",
      {
        type: "warning",
        confirmButtonText: "退出登录",
        cancelButtonText: "取消",
        autofocus: false,
      },
    );
    if (!isCurrentAccount(accountId)) return;
    await deleteAppleSession(accountId, auth.state.csrfToken);
    if (!isCurrentAccount(accountId)) return;
    appleSession.value = null;
    aliasSyncSummary.value = null;
    successMessage("Apple 登录已退出。");
  } catch (error) {
    if (confirmationCancelled(error)) return;
    if (!isCurrentAccount(accountId)) return;
    showRequestError(error, "退出 Apple 登录失败，请稍后重试。");
  } finally {
    appleDisconnectLoading.value = false;
    appleDisconnectLock.release();
  }
}

async function addAlias() {
  if (!createLock.acquire()) return;
  beginDetailMutation();
  const accountId = account.value.id;
  let sessionInvalid = false;
  createLoading.value = true;
  try {
    aliasFormError.value = null;
    const valid = await aliasFormRef.value?.validate().catch(() => false);
    if (!valid || !isCurrentAccount(accountId)) return;

    await createAlias(
      accountId,
      {
        address: aliasForm.address.trim(),
        label: aliasForm.label.trim(),
      },
      auth.state.csrfToken,
    );
    if (!isCurrentAccount(accountId)) return;
    await loadDetail();
    if (!isCurrentAccount(accountId)) return;
    Object.assign(aliasForm, { address: "", label: "" });
    aliasFormRef.value?.resetFields();
    successMessage("隐私邮箱已添加，整套凭证已签发，可通过列表中的复制操作导出。");
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

async function generateRandomAliases() {
  if (!account.value) return;
  if (!account.value.enabled) {
    randomAliasError.value = new Error("主号已停用，不能生成随机邮箱");
    return;
  }
  if (
    syncLoading.value ||
    syncActive.value ||
    !randomAliasLock.acquire()
  ) return;
  const accountId = account.value.id;
  const count = Number(randomAliasCount.value);
  if (!Number.isInteger(count) || count < 1 || count > 1000) {
    randomAliasError.value = new Error("生成数量必须是 1 到 1000 之间的整数");
    randomAliasLock.release();
    return;
  }
  beginDetailMutation();
  randomAliasLoading.value = true;
  randomAliasError.value = null;
  try {
    const result = await createRandomAliases(
      accountId,
      { count },
      auth.state.csrfToken,
    );
    if (!isCurrentAccount(accountId)) return;
    const generatedCount = Math.max(
      0,
      Number(result.count) || (Array.isArray(result.created) ? result.created.length : 0),
    );
    await loadDetail();
    if (!isCurrentAccount(accountId)) return;
    successMessage(`已生成 ${generatedCount} 个随机邮箱，可通过列表中的复制操作导出完整凭证。`);
  } catch (error) {
    if (!isCurrentAccount(accountId)) return;
    randomAliasError.value = error;
    showRequestError(error, "随机邮箱生成失败，请稍后重试。");
  } finally {
    randomAliasLoading.value = false;
    randomAliasLock.release();
  }
}

async function rotateKey(alias) {
  if (
    isAliasConfirmationPending(alias) ||
    !aliasActionLock.acquire(alias.id)
  ) {
    return;
  }
  beginDetailMutation();
  const accountId = account.value.id;
  let sessionInvalid = false;
  rotateLoading[alias.id] = true;
  const rotateCompleteBundle = alias?.credentialMode === "v2";
  try {
    await ElMessageBox.confirm(
      rotateCompleteBundle
        ? "轮换后旧 API Key、取码链接、IMAP 密码、refresh token 和访问令牌会同时失效。继续吗？"
        : "轮换后旧 API Key 和旧直达链接会失效；邮件消费状态和 IMAP 已读状态保持不变。新凭证请通过列表中的复制操作导出并保存。继续吗？",
      `${aliasRotationLabel(alias)}：${alias.address}`,
      {
        type: "warning",
        confirmButtonText: "继续轮换",
        cancelButtonText: "取消",
        autofocus: false,
      },
    );
    if (!isCurrentAccount(accountId)) return;
    const result = await rotateAlias(
      alias.id,
      auth.state.csrfToken,
      alias.credentialMode,
    );
    if (!isCurrentAccount(accountId)) return;
    replaceAlias({
      ...result.alias,
      apiKey: result.apiKey || result.alias.apiKey,
    });
    successMessage(
      rotateCompleteBundle
        ? "整套凭证已轮换，所有旧凭证与访问令牌均已失效。"
        : "API Key 已轮换，请通过列表中的复制操作导出并保存新凭证；消费和邮件状态保持不变。",
    );
  } catch (error) {
    if (confirmationCancelled(error)) return;
    sessionInvalid = isSessionInvalid(error);
    if (!isCurrentAccount(accountId)) return;
    showRequestError(
      error,
      rotateCompleteBundle
        ? "整套凭证轮换失败，请稍后重试。"
        : "API Key 轮换失败，请稍后重试。",
    );
  } finally {
    delete rotateLoading[alias.id];
    aliasActionLock.release(alias.id);
    if (sessionInvalid) redirectExpiredSession();
  }
}

function isCopying(alias) {
  return Boolean(
    copyLoading[`${alias?.id}:otp`] ||
      copyLoading[`${alias?.id}:imap`] ||
      copyLoading[`${alias?.id}:legacy-link`],
  );
}

function isAliasActionBusy(alias) {
  const id = alias?.id;
  return Boolean(
    isCopying(alias) ||
      toggleLoading[id] ||
      rotateLoading[id] ||
      deleteLoading[id] ||
      groupMoveLoading[id],
  );
}

async function copyAliasCredentials(alias, format) {
  const loadingKey = `${alias?.id}:${format}`;
  if (
    isAliasConfirmationPending(alias) ||
    !aliasActionLock.acquire(alias.id)
  ) {
    return;
  }
  const accountId = account.value.id;
  copyLoading[loadingKey] = true;
  try {
    const copied = await copyText(buildAliasExportText([alias], format));
    if (!isCurrentAccount(accountId)) return;
    if (!copied) {
      ElMessage({
        type: "error",
        message: "邮箱凭证复制失败，请检查浏览器剪切板权限后重试。",
        grouping: true,
      });
      return;
    }
    successMessage(
      format === ALIAS_EXPORT_OTP
        ? "取码链接格式已复制。"
        : "IMAP/OAuth 格式已复制。",
    );
  } catch {
    if (!isCurrentAccount(accountId)) return;
    ElMessage({
      type: "error",
      message: "邮箱凭证复制失败，请刷新页面后重试。",
      grouping: true,
    });
  } finally {
    delete copyLoading[loadingKey];
    aliasActionLock.release(alias.id);
  }
}

async function copyLegacyDirectLink(alias) {
  const lockKey = `${alias?.id}:legacy-link`;
  if (!isLegacyDirectLinkAvailable(alias) || !aliasActionLock.acquire(alias.id)) {
    return;
  }
  const accountId = account.value.id;
  copyLoading[lockKey] = true;
  try {
    const directLink = buildRecentMailDirectLink(alias.directLinkPath);
    const copied = await copyText(directLink);
    if (!isCurrentAccount(accountId)) return;
    if (!copied) throw new Error("clipboard rejected copy");
    successMessage("旧邮件 API 直达链接已复制。");
  } catch {
    if (!isCurrentAccount(accountId)) return;
    ElMessage({
      type: "error",
      message: "旧直达链接复制失败，请刷新页面后重试。",
      grouping: true,
    });
  } finally {
    delete copyLoading[lockKey];
    aliasActionLock.release(alias.id);
  }
}

async function toggleAlias(alias, enabled) {
  if (
    isAliasConfirmationPending(alias) ||
    alias.enabled === enabled ||
    !aliasActionLock.acquire(alias.id)
  ) {
    return;
  }
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

async function moveAliasToGroup(alias, groupValue) {
  if (!alias || !aliasActionLock.acquire(alias.id)) return;
  beginDetailMutation();
  groupMoveLoading[alias.id] = true;
  const accountId = account.value?.id;
  try {
    const updated = await moveAliasToGroupRequest(
      alias.id,
      groupValue === "" || groupValue == null ? null : groupValue,
      auth.state.csrfToken,
    );
    if (!isCurrentAccount(accountId)) return;
    replaceAlias(updated);
    await loadMailGroups();
    successMessage("隐私邮箱分组已更新。");
  } catch (error) {
    if (isCurrentAccount(accountId)) {
      showRequestError(error, "隐私邮箱分组移动失败，请稍后重试。");
    }
  } finally {
    delete groupMoveLoading[alias.id];
    aliasActionLock.release(alias.id);
  }
}

async function removeAlias(alias) {
  if (
    isAliasConfirmationPending(alias) ||
    !aliasActionLock.acquire(alias.id)
  ) {
    return;
  }
  beginDetailMutation();
  const accountId = account.value.id;
  deleteLoading[alias.id] = true;
  const customMailbox = isCustomMailbox.value;
  try {
    await ElMessageBox.confirm(
      customMailbox
        ? "删除后，该邮箱将从本地永久删除，同时清除完整凭证、邮件归档映射及关联记录，且无法恢复。继续吗？"
        : "删除后，该隐私邮箱将从 iCloud 永久删除，同时清除本地完整凭证、邮件归档映射及关联记录，且无法恢复。继续吗？",
      `${customMailbox ? "从本地永久删除" : "从 iCloud 永久删除"} ${alias.address}`,
      {
        type: "warning",
        confirmButtonText: "永久删除",
        cancelButtonText: "取消",
        confirmButtonClass: "el-button--danger",
        autofocus: false,
      },
    );
    if (!isCurrentAccount(accountId)) return;
    await deleteAlias(alias.id, auth.state.csrfToken);
    if (!isCurrentAccount(accountId)) return;
    await loadDetail();
    if (!isCurrentAccount(accountId)) return;
    successMessage(
      customMailbox
        ? "邮箱已从本地永久删除。"
        : "隐私邮箱已从 iCloud 和本地永久删除。",
    );
  } catch (error) {
    if (confirmationCancelled(error)) return;
    if (!isCurrentAccount(accountId)) return;
    if (isAppleSessionInvalid(error)) {
      if (appleSession.value) {
        appleSession.value = { ...appleSession.value, status: "expired" };
      }
      openAppleLogin({ error });
      return;
    }
    showRequestError(
      error,
      "隐私邮箱删除未完成，本地记录已保留，请稍后重试。",
    );
  } finally {
    delete deleteLoading[alias.id];
    aliasActionLock.release(alias.id);
  }
}

async function removeAccount() {
  if (!accountDeleteLock.acquire()) return;
  beginDetailMutation();
  const accountId = account.value.id;
  const accountIdentity = isCustomMailbox.value
    ? `@${account.value.emailSuffix}`
    : account.value.email;
  accountDeleteLoading.value = true;
  try {
    await ElMessageBox.confirm(
      `确定删除主号 ${accountIdentity} 及其全部数据吗？`,
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

watch(
  () => route.params.id,
  (id, previousId) => {
    if (id && id !== previousId) {
      detailGate.invalidate();
      detailAbortController?.abort();
      loading.value = false;
      cancelAppleAuth();
      account.value = null;
      aliases.value = [];
      currentPage.value = 1;
      total.value = 0;
      appleSession.value = null;
      autoCreation.value = null;
      aliasSyncSummary.value = null;
      randomAliasCount.value = 1;
      randomAliasError.value = null;
      loadDetail();
    }
  },
);

onMounted(() => {
  loadDetail();
  liveRefresh.start({ immediate: false });
});

onBeforeUnmount(() => {
  viewActive = false;
  liveRefresh.stop();
  detailGate.deactivate();
  detailAbortController?.abort();
  groupsLoadGate.deactivate();
  autoCreation.value = null;
  randomAliasError.value = null;
});
</script>

<style scoped>
.credential-value {
  display: block;
  max-width: 100%;
  overflow-x: auto;
  color: var(--text-primary);
  font-size: 12px;
  white-space: nowrap;
  user-select: all;
}

.alias-group-select {
  width: 132px;
}

.mobile-alias-group-select {
  min-width: 150px;
}
</style>
