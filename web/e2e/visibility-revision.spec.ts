import * as schema from "valibot";
import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { apiAs, pickRoom, signIn } from "./helpers";

// DEC-163: a visibility save names the read it was made from, like a series save (DEC-162). A save made from an
// older read is refused with nothing written; the control checks the event once and saves again by itself when the
// audience is as it opened, or closes with the panel and the page says the changes weren't saved.
const stored = schema.looseObject({ event: schema.looseObject({ visibility: schema.string(), revision: schema.optional(schema.string()) }) });

const NOTICE = "This event changed while you were editing, so your changes weren't saved. This is the event as it is now.";

const dateIn = (days: number) => new Date(Date.now() + days * 864e5).toISOString().slice(0, 10);

async function newSeries(request: APIRequestContext, title: string, startsLocal: string) {
  const grier = await apiAs(request, "grier");
  const { groups } = schema.parse(schema.looseObject({ groups: schema.array(schema.looseObject({ id: schema.number(), name: schema.string() })) }), await grier("GET", "/groups"));
  const room = groups.find((g) => g.name === "Room 201 class")!;
  const lee = await apiAs(request, "lee");

  const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), await lee("POST", `/groups/${room.id}/events`, {
    title, startsLocal, durationMinutes: 60, tzid: "America/Boise", location: "Room 201", recurrenceRule: "FREQ=WEEKLY;COUNT=3",
  }));

  const read = async () => schema.parse(stored, await lee("GET", `/events/${event.id}`)).event;

  return { id: event.id, read, lee };
}

async function openPanel(page: Page, eventId: number) {
  await page.goto(`/events/${eventId}?edit=series`);
  await expect(page.getByRole("button", { name: "Save visibility" })).toBeVisible();
}

// The answers to this event's visibility saves, in order.
function visibilitySaves(page: Page, eventId: number) {
  const statuses: number[] = [];
  page.on("response", (res) => {
    if (res.request().method() === "PUT" && new URL(res.url()).pathname === `/api/v1/events/${eventId}/visibility`) statuses.push(res.status());
  });

  return statuses;
}

test("a second manager's widening over another's tightening closes the panel and says nothing was saved, at 390px", async ({ page, browser, request }) => {
  const series = await newSeries(request, "Two audiences", `${dateIn(12)}T19:00`);
  await series.lee("PUT", `/events/${series.id}/visibility`, { visibility: "private", consent: "", revision: (await series.read()).revision ?? "" });

  const other = await browser.newContext({ baseURL: test.info().project.use.baseURL, viewport: { width: 390, height: 844 } });
  const second = await other.newPage();
  const saves = visibilitySaves(second, series.id);
  await signIn(second, "lee");
  await openPanel(second, series.id);
  await expect(second.getByRole("radio", { name: /^Private/ })).toBeChecked();

  await signIn(page, "grier");
  await openPanel(page, series.id);
  await page.getByRole("radio", { name: /^Invite only/ }).check();
  await page.getByRole("button", { name: "Save visibility" }).click();
  await page.getByRole("dialog", { name: "Making this event invite-only" }).getByRole("button", { name: "Make this event invite-only" }).click();
  await expect.poll(async () => (await series.read()).visibility).toBe("invite_only");

  await second.getByRole("radio", { name: /^Public/ }).check();
  await second.getByRole("button", { name: "Save visibility" }).click();
  const notice = second.getByRole("status").filter({ hasText: NOTICE });
  await expect(notice).toBeFocused();
  await expect(notice).toBeInViewport();
  await expect(second.getByRole("button", { name: "Save visibility" })).toHaveCount(0);
  expect(saves).toEqual([409]);
  expect((await series.read()).visibility).toBe("invite_only");

  await second.getByRole("button", { name: "Edit series" }).click();
  await expect(second.getByText(NOTICE)).toHaveCount(0);
  await expect(second.getByRole("radio", { name: /^Invite only/ })).toBeChecked();
  await other.close();
});

test("a widening chosen before an admin made the group private is not saved", async ({ page, request }) => {
  const grier = await apiAs(request, "grier");
  const withId = schema.looseObject({ id: schema.number() });
  const { group } = schema.parse(schema.looseObject({ group: withId }), await grier("POST", "/groups", { name: "Audience follows the group", visibility: "public", tags: [] }));
  const { event } = schema.parse(schema.looseObject({ event: withId }), await grier("POST", `/groups/${group.id}/events`, { title: "Group audience", startsLocal: `${dateIn(15)}T19:00`, durationMinutes: 60, tzid: "America/Boise" }));
  const read = async () => schema.parse(stored, await grier("GET", `/events/${event.id}`)).event;
  await grier("PUT", `/events/${event.id}/visibility`, { visibility: "invite_only", consent: "", revision: (await read()).revision ?? "" });

  const saves = visibilitySaves(page, event.id);
  await signIn(page, "grier");
  await openPanel(page, event.id);
  await page.getByRole("radio", { name: /^Public \(same as the group\)/ }).check();
  // The event row stays as it was; only its group's audience changes.
  await grier("PUT", `/groups/${group.id}`, { visibility: "private" });
  await page.getByRole("button", { name: "Save visibility" }).click();
  await expect(page.getByRole("status").filter({ hasText: NOTICE })).toBeFocused();
  expect(saves).toEqual([409]);
  expect((await read()).visibility).toBe("invite_only");
});

test("changing the visibility right after booking a series room saves on one press", async ({ page, request }) => {
  const series = await newSeries(request, "Booked then private", `${dateIn(98)}T05:00`);
  const saves = visibilitySaves(page, series.id);
  await signIn(page, "lee");
  await openPanel(page, series.id);
  await pickRoom(page.getByRole("combobox", { name: /^Room for every date/ }), "Fellowship Hall · Main (80 seats)");
  await page.getByRole("button", { name: "Book for every date" }).click();
  await expect(page.getByRole("status").filter({ hasText: /Booked for \d+ dates\./ })).toBeVisible();

  await page.getByRole("radio", { name: /^Invite only/ }).check();
  await page.getByRole("button", { name: "Save visibility" }).click();
  await page.getByRole("dialog", { name: "Making this event invite-only" }).getByRole("button", { name: "Make this event invite-only" }).click();
  await expect.poll(async () => (await series.read()).visibility).toBe("invite_only");
  await expect(page.getByText(NOTICE)).toHaveCount(0);
  // The booking moved the event's revision: the first send was refused, the event read, and the same save sent again.
  expect(saves).toEqual([409, 200]);
});
