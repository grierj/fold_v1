import * as schema from "valibot";
import { expect, test } from "@playwright/test";
import { apiAs, pickRoom, signIn, signInTo } from "./helpers";

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

test("a phone shows the full selected room and building with the picker closed", async ({ page, request }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const admin = await apiAs(request, "grier");
  const name = "South Fellowship Room in the West Wing beside the Community Kitchen and Welcome Desk";
  const building = "Saint Brigid's Community and Ministry Center";
  const label = `${name} · ${building}`;
  await admin("POST", "/admin/rooms", { name, building });
  const { groups } = schema.parse(schema.looseObject({ groups: schema.array(schema.looseObject({ id: schema.number(), name: schema.string() })) }), await admin("GET", "/groups"));
  const group = groups.find((g) => g.name === "Room 201 class")!;
  const date = new Date(Date.now() + 48 * 864e5).toISOString().slice(0, 10);

  const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), await admin("POST", `/groups/${group.id}/events`, {
    title: "Fellowship night", startsLocal: `${date}T18:00`, durationMinutes: 60, tzid: "America/Boise",
  }));

  // A pending booking selects the room without creating a confirmed use that would suggest it.
  await admin("PUT", "/admin/settings/room-approval", { enabled: true });

  try {
    await signInTo(page, "lee", `/events/${event.id}/dates/${date}`);
    await page.getByRole("button", { name: "Edit this date" }).click();
    const room = page.getByRole("combobox", { name: "Room (optional)", exact: true });
    const selected = room.locator("..").getByText(label, { exact: true }).and(page.locator("p"));
    await expect(selected).toHaveCount(0);
    await pickRoom(room, label);
    await expect(page.getByRole("status").filter({ hasText: `Waiting for an admin to approve ${name}.` })).toBeVisible();
    await expect(room.locator("option:checked")).toHaveText(label);
    await expect(room).toHaveAccessibleDescription("Saved as soon as you choose it, so you don't need to press Save. If bookings need an admin's approval, it waits for one.");
    await expect(selected).toBeVisible();

    const fits = await selected.evaluate((line) => {
      const range = document.createRange();
      range.selectNodeContents(line);
      const bounds = range.getBoundingClientRect();
      const field = line.getBoundingClientRect();

      return bounds.left >= field.left && bounds.right <= field.right && field.right <= window.innerWidth && bounds.bottom <= field.bottom;
    });

    expect(fits).toBe(true);

    await room.selectOption({ label: "No room" });
    await expect(page.getByRole("status").filter({ hasText: "Room released." })).toBeVisible();
    await expect(selected).toHaveCount(0);
  } finally {
    await admin("PUT", "/admin/settings/room-approval", { enabled: false });
  }
});
