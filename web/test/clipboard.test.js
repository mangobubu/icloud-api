import assert from "node:assert/strict";
import test from "node:test";

import {
  buildRecentMailDirectLink,
  copyText,
} from "../src/utils/clipboard.js";

function fallbackDocument(copyResult = true) {
  const state = { appended: null, commands: [] };
  const document = {
    body: {
      appendChild(element) {
        state.appended = element;
        element.parentNode = this;
      },
      removeChild(element) {
        if (state.appended === element) state.appended = null;
      },
    },
    createElement(tagName) {
      assert.equal(tagName, "textarea");
      return {
        style: {},
        setAttribute() {},
        select() {},
        remove() {
          state.appended = null;
        },
      };
    },
    execCommand(command) {
      state.commands.push(command);
      return copyResult;
    },
  };
  return { document, state };
}

test("recent-mail direct links stay on the current origin", () => {
  assert.equal(
    buildRecentMailDirectLink(
      "/api/v1/mail/recent?api_key=icm_a%2Bb%2Fc",
      "https://mail.example.test:8443",
    ),
    "https://mail.example.test:8443/api/v1/mail/recent?api_key=icm_a%2Bb%2Fc",
  );
});

test("recent-mail direct links reject unsafe or unexpected paths", () => {
  for (const path of [
    "//attacker.example/api/v1/mail/recent?api_key=secret",
    "https://attacker.example/api/v1/mail/recent?api_key=secret",
    "/api/v1/mail/latest?api_key=secret",
    "/api/v1/mail/recent?api_key=",
    "/api/v1/mail/recent?api_key=one&api_key=two",
    "/api/v1/mail/recent?api_key=secret&next=elsewhere",
    "/api/v1/mail/recent?api_key=secret#fragment",
  ]) {
    assert.throws(
      () => buildRecentMailDirectLink(path, "https://mail.example.test"),
      /链接格式无效/,
    );
  }
});

test("copyText uses Clipboard API immediately in a secure context", async () => {
  const writes = [];
  const copied = await copyText("https://mail.example.test/direct", {
    clipboard: {
      writeText(value) {
        writes.push(value);
        return Promise.resolve();
      },
    },
    document: null,
    isSecureContext: true,
  });

  assert.equal(copied, true);
  assert.deepEqual(writes, ["https://mail.example.test/direct"]);
});

test("copyText falls back when Clipboard API rejects access", async () => {
  const { document, state } = fallbackDocument();
  const copied = await copyText("fallback value", {
    clipboard: {
      writeText() {
        return Promise.reject(new Error("denied"));
      },
    },
    document,
    isSecureContext: true,
  });

  assert.equal(copied, true);
  assert.deepEqual(state.commands, ["copy"]);
  assert.equal(state.appended, null);
});

test("copyText uses and cleans up the fallback outside secure contexts", async () => {
  const { document, state } = fallbackDocument(false);
  const copied = await copyText("fallback value", {
    clipboard: {
      writeText() {
        assert.fail("Clipboard API must not be used outside a secure context");
      },
    },
    document,
    isSecureContext: false,
  });

  assert.equal(copied, false);
  assert.deepEqual(state.commands, ["copy"]);
  assert.equal(state.appended, null);
});
