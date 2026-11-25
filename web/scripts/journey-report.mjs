import { readFileSync } from "node:fs";

export function exceedsLimit(ms, baselineMs, limit) {
  return ms > limit.ceilingMs || ms - baselineMs > limit.increaseMs;
}

const read = (path) => JSON.parse(readFileSync(path, "utf8"));
const delta = (value) => value > 0 ? `+${value}` : String(value);

function summary(report, journey) {
  const rows = report.samples.filter((s) => s.journey === journey).sort((a, b) => a.ms - b.ms);

  if (rows.length !== 5 || rows.some((s) => !Number.isFinite(s.ms) || !Number.isFinite(s.shift) || !Number.isFinite(s.jsGzipBytes))) {
    throw new Error(`Incomplete measurements for ${journey}`);
  }

  return {
    ms: rows[2].ms, low: rows[0].ms, high: rows[4].ms,
    shift: Math.max(...rows.map((s) => s.shift)).toFixed(3),
    bytes: Math.max(...rows.map((s) => s.jsGzipBytes)),
  };
}

if (import.meta.main) {
  const [beforeFile, afterFile, limitsFile] = process.argv.slice(2);
  const before = read(beforeFile);
  const after = read(afterFile);
  const limits = limitsFile && read(limitsFile);

  if (before.browser !== after.browser || before.cpu !== after.cpu || JSON.stringify(before.profile) !== JSON.stringify(after.profile)) {
    throw new Error("The benchmark profiles differ");
  }

  console.log(`Comparing ${before.revision ?? beforeFile} to ${after.revision ?? afterFile} on ${after.cpu}, Chromium ${after.browser}.\n`);
  console.log(`Eager JavaScript: ${after.eagerGzipBytes} gzip bytes (${delta(after.eagerGzipBytes - before.eagerGzipBytes)}).\n`);
  console.log("| Journey | Base median [min, max] ms | Head median [min, max] ms | Change ms | Max shift (head) | Route JS gzip bytes (change) |");
  console.log("|---|---:|---:|---:|---:|---:|");

  for (const journey of ["directory", "date", "more-dates", "editor"]) {
    const base = summary(before, journey);
    const head = summary(after, journey);
    console.log(`| ${journey} | ${base.ms} [${base.low}, ${base.high}] | ${head.ms} [${head.low}, ${head.high}] | ${delta(head.ms - base.ms)} | ${head.shift} | ${head.bytes} (${delta(head.bytes - base.bytes)}) |`);

    if (limits) {
      const limit = limits[journey];

      if (!Number.isFinite(limit?.ceilingMs) || !Number.isFinite(limit?.increaseMs)) throw new Error(`Missing timing limit for ${journey}`);

      if (exceedsLimit(head.ms, base.ms, limit)) {
        console.error(`${journey} exceeded its timing limit: ${limit.ceilingMs} ms absolute or +${limit.increaseMs} ms from base`);
        process.exitCode = 1;
      }
    }
  }
}
