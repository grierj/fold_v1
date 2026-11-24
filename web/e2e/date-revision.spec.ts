import * as schema from "valibot";
import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { apiAs, email, PASSWORD, saveDate, signInTo } from "./helpers";

// DEC-164: "Edit this date" saves the way every manager save does. The save names the read the editor opened with
// and the server refuses an older one, writing nothing. The editor reads the date once: when another manager changed
// it, the editor closes on its own and the page says the changes weren't saved, with the line in view and focused;
// when the date shows the save already stored (an earlier press whose answer was lost), it closes as a save does.
const managed = schema.looseObject({ occurrence: schema.looseObject({
  title: schema.string(), location: schema.string(), durationMinutes: schema.number(), startsLocal: schema.string(), capacity: schema.nullable(schema.number()),
}) });

const dateIn = (days: number) => new Date(Date.now() + days * 864e5).toISOString().slice(0, 10);
const fullDay = (d: string) => new Date(`${d}T12:00:00Z`).toLocaleDateString("en-US", { weekday: "long", month: "long", day: "numeric", year: "numeric", timeZone: "UTC" });
const CHANGED = "This date changed while you were editing, so your changes could not be saved. This is the date as it is now.";

const idOf = schema.looseObject({ id: schema.number() });

// A weekly series in a group of its own, led by a leader of its own: the group page lists it alone.
async function newSeries(request: APIRequestContext, title: string) {
  const suffix = Date.now().toString(36);
  const leader = `date-leader-${suffix}`;
  const grier = await apiAs(request, "grier");
  const made = schema.parse(idOf, (await grier("POST", "/admin/users", { ageBand: "adult", name: "Date Leader", email: email(leader), password: PASSWORD, role: "member" })).user);
  const { group } = schema.parse(schema.looseObject({ group: idOf }), await grier("POST", "/groups", { name: `Date revision ${suffix}`, description: "", tags: [], location: "Hall", visibility: "public", memberLimit: null }));
  await grier("POST", `/admin/groups/${group.id}/members`, { userId: made.id, role: "leader" });
  const date = dateIn(9);

  const { event } = schema.parse(schema.looseObject({ event: idOf }), await grier("POST", `/groups/${group.id}/events`, {
    title, startsLocal: `${date}T19:00`, durationMinutes: 60, tzid: "America/Boise", location: "Room 201", recurrenceRule: "FREQ=WEEKLY;COUNT=3",
  }));

  const read = async () => schema.parse(managed, await grier("GET", `/events/${event.id}/occurrences/${date}`)).occurrence;

  return { id: event.id, groupId: group.id, date, title, leader, read };
}

type Series = Awaited<ReturnType<typeof newSeries>>;

// Each host, as a manager reaches "Edit this date" there, and the line it shows after a conflict.
const hosts: Array<{ name: string; open: (page: Page, s: Series) => Promise<void>; line: (s: Series) => string }> = [
  {
    name: "the group page",
    open: async (page, s) => {
      await signInTo(page, s.leader, `/groups/${s.groupId}`);
      await page.getByRole("button", { name: `More for ${s.title} on ${s.date}` }).click();
      await page.getByRole("menuitem", { name: "Edit this date" }).click();
    },
    line: (s) => `${fullDay(s.date)}. ${CHANGED}`,
  },
  {
    name: "the date page",
    open: async (page, s) => {
      await signInTo(page, s.leader, `/events/${s.id}/dates/${s.date}`);
      await page.getByRole("button", { name: "Edit this date" }).click();
    },
    line: () => CHANGED,
  },
  {
    name: "the event page",
    open: async (page, s) => {
      await signInTo(page, s.leader, `/events/${s.id}`);
      await page.getByRole("button", { name: `More for ${s.date}` }).click();
      await page.getByRole("menuitem", { name: "Edit this date" }).click();
    },
    line: (s) => `${fullDay(s.date)}. ${CHANGED}`,
  },
];

test.describe("at 390px", () => {
  test.use({ viewport: { width: 390, height: 844 } });

  for (const host of hosts) {
    test(`on ${host.name}, a save over another manager's change closes the editor and says nothing was saved, in view and focused`, async ({ page, request }) => {
      const series = await newSeries(request, `Two managers ${host.name.split(" ")[1]} ${Date.now().toString(36)}`);
      await host.open(page, series);
      const minutes = page.getByRole("spinbutton", { name: "Minutes" });
      await expect(minutes).toBeVisible();

      // Another manager moves the place while this editor is open.
      await saveDate(await apiAs(request, "grier"), series.id, series.date, { location: "Fellowship hall" });

      await minutes.fill("90");
      await page.getByRole("button", { name: "Save this date" }).click();
      await expect(page.getByRole("button", { name: "Save this date" })).toHaveCount(0);
      const line = page.getByRole("status").filter({ hasText: host.line(series) });
      await expect(line).toBeInViewport();
      await expect(line).toBeFocused();
      expect(await series.read()).toMatchObject({ location: "Fellowship hall", durationMinutes: 60 });
      await expect(page.getByText(/\b(reload|refresh|reopen)\b/i)).toHaveCount(0);

      // Opening the editor again starts from the date as it is now, and clears the line.
      await host.open(page, series);
      await expect(page.getByRole("textbox", { name: /^Location/ })).toHaveValue("Fellowship hall");
      await expect(page.getByText(CHANGED)).toHaveCount(0);
    });
  }
});

// The first save on this page reaches the server and is stored; its answer never reaches the page. Returns the
// statuses of the saves that did answer.
async function dropFirstAnswer(page: Page, s: Series) {
  const saves: number[] = [];
  const path = `/api/v1/events/${s.id}/occurrences/${s.date}/edits`;
  page.on("response", (res) => {
    if (res.request().method() === "POST" && new URL(res.url()).pathname === path) saves.push(res.status());
  });
  let dropped = false;
  await page.route(`**${path}`, async (route) => {
    if (dropped) return route.fallback();
    dropped = true;
    await route.fetch();
    await route.abort();
  });

  return saves;
}

// The editor closed as after a save: once the page's read after it has landed, no line says the changes could not be
// saved (a conflict also closes the editor, and its line comes only with that read).
async function closedAsSaved(page: Page) {
  await expect(page.getByRole("button", { name: "Save this date" })).toHaveCount(0);
  await page.waitForLoadState("networkidle");
  await expect(page.getByRole("button", { name: /^More for / }).first()).toBeVisible();
  await expect(page.getByText(/could not be saved/)).toHaveCount(0);
}

test("pressing Save again after a stored save's answer was lost closes the editor as a save does", async ({ page, request }) => {
  const series = await newSeries(request, `Lost date answer ${Date.now().toString(36)}`);
  await hosts[2].open(page, series);
  await expect(page.getByRole("spinbutton", { name: "Minutes" })).toBeVisible();
  const saves = await dropFirstAnswer(page, series);
  await page.getByRole("textbox", { name: "Title" }).fill(`  ${series.title} night  `);
  await page.getByRole("button", { name: "Save this date" }).click();
  await expect(page.getByText("We couldn't reach Fold. Check your connection and try again.")).toBeVisible();
  await expect(page.getByRole("textbox", { name: "Title" })).toHaveValue(`  ${series.title} night  `);
  expect((await series.read()).title).toBe(`${series.title} night`);

  await page.getByRole("button", { name: "Save this date" }).click();
  await closedAsSaved(page);
  // The second press named the first's read, so it was refused, the date read and found stored.
  expect(saves).toEqual([409]);
  expect((await series.read()).title).toBe(`${series.title} night`);
});

// The read's own values tell a stored change from the event's value: a seat limit, and a start put back on the series'
// clock after the date had a start of its own.
const ownChanges: Array<{ name: string; before?: (date: string) => Record<string, string>; change: (page: Page) => Promise<void>; stored: (date: string) => Record<string, number | string> }> = [
  {
    name: "a seat limit",
    change: (page) => page.getByRole("spinbutton", { name: /^Capacity for this date/ }).fill("12"),
    stored: () => ({ capacity: 12 }),
  },
  {
    name: "a start time back on the series' clock",
    before: (date) => ({ startsLocal: `${date}T20:00` }),
    change: (page) => page.getByLabel("Start time").fill("19:00"),
    stored: (date) => ({ startsLocal: `${date}T19:00` }),
  },
];

for (const c of ownChanges) {
  test(`${c.name} stored with its answer lost: Save again closes the editor as a save does`, async ({ page, request }) => {
    const series = await newSeries(request, `Lost own ${Date.now().toString(36)}`);

    if (c.before) await saveDate(await apiAs(request, "grier"), series.id, series.date, c.before(series.date));
    await hosts[2].open(page, series);
    await expect(page.getByRole("spinbutton", { name: "Minutes" })).toBeVisible();
    const saves = await dropFirstAnswer(page, series);
    await c.change(page);
    await page.getByRole("button", { name: "Save this date" }).click();
    await expect(page.getByText("We couldn't reach Fold. Check your connection and try again.")).toBeVisible();
    expect(await series.read()).toMatchObject(c.stored(series.date));

    await page.getByRole("button", { name: "Save this date" }).click();
    await closedAsSaved(page);
    expect(saves).toEqual([409]);
    expect(await series.read()).toMatchObject(c.stored(series.date));
  });
}
