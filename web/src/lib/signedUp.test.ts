import { beforeEach, expect, it, vi } from "vitest";
import { Events } from "../api/events";
import { stubService } from "../test/service";
import { signedUpNotice } from "./signedUp";

stubService(Events, () => ({ signedUp: vi.fn() }));

beforeEach(() => { vi.resetAllMocks(); });

it.each([
  [0, "Nobody is signed up."],
  [1, "1 person is signed up and will be notified."],
  [3, "3 people are signed up and will be notified."],
])("words %s people", async (people, line) => {
  vi.mocked(Events.signedUp).mockResolvedValue({ people });
  expect(await signedUpNotice(7)).toBe(line);
});

it("reads the event's count, or the date's", async () => {
  vi.mocked(Events.signedUp).mockResolvedValueOnce({ people: 2 }).mockResolvedValueOnce({ people: 1 });
  expect(await signedUpNotice(7)).toBe("2 people are signed up and will be notified.");
  expect(await signedUpNotice(7, "2026-12-01")).toBe("1 person is signed up and will be notified.");
  expect(Events.signedUp).toHaveBeenNthCalledWith(1, 7, undefined);
  expect(Events.signedUp).toHaveBeenNthCalledWith(2, 7, "2026-12-01");
});

it("still asks, without a number, when the count can't be read", async () => {
  vi.mocked(Events.signedUp).mockRejectedValue(new Error("offline"));
  expect(await signedUpNotice(7)).toBe("Everyone signed up will be notified.");
});
