import { createRouter, createWebHistory } from "vue-router";

import { setUnauthorizedHandler } from "../api/client.js";
import AdminLayout from "../layouts/AdminLayout.vue";
import { useAuth } from "../stores/auth.js";
import { setPageHeader } from "../stores/page.js";
import { buildLoginRedirect } from "../utils/authFlow.js";
import { ADMIN_BASE_PATH } from "../utils/runtimePath.js";

const routes = [
  {
    path: "/login",
    name: "login",
    component: () => import("../views/LoginView.vue"),
    meta: { title: "登录", public: true },
  },
  {
    path: "/",
    component: AdminLayout,
    meta: { requiresAuth: true },
    children: [
      {
        path: "",
        name: "accounts",
        component: () => import("../views/AccountsView.vue"),
        meta: {
          title: "主号管理",
          subtitle: "配置接收隐私邮箱转发邮件的 iCloud IMAP 主号",
        },
      },
      {
        path: "accounts/new",
        name: "account-new",
        component: () => import("../views/AccountFormView.vue"),
        meta: { title: "添加主号" },
      },
      {
        path: "accounts/:id/edit",
        name: "account-edit",
        component: () => import("../views/AccountFormView.vue"),
        meta: { title: "编辑主号" },
      },
      {
        path: "accounts/:id",
        name: "account-detail",
        component: () => import("../views/AccountDetailView.vue"),
        meta: {
          title: "主号详情",
          subtitle: "管理 IMAP 连接、同步状态和所属隐私邮箱",
          dynamicTitle: true,
        },
      },
      {
        path: "aliases",
        name: "aliases",
        component: () => import("../views/AliasesView.vue"),
        meta: {
          title: "隐私邮箱",
          subtitle: "查看每个地址的主号归属、完整凭证和最新收件时间",
        },
      },
      {
        path: "audit",
        name: "audit",
        component: () => import("../views/AuditView.vue"),
        meta: { title: "操作记录", subtitle: "追踪后台登录与配置变更" },
      },
      {
        path: "logs",
        name: "logs",
        component: () => import("../views/LogsView.vue"),
        meta: {
          title: "全部日志",
          subtitle: "查看服务运行、同步与后台请求日志",
        },
      },
      {
        path: "security",
        name: "security",
        component: () => import("../views/SecurityView.vue"),
        meta: { title: "安全设置", subtitle: "管理当前管理员的登录凭据" },
      },
    ],
  },
  {
    path: "/:pathMatch(.*)*",
    name: "admin-not-found",
    component: () => import("../views/NotFoundView.vue"),
    meta: { title: "页面不存在", public: true },
  },
];

const router = createRouter({
  history: createWebHistory(`${ADMIN_BASE_PATH}/`),
  routes,
  scrollBehavior() {
    return { top: 0 };
  },
});

const auth = useAuth();

router.beforeEach(async (to) => {
  setPageHeader(to.meta.title || "", to.meta.subtitle || "");
  if (!to.matched.some((record) => record.meta.requiresAuth)) {
    if (to.name === "login" && auth.isAuthenticated.value) {
      return { name: "accounts", replace: true };
    }
    return true;
  }

  try {
    if (await auth.ensureSession()) {
      return true;
    }
  } catch (error) {
    return buildLoginRedirect(error?.code, to.fullPath);
  }
  return buildLoginRedirect(auth.state.lastSessionErrorCode, to.fullPath);
});

router.afterEach((to) => {
  if (!to.meta.dynamicTitle) {
    setPageHeader(to.meta.title || "", to.meta.subtitle || "");
  }
});

setUnauthorizedHandler((error) => {
  const current = router.currentRoute.value;
  auth.clearSession({ errorCode: error?.code || "" });
  if (current.name !== "login") {
    router.replace(buildLoginRedirect(error?.code, current.fullPath));
  }
});

export default router;
