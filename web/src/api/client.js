const API_PREFIX = "/admin/api/v1";

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
  return text ? { error: { message: text } } : null;
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
    response = await fetch(`${API_PREFIX}${path}`, {
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
    const error = new ApiError({
      status: response.status,
      code: envelope.code,
      message: envelope.message,
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
