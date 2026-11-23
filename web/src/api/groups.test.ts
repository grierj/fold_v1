import { afterEach, expect, it, vi } from "vitest";
import { Groups } from "./groups";
import { bootstrapCredential } from "./core";
import { bootstrapAnswer, participatingAnswers, sentChecks } from "../test/credential";

afterEach(() => vi.unstubAllGlobals());

const stub = () => {
  const fetcher = vi.fn<typeof fetch>(async (url) => String(url).endsWith("/auth/bootstrap") ? bootstrapAnswer() : new Response("{}"));
  vi.stubGlobal("fetch", fetcher);

  return fetcher;
};

it("sends an add's opaque selection with the over-limit choice", async () => {
  const fetcher = stub();
  await Groups.addMember(3, { selectionId: "opaque" }, true);
  const post = fetcher.mock.calls.find(([, opts]) => opts?.method === "POST");
  expect(JSON.parse(String(post?.[1]?.body))).toEqual({ selectionId: "opaque", overLimit: true });
});

it("builds the list and page queries from only what is set, marking every group page", async () => {
  const fetcher = stub();
  await Groups.list("", []);
  await Groups.list("lake view", ["youth", "", "music"]);
  await Groups.get(3);
  await Groups.get(3, { cursor: "opaque+/=", limit: 100 });
  expect((fetcher.mock.calls).map(([url]) => url)).toEqual([
    "/api/v1/groups",
    "/api/v1/groups?search=lake%20view&tag=youth&tag=music",
    "/api/v1/groups/3?datePaging=2",
    "/api/v1/groups/3?datePaging=2&cursor=opaque%2B%2F%3D&limit=100",
  ]);
});

it("pins the group page, a later page and the roster to a credential only when one is passed", async () => {
  const fetcher = vi.fn<typeof fetch>(participatingAnswers("t-groups"));
  vi.stubGlobal("fetch", fetcher);
  const { credential } = await bootstrapCredential(true);
  await Groups.get(3, undefined, credential);
  await Groups.get(3, { cursor: "c" }, credential);
  await Groups.members(3, credential);
  await Groups.get(3);
  await Groups.members(3);
  expect(sentChecks(fetcher.mock.calls)).toEqual([
    ["/api/v1/groups/3?datePaging=2", "v2", "t-groups"],
    ["/api/v1/groups/3?datePaging=2&cursor=c", "v2", "t-groups"],
    ["/api/v1/groups/3/members", "v2", "t-groups"],
    ["/api/v1/groups/3?datePaging=2", null, null],
    ["/api/v1/groups/3/members", null, null],
  ]);
});

it("refuses a pinned group page acknowledged for another viewer as the credential's loss", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(participatingAnswers("t-other", "v2 m-t-other 9")));
  const { credential } = await bootstrapCredential(true);
  await expect(Groups.get(3, undefined, credential)).rejects.toMatchObject({ code: "session_expired", unreadable: true });
});
