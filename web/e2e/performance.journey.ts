import { expect, test, type Locator, type Page } from "@playwright/test";
import { renameSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { cpus } from "node:os";
import { eagerGzipBytes, gzipBytes } from "../scripts/bundle-size.mjs";
import { journeyLimits, journeyProfile as profile, timingFailures } from "../scripts/journey-report.mjs";
import { apiAs } from "./helpers";
import { continuationGroup } from "./continuation-group";

declare global {
  interface Window { journeyStart: number; journeyShift: number }
}

async function startClick(page: Page) {
  await page.evaluate(() => {
    document.addEventListener("click", (event) => {
      window.journeyStart = event.timeStamp;
      window.journeyShift = 0;
    }, { once: true, capture: true });
  });
}

async function applyProfile(page: Page) {
  const session = await page.context().newCDPSession(page);
  await session.send("Network.enable");
  await session.send("Network.setCacheDisabled", { cacheDisabled: true });
  await session.send("Network.emulateNetworkConditions", {
    offline: false,
    latency: profile.latency, downloadThroughput: profile.downloadThroughput, uploadThroughput: profile.uploadThroughput,
  });
  await session.send("Emulation.setCPUThrottlingRate", { rate: profile.cpuRate });
  await page.addInitScript(() => {
    window.journeyStart = 0;
    window.journeyShift = 0;
    new PerformanceObserver((list) => {
      for (const entry of list.getEntries()) {
        if (entry.startTime >= window.journeyStart && "value" in entry &&
          "hadRecentInput" in entry && !entry.hadRecentInput) window.journeyShift += Number(entry.value);
      }
    }).observe({ type: "layout-shift", buffered: true });
  });
}

interface JourneySample { ms: number; shift: number; scripts: string[] }

async function measureReady(page: Page, journey: string, id: number) {
  const measured = await page.waitForFunction(({ journey, id }) => {
    const visible = (selector: string) => document.querySelector(selector)?.checkVisibility();
    let ready = false;

    switch (journey) {
      case "directory":
        ready = !!visible(`#directory-group-${id} a`) && !!visible("#group-search");
        break;
      case "date":
        ready = document.querySelector("h1")?.textContent === "Visible daily series" &&
          !!visible('[data-date-answer="when"]') && !!visible('[data-date-answer="where"]') &&
          !!visible('[aria-label="Your answer"] button:not(:disabled):not([aria-disabled="true"])');
        break;
      case "more-dates":
        ready = [...document.querySelectorAll("article a")].some((link) =>
          link.checkVisibility() && link.textContent === "See all 55 dates");
        break;
      case "editor": {
        const field = document.querySelector("#group-name");
        ready = field instanceof HTMLInputElement && field.value === "Continuation group" &&
          field.checkVisibility() && !field.matches(":disabled") && !field.readOnly;
        break;
      }
    }

    if (!ready) return false;

    // Give the ready DOM one frame to paint before reading the clock.
    return new Promise<JourneySample>((resolve) => requestAnimationFrame(() => resolve({
      ms: Math.round(performance.now() - window.journeyStart),
      shift: window.journeyShift,
      scripts: [...new Set(performance.getEntriesByType("resource").map((r) => new URL(r.name).pathname)
        .filter((p) => p.startsWith("/assets/") && p.endsWith(".js")))],
    })));
  }, { journey, id }, { timeout: 15_000 });

  const sample = await measured.jsonValue();

  if (!sample) throw new Error(`No measurement for ${journey}`);

  return sample;
}

test("measure useful content and interactions", async ({ browser, request, baseURL }, info) => {
  const { group, event, dates } = await continuationGroup(request);
  const groupPath = `/groups/${group.id}`;
  const datePath = `/events/${event.id}/dates/${dates[0]}`;
  await apiAs(request, "sam");
  const sam = await request.storageState();
  await apiAs(request, "grier");
  const admin = await request.storageState();
  const results: { journey: string; run: number; ms: number; shift: number; jsGzipBytes: number }[] = [];
  let delayedDateMs: number | undefined;
  const dist = process.env.JOURNEY_DIST ?? "dist";

  const reportFile = join(info.project.outputDir, "samples.json");

  const environment = {
    revision: process.env.JOURNEY_REVISION, browser: browser.version(), cpu: cpus()[0].model,
    profile, eagerGzipBytes: eagerGzipBytes(dist).bytes,
  };

  function checkpoint(attempt: { journey: string; run: number; error?: string } | null) {
    writeFileSync(reportFile + ".tmp", JSON.stringify({ ...environment, attempt, delayedDateMs, samples: results }, null, 2));
    renameSync(reportFile + ".tmp", reportFile);
  }

  for (const journey of Object.keys(journeyLimits)) {
    const verifySlowdown = journey === "date";

    for (let run = 0; run <= (verifySlowdown ? 6 : 5); run++) {
      const attempt = { journey, run };
      checkpoint(attempt);

      const context = await browser.newContext({
        baseURL, storageState: journey === "editor" ? admin : sam,
        viewport: profile.viewport, locale: "en-US", timezoneId: "UTC", serviceWorkers: "block",
      });

      try {
        if (run === 0) await context.tracing.start({ screenshots: true, snapshots: true });

        const page = await context.newPage();
        await applyProfile(page);

        if (verifySlowdown && run === 6) {
          await page.route(`**/api/v1${datePath}`, async (route) => {
            await new Promise((resolve) => setTimeout(resolve, 4000));
            await route.continue();
          });
        }

        const path = journey === "directory" ? "/#groups" : journey === "date" ? datePath : groupPath;
        await page.goto(path, { waitUntil: "commit" });

        let control: Locator | undefined;

        if (journey === "more-dates") {
          const more = page.getByRole("button", { name: "Load more dates", exact: true });
          // Measure the final continuation. Its rendered total proves the last page arrived.
          const second = page.waitForResponse((r) => r.url().includes("cursor=") && r.ok());
          await more.click();
          await second;
          await expect(more).toBeEnabled();
          control = more;
        } else if (journey === "editor") {
          await page.getByRole("button", { name: "More group actions" }).click();
          control = page.getByRole("menuitem", { name: "Edit group" });
        }

        if (control) await startClick(page);
        const [sample] = await Promise.all([measureReady(page, journey, group.id), control?.click()]);

        expect(sample.scripts.length).toBeGreaterThan(0);

        if (verifySlowdown && run === 6) {
          const normal = results.flatMap((r) => r.journey === journey ? [r.ms] : []).sort((a, b) => a - b);

          delayedDateMs = sample.ms;
          expect(sample.ms).toBeGreaterThan(normal[4] + 2000);
          expect(timingFailures(sample.ms, sample.ms - normal[2], journeyLimits.date)).not.toHaveLength(0);
        } else if (run > 0) {
          const jsGzipBytes = sample.scripts.reduce((sum: number, asset: string) =>
            sum + gzipBytes(join(dist, asset)), 0);

          results.push({ journey, run, ms: sample.ms, shift: sample.shift, jsGzipBytes });
        }

        checkpoint(attempt);
      } catch (error) {
        checkpoint({ ...attempt, error: error instanceof Error ? error.message : String(error) });
        throw error;
      } finally {
        if (run === 0) await context.tracing.stop({ path: info.outputPath(`${journey}.zip`) });
        await context.close();
      }
    }
  }

  checkpoint(null);
});
