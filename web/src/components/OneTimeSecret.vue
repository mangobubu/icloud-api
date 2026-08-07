<template>
  <section class="one-time-secret" aria-labelledby="one-time-secret-title">
    <div class="one-time-secret__copy">
      <h2 id="one-time-secret-title">请立即保存 API Key 与直达链接</h2>
      <p>完整凭据只显示这一次，关闭页面后不再提供查看。</p>
    </div>
    <div class="one-time-secret__values">
      <div class="one-time-secret__item">
        <span class="one-time-secret__label">API Key</span>
        <div class="one-time-secret__value">
          <code>{{ value }}</code>
          <el-button
            :icon="CopyDocument"
            aria-label="复制 API Key"
            @click="copyValue('key', value)"
          >
            {{ copyLabels.key }}
          </el-button>
        </div>
      </div>
      <div class="one-time-secret__item">
        <span class="one-time-secret__label">邮件 API 直达链接</span>
        <div class="one-time-secret__value one-time-secret__value--link">
          <code>{{ directLink }}</code>
          <el-button
            :icon="CopyDocument"
            aria-label="复制邮件 API 直达链接"
            @click="copyValue('link', directLink)"
          >
            {{ copyLabels.link }}
          </el-button>
        </div>
      </div>
    </div>
    <span class="sr-only" aria-live="polite" aria-atomic="true">{{ announcement }}</span>
  </section>
</template>

<script setup>
import { CopyDocument } from "@element-plus/icons-vue";
import { computed, onBeforeUnmount, reactive, ref } from "vue";

const props = defineProps({ value: { type: String, required: true } });

const directLink = computed(
  () =>
    `${window.location.origin}/api/v1/mail/recent?api_key=${encodeURIComponent(props.value)}`,
);
const copyLabels = reactive({ key: "复制 Key", link: "复制链接" });
const announcement = ref("");
const resetTimers = { key: null, link: null };

function fallbackCopy(text) {
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.readOnly = true;
  textarea.setAttribute("aria-hidden", "true");
  textarea.style.position = "fixed";
  textarea.style.left = "-9999px";
  document.body.appendChild(textarea);
  textarea.select();
  try {
    try {
      return document.execCommand("copy");
    } catch {
      return false;
    }
  } finally {
    textarea.remove();
  }
}

async function copyValue(kind, value) {
  let copied = false;
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(value);
      copied = true;
    } else {
      copied = fallbackCopy(value);
    }
  } catch {
    copied = fallbackCopy(value);
  }

  const target = kind === "key" ? "API Key" : "直达链接";
  const idleLabel = kind === "key" ? "复制 Key" : "复制链接";
  const message = copied ? `${target} 已复制` : `${target} 复制失败，请手动复制`;
  copyLabels[kind] = copied ? "已复制" : "复制失败";
  announcement.value = "";
  window.setTimeout(() => {
    announcement.value = message;
  }, 0);
  window.clearTimeout(resetTimers[kind]);
  resetTimers[kind] = window.setTimeout(() => {
    copyLabels[kind] = idleLabel;
  }, 1600);
}

onBeforeUnmount(() => {
  window.clearTimeout(resetTimers.key);
  window.clearTimeout(resetTimers.link);
});
</script>
