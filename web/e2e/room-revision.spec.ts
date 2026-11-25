import * as schema from "valibot";
import { expect, test } from "@playwright/test";
import { apiAs, checkState, email, PASSWORD, pickDateRoom, signInTo } from "./helpers";

// DEC-167: a pick of a date's room names the read it was made from. Two leaders have "Edit this date" open on the
// same date; the first picks a room, the second then picks another. The second's pick is refused: its Room control
// shows the first's room, says so in one focused line at the field, the page's place reads again and names that room,
// and the editor stays open with its draft.
const idOf = schema.looseObject({ id: schema.number() });
const dateIn = (days: number) => new Date(Date.now() + days * 864e5).toISOString().slice(0, 10);
const LINE = "This date's room changed while you were choosing, so your choice could not be saved. This is the room as it is now.";

test("two leaders pick rooms for the same date: the second is told, sees the first's room, and keeps the draft", async ({ browser, request }) => {
  const suffix = Date.now().toString(36);
  const grier = await apiAs(request, "grier");
  const leaders = [`room-lead-a-${suffix}`, `room-lead-b-${suffix}`];
  const { group } = schema.parse(schema.looseObject({ group: idOf }), await grier("POST", "/groups", { name: `Room picks ${suffix}`, description: "", tags: [], location: "Hall", visibility: "public", memberLimit: null }));

  for (const leader of leaders) {
    const made = schema.parse(idOf, (await grier("POST", "/admin/users", { ageBand: "adult", name: "Room Leader", email: email(leader), password: PASSWORD, role: "member" })).user);
    await grier("POST", `/admin/groups/${group.id}/members`, { userId: made.id, role: "leader" });
  }

  const rooms = [];
  // Short room names: the Room select at 390px has no room for a long name beside "Used by this event once" (#484).
  const tag = suffix.slice(-4);

  for (const name of [`Loft ${tag}`, `Parlor ${tag}`]) {
    rooms.push(schema.parse(schema.looseObject({ room: idOf }), await grier("POST", "/admin/rooms", { name })).room);
  }

  const date = dateIn(12);

  const { event } = schema.parse(schema.looseObject({ event: idOf }), await grier("POST", `/groups/${group.id}/events`, {
    title: `Room picks ${suffix}`, startsLocal: `${date}T19:00`, durationMinutes: 60, tzid: "America/Boise", recurrenceRule: "FREQ=WEEKLY;COUNT=2",
  }));

  const pages = [];

  for (const leader of leaders) {
    const context = await browser.newContext({ viewport: { width: 390, height: 844 } });
    const page = await context.newPage();
    await signInTo(page, leader, `/events/${event.id}/dates/${date}`);
    await page.getByRole("button", { name: "Edit this date" }).click();
    await expect(page.getByRole("combobox", { name: /^Room/ })).toHaveValue("none");
    pages.push(page);
  }

  const [first, second] = pages;
  const where = second.locator("[data-date-answer=where]");
  await expect(where).not.toContainText(`Loft ${tag}`);
  await second.getByRole("textbox", { name: /^Title/ }).fill("Typed while choosing");
  await first.getByRole("combobox", { name: /^Room/ }).selectOption(String(rooms[0].id));
  await expect(first.getByText(`Booked Loft ${tag}.`)).toBeVisible();

  await second.getByRole("combobox", { name: /^Room/ }).selectOption(String(rooms[1].id));
  const line = second.getByText(LINE);
  await expect(line).toBeVisible();
  await expect(line).toBeFocused();
  await expect(second.getByRole("combobox", { name: /^Room/ })).toHaveValue(String(rooms[0].id));
  await expect(second.getByRole("textbox", { name: /^Title/ })).toHaveValue("Typed while choosing");
  await expect(second.getByRole("button", { name: /^Save/ })).toBeVisible();
  // The page around the editor reads again: the date's place names the first leader's room, and the draft stays.
  await expect(where).toContainText(`Loft ${tag}`);
  await expect(second.getByRole("textbox", { name: /^Title/ })).toHaveValue("Typed while choosing");
  await checkState(second, "room-revision", "conflict");

  // The first leader's room is the one stored.
  const read = schema.parse(schema.looseObject({ booking: schema.looseObject({ roomId: schema.number() }) }), await grier("GET", `/events/${event.id}/dates/${date}/rooms`));
  expect(read.booking.roomId).toBe(rooms[0].id);

  for (const page of pages) await page.context().close();
});

// A series pick says only what it measured. Another leader books the room for every date, then takes the first date's
// booking away; the first leader's "Edit series", open since before, picks the same room and books it for every date.
// The pick goes again under the new revision: it books the bare first date and says so, and that date's card shows it.
test("a series pick of the room someone else already set books the date they released, and says so", async ({ browser, request }) => {
  const suffix = Date.now().toString(36);
  const grier = await apiAs(request, "grier");
  const leaders = [`series-lead-a-${suffix}`, `series-lead-b-${suffix}`];
  const { group } = schema.parse(schema.looseObject({ group: idOf }), await grier("POST", "/groups", { name: `Series picks ${suffix}`, description: "", tags: [], location: "Hall", visibility: "public", memberLimit: null }));

  for (const leader of leaders) {
    const made = schema.parse(idOf, (await grier("POST", "/admin/users", { ageBand: "adult", name: "Series Leader", email: email(leader), password: PASSWORD, role: "member" })).user);
    await grier("POST", `/admin/groups/${group.id}/members`, { userId: made.id, role: "leader" });
  }

  const loft = schema.parse(schema.looseObject({ room: idOf }), await grier("POST", "/admin/rooms", { name: `Loft ${suffix}` })).room;
  const date = dateIn(12);

  const { event } = schema.parse(schema.looseObject({ event: idOf }), await grier("POST", `/groups/${group.id}/events`, {
    title: `Series picks ${suffix}`, startsLocal: `${date}T19:00`, durationMinutes: 60, tzid: "America/Boise", recurrenceRule: "FREQ=WEEKLY;COUNT=4",
  }));

  const context = await browser.newContext({ viewport: { width: 390, height: 844 } });
  const page = await context.newPage();
  await signInTo(page, leaders[0], `/events/${event.id}`);
  await page.getByRole("button", { name: "Edit series" }).click();
  const field = page.getByRole("combobox", { name: /^Room for every date/ });
  await expect(field).toHaveValue("none");

  const other = await apiAs(request, leaders[1]);
  const seriesRead = schema.parse(schema.looseObject({ revision: schema.string() }), await other("GET", `/events/${event.id}/rooms`));
  await other("PUT", `/events/${event.id}/room`, { roomId: loft.id, revision: seriesRead.revision });
  await pickDateRoom(other, event.id, date, { roomId: null });

  await field.selectOption(String(loft.id));
  await page.getByRole("button", { name: "Book for every date" }).click();
  await expect(page.getByRole("status").filter({ hasText: "Booked for 1 date." })).toBeVisible();
  await expect(page.getByText(/is saved|changed while you were choosing/)).toHaveCount(0);
  // The page behind reads again: the first date's card names the room.
  await expect(page.getByRole("article").first()).toContainText(`Loft ${suffix}`);

  const read = schema.parse(schema.looseObject({ booking: schema.looseObject({ roomId: schema.number(), status: schema.string() }) }), await other("GET", `/events/${event.id}/dates/${date}/rooms`));
  expect(read.booking).toMatchObject({ roomId: loft.id, status: "confirmed" });
  await context.close();
});
