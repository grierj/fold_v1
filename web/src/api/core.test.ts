import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api, apiCSV, apiIfChanged, apiUpload, configureApi, path, query } from "./core";
import { bootstrapAnswer, clearMarkerCookie, setMarkerCookie, setSecureMarkerCookie } from "../test/credential";

afterEach(() => {
  configureApi({ onUnauthorized: null });
  vi.unstubAllGlobals();
  clearMarkerCookie();
});

const csrfOr = (answer: (url: string, init?: RequestInit) => Response, token = () => "csrf") =>
  vi.fn(async (url: RequestInfo | URL, init?: RequestInit) =>
    String(url) === "/api/v1/auth/bootstrap" ? bootstrapAnswer({ token: token() }) : answer(String(url), init));

describe("path and query", () => {
  it("encodes every interpolated value as one path segment", () => {
    expect(path`/events/${"7/8?x"}/dates/${"2026-12-01/#?"}`).toBe("/events/7%2F8%3Fx/dates/2026-12-01%2F%23%3F");
    expect(path`/groups/${12}/members/${3}`).toBe("/groups/12/members/3");
  });

  it("builds a query string, leaving out empty values and repeating lists", () => {
    expect(query({ a: "x y", b: undefined, c: "", d: false, e: true, n: 0, tag: ["p", "", "q&r"] })).toBe("?a=x%20y&e=1&n=0&tag=p&tag=q%26r");
    expect(query({ a: undefined, b: null })).toBe("");
  });
});

describe("api", () => {
  it("sends the CSRF token on unsafe requests only, and always the cookie", async () => {
    const fetcher = csrfOr(() => new Response("{}"), () => "test-csrf");
    vi.stubGlobal("fetch", fetcher);
    await api("/things");
    await api("/things", { method: "POST", body: { a: 1 } });
    const calls = fetcher.mock.calls;
    expect(calls.map(([url]) => url)).toEqual(["/api/v1/things", "/api/v1/auth/bootstrap", "/api/v1/things"]);
    expect(calls[0][1]?.headers).not.toHaveProperty("X-CSRF-Token");
    expect(calls[2][1]).toMatchObject({ method: "POST", credentials: "include", body: '{"a":1}', headers: { "X-CSRF-Token": "test-csrf", "Content-Type": "application/json" } });
  });

  it("sends people to log in on a 401, except when the caller says anonymous is fine", async () => {
    vi.stubGlobal("fetch", csrfOr(() => new Response("{}", { status: 401 })));
    const onUnauthorized = vi.fn();
    configureApi({ onUnauthorized });
    await expect(api("/auth/me", { anonymousOK: true })).rejects.toMatchObject({ status: 401, code: "signed_out" });
    expect(onUnauthorized).not.toHaveBeenCalled();
    await expect(api("/me/groups")).rejects.toMatchObject({ status: 401, code: "signed_out" });
    expect(onUnauthorized).toHaveBeenCalledOnce();
  });

  it("keeps the server's own code on a 401 (a wrong password is not a sign-out)", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ error: "invalid email or password", code: "bad_credentials" }), { status: 401 })));
    await expect(api("/auth/login", { anonymousOK: true })).rejects.toMatchObject({ status: 401, code: "bad_credentials" });
  });

  it("bootstraps a fresh credential after a request that rotates it, whatever its outcome", async () => {
    vi.resetModules();
    const { api } = await import("./core");
    let minted = 0;
    const sent: Array<string | null> = [];
    let fail = false;
    vi.stubGlobal("fetch", csrfOr((_url, init) => {
      sent.push(new Headers(init?.headers).get("X-CSRF-Token"));

      if (fail) throw new TypeError("network");

      return new Response("{}");
    }, () => `token-${++minted}`));
    await api("/a", { method: "POST", rotatesCsrf: true });
    await api("/b", { method: "POST" });
    fail = true;
    await expect(api("/c", { method: "POST", rotatesCsrf: true })).rejects.toBeInstanceOf(TypeError);
    fail = false;
    await api("/d", { method: "POST" });
    expect(sent).toEqual(["token-1", "token-2", "token-2", "token-3"]);
  });

  it("only passes the status and an unreadable flag when the body is not a JSON object", async () => {
    const fail = async (response: Response) => {
      vi.stubGlobal("fetch", vi.fn(async () => response));

      return api("/x").catch((cause: unknown) => cause);
    };

    for (const make of [() => new Response("<html>bad gateway</html>", { status: 400 }), () => new Response("", { status: 400 }), () => new Response("[]", { status: 400 }), () => new Response("null", { status: 400 })]) {
      expect(await fail(make())).toMatchObject({ status: 400, unreadable: true });
    }

    expect(await fail(new Response(JSON.stringify({ error: "no", code: "last_admin" }), { status: 400 }))).toMatchObject({ status: 400, code: "last_admin", unreadable: false });
    expect(await fail(new Response("proxy", { status: 502 }))).toMatchObject({ status: 502, code: "server_error", unreadable: true });
  });

  it("reads a success body that is not JSON as an empty object", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("<html></html>")));
    expect(await api("/x")).toEqual({});
  });

  it("lets a network failure through untouched", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Network secret")));
    await expect(api("/x")).rejects.toBeInstanceOf(TypeError);
  });
});

describe("apiUpload", () => {
  const file = () => new File(["x"], "a.png", { type: "image/png" });

  it("posts multipart with the CSRF token and no JSON content type", async () => {
    const fetcher = csrfOr(() => new Response(JSON.stringify({ avatarUrl: "/u/1" })));
    vi.stubGlobal("fetch", fetcher);
    expect(await apiUpload("/me/avatar", "avatar", file())).toEqual({ avatarUrl: "/u/1" });
    const [, init] = fetcher.mock.calls[fetcher.mock.calls.length - 1];

    if (!init) throw new Error("Expected request options");
    expect(init.body).toBeInstanceOf(FormData);
    expect(init.headers).toMatchObject({ "X-CSRF-Token": expect.any(String), Accept: "application/json" });
    expect(init.headers).not.toHaveProperty("Content-Type");
  });

  it("handles a failure like any other request: 401 signs out, an unreadable body is flagged", async () => {
    const onUnauthorized = vi.fn();
    configureApi({ onUnauthorized });
    vi.stubGlobal("fetch", csrfOr(() => new Response("{}", { status: 401 })));
    await expect(apiUpload("/me/avatar", "avatar", file())).rejects.toMatchObject({ status: 401, code: "signed_out" });
    expect(onUnauthorized).toHaveBeenCalledOnce();
    vi.stubGlobal("fetch", csrfOr(() => new Response("proxy", { status: 413 })));
    await expect(apiUpload("/me/avatar", "avatar", file())).rejects.toMatchObject({ status: 413, unreadable: true });
  });
});

describe("apiCSV", () => {
  it("asks for no-store CSV and returns the blob", async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response("a,b\r\n", { headers: { "Content-Type": "text/csv; charset=utf-8" } }));
    vi.stubGlobal("fetch", fetcher);
    expect(await (await apiCSV("/x.csv")).text()).toBe("a,b\r\n");
    expect(fetcher.mock.calls[0][1]).toMatchObject({ credentials: "include", cache: "no-store", headers: { Accept: "text/csv" } });
  });

  it.each([400, 403, 500])("rejects a JSON failure %s without returning a blob", async (status) => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: "SECRET", code: "server_error" }), { status })));
    await expect(apiCSV("/x.csv")).rejects.toMatchObject({ status });
  });

  it("shares signed-out handling and rejects successful JSON or proxy bodies", async () => {
    const onUnauthorized = vi.fn();
    configureApi({ onUnauthorized });
    const answers = vi.fn().mockResolvedValueOnce(new Response("{}", { status: 401 })).mockResolvedValueOnce(new Response("{}", { headers: { "Content-Type": "application/json" } })).mockResolvedValueOnce(new Response("proxy", { status: 502 }));
    vi.stubGlobal("fetch", vi.fn(async (url: RequestInfo | URL) => (String(url) === "/api/v1/auth/bootstrap" ? bootstrapAnswer() : answers())));
    await expect(apiCSV("/x.csv")).rejects.toMatchObject({ status: 401, code: "signed_out" });
    expect(onUnauthorized).toHaveBeenCalledOnce();
    await expect(apiCSV("/x.csv")).rejects.toBeInstanceOf(ApiError);
    await expect(apiCSV("/x.csv")).rejects.toMatchObject({ status: 502 });
  });
});

describe("apiIfChanged", () => {
  it("sends the last ETag, answers null on 304 and returns the new ETag otherwise", async () => {
    const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ unread: 2 }), { headers: { ETag: '"2-0"' } })).mockResolvedValueOnce(new Response(null, { status: 304 }));
    vi.stubGlobal("fetch", fetcher);
    expect(await apiIfChanged("/n", "")).toEqual({ data: { unread: 2 }, etag: '"2-0"' });
    expect(await apiIfChanged("/n", '"2-0"')).toBeNull();
    expect(fetcher.mock.calls[0][1]?.headers).not.toHaveProperty("If-None-Match");
    expect(fetcher.mock.calls[1][1].headers).toMatchObject({ "If-None-Match": '"2-0"' });
  });

  it("treats a 401 like every other 401", async () => {
    const onUnauthorized = vi.fn();
    configureApi({ onUnauthorized });
    vi.stubGlobal("fetch", csrfOr(() => new Response("{}", { status: 401 })));
    await expect(apiIfChanged("/n", "")).rejects.toMatchObject({ status: 401, code: "signed_out" });
    expect(onUnauthorized).toHaveBeenCalledOnce();
  });
});

// The browser credential (M8.1), with a fresh copy of the module per test: a
// tab's lifetime, the cookie jar it shares with other tabs (document.cookie),
// and the server's answers, some held until the test lets them go.
describe("browser credential", () => {
  type Core = typeof import("./core");

  interface Server {
    core: Core;
    calls: Array<{ url: string; headers: Headers }>;
    // The next bootstrap answer; a function runs when the request arrives.
    boot: () => Response;
    answer: (url: string, init?: RequestInit) => Promise<Response> | Response;
  }

  async function tab(): Promise<Server> {
    vi.resetModules();
    const core = await import("./core");
    let next = () => bootstrapAnswer({ token: "t1", user: { id: 5 } });

    const server: Server = {
      core,
      calls: [],
      get boot() { return next; },
      set boot(f) { next = f; },
      answer: () => new Response("{}"),
    };

    vi.stubGlobal("fetch", vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      server.calls.push({ url: String(url), headers: new Headers(init?.headers) });

      return String(url) === "/api/v1/auth/bootstrap" ? next() : server.answer(String(url), init);
    }));

    return server;
  }

  const ack = (marker: string, viewer: number | "anon") => new Response("{}", { headers: { "Fold-Session-Checked": `v2 ${marker} ${viewer}` } });

  function held() {
    let release: (r: Response) => void = () => {};
    const promise = new Promise<Response>((resolve) => { release = resolve; });

    return { promise, release };
  }

  it("adopts a credential only when the body, the cookie and the acknowledgment agree", async () => {
    const s = await tab();
    const { credential } = await s.core.bootstrapCredential();
    expect(credential).toMatchObject({ token: "t1", marker: "m-t1", viewer: 5, verified: true });
    expect(s.core.credentialCurrent(credential)).toBe(true);

    // No acknowledgment: the token still works for plain requests, but nothing pins it.
    s.boot = () => bootstrapAnswer({ token: "t2", user: { id: 5 }, ack: false });
    const unacked = (await s.core.bootstrapCredential(true)).credential;
    expect(unacked.verified).toBe(false);
    const before = s.calls.length;
    await expect(s.core.api("/x", { pinned: true })).rejects.toMatchObject({ status: 403, code: "session_expired", unreadable: false });
    expect(s.calls.length).toBe(before);
  });

  it("reconciles two first visits racing for the cookie before adopting either", async () => {
    const s = await tab();
    let n = 0;
    s.boot = () => {
      n++;

      // The first answer lost the jar to another tab's pair.
      if (n === 1) {
        const lost = bootstrapAnswer({ token: "mine", cookie: false });
        document.cookie = "fold-credential=m-theirs; path=/";

        return lost;
      }

      return bootstrapAnswer({ token: "theirs" });
    };

    const { credential } = await s.core.bootstrapCredential();
    expect(n).toBe(2);
    expect(credential).toMatchObject({ token: "theirs", verified: true });
  });

  it("asks once when many requests need a cold token at the same moment", async () => {
    const s = await tab();
    await Promise.all([s.core.api("/a", { method: "POST" }), s.core.api("/b", { method: "POST" }), s.core.api("/c", { method: "PUT" })]);
    expect(s.calls.filter((c) => c.url === "/api/v1/auth/bootstrap")).toHaveLength(1);
  });

  it("pins a participating read and write to its credential and accepts only its acknowledgment", async () => {
    const s = await tab();
    const { credential } = await s.core.bootstrapCredential();
    s.answer = () => ack("m-t1", 5);
    await s.core.api("/read", { pinned: credential });
    await s.core.api("/write", { method: "POST", pinned: credential });

    for (const call of s.calls.filter((c) => c.url !== "/api/v1/auth/bootstrap")) {
      expect(call.headers.get("Fold-Session-Check")).toBe("v2");
      expect(call.headers.get("X-CSRF-Token")).toBe("t1");
    }

    // A refusal of the pinned token makes the tab check its credential; any other refusal is the route's own only when acknowledged.
    s.answer = () => Response.json({ error: "x", code: "session_expired" }, { status: 403 });
    const asked = s.calls.length;
    await expect(s.core.api("/read", { pinned: credential })).rejects.toMatchObject({ code: "session_expired", unreadable: true });
    await vi.waitFor(() => expect(s.calls.slice(asked).map((c) => c.url)).toContain("/api/v1/auth/bootstrap"));
    s.answer = () => Response.json({ error: "x", code: "not_leader" }, { status: 403, headers: { "Fold-Session-Checked": "v2 m-t1 5" } });
    await expect(s.core.api("/read", { pinned: credential })).rejects.toMatchObject({ status: 403, code: "not_leader", unreadable: false });
    s.answer = () => Response.json({ error: "x", code: "not_leader" }, { status: 403 });
    await expect(s.core.api("/read", { pinned: credential })).rejects.toMatchObject({ status: 403, code: "session_expired", unreadable: true });

    // An answer projected for no one, or for another marker, is not this credential's.
    for (const wrong of [ack("m-t1", "anon"), ack("m-other", 5), new Response("{}")]) {
      s.answer = () => wrong;
      await expect(s.core.api("/read", { pinned: credential })).rejects.toMatchObject({ code: "session_expired", unreadable: true });
    }
  });

  it("refuses before sending when another tab replaced the cookie, hint or no hint, and a fresh bootstrap then works", async () => {
    const s = await tab();
    const { credential } = await s.core.bootstrapCredential();
    // Tab B changed the password: its response replaced the shared jar's marker; no message reached this tab.
    document.cookie = "fold-credential=m-t2; path=/";
    const before = s.calls.length;
    await expect(s.core.api("/write", { method: "POST", pinned: credential })).rejects.toMatchObject({ code: "session_expired", unreadable: false });
    expect(s.calls.length).toBe(before);
    expect(s.core.credentialCurrent(credential)).toBe(false);
    s.boot = () => bootstrapAnswer({ token: "t2", user: { id: 5 } });
    const next = (await s.core.bootstrapCredential()).credential;
    expect(next).not.toBe(credential);
    expect(next).toMatchObject({ token: "t2", viewer: 5, verified: true });
    // A plain write for the same person goes on with the new token.
    s.answer = () => new Response("{}");
    await s.core.api("/plain", { method: "POST" });
    expect(s.calls[s.calls.length - 1].headers.get("X-CSRF-Token")).toBe("t2");
  });

  it("never publishes an answer that arrives after the cookie changed, and leaves a write uncertain", async () => {
    const s = await tab();
    const { credential } = await s.core.bootstrapCredential();
    const reply = held();
    s.answer = () => reply.promise;
    const read = s.core.api("/read", { pinned: credential });
    await vi.waitFor(() => expect(s.calls.some((c) => c.url === "/api/v1/read")).toBe(true));
    document.cookie = "fold-credential=m-t2; path=/";
    s.boot = () => bootstrapAnswer({ token: "t2", user: { id: 5 } });
    reply.release(ack("m-t1", 5));
    await expect(read).rejects.toMatchObject({ code: "session_expired", unreadable: true });
  });

  it("a plain write for a different person after another tab signed in is refused unsent", async () => {
    const s = await tab();
    await s.core.api("/first", { method: "POST" });
    document.cookie = "fold-credential=m-t9; path=/";
    s.boot = () => bootstrapAnswer({ token: "t9", user: { id: 9 } });
    const before = s.calls.length;
    await expect(s.core.api("/write", { method: "POST" })).rejects.toMatchObject({ status: 403, code: "session_expired" });
    expect(s.calls.slice(before).map((c) => c.url)).toEqual(["/api/v1/auth/bootstrap"]);
    expect(s.core.currentCredential()).toMatchObject({ token: "t9", viewer: 9 });
  });

  it("a 401 from before a sign-in cannot sign out its replacement; a live one does", async () => {
    const s = await tab();
    const onUnauthorized = vi.fn();
    s.core.configureApi({ onUnauthorized });
    await s.core.bootstrapCredential();
    const late = held();
    s.answer = (url) => (url === "/api/v1/old" ? late.promise : new Response("{}"));
    const old = s.core.api("/old");
    await vi.waitFor(() => expect(s.calls.some((c) => c.url === "/api/v1/old")).toBe(true));
    s.boot = () => bootstrapAnswer({ token: "t2", user: { id: 6 } });
    await s.core.api("/auth/login", { method: "POST", rotatesCsrf: true });
    const replacement = s.core.currentCredential();
    late.release(new Response("{}", { status: 401 }));
    await expect(old).rejects.toMatchObject({ status: 401 });
    expect(onUnauthorized).not.toHaveBeenCalled();
    expect(s.core.currentCredential()).toBe(replacement);

    s.answer = () => new Response("{}", { status: 401 });
    await expect(s.core.api("/now")).rejects.toMatchObject({ status: 401 });
    expect(onUnauthorized).toHaveBeenCalledOnce();
  });

  for (const hint of ["delivered", "withheld"] as const) {
    it(`a 401 that another tab's password change overtook does not sign this tab out (hint ${hint})`, async () => {
      const s = await tab();
      const onUnauthorized = vi.fn();
      s.core.configureApi({ onUnauthorized });
      const stop = s.core.watchCredential(() => {});
      await s.core.bootstrapCredential();
      const late = held();
      s.answer = (url) => (url === "/api/v1/old" ? late.promise : new Response("{}"));
      const old = s.core.api("/old");
      await vi.waitFor(() => expect(s.calls.some((c) => c.url === "/api/v1/old")).toBe(true));
      // Tab B changed the password: same person, new pair in the shared jar.
      s.boot = () => bootstrapAnswer({ token: "t2", user: { id: 5 } });
      setMarkerCookie("m-t2");

      if (hint === "delivered") {
        const other = new BroadcastChannel("fold-credential");
        other.postMessage("recheck");
        await vi.waitFor(() => expect(s.core.currentCredential()?.token).toBe("t2"));
        other.close();
      }

      late.release(new Response("{}", { status: 401 }));
      await expect(old).rejects.toMatchObject({ status: 401 });
      await vi.waitFor(() => expect(s.core.currentCredential()?.token).toBe("t2"));
      expect(onUnauthorized).not.toHaveBeenCalled();
      stop();
    });
  }

  it("a pinned write whose refusal arrives after the cookie changed is uncertain, not the refusal", async () => {
    const s = await tab();
    const { credential } = await s.core.bootstrapCredential();
    const reply = held();
    s.answer = () => reply.promise;
    const write = s.core.api("/write", { method: "POST", pinned: credential });
    await vi.waitFor(() => expect(s.calls.some((c) => c.url === "/api/v1/write")).toBe(true));
    setMarkerCookie("m-t2");
    s.boot = () => bootstrapAnswer({ token: "t2", user: { id: 5 } });
    reply.release(Response.json({ error: "x", code: "date_page_outdated" }, { status: 409, headers: { "Fold-Session-Checked": "v2 m-t1 5" } }));
    await expect(write).rejects.toMatchObject({ code: "session_expired", unreadable: true });
  });

  it("holds plain writes during a credential change and sends them with the new token", async () => {
    const s = await tab();
    await s.core.api("/first", { method: "POST" });
    const login = held();
    s.answer = (url) => (url === "/api/v1/auth/login" ? login.promise : new Response("{}"));
    const signingIn = s.core.api("/auth/login", { method: "POST", rotatesCsrf: true });
    await vi.waitFor(() => expect(s.calls.some((c) => c.url === "/api/v1/auth/login")).toBe(true));
    const write = s.core.api("/write", { method: "POST" });
    s.boot = () => bootstrapAnswer({ token: "t2", user: { id: 5 } });
    login.release(new Response("{}"));
    await Promise.all([signingIn, write]);
    expect(s.calls.find((c) => c.url === "/api/v1/write")?.headers.get("X-CSRF-Token")).toBe("t2");
  });

  it("never shows a bootstrap answer older than the one already adopted", async () => {
    const s = await tab();
    await s.core.bootstrapCredential();
    const slow = held();
    s.boot = () => {
      s.boot = () => bootstrapAnswer({ token: "t1", user: { id: 5, name: "New name" } });

      return bootstrapAnswer({ token: "t1", user: { id: 5, name: "Old name" } });
    };

    const seen: Array<string | undefined> = [];
    const stop = s.core.watchCredential((session) => seen.push(session?.user?.name));
    const fetcher = vi.mocked(fetch);
    const real = fetcher.getMockImplementation();
    fetcher.mockImplementationOnce(async (url, init) => {
      const answer = await real?.(url, init);
      await slow.promise;

      return answer ?? new Response();
    });
    const older = s.core.bootstrapCredential(true);
    const newer = await s.core.bootstrapCredential(true);
    slow.release(new Response());
    expect((await older).user?.name).toBe("New name");
    expect(newer.user?.name).toBe("New name");
    expect(seen).toEqual(["New name"]);
    stop();
  });

  it("keeps the same lifetime when a recheck finds the same credential, and a new one when it changed", async () => {
    const s = await tab();
    const first = (await s.core.bootstrapCredential()).credential;
    expect((await s.core.bootstrapCredential(true)).credential).toBe(first);
    s.boot = () => bootstrapAnswer({ token: "t2", user: { id: 5 } });
    expect((await s.core.bootstrapCredential(true)).credential).not.toBe(first);
  });

  it("rechecks on another tab's hint and tells its listener, which sees a suspension first on a local change", async () => {
    const s = await tab();
    const seen: Array<number | "anon" | "suspended"> = [];
    const stop = s.core.watchCredential((session) => seen.push(session ? session.credential.viewer ?? "anon" : "suspended"));
    await s.core.bootstrapCredential();
    s.boot = () => bootstrapAnswer({ token: "t9", user: { id: 9 } });
    const other = new BroadcastChannel("fold-credential");
    other.postMessage("recheck");
    await vi.waitFor(() => expect(s.core.currentCredential()?.viewer).toBe(9));
    other.close();
    s.boot = () => bootstrapAnswer({ token: "t0" });
    await s.core.api("/auth/logout", { method: "POST", rotatesCsrf: true });
    stop();
    expect(seen).toEqual([5, 9, "suspended", "suspended", "anon"]);
  });

  // A response whose headers arrive first and whose body is held until the
  // test lets it go; `reading` settles once the client has begun to read it.
  function heldBody(status: number, headers: Record<string, string>) {
    let begin = () => {};
    let finish: (text: string) => void = () => {};
    let lose = () => {};
    const reading = new Promise<void>((resolve) => { begin = resolve; });
    // SAFETY: jsdom has no ReadableStream global; the body of fetch's own Response is one, so its constructor is the class.
    const Stream = new Response("x").body?.constructor as typeof ReadableStream;

    const body = new Stream<Uint8Array>({
      pull(controller) {
        begin();

        return new Promise<void>((done) => {
          finish = (text) => {
            controller.enqueue(new TextEncoder().encode(text));
            controller.close();
            done();
          };

          lose = () => {
            controller.error(new TypeError("network"));
            done();
          };
        });
      },
    }, { highWaterMark: 0 });

    return { response: new Response(body, { status, headers }), reading, finish: (text: string) => finish(text), lose: () => lose() };
  }

  for (const [status, code] of [[200, null], [409, "date_page_outdated"], [403, "not_leader"]] as const) {
    it(`a pinned ${status} whose body is still being read when the cookie changes is uncertain, not the answer`, async () => {
      const s = await tab();
      const { credential } = await s.core.bootstrapCredential();
      const reply = heldBody(status, { "Fold-Session-Checked": "v2 m-t1 5", "Content-Type": "application/json" });
      s.answer = () => reply.response;
      const write = s.core.api("/write", { method: "POST", pinned: credential });
      await reply.reading;
      // Tab B changed the password while this answer was being decoded.
      setMarkerCookie("m-t2");
      s.boot = () => bootstrapAnswer({ token: "t2", user: { id: 5 } });
      reply.finish(JSON.stringify(code ? { error: "x", code } : { saved: true }));
      await expect(write).rejects.toMatchObject({ code: "session_expired", unreadable: true });
    });
  }

  // A 401's headers can arrive long before its body. Nothing signs out until
  // the body is read, and then only if the lifetime that sent the request is
  // still the tab's; a body that is not a JSON object, or is lost on the way,
  // leaves the outcome unsure.
  for (const kind of ["ordinary", "pinned", "rotating"] as const) {
    for (const end of ["readable", "malformed", "lost mid-body", "replaced while decoding"] as const) {
      it(`${kind}: a 401 acts only once its body is read (${end})`, async () => {
        const s = await tab();
        const onUnauthorized = vi.fn();
        s.core.configureApi({ onUnauthorized });
        const { credential } = await s.core.bootstrapCredential();
        const reply = heldBody(401, { "Content-Type": "application/json" });
        s.answer = () => reply.response;

        const request = kind === "pinned" ? s.core.api("/write", { method: "POST", pinned: credential })
          : kind === "rotating" ? s.core.api("/me/password", { method: "POST", rotatesCsrf: true })
          : s.core.api("/read");

        await reply.reading;
        const sender = s.core.currentCredential();
        expect(sender).toMatchObject({ token: "t1", verified: true });
        expect(onUnauthorized).not.toHaveBeenCalled();

        if (end === "replaced while decoding") {
          // Tab B changed the password while this answer was being decoded.
          setMarkerCookie("m-t2");
          s.boot = () => bootstrapAnswer({ token: "t2", user: { id: 5 } });
        }

        if (end === "lost mid-body") reply.lose();
        else reply.finish(end === "malformed" ? "<html>proxy</html>" : JSON.stringify({ error: "unauthorized", code: "signed_out" }));

        if (end === "replaced while decoding") {
          await expect(request).rejects.toMatchObject(kind === "pinned" ? { status: 403, code: "session_expired", unreadable: true } : { status: 401, code: "signed_out" });
          await vi.waitFor(() => expect(s.core.currentCredential()).toMatchObject({ token: "t2", verified: true }));
          expect(onUnauthorized).not.toHaveBeenCalled();

          return;
        }

        await expect(request).rejects.toMatchObject({ status: 401, code: "signed_out", unreadable: end !== "readable" });
        expect(onUnauthorized).toHaveBeenCalledOnce();
        expect(s.core.credentialCurrent(sender)).toBe(false);
      });
    }
  }

  it("a pinned decision is usable only with this credential's acknowledgment; a server error stays a server error", async () => {
    const s = await tab();
    const { credential } = await s.core.bootstrapCredential();
    const outdated = JSON.stringify({ error: "x", code: "date_page_outdated" });

    interface Refusal { status: number; code?: string; unreadable: boolean; data?: Record<string, never> }

    const cases: Array<[() => Response, Refusal]> = [
      [() => new Response(outdated, { status: 409, headers: { "Fold-Session-Checked": "v2 m-t1 5" } }), { status: 409, code: "date_page_outdated", unreadable: false }],
      // From a route or server that never took part in the binding.
      [() => new Response(outdated, { status: 409 }), { status: 403, code: "session_expired", unreadable: true }],
      [() => new Response(outdated, { status: 409, headers: { "Fold-Session-Checked": "v2 m-other 5" } }), { status: 403, code: "session_expired", unreadable: true }],
      [() => new Response(outdated, { status: 409, headers: { "Fold-Session-Checked": "v2 m-t1 anon" } }), { status: 403, code: "session_expired", unreadable: true }],
      // Acknowledged, but the decision cannot be read: uncertain, and nothing of it is passed on.
      [() => new Response("<html>conflict</html>", { status: 409, headers: { "Fold-Session-Checked": "v2 m-t1 5" } }), { status: 409, unreadable: true, data: {} }],
      [() => Response.json({ error: "boom", code: "server_error" }, { status: 500 }), { status: 500, code: "server_error", unreadable: true, data: {} }],
      [() => new Response("proxy", { status: 502 }), { status: 502, code: "server_error", unreadable: true }],
    ];

    for (const [answer, want] of cases) {
      s.answer = answer;
      await expect(s.core.api("/write", { method: "POST", pinned: credential })).rejects.toMatchObject(want);
    }

    expect(s.core.credentialCurrent(credential)).toBe(true);
  });

  it("an acknowledged pinned success that is not a JSON object is unreadable, never an empty answer", async () => {
    const s = await tab();
    const { credential } = await s.core.bootstrapCredential();

    for (const text of ["", "[]", "3", "null", "{bad", "<html>proxy</html>"]) {
      s.answer = () => new Response(text, { headers: { "Fold-Session-Checked": "v2 m-t1 5" } });
      await expect(s.core.api("/write", { method: "POST", pinned: credential })).rejects.toMatchObject({ status: 502, code: "server_error", unreadable: true });
      await expect(s.core.api("/read", { pinned: credential })).rejects.toMatchObject({ code: "server_error", unreadable: true });
    }

    s.answer = () => new Response(JSON.stringify({ saved: true }), { headers: { "Fold-Session-Checked": "v2 m-t1 5" } });
    expect(await s.core.api("/write", { method: "POST", pinned: credential })).toEqual({ saved: true });
    // A plain request keeps reading a non-JSON success as {}.
    s.answer = () => new Response("");
    expect(await s.core.api("/plain")).toEqual({});
  });

  for (const outcome of ["refused", "lost in transit"] as const) {
    it(`a pinned answer from before a rotation that ${outcome === "refused" ? "was refused" : "was lost"} never publishes, though the cookie is unchanged`, async () => {
      const s = await tab();
      const { credential } = await s.core.bootstrapCredential();
      const reply = held();
      s.answer = (url) => {
        if (url === "/api/v1/read") return reply.promise;

        if (outcome === "lost in transit") throw new TypeError("network");

        return Response.json({ error: "no", code: "bad_credentials" }, { status: 401 });
      };

      const read = s.core.api("/read", { pinned: credential });
      await vi.waitFor(() => expect(s.calls.some((c) => c.url === "/api/v1/read")).toBe(true));
      const signIn = s.core.api("/auth/login", { method: "POST", anonymousOK: true, rotatesCsrf: true });
      await expect(signIn).rejects.toMatchObject(outcome === "refused" ? { code: "bad_credentials" } : { name: "TypeError" });
      // The tab settled on the same token, marker and viewer: a new lifetime all the same.
      const after = s.core.currentCredential();
      expect(after).toMatchObject({ token: "t1", marker: "m-t1", viewer: 5, verified: true });
      expect(after).not.toBe(credential);
      expect(s.core.credentialCurrent(credential)).toBe(false);
      reply.release(ack("m-t1", 5));
      await expect(read).rejects.toMatchObject({ code: "session_expired", unreadable: true });
      // Work started under the settled lifetime goes on.
      s.answer = () => ack("m-t1", 5);
      expect(await s.core.api("/read", { pinned: true })).toEqual({});
    });
  }

  // Two tabs of one browser: two copies of the module sharing the cookie jar
  // (document.cookie), the BroadcastChannel and one server, which answers a
  // bootstrap for the marker the request's cookie carries.
  async function browser() {
    const pairs = new Map<string, { token: string; user: { id: number } | null }>([["m-t1", { token: "t1", user: { id: 5 } }]]);
    const routes = new Map<string, () => Promise<Response> | Response>();
    const sent: string[] = [];
    let bootFails = false;
    const jarMarker = () => document.cookie.split(";").map((p) => p.trim()).find((p) => p.startsWith("fold-credential="))?.slice("fold-credential=".length) ?? "";
    setMarkerCookie("m-t1");
    vi.stubGlobal("fetch", vi.fn(async (url: RequestInfo | URL) => {
      const u = String(url);
      sent.push(u);

      if (u !== "/api/v1/auth/bootstrap") return (routes.get(u) ?? (() => Response.json({})))();

      if (bootFails) throw new TypeError("network");
      const marker = jarMarker();
      const pair = pairs.get(marker) ?? { token: "t0", user: null };

      return bootstrapAnswer({ token: pair.token, marker, user: pair.user });
    }));
    vi.resetModules();
    const a = await import("./core");
    vi.resetModules();
    const b = await import("./core");

    return {
      a, b, routes, sent, pairs,
      set bootFails(v: boolean) { bootFails = v; },
      // The server signs this browser in with a fresh pair in the shared jar.
      install(token: string, user: number | null) {
        pairs.set(`m-${token}`, { token, user: user === null ? null : { id: user } });
        setMarkerCookie(`m-${token}`);
      },
    };
  }

  for (const who of [5, 6] as const) {
    it(`a password change's 401 that another tab's sign-in overtook leaves the replacement signed in (${who === 5 ? "same person" : "someone else"})`, async () => {
      const tabs = await browser();
      const onA = vi.fn();
      const onB = vi.fn();
      tabs.a.configureApi({ onUnauthorized: onA });
      tabs.b.configureApi({ onUnauthorized: onB });
      await tabs.a.bootstrapCredential();
      await tabs.b.bootstrapCredential();
      const late = held();
      tabs.routes.set("/api/v1/me/password", () => late.promise);
      const change = tabs.a.api("/me/password", { method: "POST", body: { current: "x", password: "y" }, rotatesCsrf: true });
      await vi.waitFor(() => expect(tabs.sent).toContain("/api/v1/me/password"));
      tabs.routes.set("/api/v1/auth/login", () => {
        tabs.install("t2", who);

        return Response.json({});
      });
      await tabs.b.api("/auth/login", { method: "POST", anonymousOK: true, rotatesCsrf: true });
      expect(tabs.b.currentCredential()).toMatchObject({ token: "t2", viewer: who, verified: true });
      late.release(Response.json({ error: "signed out", code: "signed_out" }, { status: 401 }));
      await expect(change).rejects.toMatchObject({ status: 401 });
      expect(onA).not.toHaveBeenCalled();
      const replacement = tabs.a.currentCredential();
      expect(replacement).toMatchObject({ token: "t2", viewer: who, verified: true });
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(tabs.a.currentCredential()).toBe(replacement);
      expect(onB).not.toHaveBeenCalled();
    });
  }

  it("a password change's own 401 still signs the tab out, also when it cannot reconcile, and an overtaken one never does", async () => {
    for (const end of ["reconciled", "unreconciled", "overtaken and unreconciled"] as const) {
      const tabs = await browser();
      const onA = vi.fn();
      tabs.a.configureApi({ onUnauthorized: onA });
      await tabs.a.bootstrapCredential();
      tabs.routes.set("/api/v1/me/password", () => {
        // The session ended elsewhere before this request was judged.
        tabs.pairs.set("m-t1", { token: "t1", user: null });

        if (end === "overtaken and unreconciled") tabs.install("t2", 5);
        tabs.bootFails = end !== "reconciled";

        return Response.json({ error: "signed out", code: "signed_out" }, { status: 401 });
      });
      await expect(tabs.a.api("/me/password", { method: "POST", rotatesCsrf: true })).rejects.toMatchObject({ status: 401 });
      expect(onA, end).toHaveBeenCalledTimes(end === "overtaken and unreconciled" ? 0 : 1);
      tabs.a.configureApi({ onUnauthorized: null });
    }
  });

  // A marker copy at another path (left by something else on this host) is
  // never sent to /api, so the server can neither see nor expire it, and
  // script cannot tell which of two values is the server's. On the pages under
  // its path the tab trusts neither: participating requests are refused
  // unsent, plain ones go on. Where only the server's own marker shows, it
  // works. The jar is jsdom's, which matches paths as a browser does.
  for (const secure of [false, true]) {
    it(`a marker copy at another path fails closed only on the pages it shows on (${secure ? "Secure" : "plain"} server)`, async () => {
      const s = await tab();
      const start = window.location.pathname;

      if (secure) {
        s.boot = () => {
          setSecureMarkerCookie("m-t1");

          return bootstrapAnswer({ token: "t1", user: { id: 5 }, cookie: false });
        };
      }

      document.cookie = "fold-credential=m-copy; path=/groups";

      try {
        const { credential } = await s.core.bootstrapCredential();
        expect(credential).toMatchObject({ seen: "m-t1", verified: true });
        history.pushState(null, "", "/groups/7");
        expect(document.cookie).toContain("m-copy");
        expect(s.core.credentialCurrent(credential)).toBe(false);
        const before = s.calls.length;
        await expect(s.core.api("/read", { pinned: credential })).rejects.toMatchObject({ code: "session_expired", unreadable: false });
        const stuck = (await s.core.bootstrapCredential(true)).credential;
        expect(stuck).toMatchObject({ token: "t1", seen: null, verified: false });
        await expect(s.core.api("/read", { pinned: true })).rejects.toMatchObject({ code: "session_expired", unreadable: false });
        expect(s.calls.slice(before).map((c) => c.url)).toEqual(["/api/v1/auth/bootstrap"]);
        await s.core.api("/plain", { method: "POST" });
        expect(s.calls[s.calls.length - 1]).toMatchObject({ url: "/api/v1/plain" });
        expect(s.calls[s.calls.length - 1].headers.get("X-CSRF-Token")).toBe("t1");

        history.pushState(null, "", "/");
        expect(document.cookie).not.toContain("m-copy");
        expect((await s.core.bootstrapCredential(true)).credential).toMatchObject({ seen: "m-t1", verified: true });
        s.answer = () => ack("m-t1", 5);
        expect(await s.core.api("/read", { pinned: true })).toEqual({});
      } finally {
        history.pushState(null, "", start);
        document.cookie = "fold-credential=; max-age=0; path=/groups";
      }
    });
  }

  it("recovers once the server expires the marker a Secure switch left beside the current one", async () => {
    const s = await tab();
    // The jar holds both names: script cannot tell which is the current one.
    setSecureMarkerCookie("m-old");
    const stuck = (await s.core.bootstrapCredential()).credential;
    expect(stuck).toMatchObject({ token: "t1", seen: null, verified: false });
    const before = s.calls.length;
    await expect(s.core.api("/read", { pinned: true })).rejects.toMatchObject({ code: "session_expired", unreadable: false });
    expect(s.calls.length).toBe(before);
    // The server's repair: the current name for T, the other name expired.
    s.boot = () => {
      document.cookie = "__Host-fold-credential=; Secure; max-age=0; path=/";

      return bootstrapAnswer({ token: "t1", user: { id: 5 } });
    };

    const repaired = (await s.core.bootstrapCredential(true)).credential;
    expect(repaired).toMatchObject({ seen: "m-t1", verified: true });
    s.answer = () => ack("m-t1", 5);
    expect(await s.core.api("/read", { pinned: true })).toEqual({});
  });
});
