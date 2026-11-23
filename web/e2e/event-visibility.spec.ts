import * as schema from "valibot";
import { expect, test } from "@playwright/test";
import { apiAs, saveDate, signIn, fromMore } from "./helpers";

function futureDate(days: number): string {
  return new Date(Date.now() + days * 24 * 60 * 60 * 1000).toISOString().slice(0, 10);
}

// Making one event more private previews the cancellations and saves only
// after the leader confirms, the same as a group (DEC-48).
test("making one event invite-only previews and confirms", async ({ page, request }) => {
  test.setTimeout(30_000);
  const admin = await apiAs(request, "grier");

  const { group } = schema.parse(schema.looseObject({ group: schema.looseObject({ id: schema.number() }) }), (await admin("POST", "/groups", {
    name: "Garden club", description: "", tags: [], imageUrl: "", location: "Back lot",
    visibility: "public", memberLimit: null,
  })));

  const date = futureDate(21);

  const ev = (schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), (await admin("POST", `/groups/${group.id}/events`, {
    title: "Seed swap", startsLocal: `${date}T10:00`, durationMinutes: 60, tzid: "America/Boise", location: "", recurrenceRule: "",
  })))).event.id;

  await (await apiAs(request, "quinn"))("PUT", `/events/${ev}/registrations`, { status: "going", date });
  const grier = await apiAs(request, "grier");

  await signIn(page, "grier");
  await page.goto(`/groups/${group.id}`);
  await fromMore(page, "Edit event");
  await page.getByRole("radio", { name: /^Invite only/ }).check();
  await page.getByRole("button", { name: "Save visibility" }).click();
  const review = page.getByRole("dialog");
  await expect(review.getByText("Making this event invite-only")).toBeVisible();
  await expect(review).toContainText("1 person registered will be cancelled and notified");
  const vis = async () => (schema.parse(schema.looseObject({ event: schema.looseObject({ visibility: schema.string() }) }), (await grier("GET", `/events/${ev}`)))).event.visibility;
  expect(await vis()).toBe("public");

  await review.getByRole("button", { name: "Make this event invite-only" }).click();
  await expect(review).toHaveCount(0);
  expect(await vis()).toBe("invite_only");

  await page.context().clearCookies();
  await signIn(page, "quinn");
  await page.getByRole("button", { name: "Alerts" }).click();
  await expect(page.getByText("Registration cancelled: Seed swap")).toBeVisible();
});

test("a private series can open one date without opening its siblings", async ({ page, request }) => {
  const admin = await apiAs(request, "grier");
  const { group } = schema.parse(schema.looseObject({ group: schema.looseObject({ id: schema.number() }) }), await admin("POST", "/groups", { name: "Date-only welcome", visibility: "invite_only", location: "Hidden meeting room" }));
  const date = futureDate(14);
  const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), await admin("POST", `/groups/${group.id}/events`, { title: "Welcome evening", startsLocal: `${date}T18:00`, durationMinutes: 60, tzid: "UTC", recurrenceRule: "FREQ=WEEKLY;COUNT=3", remoteAvailable: true, guestsAllowed: 3 }));
  await signIn(page, "grier");
  await page.goto(`/events/${event.id}`);
  await fromMore(page, "Edit this date");
  await page.getByRole("radio", { name: /^Public/ }).click();
  await page.getByRole("button", { name: "Save this date" }).click();
  await expect(page.getByText("Visibility changed for this date")).toBeVisible();
  await page.context().clearCookies();
  await page.goto(`/events/${event.id}/dates/${date}`);
  await expect(page.getByRole("heading", { name: "Welcome evening", level: 1 })).toBeVisible();
  await expect(page.getByRole("button", { name: "Register as a guest" })).toBeVisible();
  await expect(page.getByText("Hidden meeting room")).toHaveCount(0);
  await expect(page.getByText("Date-only welcome")).toHaveCount(0);
  await page.getByRole("button", { name: "Register as a guest" }).click();
  await expect(page.getByLabel("Full name")).toBeVisible();
  await signIn(page, "quinn");
  await page.goto(`/events/${event.id}/dates/${date}`);
  await page.getByLabel("Remote", { exact: true }).check();
  await page.getByLabel("Guests", { exact: true }).fill("2");
  await page.getByRole("button", { name: "Going", exact: true }).click();
  await expect(page.getByText("You're going to this date.")).toBeVisible();
  await page.getByRole("button", { name: "Can't go", exact: true }).click();
  await expect(page.getByText("You said you can't go.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Don't add me again" })).toHaveCount(0);
  await page.context().clearCookies();
  const sibling = futureDate(21);
  await page.goto(`/events/${event.id}/dates/${sibling}`);
  await expect(page.getByText("This date is not available")).toBeVisible();
  await page.goto(`/events/${event.id}`);
  await expect(page.getByText("Welcome evening")).toHaveCount(0);
});

async function gardenEvent(request: Parameters<typeof apiAs>[0], name: string, days: number) {
  const admin = await apiAs(request, "grier");

  const { group } = schema.parse(schema.looseObject({ group: schema.looseObject({ id: schema.number() }) }), (await admin("POST", "/groups", {
    name, description: "", tags: [], imageUrl: "", location: "Back lot", visibility: "public", memberLimit: null,
  })));

  const date = futureDate(days);

  const ev = (schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), (await admin("POST", `/groups/${group.id}/events`, {
    title: `${name} supper`, startsLocal: `${date}T10:00`, durationMinutes: 60, tzid: "America/Boise", location: "", recurrenceRule: "",
  })))).event.id;

  return { admin, group: group.id, ev, date };
}

// DEC-158: a sign-up between the review and the confirmation is asked about again, from the server's own
// answer, and exactly the people shown are cancelled.
test("a sign-up after the review is asked about before anyone is cancelled", async ({ page, request }) => {
  test.setTimeout(30_000);
  const { group, ev, date } = await gardenEvent(request, "Herb club", 22);
  await (await apiAs(request, "quinn"))("PUT", `/events/${ev}/registrations`, { status: "going", date });

  await signIn(page, "grier");
  await page.goto(`/groups/${group}`);
  await fromMore(page, "Edit event");
  await page.getByRole("radio", { name: /^Invite only/ }).check();
  await page.getByRole("button", { name: "Save visibility" }).click();
  const review = page.getByRole("dialog");
  await expect(review).toContainText("1 person registered will be cancelled and notified");
  await (await apiAs(request, "pat"))("PUT", `/events/${ev}/registrations`, { status: "going", date });
  // The request context now holds Pat's session: sign the API helper back in as the manager.
  const manager = await apiAs(request, "grier");
  await review.getByRole("button", { name: "Make this event invite-only" }).click();
  await expect(page.getByText("We checked again before saving. Please review the changes below.")).toBeVisible();
  await expect(review).toContainText("2 people registered will be cancelled and notified");
  const event = schema.looseObject({ event: schema.looseObject({ visibility: schema.string() }) });
  expect(schema.parse(event, await manager("GET", `/events/${ev}`)).event.visibility).toBe("public");
  await review.getByRole("button", { name: "Make this event invite-only" }).click();
  await expect(review).toHaveCount(0);
  expect(schema.parse(event, await manager("GET", `/events/${ev}`)).event.visibility).toBe("invite_only");
});

// DEC-158: another manager's series change under a reset review brings the review back, named for the new
// series value, and Cancel from the keyboard returns to Save visibility with nothing saved.
test("a series change under a reset review comes back named for the new value", async ({ page, request }) => {
  test.setTimeout(30_000);
  const { admin, group, ev, date } = await gardenEvent(request, "Seed club", 23);
  await saveDate(admin, ev, date, { visibility: "invite_only" });

  await signIn(page, "grier");
  await page.goto(`/groups/${group}`);
  await fromMore(page, "Edit event");
  await page.getByRole("checkbox", { name: "Also reset those dates to the series setting" }).check();
  await page.getByRole("button", { name: "Save visibility" }).focus();
  await page.keyboard.press("Enter");
  const review = page.getByRole("dialog");
  await expect(review.getByRole("heading", { name: "Reset this event's dates to the series setting" })).toBeFocused();
  await expect(review).toContainText("After the reset, more people will be able to see this date.");
  await admin("PUT", `/events/${ev}/visibility`, { visibility: "invite_only", consent: "" });
  await review.getByRole("button", { name: "Reset 1 date" }).focus();
  await page.keyboard.press("Enter");
  await expect(review.getByRole("heading", { name: "Making this event public" })).toBeFocused();
  await expect(review).toHaveAccessibleDescription("This event is invite-only now. Saving makes it public, so more people will be able to see it.");
  const event = schema.looseObject({ event: schema.looseObject({ visibility: schema.string() }) });
  expect(schema.parse(event, await admin("GET", `/events/${ev}`)).event.visibility).toBe("invite_only");
  await review.getByRole("button", { name: "Cancel" }).focus();
  await page.keyboard.press("Enter");
  await expect(review).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Save visibility" })).toBeFocused();
  expect(schema.parse(event, await admin("GET", `/events/${ev}`)).event.visibility).toBe("invite_only");
});

async function gardenSeries(request: Parameters<typeof apiAs>[0], name: string, days: number) {
  const admin = await apiAs(request, "grier");

  const { group } = schema.parse(schema.looseObject({ group: schema.looseObject({ id: schema.number() }) }), (await admin("POST", "/groups", {
    name, description: "", tags: [], imageUrl: "", location: "Back lot", visibility: "public", memberLimit: null,
  })));

  const dates = [0, 1, 2, 3].map((i) => futureDate(days + 7 * i));

  const ev = (schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), (await admin("POST", `/groups/${group.id}/events`, {
    title: `${name} supper`, startsLocal: `${dates[0]}T10:00`, durationMinutes: 60, tzid: "UTC", location: "", recurrenceRule: "FREQ=WEEKLY;COUNT=4",
  })))).event.id;

  const override = async (date: string) => schema.parse(
    schema.looseObject({ occurrence: schema.looseObject({ visibilityOverride: schema.optional(schema.string()) }) }),
    await admin("GET", `/events/${ev}/occurrences/${date}`),
  ).occurrence.visibilityOverride ?? "series";

  return { admin, group: group.id, ev, dates, override };
}

// DEC-158: a reset is the reset that was reviewed. The review counts the dates the server has when the
// leader saves, a change before the confirmation brings it back from the server's answer, and exactly the
// dates on screen are reset.
test("a reset review counts the dates the server will reset", async ({ page, request }) => {
  test.setTimeout(30_000);
  const { admin, group, ev, dates, override } = await gardenSeries(request, "Fern club", 24);
  await saveDate(admin, ev, dates[0], { visibility: "private" });
  await saveDate(admin, ev, dates[1], { visibility: "private" });

  await signIn(page, "grier");
  await page.goto(`/groups/${group}`);
  await fromMore(page, /^Edit (event|series)$/);
  await page.getByRole("radio", { name: /^Invite only/ }).check();
  await page.getByRole("checkbox", { name: "Also reset those dates to the series setting" }).check();
  // Another manager gives a third date its own visibility before the save.
  const other = await apiAs(request, "grier");
  await saveDate(other, ev, dates[2], { visibility: "private" });
  await page.getByRole("button", { name: "Save visibility" }).click();
  const review = page.getByRole("dialog");
  await expect(review).toContainText("This resets 3 dates to the series setting (invite-only):");
  await expect(review.getByRole("region", { name: "Dates that will be reset" }).getByRole("listitem")).toHaveCount(3);
  // And clears one before the confirmation.
  await saveDate(other, ev, dates[0], { visibility: null });
  await review.getByRole("button", { name: "Make this event invite-only" }).click();
  await expect(page.getByText("We checked again before saving. Please review the changes below.")).toBeVisible();
  await expect(review).toContainText("This resets 2 dates to the series setting (invite-only):");
  await expect(review.getByRole("region", { name: "Dates that will be reset" }).getByRole("listitem")).toHaveCount(2);
  expect(await override(dates[1])).toBe("private");
  expect(await override(dates[2])).toBe("private");
  await review.getByRole("button", { name: "Make this event invite-only" }).click();
  await expect(review).toHaveCount(0);
  expect(await Promise.all(dates.map(override))).toEqual(["series", "series", "series", "series"]);
  const event = schema.looseObject({ event: schema.looseObject({ visibility: schema.string() }) });
  expect(schema.parse(event, await admin("GET", `/events/${ev}`)).event.visibility).toBe("invite_only");
});

// DEC-158: a reset with nobody signed up is still reviewed, and a same-count swap of the dates under the
// review brings it back listing the new date; only that date is reset.
test("a same-count swap under a reset review lists the new date", async ({ page, request }) => {
  test.setTimeout(30_000);
  const { admin, group, ev, dates, override } = await gardenSeries(request, "Moss club", 25);
  await saveDate(admin, ev, dates[0], { visibility: "invite_only" });

  await signIn(page, "grier");
  await page.goto(`/groups/${group}`);
  await fromMore(page, /^Edit (event|series)$/);
  await page.getByRole("checkbox", { name: "Also reset those dates to the series setting" }).check();
  await page.getByRole("button", { name: "Save visibility" }).click();
  const review = page.getByRole("dialog");
  await expect(review.getByRole("heading", { name: "Reset this event's dates to the series setting" })).toBeVisible();
  await expect(review).toContainText("After the reset, more people will be able to see this date.");
  const listed = review.getByRole("region", { name: "Dates that will be reset" });
  const before = await listed.innerText();
  expect(await override(dates[0])).toBe("invite_only");
  // Another manager moves the date's own visibility to another date: the count stays one.
  const other = await apiAs(request, "grier");
  await saveDate(other, ev, dates[0], { visibility: null });
  await saveDate(other, ev, dates[1], { visibility: "invite_only" });
  await review.getByRole("button", { name: "Reset 1 date" }).click();
  await expect(page.getByText("We checked again before saving. Please review the changes below.")).toBeVisible();
  await expect(listed.getByRole("listitem")).toHaveCount(1);
  await expect(listed).not.toHaveText(before);
  expect(await override(dates[0])).toBe("series");
  expect(await override(dates[1])).toBe("invite_only");
  await review.getByRole("button", { name: "Reset 1 date" }).click();
  await expect(review).toHaveCount(0);
  expect(await Promise.all(dates.map(override))).toEqual(["series", "series", "series", "series"]);
});
