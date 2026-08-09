import assert from "node:assert/strict";
import test from "node:test";

import {
  normalizeSyncPercentage,
  syncProgressPresentation,
  syncProgressSourceLabel,
  syncProgressStageLabel,
} from "../src/utils/syncProgress.js";

test("inactive or absent sync progress is not presented", () => {
  const inactive = {
    active: false,
    label: "",
    stageLabel: "",
    percentage: null,
    indeterminate: false,
  };
  assert.deepEqual(syncProgressPresentation(null), inactive);
  assert.deepEqual(syncProgressPresentation(undefined), inactive);
  assert.deepEqual(syncProgressPresentation({}), inactive);
  assert.deepEqual(
    syncProgressPresentation({
      active: false,
      source: "manual",
      stage: "fetching",
      percentage: 80,
    }),
    inactive,
  );
});

test("manual and automatic progress use distinct source labels", () => {
  assert.equal(syncProgressSourceLabel("manual"), "手动同步");
  assert.equal(syncProgressSourceLabel("automatic"), "自动同步");
  assert.equal(syncProgressSourceLabel("auto"), "自动同步");
  assert.equal(syncProgressSourceLabel("unknown"), "邮件同步");

  const manual = syncProgressPresentation({
    active: true,
    source: "manual",
    stage: "queued",
    percentage: 0,
    startedAt: "2026-08-09T10:00:00Z",
    updatedAt: "2026-08-09T10:00:01Z",
  });
  assert.deepEqual(manual, {
    active: true,
    label: "手动同步",
    stageLabel: "等待开始",
    percentage: 0,
    indeterminate: false,
  });

  const automatic = syncProgressPresentation({
    active: true,
    source: "automatic",
    stage: "waiting",
    percentage: null,
  });
  assert.equal(automatic.label, "自动同步");
  assert.equal(automatic.stageLabel, "等待同步资源");
  assert.equal(automatic.percentage, null);
  assert.equal(automatic.indeterminate, true);
});

test("all known sync stages have stable Chinese labels", () => {
  const labels = {
    queued: "等待开始",
    waiting: "等待同步资源",
    connecting: "正在连接邮箱",
    authenticating: "正在验证邮箱账户",
    scanning: "正在扫描邮件",
    fetching: "正在获取邮件",
    reading: "正在读取邮件",
    validating: "正在核对邮件状态",
    saving: "正在保存结果",
  };

  for (const [stage, label] of Object.entries(labels)) {
    assert.equal(syncProgressStageLabel(stage), label, stage);
  }
});

test("unknown sync stages fall back to a safe presentation", () => {
  assert.equal(syncProgressStageLabel("future-stage"), "同步处理中");
  assert.equal(syncProgressStageLabel(null), "同步处理中");

  const progress = syncProgressPresentation({
    active: true,
    source: "future-source",
    stage: "future-stage",
    percentage: 25,
  });
  assert.equal(progress.label, "邮件同步");
  assert.equal(progress.stageLabel, "同步处理中");
});

test("sync percentages preserve zero and sanitize invalid values", () => {
  assert.equal(normalizeSyncPercentage(0), 0);
  assert.equal(normalizeSyncPercentage("0"), 0);
  assert.equal(normalizeSyncPercentage(42.5), 42.5);
  assert.equal(normalizeSyncPercentage(-10), 0);
  assert.equal(normalizeSyncPercentage(120), 100);

  for (const value of [null, undefined, "", true, false, NaN, Infinity, "bad"]) {
    const percentage = normalizeSyncPercentage(value);
    assert.equal(percentage, null, String(value));
    assert.equal(Number.isNaN(percentage), false, String(value));

    const presentation = syncProgressPresentation({
      active: true,
      source: "manual",
      stage: "fetching",
      percentage: value,
    });
    assert.equal(presentation.percentage, null, String(value));
    assert.equal(presentation.indeterminate, true, String(value));
    assert.equal(Number.isNaN(presentation.percentage), false, String(value));
  }
});
