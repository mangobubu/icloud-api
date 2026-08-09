import assert from "node:assert/strict";
import test from "node:test";

import {
  appendRuntimeLogPage,
  buildRuntimeLogQuery,
  mergeRuntimeLogs,
  normalizeRuntimeLog,
  normalizeRuntimeLogPage,
  runtimeLogAttributesText,
  runtimeLogLevelMeta,
} from "../src/utils/runtimeLogs.js";

test("runtime logs normalize timestamps, levels, attributes, and promoted context", () => {
  assert.deepEqual(
    normalizeRuntimeLog({
      id: 42,
      created_at: "2026-08-09T08:00:00Z",
      level: "WARNING",
      message: "主号同步失败",
      source: "syncer.manager",
      account_id: 12,
      request_id: "req-123",
      attributes: { attempt: 2, error: "connection closed" },
    }),
    {
      id: 42,
      time: "2026-08-09T08:00:00Z",
      level: "warn",
      message: "主号同步失败",
      source: "syncer.manager",
      accountId: 12,
      requestId: "req-123",
      attributes: { attempt: 2, error: "connection closed" },
    },
  );

  const fieldsLog = normalizeRuntimeLog({
    ID: 43,
    Time: "2026-08-09T08:01:00Z",
    Level: "fatal",
    Message: "request failed",
    Fields: { account_id: 13, request_id: "req-456" },
  });
  assert.equal(fieldsLog.level, "error");
  assert.equal(fieldsLog.accountId, 13);
  assert.equal(fieldsLog.requestId, "req-456");
});

test("runtime log pages normalize the cursor response", () => {
  assert.deepEqual(
    normalizeRuntimeLogPage({
      items: [{ id: 9, timestamp: "2026-08-09T08:00:00Z", level: "INFO" }],
      has_more: true,
      next_before_id: 9,
    }),
    {
      items: [
        {
          id: 9,
          time: "2026-08-09T08:00:00Z",
          level: "info",
          message: "",
          source: "system",
          accountId: null,
          requestId: "",
          attributes: {},
        },
      ],
      hasMore: true,
      nextBeforeId: 9,
    },
  );
});

test("runtime log queries trim filters, encode values, and clamp limits", () => {
  const query = buildRuntimeLogQuery({
    level: "ERROR",
    query: " connection closed ",
    accountId: 12,
    limit: 500,
    beforeId: 91,
  });
  const parameters = new URLSearchParams(query);

  assert.deepEqual(Object.fromEntries(parameters), {
    level: "error",
    query: "connection closed",
    account_id: "12",
    limit: "200",
    before_id: "91",
  });
  assert.equal(buildRuntimeLogQuery({ limit: "invalid" }), "limit=50");
});

test("runtime log pages merge in order without duplicate IDs", () => {
  const latest = [{ id: 4 }, { id: 3 }];
  const existing = [{ id: 3, stale: true }, { id: 2 }, { id: 1 }];
  assert.deepEqual(
    mergeRuntimeLogs(latest, existing).map((item) => item.id),
    [4, 3, 2, 1],
  );
  assert.equal(mergeRuntimeLogs(latest, existing)[1].stale, undefined);
});

test("loaded runtime log history is capped to the in-memory server window", () => {
  const current = Array.from({ length: 1990 }, (_, index) => ({
    id: 2100 - index,
  }));
  const page = {
    items: Array.from({ length: 50 }, (_, index) => ({ id: 110 - index })),
    hasMore: true,
    nextBeforeId: 61,
  };

  const result = appendRuntimeLogPage(current, page, 2000);

  assert.equal(result.items.length, 2000);
  assert.equal(result.items[0].id, 2100);
  assert.equal(result.items.at(-1).id, 101);
  assert.equal(result.hasMore, false);
  assert.equal(result.nextBeforeId, null);
});

test("runtime log display metadata and attributes remain predictable", () => {
  assert.deepEqual(runtimeLogLevelMeta("ERROR"), {
    label: "错误",
    type: "danger",
  });
  assert.equal(runtimeLogLevelMeta("custom").label, "CUSTOM");
  assert.equal(runtimeLogAttributesText(null), "");
  assert.equal(
    runtimeLogAttributesText({ error: "connection closed" }),
    '{\n  "error": "connection closed"\n}',
  );
});
