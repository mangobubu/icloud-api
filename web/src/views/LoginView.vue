<template>
  <main class="login-page">
    <div class="login-shell">
      <AppBrand title="iCloud 隐私邮箱" subtitle="后台管理" />

      <section class="login-panel" aria-labelledby="login-title">
        <div class="login-panel__heading">
          <h1 id="login-title">登录</h1>
          <p>使用管理员账号进入控制台。</p>
        </div>

        <el-alert
          v-if="noticeMessage"
          :title="noticeMessage"
          :type="noticeType"
          show-icon
          :closable="false"
          role="status"
        />

        <RequestAlert
          v-if="loginError"
          :error="loginError"
          closable
          @close="loginError = null"
        />

        <div v-if="initializing" class="login-initializing" aria-live="polite">
          <el-icon class="is-loading" :size="20" aria-hidden="true">
            <Loading />
          </el-icon>
          <span>正在确认登录状态…</span>
        </div>

        <div v-else-if="!csrfReady" class="login-retry">
          <el-button
            type="primary"
            :icon="Refresh"
            :loading="csrfLoading"
            @click="prepareLogin"
          >
            重新加载登录
          </el-button>
        </div>

        <el-form
          v-else
          ref="formRef"
          :model="form"
          :rules="rules"
          label-position="top"
          :disabled="submitting"
          @submit.prevent="submit"
        >
          <el-form-item label="用户名" prop="username">
            <el-input
              ref="usernameInput"
              v-model="form.username"
              autocomplete="username"
              maxlength="128"
              :prefix-icon="User"
            />
          </el-form-item>
          <el-form-item label="密码" prop="password">
            <el-input
              v-model="form.password"
              type="password"
              autocomplete="current-password"
              :prefix-icon="Lock"
              show-password
            />
          </el-form-item>
          <el-button
            class="login-submit"
            native-type="submit"
            type="primary"
            :loading="submitting"
          >
            登录
          </el-button>
        </el-form>
      </section>
    </div>
  </main>
</template>

<script setup>
import { Loading, Lock, Refresh, User } from "@element-plus/icons-vue";
import { computed, nextTick, onMounted, reactive, ref } from "vue";
import { useRoute, useRouter } from "vue-router";

import AppBrand from "../components/AppBrand.vue";
import RequestAlert from "../components/RequestAlert.vue";
import { useAuth } from "../stores/auth.js";
import { createActionLock } from "../utils/asyncState.js";
import { loginNoticeMessage } from "../utils/authFlow.js";

const route = useRoute();
const router = useRouter();
const auth = useAuth();
const formRef = ref(null);
const usernameInput = ref(null);
const initializing = ref(true);
const csrfLoading = ref(false);
const csrfReady = ref(false);
const submitting = ref(false);
const loginError = ref(null);
const sessionCheckFailed = ref(false);
const noticeDismissed = ref(false);
const submitLock = createActionLock();

const form = reactive({ username: "", password: "" });
const rules = {
  username: [{ required: true, message: "请填写用户名", trigger: "blur" }],
  password: [{ required: true, message: "请填写密码", trigger: "blur" }],
};

const noticeMessage = computed(() =>
  loginNoticeMessage({
    notice: String(route.query.notice || ""),
    sessionCheckFailed: sessionCheckFailed.value,
    dismissed: noticeDismissed.value,
  }),
);

const noticeType = computed(() =>
  route.query.notice === "password_changed" ? "success" : "warning",
);

function redirectTarget() {
  const target = String(route.query.redirect || "");
  if (
    target.startsWith("/admin") &&
    !target.startsWith("/admin/login") &&
    !target.startsWith("//")
  ) {
    return target;
  }
  return { name: "accounts" };
}

async function prepareLogin() {
  if (csrfLoading.value) return;
  csrfLoading.value = true;
  loginError.value = null;
  csrfReady.value = false;
  try {
    await auth.prepareLogin();
    csrfReady.value = true;
    await nextTick();
    usernameInput.value?.focus();
  } catch (error) {
    loginError.value = error;
  } finally {
    csrfLoading.value = false;
  }
}

async function initialize() {
  try {
    if (await auth.ensureSession()) {
      await router.replace(redirectTarget());
      return;
    }
  } catch {
    sessionCheckFailed.value = true;
  }

  await prepareLogin();
  initializing.value = false;
}

async function submit() {
  if (!submitLock.acquire()) return;
  submitting.value = true;
  try {
    loginError.value = null;
    const valid = await formRef.value?.validate().catch(() => false);
    if (!valid) return;

    noticeDismissed.value = true;
    const password = form.password;
    await auth.login(form.username.trim(), password);
    await router.replace(redirectTarget());
  } catch (error) {
    formRef.value?.clearValidate("password");
    loginError.value = error;
    if (error?.code === "CSRF_INVALID") {
      auth.clearSession({ checked: false });
      csrfReady.value = false;
      try {
        await auth.prepareLogin();
        csrfReady.value = true;
      } catch (refreshError) {
        loginError.value = refreshError;
      }
    }
  } finally {
    form.password = "";
    submitting.value = false;
    submitLock.release();
  }
}

onMounted(initialize);
</script>
