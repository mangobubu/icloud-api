<template>
  <section class="content-narrow page-stack">
    <div v-if="loading" class="form-panel loading-panel">
      <el-skeleton :rows="7" animated />
    </div>

    <div v-else-if="loadError" class="load-failed">
      <RequestAlert :error="loadError" />
      <div class="button-row">
        <el-button :icon="Back" @click="cancel">返回</el-button>
        <el-button type="primary" :icon="Refresh" @click="loadAccount">
          重新加载
        </el-button>
      </div>
    </div>

    <el-form
      v-else
      ref="formRef"
      class="form-panel"
      :model="form"
      :rules="rules"
      label-position="top"
      :disabled="submitting"
      status-icon
      @submit.prevent="submit"
    >
      <RequestAlert
        v-if="submitError"
        :error="submitError"
        closable
        @close="submitError = null"
      />

      <div class="form-grid">
        <el-form-item label="备注" prop="name">
          <el-input
            v-model="form.name"
            maxlength="80"
            show-word-limit
            placeholder="例如：个人主号"
            autocomplete="off"
          />
        </el-form-item>

        <el-form-item label="iCloud 主号邮箱" prop="email">
          <el-input
            v-model="form.email"
            type="email"
            placeholder="name@icloud.com"
            autocomplete="email"
            :readonly="identityLocked"
          />
        </el-form-item>

        <el-form-item label="IMAP 用户名" prop="imapUsername">
          <el-input
            v-model="form.imapUsername"
            placeholder="通常填写完整 iCloud 邮箱"
            autocomplete="username"
            :readonly="identityLocked"
          />
          <p v-if="identityLocked" class="field-help">
            已有隐私邮箱后，主号邮箱和 IMAP 用户名不能修改。
          </p>
        </el-form-item>

        <el-form-item label="IMAP 服务">
          <el-input model-value="imap.mail.me.com:993（TLS）" readonly />
        </el-form-item>

        <el-form-item class="form-span" label="App 专用密码" prop="imapPassword">
          <el-input
            v-model="form.imapPassword"
            type="password"
            show-password
            :placeholder="isEdit ? '留空则保留当前密码' : '在 Apple 账户中生成的专用密码'"
            autocomplete="new-password"
          />
          <p class="field-help">专用密码只会加密保存，后续不会回显。</p>
        </el-form-item>

        <el-form-item v-if="isEdit" class="form-span account-enabled-field">
          <div class="switch-field">
            <div>
              <strong>启用主号同步</strong>
              <p>停用后，此主号下所有隐私邮箱的 Key 会立即失效。</p>
            </div>
            <el-switch
              v-model="form.enabled"
              inline-prompt
              active-text="启用"
              inactive-text="停用"
              aria-label="启用主号同步"
            />
          </div>
        </el-form-item>
      </div>

      <div class="form-actions">
        <el-button :icon="Close" @click="cancel">取消</el-button>
        <el-button
          native-type="submit"
          type="primary"
          :icon="Check"
          :loading="submitting"
        >
          保存
        </el-button>
      </div>
    </el-form>
  </section>
</template>

<script setup>
import { Back, Check, Close, Refresh } from "@element-plus/icons-vue";
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";

import {
  createAccount,
  getAccount,
  updateAccount,
} from "../api/admin.js";
import RequestAlert from "../components/RequestAlert.vue";
import { useAuth } from "../stores/auth.js";
import {
  createActionLock,
  createLatestRequestGate,
} from "../utils/asyncState.js";
import { successMessage } from "../utils/feedback.js";

const route = useRoute();
const router = useRouter();
const auth = useAuth();
const formRef = ref(null);
const loading = ref(false);
const submitting = ref(false);
const loadError = ref(null);
const submitError = ref(null);
const aliasCount = ref(0);
const loadGate = createLatestRequestGate();
const submitLock = createActionLock();
let viewActive = true;

const isEdit = computed(() => route.name === "account-edit");
const identityLocked = computed(() => isEdit.value && aliasCount.value > 0);

const form = reactive({
  name: "",
  email: "",
  imapUsername: "",
  imapPassword: "",
  enabled: true,
});

function routeKey(name = route.name, id = route.params.id) {
  return `${String(name || "")}:${String(id || "")}`;
}

function runeLength(value) {
  return Array.from(String(value || "")).length;
}

function validateEmail(_, value, callback) {
  const normalized = String(value || "").trim();
  if (!normalized) {
    callback(new Error("请填写 iCloud 主号邮箱"));
    return;
  }
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(normalized)) {
    callback(new Error("邮箱地址格式不正确"));
    return;
  }
  callback();
}

function validateName(_, value, callback) {
  if (runeLength(value) > 80) {
    callback(new Error("备注不能超过 80 个字符"));
    return;
  }
  callback();
}

function validatePassword(_, value, callback) {
  if (!isEdit.value && !String(value || "").trim()) {
    callback(new Error("请填写 App 专用密码"));
    return;
  }
  callback();
}

const rules = {
  name: [{ validator: validateName, trigger: "blur" }],
  email: [{ validator: validateEmail, trigger: ["blur", "change"] }],
  imapUsername: [
    { required: true, message: "请填写 IMAP 用户名", trigger: "blur" },
  ],
  imapPassword: [{ validator: validatePassword, trigger: "blur" }],
};

function resetForm() {
  Object.assign(form, {
    name: "",
    email: "",
    imapUsername: "",
    imapPassword: "",
    enabled: true,
  });
  aliasCount.value = 0;
  loadError.value = null;
  submitError.value = null;
  formRef.value?.clearValidate();
}

async function loadAccount() {
  if (!isEdit.value) return;
  const accountId = String(route.params.id || "");
  const ticket = loadGate.begin(routeKey());
  loading.value = true;
  loadError.value = null;
  try {
    const { account } = await getAccount(accountId);
    if (!loadGate.isCurrent(ticket, routeKey())) return;
    Object.assign(form, {
      name: account.name,
      email: account.email,
      imapUsername: account.imapUsername,
      imapPassword: "",
      enabled: account.enabled,
    });
    aliasCount.value = account.aliasCount;
  } catch (error) {
    if (!loadGate.isCurrent(ticket, routeKey())) return;
    loadError.value = error;
  } finally {
    if (loadGate.isCurrent(ticket, routeKey())) {
      loading.value = false;
    }
  }
}

async function submit() {
  if (!submitLock.acquire()) return;
  const submittedRouteKey = routeKey();
  const submittedAccountId = String(route.params.id || "");
  submitting.value = true;
  try {
    submitError.value = null;
    const valid = await formRef.value?.validate().catch(() => false);
    if (!valid || submittedRouteKey !== routeKey()) return;

    const payload = {
      name: form.name.trim(),
      email: form.email.trim(),
      imap_username: form.imapUsername.trim(),
      imap_password: form.imapPassword.trim(),
    };
    const account = isEdit.value
      ? await updateAccount(
          submittedAccountId,
          { ...payload, enabled: Boolean(form.enabled) },
          auth.state.csrfToken,
        )
      : await createAccount(payload, auth.state.csrfToken);
    if (!viewActive || submittedRouteKey !== routeKey()) return;
    form.imapPassword = "";
    successMessage(isEdit.value ? "主号设置已保存。" : "主号已添加。");
    await router.replace({ name: "account-detail", params: { id: account.id } });
  } catch (error) {
    if (!viewActive || submittedRouteKey !== routeKey()) return;
    form.imapPassword = "";
    submitError.value = error;
    formRef.value?.clearValidate("imapPassword");
  } finally {
    submitting.value = false;
    submitLock.release();
  }
}

function cancel() {
  if (isEdit.value) {
    router.push({ name: "account-detail", params: { id: route.params.id } });
  } else {
    router.push({ name: "accounts" });
  }
}

watch(
  () => [route.name, route.params.id],
  () => {
    loadGate.invalidate();
    loading.value = false;
    resetForm();
    if (isEdit.value) loadAccount();
  },
);

onMounted(() => {
  if (isEdit.value) loadAccount();
});

onBeforeUnmount(() => {
  viewActive = false;
  loadGate.deactivate();
  form.imapPassword = "";
});
</script>
