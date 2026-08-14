import assert from "node:assert/strict";
import test from "node:test";

import { getActiveAdminSection } from "../src/utils/adminNavigation.js";

test("admin navigation selects the section for the current route", () => {
  const cases = [
    ["", "accounts"],
    ["/", "accounts"],
    ["/accounts/new", "accounts"],
    ["/accounts/42", "accounts"],
    ["/accounts/42/edit", "accounts"],
    ["/aliases", "aliases"],
    ["/audit", "audit"],
    ["/logs", "logs"],
    ["/logs/archive", "logs"],
    ["/security", "security"],
  ];

  for (const [path, section] of cases) {
    assert.equal(getActiveAdminSection(path), section, path);
  }
});

test("admin navigation only matches complete path segments", () => {
  assert.equal(getActiveAdminSection("/aliases-archive"), "");
  assert.equal(getActiveAdminSection("/logs-archive"), "");
  assert.equal(getActiveAdminSection("/accounting"), "");
  assert.equal(getActiveAdminSection("/login"), "");
});
