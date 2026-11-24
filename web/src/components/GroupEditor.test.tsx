import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../api/core";
import { Groups } from "../api/groups";
import type { GroupDetailResponse, GroupSummary, VisibilityImpact } from "../types";
import { useAction } from "../lib/useAction";
import { ERROR_COPY } from "../lib/errors";
import { stubService } from "../test/service";
import { decisionsKey, GroupEditor, groupFields, type CoverStep, type GroupValue } from "./GroupEditor";

// A photo under 1 MB goes up as it is (prepareUpload); jsdom has no object URLs for its preview.
function stubObjectUrls() {
  vi.stubGlobal("URL", class extends URL {
    static createObjectURL = vi.fn(() => "blob:chosen");
    static revokeObjectURL = vi.fn();
  });
}

stubService(Groups, () => ({ get: vi.fn(), update: vi.fn(), visibilityImpact: vi.fn() }));

const group: GroupSummary = {
  id: 4, name: "Garden club", description: "Weeds", tags: ["outdoors"], imageUrl: "/uploads/covers/4/a.jpg", coverFocalX: 50, coverFocalY: 50,
  location: "Hall", leaderName: "Lee", memberCount: 3, nextEventAt: null, status: "active", leaderId: 1, visibility: "public",
  deletionRequested: false, memberLimit: 12, revision: "1.gen.g4.1",
};

const readOf = (g: Partial<GroupSummary>, revision = "1.gen.g4.2", canManage = true): GroupDetailResponse => ({
  group: { ...group, ...g, revision: canManage ? revision : undefined }, members: [], upcomingEvents: [], truncated: false, hasMore: false, nextCursor: "", canManage, isMember: true,
});

// The answer of a save that landed: the group, by id, with its new revision.
const landed = (g: Partial<GroupSummary> = {}) => ({ group: { ...group, ...g, revision: "1.gen.g4.9" } });
const changed = () => new ApiError(409, "changed", { code: "group_changed" });

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<T>((a, b) => { resolve = a; reject = b; });

  return { promise, resolve, reject };
}

function Host(props: { onSaved: (cover: CoverStep | null) => void; onConflict: (why: string) => void }) {
  const action = useAction();

  return <main id="main" tabIndex={-1}><p>{action.running ? "busy" : "idle"}</p><GroupEditor group={group} action={action} onCancel={() => {}} {...props} /></main>;
}

function setup() {
  const onSaved = vi.fn();
  const onConflict = vi.fn();
  render(<Host onSaved={onSaved} onConflict={onConflict} />);

  return { onSaved, onConflict, save: () => userEvent.click(screen.getByRole("button", { name: "Save group" })) };
}

afterEach(() => vi.unstubAllGlobals());

beforeEach(() => {
  vi.mocked(Groups.get).mockReset();
  vi.mocked(Groups.update).mockReset();
  vi.mocked(Groups.visibilityImpact).mockReset();
});

describe("groupFields", () => {
  // Each field as PUT /groups/:id stores it, so a value the form sends and the read that follows compare alike.
  const base: GroupValue = { name: "Garden club", description: "Weeds", location: "Hall", tags: ["outdoors"], visibility: "public", memberLimit: 12, coverFocalX: 50, coverFocalY: 50, imageUrl: "" };

  it.each<[string, Partial<GroupValue>, Partial<GroupValue>]>([
    ["a name with spaces around it", { name: "  Garden club \n" }, { name: "Garden club" }],
    ["a description with spaces around it", { description: "\tWeeds  " }, { description: "Weeds" }],
    ["a location with spaces around it", { location: " Hall " }, { location: "Hall" }],
    ["tags with spaces and empty ones", { tags: [" outdoors ", "", "  "] }, { tags: ["outdoors"] }],
    ["an empty tags list", { tags: [] }, { tags: [""] }],
    ["a focal point below 0", { coverFocalX: -5 }, { coverFocalX: 0 }],
    ["a focal point above 100", { coverFocalY: 140 }, { coverFocalY: 100 }],
  ])("reads %s as the server stores it", (_, typed, stored) => {
    expect(groupFields({ ...base, ...typed })).toEqual(groupFields({ ...base, ...stored }));
  });

  it("joins tags as the server does, and keeps every field apart", () => {
    expect(groupFields({ ...base, tags: ["a", " b "] }).tags).toBe("a,b");
    expect(groupFields({ ...base, tags: [] }).tags).toBe("");

    for (const change of [{ name: "Other" }, { description: "Other" }, { location: "Other" }, { tags: ["other"] }, { visibility: "private" as const },
      { memberLimit: 13 }, { memberLimit: null }, { coverFocalX: 10 }, { coverFocalY: 10 }, { imageUrl: "/uploads/covers/4/b.jpg" }]) {
      expect(groupFields({ ...base, ...change }), JSON.stringify(change)).not.toEqual(groupFields(base));
    }
  });

  it("reads a member limit cleared to null as no limit", () => {
    expect(groupFields({ ...base, memberLimit: null }).memberLimit).toBeNull();
  });

  it("serialises the leader's decisions whatever order they were made in", () => {
    expect(decisionsKey({ b: "keep", a: "tighten" })).toBe(decisionsKey({ a: "tighten", b: "keep" }));
    expect(decisionsKey({ a: "keep" })).not.toBe(decisionsKey({ a: "tighten" }));
    expect(decisionsKey(undefined)).toBe("");
  });
});

// The group form names its read (DEC-166), through the shared protocol (useRevisionSave).
describe("GroupEditor", () => {
  it("sends the revision it opened with and the trimmed fields, and is saved when the answer names the group", async () => {
    vi.mocked(Groups.update).mockResolvedValue(landed());
    const { onSaved, onConflict, save } = setup();
    await userEvent.clear(screen.getByLabelText(/Member limit/));
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith(null));
    expect(Groups.update).toHaveBeenCalledExactlyOnceWith(4, expect.objectContaining({ revision: "1.gen.g4.1", memberLimit: null, name: "Garden club" }));
    expect(Groups.get).not.toHaveBeenCalled();
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("reads once and sends the same save again under the new revision when nothing it writes was changed", async () => {
    // Someone approved the group or changed its leader: the revision moved, nothing the form writes did.
    vi.mocked(Groups.get).mockResolvedValue(readOf({ leaderName: "Someone else", status: "active" }));
    vi.mocked(Groups.update).mockRejectedValueOnce(changed()).mockResolvedValueOnce(landed({ name: "Garden club two" }));
    const { onSaved, onConflict, save } = setup();
    await userEvent.type(screen.getByLabelText("Group name"), " two");
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(vi.mocked(Groups.update).mock.calls.map(([, body]) => body.revision)).toEqual(["1.gen.g4.1", "1.gen.g4.2"]);
    expect(onConflict).not.toHaveBeenCalled();
  });

  it("counts a refused save as saved when what is stored is what it would leave", async () => {
    vi.mocked(Groups.get).mockResolvedValue(readOf({ name: "Garden club two" }));
    vi.mocked(Groups.update).mockRejectedValue(changed());
    const { onSaved, onConflict, save } = setup();
    await userEvent.type(screen.getByLabelText("Group name"), " two ");
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(Groups.update).toHaveBeenCalledOnce();
    expect(onConflict).not.toHaveBeenCalled();
  });

  it.each<[string, Partial<GroupSummary>]>([
    ["another name", { name: "Allotment" }],
    ["another visibility", { visibility: "private" }],
    ["a cleared member limit", { memberLimit: null }],
    ["another cover", { imageUrl: "/uploads/covers/4/b.jpg" }],
    ["no cover", { imageUrl: "" }],
  ])("is a conflict, sending nothing more, when the read finds %s", async (_, stored) => {
    vi.mocked(Groups.get).mockResolvedValue(readOf(stored));
    vi.mocked(Groups.update).mockRejectedValue(changed());
    const { onSaved, onConflict, save } = setup();
    await userEvent.type(screen.getByLabelText(/^Location/), " B");
    await save();
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
    expect(Groups.update).toHaveBeenCalledOnce();
    expect(onSaved).not.toHaveBeenCalled();
  });

  it.each<[string, () => void]>([
    ["the save is refused with 403", () => vi.mocked(Groups.update).mockRejectedValue(new ApiError(403, "forbidden", {}))],
    ["the save is refused with 404", () => vi.mocked(Groups.update).mockRejectedValue(new ApiError(404, "group not found", {}))],
    ["the read after a refusal answers 404", () => {
      vi.mocked(Groups.update).mockRejectedValue(changed());
      vi.mocked(Groups.get).mockRejectedValue(new ApiError(404, "group not found", {}));
    }],
    ["the read after a refusal no longer lets this person manage it", () => {
      vi.mocked(Groups.update).mockRejectedValue(changed());
      vi.mocked(Groups.get).mockResolvedValue(readOf({}, "", false));
    }],
  ])("is gone when %s", async (_, arrange) => {
    arrange();
    const { onSaved, onConflict, save } = setup();
    await save();
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone"));
    expect(onSaved).not.toHaveBeenCalled();
  });

  it.each([403, 404])("is gone when the tightening's preview answers %i, saving nothing", async (status) => {
    vi.mocked(Groups.visibilityImpact).mockRejectedValue(new ApiError(status, "gone", {}));
    const { onSaved, onConflict, save } = setup();
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    await save();
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone"));
    expect(Groups.update).not.toHaveBeenCalled();
    expect(onSaved).not.toHaveBeenCalled();
  });

  it.each<[string, unknown]>([
    ["an empty answer", {}],
    ["another group's answer", { group: { ...group, id: 5, revision: "1.gen.g5.2" } }],
    ["an answer with no revision", { group: { ...group, revision: undefined } }],
  ])("keeps the form and its draft, saying its ordinary error, on %s", async (_, answer) => {
    // SAFETY: a wrong answer on purpose; the editor reads it as unknown (answerNames).
    vi.mocked(Groups.update).mockResolvedValue(answer as Awaited<ReturnType<typeof Groups.update>>);
    const { onSaved, onConflict, save } = setup();
    await userEvent.type(screen.getByLabelText("Group name"), " two");
    await save();
    expect(await screen.findByRole("alert")).toHaveTextContent(/./);
    expect(screen.getByLabelText("Group name")).toHaveValue("Garden club two");
    expect(onSaved).not.toHaveBeenCalled();
    expect(onConflict).not.toHaveBeenCalled();
  });

  it.each<[string, () => Promise<never>]>([
    ["a dropped connection", () => Promise.reject(new TypeError("Failed to fetch"))],
    ["a server error", () => Promise.reject(new ApiError(503, "down", {}, true))],
  ])("after %s, the next Save ends as the conflict, never as a send under the newer revision", async (_, lose) => {
    // The lost send may have been stored; someone put the opening value back since. Sending again could undo that.
    vi.mocked(Groups.update).mockImplementationOnce(lose).mockRejectedValue(changed());
    vi.mocked(Groups.get).mockResolvedValue(readOf({}, "1.gen.g4.3"));
    const { onSaved, onConflict, save } = setup();
    await userEvent.type(screen.getByLabelText("Group name"), " two");
    await save();
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    await save();
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
    expect(vi.mocked(Groups.update).mock.calls.map(([, body]) => body.revision)).toEqual(["1.gen.g4.1", "1.gen.g4.1"]);
    expect(onSaved).not.toHaveBeenCalled();
  });

  it("after a dropped connection, the next Save is done when the read finds this save stored", async () => {
    vi.mocked(Groups.update).mockRejectedValueOnce(new TypeError("Failed to fetch")).mockRejectedValue(changed());
    vi.mocked(Groups.get).mockResolvedValue(readOf({ name: "Garden club two" }, "1.gen.g4.2"));
    const { onSaved, save } = setup();
    await userEvent.type(screen.getByLabelText("Group name"), " two");
    await save();
    await screen.findByRole("alert");
    await save();
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
  });

  it("says its ordinary refusal when every send is refused while nothing it writes changed", async () => {
    vi.mocked(Groups.update).mockRejectedValue(changed());
    vi.mocked(Groups.get).mockResolvedValue(readOf({}));
    const { onSaved, onConflict, save } = setup();
    await userEvent.type(screen.getByLabelText("Group name"), " two");
    await save();
    expect(await screen.findByRole("alert")).toHaveTextContent(ERROR_COPY.group_changed);
    expect(Groups.update).toHaveBeenCalledTimes(3);
    expect(onSaved).not.toHaveBeenCalled();
    expect(onConflict).not.toHaveBeenCalled();
  });

  describe("a tightening with a leader's decisions", () => {
    const impact: VisibilityImpact = {
      visibility: "private", currentVisibility: "public", tightened: [],
      leaderOpened: [{ id: 7, title: "Open supper", visibility: "public", registrants: 0 }], consent: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    };

    async function confirmKeep() {
      vi.mocked(Groups.visibilityImpact).mockResolvedValue(impact);
      const out = setup();
      await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
      await out.save();
      const dialog = await screen.findByRole("dialog");
      await userEvent.click(within(dialog).getByRole("radio", { name: /^Keep it public/ }));
      await userEvent.click(within(dialog).getByRole("button", { name: /^Make the group private/ }));

      return out;
    }

    it("sends the decisions with the save", async () => {
      vi.mocked(Groups.update).mockResolvedValue(landed({ visibility: "private" }));
      const { onSaved } = await confirmKeep();
      await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
      expect(Groups.update).toHaveBeenCalledWith(4, expect.objectContaining({ visibility: "private", openedEvents: { 7: "keep" }, consent: impact.consent, revision: "1.gen.g4.1" }));
    });

    it("is a conflict, never saved, when a refusal finds the same visibility stored by someone else", async () => {
      // Another manager made it private with their own decisions; no read shows which.
      vi.mocked(Groups.update).mockRejectedValue(changed());
      vi.mocked(Groups.get).mockResolvedValue(readOf({ visibility: "private" }));
      const { onSaved, onConflict } = await confirmKeep();
      await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
      expect(onSaved).not.toHaveBeenCalled();
    });

    it("is saved when the send that got no answer carried the same decisions", async () => {
      vi.mocked(Groups.update).mockRejectedValueOnce(new TypeError("Failed to fetch")).mockRejectedValue(changed());
      vi.mocked(Groups.get).mockResolvedValue(readOf({ visibility: "private" }));
      const { onSaved, onConflict } = await confirmKeep();
      await screen.findByText(/couldn't|could not/);
      const dialog = screen.getByRole("dialog");
      await userEvent.click(within(dialog).getByRole("button", { name: /^Make the group private/ }));
      await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
      expect(onConflict).not.toHaveBeenCalled();
    });
  });

  describe("the cover", () => {
    it("hands the page the removal only once the details are saved", async () => {
      vi.mocked(Groups.update).mockResolvedValue(landed());
      const { onSaved, save } = setup();
      await userEvent.click(screen.getByRole("checkbox", { name: "Remove the current cover" }));
      await save();
      await waitFor(() => expect(onSaved).toHaveBeenCalledExactlyOnceWith({ remove: true }));
      // The save itself never writes the cover's address.
      expect(vi.mocked(Groups.update).mock.calls[0][1]).not.toHaveProperty("imageUrl");
    });

    it("hands the page the picked photo only once the details are saved", async () => {
      const photo = new File(["x"], "cover.jpg", { type: "image/jpeg" });
      stubObjectUrls();
      const saving = deferred<ReturnType<typeof landed>>();
      vi.mocked(Groups.update).mockReturnValue(saving.promise);
      const { onSaved, save } = setup();
      await userEvent.upload(document.querySelector<HTMLInputElement>('input[type="file"]')!, photo);
      await screen.findByRole("button", { name: "Choose different image" });
      await save();
      await waitFor(() => expect(Groups.update).toHaveBeenCalledOnce());
      expect(onSaved).not.toHaveBeenCalled();
      await act(async () => { saving.resolve(landed()); await saving.promise; });
      expect(onSaved).toHaveBeenCalledExactlyOnceWith({ file: photo });
    });

    it("hands the page no cover step on a conflict", async () => {
      vi.mocked(Groups.update).mockRejectedValue(changed());
      vi.mocked(Groups.get).mockResolvedValue(readOf({ name: "Allotment" }));
      const { onSaved, onConflict, save } = setup();
      await userEvent.click(screen.getByRole("checkbox", { name: "Remove the current cover" }));
      await save();
      await waitFor(() => expect(onConflict).toHaveBeenCalledOnce());
      expect(onSaved).not.toHaveBeenCalled();
    });
  });
});
