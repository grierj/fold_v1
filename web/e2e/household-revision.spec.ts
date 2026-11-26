import * as schema from "valibot";
import { expect, test, type Page } from "@playwright/test";
import { apiAs, checkState, email, PASSWORD, signInTo } from "./helpers";

// DEC-174: an edit of a household member names the read it was made from. Two adults of one household have the same
// child's edit form open; the first saves new care notes, the second then renames the child. The second's form closes
// on its own, one focused line says the household changed and the changes could not be saved, and the card shows the
// first adult's care notes. The second adult's seat comes from a household conversion, so this reads the invitation
// from the dev mail outbox and runs in e2e/run.sh's dev shard (APP_ENV=dev).
const personOf = schema.looseObject({ person: schema.looseObject({ id: schema.number(), guardianLinkId: schema.optional(schema.number()) }) });
const requestsOf = schema.looseObject({ requests: schema.array(schema.looseObject({ id: schema.number(), email: schema.string() })) });

async function invitationLink(page: Page, address: string): Promise<string> {
  let link = "";
  await expect.poll(async () => {
    const html = await (await page.request.get("/dev/outbox")).text();
    const section = html.split("<section").find((part) => part.includes(`To: ${address}`));
    const raw = section?.match(/https?:[^\s<]+\/invite\?token=[A-Za-z0-9_-]+/)?.[0];
    link = raw ? new URL(raw).pathname + new URL(raw).search : "";

    return link !== "";
  }).toBe(true);

  return link;
}

test("two adults edit the same household member: the second is told and sees the first's care notes", async ({ browser, request }) => {
  test.skip((await request.get("/dev/outbox")).status() !== 200, "Requires E2E_APP_ENV=dev for the mail outbox");
  const suffix = Date.now().toString(36);
  const grier = await apiAs(request, "grier");
  // The first adult is an admin, who reads every care note; the second is the child's confirmed guardian.
  const first = `household-first-${suffix}`;
  const second = `household-second-${suffix}`;
  await grier("POST", "/admin/users", { ageBand: "adult", name: "Household First", email: email(first), password: PASSWORD, role: "admin" });

  const pages: Page[] = [];

  for (let i = 0; i < 2; i++) pages.push(await (await browser.newContext({ viewport: { width: 390, height: 844 } })).newPage());

  const [firstPage, secondPage] = pages;
  const firstApi = await apiAs(firstPage.request, first);
  const pat = schema.parse(personOf, await firstApi("POST", "/me/household/people", { firstName: "Pat", lastName: "Second", kind: "adult", birthDate: "1990-01-01" })).person;
  await firstApi("POST", `/me/household/people/${pat.id}/convert`, { email: email(second) });
  await secondPage.goto(await invitationLink(secondPage, email(second)));
  await secondPage.getByLabel("Choose a password").fill(PASSWORD);
  await secondPage.getByRole("button", { name: "Create my account" }).click();
  await expect(secondPage.getByRole("heading", { name: "Your account is almost ready" })).toBeVisible();
  const asked = schema.parse(requestsOf, await grier("GET", "/admin/account-requests")).requests.find((r) => r.email === email(second));
  expect(asked).toBeTruthy();
  await grier("POST", `/admin/account-requests/${asked?.id}/approve`, {});

  const secondApi = await apiAs(secondPage.request, second);
  const kit = schema.parse(personOf, await secondApi("POST", "/me/household/people", { firstName: "Kit", kind: "child", birthDate: "2019-05-01", careNotes: "Peanuts" })).person;
  await grier("POST", `/admin/guardian-links/${kit.guardianLinkId}/confirm`, { checkNote: "Known family" });

  await signInTo(firstPage, first, "/profile/household");
  await signInTo(secondPage, second, "/profile/household");

  for (const page of pages) {
    await page.getByRole("button", { name: "Edit Kit" }).click();
    await expect(page.getByLabel(/^Care notes/)).toHaveValue("Peanuts");
  }

  await firstPage.getByLabel(/^Care notes/).fill("Peanuts and dairy");
  await firstPage.getByRole("button", { name: "Save" }).click();
  await expect(firstPage.getByText("Care notes: Peanuts and dairy")).toBeVisible();

  await secondPage.getByLabel("First name").fill("Kitty");
  await secondPage.getByRole("button", { name: "Save" }).click();
  const line = secondPage.getByText("This household changed while you were editing, so your changes could not be saved. This is the household as it is now.");
  await expect(line).toBeVisible();
  await expect(line).toBeFocused();
  await expect(secondPage.getByRole("form")).toHaveCount(0);
  await expect(secondPage.getByText("Care notes: Peanuts and dairy")).toBeVisible();
  await expect(secondPage.getByText("Kitty")).toHaveCount(0);
  await checkState(secondPage, "household-revision", "conflict");

  // The first adult's care notes are the ones stored, under Kit's own name.
  const { household } = await secondApi("GET", "/me/household");
  expect(household.people).toEqual(expect.arrayContaining([expect.objectContaining({ id: kit.id, firstName: "Kit", careNotes: "Peanuts and dairy" })]));

  for (const page of pages) await page.context().close();
});
