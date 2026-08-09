import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const componentPath = new URL(
  "../src/components/SyncErrorLogDialog.vue",
  import.meta.url,
);

test("sync error log dialog keeps logs escaped, copyable, and responsive", async () => {
  const source = await readFile(componentPath, "utf8");

  assert.match(source, /log:\s*\{\s*type:\s*String,\s*default:\s*""\s*\}/);
  assert.match(source, /v-if="hasLog"[\s\S]{0,300}点击查看错误日志/);
  assert.match(source, /查看全部日志/);
  assert.match(source, /name:\s*"logs"/);
  assert.match(source, /account_id/);
  assert.match(source, /<el-dialog[\s\S]*v-model="dialogVisible"/);
  assert.match(
    source,
    /<pre class="sync-error-log-dialog__content">\{\{ logText \}\}<\/pre>/,
  );
  assert.doesNotMatch(source, /v-html|innerHTML/);

  assert.match(source, /import \{ copyText \} from "\.\.\/utils\/clipboard\.js"/);
  assert.match(source, /:icon="CopyDocument"/);
  assert.match(source, /await copyText\(logText\.value\)/);
  assert.match(source, /错误日志已复制/);
  assert.match(source, /错误日志复制失败，请手动复制/);
  assert.match(source, /sync-error-log-dialog__feedback" role="status"/);
  assert.doesNotMatch(source, /aria-live|announcement/);

  assert.match(source, /width="min\(720px, calc\(100vw - 28px\)\)"/);
  assert.match(source, /max-width:\s*100%/);
  assert.match(source, /overflow:\s*auto/);
  assert.match(source, /white-space:\s*pre-wrap/);
  assert.match(source, /overflow-wrap:\s*anywhere/);
  assert.match(source, /@media \(max-width:\s*600px\)/);
});
