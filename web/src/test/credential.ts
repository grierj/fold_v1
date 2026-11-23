import type { User } from "../types";

interface BootstrapFixture {
  token?: string;
  marker?: string;
  user?: (Partial<User> & Pick<User, "id">) | null;
  canCompose?: boolean;
  // Set the marker cookie as the real response does (the default), or leave
  // the jar as it is (a competing tab's pair, an unreadable cookie).
  cookie?: boolean;
  // Send the Fold-Session-Checked acknowledgment (the default).
  ack?: boolean;
}

// What GET /api/v1/auth/bootstrap answers for one browser credential.
export function bootstrapAnswer({ token = "csrf", marker = `m-${token}`, user = null, canCompose = false, cookie = true, ack = true }: BootstrapFixture = {}): Response {
  if (cookie) setMarkerCookie(marker);
  const headers: Record<string, string> = ack ? { "Fold-Session-Checked": `v2 ${marker} ${user ? user.id : "anon"}` } : {};

  return new Response(JSON.stringify({ user, canCompose, csrfToken: token, credentialMarker: marker }), { status: 200, headers });
}

// Another tab's response replacing the shared cookie jar's marker.
export function setMarkerCookie(marker: string) {
  document.cookie = `fold-credential=${marker}; path=/`;
}

// The marker under the name a Secure server uses (jsdom treats localhost as secure).
export function setSecureMarkerCookie(marker: string) {
  document.cookie = `__Host-fold-credential=${marker}; Secure; path=/`;
}

// Both names: a test that leaves either behind would make the next one's jar ambiguous.
export function clearMarkerCookie() {
  document.cookie = "fold-credential=; max-age=0; path=/";
  document.cookie = "__Host-fold-credential=; Secure; max-age=0; path=/";
}

// A fetch for participating reads (M8.1): bootstrap adopts token for a
// signed-out viewer, and every other request answers {} acknowledged as ack
// (by default, that credential's own acknowledgment).
export function participatingAnswers(token: string, ack = `v2 m-${token} anon`): (url: RequestInfo | URL) => Promise<Response> {
  return async (url) => String(url).endsWith("/auth/bootstrap") ? bootstrapAnswer({ token }) : new Response("{}", { headers: { "Fold-Session-Checked": ack } });
}

// Each non-bootstrap request as [url, Fold-Session-Check, X-CSRF-Token].
export function sentChecks(calls: Array<[RequestInfo | URL, RequestInit?]>): Array<[string, string | null, string | null]> {
  return calls.flatMap(([url, init]): Array<[string, string | null, string | null]> => {
    if (String(url).endsWith("/auth/bootstrap")) return [];
    const headers = new Headers(init?.headers);

    return [[String(url), headers.get("Fold-Session-Check"), headers.get("X-CSRF-Token")]];
  });
}
