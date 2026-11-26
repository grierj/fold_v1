import { afterEach, expect, it, vi } from "vitest";
import { Admin, AdminGroups } from "./admin";
import { bootstrapAnswer } from "../test/credential";

afterEach(() => vi.unstubAllGlobals());

const posted = (fetcher: ReturnType<typeof vi.fn>) =>
  (fetcher.mock.calls).filter(([, opts]) => opts?.method === "POST").map(([url, opts]) => ({ url, body: JSON.parse(String(opts?.body)) }));

it("sends an admin add's opaque selection with its role and over-limit choice", async () => {
  const fetcher = vi.fn<typeof fetch>(async (url) => String(url).endsWith("/auth/bootstrap") ? bootstrapAnswer() : new Response("{}"));
  vi.stubGlobal("fetch", fetcher);
  await AdminGroups.addMember(3, { selectionId: "opaque" }, "member", true);
  expect(posted(fetcher)[0].body).toEqual({ selectionId: "opaque", role: "member", overLimit: true });
});

it("approves a group from the bell with the notification id, and from the queue with tags", async () => {
  const fetcher = vi.fn<typeof fetch>(async (url) => String(url).endsWith("/auth/bootstrap") ? bootstrapAnswer() : new Response("{}"));
  vi.stubGlobal("fetch", fetcher);
  await Admin.approve(9, undefined, 2);
  await Admin.approve(9, ["youth"]);
  expect(posted(fetcher).map((p) => p.body)).toEqual([{ fromNotification: 2 }, { tags: ["youth"] }]);
});

it("marks a request card's delete as its answer, and sends no marker from the Groups tab", async () => {
  const fetcher = vi.fn<typeof fetch>(async (url) => String(url).endsWith("/auth/bootstrap") ? bootstrapAnswer() : new Response("{}"));
  vi.stubGlobal("fetch", fetcher);
  await Admin.deleteGroup(9, true);
  await Admin.deleteGroup(9);
  const deletes = fetcher.mock.calls.filter(([, opts]) => opts?.method === "DELETE").map(([url]) => String(url));
  expect(deletes).toEqual(["/api/v1/admin/groups/9?deletionRequest=1", "/api/v1/admin/groups/9"]);
});
