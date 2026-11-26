import * as schema from "valibot";
import { csrfFor } from "./helpers";
import { expect, test } from "@playwright/test";
import { apiAs, checkState, groupDetailPath, signIn, signOut } from "./helpers";

// M1.3 in the browser: /join with the age gate, the login page's request
// link, invite-a-friend wording, the admin queue and self-signup
// setting, inline approval from the bell, and the set-password page.
// Requests made here land in the shared seed database, so each test
// uses its own email address, and the admin's bell is left with nothing
// waiting or unread for the specs that count them.

// Marks the admin's notifications read through the API, leaving the
// bell clean for the specs that count waiting and unread items.
async function markAllRead(page: import("@playwright/test").Page) {
  const response = await page.request.post("/api/v1/notifications/read-all", {
    headers: { "X-CSRF-Token": await csrfFor(page.request) },
  });

  expect(response.ok()).toBe(true);
}

test("join page: age gate, self wording, and a stored request", async ({ page }) => {
  await page.goto("/");
  const joinLink = page.locator("header").getByRole("link", { name: "Sign up", exact: true });
  await expect(joinLink).toBeVisible();
  await joinLink.click();
  await expect(page.getByRole("heading", { name: /Request an account/ })).toBeVisible();
  await expect(page.getByText("This is the address you will sign in with.")).toBeVisible();

  // Under 13: the household explanation replaces the submit button.
  await page.getByLabel("Under 13").click();
  await expect(page.getByText(/accounts start at 13/i)).toBeVisible();
  await expect(page.getByRole("button", { name: "Request an account" })).toHaveCount(0);

  // 13 to 17 needs a birth date.
  await page.getByLabel("13 to 17").click();
  await expect(page.getByLabel("Birth date")).toBeVisible();

  await page.getByLabel("18 or older").click();
  await page.getByLabel("Full name").fill("Browser Beth");
  await page.getByLabel("Email").fill("beth@example.com");
  await page.getByRole("button", { name: "Request an account" }).click();
  await expect(page.getByText(/check your email/i)).toBeVisible();
});

test("login page links to the request form", async ({ page }) => {
  await page.goto("/login");
  await page.getByRole("link", { name: "Request an account" }).click();
  await expect(page).toHaveURL(/\/join/);
  await expect(page.getByRole("button", { name: "Request an account" })).toBeVisible();
});

test("invite a friend speaks about them, then the admin queue decides", async ({ page }) => {
  await signIn(page, "sam");
  await page.goto("/profile/invite");
  await expect(page.getByRole("heading", { name: "Invite someone", level: 1 })).toBeVisible();
  await expect(page.getByText("This is the address they will sign in with.")).toBeVisible();
  await expect(page.getByText("Their age")).toBeVisible();
  await page.getByLabel("Full name").last().fill("Friend Fran");
  await page.getByLabel("Email").last().fill("fran-e2e@example.com");
  await page.getByRole("button", { name: "Ask an admin to invite them" }).click();
  await expect(page.getByText(/admin will review/i)).toBeVisible();
  await signOut(page);

  await signIn(page, "grier");
  await page.goto("/admin");
  await page.getByRole("tab", { name: "Settings" }).click();
  await expect(page.getByRole("tab", { name: "Settings" })).toHaveAttribute("aria-selected", "true");
  // Self-signup is two radio rows (DEC-65), On chosen.
  const mode = page.getByRole("group", { name: "Self-signup" });
  await expect(mode).toBeVisible();
  await expect(mode.getByRole("radio", { name: /^On/ })).toBeChecked();
  await page.getByRole("tab", { name: /^Approvals/ }).click();
  await expect(page.getByRole("tab", { name: /^Approvals/ })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByText("Account requests")).toBeVisible();
  await expect(page.getByText("Friend Fran")).toBeVisible();
  await expect(page.getByText(/Invited by Sam/)).toBeVisible();
  const card = page.getByRole("group", { name: "Account request from Friend Fran", exact: true });
  await card.getByRole("button", { name: "Approve" }).click();
  await expect(page.getByText("Friend Fran")).toHaveCount(0);
  // Beth (first test) is still waiting; approve her too so later specs
  // that count waiting items start clean.
  const beth = page.getByRole("group", { name: "Account request from Browser Beth", exact: true });
  await beth.getByRole("button", { name: "Approve" }).click();
  await expect(page.getByText("Browser Beth")).toHaveCount(0);
  await markAllRead(page);
});

test("clean adult request approves inline from the bell", async ({ page }) => {
  await page.goto("/join");
  await page.getByLabel("Full name").fill("Clean Carl");
  await page.getByLabel("Email").fill("carl-e2e@example.com");
  await page.getByRole("button", { name: "Request an account" }).click();
  await expect(page.getByText(/check your email/i)).toBeVisible();

  await signIn(page, "grier");
  await page.getByRole("button", { name: /Alerts/ }).click();
  const row = page.locator("li", { hasText: "Clean Carl requested an account" });
  await expect(row).toBeVisible();
  await row.getByRole("button", { name: "Approve" }).click();
  await expect(row).toContainText(/You approved this|Already approved/);
  await page.getByRole("button", { name: /Alerts/ }).click(); // close the panel
  await markAllRead(page);
});

test("set-password page explains bad and missing links", async ({ page }) => {
  await page.goto("/set-password");
  await expect(page.getByText("This link is missing its token.")).toBeVisible();
  await page.goto("/set-password?token=bogus");
  await page.getByLabel("Password", { exact: true }).fill("longenoughpassword");
  await page.getByLabel("Confirm password").fill("longenoughpassword");
  await page.getByRole("button", { name: "Set password" }).click();
  await expect(page.getByText(/expired or was already used/i)).toBeVisible();
});

// DEC-177: a lone leader's deletion request that someone else ended while the admin looked. Lee asks to delete their
// group, Grier opens the queue, Sam joins (which withdraws the request), and Grier's Delete on the card still on screen
// is refused: nothing is deleted, the card goes away on its own and the queue says the request is no longer open.
test("a deletion request that ended while the admin looked is refused, and the card goes away", async ({ page, request }) => {
  const name = `Lone study ${Date.now()}`;
  const lee = await apiAs(request, "lee");
  const { group } = schema.parse(schema.looseObject({ group: schema.looseObject({ id: schema.number() }) }), await lee("POST", "/groups", { name, visibility: "public", location: "Hall", tags: [] }));
  await (await apiAs(request, "grier"))("POST", `/admin/groups/${group.id}/approve`, {});
  await (await apiAs(request, "lee"))("POST", `/groups/${group.id}/deletion-request`, {});

  await signIn(page, "grier");
  await page.goto("/admin");
  await expect(page.getByText("Deletion requests")).toBeVisible();
  await expect(page.getByRole("link", { name })).toBeVisible();

  await (await apiAs(request, "sam"))("POST", `/groups/${group.id}/join`, {});

  const asked: string[] = [];
  page.on("dialog", (dialog) => { asked.push(dialog.message()); void dialog.accept(); });
  const refused = page.waitForResponse((res) => res.request().method() === "DELETE" && new URL(res.url()).pathname === `/api/v1/admin/groups/${group.id}`);
  await page.getByRole("button", { name: "Delete group" }).click();
  expect((await refused).status()).toBe(409);
  expect(asked).toEqual([`Delete “${name}”? This removes the group, its members, and its events. 2 other people will be notified.`]);
  const status = page.getByRole("status").filter({ hasText: `The request to delete “${name}” is no longer open. The group is still there.` });
  await expect(status).toBeVisible();
  await expect(status).toBeFocused();
  await expect(page.getByRole("link", { name })).toHaveCount(0);
  expect((await (await apiAs(request, "sam"))("GET", groupDetailPath(group.id))).group.id).toBe(group.id);
  await markAllRead(page);
});

test.describe("group deletion request decisions", () => {
  test.use({ viewport: { width: 390, height: 844 } });

  for (const { action, outcome } of [
    { action: "Delete group", outcome: "deleted" },
    { action: "Keep group", outcome: "kept" },
  ]) {
    test(`${action} moves keyboard focus to the outcome in Approvals`, async ({ page, request }) => {
      const name = `Lone study to be ${outcome}`;
      const lee = await apiAs(request, "lee");

      const { group } = schema.parse(schema.looseObject({ group: schema.looseObject({ id: schema.number() }) }),
        await lee("POST", "/groups", { name, visibility: "public", tags: [] }));

      await (await apiAs(request, "grier"))("POST", `/admin/groups/${group.id}/approve`, {});
      await (await apiAs(request, "lee"))("POST", `/groups/${group.id}/deletion-request`, {});

      await signIn(page, "grier");
      await page.goto("/admin");
      const card = page.getByRole("group", { name: `Deletion request for ${name}` });
      const link = card.getByRole("link", { name });
      await expect(link).toBeVisible();
      await checkState(page, `approvals-${outcome}`, "before");
      await link.focus();
      await page.keyboard.press("Tab");

      if (outcome === "kept") await page.keyboard.press("Tab");
      else page.once("dialog", (dialog) => { void dialog.accept(); });

      await expect(card.getByRole("button", { name: action })).toBeFocused();
      await page.keyboard.press("Enter");

      const status = page.getByRole("status").filter({ hasText: `“${name}” was ${outcome}.` });
      await expect(status).toBeFocused();
      await expect(card).toHaveCount(0);
      await checkState(page, `approvals-${outcome}`, "after");
      await markAllRead(page);
    });
  }

  test("a stale withdrawal refreshes the group's Members tab", async ({ page, request }) => {
    const name = "Lone study with a withdrawn request";
    const lee = await apiAs(request, "lee");

    const { group } = schema.parse(schema.looseObject({ group: schema.looseObject({ id: schema.number() }) }),
      await lee("POST", "/groups", { name, visibility: "public", tags: [] }));

    await (await apiAs(request, "grier"))("POST", `/admin/groups/${group.id}/approve`, {});
    const leader = await apiAs(request, "lee");
    const path = `/groups/${group.id}/deletion-request`;

    await leader("POST", path, {});
    await signIn(page, "lee");
    await page.goto(`/groups/${group.id}`);
    await page.getByRole("tab", { name: "Members", exact: true }).click();
    const cancel = page.getByRole("button", { name: "Cancel deletion request" });

    await expect(cancel).toBeVisible();
    await leader("DELETE", path);
    await cancel.click();

    await expect(page.getByText("The deletion request is no longer open.")).toBeVisible();
    await expect(page.getByRole("button", { name: "Request deletion", exact: true })).toBeVisible();
    await expect(cancel).toHaveCount(0);
    await checkState(page, "withdrawal", "refused");
  });
});
