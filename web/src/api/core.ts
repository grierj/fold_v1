// The one fetch client for /api/v1. Cookie session auth; the CSRF token comes
// from the browser credential (GET /api/v1/auth/bootstrap, below) and is sent
// as X-CSRF-Token on every unsafe request. Every request, whatever it reads
// back (JSON, a file, a 304), goes through send() so a 401, an unreadable body
// and the CSRF rules behave the same everywhere.
import { isBoolean, isJsonObject, isNumber, isString, type JsonObject, type JsonValue } from "../lib/wireValues";
import type { SessionBootstrapResponse, User } from "../types";

let onUnauthorized: (() => void) | null = null;

// Called once before the first render (main.tsx), so no request can run
// before the sign-out handler is known. Tests call it too, with null to clear.
export function configureApi(config: { onUnauthorized: (() => void) | null }) {
  onUnauthorized = config.onUnauthorized;
}

// A resource id in a URL path: a number from a loaded row, or the string a
// route handed over. path() encodes either the same way.
export type Id = number | string;

// path`/groups/${id}/members/${userId}` encodes every interpolated value as a
// path segment, so an id or token with "/", "?" or "#" in it stays one segment.
export function path(strings: TemplateStringsArray, ...values: Array<string | number>): string {
  return strings.reduce((out, s, i) => out + s + (i < values.length ? encodeURIComponent(String(values[i])) : ""), "");
}

type QueryValue = string | number | boolean | null | undefined | readonly string[];

function isQueryArray(value: QueryValue): value is readonly string[] {
  return Array.isArray(value);
}

// query({ a: "x y", tag: ["p", "q"] }) is "?a=x%20y&tag=p&tag=q". Absent, null,
// empty and false values are left out; true is sent as "1". Returns "" when
// nothing is left, so it appends to any path.
export function query(params: Record<string, QueryValue>): string {
  const parts: string[] = [];

  const add = (key: string, value: string | number | boolean) => {
    parts.push(`${encodeURIComponent(key)}=${encodeURIComponent(value === true ? "1" : String(value))}`);
  };

  for (const [key, value] of Object.entries(params)) {
    if (isQueryArray(value)) {
      for (const item of value) if (item) add(key, item);
    } else if (value !== undefined && value !== null && value !== "" && value !== false) {
      add(key, value);
    }
  }

  return parts.length ? `?${parts.join("&")}` : "";
}

// An error response. `code` is the API's stable error code; show it with
// errorText (lib/errors.ts), never the message, which is the server's
// English for logs.
export class ApiError extends Error {
  status: number;
  code: string;
  data: JsonObject;
  // True when the failure status came with a body that is not a JSON object (a
  // gateway's page, an empty body): the server did not say this, so a caller
  // that must know what happened (the delete panel) treats it as uncertain.
  unreadable: boolean;
  constructor(status: number, message: string, data: JsonObject = {}, unreadable = false) {
    super(message);
    this.status = status;
    this.data = data;
    this.unreadable = unreadable;
    this.code = isString(data.code) ? data.code : status >= 500 ? "server_error" : "invalid";
  }
}

// The browser credential (M8.1): the CSRF token T this tab holds, the marker M
// the server derived from it, and the viewer it was judged to sign in. Its
// lifetime is one object: a replacement (another account, a new T or M) is a
// new object, so work captured under the old one can tell it no longer holds.
export interface Credential {
  readonly token: string;
  readonly marker: string;
  readonly viewer: number | null;
  // The marker cookie this tab saw at adoption (null when it could not read
  // one). Only a verified credential, whose cookie, body and acknowledgment
  // agreed, may pin a participating request; plain requests need the token.
  readonly seen: string | null;
  readonly verified: boolean;
}

export interface BootstrapSession {
  credential: Credential;
  user: User | null;
  canCompose: boolean;
}

let current: Credential | null = null;
let lastAdopted: Credential | null = null;
let lastSession: BootstrapSession | null = null;

// Each bootstrap request's order; an answer older than the one adopted is not shown.
let asked = 0;
let adoptedAt = 0;

// Rises with every local invalidation; a bootstrap begun before one never adopts.
let invalidations = 0;
let flight: { at: number; promise: Promise<BootstrapSession> } | null = null;
let rotation: Promise<void> | null = null;
const listeners = new Set<(session: BootstrapSession | null) => void>();
let channel: BroadcastChannel | null = null;

const CHANNEL = "fold-credential";

// The marker cookie as script sees it now: __Host-fold-credential when the
// server marks cookies Secure (which a browser also allows on http://localhost),
// fold-credential otherwise. null when there is none, or more than one: an
// ambiguous jar is never trusted. A copy at another path shows only on the
// pages under it, and neither script nor the server can tell it apart or
// remove it, so those pages refuse participating requests until it goes.
function liveMarker(): string | null {
  const found = document.cookie.split(";").map((part) => part.trim()).filter((part) => part.startsWith("__Host-fold-credential=") || part.startsWith("fold-credential="));
  const value = found.length === 1 ? found[0].slice(found[0].indexOf("=") + 1) : "";

  return value || null;
}

/** @public The credential this tab holds now, or null while it is suspended or unknown; the lifetime a participating read pins. */
export function currentCredential(): Credential | null {
  return current;
}

// Whether a lifetime captured earlier is still this tab's, sampled now: the same
// object and the marker cookie unchanged. Callers check after every await,
// before publishing, and immediately before a fetch.
export function credentialCurrent(credential: Credential | null): credential is Credential {
  return credential !== null && credential === current && liveMarker() === credential.seen;
}

function notify(session: BootstrapSession | null) {
  for (const listener of listeners) listener(session);
}

// Suspends the lifetime before a local credential change: nothing captured
// under it is current any more, and nothing adopted before now can return,
// even when the tab settles on the same token, marker and viewer (a refused or
// lost rotation): only a routine recheck keeps the lifetime object.
function invalidateCredential() {
  invalidations++;
  current = null;
  lastAdopted = null;
  lastSession = null;
  notify(null);
}

class BootstrapRefused extends Error {}

function readSession(data: JsonValue): SessionBootstrapResponse | null {
  if (!isJsonObject(data) || !isString(data.csrfToken) || !data.csrfToken || !isString(data.credentialMarker) || !data.credentialMarker) return null;
  const user = data.user;

  if (user !== null && (!isJsonObject(user) || !isNumber(user.id))) return null;

  return { user: user && serverPayload<User>(user), canCompose: isBoolean(data.canCompose) && data.canCompose, csrfToken: data.csrfToken, credentialMarker: data.credentialMarker };
}

async function runBootstrap(at: number): Promise<BootstrapSession> {
  for (let attempt = 0; attempt < 3; attempt++) {
    const order = ++asked;
    const res = await fetch("/api/v1/auth/bootstrap", { credentials: "include", headers: { Accept: "application/json" }, cache: "no-store" });

    if (!res.ok) throw new ApiError(res.status, "could not read the session");
    const read = readSession((await readBody(res)).data);

    if (!read) throw new ApiError(502, "unreadable session", {}, true);

    if (at !== invalidations) throw new BootstrapRefused();

    if (order < adoptedAt && lastSession && current) return lastSession;
    const viewer = read.user ? read.user.id : null;
    const marker = read.credentialMarker;
    const acked = res.headers.get("Fold-Session-Checked") === `v2 ${marker} ${viewer ?? "anon"}`;
    const live = liveMarker();

    // Another tab's first visit won the cookie: ask again for the pair it holds.
    if (live !== null && live !== marker) continue;
    const next: Credential = { token: read.csrfToken, marker, viewer, seen: live, verified: acked && live === marker };
    const same = lastAdopted !== null && (["token", "marker", "viewer", "seen", "verified"] as const).every((k) => lastAdopted?.[k] === next[k]);
    const credential = same && lastAdopted ? lastAdopted : next;
    current = lastAdopted = credential;
    adoptedAt = order;
    const session = { credential, user: read.user, canCompose: !!read.user && read.canCompose };
    lastSession = session;
    notify(session);

    return session;
  }

  throw new ApiError(409, "the session changed while it was read", { code: "session_expired" }, true);
}

// The one bootstrap owner: a cold token, a recheck and a refresh share one
// request at a time. fresh starts a new one (a sign-in or a profile change
// that the answer must reflect). A bootstrap that a local change overtook
// tries again for the change's credential.
export function bootstrapCredential(fresh = false): Promise<BootstrapSession> {
  if (!fresh && flight && flight.at === invalidations) return flight.promise;
  const at = invalidations;

  const promise = runBootstrap(at).catch((cause: unknown) => {
    if (cause instanceof BootstrapRefused) return bootstrapCredential();
    throw cause;
  });

  const mine = { at, promise };
  flight = mine;
  void promise.finally(() => { if (flight === mine) flight = null; }).catch(() => {});

  return promise;
}

function recheckCredential() {
  void bootstrapCredential().catch(() => {});
}

// Thrown when a participating request's pinned credential is no longer this
// tab's. Before dispatch nothing was sent; after it (unreadable) a write's
// outcome is unknown.
class CredentialLost extends ApiError {
  constructor(dispatched: boolean) {
    super(403, "the browser credential changed", { code: "session_expired" }, dispatched);
  }
}

// A dispatched participating request whose answer is not this credential's:
// its outcome is unknown, and the tab checks the credential it holds.
function lostAfterDispatch(): never {
  recheckCredential();
  throw new CredentialLost(true);
}

function acknowledgment(credential: Credential): string {
  return `v2 ${credential.marker} ${credential.viewer ?? "anon"}`;
}

// The token for a plain unsafe request. When another tab replaced the
// credential, a new one for the same viewer is used; one for someone else is
// the session_expired refusal the server would give.
async function ensureCsrf(): Promise<string> {
  while (rotation) await rotation;
  const held = current;

  if (held && liveMarker() === held.seen) return held.token;
  const { credential } = await bootstrapCredential();

  if (held && credential.viewer !== held.viewer) throw new CredentialLost(false);

  return credential.token;
}

// Listens for credential changes (AuthProvider): every adopted bootstrap and
// every suspension (null). Another tab's content-free hint and this tab's
// return to view check the marker; correctness never depends on the hint.
export function watchCredential(listener: (session: BootstrapSession | null) => void): () => void {
  listeners.add(listener);

  if (!channel && "BroadcastChannel" in globalThis) {
    channel = new BroadcastChannel(CHANNEL);
    channel.onmessage = recheckCredential;
  }

  const sample = () => {
    if (document.visibilityState === "visible" && current && liveMarker() !== current.seen) recheckCredential();
  };

  window.addEventListener("focus", sample);
  document.addEventListener("visibilitychange", sample);

  return () => {
    listeners.delete(listener);
    window.removeEventListener("focus", sample);
    document.removeEventListener("visibilitychange", sample);

    if (!listeners.size && channel) {
      channel.close();
      channel = null;
    }
  };
}

interface RequestHeaders {
  Accept?: string;
  "Content-Type"?: string;
  "If-None-Match"?: string;
  "X-CSRF-Token"?: string;
  "Fold-Session-Check"?: string;
}

interface SendOptions {
  method?: string;
  headers?: RequestHeaders;
  body?: BodyInit;
  cache?: RequestCache;
  anonymousOK?: boolean;
  rotatesCsrf?: boolean;
  pinned?: Credential | true | null;
  // A 304 is an answer, not a failure (the badge's ETag poll).
  notModifiedOK?: boolean;
}

// Reads a body without throwing: `data` is the JSON value (or {} when there is
// none), `readable` is true only for a non-empty JSON object.
async function readBody(res: Response): Promise<{ data: JsonValue; readable: boolean }> {
  const text = await res.text();

  if (!text) return { data: {}, readable: false };

  try {
    const data: JsonValue = JSON.parse(text);

    return { data, readable: isJsonObject(data) };
  } catch {
    // A proxy's HTML error page: keep the status, drop the body.
    return { data: {}, readable: false };
  }
}

// send is the only place that calls fetch for the API. It returns a response
// whose status is acceptable and whose body is unread; every failure is an
// ApiError (a thrown fetch error, such as a network failure, passes through).
async function send(url: string, opts: SendOptions = {}): Promise<Response> {
  const method = (opts.method ?? "GET").toUpperCase();
  const headers = { ...opts.headers };
  const pin = opts.pinned === true ? current : opts.pinned;

  if (pin !== undefined) {
    while (rotation) await rotation;

    if (!pin || !pin.verified || !credentialCurrent(pin)) throw new CredentialLost(false);
    headers["Fold-Session-Check"] = "v2";
    headers["X-CSRF-Token"] = pin.token;
  } else if (method !== "GET" && method !== "HEAD") {
    headers["X-CSRF-Token"] = await ensureCsrf();
  }

  const init: RequestInit = { method, headers, body: opts.body, credentials: "include", cache: opts.cache };
  let res: Response;
  // Whether the lifetime that sent this request is still the tab's: asked once
  // a 401's body is decoded, since its headers can arrive long before it.
  let ownLifetime: () => boolean;

  if (opts.rotatesCsrf) {
    // A rotation replaces the lifetime itself, so its 401 is this tab's own
    // only while the jar still holds the marker it was sent with, and the tab
    // settled on that marker: a credential another tab installed meanwhile is
    // never signed out by it. An unreconciled tab (current null) with the jar
    // unchanged is still signed out, as before.
    const sent = await rotate(async () => {
      const marker = liveMarker();

      return { marker, res: await fetch(`/api/v1${url}`, init) };
    });

    res = sent.res;
    const settledAt = invalidations;
    ownLifetime = () => settledAt === invalidations && liveMarker() === sent.marker && (current === null || current.seen === sent.marker);
  } else {
    if (pin && !credentialCurrent(pin)) throw new CredentialLost(false);

    const sentAt = invalidations;
    const sentWith = current;
    const sentMarker = liveMarker();
    res = await fetch(`/api/v1${url}`, init);
    ownLifetime = () => sentAt === invalidations && current === sentWith && liveMarker() === sentMarker;
  }

  if (res.status === 401) {
    const { data, readable } = await readBody(res).catch(() => ({ data: {}, readable: false }));
    const own = ownLifetime();

    // Only the lifetime that sent a request may sign it out; a 401 that
    // another tab's change overtook, also while it was decoded, only makes
    // this tab check what it holds. A pinned request's overtaken 401 is the
    // credential's loss, and an unreadable or lost body leaves a write uncertain.
    if (!opts.anonymousOK) {
      if (own) {
        onUnauthorized?.();
        invalidateCredential();
      }

      recheckCredential();
    }

    if (pin && !own) lostAfterDispatch();
    throw new ApiError(401, "unauthorized", { code: "signed_out", ...(isJsonObject(data) ? data : undefined) }, !readable);
  }

  if (pin && !credentialCurrent(pin)) lostAfterDispatch();

  if (!pin && res.status === 304 && opts.notModifiedOK) return res;
  const acked = !!pin && res.headers.get("Fold-Session-Checked") === acknowledgment(pin);

  if (!res.ok) {
    const { data, readable } = await readBody(res);
    const body = isJsonObject(data) ? data : {};
    const message = isString(body.error) ? body.error : `request failed (${res.status})`;

    // A participating answer is decoded before any of it is used, then judged
    // on the credential as it is now. The server's refusal of the pinned token
    // carries no acknowledgment; any other decision is usable only with this
    // credential's acknowledgment and a readable body. A 5xx decided nothing
    // to show: its status stays, its body does not.
    if (pin) {
      if ((res.status === 403 && body.code === "session_expired") || !credentialCurrent(pin)) lostAfterDispatch();

      if (res.status >= 500) throw new ApiError(res.status, message, {}, true);

      if (!acked) lostAfterDispatch();

      if (!readable) throw new ApiError(res.status, message, {}, true);
    }

    throw new ApiError(res.status, message, body, !readable);
  }

  if (pin && !acked) lostAfterDispatch();

  return res;
}

// A request that replaces the browser credential (sign-in, sign-out, a token
// that signs someone in, a password change, a verified email). The lifetime is
// suspended before it is sent; when it settles, whatever the outcome, the tab
// bootstraps the credential it actually holds, never replaying the change, and
// tells other tabs to check theirs.
async function rotate<T>(request: () => Promise<T>): Promise<T> {
  let done = () => {};
  const mine = new Promise<void>((resolve) => { done = resolve; });
  rotation = mine;
  invalidateCredential();

  try {
    return await request();
  } finally {
    invalidateCredential();
    await bootstrapCredential().catch(() => {});
    channel?.postMessage("recheck");

    if (rotation === mine) rotation = null;
    done();
  }
}

export interface RequestOptions {
  method?: string;
  body?: unknown;
  // anonymousOK: a 401 just means "not signed in" (the boot-time session
  // check), so don't send the visitor to /login; public pages stay public.
  anonymousOK?: boolean;
  // rotatesCsrf: this request replaces the browser credential (sign-in,
  // sign-out, a token that signs someone in, a password change, a verified
  // email).
  rotatesCsrf?: boolean;
  // pinned: a participating request (M8.1), checked against this credential
  // (true: the current one) before it is sent, by the server against the
  // cookie it carries, and on its acknowledgment and after its body is read.
  pinned?: Credential | true;
}

// A JSON request. A plain success body that is not JSON reads as {}; a
// participating one must be a JSON object, or its outcome is unknown.
export async function api<T>(url: string, opts: RequestOptions = {}): Promise<T> {
  const headers: RequestHeaders = { Accept: "application/json" };
  let body: string | undefined;

  if (opts.body !== undefined) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(opts.body);
  }

  const pin = opts.pinned === true ? current : opts.pinned;
  const res = await send(url, { method: opts.method, headers, body, anonymousOK: opts.anonymousOK, rotatesCsrf: opts.rotatesCsrf, pinned: pin });
  const { data, readable } = await readBody(res);

  if (pin !== undefined) {
    if (!credentialCurrent(pin)) lostAfterDispatch();

    if (!readable) throw new ApiError(502, "unreadable answer", {}, true);
  }

  return serverPayload<T>(data);
}

// Multipart upload: the browser sets Content-Type with the boundary.
// `fields` are sent beside the file, as text.
export async function apiUpload<T>(url: string, field: string, file: File, fields: Record<string, string> = {}): Promise<T> {
  const form = new FormData();
  form.append(field, file);

  for (const [name, value] of Object.entries(fields)) form.append(name, value);

  const res = await send(url, { method: "POST", headers: { Accept: "application/json" }, body: form });

  return serverPayload<T>((await readBody(res)).data);
}

// CSV reads reject JSON/proxy answers before any downloadable blob exists.
export async function apiCSV(url: string): Promise<Blob> {
  const res = await send(url, { headers: { Accept: "text/csv" }, cache: "no-store" });

  if (res.headers.get("Content-Type")?.split(";")[0].trim().toLowerCase() !== "text/csv") {
    throw new ApiError(500, "Expected CSV", { code: "server_error" });
  }

  return res.blob();
}

// A conditional JSON read: null means the server said 304, nothing changed.
export async function apiIfChanged<T>(url: string, etag: string): Promise<{ data: T; etag: string } | null> {
  const headers: RequestHeaders = { Accept: "application/json" };

  if (etag) headers["If-None-Match"] = etag;

  const res = await send(url, {
    headers,
    notModifiedOK: true,
  });

  if (res.status === 304) return null;

  return { data: serverPayload<T>((await readBody(res)).data), etag: res.headers.get("ETag") ?? "" };
}

function serverPayload<T>(data: JsonValue): T {
  // SAFETY: Each resource supplies the response type for its server route. Web contract tests compare Go and TypeScript fields.
  return data as T;
}
