import * as schema from "valibot";
import { expect, test, type Page } from "@playwright/test";
import { apiAs, signIn, signOut } from "./helpers";

// The nav shell (UI makeover PR 5): tabs on phones, a top bar on desktop,
// the church name in the header, and a tab bar that never covers a field.
test("phones get a tab bar that reaches every main page, and it never covers a focused field", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 700 });
  await signIn(page, "sam");
  const tabs = page.getByRole("navigation", { name: "Tabs" });
  await expect(tabs).toBeVisible();
  await expect(page.getByRole("link", { name: "powered by Fold" }).or(page.getByText("powered by Fold")).first()).toBeVisible();

  await expect(tabs.getByRole("link")).toHaveCount(5);
  await tabs.getByRole("link", { name: "Calendar" }).click();
  await expect(page).toHaveURL(/\/calendar$/);
  await tabs.getByRole("link", { name: "My groups" }).click();
  await expect(page).toHaveURL(/\/my-groups$/);
  await tabs.getByRole("link", { name: /alerts/i }).click();
  await expect(page).toHaveURL(/\/notifications$/);
  await tabs.getByRole("link", { name: "Me", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Your profile" })).toBeVisible();

  // Every tab is a full 44px target.
  for (const link of await tabs.getByRole("link").all()) {
    const box = (await link.boundingBox())!;
    expect(box.height).toBeGreaterThanOrEqual(44);
  }

  // Focus each text field on the profile page: none ends up under the bar.
  const barTop = (await tabs.boundingBox())!.y;

  for (const field of await page.locator("main input:not([type=hidden]):not([type=file]), main select, main textarea").all()) {
    if (!(await field.isVisible())) continue;
    await field.focus();
    const box = (await field.boundingBox())!;
    expect(box.y + box.height, `${await field.getAttribute("id")} sits under the tab bar`).toBeLessThanOrEqual(barTop);
  }

  // Log out lives on the Me page on phones.
  await page.getByRole("button", { name: "Log out" }).click();
  await expect(page.getByRole("link", { name: "Log in" })).toBeVisible();
  await expect(tabs).toBeHidden();
});

test("desktop keeps a top bar and no tab bar", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await signIn(page, "sam");
  await expect(page.getByRole("navigation", { name: "Tabs" })).toBeHidden();
  const main = page.getByRole("navigation", { name: "Main" });
  await expect(main.getByRole("link", { name: "My groups" })).toBeVisible();
  await expect(main.getByRole("link", { name: "New group" })).toBeVisible();
  await expect(main.getByRole("link", { name: "Calendar" })).toBeVisible();
  await signOut(page);
});

// No Disable or Delete on your own row: the server refuses both anyway.
test("an admin on a phone reaches Admin from Me and can't disable or delete themselves", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  await signIn(page, "grier");
  await expect(page.getByRole("banner").getByRole("link", { name: "New group" })).toBeVisible();
  await expect(page.getByRole("banner").getByRole("link", { name: "Admin" })).toBeHidden();
  await page.getByRole("navigation", { name: "Tabs" }).getByRole("link", { name: "Me", exact: true }).click();
  await page.getByRole("main").getByRole("link", { name: "Admin", exact: true }).click();
  await page.getByRole("tablist", { name: "Admin sections", exact: true }).getByRole("tab", { name: /^Users/ }).click();
  const search = page.getByRole("searchbox", { name: "Search by name or email" });
  await search.fill("grier@example.com");
  const own = page.getByRole("row").filter({ hasText: "grier@" });
  await own.getByRole("button", { name: /^More for/ }).click();
  await expect(own.getByRole("menuitem", { name: "Edit" })).toBeVisible();
  await expect(own.getByRole("menuitem", { name: "Reset password" })).toBeVisible();
  await expect(own.getByRole("menuitem", { name: "Disable" })).toHaveCount(0);
  await expect(own.getByRole("menuitem", { name: "Delete" })).toHaveCount(0);
  await page.keyboard.press("Escape");
  // The bounded list need not put Sam on the first page of a busy church.
  await search.fill("sam@example.com");
  const other = page.getByRole("row").filter({ hasText: "sam@" }).first();
  await other.scrollIntoViewIfNeeded();
  await expect(other.getByRole("button", { name: /^More for/ })).toBeInViewport({ ratio: 1 });
  await other.getByRole("button", { name: /^More for/ }).click();

  for (const name of ["Edit", "Reset password", "Disable", "Delete"]) await expect(other.getByRole("menuitem", { name })).toBeInViewport({ ratio: 1 });

});

test("five tab labels stay on one line at 390, even with a 9+ badge", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 700 });
  await signIn(page, "sam");
  await page.route("**/api/v1/notifications/badge", (r) => r.fulfill({ json: { unread: 12, waiting: 0 } }));
  await page.goto("/calendar");
  const tabs = page.getByRole("navigation", { name: "Tabs" });
  await expect(tabs.getByText("9+")).toBeVisible();
  await expect(tabs.getByRole("link", { name: "Calendar" })).toHaveAttribute("aria-current", "page");
  // A wrapped label would make its tab taller than the 56px bar.
  const links = await tabs.getByRole("link").all();
  expect(links).toHaveLength(5);

  for (const link of links) {
    expect((await link.boundingBox())!.height, `${await link.textContent()} wraps`).toBeLessThanOrEqual(56);
  }
});

// DEC-97, DEC-98: Me > Notifications is reached from the Me list on a phone
// and from the sidebar on desktop, and its one switch saves at once.
test("Me reaches Notifications, and the optional-email switch saves and sticks", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  await signIn(page, "sam");
  await page.getByRole("navigation", { name: "Tabs" }).getByRole("link", { name: "Me", exact: true }).click();
  await page.getByRole("main").getByRole("link", { name: /^Notifications/ }).click();
  await expect(page).toHaveURL(/\/profile\/notifications$/);
  await expect(page.getByRole("heading", { name: "Notifications", level: 1 })).toBeVisible();
  const box = page.getByRole("checkbox", { name: "Turn off all optional email" });
  await expect(box).not.toBeChecked();
  await box.check();
  await expect(page.getByRole("status").filter({ hasText: "Saved" })).toBeVisible();
  await page.reload();
  await expect(box).toBeChecked();

  await page.setViewportSize({ width: 1280, height: 900 });
  await expect(page.getByRole("navigation", { name: "Profile settings" }).getByRole("link", { name: "Notifications" })).toHaveAttribute("aria-current", "page");

  // Leave the shared seed user as other specs expect it.
  await box.uncheck();
  await expect(page.getByRole("status").filter({ hasText: "Saved" })).toBeVisible();
});

// The group page is its own chunk, requested on intent and never on page
// load; while it is on its way the page shows a labelled skeleton.
test("a slow group page chunk shows a labelled skeleton, then the group", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  let requested = false;
  let release = () => {};

  const held = new Promise<void>((resolve) => {
    release = () => resolve();
  });

  await signIn(page, "sam");
  // Park the pointer away from the cards so only the click below is intent;
  // routing turns the HTTP cache off, so the fresh load asks for the chunk again.
  await page.mouse.move(0, 0);
  await page.route(/\/assets\/GroupDetail-[^/]+\.js$/, async (route) => {
    requested = true;
    await held;
    await route.continue();
  });
  await page.goto("/");
  const card = page.getByRole("heading", { name: "Room 201 class" });
  await expect(card).toBeVisible();
  expect(requested, "the group page chunk loaded before any intent").toBe(false);

  await card.click();
  await expect(page).toHaveURL(/\/groups\/\d+$/);
  await expect(page.getByRole("status").filter({ hasText: "Loading this group…" })).toBeVisible();
  expect(requested).toBe(true);
  release();
  await expect(page.getByRole("heading", { level: 1, name: "Room 201 class" })).toBeVisible();
  await expect(page.getByRole("status").filter({ hasText: "Loading this group…" })).toHaveCount(0);
});

// A failed early start must not strand the page: the click waits for it and
// fetches the chunk again. The html mark tells a same-document retry from the
// one-time reload kept for browsers that remember a failed module.
test("a group page chunk that fails while it starts on hover is fetched again for the click", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  let requests = 0;
  let seen = () => {};
  let abort = () => {};

  const started = new Promise<void>((resolve) => {
    seen = () => resolve();
  });

  const aborting = new Promise<void>((resolve) => {
    abort = () => resolve();
  });

  await signIn(page, "sam");
  await page.mouse.move(0, 0);
  await page.route(/\/assets\/GroupDetail-[^/]+\.js$/, async (route) => {
    requests += 1;

    if (requests === 1) {
      seen();
      await aborting;
      await route.abort("connectionreset");

      return;
    }

    await route.continue();
  });
  await page.goto("/");
  await page.evaluate(() => {
    document.documentElement.dataset.sameDocument = "yes";
  });
  const card = page.getByRole("heading", { name: "Room 201 class" });
  await expect(card).toBeVisible();
  expect(requests, "the group page chunk loaded before any intent").toBe(0);

  await card.hover();
  await started;
  await card.click();
  await expect(page).toHaveURL(/\/groups\/\d+$/);
  await expect(page.getByRole("status").filter({ hasText: "Loading this group…" })).toBeVisible();
  abort();
  await expect(page.getByRole("heading", { level: 1, name: "Room 201 class" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "This page didn't load" })).toHaveCount(0);
  expect(requests).toBe(2);
  await expect(page.locator("html[data-same-document=yes]")).toHaveCount(1);
});

// The group and event routes have their own loading boundaries, so leaving a
// loaded group for its event shows the event skeleton, not the old group.
// The group is this test's own: in a shared seed group, other specs' events
// make the list compact (over 6 events) and can page the second date out.
test("See all dates from a loaded group shows the event skeleton while the event page chunk loads", async ({ page, request }) => {
  const admin = await apiAs(request, "grier");
  const groupName = `Slow page group ${Date.now().toString(36)}`;

  const { group: room } = schema.parse(schema.looseObject({ group: schema.looseObject({ id: schema.number() }) }), await admin("POST", "/groups", {
    name: groupName, description: "", tags: [], location: "", visibility: "public", memberLimit: null,
  }));

  const d = new Date(Date.now() + 11 * 864e5).toISOString().slice(0, 10);
  await admin("POST", `/groups/${room.id}/events`, {
    title: "Slow page night", startsLocal: `${d}T06:30`, durationMinutes: 30, tzid: "America/Boise",
    recurrenceRule: "FREQ=WEEKLY;COUNT=2",
  });

  await page.setViewportSize({ width: 390, height: 800 });
  let requested = false;
  let release = () => {};

  const held = new Promise<void>((resolve) => {
    release = () => resolve();
  });

  await signIn(page, "sam");
  await page.mouse.move(0, 0);
  await page.route(/\/assets\/EventDetail-[^/]+\.js$/, async (route) => {
    requested = true;
    await held;
    await route.continue();
  });
  await page.goto(`/groups/${room.id}`);
  await expect(page.getByRole("heading", { level: 1, name: groupName })).toBeVisible();
  expect(requested, "the event page chunk loaded with the group").toBe(false);

  const card = page.getByRole("article", { name: "Slow page night", exact: true });
  await card.getByRole("link", { name: "See all 2 dates", exact: true }).click();
  await expect(page).toHaveURL(/\/events\/\d+$/);
  await expect(page.getByRole("status").filter({ hasText: "Loading this event…" })).toBeVisible();
  await expect(page.getByRole("heading", { level: 1, name: groupName })).toHaveCount(0);
  expect(requested).toBe(true);
  release();
  await expect(page.getByRole("heading", { level: 1, name: "Slow page night" })).toBeVisible();
  await expect(page.getByRole("status").filter({ hasText: "Loading this event…" })).toHaveCount(0);
});

// When the chunk fails twice the page reloads once, for browsers that keep a
// failed module for the whole document; a page that still fails after that
// reload stops at the reload message instead of reloading again.
async function failGroupChunk(page: Page, failures: number) {
  let requests = 0;

  await signIn(page, "sam");
  await page.mouse.move(0, 0);
  await page.route(/\/assets\/GroupDetail-[^/]+\.js$/, async (route) => {
    requests += 1;

    if (requests <= failures) await route.abort("connectionreset");
    else await route.continue();
  });
  await page.goto("/");
  await page.evaluate(() => {
    document.documentElement.dataset.sameDocument = "yes";
  });
  // Start the route load without warming the chunk on pointer entry or focus.
  await page.getByRole("heading", { name: "Room 201 class" }).dispatchEvent("click");

  return () => requests;
}

test("a group page chunk that fails twice reloads the page once and then loads", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  const requests = await failGroupChunk(page, 2);

  await expect(page.getByRole("heading", { level: 1, name: "Room 201 class" })).toBeVisible();
  await expect(page).toHaveURL(/\/groups\/\d+$/);
  expect(requests()).toBe(3);
  await expect(page.locator("html[data-same-document=yes]")).toHaveCount(0);
});

test("a group page chunk that keeps failing reloads only once, then offers a reload", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  const requests = await failGroupChunk(page, Infinity);

  await expect(page.getByRole("heading", { level: 1, name: "This page didn't load" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Reload page" })).toBeVisible();
  expect(requests()).toBe(4);
  await expect(page.locator("html[data-same-document=yes]")).toHaveCount(0);
});

// A failure that lands after the reader has left the group page is not this
// page's to handle: the page they moved to stays put, in the same document,
// and the group page offers its reload only if they come back to it.
test("a group page chunk that fails after the reader leaves never reloads the page they moved to", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  let requests = 0;
  let release = () => {};

  const held = new Promise<void>((resolve) => {
    release = () => resolve();
  });

  await signIn(page, "sam");
  await page.mouse.move(0, 0);
  await page.route(/\/assets\/GroupDetail-[^/]+\.js$/, async (route) => {
    requests += 1;

    if (requests === 1) await held;
    await route.abort("connectionreset");
  });
  await page.goto("/");
  await page.evaluate(() => {
    document.documentElement.dataset.sameDocument = "yes";
  });
  const card = page.getByRole("heading", { name: "Room 201 class" });

  await card.click();
  await expect(page).toHaveURL(/\/groups\/\d+$/);
  await expect(page.getByRole("status").filter({ hasText: "Loading this group…" })).toBeVisible();
  await page.goBack();
  await expect(page).toHaveURL(/\/$/);
  await page.mouse.move(0, 0);
  await expect(card).toBeVisible();

  release();
  await expect.poll(() => requests).toBe(2);
  await page.evaluate(() => new Promise((resolve) => setTimeout(resolve, 100)));
  await expect(card).toBeVisible();
  await expect(page.locator("html[data-same-document=yes]")).toHaveCount(1);
  expect(await page.evaluate(() => sessionStorage.getItem("fold:page-chunk-reload"))).toBeNull();

  await card.click();
  await expect(page.getByRole("heading", { level: 1, name: "This page didn't load" })).toBeVisible();
  await expect(page.locator("html[data-same-document=yes]")).toHaveCount(1);
});

// The router decodes the address before it picks a route, so going back to
// "/groups/%6Eew" opens the new-group form; a group chunk failure that lands
// there is not the form's to handle and never reloads it.
test("a group page chunk that fails after the reader goes back to an encoded new-group address never reloads the form", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  let requests = 0;
  let release = () => {};

  const held = new Promise<void>((resolve) => {
    release = () => resolve();
  });

  await signIn(page, "sam");
  await page.mouse.move(0, 0);
  await page.route(/\/assets\/GroupDetail-[^/]+\.js$/, async (route) => {
    requests += 1;

    if (requests === 1) await held;
    await route.abort("connectionreset");
  });
  await page.goto("/groups/%6Eew");
  const form = page.getByRole("heading", { level: 1, name: "Start a group" });
  await expect(form).toBeVisible();
  await page.evaluate(() => {
    document.documentElement.dataset.sameDocument = "yes";
  });
  await page.getByRole("navigation", { name: "Tabs" }).getByRole("link", { name: "Home" }).click();
  const card = page.getByRole("heading", { name: "Room 201 class" });
  await expect(card).toBeVisible();
  expect(requests, "the group page chunk loaded before any intent").toBe(0);

  await card.click();
  await expect(page).toHaveURL(/\/groups\/\d+$/);
  await expect(page.getByRole("status").filter({ hasText: "Loading this group…" })).toBeVisible();
  await page.goBack();
  await expect(page).toHaveURL(/\/$/);
  await page.goBack();
  await expect(page).toHaveURL(/\/groups\/%6Eew$/);
  await expect(form).toBeVisible();

  release();
  await expect.poll(() => requests).toBe(2);
  await page.evaluate(() => new Promise((resolve) => setTimeout(resolve, 100)));
  await expect(form).toBeVisible();
  await expect(page.locator("html[data-same-document=yes]")).toHaveCount(1);
  expect(await page.evaluate(() => sessionStorage.getItem("fold:page-chunk-reload"))).toBeNull();
});
