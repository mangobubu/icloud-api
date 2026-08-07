export const LIVE_REFRESH_INTERVAL_MS = 5_000;

function globalTarget(name) {
  return typeof globalThis[name] === "undefined" ? null : globalThis[name];
}

export function createLiveRefresh(refresh, options = {}) {
  if (typeof refresh !== "function") {
    throw new TypeError("refresh must be a function");
  }

  const intervalMs = options.intervalMs ?? LIVE_REFRESH_INTERVAL_MS;
  if (!Number.isFinite(intervalMs) || intervalMs < 0) {
    throw new RangeError("intervalMs must be a non-negative finite number");
  }

  const documentTarget =
    options.documentTarget === undefined
      ? globalTarget("document")
      : options.documentTarget;
  const windowTarget =
    options.windowTarget === undefined
      ? globalTarget("window")
      : options.windowTarget;
  const setTimeoutFn = options.setTimeoutFn || globalThis.setTimeout;
  const clearTimeoutFn = options.clearTimeoutFn || globalThis.clearTimeout;
  const onError =
    typeof options.onError === "function" ? options.onError : () => {};

  let active = false;
  let timer = null;
  let inFlight = null;

  function isHidden() {
    return Boolean(documentTarget?.hidden);
  }

  function clearScheduled() {
    if (timer === null) return;
    clearTimeoutFn(timer);
    timer = null;
  }

  function schedule() {
    clearScheduled();
    if (!active || isHidden() || inFlight) return;
    timer = setTimeoutFn(() => {
      timer = null;
      void runRefresh();
    }, intervalMs);
  }

  function reportError(error) {
    try {
      onError(error);
    } catch {
      // Error reporting must not stop later refreshes.
    }
  }

  function runRefresh() {
    clearScheduled();
    if (!active || isHidden()) return Promise.resolve(false);
    if (inFlight) return inFlight;

    const request = Promise.resolve()
      .then(refresh)
      .then(
        () => true,
        (error) => {
          reportError(error);
          return false;
        },
      )
      .finally(() => {
        if (inFlight === request) {
          inFlight = null;
        }
        schedule();
      });
    inFlight = request;
    return request;
  }

  function handleVisibilityChange() {
    if (isHidden()) {
      clearScheduled();
      return;
    }
    void runRefresh();
  }

  function handleFocus() {
    if (!isHidden()) {
      void runRefresh();
    }
  }

  function addListeners() {
    documentTarget?.addEventListener?.(
      "visibilitychange",
      handleVisibilityChange,
    );
    windowTarget?.addEventListener?.("focus", handleFocus);
  }

  function removeListeners() {
    documentTarget?.removeEventListener?.(
      "visibilitychange",
      handleVisibilityChange,
    );
    windowTarget?.removeEventListener?.("focus", handleFocus);
  }

  return {
    start({ immediate = true } = {}) {
      if (active) return inFlight || Promise.resolve(false);
      active = true;
      addListeners();
      if (immediate) return runRefresh();
      schedule();
      return Promise.resolve(false);
    },

    refreshNow() {
      return runRefresh();
    },

    stop() {
      if (!active) return;
      active = false;
      clearScheduled();
      removeListeners();
    },

    isRunning() {
      return active;
    },
  };
}
