import { expect, test } from "@playwright/test";
import { apiAs, checkState, email, PASSWORD, signIn } from "./helpers";

export function adminRoomReadTests({ path, otherRead, evidence }: { path: string; otherRead: string; evidence: string }) {
  for (const change of ["rooms off", "admin demoted"] as const) {
    test(`${path} refuses a stale admin room read when ${change}`, async ({ page, request }, testInfo) => {
      const admin = await apiAs(request, "grier");
      const login = `${evidence}-${change === "rooms off" ? "off" : "member"}-${Date.now().toString(36)}`;
      const { user } = await admin("POST", "/admin/users", { ageBand: "adult", name: "Room Admin", email: email(login), password: PASSWORD, role: "admin" });
      await signIn(page, login);

      const url = `**/api/v1${path}`;
      const dispatch = Promise.withResolvers<void>();
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

          try {
            const refused = page.waitForResponse(url);
            dispatch.resolve();
            const response = await refused;
            expect(response.status()).toBe(404);
            expect(await response.headerValue("fold-session-checked")).toBeNull();
            await expect(rooms).toHaveCount(0);
            await expect(page.getByRole("tab", { name: /^Approvals\b/ })).toHaveAttribute("aria-selected", "true");
            await expect(page.getByText("Your session timed out.")).toHaveCount(0);
            const me = await page.request.get("/api/v1/auth/me");
            expect(me.ok()).toBe(true);
            expect(await me.json()).toMatchObject({ user: { email: email(login), role: "admin" } });
            await checkState(page, evidence, "rooms-off");
          } finally {
            await admin("PUT", "/admin/settings/rooms", { enabled: true });
          }
        } else {
          const { users } = await admin("GET", "/admin/users");
          const fresh = users.find((row: { id: number }) => row.id === user.id);
          await admin("PUT", `/admin/users/${user.id}`, { role: "member", revision: fresh.revision });
          const refused = page.waitForResponse(url);
          dispatch.resolve();
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
        dispatch.resolve();
      }
    });
  }
}
