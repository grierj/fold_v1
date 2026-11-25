import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

test("timing limits catch a regression and ignore a byte increase", () => {
  const dir = mkdtempSync(join(tmpdir(), "fold-journeys-"));
  const journeys = ["directory", "date", "more-dates", "editor"];
  const save = (file, value) => writeFileSync(join(dir, file), JSON.stringify(value));

  const report = (dateMs, bytes) => ({
    eagerGzipBytes: bytes,
    samples: journeys.flatMap((journey) => Array.from({ length: 5 }, () =>
      ({ journey, ms: journey === "date" ? dateMs : 100, shift: 0, jsGzipBytes: bytes }))),
  });

  const run = (...args) => spawnSync(process.execPath, [new URL("./journey-report.mjs", import.meta.url).pathname,
    "base", "head", ...args], { cwd: dir, encoding: "utf8" });

  try {
    save("base", report(100, 100_000));
    save("head", report(100, 200_000));
    save("limits", Object.fromEntries(journeys.map((name) => [name, { ceilingMs: 500, increaseMs: 100 }])));
    assert.equal(run("limits").status, 0);
    save("head", report(220, 200_000));
    assert.equal(run().status, 0);
    assert.match(run("limits").stderr, /date exceeded its timing limit/);
    assert.equal(run("limits").status, 1);
    save("base", report(480, 100_000));
    save("head", report(501, 100_000));
    assert.equal(run("limits").status, 1);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
