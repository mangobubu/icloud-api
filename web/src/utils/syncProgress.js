const SOURCE_LABELS = Object.freeze({
  manual: "手动同步",
  automatic: "自动同步",
});

const STAGE_LABELS = Object.freeze({
  queued: "等待开始",
  waiting: "等待同步资源",
  connecting: "正在连接邮箱",
  authenticating: "正在验证邮箱账户",
  scanning: "正在扫描邮件",
  fetching: "正在获取邮件",
  reading: "正在读取邮件",
  validating: "正在核对邮件状态",
  saving: "正在保存结果",
});

const INACTIVE_PRESENTATION = Object.freeze({
  active: false,
  label: "",
  stageLabel: "",
  percentage: null,
  indeterminate: false,
});

function normalizedToken(value) {
  return String(value ?? "")
    .trim()
    .toLowerCase()
    .replaceAll("-", "_");
}

function normalizedSource(value) {
  const source = normalizedToken(value);
  if (["automatic", "auto", "scheduled", "background"].includes(source)) {
    return "automatic";
  }
  if (source === "manual") {
    return "manual";
  }
  return "";
}

export function syncProgressSourceLabel(value) {
  return SOURCE_LABELS[normalizedSource(value)] || "邮件同步";
}

export function syncProgressStageLabel(value) {
  return STAGE_LABELS[normalizedToken(value)] || "同步处理中";
}

export function normalizeSyncPercentage(value) {
  if (
    value === null ||
    value === undefined ||
    value === "" ||
    typeof value === "boolean"
  ) {
    return null;
  }
  const percentage = Number(value);
  if (!Number.isFinite(percentage)) {
    return null;
  }
  return Math.min(100, Math.max(0, percentage));
}

export function syncProgressPresentation(progress) {
  if (!progress || progress.active !== true) {
    return INACTIVE_PRESENTATION;
  }

  const source = normalizedSource(progress.source);
  const stage = normalizedToken(progress.stage);
  const stageLabel = syncProgressStageLabel(stage);
  const percentage = normalizeSyncPercentage(progress.percentage);

  return {
    active: true,
    label: syncProgressSourceLabel(source),
    stageLabel,
    percentage,
    indeterminate: percentage === null,
  };
}
