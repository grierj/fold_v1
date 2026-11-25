import * as schema from "valibot";
import { expect, test } from "@playwright/test";
import { apiAs, checkState, email, PASSWORD, pickDateRoom, signInTo } from "./helpers";

// DEC-168: a change of the series room leaves alone every date with a room of its own, booked or waiting for an
// admin's approval, and lists them before the pick. Ticking the box and picking again changes them too. Approval is a
// church-wide setting shared by the shard, so it is turned off again whatever happens. The request context holds one
// sign-in, so each change of person signs in again.
const idOf = schema.looseObject({ id: schema.number() });
const bookingOf = schema.looseObject({ booking: schema.looseObject({ roomId: schema.number(), status: schema.string(), pending: schema.boolean() }) });
const dateIn = (days: number) => new Date(Date.now() + days * 864e5).toISOString().slice(0, 10);

test.use({ viewport: { width: 390, height: 844 } });

test("a series room change keeps the dates with their own room, lists them, and includes them when asked", async ({ browser, request }) => {
  const suffix = Date.now().toString(36);
  let grier = await apiAs(request, "grier");

  try {
    const leader = `own-rooms-${suffix}`;
    const { group } = schema.parse(schema.looseObject({ group: idOf }), await grier("POST", "/groups", { name: `Own rooms ${suffix}`, description: "", tags: [], location: "Hall", visibility: "public", memberLimit: null }));
    const made = schema.parse(idOf, (await grier("POST", "/admin/users", { ageBand: "adult", name: "Own Rooms Leader", email: email(leader), password: PASSWORD, role: "member" })).user);
    await grier("POST", `/admin/groups/${group.id}/members`, { userId: made.id, role: "leader" });
    const loft = schema.parse(schema.looseObject({ room: idOf }), await grier("POST", "/admin/rooms", { name: `Loft ${suffix}` })).room;
    const parlor = schema.parse(schema.looseObject({ room: idOf }), await grier("POST", "/admin/rooms", { name: `Parlor ${suffix}` })).room;
    const first = dateIn(13);
    const second = new Date(Date.parse(first) + 7 * 864e5).toISOString().slice(0, 10);

    const { event } = schema.parse(schema.looseObject({ event: idOf }), await grier("POST", `/groups/${group.id}/events`, {
      title: `Own rooms ${suffix}`, startsLocal: `${first}T19:00`, durationMinutes: 60, tzid: "America/Boise", recurrenceRule: "FREQ=WEEKLY;COUNT=4",
    }));

    // The first date is booked into the parlor on its own; the second asks for it while bookings need approval.
    let lead = await apiAs(request, leader);
    await pickDateRoom(lead, event.id, first, { roomId: parlor.id });
    grier = await apiAs(request, "grier");
    await grier("PUT", "/admin/settings/room-approval", { enabled: true });
    lead = await apiAs(request, leader);
    await pickDateRoom(lead, event.id, second, { roomId: parlor.id });
    grier = await apiAs(request, "grier");
    await grier("PUT", "/admin/settings/room-approval", { enabled: false });
    lead = await apiAs(request, leader);

    const context = await browser.newContext({ viewport: { width: 390, height: 844 } });
    const page = await context.newPage();
    await signInTo(page, leader, `/events/${event.id}`);
    await page.getByRole("button", { name: "Edit series" }).click();
    const field = page.getByRole("combobox", { name: /^Room for every date/ });
    await field.selectOption(String(loft.id));
    await expect(page.getByText("2 dates have a room of their own and will keep them:")).toBeVisible();
    await expect(page.getByRole("listitem").filter({ hasText: `Parlor ${suffix} — waiting for an admin's approval` })).toHaveCount(1);
    await expect(page.getByRole("listitem").filter({ hasText: `Parlor ${suffix}` })).toHaveCount(2);
    const box = page.getByRole("checkbox", { name: `Also change these dates to Loft ${suffix}` });
    await expect(box).not.toBeChecked();
    await checkState(page, "series-own-rooms", "listed");

    await page.getByRole("button", { name: "Book for every date" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Booked for 2 dates. 2 dates keep their own room; 1 of them is still waiting for an admin's approval." })).toBeVisible();

    for (const [date, pending] of [[first, false], [second, true]] as const) {
      expect(schema.parse(bookingOf, await lead("GET", `/events/${event.id}/dates/${date}/rooms`)).booking).toMatchObject({ roomId: parlor.id, pending });
    }

    // The same room again, with the box ticked: both dates change.
    await expect(page.getByText("2 dates have a room of their own and will keep them:")).toBeVisible();
    await box.check();
    await expect(page.getByText("2 dates have a room of their own:")).toBeVisible();
    await checkState(page, "series-own-rooms", "included");
    await page.getByRole("button", { name: "Book for every date" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Booked for 2 dates. Also changed 2 dates that had a room of their own." })).toBeVisible();
    await expect(page.getByText(/ha(s|ve) a room of (its|their) own/)).toHaveCount(0);

    for (const date of [first, second]) {
      expect(schema.parse(bookingOf, await lead("GET", `/events/${event.id}/dates/${date}/rooms`)).booking).toMatchObject({ roomId: loft.id, status: "confirmed" });
    }
    // The second date's own request was withdrawn with its booking: no admin is left asked about it.

    grier = await apiAs(request, "grier");
    const { requests } = schema.parse(schema.looseObject({ requests: schema.array(schema.looseObject({ eventId: schema.number() })) }), await grier("GET", "/admin/room-requests"));
    expect(requests.filter((r) => r.eventId === event.id)).toEqual([]);
    await context.close();
  } finally {
    grier = await apiAs(request, "grier");
    await grier("PUT", "/admin/settings/room-approval", { enabled: false });
  }
});
