import assert from "node:assert/strict";
import test from "node:test";

import {
  createActionLock,
  createLatestRequestGate,
} from "../src/utils/asyncState.js";

test("latest request gate rejects stale route responses", () => {
  const gate = createLatestRequestGate();
  const accountA = gate.begin("account-a");
  const accountB = gate.begin("account-b");

  assert.equal(gate.isCurrent(accountA, "account-b"), false);
  assert.equal(gate.isCurrent(accountB, "account-b"), true);
  assert.equal(gate.isCurrent(accountB, "account-a"), false);
});

test("latest request gate rejects responses after deactivation", () => {
  const gate = createLatestRequestGate();
  const request = gate.begin("account-a");

  gate.deactivate();

  assert.equal(gate.isCurrent(request, "account-a"), false);
});

test("latest request gate rejects a refresh invalidated by a later mutation", () => {
  const gate = createLatestRequestGate();
  const refresh = gate.begin("account-a");

  gate.invalidate();

  assert.equal(gate.isCurrent(refresh, "account-a"), false);
  const afterMutation = gate.begin("account-a");
  assert.equal(gate.isCurrent(afterMutation, "account-a"), true);
});

test("action lock rejects duplicate work until the first action releases", async () => {
  const lock = createActionLock();
  let releasePending;
  const pending = new Promise((resolve) => {
    releasePending = resolve;
  });

  async function run() {
    if (!lock.acquire("submit")) return false;
    try {
      await pending;
      return true;
    } finally {
      lock.release("submit");
    }
  }

  const first = run();
  assert.equal(await run(), false);
  assert.equal(lock.isLocked("submit"), true);

  releasePending();
  assert.equal(await first, true);
  assert.equal(lock.isLocked("submit"), false);
});

test("keyed action locks isolate aliases while blocking duplicate alias actions", () => {
  const lock = createActionLock();

  assert.equal(lock.acquire(10), true);
  assert.equal(lock.acquire(10), false);
  assert.equal(lock.acquire(11), true);
  assert.equal(lock.hasAny(), true);

  lock.release(10);
  lock.release(11);
  assert.equal(lock.hasAny(), false);
});
