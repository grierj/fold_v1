import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as core from "../api/core";
import { ApiError } from "../api/core";
import { useRevisionSave, type RevisionRead } from "./useRevisionSave";

interface Value {
  title: string;
  note?: string;
}

const changed = () => new ApiError(409, "changed", { code: "event_changed" });
const fields = (v: Value) => ({ title: v.title, note: v.note ?? "" });
const readOf = (value: Value, revision = "r2", allowed = true): RevisionRead<Value> => ({ allowed, revision, value });

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => { resolve = r; });

  return { promise, resolve };
}

function setup(read: () => Promise<RevisionRead<Value>>, options: { fields?: (v: Value) => unknown } = {}) {
  const onConflict = vi.fn();
  const view = renderHook(() => useRevisionSave<Value>({ opened: { title: "Supper" }, revision: "r1", read, fields: options.fields ?? fields, onConflict }));

  return { send: view.result.current, view, onConflict };
}

afterEach(() => vi.restoreAllMocks());

// DEC-162, DEC-163: the one conflict protocol for a manager's save.
describe("useRevisionSave", () => {
  it("sends the revision the form opened with and reads nothing when the save lands", async () => {
    const read = vi.fn();
    const write = vi.fn().mockResolvedValue({});
    const { send, onConflict } = setup(read);
    expect(await send(write)).toBe("saved");
    expect(write).toHaveBeenCalledExactlyOnceWith("r1");
    expect(read).not.toHaveBeenCalled();
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("reads once and sends again under the new revision when nothing it can write was changed", async () => {
    // An absent value reads as the form reads it, on both sides.
    const read = vi.fn().mockResolvedValue(readOf({ title: "Supper", note: "" }));
    const write = vi.fn().mockRejectedValueOnce(changed()).mockResolvedValueOnce({});
    const { send, onConflict } = setup(read);
    expect(await send(write)).toBe("saved");
    expect(write.mock.calls).toEqual([["r1"], ["r2"]]);
    expect(read).toHaveBeenCalledOnce();
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("hands over a conflict and sends nothing more when something it can write was changed", async () => {
    const write = vi.fn().mockRejectedValue(changed());
    const { send, onConflict } = setup(vi.fn().mockResolvedValue(readOf({ title: "Brunch" })));
    expect(await send(write)).toBe("conflict");
    expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed");
    expect(write).toHaveBeenCalledOnce();
  });

  it("hands over a conflict when the normaliser cannot tell from the read", async () => {
    const write = vi.fn().mockRejectedValue(changed());
    const { send, onConflict } = setup(vi.fn().mockResolvedValue(readOf({ title: "Supper" })), { fields: () => null });
    expect(await send(write)).toBe("conflict");
    expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed");
    expect(write).toHaveBeenCalledOnce();
  });

  it("stops after three refusals with nothing changed, never calling that a conflict", async () => {
    const read = vi.fn()
      .mockResolvedValueOnce(readOf({ title: "Supper" }, "r2"))
      .mockResolvedValueOnce(readOf({ title: "Supper" }, "r3"))
      .mockResolvedValueOnce(readOf({ title: "Supper" }, "r4"));
    const write = vi.fn().mockRejectedValue(changed());
    const { send, onConflict } = setup(read);
    expect(await send(write)).toBe("refused");
    expect(write.mock.calls).toEqual([["r1"], ["r2"], ["r3"]]);
    expect(read).toHaveBeenCalledTimes(3);
    expect(onConflict).not.toHaveBeenCalled();
  });

  it.each([
    ["a server error", new ApiError(500, "boom", { code: "server_error" })],
    ["a lost connection", new TypeError("Failed to fetch")],
  ])("throws %s on the read for the form's ordinary error", async (_, err) => {
    const write = vi.fn().mockRejectedValue(changed());
    const { send, onConflict } = setup(vi.fn().mockRejectedValue(err));
    await expect(send(write)).rejects.toBe(err);
    expect(onConflict).not.toHaveBeenCalled();
    expect(write).toHaveBeenCalledOnce();
  });

  it.each([403, 404])("hands over \"gone\" when the read answers %i", async (status) => {
    const write = vi.fn().mockRejectedValue(changed());
    const { send, onConflict } = setup(vi.fn().mockRejectedValue(new ApiError(status, "no", { code: "x" })));
    expect(await send(write)).toBe("conflict");
    expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone");
    expect(write).toHaveBeenCalledOnce();
  });

  it.each([
    ["no longer allows the save", readOf({ title: "Supper" }, "r2", false)],
    ["carries no revision", readOf({ title: "Supper" }, "")],
  ])("hands over \"gone\" when the read %s", async (_, stored) => {
    const write = vi.fn().mockRejectedValue(changed());
    const { send, onConflict } = setup(vi.fn().mockResolvedValue(stored));
    expect(await send(write)).toBe("conflict");
    expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone");
  });

  it.each([403, 404])("hands over \"gone\" when a save it sent again by itself answers %i", async (status) => {
    const write = vi.fn().mockRejectedValueOnce(changed()).mockRejectedValueOnce(new ApiError(status, "no", { code: "x" }));
    const { send, onConflict } = setup(vi.fn().mockResolvedValue(readOf({ title: "Supper" })));
    expect(await send(write)).toBe("conflict");
    expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone");
    expect(write).toHaveBeenCalledTimes(2);
  });

  it("throws the first send's own refusal, a 403 or a decision, for the form to handle", async () => {
    const read = vi.fn();
    for (const err of [new ApiError(403, "no", { code: "forbidden" }), new ApiError(409, "review", { needsDecision: true })]) {
      const { send, onConflict } = setup(read);
      await expect(send(vi.fn().mockRejectedValue(err))).rejects.toBe(err);
      expect(onConflict).not.toHaveBeenCalled();
    }
    expect(read).not.toHaveBeenCalled();
  });

  it("throws a decision on a save it sent again, so the form opens its review", async () => {
    const decision = new ApiError(409, "review", { needsDecision: true });
    const write = vi.fn().mockRejectedValueOnce(changed()).mockRejectedValueOnce(decision);
    const { send, onConflict } = setup(vi.fn().mockResolvedValue(readOf({ title: "Supper" })));
    await expect(send(write)).rejects.toBe(decision);
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("sends nothing more and tells no one when the form went away during the read", async () => {
    const stored = deferred<RevisionRead<Value>>();
    const write = vi.fn().mockRejectedValue(changed());
    const { send, view, onConflict } = setup(() => stored.promise);
    const sent = send(write);
    await act(async () => { await Promise.resolve(); });
    view.unmount();
    stored.resolve(readOf({ title: "Supper" }));
    expect(await sent).toBe("left");
    expect(write).toHaveBeenCalledOnce();
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("hands over \"gone\" and sends nothing more when the tab's sign-in changed during the read", async () => {
    const mine: core.Credential = { token: "t1", marker: "m1", viewer: 1, seen: "m1", verified: true };
    vi.spyOn(core, "currentCredential").mockReturnValue(mine);
    const still = vi.spyOn(core, "credentialCurrent").mockReturnValue(true);
    const stored = deferred<RevisionRead<Value>>();
    const write = vi.fn().mockRejectedValue(changed());
    const { send, onConflict } = setup(() => stored.promise);
    const sent = send(write);
    await act(async () => { await Promise.resolve(); });
    still.mockReturnValue(false);
    stored.resolve(readOf({ title: "Supper" }));
    expect(await sent).toBe("conflict");
    expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone");
    expect(still).toHaveBeenCalledWith(mine);
    expect(write).toHaveBeenCalledOnce();
  });
});
