<template>
  <main class="not-found-page">
    <AppBrand title="iCloud 隐私邮箱" subtitle="后台管理" />
    <section class="not-found-panel" aria-labelledby="not-found-title">
      <el-icon :size="34" aria-hidden="true"><Warning /></el-icon>
      <h1 id="not-found-title">页面不存在</h1>
      <p>当前地址没有对应的后台页面。</p>
      <el-button type="primary" :icon="Back" @click="returnHome">
        返回后台首页
      </el-button>
    </section>
  </main>
</template>

<script setup>
import { Back, Warning } from "@element-plus/icons-vue";
import { useRouter } from "vue-router";

import AppBrand from "../components/AppBrand.vue";

const router = useRouter();

function returnHome() {
  router.replace({ name: "accounts" });
}
</script>
