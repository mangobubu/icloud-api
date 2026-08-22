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

        <el-form-item label="邮箱类型" prop="mailboxType">
          <el-select
            v-model="form.mailboxType"
            style="width: 100%"
            :disabled="emailLocked"
          >
            <el-option label="iCloud 隐私邮箱" value="icloud" />
            <el-option label="自定义邮箱" value="custom" />
          </el-select>
          <p class="field-help">
            iCloud 规则和自动创建计划保持不变；自定义邮箱可按后缀批量生成地址。
          </p>
        </el-form-item>

        <div class="form-span mailbox-route-summary" role="status">
          <strong>当前收件规则：{{ receiveRuleLabel }}</strong>
          <p class="field-help">{{ receiveRuleHelp }}</p>
        </div>

        <el-form-item v-if="!isCustomMailbox" label="iCloud 主号邮箱" prop="email">
          <el-input
            v-model="form.email"
            type="email"
            placeholder="name@icloud.com"
            autocomplete="email"
            :readonly="emailLocked"
          />
          <p v-if="emailLocked" class="field-help">
            已有隐私邮箱后，主号邮箱不能修改。
          </p>
        </el-form-item>

        <el-form-item
          v-if="isCustomMailbox"
          label="邮箱后缀"
          prop="emailSuffix"
        >
          <el-input
            v-model="form.emailSuffix"
            placeholder="example.com 或 @example.com"
            autocomplete="off"
            :readonly="emailLocked"
          />
          <p class="field-help">
            随机邮箱会按 8–12 位英文数字 + @后缀生成，不受 iCloud 每小时规则限制。
          </p>
        </el-form-item>

        <el-form-item label="IMAP 用户名" prop="imapUsername">
          <el-input
            v-model="form.imapUsername"
            :placeholder="isCustomMailbox ? '填写邮箱服务器的登录用户名' : '通常填写完整 iCloud 邮箱'"
            autocomplete="username"
          />
          <p v-if="!isCustomMailbox" class="field-help">
            若 iCloud 隐私邮箱转发到第三方邮箱，请填写第三方 IMAP 登录/最终投递邮箱（例如 mango@example.com）；X-Original-To 可能仍是中间转发地址。
          </p>
        </el-form-item>

        <el-form-item class="form-span imap-service-field" label="IMAP 服务">
          <div class="imap-service-fields">
            <el-form-item prop="imapHost" label="主机" class="imap-service-field__host">
              <el-input
                v-model="form.imapHost"
                placeholder="imap.mail.me.com"
                autocomplete="off"
                aria-label="IMAP 主机"
              />
            </el-form-item>
            <el-form-item prop="imapPort" label="端口" class="imap-service-field__port">
              <el-input-number
                v-model="form.imapPort"
                :step="1"
                :controls="false"
                aria-label="IMAP 端口"
              />
            </el-form-item>
          </div>
          <p class="field-help">默认使用 imap.mail.me.com:993（TLS）。</p>
        </el-form-item>

        <el-form-item
          class="form-span"
          :label="usesGenericIMAPPassword ? 'IMAP 密码' : 'App 专用密码'"
          prop="imapPassword"
        >
          <el-input
            v-model="form.imapPassword"
            type="password"
            show-password
            :placeholder="isEdit ? '留空则保留当前密码' : usesGenericIMAPPassword ? '填写邮箱服务器的 IMAP 密码' : '在 Apple 账户中生成的专用密码'"
            autocomplete="new-password"
          />
          <p class="field-help">
            {{ usesGenericIMAPPassword ? "第三方/自定义邮箱的 IMAP 密码只会加密保存，后续不会回显。" : "Apple App 专用密码只会加密保存，后续不会回显。" }}
          </p>
        </el-form-item>

        <el-form-item v-if="isEdit" class="form-span account-enabled-field">
          <div class="switch-field">
            <div>
              <strong>启用主号同步</strong>
              <p>停用后，此主号下所有隐私邮箱的全部取件凭证会立即失效。</p>
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
import {
  DEFAULT_IMAP_HOST,
  DEFAULT_IMAP_PORT,
  mailboxReceiveRule,
  normalizeIMAPEndpoint,
  validateIMAPHost as validateIMAPHostValue,
  validateIMAPPort as validateIMAPPortValue,
} from "../utils/imap.js";

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

const form = reactive({
  name: "",
  email: "",
  mailboxType: "icloud",
  emailSuffix: "",
  imapHost: DEFAULT_IMAP_HOST,
  imapPort: DEFAULT_IMAP_PORT,
  imapUsername: "",
  imapPassword: "",
  enabled: true,
});

const isEdit = computed(() => route.name === "account-edit");
const isCustomMailbox = computed(() => form.mailboxType === "custom");
const emailLocked = computed(() => isEdit.value && aliasCount.value > 0);
const receiveRule = computed(() => mailboxReceiveRule(form));
const usesGenericIMAPPassword = computed(
  () => isCustomMailbox.value || receiveRule.value === "icloud-forwarded",
);
const receiveRuleLabel = computed(() => {
  switch (receiveRule.value) {
    case "custom":
      return "自定义域名邮箱";
    case "icloud-forwarded":
      return "iCloud 隐私邮箱 + 转发第三方 IMAP";
    default:
      return "iCloud 隐私邮箱 + iCloud IMAP";
  }
});
const receiveRuleHelp = computed(() => {
  switch (receiveRule.value) {
    case "custom":
      return "按原始投递收件人标头匹配自定义域名地址。";
    case "icloud-forwarded":
      return "保留 iCloud 隐私邮箱类型；系统会结合 X-ICLOUD-HME 和转发链路投递标头匹配地址。";
    default:
      return "使用 iCloud IMAP 的主号投递标头匹配隐私邮箱。";
  }
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
    if (isCustomMailbox.value) {
      callback();
      return;
    }
    callback(new Error("请填写 iCloud 主号邮箱"));
    return;
  }
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(normalized)) {
    callback(new Error("邮箱地址格式不正确"));
    return;
  }
  callback();
}

function validateMailboxType(_, value, callback) {
  if (!["icloud", "custom"].includes(String(value || "").trim())) {
    callback(new Error("请选择邮箱类型"));
    return;
  }
  callback();
}

function normalizeSuffix(value) {
  return String(value || "").trim().replace(/^@+/, "").toLowerCase();
}

function validateEmailSuffix(_, value, callback) {
  if (!isCustomMailbox.value) {
    callback();
    return;
  }
  const normalized = normalizeSuffix(value);
  if (!normalized) {
    callback(new Error("请填写邮箱后缀"));
    return;
  }
  if (
    normalized.length > 241 ||
    !/^(?=.{1,241}$)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$/.test(
      normalized,
    )
  ) {
    callback(new Error("邮箱后缀格式不正确"));
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
  const password = String(value ?? "");
  const passwordMissing = usesGenericIMAPPassword.value
    ? password.length === 0
    : !password.trim();
  if (!isEdit.value && passwordMissing) {
    callback(
      new Error(
        usesGenericIMAPPassword.value
          ? "请填写 IMAP 密码"
          : "请填写 App 专用密码",
      ),
    );
    return;
  }
  callback();
}

function validateIMAPHost(_, value, callback) {
  const message = validateIMAPHostValue(value);
  if (message) {
    callback(new Error(message));
    return;
  }
  callback();
}

function validateIMAPPort(_, value, callback) {
  const message = validateIMAPPortValue(value);
  if (message) {
    callback(new Error(message));
    return;
  }
  callback();
}

const rules = {
  name: [{ validator: validateName, trigger: "blur" }],
  mailboxType: [{ validator: validateMailboxType, trigger: "change" }],
  email: [{ validator: validateEmail, trigger: ["blur", "change"] }],
  emailSuffix: [
    { validator: validateEmailSuffix, trigger: ["blur", "change"] },
  ],
  imapHost: [{ validator: validateIMAPHost, trigger: ["blur", "change"] }],
  imapPort: [{ validator: validateIMAPPort, trigger: ["blur", "change"] }],
  imapUsername: [
    { required: true, message: "请填写 IMAP 用户名", trigger: "blur" },
  ],
  imapPassword: [{ validator: validatePassword, trigger: "blur" }],
};

function resetForm() {
  Object.assign(form, {
    name: "",
    email: "",
    mailboxType: "icloud",
    emailSuffix: "",
    imapHost: DEFAULT_IMAP_HOST,
    imapPort: DEFAULT_IMAP_PORT,
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
    const imapEndpoint = normalizeIMAPEndpoint(
      account.imapHost,
      account.imapPort,
    );
    Object.assign(form, {
      name: account.name,
      email: account.mailboxType === "custom" ? "" : account.email,
      mailboxType: account.mailboxType || "icloud",
      emailSuffix: account.emailSuffix || "",
      imapHost: imapEndpoint.host,
      imapPort: imapEndpoint.port,
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

    const imapEndpoint = normalizeIMAPEndpoint(form.imapHost, form.imapPort);
    const payload = {
      name: form.name.trim(),
      mailbox_type: form.mailboxType,
      imap_host: imapEndpoint.host,
      imap_port: imapEndpoint.port,
      imap_username: form.imapUsername.trim(),
      imap_password: usesGenericIMAPPassword.value
        ? form.imapPassword
        : form.imapPassword.trim(),
    };
    if (isCustomMailbox.value) {
      payload.email_suffix = normalizeSuffix(form.emailSuffix);
    } else {
      payload.email = form.email.trim();
    }
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
