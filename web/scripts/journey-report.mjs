import { readFileSync } from "node:fs";

/**
 * @param {number} ms
 * @param {number} baselineMs
 * @param {{ ceilingMs: number, increaseMs: number }} limit
 */
export function exceedsLimit(ms, baselineMs, limit) {
  return ms > limit.ceilingMs || ms - baselineMs > limit.increaseMs;
}

const read = (path) => JSON.parse(readFileSync(path, "utf8"));
const delta = (value) => value > 0 ? `+${value}` : String(value);

function loadSide(label, file) {
  try {
    const report = read(file);
    console.log(`- ${label}: ${report.revision ?? file}, ${report.cpu}, Chromium ${report.browser}, eager JS ${report.eagerGzipBytes} gzip bytes.`);
    const attempt = report.attempt;

    if (attempt) {
      console.log(`  Interrupted during ${attempt.journey}, run ${attempt.run}. See samples.json and the Playwright output for details.`);
      process.exitCode = 1;
    }

    return report;
  } catch (error) {
    console.log(`- ${label}: no readable measurements (${error.message}).`);
    process.exitCode = 1;

    return null;
  }
}

function summary(report, journey) {
  const rows = (report?.samples ?? []).filter((s) => s.journey === journey).sort((a, b) => a.ms - b.ms);

  if (rows.length !== 5 || rows.some((s) => !Number.isFinite(s.ms) || !Number.isFinite(s.shift) || !Number.isFinite(s.jsGzipBytes))) {
    process.exitCode = 1;

    return { timing: `incomplete (${rows.length}/5)` };
  }

  return {
    ms: rows[2].ms, timing: `${rows[2].ms} [${rows[0].ms}, ${rows[4].ms}]`,
    shift: Math.max(...rows.map((s) => s.shift)).toFixed(3),
    bytes: Math.max(...rows.map((s) => s.jsGzipBytes)),
  };
}

if (import.meta.main) {
  const [beforeFile, afterFile, limitsFile] = process.argv.slice(2);
  const before = loadSide("Base", beforeFile);
  const after = loadSide("Head", afterFile);
  const limits = limitsFile && read(limitsFile);

  const comparable = before && after && before.browser === after.browser && before.cpu === after.cpu &&
    JSON.stringify(before.profile) === JSON.stringify(after.profile);

  if (before && after && !comparable) {
    console.log("Benchmark profiles differ. Timing deltas are unavailable.");
    process.exitCode = 1;
  }

  if (before && after) console.log(`\nEager JavaScript change: ${delta(after.eagerGzipBytes - before.eagerGzipBytes)} gzip bytes.`);
  console.log("\n| Journey | Base median [min, max] ms | Head median [min, max] ms | Change ms | Max shift (base / head) | Route JS gzip bytes (base / head, change) |");
  console.log("|---|---:|---:|---:|---:|---:|");

  for (const journey of ["directory", "date", "more-dates", "editor"]) {
    const base = summary(before, journey);
    const head = summary(after, journey);
    const paired = base.ms !== undefined && head.ms !== undefined;
    const change = comparable && paired ? delta(head.ms - base.ms) : "—";
    const bytes = paired ? `, ${delta(head.bytes - base.bytes)}` : "";
    console.log(`| ${journey} | ${base.timing} | ${head.timing} | ${change} | ${base.shift ?? "—"} / ${head.shift ?? "—"} | ${base.bytes ?? "—"} / ${head.bytes ?? "—"}${bytes} |`);

    if (limits && comparable && paired) {
      const limit = limits[journey];

      if (!Number.isFinite(limit?.ceilingMs) || !Number.isFinite(limit?.increaseMs)) throw new Error(`Missing timing limit for ${journey}`);

      if (exceedsLimit(head.ms, base.ms, limit)) {
        console.error(`${journey} exceeded its timing limit: ${limit.ceilingMs} ms absolute or +${limit.increaseMs} ms from base`);
        process.exitCode = 1;
      }
    }
  }
}
