import type { APIRequestContext, Browser, Locator, Page } from "@playwright/test";
import * as schema from "valibot";
import { expect, test } from "@playwright/test";
import { apiAs, checkState, email, PASSWORD, saveDate, signInTo } from "./helpers";

// DEC-156: a date someone kept after leaving can't end later until a manager removes them, which the
// kept-dates panel does in place, under the editor, without a reload. Each manager flow runs in its own fresh
// browser context on this shard's fresh database.
test.use({ locale: "en-US", timezoneId: "UTC" });

const DAY = 86_400_000;
const idOf = schema.looseObject({ id: schema.number() });
const managedAnswer = schema.looseObject({ occurrence: schema.looseObject({ durationMinutes: schema.number(), title: schema.string(), attendees: schema.optional(schema.array(schema.looseObject({ keptAfterLeaving: schema.optional(schema.boolean()) }))) }) });
const memberAnswer = schema.looseObject({ occurrence: schema.looseObject({ myRegistration: schema.nullable(schema.string()) }) });

const ERROR_COPY = "Someone who left the group kept this date, so it can't end later yet.";
const NOBODY_ONE = "Nobody is keeping this date now. Save again to apply your change.";
const SAVE_LINE = "Your change wasn't saved: when you saved, someone who left the group had kept at least one of these dates.";
const COUNT = /\d+ (person|people) kept/;
const MEMBER = "Kept Member";

const local = (days: number, minutes = 0) => new Date(Date.now() + days * DAY + minutes * 60_000).toISOString().slice(0, 16);
const utc = (d: string, options: Intl.DateTimeFormatOptions) => new Intl.DateTimeFormat("en-US", { timeZone: "UTC", ...options }).format(new Date(`${d}T12:00:00Z`));

// lib/when.ts's formatDayFull, formatDayLong and the month title, in en-US and UTC as this spec pins them.
const fullDay = (d: string) => utc(d, { weekday: "long", month: "long", day: "numeric", year: "numeric" });
const longDay = (d: string) => utc(d, { weekday: "long", month: "long", day: "numeric" });
const monthTitle = (d: string) => utc(d, { month: "long", year: "numeric" });
const dayHeading = (d: string) => utc(d, { weekday: "long", month: "short", day: "numeric" });

const panelOf = (page: Page) => page.getByRole("region", { name: "Sign-ups to check before you save" });

declare global {
  interface Window {
    // Set before the first Save: still 1 at the end proves the page never reloaded.
    __keptFlow?: number;
  }
}

const marker = (page: Page) => page.evaluate(() => window.__keptFlow);
const setMarker = (page: Page) => page.evaluate(() => { window.__keptFlow = 1; });

interface Seeded {
  suffix: string;
  groupId: number;
  memberLogin: string;
}

// A group created by "grier" (its first leader) with one member, plus whatever `extra` accounts the flow needs.
async function seedGroup(request: APIRequestContext, name: string, extra: Array<{ login: string; role: "admin" | "member"; leader?: boolean }> = []): Promise<Seeded> {
  const suffix = `${name}-${Date.now().toString(36)}`;
  const memberLogin = `kept-member-${suffix}`;
  const admin = await apiAs(request, "grier");
  const member = schema.parse(idOf, (await admin("POST", "/admin/users", { ageBand: "adult", name: MEMBER, email: email(memberLogin), password: PASSWORD, role: "member" })).user);
  const { group } = schema.parse(schema.looseObject({ group: idOf }), await admin("POST", "/groups", { name: `Kept dates ${suffix}`, description: "Kept after leaving", tags: [], location: "Hall", visibility: "public", memberLimit: null }));
  await admin("POST", `/admin/groups/${group.id}/members`, { userId: member.id, role: "member" });

  for (const account of extra) {
    const made = schema.parse(idOf, (await admin("POST", "/admin/users", { ageBand: "adult", name: account.role === "admin" ? "Kept Admin" : "Kept Leader", email: email(account.login), password: PASSWORD, role: account.role })).user);

    if (account.leader) await admin("POST", `/admin/groups/${group.id}/members`, { userId: made.id, role: "leader" });
  }

  return { suffix, groupId: group.id, memberLogin };
}

async function createEvent(request: APIRequestContext, groupId: number, body: { title: string; startsLocal: string; durationMinutes: number; visibility: "public" | "invite_only"; recurrenceRule?: string }) {
  const admin = await apiAs(request, "grier");

  return schema.parse(schema.looseObject({ event: idOf }), await admin("POST", `/groups/${groupId}/events`, { tzid: "UTC", capacity: null, waitlistEnabled: true, ...body })).event;
}

async function going(request: APIRequestContext, login: string, eventId: number, dates: string[]) {
  const member = await apiAs(request, login);

  for (const date of dates) await member("PUT", `/events/${eventId}/registrations`, { date, status: "going", attendanceMode: "in_person", people: [] });
}

// The member leaves through the departure dialog, in a context of their own, keeping every date it offers.
async function leaveKeeping(browser: Browser, login: string, groupId: number) {
  const context = await browser.newContext({ baseURL: test.info().project.use.baseURL, locale: "en-US", timezoneId: "UTC" });
  const page = await context.newPage();
  await signInTo(page, login, `/groups/${groupId}`);
  await page.getByRole("button", { name: "You're a member" }).click();
  await page.getByRole("menuitem", { name: "Leave group" }).click();
  const dialog = page.getByRole("dialog");
  const boxes = dialog.getByRole("checkbox", { name: /^Keep / });
  await expect(boxes.first()).toBeVisible();

  for (const box of await boxes.all()) await box.check();
  await dialog.getByRole("button", { name: "Leave group", exact: true }).click();
  await expect(page.getByText(/^[1-9]\d* dates? kept · \d+ dates? ended\.$/)).toBeVisible();
  await context.close();
}

async function setMinutes(page: Page, from: number, to: number) {
  const minutes = page.getByRole("spinbutton", { name: "Minutes" });
  await expect(minutes).toHaveValue(String(from));
  await minutes.fill(String(to));
}

// The panel's read answered for one refused date: the ERROR_COPY line and the date's own section.
async function expectOneDatePanel(page: Page, date: string) {
  const panel = panelOf(page);
  await expect(panel.getByText(ERROR_COPY)).toBeVisible();
  await expect(panel.getByRole("heading", { name: fullDay(date) })).toBeVisible();
  await expect(panel.getByText("Kept after leaving", { exact: true })).toBeVisible();
  expect(await page.locator("body").innerText()).not.toMatch(COUNT);
}

async function removeInPanel(page: Page, scope: Locator = panelOf(page)) {
  await scope.getByRole("button", { name: `More for ${MEMBER}` }).click();
  await page.getByRole("menuitem", { name: `Remove ${MEMBER}` }).click();
  const question = page.getByRole("dialog", { name: `Remove ${MEMBER}'s registration for this date?` });
  await expect(question).toContainText("They left the group and kept this date. Removing them ends that and tells them.");
  await expect(question).not.toContainText("frees their place");
  await question.getByRole("button", { name: "Remove" }).click();
}

async function managedRead(request: APIRequestContext, login: string, eventId: number, date: string) {
  return schema.parse(managedAnswer, await (await apiAs(request, login))("GET", `/events/${eventId}/occurrences/${date}`)).occurrence;
}

async function memberRead(request: APIRequestContext, login: string, eventId: number, date: string) {
  const member = await apiAs(request, login);

  return schema.parse(memberAnswer, await member("GET", `/events/${eventId}/dates/${date}`)).occurrence;
}

function failOnNativeDialogs(page: Page) {
  page.on("dialog", (dialog) => { throw new Error(`native ${dialog.type()} on a page that asks in the app's dialog`); });
}

// From the date page: Manage this date → Edit this date, a longer date, Save, and the panel's refusal.
async function refuseOnDatePage(page: Page, date: string, from: number, to: number) {
  await expect(page.getByRole("heading", { name: "Manage this date" })).toBeVisible();
  await page.getByRole("button", { name: "Edit this date" }).click();
  await setMinutes(page, from, to);
  await setMarker(page);
  await page.getByRole("button", { name: "Save this date" }).click();
  await expectOneDatePanel(page, date);
  await expect(page.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(String(to));
}

// Every host closes its date editor after a save (DEC-164).
async function removeAndSave(page: Page, to: number) {
  await removeInPanel(page);
  await expect(panelOf(page).getByText(NOBODY_ONE)).toBeVisible();
  await expect(page.getByText(ERROR_COPY)).toHaveCount(0);
  await expect(page.getByRole("spinbutton", { name: "Minutes" })).toHaveValue(String(to));
  expect(await marker(page)).toBe(1);
  await page.getByRole("button", { name: "Save this date" }).click();
  await expect(panelOf(page)).toHaveCount(0);
  await expect(page.getByRole("spinbutton", { name: "Minutes" })).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(0);
  expect(await marker(page)).toBe(1);
}

test("flow 1: an admin who is neither member nor host clears a public underway kept date from the calendar", async ({ page, request, browser }) => {
  test.setTimeout(120_000);
  failOnNativeDialogs(page);
  const adminLogin = `kept-admin-${Date.now().toString(36)}`;
  const { groupId, memberLogin, suffix } = await seedGroup(request, "f1", [{ login: adminLogin, role: "admin" }]);
  const startsLocal = local(0, -30);
  const date = startsLocal.slice(0, 10);
  const event = await createEvent(request, groupId, { title: `Underway supper ${suffix}`, startsLocal, durationMinutes: 120, visibility: "public" });
  await going(request, memberLogin, event.id, [date]);
  await leaveKeeping(browser, memberLogin, groupId);

  await signInTo(page, adminLogin, "/calendar");
  const row = page.locator(`a[href="/events/${event.id}/dates/${date}"]`);
  await expect(row).toHaveCount(1);
  await row.click();
  await refuseOnDatePage(page, date, 120, 150);
  await removeAndSave(page, 150);

  const saved = await managedRead(request, adminLogin, event.id, date);
  expect(saved.durationMinutes).toBe(150);
  expect((saved.attendees ?? []).some((a) => a.keptAfterLeaving === true)).toBe(false);
  expect(await marker(page)).toBe(1);
  await page.goto("/calendar");
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await expect(page.locator(`a[href="/events/${event.id}/dates/${date}"]`)).toHaveCount(0);
  expect((await memberRead(request, memberLogin, event.id, date)).myRegistration).toBe("removed");
});

test("flow 2: an admin reaches a customized public kept date 121 days ahead through the Month view", async ({ page, request, browser }) => {
  test.setTimeout(150_000);
  failOnNativeDialogs(page);
  const adminLogin = `kept-admin-${Date.now().toString(36)}`;
  const { groupId, memberLogin, suffix } = await seedGroup(request, "f2", [{ login: adminLogin, role: "admin" }]);
  const event = await createEvent(request, groupId, { title: `Weekly supper ${suffix}`, startsLocal: local(2), durationMinutes: 60, visibility: "public", recurrenceRule: "FREQ=WEEKLY" });
  const date = local(121).slice(0, 10);
  await saveDate(await apiAs(request, "grier"), event.id, date, { title: "Harvest night" });
  await going(request, memberLogin, event.id, [date]);
  await leaveKeeping(browser, memberLogin, groupId);

  await signInTo(page, adminLogin, "/calendar");
  await page.getByRole("navigation", { name: "Calendar view" }).getByRole("link", { name: "Month" }).click();
  const now = new Date();
  const target = new Date(`${date}T12:00:00Z`);
  const presses = (target.getUTCFullYear() - now.getUTCFullYear()) * 12 + target.getUTCMonth() - now.getUTCMonth();

  for (let i = 0; i < presses; i++) await page.getByRole("button", { name: "Next month" }).click();
  await expect(page.getByRole("heading", { name: monthTitle(date) })).toBeVisible();
  await page.getByRole("button", { name: new RegExp(`^${longDay(date)},`) }).first().click();
  const url = `/events/${event.id}/dates/${date}`;
  const daySection = page.locator("section").filter({ has: page.getByRole("heading", { level: 2, name: dayHeading(date) }) });
  const row = daySection.locator(`a[href="${url}"]`);
  await expect(row).toHaveCount(1);
  await expect(row).toContainText("Harvest night");
  // DEC-77 still keeps the series' other dates out of this month for the admin.
  await expect(page.locator(`a[href^="/events/${event.id}"]:not([href="${url}"])`)).toHaveCount(0);
  await row.click();
  await refuseOnDatePage(page, date, 60, 90);

  // A window focus rechecks the page: the editor waits behind the status line and comes back with the draft.
  let release!: () => void;
  const held = new Promise<void>((resolve) => { release = resolve; });
  await page.route(`**/api/v1/events/${event.id}/dates/${date}`, async (route) => { await held; await route.continue(); }, { times: 1 });
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect(page.getByText("Checking that you can still manage this date…")).toBeVisible();
  await expect(page.getByRole("spinbutton", { name: "Minutes" })).toHaveCount(0);
  await expect(panelOf(page)).toHaveCount(0);
  release();
  await expect(page.getByRole("spinbutton", { name: "Minutes" })).toHaveValue("90");
  await expect(panelOf(page).getByText(ERROR_COPY)).toBeVisible();
  expect(await marker(page)).toBe(1);

  await removeAndSave(page, 90);
  const saved = await managedRead(request, adminLogin, event.id, date);
  expect(saved.durationMinutes).toBe(90);
  expect(saved.title).toBe("Harvest night");
  expect((await memberRead(request, memberLogin, event.id, date)).myRegistration).toBe("removed");
});

test("flow 3: a leader clears a two-day invite-only kept date that began yesterday", async ({ page, request, browser }) => {
  test.setTimeout(120_000);
  failOnNativeDialogs(page);
  const leaderLogin = `kept-leader-${Date.now().toString(36)}`;
  const { groupId, memberLogin, suffix } = await seedGroup(request, "f3", [{ login: leaderLogin, role: "member", leader: true }]);
  const midnight = new Date(new Date().toISOString().slice(0, 10) + "T00:00:00Z").getTime();
  const startsLocal = new Date(midnight - 12 * 3_600_000).toISOString().slice(0, 16);
  const date = startsLocal.slice(0, 10);
  const event = await createEvent(request, groupId, { title: `Two-day retreat ${suffix}`, startsLocal, durationMinutes: 2880, visibility: "invite_only" });
  await going(request, memberLogin, event.id, [date]);
  await leaveKeeping(browser, memberLogin, groupId);

  await signInTo(page, leaderLogin, "/calendar");
  const row = page.locator(`a[href="/events/${event.id}/dates/${date}"]`);
  await expect(row).toHaveCount(1);
  // Carried in under its own start day, above today's.
  const heading = row.locator("xpath=ancestor::section[1]").locator("h2, h4").first();
  await expect(heading).toHaveText(dayHeading(date));
  const today = page.getByRole("heading", { name: "Today", exact: true });

  if (await today.count() > 0) {
    const order = await row.evaluate((link, todayText) => {
      const todayHeading = [...document.querySelectorAll("h2, h4")].find((h) => h.textContent === todayText);

      return todayHeading ? link.compareDocumentPosition(todayHeading) & Node.DOCUMENT_POSITION_FOLLOWING : 1;
    }, "Today");

    expect(order).toBeTruthy();
  }

  await row.click();
  await refuseOnDatePage(page, date, 2880, 2940);
  await removeAndSave(page, 2940);
  // Signed in as the member on this request context: Remove ended their access to the invite-only date.
  await apiAs(request, memberLogin);
  expect((await request.get(`/api/v1/events/${event.id}/dates/${date}`)).status()).toBe(404);
});

test("flow 4: inside the cutoff, the event page and the group page each clear a kept date in place", async ({ page, request, browser }) => {
  test.setTimeout(150_000);
  const leaderLogin = `kept-leader-${Date.now().toString(36)}`;
  const { groupId, memberLogin, suffix } = await seedGroup(request, "f4", [{ login: leaderLogin, role: "member", leader: true }]);
  const startsLocal = local(0, 90);
  const date = startsLocal.slice(0, 10);
  const one = await createEvent(request, groupId, { title: `Window one ${suffix}`, startsLocal, durationMinutes: 60, visibility: "invite_only" });
  const two = await createEvent(request, groupId, { title: `Window two ${suffix}`, startsLocal, durationMinutes: 60, visibility: "invite_only" });
  await going(request, memberLogin, one.id, [date]);
  await going(request, memberLogin, two.id, [date]);
  await leaveKeeping(browser, memberLogin, groupId);
  failOnNativeDialogs(page);

  // The event page lists date 1, so the calendar row still links the event page.
  await signInTo(page, leaderLogin, "/calendar");
  const row = page.getByRole("link", { name: new RegExp(`Window one ${suffix}`) });
  await expect(row).toHaveAttribute("href", `/events/${one.id}`);
  await row.click();
  await page.getByRole("button", { name: `More for ${date}` }).click();
  await page.getByRole("menuitem", { name: "Edit this date" }).click();
  await setMinutes(page, 60, 90);
  await setMarker(page);
  await page.getByRole("button", { name: "Save this date" }).click();
  await expectOneDatePanel(page, date);
  await removeAndSave(page, 90);

  // On the group page the panel's provider asks its questions (the date editor has its own). A window focus rechecks the panel.
  await page.goto(`/groups/${groupId}`);
  await page.getByRole("article", { name: `Window two ${suffix}`, exact: true })
    .getByRole("button", { name: /^More for / }).click();
  await page.getByRole("menuitem", { name: "Edit this date" }).click();
  await setMinutes(page, 60, 90);
  await setMarker(page);
  await page.getByRole("button", { name: "Save this date" }).click();
  await expectOneDatePanel(page, date);
  let release!: () => void;
  const held = new Promise<void>((resolve) => { release = resolve; });
  await page.route(`**/api/v1/events/${two.id}/occurrences/${date}`, async (route) => {
    if (route.request().method() !== "GET") return route.continue();
    await held;
    await route.continue();
  }, { times: 1 });
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  const panel = panelOf(page);
  await expect(panel.getByText("Your change isn't saved yet. Checking that you can still manage sign-ups…")).toBeVisible();
  await expect(panel.getByText(ERROR_COPY)).toHaveCount(0);
  await expect(panel.getByRole("heading", { name: fullDay(date) })).toHaveCount(0);
  await expect(panel.getByText(MEMBER)).toHaveCount(0);
  release();
  await expectOneDatePanel(page, date);
  await expect(page.getByRole("spinbutton", { name: "Minutes" })).toHaveValue("90");
  expect(await marker(page)).toBe(1);
  await removeAndSave(page, 90);
  expect((await managedRead(request, leaderLogin, two.id, date)).durationMinutes).toBe(90);
  expect((await managedRead(request, leaderLogin, one.id, date)).durationMinutes).toBe(90);
});

// B2q with DEC-156 and DEC-158 on the group page: a longer date that someone kept after leaving, made
// invite-only (the group's members alone) in the same save. The writer refuses the later end first, so the panel shows counts and nobody is asked
// anything; Remove promotes the person waiting, once; the next Save asks the writer's own question in the app's
// dialog, counting the one person the new value shuts out, and only Yes cancels them.
test("flow 6: a kept date made longer and invite-only on the group page asks only after the Remove, in the app's dialog", async ({ page, request, browser }) => {
  test.setTimeout(150_000);
  const leaderLogin = `kept-leader-f6-${Date.now().toString(36)}`;
  const { groupId, memberLogin, suffix } = await seedGroup(request, "f6", [{ login: leaderLogin, role: "member", leader: true }]);
  const admin = await apiAs(request, "grier");
  const outsiderLogin = `kept-outsider-${suffix}`;
  const waitingLogin = `kept-waiting-${suffix}`;
  await admin("POST", "/admin/users", { ageBand: "adult", name: "Kept Outsider", email: email(outsiderLogin), password: PASSWORD, role: "member" });
  const waiting = schema.parse(idOf, (await admin("POST", "/admin/users", { ageBand: "adult", name: "Kept Waiting", email: email(waitingLogin), password: PASSWORD, role: "member" })).user);
  await admin("POST", `/admin/groups/${groupId}/members`, { userId: waiting.id, role: "member" });
  const startsLocal = `${local(5).slice(0, 10)}T18:00`;
  const date = startsLocal.slice(0, 10);
  const { event } = schema.parse(schema.looseObject({ event: idOf }), await admin("POST", `/groups/${groupId}/events`, { tzid: "UTC", capacity: 2, waitlistEnabled: true, title: `Members supper ${suffix}`, startsLocal, durationMinutes: 60, visibility: "public" }));
  await going(request, memberLogin, event.id, [date]);
  await going(request, outsiderLogin, event.id, [date]);
  await going(request, waitingLogin, event.id, [date]);
  expect((await memberRead(request, waitingLogin, event.id, date)).myRegistration).toBe("waitlisted");
  await leaveKeeping(browser, memberLogin, groupId);
  failOnNativeDialogs(page);
  const question = page.getByRole("dialog", { name: "Save this date?" });

  await signInTo(page, leaderLogin, `/groups/${groupId}`);
  await page.getByRole("article", { name: `Members supper ${suffix}`, exact: true })
    .getByRole("button", { name: /^More for / }).click();
  await page.getByRole("menuitem", { name: "Edit this date" }).click();
  await setMinutes(page, 60, 90);
  await page.getByRole("radio", { name: /^Invite only/ }).click();
  await setMarker(page);
  await page.getByRole("button", { name: "Save this date" }).click();
  // DEC-156 first: counts only, and no question before the kept person is dealt with.
  await expectOneDatePanel(page, date);
  await expect(question).toHaveCount(0);
  await removeInPanel(page);
  await expect(panelOf(page).getByText(NOBODY_ONE)).toBeVisible();
  await expect.poll(async () => (await memberRead(request, waitingLogin, event.id, date)).myRegistration).toBe("going");
  await expect(page.getByRole("spinbutton", { name: "Minutes" })).toHaveValue("90");
  await expect(page.getByRole("radio", { name: /^Invite only/ })).toBeChecked();

  // The deliberate Save asks the writer's question for the selection it has now: one person, shut out.
  await page.getByRole("button", { name: "Save this date" }).click();
  await expect(question).toBeVisible();
  await expect(question).toHaveAccessibleDescription(`${fullDay(date)}. This date is public now. Saving makes it invite-only, so fewer people will be able to see it. This cancels and notifies 1 registered person who can no longer see this date.`);
  await expect(question.getByRole("button", { name: "Save and notify" })).toBeFocused();
  await checkState(page, "kept-dates", "group-date-question");
  await question.getByRole("button", { name: "Save and notify" }).click();
  await expect(question).toHaveCount(0);
  await expect(panelOf(page)).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect.poll(async () => (await managedRead(request, leaderLogin, event.id, date)).durationMinutes).toBe(90);
  expect(await marker(page)).toBe(1);

  const titles = async (login: string) => schema.parse(schema.looseObject({ notifications: schema.array(schema.looseObject({ title: schema.string() })) }), await (await apiAs(request, login))("GET", "/notifications")).notifications.map((n) => n.title);
  expect((await titles(waitingLogin)).filter((t) => t.startsWith("You're in: "))).toHaveLength(1);
  await expect.poll(async () => (await titles(outsiderLogin)).filter((t) => t.startsWith("Registration cancelled: "))).toHaveLength(1);
  expect((await memberRead(request, waitingLogin, event.id, date)).myRegistration).toBe("going");
});

for (const width of [390, 1280]) {
  test.describe(`flow 5 at ${width}px`, () => {
    test.use({ viewport: { width, height: 900 } });

    test("two kept series dates, switched by keyboard, keep focus in the panel", async ({ page, request, browser }) => {
      test.setTimeout(150_000);
      failOnNativeDialogs(page);
      const leaderLogin = `kept-leader-${width}-${Date.now().toString(36)}`;
      const { groupId, memberLogin, suffix } = await seedGroup(request, `f5-${width}`, [{ login: leaderLogin, role: "member", leader: true }]);
      const startsLocal = `${local(2).slice(0, 10)}T18:00`;
      const event = await createEvent(request, groupId, { title: `Series supper ${suffix}`, startsLocal, durationMinutes: 60, visibility: "public", recurrenceRule: "FREQ=WEEKLY" });
      const first = new Date(Date.parse(`${startsLocal}:00Z`) + 119 * DAY).toISOString().slice(0, 10);
      const second = new Date(Date.parse(`${startsLocal}:00Z`) + 126 * DAY).toISOString().slice(0, 10);
      await going(request, memberLogin, event.id, [first, second]);
      await leaveKeeping(browser, memberLogin, groupId);

      await signInTo(page, leaderLogin, `/events/${event.id}`);
      await page.getByRole("button", { name: "Edit series" }).click();
      await page.getByLabel("Start time").fill("19:00");
      await setMarker(page);
      await page.getByRole("button", { name: "Save series" }).click();
      const panel = panelOf(page);
      await expect(panel.getByText(SAVE_LINE)).toBeVisible();
      await expect(panel.getByRole("heading", { name: fullDay(first) })).toBeVisible();
      await expect(panel.getByRole("heading", { name: fullDay(second) })).toBeVisible();
      await expect(panel.getByText(ERROR_COPY)).toHaveCount(0);
      expect(await page.locator("body").innerText()).not.toMatch(COUNT);

      // Keyboard only: Tab to the second date's Manage sign-ups and press Enter, with its read held.
      let release!: () => void;
      const held = new Promise<void>((resolve) => { release = resolve; });
      await page.route(`**/api/v1/events/${event.id}/occurrences/${second}`, async (route) => { await held; await route.continue(); }, { times: 1 });
      await panel.getByRole("button", { name: `More for ${MEMBER}` }).focus();
      let reached = false;

      for (let i = 0; i < 40 && !reached; i++) {
        await page.keyboard.press("Tab");
        reached = await page.evaluate(() => document.activeElement?.textContent === "Manage sign-ups" && document.activeElement.tagName === "BUTTON");
      }

      expect(reached).toBe(true);
      await page.keyboard.press("Enter");
      const focusIsHome = () => page.evaluate(() => document.activeElement?.getAttribute("aria-label") === "Sign-ups to check before you save");
      expect(await focusIsHome()).toBe(true);
      await expect(panel.getByText("Your change isn't saved yet. Checking that you can still manage sign-ups…")).toBeVisible();
      expect(await page.evaluate(() => document.activeElement?.matches(":focus-visible"))).toBe(true);
      expect(await page.evaluate(() => document.activeElement ? getComputedStyle(document.activeElement).outlineStyle : "none")).not.toBe("none");
      release();
      await expect(panel.getByRole("button", { name: `More for ${MEMBER}` })).toBeVisible();
      expect(await focusIsHome()).toBe(true);
      await page.keyboard.press("Tab");
      expect(await panel.getByRole("region", { name: "Manage sign-ups" }).evaluate((roster) => roster.contains(document.activeElement))).toBe(true);
      await checkState(page, "kept-dates", "series-switched");

      // Remove on both dates, then the unchanged save applies.
      await removeInPanel(page);
      await expect(panel.getByText("Nobody is keeping this date now.", { exact: true })).toBeVisible();
      await panel.getByRole("button", { name: "Manage sign-ups" }).click();
      await removeInPanel(page);
      await expect(panel.getByText("Nobody is keeping this date now.", { exact: true })).toBeVisible();
      await expect(panel.getByText(SAVE_LINE)).toBeVisible();
      await page.getByRole("button", { name: "Save series" }).click();
      await expect(panel).toHaveCount(0);
      await expect(page.getByRole("button", { name: "Save series" })).toHaveCount(0);
      expect(await marker(page)).toBe(1);
      await page.getByRole("button", { name: "Edit series" }).click();
      await expect(page.getByLabel("Start time")).toHaveValue("19:00");
    });
  });
}
