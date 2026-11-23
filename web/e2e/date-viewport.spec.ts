import AxeBuilder from "@axe-core/playwright";
import { expect, request as playwrightRequest, test, type APIRequestContext, type Browser, type Page } from "@playwright/test";
import { mkdirSync, writeFileSync } from "node:fs";
import * as schema from "valibot";
import { apiAs, email, PASSWORD, saveDate, signInTo } from "./helpers";
import { measure } from "./ui-measure.mjs";

// M8.1 exact-date page on the real app: the six first-screen answers of a visibly changed date
// (brief 5.1), the one answer bar that stays in reach while scrolling, a real series answer and
// its Undo at the same scroll position (brief 5.2), and what a narrower reader is given.
// DATE_PROOF_DIR saves viewport-only screenshots and the measured boxes under viewport/.
const proof = process.env.DATE_PROOF_DIR ? `${process.env.DATE_PROOF_DIR}/viewport` : "";
const adminName = "grier";
const SIZES = [[390, 844], [768, 900], [1280, 900]] as const;
const VIEWER_ZONE = "America/New_York";
const EVENT_ZONE = "America/Chicago";

function save(name: string, json: string) {
  if (!proof) return;
  mkdirSync(proof, { recursive: true });
  writeFileSync(`${proof}/${name}.json`, json);
}

async function shot(page: Page, name: string) {
  if (!proof) return;
  mkdirSync(proof, { recursive: true });
  // Viewport only: a full-page capture cannot show what the first screen holds.
  await page.screenshot({ path: `${proof}/${name}.png`, animations: "disabled", caret: "hide" });
}

const day = (days: number) => new Date(Date.now() + days * 86400_000).toISOString().slice(0, 10);
const idOf = schema.looseObject({ id: schema.number() });

async function people(admin: Awaited<ReturnType<typeof apiAs>>, prefix: string, names: string[]) {
  const ids: Record<string, number> = {};

  for (const name of names) {
    const login = `${prefix}-${name.toLowerCase().replaceAll(" ", "-")}`;
    const { user } = schema.parse(schema.looseObject({ user: idOf }), await admin("POST", "/admin/users", { name, email: email(login), password: PASSWORD, ageBand: "adult", role: "member" }));
    ids[login] = user.id;
  }

  return ids;
}

// Each person answers with their own session, never an admin write on their behalf.
async function answerAs(login: string, eventId: number, body: Record<string, string | number>) {
  const ctx = await playwrightRequest.newContext({ baseURL: test.info().project.use.baseURL });
  await (await apiAs(ctx, login))("PUT", `/events/${eventId}/registrations`, body);
  await ctx.dispose();
}

async function viewerPage(browser: Browser, width: number, height: number) {
  const context = await browser.newContext({ baseURL: test.info().project.use.baseURL, viewport: { width, height }, timezoneId: VIEWER_ZONE, locale: "en-US" });

  return context.newPage();
}

// Every visible text box of each answer, the shell header, the answer bar and the Tabs, read from
// the DOM at the current scroll without any action that scrolls. The clear area is below the header
// and above the first of the bar, the Tabs and the viewport bottom.
const firstScreen = () => {
  const box = (r: DOMRect) => ({ top: Math.round(r.top), bottom: Math.round(r.bottom), left: Math.round(r.left), right: Math.round(r.right), width: Math.round(r.width), height: Math.round(r.height) });
  const header = document.querySelector("body header")!.getBoundingClientRect();
  const bar = document.querySelector('[role="region"][aria-label="Answer this date"]')?.getBoundingClientRect();
  const tabs = document.querySelector('nav[aria-label="Tabs"]')?.getBoundingClientRect();
  const floor = Math.min(innerHeight, bar && bar.height ? bar.top : Infinity, tabs && tabs.height ? tabs.top : Infinity);

  const answers = ["identity", "when", "where", "standing", "attendance", "change"].map((key) => {
    const el = document.querySelector(`[data-date-answer="${key}"]`);

    if (!el) return { key, missing: true };
    const range = document.createRange();
    range.selectNodeContents(el);
    const rects = [...range.getClientRects()].filter((r) => r.width > 0 && r.height > 0);
    const top = Math.min(...rects.map((r) => r.top)), bottom = Math.max(...rects.map((r) => r.bottom));
    const left = Math.min(...rects.map((r) => r.left)), right = Math.max(...rects.map((r) => r.right));

    return { key, text: (el instanceof HTMLElement ? el.innerText : el.textContent ?? "").replace(/\s+/g, " ").trim(), top: Math.round(top), bottom: Math.round(bottom), left: Math.round(left), right: Math.round(right), rects: rects.length };
  });

  return { scrollY, innerWidth, innerHeight, header: box(header), bar: bar && box(bar), tabs: tabs && tabs.height ? box(tabs) : null, floor: Math.round(floor), answers };
};

// The element under a control's center is the control (or inside it): nothing covers it.
async function hit(page: Page, name: string) {
  return page.evaluate((label) => {
    const region = document.querySelector('[role="region"][aria-label="Answer this date"]')!;
    const button = [...region.querySelectorAll("button")].find((b) => b.textContent?.trim() === label)!;
    const r = button.getBoundingClientRect();
    const at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);

    return { label, hit: !!at && button.contains(at), width: Math.round(r.width), height: Math.round(r.height), top: Math.round(r.top), bottom: Math.round(r.bottom) };
  }, name);
}

async function axe(page: Page) {
  const wcag = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"]).analyze();
  const headings = await new AxeBuilder({ page }).withRules(["heading-order"]).analyze();

  return [...wcag.violations, ...headings.violations].flatMap((v) => v.impact === "moderate" || v.impact === "serious" || v.impact === "critical" ? [`${v.impact} ${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`] : []);
}

// A weekly 4-date series in a photo-less public group whose second date was moved from 18:30 to 19:30
// (only startsLocal, so its length stays 90 minutes), with 14 Going (one +1 guest) and 1 Maybe on it.
// The viewer, <prefix>-avery-viewer, is Going on that date only. Each fixture has its own logins, group
// name and weekday, so two of them never share a person or a room booking.
async function changedDate(request: APIRequestContext, { prefix, groupName, startsIn }: { prefix: string; groupName: string; startsIn: number }) {
  const admin = await apiAs(request, adminName);
  const ids = await people(admin, prefix, ["Rowan Leader", "Avery Viewer", "Gale Guesthost", "Mae Maybe", ...Array.from({ length: 12 }, (_, i) => `Filler ${String.fromCharCode(65 + i)} Going`)]);
  const { group } = schema.parse(schema.looseObject({ group: idOf }), await admin("POST", "/groups", { name: groupName, description: "", tags: [], imageUrl: "", location: "Main building", visibility: "public", memberLimit: null }));
  await admin("POST", `/admin/groups/${group.id}/members`, { userId: ids[`${prefix}-rowan-leader`], role: "leader" });

  for (const login of Object.keys(ids).filter((l) => l !== `${prefix}-rowan-leader`)) await admin("POST", `/admin/groups/${group.id}/members`, { userId: ids[login], role: "member" });
  const { rooms } = schema.parse(schema.looseObject({ rooms: schema.array(schema.looseObject({ id: schema.number(), name: schema.string() })) }), await admin("GET", "/admin/rooms"));
  const hall = rooms.find((r) => r.name === "Fellowship Hall")!;

  const { event } = schema.parse(schema.looseObject({ event: idOf }), await admin("POST", `/groups/${group.id}/events`, {
    title: "Weekly supper", startsLocal: `${day(startsIn)}T18:30`, durationMinutes: 90, tzid: EVENT_ZONE, location: "Main building", recurrenceRule: "FREQ=WEEKLY;COUNT=4",
    capacity: 30, remoteAvailable: true, remoteCapacity: 10, guestsAllowed: 2, videoProvider: "link", videoUrl: "https://example.com/viewport-call", videoReveal: "registered_only", roomId: hall.id,
  }));

  const { occurrences } = schema.parse(schema.looseObject({ occurrences: schema.array(schema.looseObject({ date: schema.string() })) }), await admin("GET", `/events/${event.id}`));
  expect(occurrences).toHaveLength(4);
  const date = occurrences[1].date;
  const path = `/events/${event.id}/dates/${date}`;
  // A start-only move: stored and shown length stay 90 minutes, so no duration choice is implied.
  await saveDate(admin, event.id, date, { startsLocal: `${date}T19:30` });

  await answerAs(`${prefix}-avery-viewer`, event.id, { date, status: "going", attendanceMode: "in_person", guestCount: 0 });
  await answerAs(`${prefix}-gale-guesthost`, event.id, { date, status: "going", attendanceMode: "in_person", guestCount: 1 });
  await answerAs(`${prefix}-mae-maybe`, event.id, { date, status: "maybe", attendanceMode: "in_person", guestCount: 0 });

  for (const login of Object.keys(ids).filter((l) => l.startsWith(`${prefix}-filler`))) await answerAs(login, event.id, { date, status: "going", attendanceMode: "in_person", guestCount: 0 });

  return { event, group, date, path };
}

test("a changed date answers six questions on the first screen, and the answer bar applies and undoes a series answer in place", async ({ browser, request }) => {
  test.setTimeout(240_000);
  const { event, group, date, path } = await changedDate(request, { prefix: "vp", groupName: "Viewport supper club", startsIn: 10 });

  const manifest: object[] = [];

  for (const [width, height] of SIZES) {
    const page = await viewerPage(browser, width, height);
    const exactReads: string[] = [], parentReads: string[] = [];
    page.on("request", (r) => {
      const u = new URL(r.url());

      if (u.pathname === `/api/v1${path}`) exactReads.push(r.method());

      if (u.pathname === `/api/v1/events/${event.id}` || u.pathname.startsWith(`/api/v1/groups/${group.id}`) || u.pathname.startsWith(`/api/v1/events/${event.id}/occurrences`)) parentReads.push(`${r.method()} ${u.pathname}`);
    });
    await signInTo(page, "vp-avery-viewer", path);
    await expect(page.getByRole("heading", { level: 1, name: "Weekly supper" })).toBeVisible();
    await page.evaluate(async () => {
      await document.fonts.ready;
      await Promise.all([...document.querySelectorAll("main img")].map((img) => img instanceof HTMLImageElement ? img.decode().catch(() => undefined) : undefined));
    });

    // The reader's own read, with their own cookies: the facts the six answers come from.
    const read = await (await page.request.get(`/api/v1${path}`)).json();
    const occ = read.occurrence;
    expect(occ.series.changes).toEqual(["time"]);
    expect(occ.series.usual.startsLocal).toBe(`${date}T18:30`);
    expect(occ.startsLocal).toBe(`${date}T19:30`); expect(occ.durationMinutes).toBe(90);
    expect(occ.groupHeader).toMatchObject({ imageUrl: "", leaderName: "Grier" });
    expect(occ.room?.name).toBe("Fellowship Hall");
    expect(occ.remoteRsvp).toBe("offered"); expect(occ.canSeeVideo).toBe(true); expect(occ.videoUrl).toBe("https://example.com/viewport-call");
    expect(occ.attendance.registrations).toMatchObject({ going: 14, maybe: 1 });
    expect(occ.seats.inPerson.used).toBeGreaterThan(occ.attendance.registrations.going);
    expect(occ.series).toMatchObject({ upcomingDates: 4, coveredDates: 1, canAnswerSeries: true });

    // The first screen, unscrolled, at rest.
    const first = await page.evaluate(firstScreen);
    expect(first.scrollY).toBe(0);
    const text = Object.fromEntries(first.answers.map((a) => [a.key, "text" in a ? a.text : ""]));
    expect(text.identity).toContain("Weekly supper"); expect(text.identity).toContain("Viewport supper club"); expect(text.identity).toContain("Led by Grier");
    expect(text.when).toMatch(/ · 8:30–10:00\sPM/); expect(text.when).toMatch(/Held in Central time \(7:30\sPM C[SD]T\)/);
    expect(text.where).toContain("Fellowship Hall"); expect(text.where).toContain("Remote attendance available"); expect(text.where).toContain("Join video call");
    expect(text.standing).toContain("You're in for this date only."); expect(text.standing).toContain("You've answered 1 of 4 upcoming dates.");
    expect(text.attendance).toContain("14 going, 1 maybe");
    expect(text.change).toMatch(/^This date starts at 8:30\s?PM, a different time from usual\.$/);

    for (const a of first.answers) {
      expect("missing" in a, `${a.key} at ${width}`).toBe(false);

      if ("missing" in a) continue;
      expect.soft(a.top, `${a.key} at ${width}: under the header`).toBeGreaterThanOrEqual(first.header.bottom);
      expect.soft(a.bottom, `${a.key} at ${width}: under the bar, tabs or screen edge`).toBeLessThanOrEqual(first.floor);
      expect.soft(a.left).toBeGreaterThanOrEqual(0); expect.soft(a.right).toBeLessThanOrEqual(first.innerWidth);
    }

    // The cover is the generated one for a photo-less group, decorative and sized.
    const cover = await page.locator('[data-date-answer="identity"] img').evaluate((img: HTMLImageElement) => ({ alt: img.alt, src: img.getAttribute("src"), w: img.width, h: img.height, decoded: img.complete && img.naturalWidth > 0, position: getComputedStyle(img).objectPosition }));
    expect(cover.alt).toBe(""); expect(cover.decoded).toBe(true); expect(cover.w).toBe(64);
    const barTargets = await Promise.all(["Going", "Maybe", "Can't go"].map((name) => hit(page, name)));

    for (const t of barTargets) { expect.soft(t.hit, `${t.label} at ${width} is covered`).toBe(true); expect.soft(t.height).toBeGreaterThanOrEqual(44); }

    await shot(page, `first-${width}`);
    const layout = await page.evaluate(measure);
    expect.soft(layout.overflow).toBeLessThanOrEqual(0); expect.soft(layout.cutOff).toEqual([]); expect.soft(layout.under24).toEqual([]);

    // Names open on request with a keyboard; the disclosure is one button with aria-controls.
    const names = page.getByRole("button", { name: "Show names" });
    await expect(names).toHaveAttribute("aria-expanded", "false");
    await names.focus(); await page.keyboard.press("Enter");
    await expect(page.getByRole("button", { name: "Hide names" })).toHaveAttribute("aria-expanded", "true");
    await expect(page.getByRole("region", { name: "Going names" })).toContainText("Gale Guesthost (+1 guest)");
    await expect(page.getByRole("region", { name: "Maybe names" })).toContainText("Mae Maybe");
    await shot(page, `names-open-${width}`);
    await page.keyboard.press("Enter");
    await expect(page.getByRole("button", { name: "Show names" })).toHaveAttribute("aria-expanded", "false");
    await expect(page.getByRole("region", { name: "Going names" })).toHaveCount(0);

    // Real scroll with the same answer group: it is not replaced and stays clear of the Tabs. Names are
    // closed here; the next test applies and undoes with them open, scrolled to the end.
    await page.locator('[role="group"][aria-label="Your answer"]').evaluate((el) => { el.setAttribute("data-proof", "bar"); });
    const maxScroll = await page.evaluate(() => document.documentElement.scrollHeight - innerHeight);
    expect(maxScroll, `the page at ${width}x${height} scrolls`).toBeGreaterThan(0);
    await page.mouse.wheel(0, Math.min(400, maxScroll));
    await expect.poll(() => page.evaluate(() => scrollY)).toBeGreaterThan(0);

    const scrolled = await page.evaluate(() => {
      const bar = document.querySelector('[role="region"][aria-label="Answer this date"]')!.getBoundingClientRect();
      const tabs = document.querySelector('nav[aria-label="Tabs"]')?.getBoundingClientRect();

      return { scrollY, same: document.querySelector('[role="group"][aria-label="Your answer"]')?.getAttribute("data-proof") === "bar", bar: { top: Math.round(bar.top), bottom: Math.round(bar.bottom), left: Math.round(bar.left), right: Math.round(bar.right) }, tabsTop: tabs && tabs.height ? Math.round(tabs.top) : null };
    });

    expect(scrolled.same).toBe(true);

    if (scrolled.tabsTop !== null) expect(scrolled.bar.bottom).toBeLessThanOrEqual(scrolled.tabsTop + 1);
    else expect(scrolled.bar.bottom).toBeLessThanOrEqual(height);
    await shot(page, `scrolled-${width}`);

    // A real series answer from the bar at this scroll position: Going, then All future dates.
    // Scroll anchoring may move scrollY when content above changes; what must stay is the content the person was reading.
    const anchor = () => page.evaluate(() => {
      const standing = document.querySelector('[data-date-answer="standing"]')!, attendance = document.querySelector('[data-date-answer="attendance"]')!;

      return { scrollY, top: Math.round(standing.getBoundingClientRect().top), standingHeight: Math.round(standing.getBoundingClientRect().height), attendanceTop: Math.round(attendance.getBoundingClientRect().top) };
    });

    const at = await anchor();
    expect((await hit(page, "Going")).hit).toBe(true);
    await page.getByRole("region", { name: "Answer this date" }).getByRole("button", { name: "Going" }).click();
    const picker = page.getByRole("dialog", { name: /How far should “Going” apply\?/ });
    await expect(picker).toBeVisible();
    const pickerAxe = await axe(page);
    await shot(page, `scope-dialog-${width}`);
    // Escape writes nothing and returns focus to Going.
    const writes: string[] = [];
    page.on("request", (r) => { if (r.method() !== "GET" && r.url().includes(`/api/v1/events/${event.id}/`)) writes.push(`${r.method()} ${new URL(r.url()).pathname}`); });
    await page.keyboard.press("Escape");
    await expect(picker).toBeHidden();
    expect(writes).toEqual([]);
    await expect(page.getByRole("region", { name: "Answer this date" }).getByRole("button", { name: "Going" })).toBeFocused();
    // The same bar button from the keyboard opens the picker again.
    await page.keyboard.press("Enter");
    await expect(picker).toBeVisible();
    const applied = page.waitForResponse((r) => r.request().method() === "POST" && r.url().endsWith(`/api/v1/events/${event.id}/series-registrations`));
    await picker.getByRole("button", { name: "All future dates" }).click();
    const apply = await applied;
    const body = apply.request().postDataJSON();
    expect(body).toMatchObject({ status: "going", scope: "all_future" }); expect(body).not.toHaveProperty("date");
    // The receipt is checked, never saved: tokens stay out of the proof.
    const applyJSON = schema.parse(schema.looseObject({ undoToken: schema.pipe(schema.string(), schema.minLength(1)), undoExpiresAt: schema.string() }), await apply.json());
    expect(Date.parse(applyJSON.undoExpiresAt)).toBeGreaterThan(Date.now());
    const undo = page.getByRole("region", { name: "Answer this date" }).getByRole("button", { name: "Undo" });
    await expect(undo).toBeVisible();
    await expect(page.getByRole("status").filter({ hasText: "You're in for the series." })).toBeVisible();
    const withReceipt = { anchor: await anchor(), undo: await hit(page, "Undo"), going: await hit(page, "Going") };
    await shot(page, `receipt-${width}`);
    expect(withReceipt.anchor.scrollY).toBeGreaterThan(0);
    expect(Math.abs(withReceipt.anchor.top - at.top)).toBeLessThanOrEqual(2);
    expect(withReceipt.undo.hit).toBe(true); expect(withReceipt.undo.height).toBeGreaterThanOrEqual(44);
    expect(Math.abs(withReceipt.undo.top - withReceipt.going.top)).toBeLessThanOrEqual(1);
    const mine = async () => schema.parse(schema.looseObject({ occurrences: schema.array(schema.looseObject({ date: schema.string(), myRegistration: schema.nullable(schema.string()) })) }), await (await page.request.get(`/api/v1/events/${event.id}`)).json()).occurrences.map((o) => o.myRegistration);
    expect(await mine()).toEqual(["going", "going", "going", "going"]);

    // Undo at the same position, before the receipt's deadline.
    const undid = page.waitForResponse((r) => r.request().method() === "POST" && r.url().endsWith(`/api/v1/events/${event.id}/series-registrations/undo`));
    await undo.click();
    expect((await undid).status()).toBe(200);
    await expect(page.getByRole("status").filter({ hasText: "You're in for this date only." })).toBeVisible();
    await expect(undo).toBeHidden();
    const afterUndo = await anchor();
    expect(afterUndo.scrollY).toBeGreaterThan(0);
    expect(Math.abs(afterUndo.top - at.top)).toBeLessThanOrEqual(2);
    expect(await mine()).toEqual([null, "going", null, null]);
    await shot(page, `undone-${width}`);

    // The last content control can be focused into the clear area above the bar.
    const last = await page.evaluate(() => {
      const focusable = [...document.querySelectorAll<HTMLElement>("main a[href], main button:not([disabled]), main input:not([disabled]), main select")].filter((e) => e.getBoundingClientRect().height > 0 && !e.closest('[aria-label="Answer this date"]'));
      const el = focusable[focusable.length - 1];
      el.focus();
      const r = el.getBoundingClientRect();
      const bar = document.querySelector('[role="region"][aria-label="Answer this date"]')!.getBoundingClientRect();

      return { name: (el.textContent || el.getAttribute("aria-label") || "").trim().slice(0, 40), bottom: Math.round(r.bottom), barTop: Math.round(bar.top) };
    });

    expect(last.bottom, `${last.name} sits clear of the bar at ${width}`).toBeLessThanOrEqual(last.barTop);
    await page.evaluate(() => window.scrollTo(0, 0));
    const pageAxe = await axe(page);
    expect.soft(pageAxe, `date page at ${width}`).toEqual([]); expect.soft(pickerAxe, `scope dialog at ${width}`).toEqual([]);

    // The exact-date path never reads the parent event, its dates or its group.
    expect(parentReads).toEqual([]);
    manifest.push({ role: "member, ordinary + series", route: path, width, height, browserZone: VIEWER_ZONE, eventZone: EVENT_ZONE, first, cover: { alt: cover.alt, generated: cover.src?.startsWith("data:") || !cover.src?.startsWith("http"), position: cover.position }, barTargets, scrolled, before: at, withReceipt, afterUndo, last, axe: { page: pageAxe, scopeDialog: pickerAxe }, exactReads: exactReads.length, applyBody: body, outcome: { applied: "going ×4", undone: "going on this date only" } });
    await page.context().close();
  }

  save("changed-date-manifest", JSON.stringify(manifest, null, 2));
});

// The page's own read after a save keeps an open names list open, so the page keeps its length and the
// reader their place, with Undo as reachable as the answer (M8.1 brief 5.2). The reader is scrolled to
// the end, past where a closed list lets the page scroll, so a list that closed would shorten the page
// and move what they were reading. While that read is out, no attendee name is on the page.
test("an open names list stays open through the page's own read after a series answer and its Undo", async ({ browser, request }) => {
  test.setTimeout(180_000);
  const { event, path } = await changedDate(request, { prefix: "vn", groupName: "Names supper club", startsIn: 11 });
  const mine = async (page: Page) => schema.parse(schema.looseObject({ occurrences: schema.array(schema.looseObject({ myRegistration: schema.nullable(schema.string()) })) }), await (await page.request.get(`/api/v1/events/${event.id}`)).json()).occurrences.map((o) => o.myRegistration);
  const manifest: object[] = [];

  for (const [width, height] of SIZES) {
    const page = await viewerPage(browser, width, height);
    await signInTo(page, "vn-avery-viewer", path);
    await expect(page.getByRole("heading", { level: 1, name: "Weekly supper" })).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    const bar = page.getByRole("region", { name: "Answer this date" });
    const maxScroll = () => page.evaluate(() => document.documentElement.scrollHeight - innerHeight);

    // What the reader is looking at: the status, the names list, the page's length and where focus is.
    const anchor = () => page.evaluate(() => {
      const box = (selector: string) => document.querySelector(selector)?.getBoundingClientRect();

      const top = (selector: string) => {
        const r = box(selector);

        return r ? Math.round(r.top) : null;
      };

      const main = document.querySelector("main")?.textContent ?? "";

      return {
        scrollY: Math.round(scrollY), scrollHeight: document.documentElement.scrollHeight, standingTop: top('[data-date-answer="standing"]'), standingHeight: Math.round(box('[data-date-answer="standing"]')?.height ?? 0), maybeNamesTop: top('section[aria-label="Maybe names"]'),
        namesShown: ["Gale Guesthost", "Mae Maybe", "Filler A Going"].filter((n) => main.includes(n)),
        focus: document.activeElement?.textContent?.trim() ?? "", focusInBar: !!document.activeElement?.closest('[role="region"][aria-label="Answer this date"]'), focusKept: document.activeElement?.hasAttribute("data-proof-focus") ?? false,
      };
    });

    // Holds this page's next read of the date until the test lets it through, then stops holding.
    const holdRead = async () => {
      let open!: () => void;
      const gate = new Promise<void>((done) => { open = done; });
      let held: Promise<void> | null = null;

      await page.route(`**/api/v1${path}`, (route) => {
        if (held || route.request().method() !== "GET") return route.continue();
        held = gate.then(() => route.continue());

        return held;
      });

      return async () => {
        await expect.poll(() => held !== null, "the page asked for the date again").toBe(true);
        open();
        await held;
        await page.unroute(`**/api/v1${path}`);
      };
    };

    const closedMax = await maxScroll();
    await page.getByRole("button", { name: "Show names" }).click();
    await expect(page.getByRole("region", { name: "Maybe names" })).toContainText("Mae Maybe");
    const openMax = await maxScroll();
    expect(openMax - closedMax, `the names lengthen the page at ${width}`).toBeGreaterThan(100);
    await page.mouse.wheel(0, openMax + 200);
    await expect.poll(() => page.evaluate(() => Math.round(scrollY))).toBeGreaterThanOrEqual(openMax - 1);
    expect(await mine(page)).toEqual([null, "going", null, null]);

    // A series answer from the bar at the end of the page.
    await bar.getByRole("button", { name: "Going" }).click();
    const picker = page.getByRole("dialog", { name: /How far should “Going” apply\?/ });
    await expect(picker).toBeVisible();
    const before = await anchor();
    const letApplyRead = await holdRead();
    const applied = page.waitForResponse((r) => r.request().method() === "POST" && r.url().endsWith(`/api/v1/events/${event.id}/series-registrations`));
    await picker.getByRole("button", { name: "All future dates" }).click();
    expect((await applied).status()).toBe(200);
    await expect(page.getByRole("status").filter({ hasText: "Updating your answer…" })).toBeVisible();
    await expect.poll(async () => (await anchor()).focusInBar).toBe(true);
    await page.evaluate(() => document.activeElement?.setAttribute("data-proof-focus", ""));
    const applyHeld = await anchor();
    await shot(page, `names-held-${width}`);
    expect(applyHeld.namesShown, `no names while the read is out at ${width}`).toEqual([]);
    expect(applyHeld.maybeNamesTop).toBeNull();
    expect(applyHeld.scrollHeight).toBeGreaterThanOrEqual(before.scrollHeight - 1);
    expect(Math.abs(applyHeld.scrollY - before.scrollY)).toBeLessThanOrEqual(2);
    await letApplyRead();
    await expect(page.getByRole("status").filter({ hasText: "You're in for the series." })).toBeVisible();
    await expect(page.getByRole("button", { name: "Hide names" })).toHaveAttribute("aria-expanded", "true");
    await expect(page.getByRole("region", { name: "Maybe names" })).toContainText("Mae Maybe");
    const withReceipt = await anchor();
    await shot(page, `names-receipt-${width}`);
    expect(withReceipt.scrollY, `scroll kept after the answer at ${width}`).toBeGreaterThanOrEqual(before.scrollY - 2);
    expect(Math.abs((withReceipt.standingTop ?? 0) - (before.standingTop ?? 0)), `the status stays put at ${width}`).toBeLessThanOrEqual(2);
    // The list the reader is looking at moves only by what the status above it gained or lost.
    expect(Math.abs((withReceipt.maybeNamesTop ?? -9999) - (before.maybeNamesTop ?? 0) - (withReceipt.standingHeight - before.standingHeight)), `the names stay put at ${width}`).toBeLessThanOrEqual(2);
    expect(withReceipt.focusKept, `focus stays on ${applyHeld.focus} at ${width}`).toBe(true);
    expect(await mine(page)).toEqual(["going", "going", "going", "going"]);

    // Undo at the same place, with the names still open.
    const undo = bar.getByRole("button", { name: "Undo" });
    expect((await hit(page, "Undo")).hit).toBe(true);
    const letUndoRead = await holdRead();
    const undid = page.waitForResponse((r) => r.request().method() === "POST" && r.url().endsWith(`/api/v1/events/${event.id}/series-registrations/undo`));
    await undo.click();
    expect((await undid).status()).toBe(200);
    await expect(page.getByRole("status").filter({ hasText: "Updating your answer…" })).toBeVisible();
    const undoHeld = await anchor();
    expect(undoHeld.namesShown, `no names while the Undo read is out at ${width}`).toEqual([]);
    expect(undoHeld.scrollHeight).toBeGreaterThanOrEqual(withReceipt.scrollHeight - 1);
    await letUndoRead();
    await expect(page.getByRole("status").filter({ hasText: "You're in for this date only." })).toBeVisible();
    await expect(undo).toBeHidden();
    await expect(page.getByRole("button", { name: "Hide names" })).toHaveAttribute("aria-expanded", "true");
    await expect(page.getByRole("region", { name: "Maybe names" })).toContainText("Mae Maybe");
    const afterUndo = await anchor();
    await shot(page, `names-undone-${width}`);
    expect(afterUndo.scrollY, `scroll kept after Undo at ${width}`).toBeGreaterThanOrEqual(withReceipt.scrollY - 2);
    expect(Math.abs((afterUndo.standingTop ?? 0) - (before.standingTop ?? 0)), `the status stays put after Undo at ${width}`).toBeLessThanOrEqual(2);
    expect(Math.abs((afterUndo.maybeNamesTop ?? -9999) - (before.maybeNamesTop ?? 0) - (afterUndo.standingHeight - before.standingHeight)), `the names stay put after Undo at ${width}`).toBeLessThanOrEqual(2);
    // Undo went away with focus on it: focus is on the answer in the bar.
    expect(afterUndo.focusInBar, `focus stays in the bar after Undo at ${width}`).toBe(true);
    expect(await mine(page)).toEqual([null, "going", null, null]);

    manifest.push({ role: "member, ordinary + series, names open", route: path, width, height, browserZone: VIEWER_ZONE, closedMax, openMax, before, applyHeld, withReceipt, undoHeld, afterUndo, outcome: { applied: "going ×4", undone: "going on this date only" } });
    await page.context().close();
  }

  save("names-open-manifest", JSON.stringify(manifest, null, 2));
});

// A cancelled date (brief 2.2): the person who had a place gets exactly its title, date and that it
// was cancelled; anyone else, signed in or not, gets the same answer as a date that never existed.
test("a cancelled date is a tombstone only for someone who had a place, and reads like a missing date to anyone else", async ({ browser, request }) => {
  test.setTimeout(120_000);
  const admin = await apiAs(request, adminName);
  const ids = await people(admin, "tomb", ["Kit Kept", "Nell Norow"]);
  const { group } = schema.parse(schema.looseObject({ group: idOf }), await admin("POST", "/groups", { name: "Tombstone circle", description: "", tags: [], imageUrl: "", location: "Chapel", visibility: "public", memberLimit: null }));

  for (const id of Object.values(ids)) await admin("POST", `/admin/groups/${group.id}/members`, { userId: id, role: "member" });
  const { event } = schema.parse(schema.looseObject({ event: idOf }), await admin("POST", `/groups/${group.id}/events`, { title: "Evening prayer", startsLocal: `${day(12)}T19:00`, durationMinutes: 60, tzid: "UTC", location: "Chapel", recurrenceRule: "FREQ=WEEKLY;COUNT=3", remoteAvailable: true, videoProvider: "link", videoUrl: "https://example.com/tomb-call", videoReveal: "registered_only" }));
  const { occurrences } = schema.parse(schema.looseObject({ occurrences: schema.array(schema.looseObject({ date: schema.string() })) }), await admin("GET", `/events/${event.id}`));
  const date = occurrences[1].date;
  const path = `/events/${event.id}/dates/${date}`;
  const missing = `/events/${event.id}/dates/${new Date(Date.parse(date) + 86400_000).toISOString().slice(0, 10)}`;
  await answerAs("tomb-kit-kept", event.id, { date, status: "going", attendanceMode: "in_person", guestCount: 0 });

  // The person with a place has the scheduled date open when it is cancelled; a focus check brings the tombstone.
  const page = await viewerPage(browser, 390, 844);
  await signInTo(page, "tomb-kit-kept", path);
  await expect(page.getByRole("region", { name: "Answer this date" })).toBeVisible();
  await admin("POST", `/events/${event.id}/cancel`, { date });
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect(page.getByText("This date was cancelled. Your calendar shows the dates that are still on.")).toBeVisible();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Evening prayer");
  await expect(page.getByRole("region", { name: "Answer this date" })).toHaveCount(0);
  await expect(page.locator("main button")).toHaveCount(0);
  const links = await page.locator("main a").evaluateAll((as) => as.map((a) => a.getAttribute("href")));
  expect(links.every((href) => href === "/calendar")).toBe(true);
  await expect(page.locator(`a[href="/events/${event.id}"], a[href="/groups/${group.id}"], a[href*="example.com"]`)).toHaveCount(0);
  await shot(page, "tombstone-390");
  const tombAxe = await axe(page);
  expect(tombAxe).toEqual([]);
  const tomb = await page.request.get(`/api/v1${path}`);
  expect(tomb.status()).toBe(200);
  expect(await tomb.json()).toEqual({ occurrence: { title: "Evening prayer", date, cancelled: true } });
  const ics = await page.request.get(`/api/v1${path}/calendar.ics`);
  expect(ics.status()).toBe(404);

  // A member without a place, and an anonymous reader: the same status and body as a missing date.
  const answers: { who: string; status: number; body: string; sameAsMissing: boolean }[] = [];

  for (const who of ["tomb-nell-norow", null]) {
    const ctx = await playwrightRequest.newContext({ baseURL: test.info().project.use.baseURL });

    if (who) await apiAs(ctx, who);
    const cancelled = await ctx.get(`/api/v1${path}`), never = await ctx.get(`/api/v1${missing}`);
    const cancelledBody = await cancelled.text(), neverBody = await never.text();
    expect(cancelled.status()).toBe(never.status());
    expect(cancelledBody).toBe(neverBody);
    expect(cancelledBody).not.toContain("Evening prayer"); expect(cancelledBody).not.toContain("cancel");
    expect((await ctx.get(`/api/v1${path}/calendar.ics`)).status()).toBe(404);
    answers.push({ who: who ?? "anonymous", status: cancelled.status(), body: cancelledBody, sameAsMissing: true });
    await ctx.dispose();
  }

  save("tombstone", JSON.stringify({ qualified: { status: 200, body: { occurrence: { title: "Evening prayer", date, cancelled: true } }, ics: ics.status(), links, axe: tombAxe }, denied: answers }, null, 2));
  await page.context().close();
});

// A signed-out visitor at phone width: the bar's one guest opener, the form in the page's flow, and
// Never mind returning focus to the opener. No tabs are shown signed out, so the bar sits on the inset.
test("a signed-out visitor opens the guest form from the bar, keeps typed values on a failed submit, and gets focus back on Never mind", async ({ browser, request }) => {
  test.setTimeout(90_000);
  const admin = await apiAs(request, adminName);
  const { group } = schema.parse(schema.looseObject({ group: idOf }), await admin("POST", "/groups", { name: "Open supper table", description: "", tags: [], imageUrl: "", location: "Hall", visibility: "public", memberLimit: null }));
  const { event } = schema.parse(schema.looseObject({ event: idOf }), await admin("POST", `/groups/${group.id}/events`, { title: "Open supper", startsLocal: `${day(9)}T18:00`, durationMinutes: 60, tzid: "UTC", location: "Hall", recurrenceRule: "FREQ=WEEKLY;COUNT=2" }));
  const { occurrences } = schema.parse(schema.looseObject({ occurrences: schema.array(schema.looseObject({ date: schema.string() })) }), await admin("GET", `/events/${event.id}`));
  const path = `/events/${event.id}/dates/${occurrences[0].date}`;
  const guestDoor: object[] = [];

  for (const [width, height] of SIZES) {
    const page = await viewerPage(browser, width, height);
    let submissions = 0;
    page.on("request", (r) => { if (r.method() === "POST" && r.url().endsWith(`/api/v1/events/${event.id}/guest-registrations`)) submissions++; });
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1, name: "Open supper" })).toBeVisible();
    const bar = page.getByRole("region", { name: "Answer this date" });
    const opener = bar.getByRole("button", { name: "Register as a guest" });
    await expect(opener).toBeVisible();
    await expect(page.getByRole("button", { name: "Register as a guest" })).toHaveCount(1);
    await expect(page.locator('nav[aria-label="Tabs"]:visible')).toHaveCount(0);
    const target = await hit(page, "Register as a guest");
    expect(target.hit).toBe(true); expect(target.height).toBeGreaterThanOrEqual(44); expect(target.bottom).toBeLessThanOrEqual(height);
    await expect(page.getByRole("link", { name: "Log in to answer" })).toBeVisible();
    await shot(page, `guest-door-${width}`);
    await opener.click();
    await expect(page.getByLabel("Full name")).toBeFocused();
    await expect(page.getByRole("button", { name: "Register as a guest" })).toHaveCount(0);
    // Shot before anything is typed: a guest's email never goes into the proof.
    await shot(page, `guest-form-${width}`);
    await page.keyboard.type("Visitor Vale");
    await page.keyboard.press("Tab"); await page.keyboard.press("Tab");
    await page.getByLabel("Email").fill("vale-viewport@example.com");
    await page.getByRole("button", { name: "Email me a confirmation link" }).click();
    await expect(page.getByLabel(/I am 18 or older/)).toBeFocused();
    expect(submissions).toBe(0);
    await expect(page.getByLabel("Full name")).toHaveValue("Visitor Vale");
    const formAxe = await axe(page);
    expect.soft(formAxe, `guest form at ${width}`).toEqual([]);
    await page.getByRole("button", { name: "Never mind" }).click();
    await expect(bar.getByRole("button", { name: "Register as a guest" })).toBeFocused();
    guestDoor.push({ width, height, target, formAxe, submissionsWhileInvalid: submissions });
    await page.context().close();
  }

  // Log in to answer returns to this same date.
  const page = await viewerPage(browser, 390, 844);
  await page.goto(path);
  await page.getByRole("link", { name: "Log in to answer" }).click();
  await page.getByLabel("Email").fill(email("sam")); await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Log in" }).click();
  await expect(page).toHaveURL(new RegExp(`${path}$`));
  await page.context().close();
  save("guest-door", JSON.stringify(guestDoor, null, 2));
});
