function normalizeBasePath(value) {
  const path = String(value || "").trim().replace(/\/+$/, "");
  return path.startsWith("/") && !path.startsWith("//") ? path : "";
}

export function detectAdminBasePath({
  baseURI = globalThis.document?.baseURI,
  pathname = globalThis.location?.pathname,
} = {}) {
  for (const candidate of [baseURI, pathname]) {
    let path = "";
    try {
      path = candidate?.includes("://")
        ? new URL(candidate).pathname
        : String(candidate || "");
    } catch {
      path = "";
    }
    const match = normalizeBasePath(path).match(/^(.*\/admin)(?:\/.*)?$/);
    if (match?.[1]) return match[1];
  }
  // Vite development keeps the historical proxy prefix. Production always
  // receives an installation-specific base element from the Go server.
  return "/admin";
}

export const ADMIN_BASE_PATH = detectAdminBasePath();
export const ADMIN_API_PREFIX = `${ADMIN_BASE_PATH}/api/v1`;
