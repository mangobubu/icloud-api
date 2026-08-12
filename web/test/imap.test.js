import assert from "node:assert/strict";
import test from "node:test";

import {
  DEFAULT_IMAP_HOST,
  DEFAULT_IMAP_PORT,
  formatIMAPEndpoint,
  normalizeIMAPEndpoint,
  normalizeIMAPHost,
  normalizeIMAPPort,
  validateIMAPHost,
  validateIMAPPort,
} from "../src/utils/imap.js";

test("IMAP host normalization mirrors the server DNS and IP contract", () => {
  assert.equal(normalizeIMAPHost(" IMAP.Mail.Me.Com. "), "imap.mail.me.com");
  assert.equal(
    normalizeIMAPHost("\u0085IMAP.Mail.Me.Com.\u0085"),
    "imap.mail.me.com",
  );
  assert.equal(normalizeIMAPHost("192.0.2.10"), "192.0.2.10");
  assert.equal(normalizeIMAPHost("2001:DB8:0:0::1"), "2001:db8::1");
  assert.equal(normalizeIMAPHost("::ffff:192.0.2.1"), "192.0.2.1");
  const longestValidDNS = [63, 63, 63, 61]
    .map((length, index) => String.fromCharCode(97 + index).repeat(length))
    .join(".");
  assert.equal(longestValidDNS.length, 253);
  assert.equal(validateIMAPHost(longestValidDNS), null);
});

test("IMAP host validation rejects endpoint syntax and malformed hosts", () => {
  assert.equal(validateIMAPHost("imap.example.test"), null);
  assert.match(validateIMAPHost(""), /请填写/);
  for (const host of [
    "imaps://imap.example.test",
    "imap.example.test:993",
    "[2001:db8::1]",
    "imap_mail.example",
    "-imap.example",
    "imap..example",
    "\ufeffimap.example.test\ufeff",
    `${"a".repeat(64)}.example`,
    `${"a".repeat(63)}.${"b".repeat(63)}.${"c".repeat(63)}.${"d".repeat(62)}`,
  ]) {
    assert.match(validateIMAPHost(host), /格式不正确/, host);
  }
});

test("IMAP port normalization returns stable integers and defaults bad input", () => {
  assert.equal(normalizeIMAPPort("1143"), 1143);
  assert.equal(normalizeIMAPPort(65535), 65535);
  for (const value of [
    undefined,
    null,
    "",
    "0",
    0,
    65536,
    1.5,
    "x",
    true,
    [993],
  ]) {
    assert.equal(normalizeIMAPPort(value), DEFAULT_IMAP_PORT);
  }
  assert.equal(validateIMAPPort(1), null);
  assert.equal(validateIMAPPort(65535), null);
  assert.match(validateIMAPPort(0), /1-65535/);
  assert.match(validateIMAPPort(65536), /1-65535/);
});

test("IMAP endpoint normalization applies iCloud defaults", () => {
  assert.deepEqual(normalizeIMAPEndpoint(undefined, undefined), {
    host: DEFAULT_IMAP_HOST,
    port: DEFAULT_IMAP_PORT,
  });
  assert.deepEqual(normalizeIMAPEndpoint(" MAIL.EXAMPLE.TEST. ", "1993"), {
    host: "mail.example.test",
    port: 1993,
  });
});

test("IMAP endpoint formatting brackets IPv6 only", () => {
  assert.equal(
    formatIMAPEndpoint("imap.example.test", 993),
    "imap.example.test:993",
  );
  assert.equal(formatIMAPEndpoint("2001:db8::1", 1993), "[2001:db8::1]:1993");
  assert.equal(
    formatIMAPEndpoint({ imapHost: "2001:DB8:0:0::1", imapPort: "2993" }),
    "[2001:db8::1]:2993",
  );
});
