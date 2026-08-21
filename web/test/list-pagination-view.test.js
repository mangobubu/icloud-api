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
const stylesPath = new URL("../src/styles/index.css", import.meta.url);

test("shared list pagination binds page, size, total, loading, and page changes", async () => {
  const source = await readFile(componentPath, "utf8");

  assert.match(source, /<el-pagination/);
  assert.match(source, /<el-select/);
  assert.match(source, /:current-page="page"/);
  assert.match(source, /:page-size="pageSize"/);
  assert.match(source, /:total="total"/);
  assert.match(source, /:disabled="loading"/);
  assert.match(source, /@current-change="emit\('change', \$event\)"/);
  assert.match(source, /pageSize:\s*\{\s*type:\s*Number,\s*default:\s*20\s*\}/);
  assert.match(source, /\{ label: "20 条\/页", value: 20 \}/);
  assert.match(source, /\{ label: "50 条\/页", value: 50 \}/);
  assert.match(source, /\{ label: "100 条\/页", value: 100 \}/);
  assert.match(source, /\{ label: "500 条\/页", value: 500 \}/);
  assert.match(source, /\{ label: "1000 条\/页", value: 1000 \}/);
  assert.match(source, /\{ label: "全部显示", value: 0 \}/);
  assert.match(source, /defineEmits\(\["change", "size-change"\]\)/);
  assert.match(source, /emit\("size-change", nextPageSize\)/);
  assert.match(source, /v-if="pageSize > 0"/);
  assert.match(source, /:pager-count="5"/);
});

test("pagination stays aligned on desktop and only reflows at the mobile breakpoint", async () => {
  const styles = await readFile(stylesPath, "utf8");

  assert.match(
    styles,
    /\.list-pagination\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*max-content 120px max-content;/,
  );
  assert.doesNotMatch(styles, /\.list-pagination__controls/);
  assert.match(
    styles,
    /@media \(max-width: 720px\)[\s\S]*?\.list-pagination\s*\{[\s\S]*?grid-template-columns:\s*minmax\(0, 1fr\) 120px;/,
  );
  assert.match(
    styles,
    /@media \(max-width: 720px\)[\s\S]*?\.list-pagination \.el-pagination\s*\{[\s\S]*?grid-column:\s*1 \/ -1;/,
  );
});

test("account management supports bounded pages and a batched all-items mode", async () => {
  const source = await readFile(accountsPath, "utf8");

  assert.match(source, /pageSize\s*=\s*ref\(DEFAULT_PAGE_SIZE\)/);
  assert.match(source, /getAccountPage\(\{/);
  assert.match(source, /offset:\s*\(page - 1\) \* selectedPageSize/);
  assert.match(source, /accounts\.value = nextItems/);
  assert.match(source, /getAllAccounts\(/);
  assert.match(source, /<ListPagination/);
  assert.match(source, /@change="handlePageChange"/);
  assert.match(source, /@size-change="handlePageSizeChange"/);
});

test("alias paging resets on filters and supports full batched display/export", async () => {
  const source = await readFile(aliasesPath, "utf8");

  assert.match(source, /getAliasPage\(accountId,\s*\{/);
  assert.match(source, /offset:\s*\(page - 1\) \* selectedPageSize/);
  assert.match(
    source,
    /function reloadAliasesForFilters[\s\S]{0,180}currentPage\.value = 1/,
  );
  assert.match(source, /v-model="keywordDraft"/);
  assert.match(source, /placeholder="邮箱地址或用途备注"/);
  assert.match(source, /aria-label="关键词：模糊搜索隐私邮箱"/);
  assert.doesNotMatch(source, /:disabled="accountsLoading \|\| accounts\.length === 0"/);
  assert.match(source, /@media \(max-width: 1080px\)/);
  assert.match(source, /function applyAliasSearch/);
  assert.match(source, /appliedAliasQuery\.value = query/);
  assert.match(
    source,
    /function handleAccountFilterChange[\s\S]{0,180}appliedAliasQuery\.value = keywordDraft\.value\.trim\(\)/,
  );
  assert.match(
    source,
    /function handleGroupFilterChange[\s\S]{0,220}appliedAliasQuery\.value = keywordDraft\.value\.trim\(\)/,
  );
  assert.match(source, /query:\s*appliedAliasQuery\.value/);
  assert.match(source, /没有匹配的隐私邮箱/);
  assert.match(source, /:remote-method="searchAccounts"/);
  assert.match(source, /getAccountPage\(\{/);
  assert.match(source, /getAllAliases\(accountId,/);
  assert.match(
    source,
    /v-model="moveTargetGroupId"[\s\S]{0,420}<el-option label="未分组" value="none"/,
  );
  assert.match(
    source,
    /const targetGroupId = moveTargetGroupId\.value === "none"[\s\S]{0,180}moveAliasesToGroup/,
  );
  assert.match(
    source,
    /async function moveAlias\([\s\S]{0,900}loadAliases\(\)[\s\S]{0,120}loadGroups\(\)/,
  );
  assert.match(source, /<ListPagination/);
  assert.match(source, /@size-change="handlePageSizeChange"/);
});

test("account detail aliases use the shared server-backed pagination contract", async () => {
  const source = await readFile(accountDetailPath, "utf8");

  assert.match(source, /pageSize\s*=\s*ref\(DEFAULT_PAGE_SIZE\)/);
  assert.match(source, /currentPage\s*=\s*ref\(1\)/);
  assert.match(source, /total\s*=\s*ref\(0\)/);
  assert.match(
    source,
    /getAccount\(accountId,\s*\{[\s\S]{0,180}limit:\s*selectedPageSize,[\s\S]{0,120}offset:\s*\(page - 1\) \* selectedPageSize/,
  );
  assert.match(source, /getAllAliases\(accountId,\s*\{\s*signal:/);
  assert.match(source, /detail\?\.pagination\?\.total/);
  assert.match(source, /if \(!allItems && page > lastPage\)/);
  assert.match(source, /currentPage\.value = lastPage/);
  assert.match(source, /return await loadDetail\(\{ silent \}\)/);
  assert.match(source, /detailAbortController\?\.abort\(\)/);
  assert.match(source, /detailGate\.isCurrent\(ticket, detailRequestKey\(\)\)/);
  assert.match(source, /<ListPagination/);
  assert.match(source, /:page="currentPage"/);
  assert.match(source, /:page-size="pageSize"/);
  assert.match(source, /:total="total"/);
  assert.match(source, /@change="handlePageChange"/);
  assert.match(source, /@size-change="handlePageSizeChange"/);
  assert.match(
    source,
    /v-if="aliases\.length && pageSize > ALL_PAGE_SIZE && pageSize <= 100"/,
  );
  assert.match(
    source,
    /v-if="!loading && loadError && aliases\.length === 0"[\s\S]{0,220}重新加载邮箱列表/,
  );
  assert.match(source, /v-if="!loading && !loadError && aliases\.length === 0"/);
  assert.match(
    source,
    /'desktop-data-table--force': pageSize > 100 \|\| pageSize === ALL_PAGE_SIZE/,
  );
});

test("mail group refreshes clear loading state and remain available on mobile", async () => {
  const [aliases, accountDetail] = await Promise.all([
    readFile(aliasesPath, "utf8"),
    readFile(accountDetailPath, "utf8"),
  ]);

  for (const source of [aliases, accountDetail]) {
    assert.match(
      source,
      /finally\s*\{[\s\S]{0,180}if \(groupsLoadGate\.isCurrent\(ticket, "groups"\)\)[\s\S]{0,120}groupsLoading\.value = false/,
    );
    assert.doesNotMatch(
      source,
      /finally\s*\{[\s\S]{0,180}!silent && groupsLoadGate\.isCurrent/,
    );
  }
  assert.match(accountDetail, /class="mobile-alias-group-select"/);
  assert.match(accountDetail, /!aliasActionLock\.acquire\(alias\.id\)/);
  assert.match(
    aliases,
    /if \(editingGroupId\.value === group\.id\)\s*\{\s*cancelGroupEdit\(\)/,
  );
});

test("audit records pass page offsets and support full batched display", async () => {
  const source = await readFile(auditPath, "utf8");

  assert.match(source, /getAuditLogs\(\{/);
  assert.match(source, /limit:\s*selectedPageSize/);
  assert.match(source, /offset:\s*\(page - 1\) \* selectedPageSize/);
  assert.match(source, /logs\.value = nextItems/);
  assert.match(source, /getAllAuditLogs\(/);
  assert.match(source, /<ListPagination/);
  assert.match(source, /@size-change="handlePageSizeChange"/);
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
  assert.match(component, /height:\s*availableHeight/);
  assert.match(component, /availableHeight > 0/);
  assert.match(component, /fillHeight:\s*\{\s*type:\s*Boolean/);
  assert.match(component, /props\.fillHeight \? "100%"/);
  assert.match(component, /#cell="scope"/);
  assert.match(component, /#header-cell="scope"/);

  for (const source of [accounts, aliases, audit, logs, accountDetail]) {
    assert.match(source, /<VirtualDataTable/);
    assert.doesNotMatch(source, /<el-table(?:\s|>)/);
    assert.doesNotMatch(source, /<el-table-column(?:\s|>)/);
  }

  assert.equal(
    (accountDetail.match(/<VirtualDataTable/g) || []).length,
    1,
  );
  assert.equal(
    [accounts, aliases, audit, logs]
      .filter((source) => /class="page-stack virtual-list-page"/.test(source))
      .length,
    4,
  );
  assert.equal(
    [accounts, aliases, audit, logs, accountDetail]
      .reduce((count, source) => count + (source.match(/fill-height/g) || []).length, 0),
    5,
  );
  assert.match(accountDetail, /row-key="id"/);
  assert.match(aliases, /someExportableAliasesSelected/);
  assert.match(aliases, /function setAllAliasesSelected/);
});
