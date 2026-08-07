import assert from "node:assert/strict";
import test from "node:test";

import { normalizeAccount, normalizeAlias } from "../src/api/admin.js";
import { compactRunes, utf8Length } from "../src/utils/format.js";

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
    api_key_prefix: "icm_prefix",
    api_key_hash: "must-not-propagate",
    enabled: true,
  });

  assert.equal(account.email, "primary@icloud.com");
  assert.equal(alias.apiKeyPrefix, "icm_prefix");
  assert.equal("passwordCiphertext" in account, false);
  assert.equal("apiKeyHash" in alias, false);
});
