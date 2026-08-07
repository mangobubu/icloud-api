<template>
  <section class="one-time-secret" aria-labelledby="one-time-secret-title">
    <div class="one-time-secret__copy">
      <h2 id="one-time-secret-title">请立即保存 API Key</h2>
      <p>完整 Key 只显示这一次，关闭页面后不再提供查看。</p>
    </div>
    <div class="one-time-secret__value">
      <code>{{ value }}</code>
      <el-button :icon="CopyDocument" @click="copySecret">
        {{ copyLabel }}
      </el-button>
    </div>
    <span class="sr-only" aria-live="polite" aria-atomic="true">{{ announcement }}</span>
  </section>
</template>

<script setup>
import { CopyDocument } from "@element-plus/icons-vue";
import { onBeforeUnmount, ref } from "vue";

const props = defineProps({ value: { type: String, required: true } });

const copyLabel = ref("复制");
const announcement = ref("");
let resetTimer = null;

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

async function copySecret() {
  let copied = false;
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(props.value);
      copied = true;
    } else {
      copied = fallbackCopy(props.value);
    }
  } catch {
    copied = fallbackCopy(props.value);
  }

  const message = copied ? "已复制" : "复制失败，请手动复制";
  copyLabel.value = message;
  announcement.value = "";
  window.setTimeout(() => {
    announcement.value = message;
  }, 0);
  window.clearTimeout(resetTimer);
  resetTimer = window.setTimeout(() => {
    copyLabel.value = "复制";
  }, 1600);
}

onBeforeUnmount(() => window.clearTimeout(resetTimer));
</script>
