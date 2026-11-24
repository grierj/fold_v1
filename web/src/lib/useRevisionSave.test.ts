import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as core from "../api/core";
import { ApiError } from "../api/core";
import { answerNames, useRevisionSave, type RevisionRead } from "./useRevisionSave";

interface Value {
  title: string;
  note?: string;
}

const changed = () => new ApiError(409, "changed", { code: "event_changed" });

// The answer of a save that landed: the event saved, by id, with its new revision.
const landed = { event: { id: 7, revision: "r9" } };
const answers = answerNames("event", 7);
const fields = (v: Value) => ({ title: v.title, note: v.note ?? "" });
const readOf = (value: Value, revision = "r2", allowed = true): RevisionRead<Value> => ({ allowed, revision, value });

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => { resolve = r; });

  return { promise, resolve };
}

// Unless a test names it, the save would leave a title the reads never show.
function setup(read: () => Promise<RevisionRead<Value>>, options: { fields?: (v: Value) => ReturnType<typeof fields> | null; intended?: Value; lost?: { current: boolean } } = {}) {
  const onConflict = vi.fn();
  const view = renderHook(() => useRevisionSave({ opened: { title: "Supper" }, revision: "r1", read, fields: options.fields ?? fields, answers, lost: options.lost, onConflict }));
  const send = <R>(write: (revision: string | undefined) => Promise<R>, unseen?: string) => view.result.current(write, options.intended ?? { title: "Supper night" }, unseen);

  return { send, view, onConflict };
}

afterEach(() => vi.restoreAllMocks());

// DEC-162, DEC-163: the one conflict protocol for a manager's save.
describe("useRevisionSave", () => {
  it("sends the revision the form opened with and reads nothing when the save lands", async () => {
    const read = vi.fn();
    const write = vi.fn().mockResolvedValue(landed);
    const { send, onConflict } = setup(read);
    expect(await send(write)).toBe("saved");
    expect(write).toHaveBeenCalledExactlyOnceWith("r1");
    expect(read).not.toHaveBeenCalled();
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("reads once and sends again under the new revision when nothing it can write was changed", async () => {
    // An absent value reads as the form reads it, on both sides.
    const read = vi.fn().mockResolvedValue(readOf({ title: "Supper", note: "" }));
    const write = vi.fn().mockRejectedValueOnce(changed()).mockResolvedValueOnce(landed);
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

  it("counts a refused save as saved, sending nothing more, when what is stored is what it would leave", async () => {
    // An earlier press stored it and its answer was lost; an absent value reads as the form reads it here too.
    const read = vi.fn().mockResolvedValue(readOf({ title: "Supper night", note: "" }));
    const write = vi.fn().mockRejectedValue(changed());
    const { send, onConflict } = setup(read, { intended: { title: "Supper night" } });
    expect(await send(write)).toBe("saved");
    expect(write).toHaveBeenCalledExactlyOnceWith("r1");
    expect(read).toHaveBeenCalledOnce();
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("counts a save sent again by itself as saved when its next refusal finds what it would leave stored", async () => {
    const read = vi.fn().mockResolvedValueOnce(readOf({ title: "Supper" }, "r2")).mockResolvedValueOnce(readOf({ title: "Supper night" }, "r3"));
    const write = vi.fn().mockRejectedValue(changed());
    const { send, onConflict } = setup(read);
    expect(await send(write)).toBe("saved");
    expect(write.mock.calls).toEqual([["r1"], ["r2"]]);
    expect(read).toHaveBeenCalledTimes(2);
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("counts a refused save that changes nothing as saved after one read, sending nothing more", async () => {
    const read = vi.fn().mockResolvedValue(readOf({ title: "Supper" }));
    const write = vi.fn().mockRejectedValue(changed());
    const { send, onConflict } = setup(read, { intended: { title: "Supper" } });
    expect(await send(write)).toBe("saved");
    expect(write).toHaveBeenCalledOnce();
    expect(read).toHaveBeenCalledOnce();
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("never counts as saved a save the normaliser cannot judge, even when the read equals what it would leave", async () => {
    const write = vi.fn().mockRejectedValue(changed());
    const { send, onConflict } = setup(vi.fn().mockResolvedValue(readOf({ title: "Supper night" })), { fields: () => null, intended: { title: "Supper night" } });
    expect(await send(write)).toBe("conflict");
    expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed");
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

  it.each([403, 404])("hands over \"gone\" when the first save answers %i, reading nothing", async (status) => {
    const read = vi.fn();
    const write = vi.fn().mockRejectedValue(new ApiError(status, "no", { code: "x" }));
    const { send, onConflict } = setup(read);
    expect(await send(write)).toBe("conflict");
    expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone");
    expect(write).toHaveBeenCalledOnce();
    expect(read).not.toHaveBeenCalled();
  });

  it.each([403, 404])("hands over \"gone\" when a save it sent again by itself answers %i", async (status) => {
    const write = vi.fn().mockRejectedValueOnce(changed()).mockRejectedValueOnce(new ApiError(status, "no", { code: "x" }));
    const { send, onConflict } = setup(vi.fn().mockResolvedValue(readOf({ title: "Supper" })));
    expect(await send(write)).toBe("conflict");
    expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone");
    expect(write).toHaveBeenCalledTimes(2);
  });

  it("throws the first send's own refusal, a validation error or a decision, for the form to handle", async () => {
    const read = vi.fn();

    for (const err of [new ApiError(400, "no", { code: "visibility_not_allowed" }), new ApiError(409, "review", { needsDecision: true })]) {
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

  it("takes a single date's date_changed as its refusal when told to, and throws an event_changed it was not told of", async () => {
    const read = vi.fn().mockResolvedValue(readOf({ title: "Supper" }));
    const dateChanged = new ApiError(409, "changed", { code: "date_changed" });
    const view = renderHook(() => useRevisionSave({ opened: { title: "Supper" }, revision: "r1", read, fields, answers, changedCode: "date_changed" }));
    const write = vi.fn().mockRejectedValueOnce(dateChanged).mockResolvedValueOnce(landed);
    expect(await view.result.current(write, { title: "Supper night" })).toBe("saved");
    expect(write.mock.calls).toEqual([["r1"], ["r2"]]);
    const other = changed();
    await expect(view.result.current(vi.fn().mockRejectedValue(other), { title: "Supper night" })).rejects.toBe(other);
    expect(read).toHaveBeenCalledOnce();
  });

  describe("a save counts only when its answer names the thing saved", () => {
    it.each([
      ["an empty object (a page from something in between)", {}],
      ["another event's answer", { event: { id: 8, revision: "r9" } }],
      ["an answer with no revision", { event: { id: 7 } }],
      ["an answer with an empty revision", { event: { id: 7, revision: "" } }],
      ["an answer with the id as text", { event: { id: "7", revision: "r9" } }],
    ])("throws %s as an unreadable answer, for the form's ordinary error", async (_, answer) => {
      const read = vi.fn();
      const { send, onConflict } = setup(read);
      const err: unknown = await send(vi.fn().mockResolvedValue(answer)).catch((cause: unknown) => cause);
      expect(err).toBeInstanceOf(ApiError);
      expect(err).toMatchObject({ status: 502, unreadable: true });
      expect(read).not.toHaveBeenCalled();
      expect(onConflict).not.toHaveBeenCalled();
    });

    it("names a date by its occurrence", () => {
      const date = answerNames("occurrence", 3);
      expect(date({ occurrence: { id: 3, revision: "1.a" } })).toBe(true);
      expect(date({ occurrence: { id: 4, revision: "1.a" } })).toBe(false);
      expect(date({ event: { id: 3, revision: "1.a" } })).toBe(false);
      expect(date(null)).toBe(false);
      expect(date("ok")).toBe(false);
    });

    it("names a group by its id", () => {
      const group = answerNames("group", 3);
      expect(group({ group: { id: 3, revision: "1.a.g3.2" } })).toBe(true);
      expect(group({ group: { id: 3, revision: "" } })).toBe(false);
      expect(group({ group: { id: 4, revision: "1.a.g4.2" } })).toBe(false);
      expect(group({ event: { id: 3, revision: "1.a" } })).toBe(false);
      expect(group({ group: { id: 3 } })).toBe(false);
    });
  });

  // #455: once a send got no answer it may have been stored, so the form never sends again under a newer revision.
  describe("after a send that got no answer", () => {
    it.each([
      ["a lost connection", () => Promise.reject(new TypeError("Failed to fetch"))],
      ["a server error", () => Promise.reject(new ApiError(503, "down", {}, true))],
      ["an unreadable answer", () => Promise.resolve({})],
    ])("after %s, a refusal finding the value as it opened is a conflict, sending only the opened revision twice", async (_, lose) => {
      // Saved at r1 and stored, the answer lost; another manager put the opening value back (r3); Save again.
      const read = vi.fn().mockResolvedValue(readOf({ title: "Supper" }, "r3"));
      const write = vi.fn().mockImplementationOnce(lose).mockRejectedValue(changed());
      const { send, onConflict } = setup(read);
      await expect(send(write)).rejects.toBeDefined();
      expect(await send(write)).toBe("conflict");
      expect(write.mock.calls).toEqual([["r1"], ["r1"]]);
      expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed");
    });

    it("counts a refusal finding what it would leave as saved, and a landed save opens sending again", async () => {
      const read = vi.fn()
        .mockResolvedValueOnce(readOf({ title: "Supper night" }, "r2"))
        .mockResolvedValueOnce(readOf({ title: "Supper" }, "r3"));

      const write = vi.fn().mockRejectedValueOnce(new TypeError("Failed to fetch")).mockRejectedValueOnce(changed()).mockRejectedValueOnce(changed()).mockResolvedValueOnce(landed);
      const { send, onConflict } = setup(read);
      await expect(send(write)).rejects.toBeInstanceOf(TypeError);
      expect(await send(write)).toBe("saved");
      // Nothing is lost any more: as it opened, the same save goes again under the new revision.
      expect(await send(write)).toBe("saved");
      expect(write.mock.calls).toEqual([["r1"], ["r1"], ["r1"], ["r3"]]);
      expect(onConflict).not.toHaveBeenCalled();
    });

    it("keeps it in the form's own store across a remount", async () => {
      const lost = { current: false };
      const read = vi.fn().mockResolvedValue(readOf({ title: "Supper" }, "r3"));
      const first = setup(read, { lost });
      await expect(first.send(vi.fn().mockRejectedValue(new TypeError("Failed to fetch")))).rejects.toBeInstanceOf(TypeError);
      first.view.unmount();
      expect(lost.current).toBe(true);
      const again = setup(read, { lost });
      const write = vi.fn().mockRejectedValue(changed());
      expect(await again.send(write)).toBe("conflict");
      expect(write).toHaveBeenCalledExactlyOnceWith("r1");
    });

    // What a save carries that no read shows (a reason): only a send that got no answer may have stored it.
    it.each([
      ["a dropped connection", () => Promise.reject(new TypeError("Failed to fetch"))],
      ["an unreadable answer", () => Promise.resolve({})],
    ])("after %s, what is stored counts as this save only when that send carried the same unseen part", async (_, lose) => {
      const stored = () => vi.fn().mockResolvedValue(readOf({ title: "Supper night" }, "r2"));
      const kept = setup(stored());
      const write = vi.fn().mockImplementationOnce(lose).mockRejectedValue(changed());
      await expect(kept.send(write, "Choir")).rejects.toBeDefined();
      expect(await kept.send(write, "Choir")).toBe("saved");
      expect(kept.onConflict).not.toHaveBeenCalled();

      // The reason was edited between the presses: the stored save went out with the first one.
      const edited = setup(stored());
      const again = vi.fn().mockImplementationOnce(lose).mockRejectedValue(changed());
      await expect(edited.send(again, "Choir")).rejects.toBeDefined();
      expect(await edited.send(again, "Choir and friends")).toBe("conflict");
      expect(edited.onConflict).toHaveBeenCalledExactlyOnceWith("changed");
      expect(again.mock.calls).toEqual([["r1"], ["r1"]]);
    });

    it("never counts an unseen part as stored when no send carried it unanswered: a refusal was not stored, and a failed read says nothing", async () => {
      // Someone else stored the same value; this save's reason went nowhere.
      const first = setup(vi.fn().mockResolvedValue(readOf({ title: "Supper night" }, "r2")));
      expect(await first.send(vi.fn().mockRejectedValue(changed()), "Choir")).toBe("conflict");

      // Refused, and the read failed: the next press finds the value stored by someone else.
      const read = vi.fn().mockRejectedValueOnce(new TypeError("Failed to fetch")).mockResolvedValue(readOf({ title: "Supper night" }, "r2"));
      const second = setup(read);
      const write = vi.fn().mockRejectedValue(changed());
      await expect(second.send(write, "Choir")).rejects.toBeInstanceOf(TypeError);
      expect(await second.send(write, "Choir")).toBe("conflict");

      // With nothing unseen, the same read is the save.
      const plain = setup(vi.fn().mockResolvedValue(readOf({ title: "Supper night" }, "r2")));
      expect(await plain.send(vi.fn().mockRejectedValue(changed()))).toBe("saved");
    });

    it.each([
      ["a readable refusal", new ApiError(409, "review", { needsDecision: true })],
      ["the form's own stop (a question answered no)", Symbol("declined")],
    ])("does not count %s as a send with no answer", async (_, stop) => {
      const read = vi.fn().mockResolvedValue(readOf({ title: "Supper" }, "r3"));
      const write = vi.fn().mockRejectedValueOnce(stop).mockRejectedValueOnce(changed()).mockResolvedValueOnce(landed);
      const { send } = setup(read);
      await expect(send(write)).rejects.toBe(stop);
      expect(await send(write)).toBe("saved");
      expect(write.mock.calls).toEqual([["r1"], ["r1"], ["r3"]]);
    });
  });

  // A form that stays open after an outcome and is used again (a room control).
  describe("with stays", () => {
    // The answer says what was stored: here a title, which the server may store unlike the form sent it.
    let storedTitle = "";
    const stays = (): Value => ({ title: storedTitle });

    const answered = (revision: string, title: string) => async () => {
      storedTitle = title;

      return { event: { id: 7, revision } };
    };

    function open(read: () => Promise<RevisionRead<Value>>, gone?: (err: ApiError) => boolean) {
      const onConflict = vi.fn();
      const view = renderHook(() => useRevisionSave({ opened: { title: "Supper" }, revision: "r1", read, fields, answers, onConflict, stays, gone }));
      const send = <R>(write: (revision: string | undefined) => Promise<R>, intended: Value) => view.result.current(write, intended);

      return { send, onConflict, view };
    }

    it("hands nothing over when a send's \"gone\" refusal arrives after the control went away", async () => {
      let refuse!: (err: ApiError) => void;
      const write = vi.fn().mockReturnValue(new Promise((_, reject) => { refuse = reject; }));
      const { send, onConflict, view } = open(vi.fn());
      const sent = send(write, { title: "Brunch" });
      view.unmount();
      refuse(new ApiError(403, "no", { code: "not_allowed" }));
      expect(await sent).toBe("left");
      expect(onConflict).not.toHaveBeenCalled();
    });

    it("takes the read's refusal as \"gone\" only when the form names it", async () => {
      const off = new ApiError(404, "off", { code: "rooms_off" });
      const { send, onConflict } = open(vi.fn().mockRejectedValue(off), (err) => err.status === 404 && err.code === "not_found");
      await expect(send(vi.fn().mockRejectedValue(changed()), { title: "Brunch" })).rejects.toBe(off);
      expect(onConflict).not.toHaveBeenCalled();
    });

    it("names the revision a landed save answered on the next save, from the value the answer says was stored", async () => {
      const read = vi.fn().mockResolvedValue(readOf({ title: "Picnic" }, "r5"));
      const write = vi.fn().mockImplementationOnce(answered("r2", "Supper night")).mockImplementationOnce(answered("r3", "Brunch"));
      const { send, onConflict } = open(read);
      expect(await send(write, { title: "Supper night" })).toBe("saved");
      expect(await send(write, { title: "Brunch" })).toBe("saved");
      expect(write.mock.calls).toEqual([["r1"], ["r2"]]);
      // A refusal of the third finds the second as it was stored: nothing it writes changed, so it goes again.
      const third = vi.fn().mockRejectedValueOnce(changed()).mockImplementationOnce(answered("r6", "Picnic"));
      read.mockResolvedValueOnce(readOf({ title: "Brunch" }, "r5"));
      expect(await send(third, { title: "Picnic" })).toBe("saved");
      expect(third.mock.calls).toEqual([["r3"], ["r5"]]);
      expect(onConflict).not.toHaveBeenCalled();
    });

    it("takes the stored value from the answer, not from what was sent", async () => {
      // The server stored "Supper night" for a send of "supper night " (it normalises): the base is what it stored.
      const read = vi.fn().mockResolvedValue(readOf({ title: "Supper night" }, "r4"));
      const write = vi.fn().mockImplementationOnce(answered("r2", "Supper night")).mockRejectedValueOnce(changed()).mockImplementationOnce(answered("r5", "Brunch"));
      const { send, onConflict } = open(read);
      expect(await send(write, { title: "supper night " })).toBe("saved");
      expect(await send(write, { title: "Brunch" })).toBe("saved");
      expect(write.mock.calls).toEqual([["r1"], ["r2"], ["r4"]]);
      expect(onConflict).not.toHaveBeenCalled();
    });

    it("bases a new pick after a conflict on what the read found, with its revision", async () => {
      const read = vi.fn().mockResolvedValue(readOf({ title: "Brunch" }, "r3"));
      const write = vi.fn().mockRejectedValueOnce(changed()).mockImplementationOnce(answered("r4", "Picnic"));
      const { send, onConflict } = open(read);
      expect(await send(write, { title: "Supper night" })).toBe("conflict");
      expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed");
      expect(await send(write, { title: "Picnic" })).toBe("saved");
      expect(write.mock.calls).toEqual([["r1"], ["r3"]]);
    });

    it("bases the next save on the read that found a save already stored", async () => {
      const read = vi.fn().mockResolvedValue(readOf({ title: "Supper night" }, "r3"));
      const write = vi.fn().mockRejectedValueOnce(changed()).mockImplementationOnce(answered("r4", "Brunch"));
      const { send } = open(read);
      expect(await send(write, { title: "Supper night" })).toBe("saved");
      expect(await send(write, { title: "Brunch" })).toBe("saved");
      expect(write.mock.calls).toEqual([["r1"], ["r3"]]);
    });

    it("moves nothing on a send with no answer: a later refusal that finds the value as it was is a conflict, never a resend", async () => {
      const read = vi.fn().mockResolvedValue(readOf({ title: "Supper night" }, "r3"));

      const write = vi.fn()
        .mockImplementationOnce(answered("r2", "Supper night"))
        .mockRejectedValueOnce(new TypeError("Failed to fetch"))
        .mockRejectedValueOnce(changed());

      const { send, onConflict } = open(read);
      expect(await send(write, { title: "Supper night" })).toBe("saved");
      await expect(send(write, { title: "Brunch" })).rejects.toBeInstanceOf(TypeError);
      // The read shows the first save as it was: the lost send may have been stored and undone since.
      expect(await send(write, { title: "Picnic" })).toBe("conflict");
      expect(write.mock.calls).toEqual([["r1"], ["r2"], ["r2"]]);
      expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed");
    });

    it("hands over \"gone\" only for the refusals the form names, and throws the others for its own error", async () => {
      const read = vi.fn();
      const restricted = new ApiError(403, "no", { code: "room_restricted" });
      const { send, onConflict } = open(read, (err) => err.status === 403 && err.code === "not_allowed");
      await expect(send(vi.fn().mockRejectedValue(restricted), { title: "Brunch" })).rejects.toBe(restricted);
      expect(onConflict).not.toHaveBeenCalled();
      expect(await send(vi.fn().mockRejectedValue(new ApiError(403, "no", { code: "not_allowed" })), { title: "Brunch" })).toBe("conflict");
      expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone");
      expect(read).not.toHaveBeenCalled();
    });
  });
});
