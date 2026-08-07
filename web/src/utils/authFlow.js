export const AUTH_REQUIRED = "AUTH_REQUIRED";
export const SESSION_EXPIRED = "SESSION_EXPIRED";

export function buildLoginRedirect(errorCode, redirect) {
  const query = { redirect };
  if (errorCode === SESSION_EXPIRED) {
    query.notice = "session_expired";
  } else if (errorCode && errorCode !== AUTH_REQUIRED) {
    query.notice = "session_error";
  }
  return { name: "login", query };
}

export function loginNoticeMessage({
  notice = "",
  sessionCheckFailed = false,
  dismissed = false,
} = {}) {
  if (dismissed) return "";
  if (notice === "session_expired") {
    return "登录会话已过期，请重新登录。";
  }
  if (notice === "password_changed") {
    return "管理员密码已更新，请使用新密码重新登录。";
  }
  if (notice === "session_error" || sessionCheckFailed) {
    return "未能确认现有会话，请重新登录。";
  }
  return "";
}
