import { bootstrapAnswer, clearMarkerCookie, setMarkerCookie } from "../../test/credential";
import { renderWithAuth } from "../../test/auth";
import { stubService } from "../../test/service";
import { AdminRooms } from "../../api/rooms";
import { AuthProvider, useAuth } from "../../auth";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError, bootstrapCredential } from "../../api/core";
import type { Room } from "../../types";
import { RoomFeedPanel } from "./RoomFeedPanel";

const getFeed = vi.fn();
const makeFeed = vi.fn();
const revokeFeed = vi.fn();

const hall: Room = {
  id: 7, name: "Fellowship Hall", building: "Main", capacity: 80, features: "", notes: "",
  active: true, restricted: false, defaultSetupMinutes: 0, defaultTeardownMinutes: 0, revision: "rev-7",
};

const URL_ = "https://church.example/cal/rooms/7/s3cret.ics";

describe("RoomFeedPanel (M2.2b)", () => {
  stubService(AdminRooms, () => ({ getFeed: (id) => getFeed(id), makeFeed: (id, revision) => makeFeed(id, revision), revokeFeed: (id, revision) => revokeFeed(id, revision) }));

  beforeEach(() => {
    getFeed.mockReset();
    makeFeed.mockReset();
    revokeFeed.mockReset();
  });
  afterEach(() => vi.restoreAllMocks());

  it("a failed load offers Try again, never Create", async () => {
    getFeed.mockRejectedValueOnce(new ApiError(500, "boom"));
    getFeed.mockResolvedValueOnce({ feed: { hasToken: true, createdAt: "2026-10-01T15:00:00Z", revision: "first" } });
    render(<RoomFeedPanel room={hall} onClose={() => {}} onGone={() => {}} />);
    const retry = await screen.findByRole("button", { name: "Try again" });
    expect(screen.queryByRole("button", { name: "Create calendar link" })).toBeNull();
    await userEvent.click(retry);
    expect(await screen.findByRole("button", { name: "Make a new link" })).toBeInTheDocument();
    expect(screen.getByText(/A calendar link is on, made/)).toBeInTheDocument();
  });

  it("makes a link and shows it once", async () => {
    getFeed.mockResolvedValue({ feed: { hasToken: false, createdAt: null, revision: "off" } });
    makeFeed.mockResolvedValue({ feed: { hasToken: true, createdAt: "2026-10-30T15:00:00Z", revision: "first", url: URL_ } });
    const { unmount } = render(<RoomFeedPanel room={hall} onClose={() => {}} onGone={() => {}} />);
    expect(await screen.findByText(/doesn.t have a calendar link yet/)).toBeInTheDocument();
    expect(screen.getByText(/members-only and invite-only dates, appear as Reserved/)).toBeVisible();
    await userEvent.click(screen.getByRole("button", { name: "Create calendar link" }));
    expect(makeFeed).toHaveBeenCalledWith(7, "off");
    expect(await screen.findByLabelText("Calendar link")).toHaveValue(URL_);
    expect(screen.getByText(/Save this link now; we won't show it again/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy link" })).toBeInTheDocument();
    unmount();

    // Opened again, the link is on but never shown.
    getFeed.mockResolvedValue({ feed: { hasToken: true, createdAt: "2026-10-30T15:00:00Z", revision: "first" } });
    render(<RoomFeedPanel room={hall} onClose={() => {}} onGone={() => {}} />);
    expect(await screen.findByText(/A calendar link is on/)).toBeInTheDocument();
    expect(screen.queryByDisplayValue(URL_)).toBeNull();
  });

  it("a new link and turning it off both ask first", async () => {
    getFeed.mockResolvedValue({ feed: { hasToken: true, createdAt: "2026-10-30T15:00:00Z", revision: "first" } });
    makeFeed.mockResolvedValue({ feed: { hasToken: true, createdAt: "2026-10-30T16:00:00Z", revision: "second", url: URL_ } });
    revokeFeed.mockResolvedValue({ feed: { hasToken: false, createdAt: null, revision: "off" } });
    const ask = vi.spyOn(window, "confirm").mockReturnValueOnce(false).mockReturnValue(true);
    render(<RoomFeedPanel room={hall} onClose={() => {}} onGone={() => {}} />);

    await userEvent.click(await screen.findByRole("button", { name: "Make a new link" }));
    expect(ask).toHaveBeenCalledWith(expect.stringMatching(/old link will stop working/));
    expect(makeFeed).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Make a new link" }));
    expect(await screen.findByLabelText("Calendar link")).toHaveValue(URL_);

    await userEvent.click(screen.getByRole("button", { name: "Turn off calendar link" }));
    expect(revokeFeed).toHaveBeenCalledWith(7, "second");
    expect(await screen.findByText("Calendar link turned off.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create calendar link" })).toBeInTheDocument();
    expect(screen.queryByDisplayValue(URL_)).toBeNull();
  });

  it("reads again after a stale turn-off, hides the old secret, and waits for a fresh choice", async () => {
    getFeed.mockResolvedValueOnce({ feed: { hasToken: false, createdAt: null, revision: "off" } });
    makeFeed.mockResolvedValueOnce({ feed: { hasToken: true, createdAt: "2026-10-30T15:00:00Z", revision: "first", url: URL_ } });
    revokeFeed.mockRejectedValueOnce(new ApiError(409, "changed", { code: "room_feed_changed" }));
    getFeed.mockResolvedValueOnce({ feed: { hasToken: true, createdAt: "2026-10-31T16:00:00Z", revision: "replacement" } });
    revokeFeed.mockResolvedValueOnce({ feed: { hasToken: false, createdAt: null, revision: "off-again" } });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    render(<RoomFeedPanel room={hall} onClose={() => {}} onGone={() => {}} />);

    await userEvent.click(await screen.findByRole("button", { name: "Create calendar link" }));
    expect(await screen.findByLabelText("Calendar link")).toHaveValue(URL_);
    await userEvent.click(screen.getByRole("button", { name: "Turn off calendar link" }));

    const line = await screen.findByText(/This calendar link changed while you were choosing/);
    expect(line).toHaveFocus();
    expect(screen.getByText(/A calendar link is on, made/)).toBeVisible();
    expect(screen.queryByRole("button", { name: "Copy link" })).toBeNull();
    expect(screen.queryByDisplayValue(URL_)).toBeNull();
    expect(revokeFeed).toHaveBeenCalledExactlyOnceWith(7, "first");

    await userEvent.click(screen.getByRole("button", { name: "Turn off calendar link" }));
    expect(confirm).toHaveBeenCalledTimes(2);
    expect(revokeFeed).toHaveBeenLastCalledWith(7, "replacement");
    expect(await screen.findByText("Calendar link turned off.")).toBeVisible();
  });

  it("closes when a stale write's status read confirms the room feed is unavailable", async () => {
    getFeed.mockResolvedValueOnce({ feed: { hasToken: true, createdAt: "2026-10-30T15:00:00Z", revision: "first" } });
    getFeed.mockRejectedValueOnce(new ApiError(404, "not found"));
    revokeFeed.mockRejectedValueOnce(new ApiError(409, "changed", { code: "room_feed_changed" }));
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const onGone = vi.fn();
    render(<RoomFeedPanel room={hall} onClose={() => {}} onGone={onGone} />);

    await userEvent.click(await screen.findByRole("button", { name: "Turn off calendar link" }));
    await waitFor(() => expect(onGone).toHaveBeenCalledOnce());
    expect(getFeed).toHaveBeenCalledTimes(2);
    expect(revokeFeed).toHaveBeenCalledExactlyOnceWith(7, "first");
    expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
  });

  it("reads after a lost answer without replaying the write or claiming it succeeded", async () => {
    getFeed.mockResolvedValueOnce({ feed: { hasToken: false, createdAt: null, revision: "off" } });
    makeFeed.mockRejectedValueOnce(new ApiError(502, "unreadable answer", {}, true));
    getFeed.mockResolvedValueOnce({ feed: { hasToken: true, createdAt: "2026-10-30T15:00:00Z", revision: "after" } });
    render(<RoomFeedPanel room={hall} onClose={() => {}} onGone={() => {}} />);

    await userEvent.click(await screen.findByRole("button", { name: "Create calendar link" }));

    expect(await screen.findByRole("button", { name: "Make a new link" })).toBeVisible();
    expect(screen.getByText(/couldn.t confirm whether your calendar link changed/)).toBeVisible();
    expect(screen.queryByRole("button", { name: "Copy link" })).toBeNull();
    expect(makeFeed).toHaveBeenCalledExactlyOnceWith(7, "off");
  });

  it("Done closes the panel", async () => {
    getFeed.mockResolvedValue({ feed: { hasToken: false, createdAt: null, revision: "off" } });
    const onClose = vi.fn();
    render(<RoomFeedPanel room={hall} onClose={onClose} onGone={() => {}} />);
    await userEvent.click(await screen.findByRole("button", { name: "Done" }));
    expect(onClose).toHaveBeenCalled();
  });
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  clearMarkerCookie();
});

it.each([200, 404])("recovers an unreadable %i write and status read through the room API", async (status) => {
  const viewer = { token: "feed-test", user: { id: 7 } };
  const headers = { "Fold-Session-Checked": "v2 m-feed-test 7" };

  const replies = [
    Response.json({ feed: { hasToken: false, createdAt: null, revision: "off" } }, { headers }),
    new Response("<html>Unavailable</html>", { status }),
    Response.json({ feed: { hasToken: true, createdAt: "2026-10-30T15:00:00Z" } }, { headers }),
    Response.json({ feed: { hasToken: true, createdAt: "2026-10-30T15:00:00Z", revision: "on" } }, { headers }),
  ];

  const requests: string[] = [];
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async (url, init) => {
    if (String(url).endsWith("/auth/bootstrap")) return bootstrapAnswer(viewer);

    if (String(url).endsWith("/site")) return Response.json({ roomsEnabled: true });

    expect(String(url)).toBe("/api/v1/admin/rooms/7/feed");
    requests.push(init?.method ?? "GET");
    const next = replies.shift();

    if (!next) throw new Error("Unexpected request");

    return next;
  }));
  const { credential } = await bootstrapCredential(true);
  const onGone = vi.fn();
  renderWithAuth(() => ({ credential }))(<RoomFeedPanel room={hall} onClose={() => {}} onGone={onGone} />);
  await userEvent.click(await screen.findByRole("button", { name: "Create calendar link" }));
  const retry = await screen.findByRole("button", { name: "Try again" });
  const message = screen.getByText(/couldn.t confirm whether your calendar link changed/);
  expect(message).toHaveFocus();
  expect(message).not.toHaveTextContent("Its current status is shown below.");
  expect(screen.queryByRole("button", { name: "Create calendar link" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Make a new link" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Copy link" })).toBeNull();

  await userEvent.tab();
  expect(retry).toHaveFocus();
  await userEvent.keyboard("{Enter}");
  expect(await screen.findByText(/A calendar link is on, made/)).toBeVisible();
  expect(message).toHaveFocus();
  expect(message).toHaveTextContent("Its current status is shown below.");
  expect(onGone).not.toHaveBeenCalled();
  expect(screen.queryByRole("button", { name: "Copy link" })).toBeNull();
  expect(requests).toEqual(["GET", "POST", "GET", "GET"]);
});

it("closes and hides a shown URL when the cookie changes before dispatch", async () => {
  const viewer = { token: "previous", user: { id: 7, role: "admin" as const, timezone: "UTC" } };
  const replacement = { token: "replacement", user: { id: 9, role: "admin" as const, timezone: "UTC" } };
  let session = viewer;
  const writes: string[] = [];

  const fetcher = vi.fn<typeof fetch>(async (url, init) => {
    if (String(url).endsWith("/auth/bootstrap")) return bootstrapAnswer(session);

    expect(url).toBe("/api/v1/admin/rooms/7/feed");
    const method = init?.method ?? "GET";
    writes.push(method);
    const headers = { "Fold-Session-Checked": "v2 m-previous 7" };

    const feed = method === "GET"
      ? { hasToken: false, createdAt: null, revision: "off" }
      : { hasToken: true, createdAt: "2026-10-30T15:00:00Z", revision: "on", url: URL_ };

    return Response.json({ feed }, { headers });
  });

  vi.stubGlobal("fetch", fetcher);
  vi.spyOn(window, "confirm").mockReturnValue(true);
  const onClose = vi.fn();

  function ReadyPanel() {
    const { loading } = useAuth();

    return loading ? null : <RoomFeedPanel room={hall} onClose={onClose} onGone={() => {}} />;
  }

  render(<ReadyPanel />, { wrapper: AuthProvider });
  await userEvent.click(await screen.findByRole("button", { name: "Create calendar link" }));
  expect(await screen.findByLabelText("Calendar link")).toHaveValue(URL_);

  session = replacement;
  setMarkerCookie("m-replacement");
  await userEvent.click(screen.getByRole("button", { name: "Turn off calendar link" }));

  await waitFor(() => expect(onClose).toHaveBeenCalled());
  expect(screen.queryByDisplayValue(URL_)).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Copy link" })).not.toBeInTheDocument();
  expect(writes).toEqual(["GET", "POST"]);
  expect(fetcher.mock.calls.filter(([url]) => url === "/api/v1/auth/bootstrap")).toHaveLength(2);
});

it("offers retry when a fresh settings read cannot confirm that rooms are off", async () => {
  const viewer = { token: "feed-test", user: { id: 7 } };
  const off = { feed: { hasToken: false, createdAt: null, revision: "off" } };
  let reads = 0;

  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async (url) => {
    if (url === "/api/v1/auth/bootstrap") return bootstrapAnswer(viewer);

    if (url === "/api/v1/site") {
      return Response.json({ error: "failed", code: "server_error" }, { status: 500 });
    }

    expect(url).toBe("/api/v1/admin/rooms/7/feed");
    reads++;

    return reads === 1
      ? Response.json({ error: "not found", code: "not_found" }, { status: 404 })
      : Response.json(off, { headers: { "Fold-Session-Checked": "v2 m-feed-test 7" } });
  }));

  const { credential } = await bootstrapCredential(true);
  const onGone = vi.fn();
  renderWithAuth(() => ({ credential }))(<RoomFeedPanel room={hall} onClose={() => {}} onGone={onGone} />);
  expect(await screen.findByRole("alert")).toHaveTextContent("We couldn't load this room's calendar link.");
  await userEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(await screen.findByRole("button", { name: "Create calendar link" })).toBeVisible();
  expect(onGone).not.toHaveBeenCalled();
  expect(reads).toBe(2);
});
