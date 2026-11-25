import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

const journeys = ["directory", "date", "more-dates", "editor"];

function report(dateMs, bytes) {
  return {
    cpu: "test CPU", browser: "test Chromium", profile: { cpuRate: 4 }, eagerGzipBytes: bytes,
    samples: journeys.flatMap((journey) => Array.from({ length: 5 }, () =>
      ({ journey, ms: journey === "date" ? dateMs : 100, shift: 0, jsGzipBytes: bytes }))),
  };
}

function comparison(t) {
  const dir = mkdtempSync(join(tmpdir(), "fold-journeys-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));

  return {
    save: (file, value) => writeFileSync(join(dir, file), JSON.stringify(value)),
    run: (...args) => spawnSync(process.execPath, [new URL("./journey-report.mjs", import.meta.url).pathname,
      "base", "head", ...args], { cwd: dir, encoding: "utf8" }),
  };
}

test("timing limits catch a regression and ignore a byte increase", (t) => {
  const { save, run } = comparison(t);
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
});

test("a timeout preserves comparisons for finished journeys and reports the missing samples", (t) => {
  const { save, run } = comparison(t);
  const base = report(100, 100_000);
  base.samples.length = 7;
  save("base", { ...base, attempt: { journey: "date", run: 3, error: "Timeout" } });
  save("head", report(220, 200_000));
  const result = run();
  assert.equal(result.status, 1);
  assert.match(result.stdout, /Interrupted during date, run 3/);
  assert.match(result.stdout, /directory \| 100 \[100, 100\] \| 100 \[100, 100\] \| 0 \|/);
  assert.match(result.stdout, /date \| incomplete \(2\/5\) \| 220 \[220, 220\] \| — \|/);
  assert.match(result.stdout, /editor \| incomplete \(0\/5\) \| 100 \[100, 100\] \| — \|/);
});

test("a build with no report leaves the other build's measurements visible", (t) => {
  const { save, run } = comparison(t);
  save("base", report(100, 100_000));
  const result = run();
  assert.equal(result.status, 1);
  assert.match(result.stdout, /Head: no readable measurements/);
  assert.match(result.stdout, /editor \| 100 \[100, 100\] \| incomplete \(0\/5\) \| — \|/);
});

test("the CI runner measures head and publishes its summary after base measurement fails", (t) => {
  const repo = mkdtempSync(join(tmpdir(), "fold-journey-runner-"));
  t.after(() => rmSync(repo, { recursive: true, force: true }));

  for (const dir of ["web/e2e", "web/scripts", "bin", "work"]) mkdirSync(join(repo, dir), { recursive: true });
  const runner = "web/e2e/compare-journeys.sh";
  copyFileSync(new URL("../e2e/compare-journeys.sh", import.meta.url), join(repo, runner));
  copyFileSync(new URL("./journey-report.mjs", import.meta.url), join(repo, "web/scripts/journey-report.mjs"));

  const git = (...args) => execFileSync("git", ["-c", "user.name=Test", "-c", "user.email=test@example.com",
    "-c", "commit.gpgSign=false", ...args], { cwd: repo, stdio: "pipe" });

  git("init");
  git("add", "web");
  git("commit", "-m", "Base");
  git("commit", "--allow-empty", "-m", "Head");
  writeFileSync(join(repo, "work/head.json"), JSON.stringify(report(100, 100_000)));
  writeFileSync(join(repo, "bin/go"), '#!/bin/sh\ntest "$CGO_ENABLED" = 0\n', { mode: 0o755 });
  writeFileSync(join(repo, "bin/npm"), `#!/bin/sh
if [ "$*" = "run journeys" ]; then
  echo "$E2E_SHARD" >> "$RUNNER_TEMP/attempts"
  if [ "$E2E_SHARD" = journey_base ]; then exit 42; fi
  mkdir -p "$E2E_OUTPUT_DIR" && cp "$RUNNER_TEMP/head.json" "$E2E_OUTPUT_DIR/samples.json"
fi
`, { mode: 0o755 });

  const result = spawnSync("bash", [runner], {
    cwd: repo, encoding: "utf8", env: { ...process.env, PATH: join(repo, "bin") + ":" + process.env.PATH,
      RUNNER_TEMP: join(repo, "work"), GITHUB_STEP_SUMMARY: join(repo, "summary") },
  });

  assert.equal(result.status, 1, result.stderr);
  assert.equal(readFileSync(join(repo, "work/attempts"), "utf8"), "journey_base\njourney_head\n");
  const summary = readFileSync(join(repo, "summary"), "utf8");
  assert.match(summary, /base build or measurement failed \(exit 42\)/);
  assert.match(summary, /head measurement completed/);
  assert.match(summary, /editor \| incomplete \(0\/5\) \| 100 \[100, 100\] \| — \|/);
});
