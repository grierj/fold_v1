import { readFileSync } from "node:fs";
import { join } from "node:path";
import { gzipSync } from "node:zlib";

const BUDGET_GZIP_KB = 145;

/** @param {string} file */
export function gzipBytes(file) {
  return gzipSync(readFileSync(file)).length;
}

/** @param {string} dist */
export function eagerGzipBytes(dist) {
  const html = readFileSync(join(dist, "index.html"), "utf8");
  const entry = html.match(/<script[^>]+src="\/assets\/([^"]+\.js)"/)?.[1];

  if (!entry) throw new Error("bundle-size: no entry chunk found in dist/index.html");
  const chunks = new Set([entry]);

  for (const [tag] of html.matchAll(/<link\b[^>]*>/g)) {
    if (!/\brel="modulepreload"/.test(tag)) continue;
    const chunk = tag.match(/\bhref="\/assets\/([^"/]+\.js)"/)?.[1];

    if (chunk) chunks.add(chunk);
  }

  const sizes = new Map();

  for (const chunk of chunks) sizes.set(chunk, gzipBytes(join(dist, "assets", chunk)));

  return { sizes, bytes: [...sizes.values()].reduce((sum, size) => sum + size, 0) };
}

if (import.meta.main) {
  const { sizes, bytes } = eagerGzipBytes("dist");

  for (const [chunk, size] of sizes) console.log(`bundle-size: ${chunk}: ${(size / 1024).toFixed(1)} KiB gzip`);
  console.log(`bundle-size: ${sizes.size} eager chunks total ${(bytes / 1024).toFixed(1)} KiB gzip (budget ${BUDGET_GZIP_KB} KiB)`);

  if (bytes > BUDGET_GZIP_KB * 1024) {
    console.error("bundle-size: over budget. Lazy-load what most pages don't need, or raise the budget on purpose.");
    process.exitCode = 1;
  }
}
