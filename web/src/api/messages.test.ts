import { afterEach, expect, it, vi } from "vitest";
import { Messages, previewHTMLPath } from "./messages";
import { bootstrapAnswer } from "../test/credential";

afterEach(() => vi.unstubAllGlobals());

it("sends email drafts, and schedules for a time or now", async () => {
  const fetcher = vi.fn<typeof fetch>(async (url) => String(url).endsWith("/auth/bootstrap") ? bootstrapAnswer({ token: "t" }) : new Response("{}"));
  vi.stubGlobal("fetch", fetcher);
  const draft = { subject: "Hi", bodyHtml: "<p>Hi</p>", audience: { type: "group" as const, groupId: 7 }, pushAlert: true };
  await Messages.create(draft);
  await Messages.schedule(5, "2026-11-05T16:00:00.000Z");
  await Messages.schedule(5);
  const bodies = (fetcher.mock.calls).filter(([, o]) => o?.method === "POST").map(([, o]) => JSON.parse(String(o?.body)));
  expect(bodies).toEqual([{ channel: "email", ...draft }, { scheduledAt: "2026-11-05T16:00:00.000Z" }, {}]);
  expect(previewHTMLPath(5)).toBe("/api/v1/messages/5/preview.html");
});
