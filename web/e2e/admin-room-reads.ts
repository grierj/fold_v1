import { expect, test } from "@playwright/test";
import { apiAs, checkState, email, PASSWORD, signIn } from "./helpers";

function responseGate() {
  let open = () => {};
  const promise = new Promise<void>((resolve) => { open = resolve; });

  return { promise, open };
}

export function adminRoomReadTests({ path, otherRead, evidence }: { path: string; otherRead: string; evidence: string }) {
  for (const change of ["rooms off", "admin demoted"] as const) {
    test(`${path} refuses a stale admin room read when ${change}`, async ({ page, request }, testInfo) => {
      const admin = await apiAs(request, "grier");
      const login = `${evidence}-${change === "rooms off" ? "off" : "member"}-${Date.now().toString(36)}`;
      const { user } = await admin("POST", "/admin/users", { ageBand: "adult", name: "Room Admin", email: email(login), password: PASSWORD, role: "admin" });
      await signIn(page, login);

      const url = `**/api/v1${path}`;
      const dispatch = responseGate();
      await page.route(url, async (route) => {
        await dispatch.promise;
        await route.continue();
      }, { times: 1 });

      try {
        const started = page.waitForRequest(url);
        const sibling = page.waitForResponse(`**/api/v1${otherRead}`);
        await page.goto("/admin?tab=rooms");
        expect((await started).headers()["fold-session-check"]).toBe("v2");
        expect((await sibling).status()).toBe(200);
        const rooms = page.getByRole("tab", { name: /^Rooms\b/ });
        await expect(rooms).toHaveAttribute("aria-selected", "true");

        if (change === "rooms off") {
          await page.screenshot({ path: testInfo.outputPath("rooms-on-390.png"), fullPage: true });
          await admin("PUT", "/admin/settings/rooms", { enabled: false });

          const releaseSite = responseGate();
          const siteUrl = "**/api/v1/site";
          await page.route(siteUrl, async (route) => {
            await releaseSite.promise;
            await route.continue();
          }, { times: 1 });

          try {
            const refused = page.waitForResponse(url);
            const recheck = page.waitForRequest(siteUrl);
            dispatch.open();
            const response = await refused;
            expect(response.status()).toBe(404);
            expect(await response.headerValue("fold-session-checked")).toBeNull();
            await recheck;
            // The error from the rejected room response would render before /site answers.
            await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
            await expect(page.getByRole("alert").filter({ hasText: "Your session timed out" })).toHaveCount(0);
            await expect(rooms).toHaveAttribute("aria-selected", "true");
            const focused = path.endsWith("/locations") ? page.getByRole("tabpanel", { name: /^Rooms\b/ }) : rooms;
            await focused.focus();
            releaseSite.open();

            const approvals = page.getByRole("tab", { name: /^Approvals\b/ });
            await expect(rooms).toHaveCount(0);
            await expect(approvals).toHaveAttribute("aria-selected", "true");
            await expect(approvals).toBeFocused();
            const me = await page.request.get("/api/v1/auth/me");
            expect(me.ok()).toBe(true);
            expect(await me.json()).toMatchObject({ user: { email: email(login), role: "admin" } });
            await checkState(page, evidence, "rooms-off");
          } finally {
            releaseSite.open();
            await admin("PUT", "/admin/settings/rooms", { enabled: true });
          }
        } else {
          const { users } = await admin("GET", "/admin/users");
          const fresh = users.find((row: { id: number }) => row.id === user.id);
          await admin("PUT", `/admin/users/${user.id}`, { role: "member", revision: fresh.revision });
          const refused = page.waitForResponse(url);
          dispatch.open();
          const response = await refused;
          expect(response.status()).toBe(401);
          expect(await response.headerValue("fold-session-checked")).toBeNull();
          await expect(page).toHaveURL(/\/login\b/);
          expect((await page.request.get("/api/v1/auth/me")).status()).toBe(401);
          expect(await (await page.request.get("/api/v1/site")).json()).toMatchObject({ roomsEnabled: true });
          await signIn(page, login);
          expect((await page.request.get(`/api/v1${path}`)).status()).toBe(403);
          await page.goto("/admin?tab=rooms");
          await expect(page.getByText("This page is for church admins only.")).toBeVisible();
        }
      } finally {
        dispatch.open();
      }
    });
  }
}
