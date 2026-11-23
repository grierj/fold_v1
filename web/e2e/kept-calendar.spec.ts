import type { APIRequestContext, Page, Response } from "@playwright/test";
import * as schema from "valibot";
import { expect, test } from "@playwright/test";
import { apiAs, checkState, email, PASSWORD, signIn } from "./helpers";

// Flow 6 (DEC-157): a future Week carries a date someone kept after leaving that is underway, and only once it
// has started; Month shows the same row above its grid. The server's clock is the e2e attempt clock
// (attempt_clock_e2e.go, process state), so it is set to 0 before and after the spec, pass or fail.
test.use({ locale: "en-US", timezoneId: "UTC" });

const MINUTE = 60_000;
const DAY = 24 * 60 * MINUTE;
const idOf = schema.looseObject({ id: schema.number() });
const calendarRow = schema.looseObject({ eventId: schema.number(), date: schema.string(), startsAt: schema.string() });

const day = (at: number) => new Date(at).toISOString().slice(0, 10);
const local = (at: number) => new Date(at).toISOString().slice(0, 16);

// The Sunday 00:00 UTC that starts the week holding `at`.
function weekStart(at: number) {
  const d = new Date(at);

  return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate() - d.getUTCDay());
}

async function setClock(request: APIRequestContext, offsetSeconds: number) {
  await (await apiAs(request, "grier"))("POST", "/e2e/attempt-clock", { offsetSeconds });
}

async function calendarAnswer(page: Page, act: () => Promise<void>): Promise<Response> {
  const [response] = await Promise.all([page.waitForResponse((r) => r.url().includes("/api/v1/calendar?") && r.request().method() === "GET"), act()]);

  return response;
}

for (const width of [390, 1280]) {
  test.describe(`at ${width}px`, () => {
    test.use({ viewport: { width, height: 900 } });

    test("a future week carries an underway kept date, only once it has started", async ({ page, request }) => {
      test.skip(process.env.E2E_NO_CLOCK === "1", "E2E_NO_CLOCK=1: this binary has no attempt clock");
      test.setTimeout(150_000);
      await setClock(request, 0);

      try {
        // The boundary guard: start at least 5 minutes after W and 2 hours before W', so the 61-minute move
        // below keeps next week in the future.
        let offset = 0;
        let now = Date.now();
        let w = weekStart(now);

        if (now - w < 5 * MINUTE || w + 7 * DAY - now < 2 * 60 * MINUTE) {
          offset = 125 * 60;
          await setClock(request, offset);
          now += offset * 1000;
          await page.clock.setSystemTime(now);
          w = weekStart(now);
        }

        const next = w + 7 * DAY;
        const suffix = `${width}-${Date.now().toString(36)}`;
        const leaderLogin = `kept-leader-${suffix}`;
        const memberLogin = `kept-member-${suffix}`;
        const admin = await apiAs(request, "grier");
        const leader = schema.parse(idOf, (await admin("POST", "/admin/users", { ageBand: "adult", name: "Kept Leader", email: email(leaderLogin), password: PASSWORD, role: "member" })).user);
        const member = schema.parse(idOf, (await admin("POST", "/admin/users", { ageBand: "adult", name: "Kept Member", email: email(memberLogin), password: PASSWORD, role: "member" })).user);
        const { group } = schema.parse(schema.looseObject({ group: idOf }), await admin("POST", "/groups", { name: `Kept calendar ${suffix}`, description: "Kept dates on the calendar", tags: [], location: "Hall", visibility: "public", memberLimit: null }));
        await admin("POST", `/admin/groups/${group.id}/members`, { userId: leader.id, role: "leader" });
        await admin("POST", `/admin/groups/${group.id}/members`, { userId: member.id, role: "member" });
        // D1 is underway now and began before next week; D2 starts inside the 2-hour cutoff, after seeding.
        const d1Start = w + MINUTE;
        const d2Start = Math.ceil((now + 60 * MINUTE) / MINUTE) * MINUTE;
        const create = async (title: string, start: number, minutes: number) => schema.parse(schema.looseObject({ event: idOf }), await admin("POST", `/groups/${group.id}/events`, { title, startsLocal: local(start), durationMinutes: minutes, tzid: "UTC", visibility: "public", capacity: null, waitlistEnabled: true })).event;
        const d1 = await create(`Retreat week ${suffix}`, d1Start, 10080);
        const d2 = await create(`Late start ${suffix}`, d2Start, (next + MINUTE - d2Start) / MINUTE);
        const memberApi = await apiAs(request, memberLogin);

        for (const [event, start] of [[d1, d1Start], [d2, d2Start]] as const) await memberApi("PUT", `/events/${event.id}/registrations`, { date: day(start), status: "going", attendanceMode: "in_person", people: [] });
        const { departure } = schema.parse(schema.looseObject({ departure: schema.looseObject({ snapshot: schema.string(), dates: schema.array(schema.looseObject({ occurrenceId: schema.number(), defaultKeep: schema.boolean() })) }) }), await memberApi("GET", `/groups/${group.id}/departure-preview`));
        // Departure judges on the wall clock: D1 is underway, and D2 is offered (it is public) and inside the
        // cutoff unless the boundary guard moved the server clock. Both are kept, as their boxes checked.
        expect(departure.dates).toHaveLength(2);
        await memberApi("POST", `/groups/${group.id}/leave`, { snapshot: departure.snapshot, keepOccurrenceIds: departure.dates.map((d) => d.occurrenceId) });
        const d1Url = `/events/${d1.id}/dates/${day(d1Start)}`;
        const d2Url = `/events/${d2.id}/dates/${day(d2Start)}`;
        const underway = page.getByRole("region", { name: "Already underway" });

        // This week: both on their own days, nothing carried.
        await signIn(page, leaderLogin);
        // The group filter keeps out other specs' public dates in this shard's database.
        await page.goto(`/calendar?group=${group.id}`);
        await calendarAnswer(page, () => page.getByRole("navigation", { name: "Calendar view" }).getByRole("link", { name: "Week" }).click());
        await expect(page.getByText("2 events this week")).toBeVisible();
        await expect(underway).toHaveCount(0);
        await checkState(page, "kept-calendar", "this-week");

        // Next week, before D2 starts: only D1 is carried, under its start day.
        const answer = await calendarAnswer(page, () => page.getByRole("button", { name: "Next week" }).click());
        const rows = schema.parse(schema.looseObject({ occurrences: schema.array(calendarRow) }), await answer.json()).occurrences;
        const d1Row = rows.find((r) => r.eventId === d1.id);
        expect(rows.map((r) => r.eventId)).toEqual([d1.id]);
        await expect(underway).toBeVisible();
        await expect(underway.getByRole("heading", { level: 4 })).toHaveCount(1);
        await expect(underway.getByRole("link")).toHaveCount(1);
        await expect(underway.getByRole("link")).toHaveAttribute("href", d1Url);
        await expect(page.getByText("1 event this week")).toBeVisible();
        await expect(page.getByText("No events this week")).toHaveCount(0);
        await expect(page.locator(`a[href="${d2Url}"]`)).toHaveCount(0);
        await checkState(page, "kept-calendar", "next-week-before");

        // Keyboard: Tab from the view switch reaches D1's link inside the section.
        await page.getByRole("navigation", { name: "Calendar view" }).getByRole("link", { name: "Week" }).focus();
        let reached = false;

        for (let i = 0; i < 40 && !reached; i++) {
          await page.keyboard.press("Tab");
          reached = await page.evaluate((href) => document.activeElement?.getAttribute("href") === href, d1Url);
        }

        expect(reached).toBe(true);

        // After D2 starts: both carried, each under its own start day.
        offset += 61 * 60;
        await setClock(request, offset);
        await page.clock.setSystemTime(Date.now() + offset * 1000);
        await signIn(page, leaderLogin);
        await page.goto(`/calendar?view=week&group=${group.id}`);
        await calendarAnswer(page, () => page.getByRole("button", { name: "Next week" }).click());
        await expect(underway.getByRole("link")).toHaveCount(2);
        await expect(underway.locator(`a[href="${d1Url}"]`)).toHaveCount(1);
        await expect(underway.locator(`a[href="${d2Url}"]`)).toHaveCount(1);
        await expect(page.getByText("2 events this week")).toBeVisible();
        await checkState(page, "kept-calendar", "next-week-after");

        // The row opens the date page and its manager tools.
        await underway.locator(`a[href="${d1Url}"]`).click();
        await expect(page.getByRole("heading", { name: "Manage this date" })).toBeVisible();

        // Month: the grid's first day is after D1's start, and the one calendar answer is D1's row as the server
        // returned it (a real-server Month carry needs a grid within a week of the server's clock: N3n).
        await page.goto(`/calendar?view=month&group=${group.id}`);
        await expect(page.getByRole("button", { name: "Next month" })).toBeVisible();
        const today = new Date(Date.now() + offset * 1000);
        let presses = 0;

        for (let m = 0; m < 3; m++) {
          const first = Date.UTC(today.getUTCFullYear(), today.getUTCMonth() + m, 1);
          const gridStart = first - new Date(first).getUTCDay() * DAY;

          if (day(gridStart) > day(d1Start)) { presses = m; break; }
        }

        expect(presses).toBeGreaterThan(0);

        for (let i = 1; i < presses; i++) await calendarAnswer(page, () => page.getByRole("button", { name: "Next month" }).click());
        await page.route("**/api/v1/calendar?**", (route) => route.fulfill({ json: { occurrences: [d1Row], truncated: false } }), { times: 1 });
        await calendarAnswer(page, () => page.getByRole("button", { name: "Next month" }).click());
        await expect(underway.locator(`a[href="${d1Url}"]`)).toHaveCount(1);
        await expect(underway.getByRole("heading", { level: 4 })).toHaveCount(1);
        await expect(page.getByText("Started before this month and still going.")).toBeVisible();
        await expect(page.getByText("No events this month.")).toHaveCount(0);
        await expect(page.getByText("Nothing on this day.")).toBeVisible();
        await checkState(page, "kept-calendar", "month");
      } finally {
        await setClock(request, 0);
      }
    });
  });
}
