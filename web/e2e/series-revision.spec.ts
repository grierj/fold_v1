import * as schema from "valibot";
import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { apiAs, signIn } from "./helpers";

// DEC-162: a series save names the read it was made from. A save made from an older read is refused with nothing
// written; the editor keeps what was typed, shows what changed, and the person saves again. Clearing a seat limit
// saves (#445).
const stored = schema.looseObject({ event: schema.looseObject({ title: schema.string(), location: schema.string(), capacity: schema.nullable(schema.number()) }) });

async function newSeries(request: APIRequestContext, title: string, extra: Record<string, number> = {}) {
  const grier = await apiAs(request, "grier");
  const { groups } = schema.parse(schema.looseObject({ groups: schema.array(schema.looseObject({ id: schema.number(), name: schema.string() })) }), await grier("GET", "/groups"));
  const room = groups.find((g) => g.name === "Room 201 class")!;
  const lee = await apiAs(request, "lee");
  const d = new Date(Date.now() + 9 * 864e5).toISOString().slice(0, 10);

  const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), await lee("POST", `/groups/${room.id}/events`, {
    title, startsLocal: `${d}T19:00`, durationMinutes: 60, tzid: "America/Boise", location: "Room 201", recurrenceRule: "FREQ=WEEKLY;COUNT=3", ...extra,
  }));

  const read = async () => schema.parse(stored, await lee("GET", `/events/${event.id}`)).event;

  return { id: event.id, read };
}

async function openEditor(page: Page, eventId: number) {
  await page.goto(`/events/${eventId}?edit=series`);
  await expect(page.getByRole("button", { name: "Save series" })).toBeVisible();
}

test("a second editor's save from an older read is refused, keeps what was typed, and saves after a check", async ({ page, browser, request }) => {
  const series = await newSeries(request, "Two editors night");
  await signIn(page, "lee");
  await openEditor(page, series.id);
  const other = await browser.newContext({ baseURL: test.info().project.use.baseURL });
  const second = await other.newPage();
  await signIn(second, "grier");
  await openEditor(second, series.id);

  const leeTitle = page.getByRole("textbox", { name: "Title" });
  await leeTitle.fill("Two editors supper");
  await page.getByRole("button", { name: "Save series" }).click();
  await expect(page.getByRole("button", { name: "Save series" })).toHaveCount(0);
  await expect.poll(async () => (await series.read()).title).toBe("Two editors supper");

  const place = second.getByRole("textbox", { name: /^Location/ });
  await place.fill("Fellowship hall");
  await second.getByRole("button", { name: "Save series" }).click();
  await expect(second.getByText("This event changed while you were saving. Check it and save again.")).toBeVisible();
  // The page read the event again: the other editor's title shows, the typed place stays, and nothing was saved.
  await expect(second.getByRole("textbox", { name: "Title" })).toHaveValue("Two editors supper");
  await expect(place).toHaveValue("Fellowship hall");
  expect(await series.read()).toMatchObject({ title: "Two editors supper", location: "Room 201" });

  await second.getByRole("button", { name: "Save series" }).click();
  await expect(second.getByRole("button", { name: "Save series" })).toHaveCount(0);
  await expect.poll(series.read).toMatchObject({ title: "Two editors supper", location: "Fellowship hall" });
  await other.close();
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
