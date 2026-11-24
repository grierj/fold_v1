import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement, Fragment, useLayoutEffect, type ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { AuthProvider, useAuth } from "../auth";
import type { Credential } from "../api/core";
import { RosterWritesProvider } from "../lib/rosterWrites";
import { bootstrapAnswer } from "./credential";
import { testUser } from "./fixtures";
import type { SentEdit } from "./guardedEdits";

interface SessionProps {
  children: ReactNode;
  // Runs in the layout effect of every commit that shows a new credential (null included).
  onCredential?: (credential: Credential | null) => void;
}

// App's own session shape (App.tsx): the real AuthProvider, and another person, an erased account reading as signed
// out included, remounts everything under it.
function Keyed({ children, onCredential }: SessionProps) {
  const { user, loading, credential } = useAuth();

  useLayoutEffect(() => { onCredential?.(credential); }, [credential, onCredential]);

  if (loading) return null;

  return createElement(Fragment, { key: user ? `${user.id}:${user.status}` : "anon" }, createElement(RosterWritesProvider, { children }));
}

export function SignedInSession(props: SessionProps) {
  return createElement(AuthProvider, null, createElement(Keyed, props));
}

// What GET /auth/bootstrap answers this tab's cookie from now on, counting each ask; every other request fails.
// `signedIn: false` is an account an administrator erased from their own browser: the server ended its sessions,
// so the same token and marker read as signed out, and the response sets no cookie (the jar stays byte for byte as
// it was, unlike a sign-out in another tab of this browser, which replaces the marker).
function serveBootstrap(credential: Credential, signedIn: boolean) {
  let asked = 0;
  const user = signedIn && credential.viewer !== null ? testUser({ id: credential.viewer }) : null;

  vi.stubGlobal("fetch", vi.fn(async (url: RequestInfo | URL) => {
    if (!String(url).endsWith("/auth/bootstrap")) return new Response("{}", { status: 503 });
    asked++;

    return bootstrapAnswer({ token: credential.token, marker: credential.marker, user, cookie: false });
  }));

  return { asked: () => asked };
}

export interface ErasureHost {
  credential: () => Credential;
  sent: () => SentEdit[];
  // Renders the host under SignedInSession, passing onCredential on.
  mount: (onCredential: (credential: Credential | null) => void) => void;
  // Opens the editor and chooses a more private value, asking nothing.
  draft: () => Promise<void>;
  // That value's control, which must still hold the draft.
  drafted: () => HTMLElement;
  // Saves the draft once the writer's next answer is a question about it.
  ask: () => Promise<void>;
}

const asked = () => screen.queryByRole("dialog", { name: "Save this date?" });

// Every date question's description ends with its count.
const QUESTION_TEXT = "who can no longer see this date";

const returns: Array<[string, () => void]> = [
  ["the window gets focus back", () => { window.dispatchEvent(new Event("focus")); }],
  ["the tab is shown again", () => { document.dispatchEvent(new Event("visibilitychange")); }],
];

// An administrator erasing this manager from their own browser leaves this tab's marker cookie as it was, so only
// the server can say so: coming back to the window while the date question is open asks it, and an answer that
// signs no one in closes the question before the next paint, with nothing sent.
export function erasureCases(host: () => ErasureHost) {
  for (const [name, comeBack] of returns) {
    it(`closes the question before the next paint once ${name}, and a Yes kept from it sends nothing`, async () => {
      const h = host();
      serveBootstrap(h.credential(), true);
      // Whether the question or its text was still in the document after each commit that showed a new credential.
      const seen: boolean[] = [];
      let watching = false;
      h.mount(() => { if (watching) queueMicrotask(() => seen.push(!!asked() || document.body.textContent?.includes(QUESTION_TEXT) === true)); });
      await h.draft();
      await h.ask();
      const yes = within(await screen.findByRole("dialog", { name: "Save this date?" })).getByRole("button", { name: "Save and notify" });
      watching = true;
      const jar = document.cookie;
      const server = serveBootstrap(h.credential(), false);
      await act(async () => { comeBack(); });
      await waitFor(() => expect(asked()).not.toBeInTheDocument());
      expect(server.asked()).toBe(1);
      expect(seen).toEqual([false]);
      expect(document.cookie).toBe(jar);
      expect(document.body).not.toHaveTextContent(QUESTION_TEXT);
      expect(document.activeElement?.closest("[role='dialog']") ?? null).toBeNull();
      await userEvent.click(yes);
      expect(h.sent()).toHaveLength(1);
      expect(h.sent()[0].consent).toBeUndefined();
    });
  }

  it("keeps the question, its description, the draft and its Yes when the server still signs this manager in", async () => {
    const h = host();
    serveBootstrap(h.credential(), true);
    h.mount(() => {});
    await h.draft();
    await h.ask();
    const dialog = await screen.findByRole("dialog", { name: "Save this date?" });
    const description = dialog.getAttribute("aria-describedby");
    const server = serveBootstrap(h.credential(), true);
    await act(async () => { window.dispatchEvent(new Event("focus")); document.dispatchEvent(new Event("visibilitychange")); });
    await waitFor(() => expect(server.asked()).toBe(1));
    expect(screen.getByRole("dialog", { name: "Save this date?" })).toBe(dialog);
    expect(dialog.getAttribute("aria-describedby")).toBe(description);
    expect(dialog).toHaveAccessibleDescription(expect.stringContaining(QUESTION_TEXT));
    expect(h.drafted()).toBeChecked();
    await userEvent.click(within(dialog).getByRole("button", { name: "Save and notify" }));
    await waitFor(() => expect(h.sent()).toHaveLength(2));
    // The same save with the question's token added, under a new attempt id: no send is ever replayed.
    expect(h.sent()[1]).toMatchObject({ revision: h.sent()[0].revision, changes: h.sent()[0].changes, consent: expect.stringMatching(/^[0-9a-f]{32}$/) });
    expect(h.sent()[1].attempt).not.toBe(h.sent()[0].attempt);
  });

  it("asks the server nothing when the window gets focus back while no question is open", async () => {
    const h = host();
    serveBootstrap(h.credential(), true);
    h.mount(() => {});
    await h.draft();
    const server = serveBootstrap(h.credential(), false);
    await act(async () => { window.dispatchEvent(new Event("focus")); document.dispatchEvent(new Event("visibilitychange")); });
    expect(server.asked()).toBe(0);
    expect(h.drafted()).toBeChecked();
  });
}
