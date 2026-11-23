import * as schema from "valibot";
import { expect, test, type Page } from "@playwright/test";
import { mkdirSync, writeFileSync } from "node:fs";
import { apiAs, signIn, signInTo } from "./helpers";

// M1.5: capacity and waitlist. A leader sets capacity 1 with the waitlist
// on. One member goes, a second sees position 1, the first cancels, and
// the second is going with one bell item.
test("the waitlist arc: capacity 1, position 1, cancel, promoted", async ({ page, request }) => {
  const admin = await apiAs(request, "grier");

  const { group } = schema.parse(schema.looseObject({ group: schema.looseObject({ id: schema.number() }) }), (await admin("POST", "/groups", {
    name: "Waitlist club", description: "", tags: [], imageUrl: "", location: "Hall",
    visibility: "public", memberLimit: null,
  })));

  const { users } = schema.parse(schema.looseObject({ users: schema.array(schema.looseObject({ id: schema.number(), email: schema.string() })) }), (await admin("GET", "/admin/users")));
  const id = (name: string) => users.find((u) => u.email === `${name}@example.com`)!.id;

  for (const name of ["sam", "lee"]) {
    await admin("POST", `/admin/groups/${group.id}/members`, { userId: id(name), role: "member" });
  }

  const when = new Date(Date.now() + 30 * 24 * 60 * 60 * 1000);
  const ymd = when.toISOString().slice(0, 10);

  const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), (await admin("POST", `/groups/${group.id}/events`, {
    title: "Tiny supper",
    startsLocal: `${ymd}T19:00`,
    durationMinutes: 60,
    tzid: "UTC",
    capacity: 1,
    waitlistEnabled: true,
  })));

  const url = `/events/${event.id}`;

  // Sam takes the one seat.
  await signIn(page, "sam");
  await page.goto(url);
  await page.getByRole("button", { name: "Going" }).click();
  await expect(page.getByRole("button", { name: "Going" })).toHaveAttribute("aria-pressed", "true");

  // Lee sees position 1 on the waitlist.
  await signIn(page, "lee");
  await page.goto(url);
  await page.getByRole("button", { name: "Going" }).click();
  await expect(page.getByText(/position 1/)).toBeVisible();

  // Sam cancels; Lee is promoted.
  await signIn(page, "sam");
  await page.goto(url);
  await page.getByRole("button", { name: "Can't go" }).click();
  await expect(page.getByRole("button", { name: "Can't go" })).toHaveAttribute("aria-pressed", "true");

  await signIn(page, "lee");
  await page.goto(url);
  await expect(page.getByRole("button", { name: "Going" })).toHaveAttribute("aria-pressed", "true");
  const bell = page.getByRole("button", { name: /Alerts/ });
  await expect(bell).toHaveAccessibleName(/unread/);
  await bell.click();
  const panel = page.locator("#notif-panel");
  await expect(panel.getByText("You're in: Tiny supper", { exact: true })).toBeVisible();
});

// M8.1 on the exact-date page: a waiting member's position and the conditional promotion copy, then
// Going after the holder leaves outside the cutoff, from each person's own session and JSON. Inside
// the cutoff the waiter stays waiting until a host promotes by hand.
for (const near of [false, true]) {
  test(`the date page shows the waitlist and then Going after ${near ? "a host promotes inside the cutoff" : "the seat is freed"}`, async ({ browser, request }) => {
    test.setTimeout(90_000);
    const admin = await apiAs(request, "grier");
    const tag = near ? "near" : "far";
    const ids: Record<string, number> = {};

    for (const who of ["holder", "waiter"]) {
      const { user } = schema.parse(schema.looseObject({ user: schema.looseObject({ id: schema.number() }) }), await admin("POST", "/admin/users", { name: `Wait ${who} ${tag}`, email: `wait-${who}-${tag}@example.com`, password: "password1234", ageBand: "adult", role: "member" }));
      ids[who] = user.id;
    }

    const { group } = schema.parse(schema.looseObject({ group: schema.looseObject({ id: schema.number() }) }), await admin("POST", "/groups", { name: `Date waitlist ${tag}`, description: "", tags: [], imageUrl: "", location: "Hall", visibility: "public", memberLimit: null }));

    for (const id of Object.values(ids)) await admin("POST", `/admin/groups/${group.id}/members`, { userId: id, role: "member" });
    // Inside the 2-hour cutoff, a freed seat is offered to a host, not moved automatically.
    const starts = near ? new Date(Date.now() + 60 * 60_000) : new Date(Date.now() + 20 * 86400_000);
    const startsLocal = starts.toISOString().slice(0, 16);
    const title = `Tiny date supper ${tag}`;
    const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), await admin("POST", `/groups/${group.id}/events`, { title, startsLocal, durationMinutes: 60, tzid: "UTC", capacity: 1, waitlistEnabled: true }));
    const date = startsLocal.slice(0, 10);
    const path = `/events/${event.id}/dates/${date}`;

    if (process.env.DATE_PROOF_DIR) mkdirSync(`${process.env.DATE_PROOF_DIR}/viewport`, { recursive: true });

    const open = async (who: string) => {
      const ctx = await browser.newContext({ baseURL: test.info().project.use.baseURL, viewport: { width: 390, height: 844 } });
      const page = await ctx.newPage();
      await signInTo(page, `wait-${who}-${tag}`, path);
      await expect(page.getByRole("heading", { level: 1, name: title })).toBeVisible();

      return page;
    };

    const going = (page: Page) => page.getByRole("region", { name: "Answer this date" }).getByRole("button", { name: "Going" });
    const holder = await open("holder");
    await going(holder).click();
    await expect(holder.getByRole("status").filter({ hasText: "You're going to this date." })).toBeVisible();

    const waiter = await open("waiter");
    await expect(waiter.getByText("If it’s full, Going puts you on the waitlist.")).toBeVisible();
    await going(waiter).click();
    const status = waiter.getByRole("status").filter({ hasText: "You're on the waitlist." });
    await expect(status).toContainText("Your position on this date's waitlist is 1.");
    await expect(status).toContainText("If a space opens and your party fits, you may be moved to Going. Fold will let you know if that happens.");
    await expect(status).not.toContainText(/deadline|confirm|email|guarantee|next in line/i);
    await expect(going(waiter)).toHaveAttribute("aria-pressed", "true");
    const before = (await (await waiter.request.get(`/api/v1${path}`)).json()).occurrence;
    expect(before).toMatchObject({ myRegistration: "waitlisted", myWaitlistPosition: 1, myAttendanceMode: "in_person" });

    if (process.env.DATE_PROOF_DIR) await waiter.screenshot({ path: `${process.env.DATE_PROOF_DIR}/viewport/waitlisted-${tag}-390.png` });

    // The holder leaves from their own date page.
    await holder.getByRole("region", { name: "Answer this date" }).getByRole("button", { name: "Can't go" }).click();
    await expect(holder.getByRole("status").filter({ hasText: "You said you can't go." })).toBeVisible();

    if (near) {
      const still = (await (await waiter.request.get(`/api/v1${path}`)).json()).occurrence;
      expect(still).toMatchObject({ myRegistration: "waitlisted" });
      const { occurrences } = schema.parse(schema.looseObject({ occurrences: schema.array(schema.looseObject({ attendees: schema.array(schema.looseObject({ userId: schema.number(), registrationId: schema.number(), status: schema.string() })) })) }), await admin("GET", `/events/${event.id}`));
      const row = occurrences[0].attendees.find((a) => a.userId === ids.waiter)!;
      expect(row.status).toBe("waitlisted");
      await admin("POST", `/events/${event.id}/registrations/promote`, { registrationId: row.registrationId, date });
    }

    await waiter.evaluate(() => window.dispatchEvent(new Event("focus")));
    await expect(waiter.getByRole("status").filter({ hasText: "You're going to this date." })).toBeVisible();
    await expect(waiter.getByText(/waitlist is \d/)).toHaveCount(0);
    const after = (await (await waiter.request.get(`/api/v1${path}`)).json()).occurrence;
    expect(after.myRegistration).toBe("going"); expect(after).not.toHaveProperty("myWaitlistPosition");
    const notices = await (await waiter.request.get("/api/v1/notifications")).json();
    expect(JSON.stringify(notices)).toContain(`You're in: ${title}`);

    if (process.env.DATE_PROOF_DIR) {
      await waiter.screenshot({ path: `${process.env.DATE_PROOF_DIR}/viewport/promoted-${tag}-390.png` });
      writeFileSync(`${process.env.DATE_PROOF_DIR}/viewport/waitlist-${tag}.json`, JSON.stringify({ before: { myRegistration: before.myRegistration, myWaitlistPosition: before.myWaitlistPosition, myAttendanceMode: before.myAttendanceMode }, after: { myRegistration: after.myRegistration, myWaitlistPosition: after.myWaitlistPosition ?? "absent" }, manualPromotion: near, notice: `You're in: ${title}` }, null, 2));
    }

    await holder.context().close(); await waiter.context().close();
  });
}
