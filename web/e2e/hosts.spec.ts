import * as schema from "valibot";
import { expect, test } from "@playwright/test";
import { apiAs, email, signIn, signOut, fromMore } from "./helpers";
import { expectModalDialog } from "./dialogs";

test.use({ locale: "en-US", timezoneId: "America/New_York" });

function futureDate(days: number): string {
  return new Date(Date.now() + days * 24 * 60 * 60 * 1000).toISOString().slice(0, 10);
}

test("an admin schedules a church-wide event in the church time zone", async ({ page, request }) => {
  // Make this date fall outside the four-date preview without relying on
  // another spec having seeded earlier church events.
  const admin = await apiAs(request, "grier");

  for (let i = 0; i < 5; i++) await admin("POST", "/events", {
    title: `Earlier church gathering ${i + 1}`, startsLocal: `${futureDate(2 + i)}T18:00`,
    durationMinutes: 60, tzid: "UTC", visibility: "public",
  });
  await signIn(page, "grier");
  await page.goto("/events/new");
  await page.getByRole("button", { name: "Change time zone" }).click();
  await expect(page.getByLabel("Time zone")).toHaveAttribute("title", /America\/Boise$/);

  await page.getByLabel("Title").fill("Church potluck");
  await page.getByLabel("Description (optional)").fill("Bring a side dish.");
  await page.getByLabel("Date", { exact: true }).fill(futureDate(18));
  await page.getByLabel("Start time").fill("18:00");
  await page.getByRole("button", { name: "Schedule event" }).click();

  await expect(page).toHaveURL(/\/events\/\d+$/);
  await expect(page.getByRole("heading", { name: "Church potluck", level: 1 })).toBeVisible();
  await expect(page.getByText("Bring a side dish.")).toBeVisible();
  // A church-wide event says so rather than naming the church (DEC-99).
  await expect(page.getByText("Church-wide event")).toBeVisible();
  await expect(page.getByText(/^Hosted by/)).toHaveCount(0);

  // The Church events block is for signed-out visitors (DEC-94): signed in,
  // the date reaches people through "Coming up for you".
  await signOut(page);
  await expect(page.getByRole("heading", { name: "Church events" })).toBeVisible();
  const preview = page.locator('main a[href^="/events/"]');
  await expect(preview).toHaveCount(4);
  await expect(page.getByRole("link", { name: /Church potluck/ })).toHaveCount(0);
  // Home lists these under "Church events", so the preview names no host (DEC-99).
  expect((await preview.allTextContents()).join(" ")).not.toContain("Hosted by");
  await page.getByRole("link", { name: "See all church events" }).click();
  await page.getByRole("button", { name: "Filters", exact: true }).click();
  await expect(page.getByLabel("Group", { exact: true })).toHaveValue("church");
  await page.getByLabel("From date", { exact: true }).fill(futureDate(18));
  await expect(page.getByRole("link", { name: /Church potluck/ })).toBeVisible();
  await expect(page.getByRole("link", { name: /Church potluck/ })).not.toContainText("Hosted by");
});

test("opening an invite-only group's event hides the group and the names", async ({ page, request }) => {
  test.setTimeout(30_000);
  const admin = await apiAs(request, "grier");
  const { users } = schema.parse(schema.looseObject({ users: schema.array(schema.looseObject({ id: schema.number(), email: schema.string() })) }), (await admin("GET", "/admin/users")));
  const sam = users.find((u) => u.email === email("sam"));
  expect(sam).toBeTruthy();

  const { group } = schema.parse(schema.looseObject({ group: schema.looseObject({ id: schema.number() }) }), (await admin("POST", "/groups", {
    name: "Hidden Care",
    description: "Not for visitors.",
    tags: [],
    imageUrl: "",
    location: "",
    visibility: "invite_only",
    memberLimit: null,
  })));

  await admin("POST", `/admin/groups/${group.id}/members`, { userId: sam!.id, role: "member" });

  const date = futureDate(21);

  const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), (await admin("POST", `/groups/${group.id}/events`, {
    title: "Quiet supper",
    startsLocal: `${date}T18:00`,
    durationMinutes: 60,
    tzid: "America/Boise",
    location: "",
    recurrenceRule: "",
  })));

  await (await apiAs(request, "sam"))("PUT", `/events/${event.id}/registrations`, { status: "going", date });

  await signIn(page, "grier");
  await page.goto(`/groups/${group.id}`);
  await fromMore(page, "Edit event");
  await expect(page).toHaveURL(new RegExp(`/events/${event.id}\\?edit=series$`));
  await page.getByRole("radio", { name: /^Public/ }).check();
  await expect(
    page.getByText("Only leaders will see who signed up, so members of Hidden Care aren't revealed.")
  ).toBeVisible();
  const saved = page.waitForResponse((r) => r.url().includes("/visibility") && r.request().method() === "PUT");
  await page.getByRole("button", { name: "Save visibility" }).click();
  expect((await saved).ok()).toBe(true);
  await expect(page.getByRole("radio", { name: /^Public/ })).toBeChecked();

  await signOut(page);

  await page.goto(`/events/${event.id}`);
  await expect(page.getByRole("heading", { name: "Quiet supper", level: 1 })).toBeVisible();
  await expect(page.getByText("Hosted by Fold e2e")).toBeVisible();
  await expect(page.getByText("1 going")).toBeVisible();
  await expect(page.getByText("Hidden Care")).toHaveCount(0);
  await expect(page.getByText("Sam Member")).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Hidden Care" })).toHaveCount(0);

  await page.goto(`/groups/${group.id}`);
  await expect(page.getByRole("heading", { name: "Group not found" })).toBeVisible();
});

test("an admin edits one date, cancels a date, and deletes a church-wide event", async ({ page, request }) => {
  // The event page asks in its own labelled dialog; a browser prompt here is a regression.
  const nativeDialogs: string[] = [];
  page.on("dialog", (d) => { nativeDialogs.push(d.message()); void d.dismiss(); });
  const admin = await apiAs(request, "grier");

  const mk = async (title: string, days: number) =>
    (schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), (await admin("POST", "/events", {
      title,
      startsLocal: `${futureDate(days)}T19:00`,
      durationMinutes: 60,
      tzid: "America/Boise",
      location: "Hall",
      visibility: "public",
      hosts: [],
      recurrenceRule: "",
    })))).event;

  // Per-date edit: only that date changes.
  const edited = await mk("Date night", 26);
  await signIn(page, "grier");
  await page.goto(`/events/${edited.id}`);
  await fromMore(page, "Edit this date");
  await page.getByLabel(/^Location/).fill("Gym");
  await page.getByRole("button", { name: "Save this date" }).click();
  await expect(page.getByText(/Gym/)).toBeVisible();
  await expect(page.getByText(/edited on its own/)).toBeVisible();

  // Cancelling the only date leaves no upcoming dates.
  const cancelled = await mk("Cancel night", 27);
  await page.goto(`/events/${cancelled.id}`);
  await fromMore(page, "Cancel this date");
  const cancelQuestion = page.getByRole("dialog", { name: /^Cancel this date on / });
  await expect(cancelQuestion.getByRole("button", { name: "Keep this date" })).toBeFocused();
  await expectModalDialog(page, cancelQuestion, "date-cancel-question");
  await cancelQuestion.getByRole("button", { name: "Cancel this date" }).click();
  await expect(cancelQuestion).toBeHidden();
  await expect(page.getByText("No upcoming dates.")).toBeVisible();

  // Members without manage rights see none of these controls.
  await signIn(page, "sam");
  await page.goto(`/events/${edited.id}`);
  await expect(page.getByRole("button", { name: /^More for/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "More event actions" })).toHaveCount(0);
  await signOut(page);

  // Delete removes the event entirely.
  await signIn(page, "grier");
  await page.goto(`/events/${edited.id}`);
  await fromMore(page, "Delete event", "More event actions");
  const deleteQuestion = page.getByRole("dialog", { name: "Delete “Date night”?" });
  await expect(deleteQuestion).toContainText("This removes every date and its registrations.");
  await expect(deleteQuestion.getByRole("button", { name: "Cancel" })).toBeFocused();
  await expectModalDialog(page, deleteQuestion, "event-delete-question");
  // Escape answers no, even after a click on the heading took focus off the buttons: nothing is deleted and focus is back on the menu button.
  await deleteQuestion.getByRole("heading").click();
  await page.keyboard.press("Escape");
  await expect(deleteQuestion).toBeHidden();
  await expect(page.getByRole("button", { name: "More event actions" })).toBeFocused();
  await expect(page.getByRole("heading", { name: "Date night", level: 1 })).toBeVisible();
  await fromMore(page, "Delete event", "More event actions");
  await deleteQuestion.getByRole("button", { name: "Delete event" }).click();
  await expect(page).toHaveURL(/\/$/);
  await page.goto(`/events/${edited.id}`);
  await expect(page.getByRole("heading", { name: "Event not found" })).toBeVisible();
  expect(nativeDialogs).toEqual([]);
});

test("a 200-character unbroken title wraps inside the delete question, and both answers stay reachable on a short phone screen", async ({ page, request }) => {
  const admin = await apiAs(request, "grier");
  const title = "W".repeat(200);

  const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), (await admin("POST", "/events", {
    title, startsLocal: `${futureDate(28)}T19:00`, durationMinutes: 60, tzid: "America/Boise", location: "Hall", visibility: "public", hosts: [], recurrenceRule: "",
  })));

  await signIn(page, "grier");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`/events/${event.id}`);
  await fromMore(page, "Delete event", "More event actions");
  const question = page.getByRole("dialog", { name: `Delete “${title}”?` });
  await expect(question.getByRole("heading", { name: `Delete “${title}”?` })).toBeVisible();
  await expectModalDialog(page, question, "event-delete-long-title");
  await page.keyboard.press("Escape");
  await expect(question).toBeHidden();
  await expect(page.getByRole("button", { name: "More event actions" })).toBeFocused();
});

// Pressing the browser's history buttons again while the leave question is open
// asks nothing more; Keep editing keeps the draft and Leave goes to the page the
// last press was for, not the first.
test("repeated history navigation with unsaved edits asks once in the app's dialog", async ({ page, request }) => {
  const admin = await apiAs(request, "grier");

  const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), (await admin("POST", "/events", {
    title: "Draft night", startsLocal: `${futureDate(29)}T19:00`, durationMinutes: 60, tzid: "America/Boise", location: "Hall", visibility: "public", hosts: [], recurrenceRule: "",
  })));

  const nativeDialogs: string[] = [];
  page.on("dialog", (d) => { nativeDialogs.push(d.message()); void d.dismiss(); });
  await signIn(page, "grier");
  await page.setViewportSize({ width: 390, height: 844 });
  const eventUrl = new RegExp(`/events/${event.id}$`);
  await page.goto(`/events/${event.id}`);
  await expect(page.getByRole("heading", { name: "Draft night", level: 1 })).toBeVisible();
  // Two in-app steps away and back, so the next two history entries (My groups, then Home) are in this document.
  const tabs = page.getByRole("navigation", { name: "Tabs" });
  await tabs.getByRole("link", { name: "My groups" }).click();
  await expect(page).toHaveURL(/\/my-groups$/);
  await tabs.getByRole("link", { name: "Home" }).click();
  await expect(page).toHaveURL(/\/$/);
  await page.goBack();
  await expect(page).toHaveURL(/\/my-groups$/);
  await page.goBack();
  await expect(page).toHaveURL(eventUrl);

  await page.getByRole("button", { name: "Edit event" }).click();
  const titleField = page.getByLabel("Title", { exact: true });
  await titleField.fill("Draft night, changed");
  await titleField.focus();

  const leave = page.getByRole("dialog", { name: "Leave without saving?" });
  await page.goForward({ waitUntil: "commit" });
  await expect(leave).toBeVisible();
  await expect(page).toHaveURL(eventUrl);
  await page.goForward({ waitUntil: "commit" });
  await expect(page).toHaveURL(eventUrl);
  await expect(page.getByRole("dialog")).toHaveCount(1);
  await expect(leave).toContainText("Your changes will be lost.");
  await expect(leave.getByRole("button", { name: "Leave without saving" })).toBeFocused();
  await expectModalDialog(page, leave, "leave-without-saving");

  await leave.getByRole("button", { name: "Keep editing" }).click();
  await expect(leave).toBeHidden();
  await expect(page).toHaveURL(eventUrl);
  await expect(titleField).toHaveValue("Draft night, changed");
  await expect(titleField).toBeFocused();

  // First Forward is for My groups; then, with the question open, a jump of two entries is for Home.
  await page.goForward({ waitUntil: "commit" });
  await expect(leave).toBeVisible();
  await expect(page).toHaveURL(eventUrl);
  // Resolves once the app has seen that jump (its popstate listener runs first), so Leave cannot race it.
  await page.evaluate(() => new Promise<void>((resolve) => {
    window.addEventListener("popstate", () => window.setTimeout(resolve, 50), { once: true });
    history.go(2);
  }));
  await expect(page).toHaveURL(eventUrl);
  await expect(page.getByRole("dialog")).toHaveCount(1);
  await leave.getByRole("button", { name: "Leave without saving" }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page).not.toHaveURL(/\/my-groups$/);
  await expect(page.getByRole("dialog")).toHaveCount(0);
  expect(nativeDialogs).toEqual([]);
});

test("a non-admin host can edit the church-wide event they host", async ({ page, request }) => {
  const admin = await apiAs(request, "grier");
  const date = futureDate(24);

  const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), (await admin("POST", "/events", {
    title: "Host night",
    startsLocal: `${date}T19:00`,
    durationMinutes: 60,
    tzid: "America/Boise",
    location: "Hall",
    visibility: "public",
    hosts: [email("lee")],
    recurrenceRule: "",
  })));

  await signIn(page, "lee");
  await page.goto(`/events/${event.id}`);
  await expect(page.getByText("grier@example.com")).toHaveCount(0);
  await page.getByRole("button", { name: "Edit event" }).click();
  await page.getByLabel("Title").fill("Host night revised");
  await page.getByRole("button", { name: "Save series" }).click();
  await expect(page.getByRole("heading", { name: "Host night revised", level: 1 })).toBeVisible();

  await page.goto("/events/new");
  await expect(page.getByText("Only an admin can schedule a church-wide event.")).toBeVisible();
});
