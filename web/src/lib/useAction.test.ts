import { act, renderHook } from "@testing-library/react";
import { StrictMode } from "react";
import { describe, expect, it } from "vitest";
import { ApiError } from "../api/core";
import { ERROR_COPY } from "./errors";
import { useAction } from "./useAction";

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<T>((accept, decline) => { resolve = accept; reject = decline; });

  return { promise, resolve, reject };
}

describe("useAction", () => {
  it("is running until the work settles, then done with what the work said", async () => {
    const { result } = renderHook(() => useAction());
    const work = deferred<string>();
    let run!: Promise<void>;

    act(() => { run = result.current.run(() => work.promise); });
    expect(result.current.state).toEqual({ status: "running" });
    expect(result.current.running).toBe(true);
    await act(async () => { work.resolve("Saved."); await run; });
    expect(result.current.state).toEqual({ status: "done", text: "Saved.", followUpFailed: false });
    expect(result.current.running).toBe(false);
  });

  it("refuses a second run in the same tick and starts again once the first settles", async () => {
    const { result } = renderHook(() => useAction());
    const first = deferred<void>();
    let calls = 0;

    const counted = (answer: Promise<void>) => () => {
      calls++;

      return answer;
    };

    let run!: Promise<void>;

    act(() => {
      run = result.current.run(counted(first.promise));
      void result.current.run(counted(Promise.resolve()));
    });
    expect(calls).toBe(1);
    await act(async () => { first.resolve(); await run; });
    await act(() => result.current.run(counted(Promise.resolve())));
    expect(calls).toBe(2);
  });

  it("words a thrown error with errorText and the caller's fallback, and leaves running", async () => {
    const { result } = renderHook(() => useAction());

    await act(() => result.current.run(() => Promise.reject(new ApiError(409, "private", { code: "consent_teen_only" }))));
    expect(result.current.state).toEqual({ status: "failed", text: ERROR_COPY.consent_teen_only });
    await act(() => result.current.run(() => Promise.reject(new ApiError(500, "private", { code: "server_error" })), { fallback: "We couldn't save that." }));
    expect(result.current.state).toEqual({ status: "failed", text: "We couldn't save that. Please try again in a minute." });
  });

  it("goes back to idle when the work says it did nothing", async () => {
    const { result } = renderHook(() => useAction());

    await act(() => result.current.run(() => Promise.reject(new Error("offline"))));
    await act(() => result.current.run(() => Promise.resolve(false)));
    expect(result.current.state).toEqual({ status: "idle" });
  });

  it("keeps a work that succeeded apart from a follow-up that failed, running until both end", async () => {
    const { result } = renderHook(() => useAction());
    const reload = deferred<void>();
    let run!: Promise<void>;

    act(() => { run = result.current.run(() => Promise.resolve("Consent granted."), { followUp: () => reload.promise }); });
    await act(async () => { await Promise.resolve(); });
    expect(result.current.running).toBe(true);
    await act(async () => { reload.reject(new Error("offline")); await run; });
    expect(result.current.state).toEqual({ status: "done", text: "Consent granted.", followUpFailed: true });
  });

  it("skips the follow-up when the work failed", async () => {
    const { result } = renderHook(() => useAction());
    let followed = false;

    await act(() => result.current.run(() => Promise.reject(new Error("offline")), { followUp: async () => { followed = true; } }));
    expect(followed).toBe(false);
    expect(result.current.state.status).toBe("failed");
  });

  it("clears a done or failed state on a new attempt and on reset, never a run on its way", async () => {
    const { result } = renderHook(() => useAction());
    const work = deferred<string>();
    let run!: Promise<void>;

    await act(() => result.current.run(() => Promise.resolve("Sent.")));
    act(() => { run = result.current.run(() => work.promise); });
    expect(result.current.state).toEqual({ status: "running" });
    act(() => result.current.reset());
    expect(result.current.state).toEqual({ status: "running" });
    await act(async () => { work.resolve("Sent again."); await run; });
    act(() => result.current.reset());
    expect(result.current.state).toEqual({ status: "idle" });
  });

  it("still settles under StrictMode's rehearsal unmount", async () => {
    const { result } = renderHook(() => useAction(), { wrapper: StrictMode });

    await act(() => result.current.run(() => Promise.resolve("Saved.")));
    expect(result.current.state).toEqual({ status: "done", text: "Saved.", followUpFailed: false });
  });
});
