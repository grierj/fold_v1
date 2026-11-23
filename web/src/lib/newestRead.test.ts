import { act, renderHook } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { useNewestRead } from "./newestRead";

function held<T>() {
  let resolve!: (value: T) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });

  return { promise, resolve, reject };
}

it("applies only the newest ask's answer, and every caller settles with its outcome", async () => {
  const older = held<string>();
  const newer = held<string>();
  const fetch = vi.fn().mockReturnValueOnce(older.promise).mockReturnValueOnce(newer.promise);
  const apply = vi.fn();
  const { result } = renderHook(() => useNewestRead(fetch, apply));
  let first!: Promise<void>;
  let second!: Promise<void>;
  act(() => { first = result.current(); second = result.current(); });
  const failed = new Error("offline");
  const outcomes = Promise.allSettled([first, second]);
  await act(async () => { newer.reject(failed); older.resolve("old"); await outcomes; });
  expect(await outcomes).toEqual([{ status: "rejected", reason: failed }, { status: "rejected", reason: failed }]);
  expect(apply).not.toHaveBeenCalled();
});

it("an older answer landing after the newest one is dropped", async () => {
  const older = held<string>();
  const fetch = vi.fn().mockReturnValueOnce(older.promise).mockResolvedValueOnce("new");
  const apply = vi.fn();
  const { result } = renderHook(() => useNewestRead(fetch, apply));
  let first!: Promise<void>;
  await act(async () => { first = result.current(); await result.current(); });
  expect(apply).toHaveBeenCalledWith("new");
  await act(async () => { older.resolve("old"); await first; });
  expect(apply).toHaveBeenCalledTimes(1);
});
