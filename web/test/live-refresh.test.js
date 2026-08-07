import assert from "node:assert/strict";
import test from "node:test";

import {
  createLiveRefresh,
  LIVE_REFRESH_INTERVAL_MS,
} from "../src/utils/liveRefresh.js";

class FakeEventTarget {
  constructor() {
    this.listeners = new Map();
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || new Set();
    listeners.add(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type, listener) {
    this.listeners.get(type)?.delete(listener);
  }

  dispatch(type) {
    for (const listener of this.listeners.get(type) || []) {
      listener({ type, target: this });
    }
  }

  listenerCount(type) {
    return this.listeners.get(type)?.size || 0;
  }
}

function createFakeTimers() {
  let nextId = 1;
  const timers = new Map();

  return {
    setTimeoutFn(callback, delay) {
      const id = nextId;
      nextId += 1;
      timers.set(id, { callback, delay });
      return id;
    },
    clearTimeoutFn(id) {
      timers.delete(id);
    },
    fireNext() {
      const entry = timers.entries().next().value;
      assert.ok(entry, "expected a scheduled timer");
      const [id, timer] = entry;
      timers.delete(id);
      timer.callback();
      return timer.delay;
    },
    entries() {
      return [...timers.values()];
    },
    get size() {
      return timers.size;
    },
  };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function flushPromises() {
  return new Promise((resolve) => setImmediate(resolve));
}

function createHarness(refresh, overrides = {}) {
  const timers = createFakeTimers();
  const documentTarget = Object.assign(new FakeEventTarget(), {
    hidden: false,
  });
  const windowTarget = new FakeEventTarget();
  const live = createLiveRefresh(refresh, {
    documentTarget,
    windowTarget,
    setTimeoutFn: timers.setTimeoutFn,
    clearTimeoutFn: timers.clearTimeoutFn,
    ...overrides,
  });
  return { documentTarget, live, timers, windowTarget };
}

test("live refresh defaults to five seconds and schedules after completion", async () => {
  assert.equal(LIVE_REFRESH_INTERVAL_MS, 5_000);

  const pending = deferred();
  let calls = 0;
  const { live, timers } = createHarness(() => {
    calls += 1;
    return pending.promise;
  });

  const first = live.start();
  await Promise.resolve();
  assert.equal(calls, 1);
  assert.equal(timers.size, 0);

  pending.resolve();
  assert.equal(await first, true);
  assert.deepEqual(timers.entries().map(({ delay }) => delay), [5_000]);

  live.stop();
});

test("refreshes are self-scheduled and never overlap", async () => {
  const requests = [];
  let activeRequests = 0;
  let maximumActiveRequests = 0;
  const { live, timers } = createHarness(() => {
    const request = deferred();
    requests.push(request);
    activeRequests += 1;
    maximumActiveRequests = Math.max(maximumActiveRequests, activeRequests);
    return request.promise.finally(() => {
      activeRequests -= 1;
    });
  });

  await live.start({ immediate: false });
  assert.equal(timers.size, 1);
  timers.fireNext();
  await Promise.resolve();
  assert.equal(requests.length, 1);
  assert.equal(timers.size, 0);

  const sameRequest = live.refreshNow();
  assert.equal(requests.length, 1);
  requests[0].resolve();
  assert.equal(await sameRequest, true);
  assert.equal(maximumActiveRequests, 1);
  assert.equal(timers.size, 1);

  live.stop();
});

test("hidden documents pause refresh and becoming visible refreshes immediately", async () => {
  let calls = 0;
  const { documentTarget, live, timers, windowTarget } = createHarness(() => {
    calls += 1;
  });

  await live.start({ immediate: false });
  assert.equal(timers.size, 1);

  documentTarget.hidden = true;
  documentTarget.dispatch("visibilitychange");
  assert.equal(timers.size, 0);
  assert.equal(await live.refreshNow(), false);
  assert.equal(calls, 0);

  windowTarget.dispatch("focus");
  await Promise.resolve();
  assert.equal(calls, 0);

  documentTarget.hidden = false;
  documentTarget.dispatch("visibilitychange");
  await flushPromises();
  assert.equal(calls, 1);
  assert.equal(timers.size, 1);

  live.stop();
});

test("window focus refreshes immediately and replaces the pending timer", async () => {
  let calls = 0;
  const { live, timers, windowTarget } = createHarness(() => {
    calls += 1;
  });

  await live.start({ immediate: false });
  assert.equal(timers.size, 1);

  windowTarget.dispatch("focus");
  await flushPromises();
  assert.equal(calls, 1);
  assert.equal(timers.size, 1);

  live.stop();
});

test("failures are reported and do not stop later refreshes", async () => {
  const errors = [];
  let calls = 0;
  const { live, timers } = createHarness(
    () => {
      calls += 1;
      if (calls === 1) throw new Error("refresh failed");
    },
    { onError: (error) => errors.push(error.message) },
  );

  assert.equal(await live.start(), false);
  assert.deepEqual(errors, ["refresh failed"]);
  assert.equal(timers.size, 1);

  timers.fireNext();
  await flushPromises();
  assert.equal(calls, 2);
  assert.equal(timers.size, 1);

  live.stop();
});

test("stop removes listeners and pending work cannot restart the scheduler", async () => {
  const pending = deferred();
  const { documentTarget, live, timers, windowTarget } = createHarness(
    () => pending.promise,
  );

  const refresh = live.start();
  await Promise.resolve();
  assert.equal(documentTarget.listenerCount("visibilitychange"), 1);
  assert.equal(windowTarget.listenerCount("focus"), 1);

  live.stop();
  assert.equal(live.isRunning(), false);
  assert.equal(documentTarget.listenerCount("visibilitychange"), 0);
  assert.equal(windowTarget.listenerCount("focus"), 0);
  assert.equal(timers.size, 0);

  pending.resolve();
  assert.equal(await refresh, true);
  assert.equal(timers.size, 0);

  documentTarget.dispatch("visibilitychange");
  windowTarget.dispatch("focus");
  await Promise.resolve();
  assert.equal(timers.size, 0);
});
