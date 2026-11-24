import * as schema from "valibot";
import { expect, test } from "@playwright/test";
import { apiAs, checkState, email, PASSWORD, signInTo } from "./helpers";

// DEC-167: a pick of a date's room names the read it was made from. Two leaders have "Edit this date" open on the
// same date; the first picks a room, the second then picks another. The second's pick is refused: its Room control
// shows the first's room, says so in one focused line at the field, and the editor stays open with its draft.
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

  for (const name of [`Loft ${suffix}`, `Parlor ${suffix}`]) {
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
  await second.getByRole("textbox", { name: /^Title/ }).fill("Typed while choosing");
  await first.getByRole("combobox", { name: /^Room/ }).selectOption(String(rooms[0].id));
  await expect(first.getByText(`Booked Loft ${suffix}.`)).toBeVisible();

  await second.getByRole("combobox", { name: /^Room/ }).selectOption(String(rooms[1].id));
  const line = second.getByText(LINE);
  await expect(line).toBeVisible();
  await expect(line).toBeFocused();
  await expect(second.getByRole("combobox", { name: /^Room/ })).toHaveValue(String(rooms[0].id));
  await expect(second.getByRole("textbox", { name: /^Title/ })).toHaveValue("Typed while choosing");
  await expect(second.getByRole("button", { name: /^Save/ })).toBeVisible();
  await checkState(second, "room-revision", "conflict");

  // The first leader's room is the one stored.
  const read = schema.parse(schema.looseObject({ booking: schema.looseObject({ roomId: schema.number() }) }), await grier("GET", `/events/${event.id}/dates/${date}/rooms`));
  expect(read.booking.roomId).toBe(rooms[0].id);

  for (const page of pages) await page.context().close();
});
