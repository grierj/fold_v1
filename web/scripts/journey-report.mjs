import { readFileSync } from "node:fs";
import assert from "node:assert/strict";

export const journeyLimits = {
  directory: { ceilingMs: 4000, increaseMs: 250 },
  date: { ceilingMs: 4500, increaseMs: 250 },
  "more-dates": { ceilingMs: 200, increaseMs: 30 },
  editor: { ceilingMs: 100, increaseMs: 30 },
};

export const journeyProfile = {
  viewport: { width: 390, height: 844 }, cpuRate: 4, latency: 80,
  downloadThroughput: 200_000, uploadThroughput: 90_000,
};

const delta = (value) => value > 0 ? `+${value}` : String(value);
const nonnegative = (value) => Number.isFinite(value) && value >= 0;

export function timingFailures(ms, change, { ceilingMs, increaseMs }) {
  const reasons = [];

  if (ms > ceilingMs) reasons.push(`head median ${ms} ms exceeds ${ceilingMs} ms`);

  if (change > increaseMs) reasons.push(`increase of +${change} ms exceeds +${increaseMs} ms`);

  return reasons;
}

function loadSide(label, file, failures) {
  try {
    const report = JSON.parse(readFileSync(file, "utf8"));

    if (!Array.isArray(report?.samples)) throw new Error("expected a samples array");
    console.log(`- ${label}: ${report.revision ?? file}, ${report.cpu}, Chromium ${report.browser}, eager JS ${report.eagerGzipBytes} gzip bytes.`);

    if (report.attempt !== null) {
      const attempt = report.attempt;
      failures.push(attempt?.journey ? `${label}: Interrupted during ${attempt.journey}, run ${attempt.run}. See samples.json and the Playwright output.` :
        `${label}: missing completion marker in samples.json.`);
    }

    if (!nonnegative(report.eagerGzipBytes)) failures.push(`${label}: invalid eagerGzipBytes.`);

    if (report.samples.some((s) => !Object.keys(journeyLimits).includes(s?.journey))) failures.push(`${label}: unrecognized journey in samples.`);

    return report;
  } catch (error) {
    failures.push(`${label}: no readable measurements (${error.message}).`);

    return null;
  }
}

function summary(report, journey, label, failures) {
  const rows = (report?.samples ?? []).filter((s) => s?.journey === journey);

  const problem = (reason) => {
    if (report) failures.push(`${label} ${journey}: ${reason}.`);

    return { timing: reason };
  };

  if (rows.length !== 5) return problem(`incomplete (${rows.length}/5)`);

  if (rows.some((s) => !Number.isInteger(s.run) || s.run < 1 || s.run > 5) || new Set(rows.map((s) => s.run)).size !== 5)
    return problem("expected one sample for each run 1–5");

  for (const row of rows) {
    for (const field of ["ms", "shift", "jsGzipBytes"]) {
      if (!nonnegative(row[field])) return problem(`invalid ${field} in run ${row.run}`);
    }
  }

  rows.sort((a, b) => a.ms - b.ms);

  return {
    ms: rows[2].ms, timing: `${rows[2].ms} [${rows[0].ms}, ${rows[4].ms}]`,
    shift: Math.max(...rows.map((s) => s.shift)).toFixed(3),
    bytes: Math.max(...rows.map((s) => s.jsGzipBytes)),
  };
}

function comparableEnvironment(before, after, failures) {
  if (!before || !after) return false;

  try {
    for (const [label, report] of [["Base", before], ["Head", after]]) {
      for (const field of ["cpu", "browser"]) assert.match(report[field], /\S/, `${label}: missing or invalid ${field}.`);
      assert.deepEqual(report.profile, journeyProfile, `${label}: measurements must use the configured browser profile.`);
    }

    for (const field of ["cpu", "browser"]) {
      assert.equal(before[field], after[field], `Cannot compare ${field}: base "${before[field]}" differs from head "${after[field]}".`);
    }

    return true;
  } catch (error) {
    failures.push(error.message);

    return false;
  }
}

function comparableRevisions(before, after, expectedBase, expectedHead, failures) {
  if (!before || !after) return false;
  const sha = /^[0-9a-f]{40}$/;

  try {
    const expected = expectedBase !== undefined || expectedHead !== undefined;

    for (const [label, report, revision] of [["Base", before, expectedBase], ["Head", after, expectedHead]]) {
      assert.match(report.revision, sha, `${label}: missing or invalid revision SHA.`);

      if (expected) {
        assert.match(revision, sha, `${label}: missing or invalid expected revision SHA.`);
        assert.equal(report.revision, revision, `${label}: report revision ${report.revision} does not match expected ${revision}.`);
      }
    }

    assert.notEqual(before.revision, after.revision, "Base and head reports must measure different revisions.");

    return true;
  } catch (error) {
    failures.push(error.message);

    return false;
  }
}

if (import.meta.main) {
  const [beforeFile, afterFile, expectedBase, expectedHead] = process.argv.slice(2);
  const failures = [];
  const before = loadSide("Base", beforeFile, failures);
  const after = loadSide("Head", afterFile, failures);

  const revisionsMatch = comparableRevisions(before, after, expectedBase, expectedHead, failures);
  const comparable = comparableEnvironment(before, after, failures) && revisionsMatch;

  if (nonnegative(before?.eagerGzipBytes) && nonnegative(after?.eagerGzipBytes)) {
    console.log(`\nEager JavaScript change: ${delta(after.eagerGzipBytes - before.eagerGzipBytes)} gzip bytes.`);
  }

  console.log("\n| Journey | Base median [min, max] ms | Head median [min, max] ms | Change ms | Max shift (base / head) | Route JS gzip bytes (base / head, change) |");
  console.log("|---|---:|---:|---:|---:|---:|");

  for (const [journey, limit] of Object.entries(journeyLimits)) {
    const base = summary(before, journey, "Base", failures);
    const head = summary(after, journey, "Head", failures);
    const paired = base.ms !== undefined && head.ms !== undefined;
    const change = comparable && paired ? head.ms - base.ms : undefined;
    const bytes = paired ? `, ${delta(head.bytes - base.bytes)}` : "";
    console.log(`| ${journey} | ${base.timing} | ${head.timing} | ${change === undefined ? "—" : delta(change)} | ${base.shift ?? "—"} / ${head.shift ?? "—"} | ${base.bytes ?? "—"} / ${head.bytes ?? "—"}${bytes} |`);

    if (head.ms !== undefined) {
      for (const reason of timingFailures(head.ms, change, limit)) failures.push(`${journey}: ${reason}.`);
    }
  }

  if (failures.length) {
    console.log("\n### Journey check failed\n");

    for (const reason of failures) console.log(`- ${reason}`);
    process.exitCode = 1;
  }
}
