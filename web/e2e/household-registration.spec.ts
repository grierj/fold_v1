import * as schema from "valibot";
import { expect, test } from "@playwright/test";
import { apiAs, email, PASSWORD, signIn } from "./helpers";
import { expectModalDialog } from "./dialogs";

// What Add back sends for a household person (Events.restoreRegistration).
const RestoreBody = schema.object({ date: schema.string(), personId: schema.number(), registeredBy: schema.number(), attendanceMode: schema.string(), overCapacity: schema.boolean() });

// The complete household arc uses its own account and event so the UI
// fixtures and the existing account-only waitlist specs remain independent.
test("household profile, pending link, independent answers, privacy and recovery", async ({ page, request }) => {
  const admin = await apiAs(request, "grier");
  const { user } = await admin("POST", "/admin/users", { ageBand: "adult", name: "Flow Parent", email: email("household-flow"), password: PASSWORD, role: "member" });
  const { group } = await admin("POST", "/groups", { name: "Household flow", visibility: "public", description: "Family dates", tags: [], location: "Hall" });
  const start = new Date(Date.now() + 6 * 3600_000).toISOString().slice(0, 16);
  const { event } = await admin("POST", `/groups/${group.id}/events`, { title: "Family gathering", startsLocal: start, durationMinutes: 60, tzid: "UTC", capacity: 2, waitlistEnabled: true, remoteAvailable: true, remoteCapacity: 1, childcareAvailable: true, childcareCapacity: 1 });
  await signIn(page, "household-flow"); await page.goto("/profile/household");
  await page.getByRole("button", { name: "Add a household member" }).click();
  await page.getByLabel("First name", { exact: true }).fill("Mary");
  await page.getByRole("radio", { name: "Adult", exact: true }).check();
  await page.getByLabel(/^Birth date/).fill("1990-01-01");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("button", { name: "Edit Mary" })).toBeVisible();
  await page.getByRole("button", { name: "Add a household member" }).click();
  await page.getByLabel("First name", { exact: true }).fill("Kit");
  await page.getByLabel(/^Birth date/).fill("2020-05-01");
  await page.getByLabel(/^Care notes/).fill("Private allergy plan\n" + "LongCareText".repeat(90));
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("button", { name: "Edit Kit" })).toBeVisible();
  await page.getByRole("button", { name: "Rename" }).click();
  await page.getByLabel(/^Household name/).fill("Flow household");
  await page.getByLabel(/^Address/).fill("Private street address");
  await page.getByRole("button", { name: "Save household" }).click();
  await page.reload(); await expect(page.getByText("Flow household", { exact: true })).toBeVisible();
  await expect(page.getByText("Private street address", { exact: true })).toBeVisible();
  await page.goto(`/events/${event.id}`);
  await expect(page.getByRole("checkbox", { name: "Kit", exact: true })).toBeDisabled();
  await expect(page.getByText(/Waiting for an admin to confirm/)).toBeVisible();
  const adminAgain = await apiAs(request, "grier");
  const { guardianLinks: links } = await adminAgain("GET", "/admin/guardian-links?status=pending");
  const link = links.find((l: { guardianUserId: number; personName: string }) => l.guardianUserId === user.id && l.personName === "Kit");
  expect(link).toBeTruthy();
  await adminAgain("POST", `/admin/guardian-links/${link.id}/confirm`, { checkNote: "Known family" });
  await page.reload();
  await page.getByRole("checkbox", { name: "Me", exact: true }).check();
  await page.getByRole("group", { name: "How you will attend" }).getByRole("radio", { name: "Remote" }).check();
  await page.getByRole("checkbox", { name: "Kit", exact: true }).check();
  await page.getByRole("checkbox", { name: "Childcare for Kit" }).check();
  await page.getByRole("checkbox", { name: "Mary", exact: true }).check();
  await page.getByRole("button", { name: "Save answers" }).click();
  await expect(page.getByRole("status")).toContainText("You: going remotely");
  await expect(page.getByRole("status")).toContainText("Kit: going in person");
  await page.reload(); await expect(page.getByRole("checkbox", { name: "Childcare for Kit" })).toBeChecked();
  await signIn(page, "sam"); await page.goto(`/events/${event.id}`);
  const unrelated = await (await page.request.get(`/api/v1/events/${event.id}`)).text();
  expect(unrelated).not.toContain("Flow household"); expect(unrelated).not.toContain("Private allergy"); expect(unrelated).not.toContain('"householdPerson"');
  await signIn(page, "grier"); await page.goto(`/events/${event.id}`);
  await page.getByRole("button", { name: /^More for/ }).click(); await page.getByRole("menuitem", { name: "Manage sign-ups" }).click();
  await expect(page.getByText("Kit · Flow household")).toBeVisible();
  await expect(page.getByText("Signed up by Flow Parent").first()).toBeVisible();
  const childRow = page.getByText("Kit · Flow household").locator("..", { has: page.getByText("Signed up by Flow Parent") });
  await expect(childRow.locator("div", { hasText: /^Signed up by Flow Parent$/ }).first()).toBeVisible();
  const order = await childRow.evaluate((node) => node.textContent ?? "");
  expect(order.indexOf("Going")).toBeGreaterThan(order.indexOf("Signed up by"));
  expect(order.indexOf("Going")).toBeLessThan(order.indexOf("Care notes"));
  const fullNotes = page.getByRole("button", { name: "Show full care notes for Kit" });
  await fullNotes.click(); await expect(page.getByRole("button", { name: "Hide care notes" })).toHaveAttribute("aria-expanded", "true");
  // The event page asks in its own labelled dialog; a browser prompt here is a regression.
  const nativeDialogs: string[] = [];
  page.on("dialog", (dialog) => { nativeDialogs.push(dialog.message()); void dialog.dismiss(); });
  await page.getByRole("button", { name: "More for Kit" }).click();
  await page.getByRole("menuitem", { name: "Remove Kit" }).click();
  const removeQuestion = page.getByRole("dialog", { name: "Remove Kit's registration for this date?" });
  await expect(removeQuestion).toContainText("They won't be able to re-register on their own.");
  await expect(removeQuestion.getByRole("button", { name: "Cancel" })).toBeFocused();
  await expectModalDialog(page, removeQuestion, "roster-remove-question");
  await removeQuestion.getByRole("button", { name: "Remove", exact: true }).click();
  await expect(removeQuestion).toBeHidden();
  await expect(page.getByRole("region", { name: "Removed or left out" })).toBeVisible();
  // Seats that fill while Add back is on its way: the server refuses, and only a yes to the retry adds Kit over capacity.
  const restorePath = `**/api/v1/events/${event.id}/registrations/add`;
  const restores: Array<schema.InferOutput<typeof RestoreBody>> = [];
  await page.route(restorePath, async (route) => {
    const body = schema.parse(RestoreBody, route.request().postDataJSON());
    restores.push(body);

    if (body.overCapacity === true) await route.continue();
    else await route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: "This event is full.", code: "event_full" }) });
  });
  const addBack = page.getByRole("button", { name: "Add back Kit" });
  await addBack.click();
  const retry = page.getByRole("dialog", { name: "Add Kit over capacity?" });
  await expect(retry).toContainText("seats filled");
  await expect(retry.getByRole("button", { name: "Add over capacity" })).toBeFocused();
  await expectModalDialog(page, retry, "roster-restore-retry");
  // Escape answers no, even after a click on the heading: the refusal stands, nothing more is sent and focus is back on Add back.
  await retry.getByRole("heading").click();
  await page.keyboard.press("Escape");
  await expect(retry).toBeHidden();
  await expect(page.getByRole("region", { name: "Manage sign-ups" }).getByRole("alert").filter({ hasText: "This event is full." })).toBeVisible();
  await expect(addBack).toBeFocused();
  expect(restores).toHaveLength(1);
  await addBack.click();
  await retry.getByRole("button", { name: "Add over capacity" }).click();
  await expect(retry).toBeHidden();
  await expect(page.getByRole("button", { name: "More for Kit" })).toBeVisible();
  expect(restores.map((body) => body.overCapacity)).toEqual([false, false, true]);
  // The one override is for the same person and attendance mode the refusal was for.
  expect(new Set(restores.map((body) => `${body.date}/${body.personId}/${body.registeredBy}/${body.attendanceMode}`)).size).toBe(1);
  await page.unroute(restorePath);
  await page.getByRole("button", { name: "More for Kit" }).click();
  await expect(page.getByRole("menuitem", { name: "Remove Kit" })).toBeVisible();
  await page.keyboard.press("Escape");
  expect(nativeDialogs).toEqual([]);
  // Leave the admin bell as later specs expect (notifications.spec.ts).
  await (await apiAs(request, "grier"))("POST", "/notifications/read-all", {});
});
