import { buildRecentMailDirectLink } from "./clipboard.js";

export function buildAliasExportText(
  aliases,
  origin = globalThis.location?.origin,
) {
  if (!Array.isArray(aliases)) {
    throw new TypeError("邮箱导出数据格式无效。");
  }

  return aliases
    .map((alias) => {
      const address =
        typeof alias?.address === "string" ? alias.address.trim() : "";
      if (!address || /[\r\n]/.test(address)) {
        throw new TypeError("邮箱地址格式无效。");
      }

      return `${address}----${buildRecentMailDirectLink(
        alias.directLinkPath,
        origin,
      )}`;
    })
    .join("\r\n");
}
