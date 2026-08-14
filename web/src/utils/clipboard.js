const OTP_PATH = "/api/v1/otp";
const LEGACY_RECENT_MAIL_PATHS = new Set([
  "/api/v1/mail/recent",
  "/api/v1/mail/recent/",
]);

export function buildRecentMailDirectLink(relativePath, origin = globalThis.location?.origin) {
  if (
    typeof relativePath !== "string" ||
    relativePath !== relativePath.trim() ||
    !relativePath.startsWith("/") ||
    relativePath.startsWith("//")
  ) {
    throw new TypeError("邮件 API 直达链接格式无效。");
  }

  let base;
  let target;
  try {
    base = new URL(origin);
    target = new URL(relativePath, `${base.origin}/`);
  } catch {
    throw new TypeError("邮件 API 直达链接格式无效。");
  }

  if (
    !["http:", "https:"].includes(base.protocol) ||
    base.username ||
    base.password ||
    target.origin !== base.origin ||
    !LEGACY_RECENT_MAIL_PATHS.has(target.pathname) ||
    target.hash
  ) {
    throw new TypeError("邮件 API 直达链接格式无效。");
  }

  const queryKeys = [...target.searchParams.keys()];
  const apiKeys = target.searchParams.getAll("api_key");
  if (
    queryKeys.length !== 1 ||
    queryKeys[0] !== "api_key" ||
    apiKeys.length !== 1 ||
    !apiKeys[0]
  ) {
    throw new TypeError("邮件 API 直达链接格式无效。");
  }
  return target.href;
}

export function buildOTPDirectLink(relativePath, origin = globalThis.location?.origin) {
  if (
    typeof relativePath !== "string" ||
    relativePath !== relativePath.trim() ||
    !relativePath.startsWith("/") ||
    relativePath.startsWith("//")
  ) {
    throw new TypeError("邮件 API 直达链接格式无效。");
  }

  let base;
  let target;
  try {
    base = new URL(origin);
    target = new URL(relativePath, `${base.origin}/`);
  } catch {
    throw new TypeError("邮件 API 直达链接格式无效。");
  }

  if (
    !["http:", "https:"].includes(base.protocol) ||
    base.username ||
    base.password ||
    target.origin !== base.origin ||
    target.pathname !== OTP_PATH ||
    target.hash
  ) {
    throw new TypeError("邮件 API 直达链接格式无效。");
  }

  const queryKeys = [...target.searchParams.keys()];
  const tokens = target.searchParams.getAll("token");
  if (
    queryKeys.length !== 1 ||
    queryKeys[0] !== "token" ||
    tokens.length !== 1 ||
    !tokens[0]
  ) {
    throw new TypeError("邮件 API 直达链接格式无效。");
  }

  return target.href;
}

function fallbackCopy(text, documentRef) {
  if (
    !documentRef?.body ||
    typeof documentRef.createElement !== "function" ||
    typeof documentRef.execCommand !== "function"
  ) {
    return false;
  }

  let textarea;
  try {
    textarea = documentRef.createElement("textarea");
    textarea.value = text;
    textarea.readOnly = true;
    textarea.setAttribute("aria-hidden", "true");
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    documentRef.body.appendChild(textarea);
    textarea.select();
    return documentRef.execCommand("copy") === true;
  } catch {
    return false;
  } finally {
    if (textarea?.remove) {
      textarea.remove();
    } else if (textarea?.parentNode) {
      textarea.parentNode.removeChild(textarea);
    }
  }
}

export async function copyText(
  text,
  {
    clipboard = globalThis.navigator?.clipboard,
    document: documentRef = globalThis.document,
    isSecureContext = globalThis.isSecureContext,
  } = {},
) {
  const value = typeof text === "string" ? text : "";
  if (!value) return false;

  if (isSecureContext && typeof clipboard?.writeText === "function") {
    try {
      await clipboard.writeText(value);
      return true;
    } catch {
      // Some browsers expose Clipboard API but deny access at runtime.
    }
  }

  return fallbackCopy(value, documentRef);
}
