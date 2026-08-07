import { ElMessage, ElNotification } from "element-plus";

export function successMessage(message) {
  ElMessage({ type: "success", message, duration: 2400 });
}

export function showRequestError(error, fallback = "请求处理失败，请稍后重试。") {
  const requestId = error?.requestId || "";
  ElNotification({
    type: "error",
    title: error?.message || fallback,
    message: requestId ? `请求编号：${requestId}` : "",
    duration: 5000,
  });
}

export function confirmationCancelled(error) {
  return error === "cancel" || error === "close";
}
