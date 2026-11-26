import { expect, test, type Page } from "@playwright/test";
import { apiAs, checkState, email, fromMore, PASSWORD, signInTo } from "./helpers";

const viewport = { width: 390, height: 844 };

async function pickZone(page: Page, city: string) {
  await page.getByRole("combobox", { name: "Church time zone" }).fill(city);
  await page.getByRole("option", { name: new RegExp(city) }).first().click();
}

test("a stale settings form shows the other admin's save and accepts a fresh edit", async ({ browser, request }) => {
  test.setTimeout(90_000);
  const api = await apiAs(request, "grier");
  const login = `settings-revision-admin-${Date.now().toString(36)}`;
  await api("POST", "/admin/users", { ageBand: "adult", role: "admin", name: "Settings Admin", email: email(login), password: PASSWORD });

  const cases = [
    { path: "/admin/settings/church-address", field: "address", noun: "church mailing address", label: "Save address",
      edit: (page: Page, value: string) => page.getByRole("textbox", { name: /Church mailing address/ }).fill(value),
      winner: "18 First Street", draft: "24 Second Street", spare: "35 Third Street" },
    { path: "/admin/settings/reply-to", field: "address", noun: "reply address", label: "Save reply address",
      edit: (page: Page, value: string) => page.getByRole("textbox", { name: /Where replies go/ }).fill(value),
      winner: "first-office@example.com", draft: "second-office@example.com", spare: "third-office@example.com" },
    { path: "/admin/timezone", field: "timezone", noun: "church time zone", label: "Save time zone",
      edit: pickZone, winner: "Denver", draft: "Chicago", spare: "Phoenix" },
  ];

  const originals = await Promise.all(cases.map(async ({ path, field }) => (await api("GET", path))[field]));
  const firstContext = await browser.newContext({ viewport });
  const secondContext = await browser.newContext({ viewport });
  const first = await firstContext.newPage();
  const second = await secondContext.newPage();

  try {
    await signInTo(first, "grier", "/admin?tab=settings");
    await signInTo(second, login, "/admin?tab=settings");

    for (const [index, setting] of cases.entries()) {
      const { path, field, noun, label, edit, winner, draft, spare } = setting;
      const actualWinner = field === "timezone" ? "America/Denver" : winner;
      const actualDraft = field === "timezone" ? "America/Chicago" : draft;

      // In case another spec used this value, choose the other city as the first edit.
      const reverse = originals[index] === actualWinner;
      const firstValue = reverse ? draft : winner;
      const secondValue = reverse ? winner : draft;
      const storedFirst = reverse ? actualDraft : actualWinner;
      const storedSecond = reverse ? actualWinner : actualDraft;
      await edit(first, firstValue);
      await first.getByRole("button", { name: label, exact: true }).click();
      await expect.poll(async () => (await api("GET", path))[field]).toBe(storedFirst);

      // A field can already contain the intended value in the old read. Edit it
      // to a distinct third value to ensure the stale form sends.
      const conflictsWith = storedSecond === originals[index] ? spare : secondValue;
      await edit(second, conflictsWith);
      await second.getByRole("button", { name: label, exact: true }).click();
      const line = second.getByText(`This ${noun} changed while you were editing, so your changes could not be saved. This is the ${noun} as it is now.`);
      await expect(line).toBeFocused();
      await expect(second.getByRole("button", { name: label, exact: true })).toBeDisabled();
      expect((await api("GET", path))[field]).toBe(storedFirst);
      await checkState(second, "admin-settings-revision", noun.replace(/ /g, "-"));

      await edit(second, secondValue);
      await second.getByRole("button", { name: label, exact: true }).click();
      await expect.poll(async () => (await api("GET", path))[field]).toBe(storedSecond);
      await expect(second.getByRole("button", { name: label, exact: true })).toBeDisabled();
    }
  } finally {
    await firstContext.close();
    await secondContext.close();

    for (const [index, { path, field }] of cases.entries()) {
      const current = await api("GET", path);
      await api("PUT", path, { [field]: originals[index], revision: current.revision });
    }
  }
});

test("two account editors keep the first edit, then retry after an unrelated account change", async ({ browser, request }) => {
  test.setTimeout(90_000);
  const api = await apiAs(request, "grier");
  const suffix = Date.now().toString(36);
  const login = `account-revision-admin-${suffix}`;
  await api("POST", "/admin/users", { ageBand: "adult", role: "admin", name: "Accounts Admin", email: email(login), password: PASSWORD });
  const subjectLogin = `account-revision-subject-${suffix}`;
  const { user } = await api("POST", "/admin/users", { ageBand: "adult", role: "member", name: "Revision Subject", email: email(subjectLogin), password: PASSWORD });
  const firstContext = await browser.newContext({ viewport });
  const secondContext = await browser.newContext({ viewport });
  const first = await firstContext.newPage();
  const second = await secondContext.newPage();
  const open = (page: Page) => fromMore(page, "Edit", "More for Revision Subject");

  try {
    await signInTo(first, "grier", "/admin?tab=users");
    await signInTo(second, login, "/admin?tab=users");

    for (const page of [first, second]) {
      await page.getByRole("searchbox", { name: "Search by name or email" }).fill(subjectLogin);
      await open(page);
    }

    await checkState(second, "admin-account-revision", "editor");
    await first.getByRole("textbox", { name: /Phone/ }).fill("555-0101");
    await first.getByRole("button", { name: "Save user" }).click();
    await expect(first.getByRole("button", { name: "Save user" })).toHaveCount(0);

    await second.getByRole("textbox", { name: "Full name" }).fill("Stale name");
    await second.getByRole("button", { name: "Save user" }).click();
    const line = second.getByText("The account changed while you were editing. Your changes could not be saved.");
    await expect(line).toBeFocused();
    await expect(second.getByRole("textbox", { name: "Full name" })).toHaveCount(0);
    await expect(second.getByRole("searchbox", { name: "Search by name or email" })).toHaveValue(subjectLogin);
    await checkState(second, "admin-account-revision", "conflict");

    await open(second);
    await expect(second.getByRole("textbox", { name: /Phone/ })).toHaveValue("555-0101");
    await expect(second.getByRole("textbox", { name: "Full name" })).toHaveValue("Revision Subject");

    // Resetting the password moves the account's revision without changing a
    // field the editor submits. The stale edit should read and retry once.
    await api("POST", `/admin/users/${user.id}/password`, { password: "new-password1234" });
    await second.getByRole("textbox", { name: /Display name/ }).fill("Revision Member");
    const statuses: number[] = [];
    second.on("response", (res) => {
      if (res.request().method() === "PUT" && new URL(res.url()).pathname === `/api/v1/admin/users/${user.id}`) statuses.push(res.status());
    });
    await second.getByRole("button", { name: "Save user" }).click();
    await expect(second.getByRole("button", { name: "More for Revision Member" })).toBeVisible();
    expect(statuses).toEqual([409, 200]);
  } finally {
    await firstContext.close();
    await secondContext.close();
  }
});
