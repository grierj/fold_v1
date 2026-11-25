import * as schema from "valibot";
import { expect, test, type APIRequestContext } from "@playwright/test";
import { apiAs, email, PASSWORD, signInTo } from "./helpers";

// DEC-169: a message's edit names the read it was made from, and the send confirmation names the list the sender
// reviewed. A change to who gets it between the review and the press is shown, and the sender sends to the list as it
// is now or reviews it again; an edit over another tab's says the changes could not be saved.
const NOTICE = "This message changed while you were editing, so your changes could not be saved. This is the message as it is now.";

const withId = schema.looseObject({ id: schema.number() });

// A group Lee leads, approved, with one member besides Lee. One request context holds one sign-in at a time, so each
// person signs in again right before what they do.
async function leesGroup(request: APIRequestContext, name: string) {
  const lee = await apiAs(request, "lee");
  const { group } = schema.parse(schema.looseObject({ group: withId }), await lee("POST", "/groups", { name, visibility: "public", location: "Hall", tags: [] }));
  await (await apiAs(request, "grier"))("POST", `/admin/groups/${group.id}/approve`, {});
  const join = async (display: string) => {
    const grier = await apiAs(request, "grier");
    const handle = `review-${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
    const { user } = schema.parse(schema.looseObject({ user: withId }), await grier("POST", "/admin/users", { name: display, email: email(handle), password: PASSWORD, ageBand: "adult" }));
    await grier("POST", `/admin/groups/${group.id}/members`, { userId: user.id, role: "member" });
  };
  await join("Ruth Early");

  return { id: group.id, join };
}

test("a message sender sees who joined since they looked, then sends to the list as it is now, at 390px", async ({ page, request }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const group = await leesGroup(request, `Review ${Date.now()}`);
  const subject = `Potluck ${Date.now()}`;
  const schedules: number[] = [];
  page.on("response", (res) => {
    if (res.request().method() === "POST" && /\/api\/v1\/messages\/\d+\/schedule$/.test(new URL(res.url()).pathname)) schedules.push(res.status());
  });

  await signInTo(page, "lee", `/messages/new?group=${group.id}`);
  await expect(page.getByRole("heading", { name: "New message", level: 1 })).toBeVisible();
  await page.getByRole("textbox", { name: "Subject" }).fill(subject);
  await page.getByRole("textbox", { name: "Message" }).click();
  await page.keyboard.type("Bring a dish to share.");
  await page.getByRole("button", { name: "Send now" }).click();
  const ask = page.getByRole("group", { name: "Send to 2 people now?" });
  await expect(ask).toBeVisible();

  // While the question is on screen, an admin adds someone to the group.
  await group.join("Pat Newcomer");
  await ask.getByRole("button", { name: "Send now" }).click();

  const step = page.getByRole("group", { name: "Who gets this changed since you looked." });
  await expect(step).toBeVisible();
  await expect(step).toContainText("Pat Newcomer will now get this.");
  await expect(step).toContainText("Send to 3 people now?");
  const go = step.getByRole("button", { name: "Send to the list as it is now" });
  await expect(go).toBeFocused();
  await expect(step.getByRole("button", { name: "Review the whole list" })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await step.screenshot({ path: "/var/tmp/msgslice/changed-list-390.png" });
  await page.screenshot({ path: "/var/tmp/msgslice/changed-list-390-page.png", fullPage: true });
  expect(schedules).toEqual([409]);

  await go.click();
  await expect(page.getByRole("heading", { name: subject, level: 1 })).toBeVisible();
  await expect(page.getByRole("textbox", { name: "Subject" })).toHaveCount(0);
  expect(schedules).toEqual([409, 200]);
});

test("a message edited in two tabs: the second save says the changes could not be saved and shows the first", async ({ page, browser, request }) => {
  const group = await leesGroup(request, `Two tabs ${Date.now()}`);
  const lee = await apiAs(request, "lee");
  const { message } = schema.parse(schema.looseObject({ message: withId }), await lee("POST", "/messages", {
    channel: "email", subject: "First words", bodyHtml: "<p>Hello</p>", audience: { type: "group", groupId: group.id }, pushAlert: true,
  }));

  const other = await browser.newContext({ baseURL: test.info().project.use.baseURL, viewport: { width: 390, height: 844 } });
  const second = await other.newPage();
  await signInTo(second, "lee", `/messages/${message.id}`);
  await expect(second.getByRole("textbox", { name: "Subject" })).toHaveValue("First words");

  await signInTo(page, "lee", `/messages/${message.id}`);
  await page.getByRole("textbox", { name: "Subject" }).fill("Words from the first tab");
  await page.getByRole("button", { name: "Save draft" }).click();
  await expect(page.getByText("Draft saved.")).toBeVisible();

  await second.getByRole("textbox", { name: "Subject" }).fill("Words from the second tab");
  await second.getByRole("button", { name: "Save draft" }).click();
  const notice = second.getByRole("status").filter({ hasText: NOTICE });
  await expect(notice).toBeFocused();
  await expect(second.getByRole("heading", { name: "Words from the first tab", level: 1 })).toBeVisible();
  await expect(second.getByRole("textbox", { name: "Subject" })).toHaveCount(0);

  // Editing again starts from what is stored, and the line goes.
  await second.getByRole("button", { name: "Edit" }).click();
  await expect(second.getByRole("textbox", { name: "Subject" })).toHaveValue("Words from the first tab");
  await expect(second.getByText(NOTICE)).toHaveCount(0);
  await other.close();
});
