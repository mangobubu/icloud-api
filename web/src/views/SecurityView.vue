<template>
  <section class="content-narrow page-stack" aria-labelledby="security-form-title">
    <SectionHeader
      id="security-form-title"
      title="修改管理员密码"
      description="修改后全部后台会话都会失效，需要重新登录。"
    />

    <el-form
      ref="formRef"
      class="form-panel security-form"
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

      <el-form-item label="当前密码" prop="currentPassword">
        <el-input
          v-model="form.currentPassword"
          type="password"
          autocomplete="current-password"
          show-password
        />
      </el-form-item>

      <el-form-item label="新密码" prop="newPassword">
        <el-input
          v-model="form.newPassword"
          type="password"
          autocomplete="new-password"
          show-password
        />
        <p class="field-help">长度为 12 到 72 字节，当前 {{ newPasswordBytes }} 字节。</p>
      </el-form-item>

      <el-form-item label="确认新密码" prop="confirmPassword">
        <el-input
          v-model="form.confirmPassword"
          type="password"
          autocomplete="new-password"
          show-password
        />
      </el-form-item>

      <div class="form-actions">
        <el-button
          native-type="submit"
          type="primary"
          :icon="Lock"
          :loading="submitting"
        >
          更新密码
        </el-button>
      </div>
    </el-form>
  </section>
</template>

<script setup>
import { Lock } from "@element-plus/icons-vue";
import { computed, nextTick, reactive, ref } from "vue";
import { useRouter } from "vue-router";

import { updatePassword } from "../api/admin.js";
import RequestAlert from "../components/RequestAlert.vue";
import SectionHeader from "../components/SectionHeader.vue";
import { useAuth } from "../stores/auth.js";
import { createActionLock } from "../utils/asyncState.js";
import { utf8Length } from "../utils/format.js";

const router = useRouter();
const auth = useAuth();
const formRef = ref(null);
const submitting = ref(false);
const submitError = ref(null);
const submitLock = createActionLock();
const form = reactive({
  currentPassword: "",
  newPassword: "",
  confirmPassword: "",
});

const newPasswordBytes = computed(() => utf8Length(form.newPassword));

function validateNewPassword(_, value, callback) {
  const length = utf8Length(value);
  if (length < 12 || length > 72) {
    callback(new Error("新密码长度需要在 12 到 72 字节之间"));
    return;
  }
  callback();
}

function validateConfirmation(_, value, callback) {
  if (!value) {
    callback(new Error("请再次填写新密码"));
    return;
  }
  if (value !== form.newPassword) {
    callback(new Error("两次填写的新密码不一致"));
    return;
  }
  callback();
}

const rules = {
  currentPassword: [
    { required: true, message: "请填写当前密码", trigger: "blur" },
  ],
  newPassword: [{ validator: validateNewPassword, trigger: ["blur", "change"] }],
  confirmPassword: [
    { validator: validateConfirmation, trigger: ["blur", "change"] },
  ],
};

function clearPasswords() {
  Object.assign(form, {
    currentPassword: "",
    newPassword: "",
    confirmPassword: "",
  });
}

async function submit() {
  if (!submitLock.acquire()) return;
  submitting.value = true;
  try {
    submitError.value = null;
    const valid = await formRef.value?.validate().catch(() => false);
    if (!valid) return;

    await updatePassword(
      {
        current_password: form.currentPassword,
        new_password: form.newPassword,
        confirm_password: form.confirmPassword,
      },
      auth.state.csrfToken,
    );
    clearPasswords();
    auth.clearSession({ checked: false });
    await router.replace({ name: "login", query: { notice: "password_changed" } });
  } catch (error) {
    clearPasswords();
    submitError.value = error;
    await nextTick();
    formRef.value?.clearValidate();
  } finally {
    submitting.value = false;
    submitLock.release();
  }
}
</script>
