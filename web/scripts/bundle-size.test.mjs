import { test } from "node:test";
import assert from "node:assert/strict";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { randomBytes } from "node:crypto";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";

const script = new URL("./bundle-size.mjs", import.meta.url);

test("large bundles report unique eager chunks and exclude lazy chunks", () => {
  const cwd = mkdtempSync(join(tmpdir(), "fold-bundle-size-"));

  try {
    mkdirSync(join(cwd, "dist/assets"), { recursive: true });
    writeFileSync(join(cwd, "dist/assets/entry.js"), randomBytes(160 * 1024));
    writeFileSync(join(cwd, "dist/assets/shared.js"), randomBytes(32 * 1024));
    writeFileSync(join(cwd, "dist/assets/lazy.js"), randomBytes(16 * 1024));

    const run = (links) => {
      writeFileSync(join(cwd, "dist/index.html"), `<script type="module" src="/assets/entry.js"></script>${links}`);

      return spawnSync(process.execPath, [script.pathname], { cwd, encoding: "utf8" });
    };

    const eager = run('<link rel="modulepreload" href="/assets/shared.js">');
    assert.equal(eager.status, 0, eager.stdout + eager.stderr);
    assert.match(eager.stdout, /2 eager chunks total 192\.\d KiB gzip/);
    const lazy = run('<link rel="stylesheet" href="/assets/lazy.js">');
    assert.equal(lazy.status, 0, lazy.stdout + lazy.stderr);
    assert.match(lazy.stdout, /1 eager chunks total 160\.\d KiB gzip/);
    const duplicate = run('<link href="/assets/entry.js" rel="modulepreload">');
    assert.equal(duplicate.status, 0, duplicate.stdout + duplicate.stderr);
    assert.match(duplicate.stdout, /1 eager chunks total/);
  } finally {
    rmSync(cwd, { recursive: true, force: true });
  }
});
