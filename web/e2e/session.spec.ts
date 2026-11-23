import { expect, test, type Page } from "@playwright/test";
import { PASSWORD, apiAs, email, signIn, signOut } from "./helpers";

// Grier's Oct 10 reports: logout left the previous session on screen, and
// an admin signing in after a member created a group saw no notice.

test("logout clears the session's data, including through back", async ({ page }) => {
  await signIn(page, "sam");
  const search = page.getByRole("searchbox", { name: "Search groups" });
  await search.fill("circle");
  await expect(page.getByRole("heading", { name: "Members-only circle" })).toBeVisible();
  await page.getByRole("heading", { name: "Invite-only circle" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Invite-only circle" })).toBeVisible();
  // Back to the listing, where Grier was when the stale screen stayed up.
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Home" }).click();
  await search.fill("circle");
  await expect(page.getByRole("heading", { name: "Members-only circle" })).toBeVisible();
  await page.evaluate(() => sessionStorage.setItem("probe", "x"));
  const session = async () => (await page.context().cookies()).find((c) => c.name === "fold-session")?.value;
  const before = await session();

  await signOut(page);
  // The first load renders two live regions (the count line plus the
  // skeleton's label), which fails an unscoped status locator in strict
  // mode. Scope to the count line, whose text settles on the assertion.
  await expect(page.locator("#groups p[role='status']")).toHaveText(/\d+ groups?$/);

  for (const text of ["Members-only circle", "Invite-only circle", "Sam Member", "Log out"]) {
    await expect(page.getByText(text)).toHaveCount(0);
  }

  expect(await page.evaluate(() => localStorage.length + sessionStorage.length)).toBe(0);
  // The server session is over: the old cookie is gone (the fresh page got
  // a new anonymous one for its CSRF token) and the API sees no one.
  await search.fill("Room 201 class");
  await expect(page.getByRole("heading", { name: "Room 201 class" })).toBeVisible();
  expect(await session()).not.toBe(before);
  expect((await page.request.get("/api/v1/auth/me")).status()).toBe(401);

  // Back to the invite-only group's URL, then the listing before it: each
  // loads fresh and signed out.
  for (const url of [/\/groups\/\d+$/, /\/\?search=circle$/]) {
    await page.goBack();
    await expect(page).toHaveURL(url);
    await expect(page.getByRole("link", { name: "Log in" })).toBeVisible();

    for (const text of ["Members-only circle", "Invite-only circle", "Sam Member"]) {
      await expect(page.getByText(text)).toHaveCount(0);
    }
  }
});

test("the shell and API responses are never stored", async ({ page, request }) => {
  const shell = await page.goto("/groups/1");
  expect(shell?.headers()["cache-control"]).toBe("no-store");
  expect((await request.get("/api/v1/groups")).headers()["cache-control"]).toBe("no-store");
});

test("an admin signing in after a member's new group sees the notice", async ({ page, request }) => {
  await signIn(page, "sam");
  await page.goto("/groups/new");
  await page.getByLabel("Group name").fill("Sam's new study");
  await page.getByRole("button", { name: "Create group" }).click();
  await expect(page.getByRole("heading", { name: /An admin will review “Sam's new study”/ })).toBeVisible();
  await signOut(page);

  await signIn(page, "grier");
  const bell = page.getByRole("button", { name: /Alerts/ });
  await expect(bell).toHaveAccessibleName(/waiting on you/);
  await bell.click();
  const notice = page.getByRole("link", { name: /Sam Member started a group: Sam's new study/ });
  await expect(notice).toBeVisible();

  // Opening it doesn't clear a task. Another member's group arriving while
  // signed in is its own row on the next navigation.
  await notice.click();
  await expect(page).toHaveURL(/tab=approvals/);
  const sam = await apiAs(request, "sam");
  await sam("POST", "/groups", { name: "Another study", description: "", tags: [], imageUrl: "", location: "", visibility: "public", memberLimit: null });
  await page.getByRole("link", { name: "My groups" }).click();
  await bell.click();
  await expect(page.getByRole("link", { name: /started a group: Another study/ })).toBeVisible();
  await expect(page.getByRole("link", { name: /started a group: Sam's new study/ })).toBeVisible();

  // Leave nothing pending for other specs.
  const again = await apiAs(request, "grier"); // the request context is Sam's now
  const left = await again("GET", "/admin/pending");

  for (const g of left.groups) await again("POST", `/admin/groups/${g.id}/approve`, {});
});

test("an admin creating a group sees Group created", async ({ page }) => {
  await signIn(page, "grier");
  await page.goto("/groups/new");
  await page.getByLabel("Group name").fill("Admin's study");
  await page.getByRole("button", { name: "Create group" }).click();
  await expect(page.getByRole("heading", { name: "Group created" })).toBeVisible();
  await expect(page.getByText("Pending approval")).toHaveCount(0);
  await page.getByRole("link", { name: "Go to the group" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Admin's study" })).toBeVisible();
});

// The browser credential (M8.1): what a tab's bootstrap reads, and whether the
// marker cookie script sees is the one the server answered with.
async function credentialIn(page: Page) {
  return page.evaluate(async () => {
    const res = await fetch("/api/v1/auth/bootstrap", { credentials: "include" });
    const body = await res.json();

    return { token: String(body.csrfToken), marker: String(body.credentialMarker), user: body.user ? Number(body.user.id) : null, ack: res.headers.get("Fold-Session-Checked"), cookie: document.cookie };
  });
}

for (const hint of ["delivered", "withheld"] as const) {
  test(`a password change in one tab replaces the credential of every tab on the cookie (hint ${hint})`, async ({ page, browser, request }) => {
    const login = `credential-tabs-${hint}`;
    const admin = await apiAs(request, "grier");
    await admin("POST", "/admin/users", { ageBand: "adult", name: `Credential ${hint}`, email: email(login), password: PASSWORD, role: "member" });

    // Tab A never hears another tab's hint when it is withheld.
    if (hint === "withheld") await page.addInitScript(() => { Reflect.deleteProperty(window, "BroadcastChannel"); });
    await signIn(page, login);
    const tabB = await page.context().newPage();
    const isolated = await browser.newContext();
    const elsewhere = await isolated.newPage();
    await signIn(elsewhere, login);
    await page.goto("/profile/details");
    await expect(page.getByLabel("Full name")).toHaveValue(`Credential ${hint}`);

    const before = await credentialIn(page);
    expect(before.ack).toBe(`v2 ${before.marker} ${before.user}`);
    expect(before.cookie).toContain(`fold-credential=${before.marker}`);

    await tabB.goto("/profile/security?tab=password");
    await tabB.getByLabel("Current password").fill(PASSWORD);
    await tabB.getByLabel("New password", { exact: true }).fill("replacement-password-1");
    await tabB.getByLabel("Confirm new password").fill("replacement-password-1");
    await tabB.getByRole("button", { name: "Change password" }).click();
    await expect(tabB.getByText("Password changed.")).toBeVisible();

    // The server refuses A's pinned old token before any work, setting nothing.
    const refused = await page.request.get("/api/v1/auth/me", { headers: { "Fold-Session-Check": "v2", "X-CSRF-Token": before.token } });
    expect(refused.status()).toBe(403);
    expect((await refused.json()).code).toBe("session_expired");
    expect(refused.headers()["set-cookie"]).toBeUndefined();

    // A's next write, made in the app with no reload, goes out with the new
    // credential for the same person and saves.
    await page.getByLabel("Full name").fill(`Credential ${hint} renamed`);
    await page.getByRole("button", { name: "Save profile" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Profile saved." })).toBeVisible();

    const after = await credentialIn(page);
    expect(after.token).not.toBe(before.token);
    expect(after.marker).not.toBe(before.marker);
    expect(after.user).toBe(before.user);
    expect(after.cookie).toContain(`fold-credential=${after.marker}`);

    // The isolated browser's session ended with the password change.
    expect((await elsewhere.request.get("/api/v1/auth/me")).status()).toBe(401);
    await isolated.close();
  });
}

// A deployment that switched between HTTP and HTTPS on one host leaves the old
// setting's marker in the browser beside the current one; script that sees
// both trusts neither. The next load's bootstrap expires the old name, so the
// real jar holds one marker, the one the server answers with.
test("a marker left under the other name by a switch between HTTP and HTTPS is expired on the next load", async ({ page, request }) => {
  const login = "credential-rollover";
  const admin = await apiAs(request, "grier");
  await admin("POST", "/admin/users", { ageBand: "adult", name: "Credential rollover", email: email(login), password: PASSWORD, role: "member" });
  await signIn(page, login);
  const markers = async () => (await page.context().cookies()).filter((c) => c.name === "fold-credential" || c.name === "__Host-fold-credential");
  const [current] = await markers();
  const other = current.name === "fold-credential" ? "__Host-fold-credential" : "fold-credential";
  await page.context().addCookies([{ name: other, value: "left-from-the-switch", url: new URL(page.url()).origin, secure: other.startsWith("__Host-") }]);
  expect((await markers()).map((c) => c.name).sort()).toEqual(["__Host-fold-credential", "fold-credential"]);

  await page.reload();
  await expect.poll(async () => (await markers()).map((c) => c.name)).toEqual([current.name]);
  const now = await credentialIn(page);
  expect(now.ack).toBe(`v2 ${now.marker} ${now.user}`);
  expect(now.cookie.split("; ").filter((part) => part.includes("fold-credential="))).toEqual([`${current.name}=${now.marker}`]);

  // The app goes on working with that credential.
  await page.goto("/profile/details");
  await page.getByLabel("Full name").fill("Credential rollover renamed");
  await page.getByRole("button", { name: "Save profile" }).click();
  await expect(page.getByRole("status").filter({ hasText: "Profile saved." })).toBeVisible();
});

// A same-name marker copy at another path, left by something else on this
// host, is a separate cookie the browser sends only under that path. No
// response can name it, so it stays; script on a page under its path sees two
// markers and trusts neither (participating requests there are refused unsent:
// core.test.ts). Plain requests go on there, and other pages see only the
// server's marker.
test("a marker copy at another path stays, and only the pages under it see two markers", async ({ page, request }) => {
  const login = "credential-copy";
  const admin = await apiAs(request, "grier");
  await admin("POST", "/admin/users", { ageBand: "adult", name: "Credential copy", email: email(login), password: PASSWORD, role: "member" });
  await signIn(page, login);
  const origin = new URL(page.url()).origin;
  const markers = async () => (await page.context().cookies()).filter((c) => c.name === "fold-credential" || c.name === "__Host-fold-credential");
  const [own] = await markers();
  await page.context().addCookies([{ name: "fold-credential", value: "copy", domain: new URL(origin).hostname, path: "/groups" }]);
  const seen = () => page.evaluate(() => document.cookie.split("; ").filter((part) => part.includes("fold-credential=")).sort());

  await page.goto("/groups/new");
  const there = await credentialIn(page);
  expect(there.user).not.toBeNull();
  expect(await seen()).toEqual([`${own.name}=${there.marker}`, "fold-credential=copy"].sort());
  await page.getByLabel("Group name").fill("Copy study");
  await page.getByRole("button", { name: "Create group" }).click();
  await expect(page.getByRole("heading", { name: "Thanks! An admin will review “Copy study”." })).toBeVisible();
  expect((await markers()).map((c) => `${c.name} ${c.path}`).sort()).toEqual([`${own.name} /`, "fold-credential /groups"].sort());

  await page.goto("/profile/details");
  expect(await seen()).toEqual([`${own.name}=${there.marker}`]);
  await page.getByLabel("Full name").fill("Credential copy renamed");
  await page.getByRole("button", { name: "Save profile" }).click();
  await expect(page.getByRole("status").filter({ hasText: "Profile saved." })).toBeVisible();
});
