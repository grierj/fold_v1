import { beforeEach, expect, it, vi } from "vitest";
import { Admin } from "../api/admin";
import { Events } from "../api/events";
import { stubService } from "../test/service";
import { groupDeleteNotice, signedUpNotice } from "./signedUp";

stubService(Events, () => ({ signedUp: vi.fn() }));

stubService(Admin, () => ({ deleteCount: vi.fn() }));

beforeEach(() => { vi.resetAllMocks(); });

it.each([
  [0, "No one else will be notified."],
  [1, "1 other person will be notified."],
  [3, "3 other people will be notified."],
])("words %s people", async (people, line) => {
  vi.mocked(Events.signedUp).mockResolvedValue({ people });
  expect(await signedUpNotice(7)).toBe(line);
});

it("reads the event's count, or the date's", async () => {
  vi.mocked(Events.signedUp).mockResolvedValueOnce({ people: 2 }).mockResolvedValueOnce({ people: 1 });
  expect(await signedUpNotice(7)).toBe("2 other people will be notified.");
  expect(await signedUpNotice(7, "2026-12-01")).toBe("1 other person will be notified.");
  expect(Events.signedUp).toHaveBeenNthCalledWith(1, 7, undefined);
  expect(Events.signedUp).toHaveBeenNthCalledWith(2, 7, "2026-12-01");
});

it("still asks, without a number, when the count can't be read", async () => {
  vi.mocked(Events.signedUp).mockRejectedValue(new Error("offline"));
  expect(await signedUpNotice(7)).toBe("Anyone else signed up for an upcoming date will be notified.");
  expect(await signedUpNotice(7, "2026-12-01")).toBe("Anyone else signed up for this date will be notified.");
});

it.each([
  [0, "No one else will be notified."],
  [1, "1 other person will be notified."],
  [4, "4 other people will be notified."],
])("words a group delete's %s people in the same words", async (people, line) => {
  vi.mocked(Admin.deleteCount).mockResolvedValue({ people });
  expect(await groupDeleteNotice(5)).toBe(line);
  expect(Admin.deleteCount).toHaveBeenCalledWith(5);
});

it("says who a group delete tells when its count can't be read", async () => {
  vi.mocked(Admin.deleteCount).mockRejectedValue(new Error("offline"));
  expect(await groupDeleteNotice(5)).toBe("Anyone else who is a member or signed up for an upcoming date will be notified.");
});
