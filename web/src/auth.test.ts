import { testUser } from "./test/fixtures";
import { stubService } from "./test/service";
import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Auth as AuthApi } from "./api/auth";
import { ApiError } from "./api/core";
import { AuthProvider, clearClientData, endSession, useAuth } from "./auth";
import type { User } from "./types";
import { fakeSub, installBrowser, uninstallBrowser } from "./test/push";
import { bootstrapAnswer, clearMarkerCookie } from "./test/credential";

stubService(AuthApi, () => ({ login: vi.fn(), logout: vi.fn() }));

// What GET /auth/bootstrap says about this browser now; a test changes it as the
// server's cookie would change. profile answers PUT /me (the time zone fill).
interface Served {
  user: User | null;
  canCompose?: boolean;
  token?: string;
  fails?: boolean;
}

let served: Served = { user: null };
let bootstraps = 0;
let profile: () => Promise<Response> | Response = () => Response.json({});

beforeEach(() => {
  served = { user: null };
  bootstraps = 0;
  profile = () => Response.json({});
  vi.stubGlobal("fetch", vi.fn(async (url: RequestInfo | URL) => {
    if (String(url) === "/api/v1/auth/bootstrap") {
      bootstraps++;

      if (served.fails) return new Response("{}", { status: 503 });

      return bootstrapAnswer({ token: served.token ?? `t-${served.user?.id ?? "anon"}`, user: served.user, canCompose: served.canCompose });
    }

    return String(url) === "/api/v1/me" ? profile() : Response.json({});
  }));
});

afterEach(() => {
  vi.unstubAllGlobals();
  clearMarkerCookie();
});

it("clears stale adult auth when a pending-teen login ends the server session", async () => {
  const adult: User = { id: 1, name: "Pat", displayName: "Pat", email: "pat@example.com", phone: "", hideContactFromLeaders: false, avatarUrl: "", avatarFocalX: 50, avatarFocalY: 50, role: "member", status: "active", hasPassword: true, accountType: "adult", timezone: "UTC" };
  served = { user: adult };
  vi.mocked(AuthApi.login).mockRejectedValue(new ApiError(403, "private", { code: "consent_required" }));
  const { result } = renderHook(useAuth, { wrapper: AuthProvider });
  await waitFor(() => expect(result.current.user?.id).toBe(1));
  await act(async () => {
    await expect(result.current.login("teen@example.com", "password1234")).rejects.toMatchObject({ status: 403 });
  });
  expect(result.current.user).toBeNull();
});

it("a session that ended elsewhere stops counting as signed in", async () => {
  const adult: User = { id: 1, name: "Pat", displayName: "Pat", email: "pat@example.com", phone: "", hideContactFromLeaders: false, avatarUrl: "", avatarFocalX: 50, avatarFocalY: 50, role: "member", status: "active", hasPassword: true, accountType: "adult", timezone: "UTC" };
  served = { user: adult, canCompose: true };
  const { result } = renderHook(useAuth, { wrapper: AuthProvider });
  await waitFor(() => expect(result.current.user?.id).toBe(1));
  act(() => endSession());
  expect(result.current.user).toBeNull();
  expect(result.current.canCompose).toBe(false);
});

describe("clearClientData", () => {
  it("empties local and session storage", () => {
    localStorage.setItem("fold-dismissed-notifications:7", "[]");
    sessionStorage.setItem("anything", "x");
    clearClientData();
    expect(localStorage.length + sessionStorage.length).toBe(0);
  });
});

it("says whether the person can compose, from the session and from signing in", async () => {
  const adult: User = { id: 1, name: "Lee", displayName: "Lee", email: "lee@example.com", phone: "", hideContactFromLeaders: false, avatarUrl: "", avatarFocalX: 50, avatarFocalY: 50, role: "member", status: "active", hasPassword: true, accountType: "adult", timezone: "UTC" };
  served = { user: adult, canCompose: false };
  vi.mocked(AuthApi.login).mockImplementation(async () => {
    served = { user: adult, canCompose: true, token: "after-login" };

    return { user: adult, canCompose: true };
  });
  const { result } = renderHook(useAuth, { wrapper: AuthProvider });
  await waitFor(() => expect(result.current.user?.id).toBe(1));
  expect(result.current.canCompose).toBe(false);
  const before = result.current.credential;
  await act(async () => { await result.current.login("lee@example.com", "password1234"); });
  expect(result.current.canCompose).toBe(true);
  // Signing in as the same person again is still a new credential lifetime.
  expect(result.current.credential).not.toBe(before);
  expect(result.current.credential).toMatchObject({ token: "after-login", viewer: 1 });
});

describe("logout", () => {
  afterEach(() => {
    uninstallBrowser();
    vi.restoreAllMocks();
  });

  it("sends this browser's push endpoint, then drops the subscription and closes what's on screen", async () => {
    const sub = fakeSub("https://fcm.googleapis.com/fcm/send/logout-test");
    const browser = installBrowser({ permission: "granted", subscription: sub });
    const order: string[] = [];
    vi.mocked(AuthApi.logout).mockImplementation(async () => {
      order.push("logout");

      return {};
    });
    sub.unsubscribe.mockImplementation(async () => {
      order.push("unsubscribe");

      return true;
    });
    vi.spyOn(console, "error").mockImplementation(() => {}); // jsdom: "navigation not implemented"
    const { result } = renderHook(useAuth, { wrapper: AuthProvider });
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => { await result.current.logout(); });
    expect(AuthApi.logout).toHaveBeenCalledWith(sub.endpoint);
    expect(order.sort()).toEqual(["logout", "unsubscribe"]);
    expect(browser.shown.every((n) => n.close.mock.calls.length === 1)).toBe(true);
    expect(browser.serviceWorker.register).not.toHaveBeenCalled();
  });

  it("sends no endpoint from a browser without notifications", async () => {
    vi.mocked(AuthApi.logout).mockResolvedValue({});
    vi.spyOn(console, "error").mockImplementation(() => {}); // jsdom: "navigation not implemented"
    const { result } = renderHook(useAuth, { wrapper: AuthProvider });
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => { await result.current.logout(); });
    expect(AuthApi.logout).toHaveBeenCalledWith(undefined);
  });
});

it("signedIn loads the new session and sends a pending teen to wait, however the server said so", async () => {
  const teen = testUser({ id: 2, status: "pending_consent", accountType: "teen" });
  const { result } = renderHook(useAuth, { wrapper: AuthProvider });
  await waitFor(() => expect(result.current.loading).toBe(false));
  served = { user: teen };
  let to = "";
  // The reset answers with the user alone; account requests and invitations say pendingConsent.
  await act(async () => { to = await result.current.signedIn({ user: teen }); });
  expect(to).toBe("/waiting-for-consent");
  await act(async () => { to = await result.current.signedIn({ pendingConsent: true }); });
  expect(to).toBe("/waiting-for-consent");
  await act(async () => { to = await result.current.signedIn({ user: { ...teen, status: "active" }, pendingConsent: false }); });
  expect(to).toBe("/");
  expect(bootstraps).toBe(4);
});

it("a session it cannot read counts as signed out", async () => {
  served = { user: null, fails: true };
  const { result } = renderHook(useAuth, { wrapper: AuthProvider });
  await waitFor(() => expect(result.current.loading).toBe(false));
  expect(result.current.user).toBeNull();
  expect(result.current.credential).toBeNull();
});

it("shows the person another tab signed in, and never an answer begun under the old credential", async () => {
  const first = testUser({ id: 1, timezone: "" });
  let zoneReply: (r: Response) => void = () => {};

  profile = () => new Promise<Response>((resolve) => { zoneReply = resolve; });
  served = { user: first };
  const { result } = renderHook(useAuth, { wrapper: AuthProvider });
  await waitFor(() => expect(result.current.user?.id).toBe(1));
  // The first sign-in's time zone fill is still on its way when the other tab
  // signs in as someone else; its hint reaches this one.
  served = { user: testUser({ id: 2, timezone: "UTC" }) };
  const otherTab = new BroadcastChannel("fold-credential");
  otherTab.postMessage("recheck");
  await waitFor(() => expect(result.current.user?.id).toBe(2));
  otherTab.close();
  await act(async () => { zoneReply(Response.json({ user: { ...first, timezone: "America/Chicago" } })); });
  expect(result.current.user?.id).toBe(2);
  expect(result.current.credential).toMatchObject({ viewer: 2 });
});
