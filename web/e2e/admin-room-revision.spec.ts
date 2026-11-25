import * as schema from "valibot";
import { expect, test } from "@playwright/test";
import { apiAs, checkState, email, PASSWORD, signInTo } from "./helpers";

// DEC-171: an admin's edit of a room names the read it was made from. Two admins have the same room's edit form open;
// the first saves new seats and features, the second then renames it. The second's form closes on its own, one focused
// line says the room changed and the changes could not be saved, and the row shows the first admin's values.
const idOf = schema.looseObject({ id: schema.number() });
const roomOf = schema.looseObject({ id: schema.number(), name: schema.string(), capacity: schema.nullable(schema.number()), features: schema.string() });

test("two admins edit the same room: the second is told and sees the first's values", async ({ browser, request }) => {
  const suffix = Date.now().toString(36);
  const grier = await apiAs(request, "grier");
  const admin = `room-admin-${suffix}`;
  schema.parse(idOf, (await grier("POST", "/admin/users", { ageBand: "adult", name: "Room Admin", email: email(admin), password: PASSWORD, role: "admin" })).user);
  // A short name: the tab's selects and rows at 390px have no room for a long one (#484).
  const name = `Den ${suffix.slice(-4)}`;
  const room = schema.parse(schema.looseObject({ room: idOf }), await grier("POST", "/admin/rooms", { name })).room;

  const pages = [];

  for (const login of ["grier", admin]) {
    const context = await browser.newContext({ viewport: { width: 390, height: 844 } });
    const page = await context.newPage();
    await signInTo(page, login, "/admin?tab=rooms");
    await page.getByRole("searchbox", { name: "Search rooms" }).fill(name);
    await page.getByRole("button", { name: `Edit ${name}` }).click();
    await expect(page.getByLabel(/^Name/)).toHaveValue(name);
    pages.push(page);
  }

  const [first, second] = pages;
  await first.getByLabel("Capacity (optional)").fill("12");
  await first.getByLabel("Features (optional)").fill("piano");
  await first.getByRole("button", { name: "Save room" }).click();
  await expect(first.getByText(`“${name}” saved.`)).toBeVisible();

  await second.getByLabel(/^Name/).fill(`${name} B`);
  await second.getByRole("button", { name: "Save room" }).click();
  const line = second.getByText(`${name}. This room changed while you were editing, so your changes could not be saved. This is the room as it is now.`);
  await expect(line).toBeVisible();
  await expect(line).toBeFocused();
  await expect(second.getByRole("form")).toHaveCount(0);
  await expect(second.getByRole("group", { name })).toContainText("Seats 12 · piano");
  await expect(second.getByText(`${name} B`)).toHaveCount(0);
  await checkState(second, "admin-room-revision", "conflict");

  // The first admin's values are the ones stored.
  const { rooms } = schema.parse(schema.looseObject({ rooms: schema.array(roomOf) }), await grier("GET", "/admin/rooms"));
  expect(rooms.find((r) => r.id === room.id)).toMatchObject({ name, capacity: 12, features: "piano" });

  for (const page of pages) await page.context().close();
});
