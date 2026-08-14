import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  ALIAS_EXPORT_IMAP,
  ALIAS_EXPORT_OTP,
  buildAliasExportText,
} from "../src/utils/aliasExport.js";

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

test("OTP export uses five hyphens, absolute links, one line per alias, and no header", () => {
  const exported = buildAliasExportText(
    [
      {
        address: "first@icloud.com",
        otpUrlPath: "/api/v1/otp?token=first-token",
      },
      {
        address: "second@icloud.com",
        otpUrlPath: "/api/v1/otp?token=second%2Btoken",
      },
    ],
    ALIAS_EXPORT_OTP,
    "https://mail.example.test:8443",
  );

  assert.equal(
    exported,
    [
      "first@icloud.com-----https://mail.example.test:8443/api/v1/otp?token=first-token",
      "second@icloud.com-----https://mail.example.test:8443/api/v1/otp?token=second%2Btoken",
    ].join("\r\n"),
  );
});

test("IMAP export uses four hyphens and preserves requested alias order", () => {
  const exported = buildAliasExportText(
    [
      {
        address: "z-last@icloud.com",
        imapPassword: "imap-z",
        clientId: "client-z",
        refreshToken: "refresh-z",
      },
      {
        address: "a-first@icloud.com",
        imapPassword: "imap-a",
        clientId: "client-a",
        refreshToken: "refresh-a",
      },
    ],
    ALIAS_EXPORT_IMAP,
  );

  assert.equal(
    exported,
    [
      "z-last@icloud.com----imap-z----client-z----refresh-z",
      "a-first@icloud.com----imap-a----client-a----refresh-a",
    ].join("\r\n"),
  );
});

test("credential export rejects missing fields, injected lines, and foreign OTP links", () => {
  assert.throws(
    () =>
      buildAliasExportText(
        [{ address: "", otpUrlPath: "/api/v1/otp?token=token" }],
        ALIAS_EXPORT_OTP,
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
            otpUrlPath: "https://attacker.example/api/v1/otp?token=token",
          },
        ],
        ALIAS_EXPORT_OTP,
        "https://mail.example.test",
      ),
    /链接格式无效/,
  );
  assert.throws(
    () =>
      buildAliasExportText(
        [
          {
            address: "private@icloud.com",
            imapPassword: "imap\npassword",
            clientId: "client",
            refreshToken: "refresh",
          },
        ],
        ALIAS_EXPORT_IMAP,
      ),
    /IMAP 密码格式无效/,
  );
});

test("all aliases view keeps full credentials visible and supports single, checked, and all copy", async () => {
  const source = await readFile(aliasesViewPath, "utf8");
  const exportableBody = functionBody(source, "function isAliasExportable");
  const isAliasExportable = Function(
    `"use strict"; return function (alias) ${exportableBody}`,
  )();
  const complete = {
    address: "private@icloud.com",
    apiKey: "api-key",
    imapPassword: "imap-password",
    clientId: "client-id",
    refreshToken: "refresh-token",
    otpUrlPath: "/api/v1/otp?token=derived",
  };

  assert.equal(isAliasExportable(complete), true);
  assert.equal(isAliasExportable({ ...complete, refreshToken: "" }), false);
  for (const field of ["apiKey", "imapPassword", "clientId", "refreshToken"]) {
    assert.match(source, new RegExp(`\\{\\{ (?:row|alias)\\.${field} \\}\\}`));
  }
  assert.match(source, /copyAliasLine\(row, ALIAS_EXPORT_OTP\)/);
  assert.match(source, /copyAliasLine\(row, ALIAS_EXPORT_IMAP\)/);
  assert.match(source, /copySelectedAliases\(ALIAS_EXPORT_OTP\)/);
  assert.match(source, /copySelectedAliases\(ALIAS_EXPORT_IMAP\)/);
  assert.match(source, /copyAllAliases\(ALIAS_EXPORT_OTP\)/);
  assert.match(source, /copyAllAliases\(ALIAS_EXPORT_IMAP\)/);
  assert.match(
    functionBody(source, "function copyAllAliases"),
    /getAllAliases\(selectedAccountId\.value,\s*\{[\s\S]*query:\s*appliedAliasQuery\.value/,
  );
});
