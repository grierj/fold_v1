import * as schema from "valibot";
import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { apiAs, fromMore, groupDetailPath, signIn, signInTo } from "./helpers";

// DEC-166: the group form names the read it was made from. A save made from an older read is refused with nothing
// written; the form reads the group once and saves again by itself when nothing it writes has changed, and otherwise
// closes on its own while the page says the changes could not be saved.
const NOTICE = "This group changed while you were editing, so your changes could not be saved. This is the group as it is now.";

const withId = schema.looseObject({ id: schema.number() });
const stored = schema.looseObject({ group: schema.looseObject({ name: schema.string(), location: schema.string(), status: schema.string() }) });

// A group Lee leads, which Grier (an admin) may manage too: Lee starts it, so it waits for approval until `approve`.
async function leesGroup(request: APIRequestContext, name: string) {
  const lee = await apiAs(request, "lee");
  const { group } = schema.parse(schema.looseObject({ group: withId }), await lee("POST", "/groups", { name, visibility: "public", location: "Hall", tags: [] }));
  const read = async () => schema.parse(stored, await lee("GET", groupDetailPath(group.id))).group;
  const approve = async () => (await apiAs(request, "grier"))("POST", `/admin/groups/${group.id}/approve`, {});

  return { id: group.id, read, approve };
}

// The answers to this group's form saves, in order.
function groupSaves(page: Page, groupId: number) {
  const statuses: number[] = [];
  page.on("response", (res) => {
    if (res.request().method() === "PUT" && new URL(res.url()).pathname === `/api/v1/groups/${groupId}`) statuses.push(res.status());
  });

  return statuses;
}

async function openEditor(page: Page) {
  await fromMore(page, "Edit group", "More group actions");
  await expect(page.getByRole("button", { name: "Save group" })).toBeVisible();
}

test("a manager's save over another's closes the form and says nothing was saved, showing the other's values, at 390px", async ({ page, browser, request }) => {
  const group = await leesGroup(request, "Two editors");
  await group.approve();

  const other = await browser.newContext({ baseURL: test.info().project.use.baseURL, viewport: { width: 390, height: 844 } });
  const second = await other.newPage();
  const saves = groupSaves(second, group.id);
  await signInTo(second, "lee", `/groups/${group.id}`);
  await openEditor(second);
  await second.getByLabel(/^Location/).fill("Lee's porch");

  await signIn(page, "grier");
  await page.goto(`/groups/${group.id}`);
  await openEditor(page);
  await page.getByLabel("Group name").fill("Renamed by Grier");
  await page.getByRole("button", { name: "Save group" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Renamed by Grier" })).toBeVisible();

  await second.getByRole("button", { name: "Save group" }).click();
  const notice = second.getByRole("status").filter({ hasText: NOTICE });
  await expect(notice).toBeFocused();
  await expect(notice).toBeInViewport();
  await expect(second.getByRole("button", { name: "Save group" })).toHaveCount(0);
  await expect(second.getByRole("heading", { level: 1, name: "Renamed by Grier" })).toBeVisible();
  expect(saves).toEqual([409]);
  expect(await group.read()).toMatchObject({ name: "Renamed by Grier", location: "Hall" });

  await openEditor(second);
  await expect(second.getByText(NOTICE)).toHaveCount(0);
  await expect(second.getByLabel("Group name")).toHaveValue("Renamed by Grier");
  await other.close();
});

test("a change the form does not write lets the save through on one press, with no message", async ({ page, request }) => {
  const group = await leesGroup(request, "Approved while editing");
  const saves = groupSaves(page, group.id);
  await signInTo(page, "lee", `/groups/${group.id}`);
  await openEditor(page);
  await page.getByLabel(/^Location/).fill("Chapel");

  // An admin approves the group: its revision moves, nothing the form writes does.
  await group.approve();
  await page.getByRole("button", { name: "Save group" }).click();
  await expect(page.getByRole("button", { name: "Save group" })).toHaveCount(0);
  await expect.poll(async () => (await group.read()).location).toBe("Chapel");
  await expect(page.getByText(NOTICE)).toHaveCount(0);
  // The first send was refused, the group read, and the same save sent again.
  expect(saves).toEqual([409, 200]);
  expect((await group.read()).status).toBe("active");
});
