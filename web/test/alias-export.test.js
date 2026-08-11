import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { buildAliasExportText } from "../src/utils/aliasExport.js";

const aliasesViewPath = new URL("../src/views/AliasesView.vue", import.meta.url);

function functionBody(source, signature) {
  const start = source.indexOf(signature);
  assert.notEqual(start, -1, `missing ${signature}`);
  const opening = source.indexOf("{", start);
  assert.notEqual(opening, -1, `missing body for ${signature}`);
  let depth = 0;
  for (let index = opening; index < source.length; index += 1) {
    if (source[index] === "{") depth += 1;
    if (source[index] === "}") {
      depth -= 1;
      if (depth === 0) return source.slice(opening, index + 1);
    }
  }
  assert.fail(`unterminated body for ${signature}`);
}

test("alias export uses one email and absolute direct link per line", () => {
  const exported = buildAliasExportText(
    [
      {
        address: "first@icloud.com",
        directLinkPath: "/api/v1/mail/recent?api_key=first-token",
      },
      {
        address: "second@icloud.com",
        directLinkPath: "/api/v1/mail/recent/?api_key=second%2Btoken",
      },
    ],
    "https://mail.example.test:8443",
  );

  assert.equal(
    exported,
    [
      "first@icloud.com----https://mail.example.test:8443/api/v1/mail/recent?api_key=first-token",
      "second@icloud.com----https://mail.example.test:8443/api/v1/mail/recent/?api_key=second%2Btoken",
    ].join("\r\n"),
  );
});

test("alias export preserves the requested order and has no header", () => {
  const exported = buildAliasExportText(
    [
      {
        address: "z-last@icloud.com",
        directLinkPath: "/api/v1/mail/recent?api_key=z-token",
      },
      {
        address: "a-first@icloud.com",
        directLinkPath: "/api/v1/mail/recent?api_key=a-token",
      },
    ],
    "https://mail.example.test",
  );

  assert.deepEqual(
    exported.split("\r\n").map((line) => line.split("----")[0]),
    ["z-last@icloud.com", "a-first@icloud.com"],
  );
});

test("alias export rejects missing addresses and invalid direct links", () => {
  assert.throws(
    () =>
      buildAliasExportText(
        [
          {
            address: "",
            directLinkPath: "/api/v1/mail/recent?api_key=token",
          },
        ],
        "https://mail.example.test",
      ),
    /邮箱地址格式无效/,
  );

  assert.throws(
    () =>
      buildAliasExportText(
        [
          {
            address: "private@icloud.com",
            directLinkPath:
              "https://attacker.example/api/v1/mail/recent?api_key=token",
          },
        ],
        "https://mail.example.test",
      ),
    /链接格式无效/,
  );
});

test("all aliases view withholds pending confirmation links and exports", async () => {
  const source = await readFile(aliasesViewPath, "utf8");
  const pendingBody = functionBody(source, "function isAliasConfirmationPending");
  const isAliasConfirmationPending = Function(
    `"use strict"; return function (alias) ${pendingBody}`,
  )();
  const exportableBody = functionBody(source, "function isAliasExportable");
  const isAliasExportable = Function(
    "isAliasConfirmationPending",
    `"use strict"; return function (alias) ${exportableBody}`,
  )(isAliasConfirmationPending);

  const pending = {
    enabled: false,
    lastSyncError: "APPLE_ALIAS_CONFIRMATION_PENDING",
    directLinkPath: "/api/v1/mail/recent?api_key=pending-token",
  };
  assert.equal(isAliasConfirmationPending(pending), true);
  assert.equal(isAliasExportable(pending), false);
  assert.equal(
    isAliasExportable({ enabled: true, lastSyncError: "", directLinkPath: "/recent" }),
    true,
  );
  assert.equal(isAliasExportable({ enabled: true, directLinkPath: "" }), false);

  assert.match(source, /:disabled="!isAliasExportable\(row\)"/);
  assert.match(source, /:model-value="allExportableAliasesSelected"/);
  assert.match(source, /:indeterminate="someExportableAliasesSelected"/);
  assert.match(source, /function setAllAliasesSelected/);
  assert.match(source, /:disabled="!isAliasExportable\(alias\)"/);
  assert.match(
    source,
    /v-if="isAliasConfirmationPending\(row\)"[\s\S]{0,180}等待目录确认/,
  );
  assert.match(
    source,
    /v-if="isAliasConfirmationPending\(alias\)"[\s\S]{0,180}等待目录确认/,
  );
  assert.match(source, /v-if="isAliasExportable\(row\)"/);
  assert.match(source, /v-if="isAliasExportable\(alias\)"/);
  assert.match(
    functionBody(source, "function exportAliases"),
    /items\.filter\(isAliasExportable\)/,
  );
  assert.match(
    functionBody(source, "function exportAllAliases"),
    /getAllAliases\(selectedAccountId\.value,\s*\{[\s\S]*query:\s*appliedAliasQuery\.value/,
  );
  assert.match(source, /:loading="exportingAll"/);
  assert.match(source, /exportingAll\.value = true/);
  assert.match(
    functionBody(source, "async function copyAliasDirectLink"),
    /!isAliasExportable\(alias\)/,
  );
});
