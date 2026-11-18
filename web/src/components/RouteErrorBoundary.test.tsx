import { lazy, Suspense } from "react";
import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { opensRoute, pageChunk, RouteErrorBoundary } from "./RouteErrorBoundary";
import { groupDetailChunk } from "./GroupCard";
import { eventDetailChunk } from "../pages/Home";

function Healthy() {
  return <h1>Group page</h1>;
}

const BrokenPage = lazy(() => Promise.reject(new Error("Failed to fetch dynamically imported module /assets/GroupDetail-x.js")));

describe("RouteErrorBoundary", () => {
  afterEach(() => vi.restoreAllMocks());

  it("shows a healthy page as it is", () => {
    render(
      <RouteErrorBoundary resetKey="a">
        <Healthy />
      </RouteErrorBoundary>
    );
    expect(screen.getByRole("heading", { level: 1, name: "Group page" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Reload page" })).not.toBeInTheDocument();
  });

  it("offers a reload when a page's chunk fails to load, without the error's text", async () => {
    // React reports the caught error on the console; keep the run quiet.
    vi.spyOn(console, "error").mockImplementation(() => {});
    render(
      <RouteErrorBoundary resetKey="a">
        <Suspense fallback={<p>Loading…</p>}>
          <BrokenPage />
        </Suspense>
      </RouteErrorBoundary>
    );
    expect(await screen.findByRole("heading", { level: 1, name: "This page didn't load" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reload page" })).toHaveClass("btn-primary");
    expect(screen.queryByText(/dynamically imported/)).not.toBeInTheDocument();
  });

  it("clears the failure when the address changes", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});

    const { rerender } = render(
      <RouteErrorBoundary resetKey="a">
        <Suspense fallback={<p>Loading…</p>}>
          <BrokenPage />
        </Suspense>
      </RouteErrorBoundary>
    );

    expect(await screen.findByRole("button", { name: "Reload page" })).toBeInTheDocument();
    rerender(
      <RouteErrorBoundary resetKey="b">
        <Healthy />
      </RouteErrorBoundary>
    );
    expect(screen.getByRole("heading", { level: 1, name: "Group page" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Reload page" })).not.toBeInTheDocument();
  });
});

// A load that settles only when the test says so.
function deferred() {
  let resolve = (_value: string) => {};
  let reject = () => {};

  const promise = new Promise<string>((ok, fail) => {
    resolve = ok;
    reject = () => fail(new Error("Failed to fetch dynamically imported module"));
  });

  return { promise, resolve, reject };
}

async function stillPending(promise: Promise<unknown>) {
  const marker = "pending";

  return (await Promise.race([promise, new Promise<string>((ok) => setTimeout(() => ok(marker), 20))])) === marker;
}

describe("pageChunk", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    sessionStorage.clear();
  });

  it("lets the route wait for an early start that is still on its way, and retries once after it fails", async () => {
    const early = deferred();
    const load = vi.fn().mockReturnValueOnce(early.promise).mockResolvedValueOnce("page");
    const reload = vi.fn();
    const chunk = pageChunk(load, { needed: () => true, reload });

    chunk.warm();
    chunk.warm();
    const route = chunk.route();
    expect(load).toHaveBeenCalledTimes(1);
    early.reject();
    await expect(route).resolves.toBe("page");
    expect(load).toHaveBeenCalledTimes(2);
    expect(reload).not.toHaveBeenCalled();
  });

  it("reuses an early start that arrived", async () => {
    const load = vi.fn().mockResolvedValue("page");
    const chunk = pageChunk(load, { needed: () => true, reload: vi.fn() });

    chunk.warm();
    await expect(chunk.route()).resolves.toBe("page");
    expect(load).toHaveBeenCalledTimes(1);
  });

  it("forgets a failed early start, so the next hover starts again", async () => {
    const early = deferred();
    const load = vi.fn().mockReturnValueOnce(early.promise).mockResolvedValueOnce("page");
    const chunk = pageChunk(load, { needed: () => true, reload: vi.fn() });

    chunk.warm();
    early.reject();
    await early.promise.catch(() => {});
    chunk.warm();
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("retries a failed load once on navigation with no early start", async () => {
    const load = vi.fn().mockRejectedValueOnce(new Error("Failed to fetch")).mockResolvedValueOnce("page");

    await expect(pageChunk(load, { needed: () => true, reload: vi.fn() }).route()).resolves.toBe("page");
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("reloads the page once when the retry fails too, and waits for the reload", async () => {
    const load = vi.fn().mockRejectedValue(new Error("Failed to fetch"));
    const reload = vi.fn();
    const route = pageChunk(load, { needed: () => true, reload }).route();

    expect(await stillPending(route)).toBe(true);
    expect(load).toHaveBeenCalledTimes(2);
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("hands a second failure after that reload to the error boundary", async () => {
    const load = vi.fn().mockRejectedValue(new Error("Failed to fetch"));
    const reload = vi.fn();

    void pageChunk(load, { needed: () => true, reload }).route();
    await vi.waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
    await expect(pageChunk(load, { needed: () => true, reload }).route()).rejects.toThrow("Failed to fetch");
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("clears the reload mark once a page arrives", async () => {
    const reload = vi.fn();

    void pageChunk(vi.fn().mockRejectedValue(new Error("Failed to fetch")), { needed: () => true, reload }).route();
    await vi.waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
    await expect(pageChunk(vi.fn().mockResolvedValue("page"), { needed: () => true, reload }).route()).resolves.toBe("page");
    void pageChunk(vi.fn().mockRejectedValue(new Error("Failed to fetch")), { needed: () => true, reload }).route();
    await vi.waitFor(() => expect(reload).toHaveBeenCalledTimes(2));
  });

  it("never reloads while offline", async () => {
    vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
    const reload = vi.fn();

    await expect(pageChunk(vi.fn().mockRejectedValue(new Error("Failed to fetch")), { needed: () => true, reload }).route()).rejects.toThrow("Failed to fetch");
    expect(reload).not.toHaveBeenCalled();
  });

  it("never reloads when session storage is unavailable", async () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    const reload = vi.fn();

    await expect(pageChunk(vi.fn().mockRejectedValue(new Error("Failed to fetch")), { needed: () => true, reload }).route()).rejects.toThrow("Failed to fetch");
    expect(reload).not.toHaveBeenCalled();
  });

  it("hands the failure to the error boundary, without a reload, once the page has moved on", async () => {
    const needed = vi.fn().mockReturnValue(false);
    const reload = vi.fn();

    await expect(pageChunk(vi.fn().mockRejectedValue(new Error("Failed to fetch")), { needed, reload }).route()).rejects.toThrow("Failed to fetch");
    expect(needed).toHaveBeenCalledWith(window.location.pathname);
    expect(reload).not.toHaveBeenCalled();
    expect(sessionStorage.getItem("fold:page-chunk-reload")).toBeNull();
  });
});

describe("opensRoute", () => {
  afterEach(() => {
    window.history.replaceState(null, "", "/");
    sessionStorage.clear();
  });

  it("picks the detail route only where the router would open it", () => {
    const group = opensRoute("/groups/:id", ["/groups/new"]);
    const event = opensRoute("/events/:id", ["/events/new"]);

    expect(group("/groups/12")).toBe(true);
    expect(group("/groups/12/")).toBe(true);
    expect(event("/events/7")).toBe(true);
    expect(group("/groups/new")).toBe(false);
    expect(event("/events/new")).toBe(false);
    expect(group("/groups/12/events/new")).toBe(false);
    expect(group("/")).toBe(false);
  });

  it("keeps the group and event page chunks off their creation pages, encoded or not", () => {
    expect(groupDetailChunk.needed("/groups/12")).toBe(true);
    expect(eventDetailChunk.needed("/events/7")).toBe(true);

    for (const path of ["/groups/new", "/groups/%6Eew"]) expect(groupDetailChunk.needed(path)).toBe(false);

    for (const path of ["/events/new", "/events/%6Eew"]) expect(eventDetailChunk.needed(path)).toBe(false);
  });

  it("decodes the address first, so an encoded creation page is never taken for a detail page", () => {
    const group = opensRoute("/groups/:id", ["/groups/new"]);
    const event = opensRoute("/events/:id", ["/events/new"]);

    expect(group("/groups/%6Eew")).toBe(false);
    expect(group("/groups/%6E%65%77")).toBe(false);
    expect(group("/groups/NEW")).toBe(false);
    expect(event("/events/%6Eew")).toBe(false);
    expect(event("/events/n%65w/")).toBe(false);
    expect(group("/groups/%31%32")).toBe(true);
  });

  it("hands a failure that lands on an encoded creation page to the error boundary, without a reload", async () => {
    window.history.pushState(null, "", "/groups/%6Eew");
    const reload = vi.fn();
    const chunk = pageChunk(vi.fn().mockRejectedValue(new Error("Failed to fetch")), { needed: opensRoute("/groups/:id", ["/groups/new"]), reload });

    await expect(chunk.route()).rejects.toThrow("Failed to fetch");
    expect(reload).not.toHaveBeenCalled();
    expect(sessionStorage.getItem("fold:page-chunk-reload")).toBeNull();
  });
});
