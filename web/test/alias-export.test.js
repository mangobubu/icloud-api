import assert from "node:assert/strict";
import test from "node:test";

import { buildAliasExportText } from "../src/utils/aliasExport.js";

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
