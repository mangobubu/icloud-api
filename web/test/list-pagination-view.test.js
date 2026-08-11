import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const componentPath = new URL(
  "../src/components/ListPagination.vue",
  import.meta.url,
);
const accountsPath = new URL("../src/views/AccountsView.vue", import.meta.url);
const aliasesPath = new URL("../src/views/AliasesView.vue", import.meta.url);
const auditPath = new URL("../src/views/AuditView.vue", import.meta.url);
const logsPath = new URL("../src/views/LogsView.vue", import.meta.url);
const accountDetailPath = new URL(
  "../src/views/AccountDetailView.vue",
  import.meta.url,
);
const virtualTablePath = new URL(
  "../src/components/VirtualDataTable.vue",
  import.meta.url,
);

test("shared list pagination binds page, size, total, loading, and page changes", async () => {
  const source = await readFile(componentPath, "utf8");

  assert.match(source, /<el-pagination/);
  assert.match(source, /:current-page="page"/);
  assert.match(source, /:page-size="pageSize"/);
  assert.match(source, /:total="total"/);
  assert.match(source, /:disabled="loading"/);
  assert.match(source, /@current-change="emit\('change', \$event\)"/);
});

test("account management requests and renders only the current server page", async () => {
  const source = await readFile(accountsPath, "utf8");

  assert.match(source, /PAGE_SIZE\s*=\s*50/);
  assert.match(source, /getAccountPage\(\{/);
  assert.match(source, /offset:\s*\(page - 1\) \* PAGE_SIZE/);
  assert.match(source, /accounts\.value = Array\.isArray\(result\?\.items\)/);
  assert.match(source, /<ListPagination/);
  assert.match(source, /@change="handlePageChange"/);
});

test("alias paging resets on filters and keeps full export independent", async () => {
  const source = await readFile(aliasesPath, "utf8");

  assert.match(source, /getAliasPage\(accountId,\s*\{/);
  assert.match(source, /offset:\s*\(page - 1\) \* PAGE_SIZE/);
  assert.match(
    source,
    /function handleAccountFilterChange[\s\S]{0,180}currentPage\.value = 1/,
  );
  assert.match(source, /:remote-method="searchAccounts"/);
  assert.match(source, /getAccountPage\(\{/);
  assert.match(source, /getAllAliases\(selectedAccountId\.value\)/);
  assert.match(source, /<ListPagination/);
});

test("audit records pass page offsets to the server and replace visible rows", async () => {
  const source = await readFile(auditPath, "utf8");

  assert.match(source, /getAuditLogs\(\{/);
  assert.match(source, /limit:\s*PAGE_SIZE/);
  assert.match(source, /offset:\s*\(page - 1\) \* PAGE_SIZE/);
  assert.match(source, /logs\.value = Array\.isArray\(result\?\.items\)/);
  assert.match(source, /<ListPagination/);
});

test("every desktop data table uses the shared virtual table", async () => {
  const [component, accounts, aliases, audit, logs, accountDetail] =
    await Promise.all([
      readFile(virtualTablePath, "utf8"),
      readFile(accountsPath, "utf8"),
      readFile(aliasesPath, "utf8"),
      readFile(auditPath, "utf8"),
      readFile(logsPath, "utf8"),
      readFile(accountDetailPath, "utf8"),
    ]);

  assert.match(component, /<el-table-v2/);
  assert.match(component, /<el-auto-resizer/);
  assert.match(component, /#cell="scope"/);
  assert.match(component, /#header-cell="scope"/);

  for (const source of [accounts, aliases, audit, logs, accountDetail]) {
    assert.match(source, /<VirtualDataTable/);
    assert.doesNotMatch(source, /<el-table(?:\s|>)/);
    assert.doesNotMatch(source, /<el-table-column(?:\s|>)/);
  }

  assert.equal(
    (accountDetail.match(/<VirtualDataTable/g) || []).length,
    2,
  );
  assert.match(accountDetail, /row-key="address"/);
  assert.match(aliases, /someExportableAliasesSelected/);
  assert.match(aliases, /function setAllAliasesSelected/);
});
