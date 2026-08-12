import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { normalizeAccount } from "../src/api/admin.js";

const viewPath = new URL(
  "../src/views/AccountFormView.vue",
  import.meta.url,
);
const stylesPath = new URL("../src/styles/index.css", import.meta.url);

test("account form exposes editable IMAP host and port with iCloud defaults", async () => {
  const source = await readFile(viewPath, "utf8");

  assert.match(source, /v-model="form\.imapHost"/);
  assert.match(source, /v-model="form\.imapPort"/);
  assert.match(source, /DEFAULT_IMAP_HOST/);
  assert.match(source, /DEFAULT_IMAP_PORT/);
  assert.match(source, /imapHost: DEFAULT_IMAP_HOST/);
  assert.match(source, /imapPort: DEFAULT_IMAP_PORT/);
  assert.match(source, /normalizeIMAPEndpoint/);
  assert.match(source, /imapHost: imapEndpoint\.host/);
  assert.match(source, /imapPort: imapEndpoint\.port/);
  assert.match(source, /imap_host: imapEndpoint\.host/);
  assert.match(source, /imap_port: imapEndpoint\.port/);
  assert.match(source, /imapPort: \[\{ validator: validateIMAPPort/);
  assert.doesNotMatch(source, /model-value="imap\.mail\.me\.com:993（TLS）"\s+readonly/);
});

test("account normalizer keeps custom IMAP endpoint and defaults missing values", () => {
  const custom = normalizeAccount({
    imap_host: "mail.example.test",
    imap_port: 1143,
  });
  assert.deepEqual(
    {
      host: custom.imapHost,
      port: custom.imapPort,
    },
    { host: "mail.example.test", port: 1143 },
  );
  assert.deepEqual(
    {
      host: normalizeAccount({}).imapHost,
      port: normalizeAccount({}).imapPort,
    },
    { host: "imap.mail.me.com", port: 993 },
  );
  const normalizedTypes = normalizeAccount({
    imap_host: " IMAP.Example.Test. ",
    imap_port: "1993",
  });
  assert.equal(normalizedTypes.imapHost, "imap.example.test");
  assert.equal(normalizedTypes.imapPort, 1993);
  assert.equal(typeof normalizedTypes.imapHost, "string");
  assert.equal(typeof normalizedTypes.imapPort, "number");

  const normalizedIPv6 = normalizeAccount({
    IMAPHost: "2001:DB8:0:0::1",
    IMAPPort: 65535,
  });
  assert.equal(normalizedIPv6.imapHost, "2001:db8::1");
  assert.equal(normalizedIPv6.imapPort, 65535);
});

test("IMAP validation errors stay in flow before the endpoint hint", async () => {
  const styles = await readFile(stylesPath, "utf8");

  assert.match(
    styles,
    /\.imap-service-fields \.el-form-item__error\s*\{[^}]*position:\s*static;/s,
  );
  assert.match(
    styles,
    /\.imap-service-fields \.el-form-item__error\s*\{[^}]*flex:\s*0 0 100%;/s,
  );
  assert.match(
    styles,
    /\.imap-service-fields \.el-form-item\s*\{[^}]*margin-bottom:\s*0;/s,
  );
});
