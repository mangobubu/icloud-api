function normalizeKey(key) {
  return String(key ?? "");
}

export function createLatestRequestGate() {
  let generation = 0;
  let active = true;

  return {
    begin(key) {
      generation += 1;
      return { generation, key: normalizeKey(key) };
    },
    isCurrent(ticket, currentKey = ticket?.key) {
      return Boolean(
        active &&
          ticket &&
          ticket.generation === generation &&
          ticket.key === normalizeKey(currentKey),
      );
    },
    invalidate() {
      generation += 1;
    },
    deactivate() {
      active = false;
      generation += 1;
    },
  };
}

export function createActionLock() {
  const activeKeys = new Set();

  return {
    acquire(key = "default") {
      const normalized = normalizeKey(key);
      if (activeKeys.has(normalized)) {
        return false;
      }
      activeKeys.add(normalized);
      return true;
    },
    release(key = "default") {
      activeKeys.delete(normalizeKey(key));
    },
    isLocked(key = "default") {
      return activeKeys.has(normalizeKey(key));
    },
    hasAny() {
      return activeKeys.size > 0;
    },
  };
}

export function oneTimeSecretNavigationMode({
  requestPending = false,
  keyVisible = false,
} = {}) {
  if (requestPending) return "block";
  if (keyVisible) return "confirm";
  return "allow";
}
