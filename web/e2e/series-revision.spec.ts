import * as schema from "valibot";
import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { apiAs, pickRoom, signIn } from "./helpers";

// DEC-162: a series save names the read it was made from, and a save made from an older read is refused with nothing
// written. The editor checks the event once: when nothing it shows was changed it saves again by itself; when
// something was, it closes on its own (DEC-161) and the page says the changes weren't saved; when the read shows the
// save already stored (an earlier press whose answer was lost), it closes as a save does (DEC-163). Clearing a seat
// limit saves (#445).
const stored = schema.looseObject({ event: schema.looseObject({
  title: schema.string(), location: schema.string(), capacity: schema.nullable(schema.number()),
}) });

const NOTICE = "This event changed while you were editing, so your changes weren't saved. This is the event as it is now.";

const dateIn = (days: number) => new Date(Date.now() + days * 864e5).toISOString().slice(0, 10);

async function newSeries(request: APIRequestContext, title: string, extra: Record<string, number | string> = {}) {
  const grier = await apiAs(request, "grier");
  const { groups } = schema.parse(schema.looseObject({ groups: schema.array(schema.looseObject({ id: schema.number(), name: schema.string() })) }), await grier("GET", "/groups"));
  const room = groups.find((g) => g.name === "Room 201 class")!;
  const lee = await apiAs(request, "lee");

  const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), await lee("POST", `/groups/${room.id}/events`, {
    title, startsLocal: `${dateIn(9)}T19:00`, durationMinutes: 60, tzid: "America/Boise", location: "Room 201", recurrenceRule: "FREQ=WEEKLY;COUNT=3", ...extra,
  }));

  const read = async () => schema.parse(stored, await lee("GET", `/events/${event.id}`)).event;

  return { id: event.id, read };
}

async function openEditor(page: Page, eventId: number) {
  await page.goto(`/events/${eventId}?edit=series`);
  await expect(page.getByRole("button", { name: "Save series" })).toBeVisible();
}

// The answers to this event's series saves, in order.
function seriesSaves(page: Page, eventId: number) {
  const statuses: number[] = [];
  page.on("response", (res) => {
    if (res.request().method() === "PUT" && new URL(res.url()).pathname === `/api/v1/events/${eventId}`) statuses.push(res.status());
  });

  return statuses;
}

for (const width of [390, 1280]) {
  test(`a second manager's save over another's change closes the editor and says nothing was saved, at ${width}px`, async ({ page, browser, request }) => {
    const series = await newSeries(request, `Two editors ${width}`);
    await signIn(page, "lee");
    await openEditor(page, series.id);
    const other = await browser.newContext({ baseURL: test.info().project.use.baseURL, viewport: { width, height: 844 } });
    const second = await other.newPage();
    await signIn(second, "grier");
    // Through the group page's "Manage sign-ups" link: the roster it opens moves the page to itself on every read.
    await second.goto(`/events/${series.id}?manage=${dateIn(9)}&edit=series`);
    await expect(second.getByRole("button", { name: "Save series" })).toBeVisible();
    await expect(second.getByRole("region", { name: "Manage sign-ups" })).toBeVisible();

    await page.getByRole("textbox", { name: "Title" }).fill(`Two editors supper ${width}`);
    await page.getByRole("button", { name: "Save series" }).click();
    await expect(page.getByRole("button", { name: "Save series" })).toHaveCount(0);
    await expect.poll(async () => (await series.read()).title).toBe(`Two editors supper ${width}`);

    await second.getByRole("textbox", { name: /^Location/ }).fill("Fellowship hall");
    await second.getByRole("button", { name: "Save series" }).click();
    await expect(second.getByRole("button", { name: "Save series" })).toHaveCount(0);
    await expect(second.getByRole("status").filter({ hasText: NOTICE })).toBeInViewport();
    await expect(second.getByRole("status").filter({ hasText: NOTICE })).toBeFocused();
    await expect(second.getByRole("heading", { level: 1, name: `Two editors supper ${width}` })).toBeVisible();
    await expect(second.getByText("Fellowship hall")).toHaveCount(0);
    expect(await series.read()).toMatchObject({ title: `Two editors supper ${width}`, location: "Room 201" });

    await second.getByRole("button", { name: "Edit series" }).click();
    await expect(second.getByText(NOTICE)).toHaveCount(0);
    await expect(second.getByRole("textbox", { name: "Title" })).toHaveValue(`Two editors supper ${width}`);
    await other.close();
  });
}

test("booking a series room and then saving saves at once, with nothing to check", async ({ page, request }) => {
  const series = await newSeries(request, "Booked then saved", { startsLocal: `${dateIn(63)}T06:00` });
  const saves = seriesSaves(page, series.id);
  await signIn(page, "lee");
  await openEditor(page, series.id);
  await pickRoom(page.getByRole("combobox", { name: /^Room for every date/ }), "Fellowship Hall · Main (80 seats)");
  await page.getByRole("button", { name: "Book for every date" }).click();
  await expect(page.getByRole("status").filter({ hasText: /Booked for \d+ dates\./ })).toBeVisible();

  await page.getByRole("textbox", { name: "Title" }).fill("Booked then saved again");
  await page.getByRole("button", { name: "Save series" }).click();
  await expect(page.getByRole("button", { name: "Save series" })).toHaveCount(0);
  await expect(page.getByRole("heading", { level: 1, name: "Booked then saved again" })).toBeVisible();
  await expect(page.getByText(NOTICE)).toHaveCount(0);
  // The booking moved the event's revision: the first send was refused, the event read, and the same save sent again.
  expect(saves).toEqual([409, 200]);
  expect((await series.read()).title).toBe("Booked then saved again");
});

test("pressing Save again after a stored save's answer was lost closes the editor as a save does", async ({ page, request }) => {
  const series = await newSeries(request, "Lost answer", { capacity: 12 });
  await signIn(page, "lee");
  await openEditor(page, series.id);
  // The first save reaches the server and is stored; its answer never reaches the page.
  let dropped = false;
  await page.route(`**/api/v1/events/${series.id}`, async (route) => {
    if (dropped || route.request().method() !== "PUT") return route.fallback();
    dropped = true;
    await route.fetch();
    await route.abort();
  });
  await page.getByRole("textbox", { name: "Title" }).fill("  Lost answer supper  ");
  await page.getByLabel(/^Seat limit/).fill("");
  await page.getByRole("button", { name: "Save series" }).click();
  await expect(page.getByText("We couldn't reach Fold. Check your connection and try again.")).toBeVisible();
  expect(await series.read()).toMatchObject({ title: "Lost answer supper", capacity: null });

  await page.getByRole("button", { name: "Save series" }).click();
  await expect(page.getByRole("button", { name: "Save series" })).toHaveCount(0);
  await expect(page.getByRole("heading", { level: 1, name: "Lost answer supper" })).toBeVisible();
  await expect(page.getByText(/weren't saved/)).toHaveCount(0);
  expect(await series.read()).toMatchObject({ title: "Lost answer supper", capacity: null });
});

test("clearing a series seat limit saves", async ({ page, request }) => {
  const series = await newSeries(request, "Open seats night", { capacity: 12 });
  await signIn(page, "lee");
  await openEditor(page, series.id);
  const seats = page.getByLabel(/^Seat limit/);
  await expect(seats).toHaveValue("12");
  await seats.fill("");
  await page.getByRole("button", { name: "Save series" }).click();
  await expect(page.getByRole("button", { name: "Save series" })).toHaveCount(0);
  await expect.poll(async () => (await series.read()).capacity).toBeNull();
  await openEditor(page, series.id);
  await expect(page.getByLabel(/^Seat limit/)).toHaveValue("");
});
