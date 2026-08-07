import { computed, reactive } from "vue";

import {
  getLoginCsrf,
  getSession,
  login as loginRequest,
  logout as logoutRequest,
} from "../api/admin.js";

const state = reactive({
  username: "",
  csrfToken: "",
  sessionChecked: false,
});

let sessionPromise = null;

function applySession(session) {
  state.username = session?.username || "";
  state.csrfToken = session?.csrfToken || "";
  state.sessionChecked = true;
}

function clearSession({ checked = true } = {}) {
  state.username = "";
  state.csrfToken = "";
  state.sessionChecked = checked;
}

async function ensureSession({ force = false } = {}) {
  if (!force && state.sessionChecked) {
    return Boolean(state.username && state.csrfToken);
  }
  if (sessionPromise) {
    return sessionPromise;
  }

  sessionPromise = getSession()
    .then((session) => {
      applySession(session);
      return Boolean(state.username && state.csrfToken);
    })
    .catch((error) => {
      clearSession();
      if (error?.status === 401) {
        return false;
      }
      throw error;
    })
    .finally(() => {
      sessionPromise = null;
    });
  return sessionPromise;
}

async function prepareLogin() {
  state.csrfToken = await getLoginCsrf();
  return state.csrfToken;
}

async function login(username, password) {
  if (!state.csrfToken) {
    await prepareLogin();
  }
  const session = await loginRequest(username, password, state.csrfToken);
  applySession(session);
  return session;
}

async function logout() {
  await logoutRequest(state.csrfToken);
  clearSession({ checked: false });
}

export function useAuth() {
  return {
    state,
    isAuthenticated: computed(() => Boolean(state.username && state.csrfToken)),
    ensureSession,
    prepareLogin,
    login,
    logout,
    clearSession,
  };
}
