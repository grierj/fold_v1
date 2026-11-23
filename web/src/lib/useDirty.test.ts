import { renderHook } from "@testing-library/react";
import { expect, it } from "vitest";
import { useDirty } from "./useDirty";

it("counts edits from the first ready snapshot, and from the snapshot a new savedAt comes with", () => {
  const { result, rerender } = renderHook(({ title, ready, savedAt }) => useDirty({ title }, ready, savedAt), {
    initialProps: { title: "", ready: false, savedAt: 0 },
  });
  expect(result.current).toBe(false);
  rerender({ title: "Supper", ready: true, savedAt: 0 });
  expect(result.current).toBe(false);
  rerender({ title: "Supper night", ready: true, savedAt: 0 });
  expect(result.current).toBe(true);
  rerender({ title: "Supper night", ready: true, savedAt: 1 });
  expect(result.current).toBe(false);
  rerender({ title: "Supper", ready: true, savedAt: 1 });
  expect(result.current).toBe(true);
});
