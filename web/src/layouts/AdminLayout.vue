<template>
  <div class="admin-shell">
    <aside class="admin-sidebar" aria-label="后台导航">
      <AppBrand />
      <AdminNavigation />
      <SidebarFooter />
    </aside>

    <el-drawer
      v-model="drawerOpen"
      class="mobile-drawer"
      direction="ltr"
      size="min(84vw, 280px)"
      :with-header="false"
      title="后台导航"
      append-to-body
    >
      <div class="mobile-drawer__inner" aria-label="后台导航">
        <AppBrand />
        <AdminNavigation @navigate="drawerOpen = false" />
        <SidebarFooter />
      </div>
    </el-drawer>

    <div class="admin-main">
      <header class="admin-topbar">
        <el-button
          class="admin-menu-button"
          :icon="Menu"
          aria-label="打开导航"
          :aria-expanded="drawerOpen"
          circle
          @click="drawerOpen = true"
        />
        <div class="admin-topbar__copy">
          <h1>{{ page.title }}</h1>
          <p v-if="page.subtitle">{{ page.subtitle }}</p>
        </div>
      </header>
      <main class="admin-content">
        <router-view />
      </main>
    </div>
  </div>
</template>

<script setup>
import {
  Connection,
  Document,
  Lock,
  Menu,
  Message,
  SwitchButton,
} from "@element-plus/icons-vue";
import { ElMessage } from "element-plus";
import { computed, defineComponent, h, ref, watch } from "vue";
import { RouterLink, useRoute, useRouter } from "vue-router";

import AppBrand from "../components/AppBrand.vue";
import { useAuth } from "../stores/auth.js";
import { usePageHeader } from "../stores/page.js";
import { getActiveAdminSection } from "../utils/adminNavigation.js";
import { showRequestError } from "../utils/feedback.js";

const route = useRoute();
const router = useRouter();
const auth = useAuth();
const page = usePageHeader();
const drawerOpen = ref(false);
const logoutLoading = ref(false);

const menuItems = [
  { to: "/admin/", label: "主号管理", icon: Connection, section: "accounts" },
  { to: "/admin/aliases", label: "隐私邮箱", icon: Message, section: "aliases" },
  { to: "/admin/audit", label: "操作记录", icon: Document, section: "audit" },
  { to: "/admin/security", label: "安全设置", icon: Lock, section: "security" },
];

const activeSection = computed(() => getActiveAdminSection(route.path));

const AdminNavigation = defineComponent({
  emits: ["navigate"],
  setup(_, { emit }) {
    return () =>
      h(
        "nav",
        { class: "admin-nav", "aria-label": "主要导航" },
        menuItems.map((item) =>
          h(
            RouterLink,
            {
              to: item.to,
              class: ["admin-nav__item", { "is-active": activeSection.value === item.section }],
              "aria-current": activeSection.value === item.section ? "page" : undefined,
              onClick: () => emit("navigate"),
            },
            {
              default: () => [h(item.icon, { "aria-hidden": "true" }), h("span", item.label)],
            },
          ),
        ),
      );
  },
});

const SidebarFooter = defineComponent({
  setup() {
    const performLogout = async () => {
      if (logoutLoading.value) return;
      logoutLoading.value = true;
      try {
        await auth.logout();
        ElMessage.closeAll();
        await router.replace({ name: "login" });
      } catch (error) {
        showRequestError(error, "退出失败，请稍后重试。");
      } finally {
        logoutLoading.value = false;
      }
    };
    return () =>
      h("div", { class: "admin-sidebar__footer" }, [
        h("span", { class: "admin-sidebar__username", title: auth.state.username }, auth.state.username),
        h(
          "button",
          {
            type: "button",
            class: "quiet-action",
            disabled: logoutLoading.value,
            onClick: performLogout,
          },
          [h(SwitchButton, { "aria-hidden": "true" }), h("span", logoutLoading.value ? "退出中" : "退出")],
        ),
      ]);
  },
});

watch(
  () => route.fullPath,
  () => {
    drawerOpen.value = false;
  },
);
</script>
