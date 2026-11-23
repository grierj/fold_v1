import { afterEach, expect, it, vi } from "vitest";
import { Households } from "./households";
import { bootstrapCredential } from "./core";
import { participatingAnswers, sentChecks } from "../test/credential";

afterEach(() => vi.unstubAllGlobals());

it("pins the household read to a credential only when one is passed", async () => {
  const fetcher = vi.fn<typeof fetch>(participatingAnswers("t-household"));
  vi.stubGlobal("fetch", fetcher);
  const { credential } = await bootstrapCredential(true);
  await Households.mine(credential);
  await Households.mine();
  expect(sentChecks(fetcher.mock.calls)).toEqual([["/api/v1/me/household", "v2", "t-household"], ["/api/v1/me/household", null, null]]);
});

it("refuses a pinned household acknowledged for another viewer as the credential's loss", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(participatingAnswers("t-household-other", "v2 m-t-household-other 4")));
  const { credential } = await bootstrapCredential(true);
  await expect(Households.mine(credential)).rejects.toMatchObject({ code: "session_expired", unreadable: true });
});
