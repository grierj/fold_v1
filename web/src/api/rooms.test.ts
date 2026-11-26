import { afterEach, expect, it, vi } from "vitest";
import { bootstrapAnswer, clearMarkerCookie, sentChecks, setMarkerCookie } from "../test/credential";
import { deferred } from "../test/keptPanel";
import { bootstrapCredential, type Credential } from "./core";
import { AdminRooms, RoomRequests } from "./rooms";

const endpoint = "/api/v1/admin/rooms/7/feed";
const oldViewer = { token: "before", user: { id: 5 } };
const newViewer = { token: "after", user: { id: 9 } };
const off = { hasToken: false, createdAt: null, revision: "off" };
const live = { hasToken: true, createdAt: "2026-10-30T15:00:00Z", revision: "on" };
const created = { ...live, url: "https://church.example/cal/rooms/7/secret.ics" };

afterEach(() => {
  vi.unstubAllGlobals();
  clearMarkerCookie();
});

it.each([
  ["read", (pin: Credential) => AdminRooms.getFeed(7, pin), Response.json({ error: "not found", code: "room_gone" }, { status: 404, headers: { "Fold-Session-Checked": "v2 m-before 5" } })],
  ["create", (pin: Credential) => AdminRooms.makeFeed(7, "off", pin), Response.json({ feed: created }, { headers: { "Fold-Session-Checked": "v2 m-before 5" } })],
  ["revoke", (pin: Credential) => AdminRooms.revokeFeed(7, "on", pin), Response.json({ feed: off }, { headers: { "Fold-Session-Checked": "v2 m-before 5" } })],
] as const)("discards a delayed %s answer when another tab changes the credential", async (_name, call, answer) => {
  let viewer = oldViewer;
  const reply = deferred<Response>();
  const dispatched = deferred<void>();

  const fetcher = vi.fn<typeof fetch>(async (url) => {
    if (String(url).endsWith("/auth/bootstrap")) return bootstrapAnswer(viewer);
    expect(url).toBe(endpoint);
    dispatched.resolve();

    return reply.promise;
  });

  vi.stubGlobal("fetch", fetcher);
  const { credential } = await bootstrapCredential(true);

  const pending = call(credential);
  await dispatched.promise;
  viewer = newViewer;
  setMarkerCookie("m-after");
  reply.resolve(answer);

  await expect(pending).rejects.toMatchObject({ code: "session_expired", unreadable: true });
  expect(sentChecks(fetcher.mock.calls)).toEqual([[endpoint, "v2", "before"]]);
  await bootstrapCredential();
});

it.each([
  { label: "still on", enabled: true, rotates: false, code: "server_error" },
  { label: "off", enabled: false, rotates: false, code: "rooms_off" },
  { label: "off under a replacement credential", enabled: false, rotates: true, code: "session_expired" },
])("checks the public switch after an unacknowledged feed read ($label)", async ({ enabled, rotates, code }) => {
  let viewer = oldViewer;
  const site = deferred<Response>();
  const siteStarted = deferred<void>();

  const fetcher = vi.fn<typeof fetch>(async (url) => {
    if (String(url).endsWith("/auth/bootstrap")) return bootstrapAnswer(viewer);

    if (url === "/api/v1/site") {
      siteStarted.resolve();

      return site.promise;
    }

    expect(url).toBe(endpoint);

    return Response.json({ error: "not found", code: "not_found" }, { status: 404 });
  });

  vi.stubGlobal("fetch", fetcher);
  const { credential } = await bootstrapCredential(true);

  const pending = AdminRooms.getFeed(7, credential);
  await siteStarted.promise;

  if (rotates) {
    viewer = newViewer;
    setMarkerCookie("m-after");
    await bootstrapCredential(true);
  }

  site.resolve(Response.json({ roomsEnabled: enabled }));
  await expect(pending).rejects.toMatchObject({ code, unreadable: code !== "rooms_off" });
  expect(sentChecks(fetcher.mock.calls)).toEqual([[endpoint, "v2", "before"], ["/api/v1/site", null, null]]);
  await bootstrapCredential();
});

const adminLists = [
  { path: "/admin/rooms/locations", read: AdminRooms.locations },
  { path: "/admin/room-requests", read: RoomRequests.list },
];

it.each(adminLists)("accepts $path only with the captured admin acknowledgment", async ({ path, read }) => {
  let acknowledged: string | undefined;
  const answer = { marker: "room-read" };

  const fetcher = vi.fn<typeof fetch>(async (url) => {
    if (String(url).endsWith("/auth/bootstrap")) return bootstrapAnswer({ token: "room", user: { id: 5 } });

    return Response.json(answer, { headers: acknowledged ? { "Fold-Session-Checked": acknowledged } : {} });
  });

  vi.stubGlobal("fetch", fetcher);
  await bootstrapCredential(true);

  await expect(read()).rejects.toMatchObject({ code: "session_expired" });
  acknowledged = "v2 m-room 8";
  await expect(read()).rejects.toMatchObject({ code: "session_expired" });
  acknowledged = "v2 m-room 5";
  await expect(read()).resolves.toEqual(answer);
  expect(sentChecks(fetcher.mock.calls)).toEqual(Array.from({ length: 3 }, () => ["/api/v1" + path, "v2", "room"]));
});
