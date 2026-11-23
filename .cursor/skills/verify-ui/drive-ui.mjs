// verify-ui drive: seed a fresh verify-fold instance, then load the main
// pages as signed-out, member, leader and admin at 390/768/1280, saving a
// screenshot and running the UI checks for each. Writes results.json and
// result.txt under $VERIFY_EVIDENCE/ui/. Exit 1 when a hard check fails.
// Run only against an instance started by `verify-fold up`.
import { randomBytes } from "node:crypto";
import { createRequire } from "node:module";
import { mkdir, writeFile } from "node:fs/promises";
import path from "node:path";
// One measurement shared with web/e2e/layout.spec.ts.
import { measure } from "../../../web/e2e/ui-measure.mjs";

const require = createRequire(new URL("../../../web/package.json", import.meta.url));
const { chromium } = require("playwright");
const AxeBuilder = require("@axe-core/playwright").default;

const url = process.env.VERIFY_URL;
const evidence = process.env.VERIFY_EVIDENCE;
if (!url || !evidence) { console.error("drive-ui: eval \"$(verify-fold env RUN_ID)\" first"); process.exit(2); }
const only = (process.env.UI_PAGES || "").split(",").filter(Boolean); // e.g. UI_PAGES=admin,schedule-event
const widths = (process.env.UI_WIDTHS || "390,768,1280").split(",").map(Number);
const dir = path.join(evidence, "ui");
await mkdir(dir, { recursive: true });

const PASSWORD = "password1234";
const people = { admin: ["verify-admin@example.com", "Verify Admin"], leader: ["verify-leader@example.com", "Lee Leader"], member: ["verify-member@example.com", "Sam Member"] };

const browser = await chromium.launch();
const newPage = async () => {
  const ctx = await browser.newContext({ locale: "en-US", timezoneId: "America/Boise", viewport: { width: 1280, height: 900 } });
  const p = await ctx.newPage(); p.setDefaultTimeout(15_000); return p;
};
async function signIn(page, who) {
  await page.goto(url + "/login");
  await page.getByLabel("Email").fill(people[who][0]);
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Log in" }).click();
  await page.waitForURL((u) => !u.pathname.startsWith("/login"));
}
async function api(page, method, p, data) {
  const csrf = (await (await page.request.get(url + "/api/v1/auth/csrf")).json()).csrfToken;
  const r = await page.request.fetch(url + "/api/v1" + p, { method, data, headers: { "X-CSRF-Token": csrf } });
  if (!r.ok()) throw new Error(`${method} ${p}: ${r.status()} ${await r.text()}`);
  return r.json();
}

// A manager's date save through the guarded edit, as the date editor sends it: the revision from the
// manager's own read of that date, a fresh attempt id and only the changed fields.
async function saveDate(page, eventId, date, changes) {
  const { occurrence: base } = await api(page, "GET", `/events/${eventId}/occurrences/${date}`);
  const { occurrence } = await api(page, "POST", `/events/${eventId}/occurrences/${date}/edits`, { attempt: randomBytes(16).toString("hex"), revision: base.revision, changes });
  if (occurrence?.id !== base.id || !occurrence.revision) throw new Error(`seed: the save of ${date} did not answer that date`);
}

// ---- Seed (precondition, not proof): setup wizard through the real form,
// then people, a group and a weekly series through the admin's own API calls.
const seedPage = await newPage();
await seedPage.goto(url + "/");
let seed;
if (new URL(seedPage.url()).pathname === "/setup") {
  await seedPage.getByLabel("Setup code").fill("a".repeat(64));
  await seedPage.getByRole("button", { name: "Continue" }).click();
  await seedPage.locator("[name=site_name]").fill("Fold verify");
  await seedPage.locator("[name=timezone]").fill("America/Boise");
  await seedPage.locator("[name=name]").fill(people.admin[1]);
  await seedPage.locator("[name=email]").fill(people.admin[0]);
  await seedPage.locator("[name=password]").fill(PASSWORD);
  await seedPage.locator("[name=password_confirm]").fill(PASSWORD);
  await seedPage.locator("[name=age_band]").check();
  await seedPage.getByRole("button", { name: "Create site" }).click();
  await seedPage.waitForURL((u) => u.pathname === "/login");
}
await signIn(seedPage, "admin");
{
  const { groups } = await api(seedPage, "GET", "/groups");
  let group = groups.find((g) => g.name === "Tuesday Bible Study");
  if (!group) {
    const ids = {};
    for (const who of ["leader", "member"]) {
      ids[who] = (await api(seedPage, "POST", "/admin/users", { ageBand: "adult", email: people[who][0], name: people[who][1], password: PASSWORD, role: "member" })).user.id;
    }
    group = (await api(seedPage, "POST", "/groups", { name: "Tuesday Bible Study", description: "Weekly study of Romans.", tags: ["bible-study", "adults"], imageUrl: "", location: "Fellowship Hall", visibility: "public", memberLimit: 12 })).group;
    await api(seedPage, "POST", `/admin/groups/${group.id}/members`, { userId: ids.leader, role: "leader" });
    await api(seedPage, "POST", `/admin/groups/${group.id}/members`, { userId: ids.member, role: "member" });
    await api(seedPage, "POST", "/groups", { name: "Youth Choir", description: "Ages 13 to 17.", tags: ["music", "youth"], imageUrl: "", location: "Choir room", visibility: "public", memberLimit: null });
  }
  // A long-named group for the composer's Send to check (#288). Looked up by name
  // so a repeat run finds it; the leader's id is resolved from the user list when
  // the Tuesday branch above was skipped.
  const longName = "Saint Brigid's Wednesday Evening Women's Bible Study and Prayer Fellowship (North Campus)";
  let longGroup = groups.find((g) => g.name === longName);
  if (!longGroup) {
    const { users = [] } = await api(seedPage, "GET", "/admin/users");
    const leaderId = users.find((u) => u.email === people.leader[0])?.id;
    if (!leaderId) throw new Error("seed: verify-leader is missing, so the long group has no leader");
    longGroup = (await api(seedPage, "POST", "/groups", { name: longName, description: "Not for visitors.", tags: [], imageUrl: "", location: "", visibility: "invite_only", memberLimit: null })).group;
    await api(seedPage, "POST", `/admin/groups/${longGroup.id}/members`, { userId: leaderId, role: "leader" });
  }
  const now = new Date(); const d = new Date(now.getTime() + 7 * 864e5).toISOString().slice(0, 10);
  const { events = [] } = await api(seedPage, "GET", `/groups/${group.id}/events`).catch(() => ({}));
  // The group's event list names each series by eventId (its row id is the date's).
  const listed = events.find((e) => e.title === "Romans 8");
  const event = listed ? { id: listed.eventId ?? listed.id } : (await api(seedPage, "POST", `/groups/${group.id}/events`, { title: "Romans 8", startsLocal: `${d}T19:00`, durationMinutes: 90, tzid: "America/Boise", location: "Fellowship Hall", recurrenceRule: "FREQ=WEEKLY;COUNT=4" })).event;
  seed = { groupId: group.id, eventId: event.id, longGroupId: longGroup.id, longName };

  // Exact-date fixtures (M8.1): a moved recurring date the member answered for that date only, a full
  // date with the member waiting, a date only an exact-date invitation opens, and a cancelled date the
  // member had a place on (the leader had none, so it reads as missing to them). Looked up by title so a
  // re-run reuses them; each person writes with their own session.
  const as = async (who, fn) => { const pg = await newPage(); await signIn(pg, who); try { return await fn(pg); } finally { await pg.context().close(); } };
  const { users = [] } = await api(seedPage, "GET", "/admin/users");
  const userId = (who) => users.find((u) => u.email === people[who][0])?.id;
  const datesOf = async (id) => (await api(seedPage, "GET", `/events/${id}`)).occurrences;
  const eventNamed = async (groupId, title, body) => {
    const { events: list = [] } = await api(seedPage, "GET", `/groups/${groupId}/events`).catch(() => ({}));
    const found = list.find((e) => e.title === title);
    return found ? { id: found.eventId ?? found.id } : (await api(seedPage, "POST", `/groups/${groupId}/events`, { title, ...body })).event;
  };
  const day = (n) => new Date(now.getTime() + n * 864e5).toISOString().slice(0, 10);
  const romans = await datesOf(event.id);
  const moved = romans[1];
  if (moved.startsLocal.slice(11, 16) !== "20:00") await saveDate(seedPage, event.id, moved.date, { startsLocal: `${moved.date}T20:00` });
  await as("member", (pg) => api(pg, "PUT", `/events/${event.id}/registrations`, { date: moved.date, status: "going", attendanceMode: "in_person", guestCount: 0 }));
  const full = await eventNamed(group.id, "Full supper", { startsLocal: `${day(11)}T18:00`, durationMinutes: 60, tzid: "America/Boise", location: "Fellowship Hall", capacity: 1, waitlistEnabled: true });
  const fullDate = (await datesOf(full.id))[0].date;
  await as("leader", (pg) => api(pg, "PUT", `/events/${full.id}/registrations`, { date: fullDate, status: "going", attendanceMode: "in_person", guestCount: 0 }));
  await as("member", (pg) => api(pg, "PUT", `/events/${full.id}/registrations`, { date: fullDate, status: "going", attendanceMode: "in_person", guestCount: 0 }));
  const invited = await eventNamed(longGroup.id, "Invited evening", { startsLocal: `${day(12)}T18:30`, durationMinutes: 60, tzid: "America/Boise", location: "North Campus parlor", recurrenceRule: "FREQ=WEEKLY;COUNT=2", remoteAvailable: true });
  const invitedDates = await datesOf(invited.id);
  if (!invitedDates[0].attendees?.some((a) => a.userId === userId("member"))) {
    const { people: found = [] } = await as("leader", (pg) => api(pg, "GET", `/people/search?q=${encodeURIComponent(people.member[1])}`));
    const selection = found.find((x) => x.displayName === people.member[1]);
    const { request: ask } = await as("leader", (pg) => api(pg, "POST", "/add-requests", { selectionId: selection.selectionId, contextType: "event", contextId: invited.id, occurrenceId: invitedDates[0].id }));
    await as("member", (pg) => api(pg, "POST", `/add-requests/${ask.id}/decision`, { party: "subject", decision: "approved" }));
  }
  const vespers = await eventNamed(group.id, "Cancelled vespers", { startsLocal: `${day(13)}T19:00`, durationMinutes: 60, tzid: "America/Boise", location: "Chapel", recurrenceRule: "FREQ=WEEKLY;COUNT=2" });
  // The event's date list leaves cancelled dates out: with both listed, the first is cancelled now; with
  // one, the first was cancelled a week before it on an earlier run (the manager's read confirms it).
  const vespersDates = await datesOf(vespers.id);
  const cancelled = vespersDates.length === 2 ? vespersDates[0] : { date: new Date(Date.parse(vespersDates[0].date) - 7 * 864e5).toISOString().slice(0, 10) };
  if (vespersDates.length === 2) {
    await as("member", (pg) => api(pg, "PUT", `/events/${vespers.id}/registrations`, { date: cancelled.date, status: "going", attendanceMode: "in_person", guestCount: 0 }));
    await api(seedPage, "POST", `/events/${vespers.id}/cancel`, { date: cancelled.date });
  }
  if (!(await api(seedPage, "GET", `/events/${vespers.id}/occurrences/${cancelled.date}`)).occurrence?.cancelled) throw new Error("seed: the vespers date is not cancelled");
  seed.dates = { moved: `/events/${event.id}/dates/${moved.date}`, recurring: `/events/${event.id}/dates/${romans[2].date}`, full: `/events/${full.id}/dates/${fullDate}`, invited: `/events/${invited.id}/dates/${invitedDates[0].date}`, cancelled: `/events/${vespers.id}/dates/${cancelled.date}` };
}
await seedPage.context().close();

// ---- Pages to check: [id, role, path, ready locator]
const pages = [
  ["home-signed-out", null, "/", (p) => p.getByText("Tuesday Bible Study").first()],
  ["login", null, "/login", (p) => p.getByRole("heading", { name: "Log in to Fold" })],
  ["join", null, "/join", (p) => p.locator("main")],
  ["home", "member", "/", (p) => p.getByText("Tuesday Bible Study").first()],
  ["my-groups", "member", "/my-groups", (p) => p.getByText("Tuesday Bible Study").first()],
  ["group-member", "member", `/groups/${seed.groupId}`, (p) => p.getByRole("heading", { name: "Tuesday Bible Study" })],
  ["event", "member", `/events/${seed.eventId}`, (p) => p.getByRole("heading", { name: "Romans 8", level: 1 })],
  ["profile", "member", "/profile", (p) => p.getByRole("heading", { name: "Your profile", level: 1, exact: true })],
  ["profile-details", "member", "/profile/details", (p) => p.getByRole("heading", { name: "Profile details", level: 1, exact: true })],
  ["profile-privacy", "member", "/profile/privacy", (p) => p.getByRole("heading", { name: "Privacy", level: 1, exact: true })],
  ["profile-security-email", "member", "/profile/security", (p) => p.getByRole("heading", { name: "Email and password", level: 1, exact: true })],
  ["profile-security-password", "member", "/profile/security?tab=password", (p) => p.getByRole("heading", { name: "Email and password", level: 1, exact: true })],
  ["profile-calendar", "member", "/profile/calendar", (p) => p.getByRole("heading", { name: "Calendar feed", level: 1, exact: true })],
  ["profile-household", "member", "/profile/household", (p) => p.getByRole("heading", { name: "Household", level: 1, exact: true })],
  ["profile-guardian", "member", "/profile/guardian", (p) => p.getByRole("heading", { name: "Parent or guardian link", level: 1, exact: true })],
  ["profile-invite", "member", "/profile/invite", (p) => p.getByRole("heading", { name: "Invite someone", level: 1, exact: true })],
  ["profile-requests", "member", "/profile/requests", (p) => p.getByRole("heading", { name: "Contact and add requests", level: 1, exact: true })],
  ["notifications", "member", "/notifications", (p) => p.getByRole("heading", { name: "Alerts" })],
  ["group-leader", "leader", `/groups/${seed.groupId}`, (p) => p.getByRole("heading", { name: "Tuesday Bible Study" })],
  ["schedule-event", "leader", `/groups/${seed.groupId}/events/new`, (p) => p.getByRole("button", { name: "Change time zone", exact: true })],
  ["messages", "leader", "/messages", (p) => p.getByRole("heading", { name: "Messages", level: 1 })],
  ["compose", "leader", `/messages/new?group=${seed.groupId}`, (p) => p.getByRole("textbox", { name: "Message" })],
  ["compose-long-name", "leader", `/messages/new?group=${seed.longGroupId}`, (p) => p.getByRole("textbox", { name: "Message" })],
  ["admin", "admin", "/admin", (p) => p.getByRole("heading", { name: "Admin" })],
  ["admin-users", "admin", "/admin?tab=users", (p) => p.getByRole("heading", { name: "Admin" })],
  ["new-group", "admin", "/groups/new", (p) => p.getByLabel("Group name")],
  ["church-event", "admin", "/events/new", (p) => p.getByRole("button", { name: "Change time zone", exact: true })],
].filter(([id]) => !only.length || only.includes(id));

const results = []; let hard = 0;
const sessions = {};
for (const [id, role, p, ready] of pages) {
  if (!sessions[role ?? "anon"]) { const pg = await newPage(); if (role) await signIn(pg, role); sessions[role ?? "anon"] = pg; }
  const page = sessions[role ?? "anon"];
  for (const w of widths) {
    await page.setViewportSize({ width: w, height: 900 });
    await page.goto(url + p); await ready(page).waitFor(); await page.waitForLoadState("networkidle");
    const shot = `${id}-${w}.png`;
    await page.screenshot({ path: path.join(dir, shot), fullPage: true, animations: "disabled", caret: "hide" });
    const m = await page.evaluate(measure);
    const { violations } = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"]).analyze();
    const axe = violations.filter((v) => v.impact === "moderate" || v.impact === "serious" || v.impact === "critical").map((v) => `${v.id} (${v.nodes.length})`);
    const fails = [m.overflow > 0 && `horizontal overflow ${m.overflow}px`, m.cutOff.length && `cut off ${m.cutOff.map((c) => `${c.el} "${c.name}"`).join("; ")}`, m.clipped.length && `clipped select ${m.clipped.map((c) => `#${c.id} "${c.label}" ${c.text}>${c.room}px`).join("; ")}`,
      m.under24.length && `targets under 24px: ${m.under24.map((t) => `${t.el} "${t.name}" ${t.w}x${t.h}`).join("; ")}`, axe.length && `axe ${axe.join(", ")}`].filter(Boolean);
    hard += fails.length;
    results.push({ id, role, path: p, width: w, shot, fails, under44: w === 390 ? m.under44 : undefined });
    console.log(`${fails.length ? "FAIL" : "ok  "} ${id}@${w}${fails.length ? "  " + fails.join(" | ") : ""}${w === 390 ? `  (${m.under44.length} controls under 44px)` : ""}`);
  }
}
// ---- Exact-date pages (M8.1): viewport-only captures at the reference sizes, first screen and a real
// mid-scroll, so the image shows what a person sees without scrolling. On the moved date the six answers
// are measured against the clear area between the sticky header and the first of the answer bar, the
// Tabs and the screen edge.
const sizes = { 390: 844, 768: 900, 1280: 900 };
const datePages = [
  ["date-moved", "member", seed.dates.moved, true],
  ["date-recurring", "member", seed.dates.recurring, false],
  ["date-waitlisted", "member", seed.dates.full, false],
  ["date-invited", "member", seed.dates.invited, false],
  ["date-cancelled", "member", seed.dates.cancelled, false],
  ["date-cancelled-denied", "leader", seed.dates.cancelled, false],
  ["date-guest", null, seed.dates.recurring, false],
].filter(([id]) => !only.length || only.includes(id) || only.includes("dates"));
const firstScreen = () => {
  const header = document.querySelector("body header").getBoundingClientRect();
  const bar = document.querySelector('[role="region"][aria-label="Answer this date"]')?.getBoundingClientRect();
  const tabs = document.querySelector('nav[aria-label="Tabs"]')?.getBoundingClientRect();
  const floor = Math.min(innerHeight, bar?.height ? bar.top : Infinity, tabs?.height ? tabs.top : Infinity);
  const answers = ["identity", "when", "where", "standing", "attendance", "change"].map((key) => {
    const el = document.querySelector(`[data-date-answer="${key}"]`);
    if (!el) return { key, missing: true };
    const range = document.createRange(); range.selectNodeContents(el);
    const rects = [...range.getClientRects()].filter((r) => r.width > 0 && r.height > 0);
    const top = Math.round(Math.min(...rects.map((r) => r.top))), bottom = Math.round(Math.max(...rects.map((r) => r.bottom)));
    return { key, text: el.innerText.replace(/\s+/g, " ").trim().slice(0, 120), top, bottom, inside: top >= header.bottom && bottom <= floor };
  });
  return { scrollY, headerBottom: Math.round(header.bottom), floor: Math.round(floor), answers };
};
for (const [id, role, p, sixAnswers] of datePages) {
  const ctx = await browser.newContext({ locale: "en-US", timezoneId: "America/New_York", viewport: { width: 390, height: 844 } });
  const page = await ctx.newPage(); page.setDefaultTimeout(15_000);
  if (role) await signIn(page, role);
  for (const w of widths) {
    await page.setViewportSize({ width: w, height: sizes[w] ?? 900 });
    await page.goto(url + p); await page.getByRole("heading", { level: 1 }).waitFor(); await page.waitForLoadState("networkidle");
    await page.evaluate(() => document.fonts.ready);
    const first = await page.evaluate(firstScreen);
    const shot = `${id}-${w}-first.png`;
    await page.screenshot({ path: path.join(dir, shot), animations: "disabled", caret: "hide" });
    const m = await page.evaluate(measure);
    const { violations } = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"]).analyze();
    const { violations: order } = await new AxeBuilder({ page }).withRules(["heading-order"]).analyze();
    const axe = [...violations, ...order].filter((v) => v.impact === "moderate" || v.impact === "serious" || v.impact === "critical").map((v) => `${v.id} (${v.nodes.length})`);
    const outside = sixAnswers ? first.answers.filter((a) => a.missing || !a.inside).map((a) => a.key) : [];
    const fails = [m.overflow > 0 && `horizontal overflow ${m.overflow}px`, m.cutOff.length && `cut off ${m.cutOff.map((c) => `${c.el} "${c.name}"`).join("; ")}`,
      m.under24.length && `targets under 24px: ${m.under24.map((t) => `${t.el} "${t.name}" ${t.w}x${t.h}`).join("; ")}`, axe.length && `axe ${axe.join(", ")}`,
      first.scrollY !== 0 && `first screen scrolled ${first.scrollY}px`, outside.length && `answers outside the first screen: ${outside.join(", ")}`].filter(Boolean);
    let midShot = null;
    const max = await page.evaluate(() => document.documentElement.scrollHeight - innerHeight);
    if (max > 0) { await page.mouse.wheel(0, Math.min(400, max)); await page.waitForFunction(() => scrollY > 0); midShot = `${id}-${w}-mid.png`; await page.screenshot({ path: path.join(dir, midShot), animations: "disabled", caret: "hide" }); }
    hard += fails.length;
    results.push({ id, role, path: p, width: w, height: sizes[w] ?? 900, zone: "America/New_York", shot, midShot, first, fails, under44: w === 390 ? m.under44 : undefined });
    console.log(`${fails.length ? "FAIL" : "ok  "} ${id}@${w}x${sizes[w] ?? 900}${fails.length ? "  " + fails.join(" | ") : ""}${w === 390 ? `  (${m.under44.length} controls under 44px)` : ""}`);
  }
  await ctx.close();
}

await browser.close();
await writeFile(path.join(dir, "results.json"), JSON.stringify({ url, seed, results }, null, 2));
const summary = results.map((r) => `${r.fails.length ? "FAIL" : "ok"} ${r.id}@${r.width} ${r.shot}${r.fails.length ? " — " + r.fails.join(" | ") : ""}${r.under44 ? ` [${r.under44.length} under 44px at 390]` : ""}`).join("\n");
await writeFile(path.join(dir, "result.txt"), `feature: ui-checks\nentry: verify-ui drive-ui.mjs, pages ${[...pages, ...datePages].map((x) => x[0]).join(",")}, widths ${widths}\nhard failures: ${hard}\n\n${summary}\n`);
process.exit(hard ? 1 : 0);
