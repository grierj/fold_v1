import * as schema from "valibot";
import { expect, test, type Locator } from "@playwright/test";
import { apiAs, checkState, pickRoom, signIn, signInTo } from "./helpers";

// M2.1d: with approval on, a leader's room booking waits for an admin, the
// admin confirms it from the Rooms tab, and the leader's date shows the room.
// The setting is shared by every spec in the shard, so it is turned off again
// whatever happens.
test("a leader's room waits for an admin, who confirms it", async ({ page, request }) => {
  try {
    const admin = await apiAs(request, "grier");
    await admin("PUT", "/admin/settings/room-approval", { enabled: true });
    const { groups } = schema.parse(schema.looseObject({ groups: schema.array(schema.looseObject({ id: schema.number(), name: schema.string() })) }), (await admin("GET", "/groups")));
    const group = groups.find((g) => g.name === "Room 201 class")!;
    const lee = await apiAs(request, "lee");
    const day = new Date(Date.now() + 47 * 864e5).toISOString().slice(0, 10);

    const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), (await lee("POST", `/groups/${group.id}/events`, {
      title: "Approval night", startsLocal: `${day}T14:00`, durationMinutes: 60, tzid: "America/Boise",
      location: "south door",
    })));

    // The leader asks for the hall and is told it is waiting.
    await signIn(page, "lee");
    await page.goto(`/events/${event.id}`);
    await expect(page.getByRole("heading", { name: "Approval night", level: 1 })).toBeVisible();
    await page.getByRole("button", { name: "Edit event" }).click();
    await pickRoom(page.getByRole("combobox", { name: /^Room/ }), "Fellowship Hall · Main (80 seats)");
    await expect(page.getByRole("status").filter({ hasText: "Waiting for an admin to approve Fellowship Hall." })).toBeVisible();
    // Nothing is held yet: the place is still the typed location.
    await expect(page.getByText("south door", { exact: true })).toBeVisible();

    // The admin sees it under Needs a room and approves it.
    await signIn(page, "grier");
    await page.goto("/admin?tab=rooms");
    await expect(page.getByRole("heading", { name: "Needs a room" })).toBeVisible();
    const row = page.getByRole("listitem").filter({ has: page.getByRole("heading", { name: "Approval night" }) });
    await expect(row.getByText("Fellowship Hall · Main", { exact: true })).toBeVisible();
    await row.getByRole("button", { name: "Approve for Approval night" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Approved Fellowship Hall · Main for Approval night: 1 date." })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Approval night" })).toHaveCount(0);

    // The leader's date now shows the room first, then the typed location.
    await signIn(page, "lee");
    await page.goto(`/events/${event.id}`);
    await expect(page.getByText("Fellowship Hall · Main · south door", { exact: true })).toBeVisible();
  } finally {
    const admin = await apiAs(request, "grier");
    await admin("PUT", "/admin/settings/room-approval", { enabled: false });
  }
});

test("a phone shows the full selected room and building in all four pickers", async ({ page, request }) => {
  test.setTimeout(60_000);
  await page.setViewportSize({ width: 390, height: 844 });
  const admin = await apiAs(request, "grier");
  const name = "South Fellowship Room in the West Wing beside the Community Kitchen and Welcome Desk";
  const building = "Saint Brigid's Community and Ministry Center";
  const otherBuilding = "Saint Brigid's Community and Ministry Annex";
  const label = `${name} · ${building}`;
  await admin("POST", "/admin/rooms", { name, building });
  await admin("POST", "/admin/rooms", { name, building: otherBuilding });
  const { groups } = schema.parse(schema.looseObject({ groups: schema.array(schema.looseObject({ id: schema.number(), name: schema.string() })) }), await admin("GET", "/groups"));
  const group = groups.find((g) => g.name === "Room 201 class")!;
  const date = new Date(Date.now() + 48 * 864e5).toISOString().slice(0, 10);

  const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), await admin("POST", `/groups/${group.id}/events`, {
    title: "Fellowship night", startsLocal: `${date}T18:00`, durationMinutes: 60, tzid: "America/Boise",
    recurrenceRule: "FREQ=WEEKLY;COUNT=2",
  }));

  const visibleValue = (room: Locator) => room.locator("..").locator("[data-selected-option]");

  const checkChoice = async (room: Locator, entry: string) => {
    await expect(room.locator("option:checked")).toHaveText(label);
    await expect(room.getByRole("option", { name: `${name} · ${otherBuilding}`, exact: true })).toHaveCount(1);
    await checkState(page, "long-room", entry);
    await expect(visibleValue(room)).toBeVisible();
    await expect(visibleValue(room)).toHaveText(label);
  };

  // Pending bookings don't add a suggestion based on a confirmed use.
  await admin("PUT", "/admin/settings/room-approval", { enabled: true });

  try {
    await signInTo(page, "lee", `/events/${event.id}/dates/${date}`);
    await page.getByRole("button", { name: "Edit this date" }).click();
    const dateRoom = page.getByRole("combobox", { name: "Room (optional)", exact: true });
    await expect(visibleValue(dateRoom)).toHaveCount(0);
    await pickRoom(dateRoom, label);
    await expect(page.getByRole("status").filter({ hasText: `Waiting for an admin to approve ${name}.` })).toBeVisible();
    await expect(dateRoom).toHaveAccessibleDescription("Saved as soon as you choose it, so you don't need to press Save. If bookings need an admin's approval, it waits for one.");
    await checkChoice(dateRoom, "edit-date");

    await dateRoom.press("Home");
    await expect(page.getByRole("status").filter({ hasText: "Room released." })).toBeVisible();
    await expect(dateRoom.locator("option:checked")).toHaveText("No room");
    await expect(visibleValue(dateRoom)).toHaveCount(0);

    await page.goto(`/events/${event.id}`);
    await page.getByRole("button", { name: "Edit series" }).click();
    const seriesRoom = page.getByRole("combobox", { name: "Room for every date (optional)", exact: true });
    await pickRoom(seriesRoom, label);
    await checkChoice(seriesRoom, "edit-series");

    for (const entry of [
      { who: "lee", path: `/groups/${group.id}/events/new`, name: "schedule-group" },
      { who: "grier", path: "/events/new", name: "schedule-church" },
    ]) {
      await signInTo(page, entry.who, entry.path);
      await page.getByLabel("Date", { exact: true }).fill(date);
      const room = page.getByRole("combobox", { name: "Room (optional)", exact: true });
      await pickRoom(room, label);
      await checkChoice(room, entry.name);
    }
  } finally {
    await admin("PUT", "/admin/settings/room-approval", { enabled: false });
  }
});
