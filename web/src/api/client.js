import { ADMIN_API_PREFIX } from "../utils/runtimePath.js";

let unauthorizedHandler = null;

export class ApiError extends Error {
  constructor({ status, code, message, requestId, fields }) {
    super(message || "请求处理失败，请稍后重试。");
    this.name = "ApiError";
    this.status = status;
    this.code = code || "REQUEST_FAILED";
    this.requestId = requestId || "";
    this.fields = fields || null;
  }
}

export function setUnauthorizedHandler(handler) {
  unauthorizedHandler = typeof handler === "function" ? handler : null;
}

async function parsePayload(response) {
  if (response.status === 204) {
    return null;
  }

  const contentType = response.headers.get("content-type") || "";
  if (contentType.includes("application/json")) {
    return response.json();
  }

  const text = await response.text();
  if (
    contentType.includes("text/html") ||
    /^\s*<(?:!doctype\s+html|html)(?:\s|>)/i.test(text)
  ) {
    return null;
  }
  return text ? { error: { message: text } } : null;
}

function fallbackHTTPError(status) {
  if (status === 504) {
    return {
      code: "GATEWAY_TIMEOUT",
      message: "网关等待服务响应超时，请稍后重试。",
    };
  }
  if (status >= 500) {
    return {
      code: "SERVICE_UNAVAILABLE",
      message: "服务暂时不可用，请稍后重试。",
    };
  }
  return {};
}

export async function apiRequest(path, options = {}) {
  const {
    method = "GET",
    body,
    csrfToken = "",
    signal,
    handleUnauthorized = true,
  } = options;
  const headers = new Headers({ Accept: "application/json" });

  if (body !== undefined) {
    headers.set("Content-Type", "application/json");
  }
  if (csrfToken) {
    headers.set("X-CSRF-Token", csrfToken);
  }

  let response;
  try {
    response = await fetch(`${ADMIN_API_PREFIX}${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: "same-origin",
      signal,
    });
  } catch (error) {
    if (error?.name === "AbortError") {
      throw error;
    }
    throw new ApiError({
      status: 0,
      code: "NETWORK_ERROR",
      message: "无法连接到服务，请检查网络后重试。",
    });
  }

  let payload = null;
  try {
    payload = await parsePayload(response);
  } catch {
    throw new ApiError({
      status: response.status,
      code: "INVALID_RESPONSE",
      message: "服务返回了无法识别的响应。",
      requestId: response.headers.get("X-Request-ID") || "",
    });
  }

  if (!response.ok) {
    const envelope = payload?.error || {};
    const fallback = fallbackHTTPError(response.status);
    const error = new ApiError({
      status: response.status,
      code: envelope.code || fallback.code,
      message: envelope.message || fallback.message,
      requestId:
        envelope.request_id || response.headers.get("X-Request-ID") || "",
      fields: envelope.fields,
    });
    const sessionInvalid =
      response.status === 401 &&
      (error.code === "AUTH_REQUIRED" || error.code === "SESSION_EXPIRED");
    if (sessionInvalid && handleUnauthorized && unauthorizedHandler) {
      unauthorizedHandler(error);
    }
    throw error;
  }

  return payload?.data ?? null;
}
