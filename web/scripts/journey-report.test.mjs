import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { copyFileSync, mkdirSync, readdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { journeyProfile } from "./journey-report.mjs";

const journeys = ["directory", "date", "more-dates", "editor"];

function report(dateMs, bytes) {
  return {
    cpu: "test CPU", browser: "test Chromium", profile: journeyProfile, eagerGzipBytes: bytes, attempt: null,
    samples: journeys.flatMap((journey) => Array.from({ length: 5 }, (_, i) =>
      ({ journey, run: i + 1, ms: journey === "date" ? dateMs : 100, shift: 0, jsGzipBytes: bytes }))),
  };
}

function comparison(t) {
  const dir = mkdtempSync(join(tmpdir(), "fold-journeys-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));

  return {
    save: (file, value) => writeFileSync(join(dir, file), JSON.stringify(value)),
    run: () => spawnSync(process.execPath, [new URL("./journey-report.mjs", import.meta.url).pathname,
      "base", "head"], { cwd: dir, encoding: "utf8" }),
  };
}

test("medians at both limits pass, including a size increase and one slow outlier", (t) => {
  const { save, run } = comparison(t);
  const base = report(4250, 100_000);
  const head = report(4500, 200_000);

  for (const [journey, baseMs, headMs] of [["directory", 3750, 4000], ["more-dates", 170, 200], ["editor", 70, 100]]) {
    for (const row of base.samples.filter((s) => s.journey === journey)) row.ms = baseMs;

    for (const row of head.samples.filter((s) => s.journey === journey)) row.ms = headMs;
  }

  head.samples[0].ms = 8000;
  save("base", base);
  save("head", head);
  const result = run();
  assert.equal(result.status, 0, result.stdout);
  assert.match(result.stdout, /directory \| 3750 \[3750, 3750\] \| 4000 \[4000, 8000\] \| \+250/);
  assert.match(result.stdout, /Eager JavaScript change: \+100000 gzip bytes/);

  for (const row of head.samples) row.ms += 1;
  save("head", head);
  const over = run();
  assert.equal(over.status, 1);

  for (const journey of journeys) {
    assert.ok(over.stdout.includes(`${journey}: head median`));
    assert.ok(over.stdout.includes(`${journey}: increase of +`));
  }
});

test("the absolute and slower-than-base limits fail independently", (t) => {
  const { save, run } = comparison(t);

  for (const [baseMs, headMs, failure] of [
    [4600, 4501, "head median 4501 ms exceeds 4500 ms"],
    [3900, 4151, "increase of +251 ms exceeds +250 ms"],
    [3900, 7950, "head median 7950 ms exceeds 4500 ms"],
  ]) {
    save("base", report(baseMs, 100_000));
    save("head", report(headMs, 100_000));
    const result = run();
    assert.equal(result.status, 1);
    assert.ok(result.stdout.includes("date: " + failure), result.stdout);
  }

  save("base", report(4200, 100_000));
  save("head", report(0, 100_000));
  assert.equal(run().status, 0);
});

test("invalid evidence identifies the side and preserves the rest of the report", (t) => {
  const { save, run } = comparison(t);

  for (const [side, mutate, failure] of [
    ["base", (r) => { r.samples[0].ms = -1; }, "Base directory: invalid ms in run 1"],
    ["head", (r) => { r.samples[0].ms = null; }, "Head directory: invalid ms in run 1"],
    ["head", (r) => { r.samples[0].run = 2; }, "Head directory: expected one sample"],
    ["head", (r) => { r.samples = {}; }, "Head: no readable measurements (expected a samples array)"],
    ["base", (r) => { r.samples[0] = null; }, "Base: unrecognized journey"],
    ["head", (r) => { delete r.eagerGzipBytes; }, "Head: invalid eagerGzipBytes"],
  ]) {
    const base = report(100, 100_000);
    const head = report(100, 100_000);
    mutate(side === "base" ? base : head);
    save("base", base);
    save("head", head);
    const result = run();
    assert.equal(result.status, 1);
    assert.ok(result.stdout.includes(failure), result.stdout);
    assert.match(result.stdout, /\| editor \|/);
  }
});

test("timing deltas require evidence of a common CPU, browser and profile", (t) => {
  const { save, run } = comparison(t);

  for (const [field, before, after] of [
    ["cpu", "one", "two"], ["browser", "one", "two"], ["cpu", "", ""], ["browser", null, null],
    ["profile", journeyProfile, { ...journeyProfile, cpuRate: 2 }], ["profile", {}, {}],
  ]) {
    save("base", { ...report(100, 100_000), [field]: before });
    save("head", { ...report(100, 100_000), [field]: after });
    const result = run();
    assert.equal(result.status, 1);
    assert.match(result.stdout, /Cannot compare|missing or invalid|configured browser profile/);
    assert.match(result.stdout, /date \| 100 \[100, 100\] \| 100 \[100, 100\] \| — \|/);
  }
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

for (const missingBase of [true, false]) {
  const failure = missingBase ? "base measurement fails" : "the measured head is too slow";
  test(`the CI runner fails and saves its summary when ${failure}`, (t) => {
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

    if (!missingBase) writeFileSync(join(repo, "work/journey_base.json"), JSON.stringify(report(3900, 100_000)));
    writeFileSync(join(repo, "work/journey_head.json"), JSON.stringify(report(missingBase ? 100 : 7900, 100_000)));
    writeFileSync(join(repo, "bin/go"), '#!/bin/sh\ntest "$CGO_ENABLED" = 0\n', { mode: 0o755 });
    writeFileSync(join(repo, "bin/npm"), `#!/bin/sh
if [ "$*" = "run journeys" ]; then
  echo "$E2E_SHARD" >> "$RUNNER_TEMP/attempts"
  if [ ! -f "$RUNNER_TEMP/$E2E_SHARD.json" ]; then exit 42; fi
  mkdir -p "$E2E_OUTPUT_DIR" && cp "$RUNNER_TEMP/$E2E_SHARD.json" "$E2E_OUTPUT_DIR/samples.json"
fi
`, { mode: 0o755 });

    const result = spawnSync("bash", [runner], {
      cwd: repo, encoding: "utf8", env: { ...process.env, PATH: join(repo, "bin") + ":" + process.env.PATH,
        RUNNER_TEMP: join(repo, "work"), GITHUB_STEP_SUMMARY: join(repo, "summary") },
    });

    assert.equal(result.status, 1, result.stderr);
    assert.equal(readFileSync(join(repo, "work/attempts"), "utf8"), "journey_base\njourney_head\n");
    const summary = readFileSync(join(repo, "summary"), "utf8");
    const results = join(repo, "web/test-results");
    const savedReport = readFileSync(join(results, readdirSync(results)[0], "report.md"), "utf8");
    assert.equal(summary, savedReport);
    assert.match(summary, /head measurement completed/);

    if (missingBase) {
      assert.match(summary, /base build or measurement failed \(exit 42\)/);
      assert.match(summary, /Base: no readable measurements/);
      assert.match(summary, /editor \| incomplete \(0\/5\) \| 100 \[100, 100\] \| — \|/);
    } else {
      assert.match(summary, /base measurement completed/);
      assert.match(summary, /date: increase of \+4000 ms exceeds \+250 ms/);
    }
  });
}
