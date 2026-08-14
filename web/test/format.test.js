import assert from "node:assert/strict";
import test from "node:test";

import { normalizeAccount, normalizeAlias } from "../src/api/admin.js";
import { compactRunes, formatTime, utf8Length } from "../src/utils/format.js";

test("sync timestamps can be rendered with second precision", () => {
  const value = new Date(2026, 7, 7, 9, 8, 7);

  assert.equal(formatTime(value), "2026-08-07 09:08");
  assert.equal(
    formatTime(value, { seconds: true }),
    "2026-08-07 09:08:07",
  );
});

test("UTF-8 password length counts bytes", () => {
  assert.equal(utf8Length("password1234"), 12);
  assert.equal(utf8Length("密码123456"), 12);
});

test("sync errors are normalized and truncated by Unicode code point", () => {
  const compact = compactRunes(`  IMAP\n${"错误".repeat(50)}  `, 20);
  assert.equal(Array.from(compact).length, 20);
  assert.match(compact, /^IMAP 错误/);
  assert.match(compact, /…$/);
});

test("admin DTO normalizers expose only the frontend contract", () => {
  const account = normalizeAccount({
    id: 7,
    email: "primary@icloud.com",
    enabled: true,
    password_ciphertext: "must-not-propagate",
  });
  const alias = normalizeAlias({
    id: 9,
    address: "relay@icloud.com",
    api_key: "api-key",
    imap_password: "imap-password",
    client_id: "client-id",
    refresh_token: "refresh-token",
    otp_url_path: "/api/v1/otp?token=derived-token",
    credential_version: 4,
    api_key_hash: "must-not-propagate",
    credential_ciphertext: "must-not-propagate",
    enabled: true,
  });

  assert.equal(account.email, "primary@icloud.com");
  assert.equal(alias.apiKey, "api-key");
  assert.equal(alias.imapPassword, "imap-password");
  assert.equal(alias.clientId, "client-id");
  assert.equal(alias.refreshToken, "refresh-token");
  assert.equal(alias.otpUrlPath, "/api/v1/otp?token=derived-token");
  assert.equal(alias.credentialVersion, 4);
  assert.equal("passwordCiphertext" in account, false);
  assert.equal("apiKeyHash" in alias, false);
  assert.equal("credentialCiphertext" in alias, false);
});
