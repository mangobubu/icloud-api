import {
  buildOTPDirectLink,
  buildRecentMailDirectLink,
} from "./clipboard.js";

export const ALIAS_EXPORT_OTP = "otp";
export const ALIAS_EXPORT_IMAP = "imap";

function field(value, label) {
  const normalized = typeof value === "string" ? value.trim() : "";
  if (!normalized || /[\r\n]/.test(normalized)) {
    throw new TypeError(`${label}格式无效。`);
  }
  return normalized;
}

export function buildAliasExportLine(
  alias,
  format = ALIAS_EXPORT_OTP,
  origin = globalThis.location?.origin,
) {
  const address = field(alias?.address, "邮箱地址");
  if (format === ALIAS_EXPORT_OTP) {
    const path = alias?.otpUrlPath || alias?.directLinkPath || alias?.legacyDirectLinkPath;
    const link = String(path || "").includes("/api/v1/otp")
      ? buildOTPDirectLink(path, origin)
      : buildRecentMailDirectLink(path, origin);
    return `${address}-----${link}`;
  }
  if (format === ALIAS_EXPORT_IMAP) {
    return `${address}----${field(alias?.imapPassword, "IMAP 密码")}----${field(
      alias?.clientId,
      "client ID",
    )}----${field(alias?.refreshToken, "刷新令牌")}`;
  }
  throw new TypeError("邮箱复制格式无效。");
}

export function buildAliasExportText(
  aliases,
  format = ALIAS_EXPORT_OTP,
  origin = globalThis.location?.origin,
) {
  if (!Array.isArray(aliases)) {
    throw new TypeError("邮箱导出数据格式无效。");
  }

  return aliases
    .map((alias) => buildAliasExportLine(alias, format, origin))
    .join("\r\n");
}
