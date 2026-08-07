(function () {
  "use strict";

  var root = document.documentElement;
  var appShell = document.querySelector(".app-shell");
  var sidebar = document.querySelector(".sidebar");
  var sidebarToggle = document.querySelector(
    "[data-sidebar-toggle], .sidebar-toggle"
  );
  var sidebarBackdrop = document.querySelector(".sidebar-backdrop");
  var lastSidebarTrigger = null;

  root.classList.add("js");

  function sidebarIsOpen() {
    return root.classList.contains("sidebar-open");
  }

  function setSidebarOpen(open, returnFocus) {
    root.classList.toggle("sidebar-open", open);
    if (appShell) {
      appShell.classList.toggle("is-sidebar-open", open);
    }
    if (sidebarToggle) {
      sidebarToggle.setAttribute("aria-expanded", String(open));
    }
    if (sidebar) {
      sidebar.setAttribute("aria-hidden", String(!open && isMobile()));
    }

    if (open) {
      lastSidebarTrigger = document.activeElement;
      var firstLink = sidebar && sidebar.querySelector("a, button");
      if (firstLink) {
        window.setTimeout(function () {
          firstLink.focus();
        }, 0);
      }
    } else if (returnFocus && lastSidebarTrigger) {
      lastSidebarTrigger.focus();
    }
  }

  function isMobile() {
    return window.matchMedia("(max-width: 900px)").matches;
  }

  function syncSidebarState() {
    if (!isMobile()) {
      setSidebarOpen(false, false);
      if (sidebar) {
        sidebar.removeAttribute("aria-hidden");
      }
    } else if (sidebar) {
      sidebar.setAttribute("aria-hidden", String(!sidebarIsOpen()));
    }
  }

  if (sidebarToggle) {
    sidebarToggle.setAttribute("aria-expanded", "false");
    sidebarToggle.addEventListener("click", function () {
      setSidebarOpen(!sidebarIsOpen(), sidebarIsOpen());
    });
  }

  document.addEventListener("click", function (event) {
    var closeTrigger = event.target.closest("[data-sidebar-close]");
    if (closeTrigger || event.target === sidebarBackdrop) {
      setSidebarOpen(false, true);
      return;
    }

    var navLink = event.target.closest(".sidebar a");
    if (navLink && isMobile()) {
      setSidebarOpen(false, false);
    }
  });

  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape" && sidebarIsOpen()) {
      setSidebarOpen(false, true);
    }
  });

  window.addEventListener("resize", syncSidebarState);
  syncSidebarState();

  function textFromCopyTrigger(trigger) {
    var reference = (trigger.getAttribute("data-copy") || "").trim();
    var source = null;

    if (reference) {
      try {
        source = document.querySelector(reference);
      } catch (error) {
        source = null;
      }
    }

    if (!source) {
      var container = trigger.closest(".secret-box") || trigger.parentElement;
      source = container && container.querySelector(
        "[data-copy-value], input, textarea, code, .secret-value"
      );
    }

    if (source) {
      if ("value" in source) {
        return source.value;
      }
      return source.getAttribute("data-copy-value") || source.textContent.trim();
    }

    return reference;
  }

  function fallbackCopy(text) {
    var textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.setAttribute("readonly", "");
    textarea.setAttribute("aria-hidden", "true");
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    document.body.appendChild(textarea);
    textarea.select();

    var copied = false;
    try {
      copied = document.execCommand("copy");
    } finally {
      textarea.remove();
    }
    return copied;
  }

  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text).catch(function () {
        if (fallbackCopy(text)) {
          return;
        }
        throw new Error("copy failed");
      });
    }
    return fallbackCopy(text)
      ? Promise.resolve()
      : Promise.reject(new Error("copy failed"));
  }

  function announce(message) {
    var region = document.getElementById("app-live-region");
    if (!region) {
      region = document.createElement("div");
      region.id = "app-live-region";
      region.className = "sr-only";
      region.setAttribute("aria-live", "polite");
      region.setAttribute("aria-atomic", "true");
      document.body.appendChild(region);
    }
    region.textContent = "";
    window.setTimeout(function () {
      region.textContent = message;
    }, 0);
  }

  function showCopyResult(trigger, succeeded) {
    var message = succeeded ? "已复制" : "复制失败，请手动复制";
    var originalLabel = trigger.getAttribute("aria-label");
    var originalTitle = trigger.getAttribute("title");
    var originalText = trigger.textContent;
    var canReplaceText = trigger.children.length === 0 && originalText.trim();

    trigger.classList.toggle("is-copied", succeeded);
    trigger.setAttribute("aria-label", message);
    trigger.setAttribute("title", message);
    if (canReplaceText) {
      trigger.textContent = message;
    }
    announce(message);

    window.setTimeout(function () {
      trigger.classList.remove("is-copied");
      if (originalLabel === null) {
        trigger.removeAttribute("aria-label");
      } else {
        trigger.setAttribute("aria-label", originalLabel);
      }
      if (originalTitle === null) {
        trigger.removeAttribute("title");
      } else {
        trigger.setAttribute("title", originalTitle);
      }
      if (canReplaceText) {
        trigger.textContent = originalText;
      }
    }, 1600);
  }

  document.addEventListener("click", function (event) {
    var trigger = event.target.closest("[data-copy]");
    if (!trigger) {
      return;
    }

    event.preventDefault();
    var text = textFromCopyTrigger(trigger);
    if (!text) {
      showCopyResult(trigger, false);
      return;
    }

    copyText(text).then(
      function () {
        showCopyResult(trigger, true);
      },
      function () {
        showCopyResult(trigger, false);
      }
    );
  });

  function confirmationMessage(element) {
    var message = (element && element.getAttribute("data-confirm")) || "";
    return message.trim() || "确定要执行此操作吗？";
  }

  document.addEventListener("click", function (event) {
    var trigger = event.target.closest("[data-confirm]");
    if (
      !trigger ||
      trigger.tagName === "FORM" ||
      trigger.matches("button[type='submit'], input[type='submit']")
    ) {
      return;
    }
    if (trigger.tagName === "BUTTON" && !trigger.getAttribute("type")) {
      return;
    }
    if (!window.confirm(confirmationMessage(trigger))) {
      event.preventDefault();
      event.stopImmediatePropagation();
    }
  });

  document.addEventListener("submit", function (event) {
    var form = event.target;
    var submitter = event.submitter;
    var confirmSource =
      (submitter && submitter.hasAttribute("data-confirm") && submitter) ||
      (form.hasAttribute("data-confirm") && form);

    if (confirmSource && !window.confirm(confirmationMessage(confirmSource))) {
      event.preventDefault();
      return;
    }

    if (event.defaultPrevented) {
      return;
    }

    if (submitter && submitter.name && !submitter.disabled) {
      var submittedValue = document.createElement("input");
      submittedValue.type = "hidden";
      submittedValue.name = submitter.name;
      submittedValue.value = submitter.value;
      submittedValue.className = "js-submitter-value";
      form.appendChild(submittedValue);
    }

    var buttons = form.querySelectorAll(
      "button[type='submit'], input[type='submit'], button:not([type])"
    );
    buttons.forEach(function (button) {
      if (button.disabled) {
        return;
      }
      button.disabled = true;
      button.setAttribute("aria-disabled", "true");
      button.classList.add("is-submit-disabled");
    });

    if (submitter) {
      submitter.classList.add("is-submitting");
      var submittingText = submitter.getAttribute("data-submitting-text");
      if (submittingText && submitter.tagName === "BUTTON") {
        submitter.dataset.originalText = submitter.textContent;
        submitter.textContent = submittingText;
      }
    }
  });

  window.addEventListener("pageshow", function () {
    document.querySelectorAll(".is-submit-disabled").forEach(function (button) {
      button.disabled = false;
      button.removeAttribute("aria-disabled");
      button.classList.remove("is-submit-disabled");
    });
    document.querySelectorAll(".is-submitting").forEach(function (button) {
      button.classList.remove("is-submitting");
      if (button.dataset.originalText) {
        button.textContent = button.dataset.originalText;
        delete button.dataset.originalText;
      }
    });
    document.querySelectorAll(".js-submitter-value").forEach(function (input) {
      input.remove();
    });
  });

  var syncPollRoot = document.querySelector("[data-sync-poll-endpoint]");
  var syncPollTimer = null;
  var syncPollInFlight = false;
  var syncPollController = null;
  var syncPollingStopped = false;
  var syncPollInterval = 5000;

  function normalizedSyncItem(raw) {
    raw = raw || {};
    return {
      id: raw.id,
      enabled: Boolean(raw.enabled),
      lastSyncStatus: raw.last_sync_status || "pending",
      lastSyncError: raw.last_sync_error || "",
      lastSyncedAt: raw.last_synced_at || null,
    };
  }

  function initialSyncItem(record) {
    return {
      id: record.dataset.syncId,
      enabled: record.dataset.syncEnabled === "true",
      lastSyncStatus: record.dataset.syncStatus || "pending",
      lastSyncError: record.dataset.syncError || "",
      lastSyncedAt: record.dataset.syncAt || null,
    };
  }

  function syncDate(value) {
    if (!value) {
      return null;
    }
    var date;
    if (typeof value === "string" && /^\d+$/.test(value)) {
      date = new Date(Number(value) * 1000);
    } else {
      date = new Date(value);
    }
    return Number.isNaN(date.getTime()) ? null : date;
  }

  function twoDigits(value) {
    return String(value).padStart(2, "0");
  }

  function formatSyncDate(date) {
    return (
      date.getFullYear() +
      "-" +
      twoDigits(date.getMonth() + 1) +
      "-" +
      twoDigits(date.getDate()) +
      " " +
      twoDigits(date.getHours()) +
      ":" +
      twoDigits(date.getMinutes()) +
      ":" +
      twoDigits(date.getSeconds())
    );
  }

  function updateSyncTime(element, value) {
    if (!element) {
      return false;
    }
    var date = syncDate(value);
    if (!date) {
      element.textContent = "-";
      element.removeAttribute("datetime");
      return false;
    }
    element.textContent = formatSyncDate(date);
    element.setAttribute("datetime", date.toISOString());
    return true;
  }

  function compactSyncError(value) {
    var normalized = String(value || "")
      .trim()
      .replace(/\s+/g, " ");
    var characters = Array.from(normalized);
    return characters.length <= 80
      ? normalized
      : characters.slice(0, 79).join("") + "…";
  }

  function syncStatusPresentation(item) {
    if (!item.enabled) {
      return { label: "已停用", className: "status-muted" };
    }
    if (item.lastSyncStatus === "ok") {
      return { label: "正常", className: "status-ok" };
    }
    if (item.lastSyncStatus === "error") {
      return { label: "同步异常", className: "status-error" };
    }
    return { label: "待同步", className: "status-pending" };
  }

  function renderSyncStatus(container, item) {
    if (!container) {
      return;
    }
    var presentation = syncStatusPresentation(item);
    var status = document.createElement("span");
    status.className = "status " + presentation.className;
    status.textContent = presentation.label;
    container.replaceChildren(status);

    if (!container.hasAttribute("data-sync-details") || !item.enabled) {
      return;
    }
    if (item.lastSyncStatus === "error" && item.lastSyncError) {
      var error = document.createElement("small");
      error.textContent = "错误：" + compactSyncError(item.lastSyncError);
      container.appendChild(error);
    }
    if (
      (item.lastSyncStatus === "ok" || item.lastSyncStatus === "error") &&
      item.lastSyncedAt
    ) {
      var detail = document.createElement("small");
      detail.appendChild(
        document.createTextNode(
          item.lastSyncStatus === "error" ? "尝试于 " : "同步于 "
        )
      );
      var time = document.createElement("time");
      if (updateSyncTime(time, item.lastSyncedAt)) {
        detail.appendChild(time);
        container.appendChild(detail);
      }
    }
  }

  function updateSyncError(record, message) {
    var container = record.querySelector("[data-sync-error-container]");
    if (!container) {
      return;
    }
    var text = container.querySelector("[data-sync-error-text]");
    if (text) {
      text.textContent = message || "";
    }
    container.hidden = !message;
  }

  function updateSyncRecord(record, item) {
    record.dataset.syncEnabled = String(item.enabled);
    record.dataset.syncStatus = item.lastSyncStatus;
    record.dataset.syncError = item.lastSyncError;
    if (item.lastSyncedAt) {
      record.dataset.syncAt = item.lastSyncedAt;
    } else {
      delete record.dataset.syncAt;
    }
    renderSyncStatus(record.querySelector("[data-sync-status-cell]"), item);
    updateSyncTime(
      record.querySelector("[data-sync-primary-time]"),
      item.lastSyncedAt
    );
    updateSyncError(record, item.lastSyncError);
  }

  function syncItemsFromPayload(page, data) {
    if (page === "accounts" && Array.isArray(data)) {
      return data.map(function (item) {
        return { kind: "account", item: normalizedSyncItem(item) };
      });
    }
    if (page === "aliases" && Array.isArray(data)) {
      return data.map(function (item) {
        return { kind: "alias", item: normalizedSyncItem(item) };
      });
    }
    if (page === "account-detail" && data && data.account) {
      var items = [
        { kind: "account", item: normalizedSyncItem(data.account) },
      ];
      (Array.isArray(data.aliases) ? data.aliases : []).forEach(function (item) {
        items.push({ kind: "alias", item: normalizedSyncItem(item) });
      });
      return items;
    }
    return [];
  }

  function applySyncPayload(payload) {
    var records = new Map();
    syncPollRoot.querySelectorAll("[data-sync-record]").forEach(function (record) {
      records.set(
        record.dataset.syncKind + ":" + record.dataset.syncId,
        record
      );
    });
    syncItemsFromPayload(syncPollRoot.dataset.syncPollPage, payload.data).forEach(
      function (entry) {
        var record = records.get(entry.kind + ":" + entry.item.id);
        if (record) {
          updateSyncRecord(record, entry.item);
        }
      }
    );
  }

  function syncPageIsVisible() {
    return document.visibilityState !== "hidden";
  }

  function clearSyncPollTimer() {
    if (syncPollTimer !== null) {
      window.clearTimeout(syncPollTimer);
      syncPollTimer = null;
    }
  }

  function scheduleSyncPoll(delay) {
    clearSyncPollTimer();
    if (
      !syncPollRoot ||
      syncPollingStopped ||
      syncPollInFlight ||
      !syncPageIsVisible()
    ) {
      return;
    }
    syncPollTimer = window.setTimeout(runSyncPoll, delay);
  }

  function runSyncPoll() {
    syncPollTimer = null;
    if (
      !syncPollRoot ||
      syncPollingStopped ||
      syncPollInFlight ||
      !syncPageIsVisible()
    ) {
      return;
    }
    syncPollInFlight = true;
    syncPollController = new AbortController();
    window
      .fetch(syncPollRoot.dataset.syncPollEndpoint, {
        method: "GET",
        headers: { Accept: "application/json" },
        credentials: "same-origin",
        cache: "no-store",
        signal: syncPollController.signal,
      })
      .then(function (response) {
        if (response.status === 401 || response.status === 403) {
          syncPollingStopped = true;
        }
        if (!response.ok) {
          throw new Error("sync poll failed");
        }
        return response.json();
      })
      .then(function (payload) {
        if (payload && payload.data !== undefined) {
          applySyncPayload(payload);
        }
      })
      .catch(function (error) {
        if (error.name !== "AbortError") {
          return;
        }
      })
      .finally(function () {
        syncPollInFlight = false;
        syncPollController = null;
        scheduleSyncPoll(syncPollInterval);
      });
  }

  if (syncPollRoot) {
    syncPollRoot.querySelectorAll("[data-sync-record]").forEach(function (record) {
      updateSyncRecord(record, initialSyncItem(record));
    });
    scheduleSyncPoll(syncPollInterval);

    document.addEventListener("visibilitychange", function () {
      if (!syncPageIsVisible()) {
        clearSyncPollTimer();
        if (syncPollController) {
          syncPollController.abort();
        }
        return;
      }
      scheduleSyncPoll(0);
    });
    window.addEventListener("pagehide", clearSyncPollTimer);
    window.addEventListener("pageshow", function () {
      scheduleSyncPoll(0);
    });
  }
})();
