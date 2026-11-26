import { afterEach, expect, it, vi } from "vitest";
import { bootstrapAnswer, clearMarkerCookie, sentChecks } from "../test/credential";

afterEach(() => {
  vi.unstubAllGlobals();
  clearMarkerCookie();
});

const routes = [{ path: "/admin/rooms/locations", read: (rooms: typeof import("./rooms")) => rooms.AdminRooms.locations() }, { path: "/admin/room-requests", read: (rooms: typeof import("./rooms")) => rooms.RoomRequests.list() }];

it.each(routes)("accepts $path only with the captured admin acknowledgment", async ({ path, read }) => {
  vi.resetModules();
  const rooms = await import("./rooms");
  const { bootstrapCredential } = await import("./core");
  let acknowledged: string | undefined;
  const answer = { marker: "room-read" };


  const fetcher = vi.fn<typeof fetch>(async (url) => {
    if (String(url).endsWith("/auth/bootstrap")) return bootstrapAnswer({ token: "room", user: { id: 5 } });

    return Response.json(answer, { headers: acknowledged ? { "Fold-Session-Checked": acknowledged } : {} });
  });


  vi.stubGlobal("fetch", fetcher);
  await bootstrapCredential();

  await expect(read(rooms)).rejects.toMatchObject({ code: "session_expired" });
  acknowledged = "v2 m-room 8";
  await expect(read(rooms)).rejects.toMatchObject({ code: "session_expired" });
  acknowledged = "v2 m-room 5";
  await expect(read(rooms)).resolves.toEqual(answer);
  expect(sentChecks(fetcher.mock.calls)).toEqual(Array.from({ length: 3 }, () => ["/api/v1" + path, "v2", "room"]));
});
