import assert from "node:assert/strict";
import test from "node:test";

import { getActiveAdminSection } from "../src/utils/adminNavigation.js";

test("admin navigation selects the section for the current route", () => {
  const cases = [
    ["/admin", "accounts"],
    ["/admin/", "accounts"],
    ["/admin/accounts/new", "accounts"],
    ["/admin/accounts/42", "accounts"],
    ["/admin/accounts/42/edit", "accounts"],
    ["/admin/aliases", "aliases"],
    ["/admin/audit", "audit"],
    ["/admin/logs", "logs"],
    ["/admin/logs/archive", "logs"],
    ["/admin/security", "security"],
  ];

  for (const [path, section] of cases) {
    assert.equal(getActiveAdminSection(path), section, path);
  }
});

test("admin navigation only matches complete path segments", () => {
  assert.equal(getActiveAdminSection("/admin/aliases-archive"), "");
  assert.equal(getActiveAdminSection("/admin/logs-archive"), "");
  assert.equal(getActiveAdminSection("/admin/accounting"), "");
  assert.equal(getActiveAdminSection("/admin/login"), "");
});
