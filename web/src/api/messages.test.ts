import { afterEach, expect, it, vi } from "vitest";
import { Messages, previewHTMLPath } from "./messages";
import { bootstrapAnswer } from "../test/credential";

afterEach(() => vi.unstubAllGlobals());

it("sends email drafts and edits naming their read, and schedules for a time or now naming the review", async () => {
  const fetcher = vi.fn<typeof fetch>(async (url) => String(url).endsWith("/auth/bootstrap") ? bootstrapAnswer({ token: "t" }) : new Response("{}"));
  vi.stubGlobal("fetch", fetcher);
  const draft = { subject: "Hi", bodyHtml: "<p>Hi</p>", audience: { type: "group" as const, groupId: 7 }, pushAlert: true };
  await Messages.create(draft);
  await Messages.update(5, draft, "1.r");
  await Messages.schedule(5, { revision: "1.r", review: "tok", scheduledAt: "2026-11-05T16:00:00.000Z" });
  await Messages.schedule(5, { revision: "1.r", review: "tok" });
  await Messages.review(5);
  const bodies = (fetcher.mock.calls).filter(([, o]) => o?.method === "POST" || o?.method === "PUT").map(([, o]) => JSON.parse(String(o?.body)));
  expect(bodies).toEqual([{ channel: "email", ...draft }, { channel: "email", ...draft, revision: "1.r" }, { revision: "1.r", review: "tok", scheduledAt: "2026-11-05T16:00:00.000Z" }, { revision: "1.r", review: "tok" }]);
  expect(fetcher.mock.calls.some(([url, o]) => String(url).endsWith("/messages/5/review") && (o?.method ?? "GET") === "GET")).toBe(true);
  expect(previewHTMLPath(5)).toBe("/api/v1/messages/5/preview.html");
});
