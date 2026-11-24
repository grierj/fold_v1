import { createRosterWrites, RosterWritesProvider, type RosterWriteEnd, type RosterWrites } from "../lib/rosterWrites";
import { stubService } from "../test/service";
import { Events } from "../api/events";
import { ApiError } from "../api/core";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { EventVisibilityControl } from "./EventVisibility";
import type { EventDetailResponse, EventSeries, VisibilityImpact } from "../types";
import { testEvent } from "../test/fixtures";
import { watchChecks, writeAfterTheCheck } from "../test/rosterOps";

const { setVisibility, visibilityImpact } = vi.hoisted(() => ({ setVisibility: vi.fn(), visibilityImpact: vi.fn() }));

stubService(Events, () => ({ setVisibility, visibilityImpact }));

const A = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const B = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const decision = (impact: VisibilityImpact) => new ApiError(409, "Review this change before saving", { needsDecision: true, impact: JSON.parse(JSON.stringify(impact)) });

beforeEach(() => {
  setVisibility.mockReset();
  visibilityImpact.mockReset();
});

// Making one event more private previews and confirms, like a group (DEC-48),
// with the consent of the impact the server showed (DEC-158).
describe("event tightening", () => {
  it("shows the registrations it cancels and saves only after confirm, with the review's consent", async () => {
    visibilityImpact.mockResolvedValue({
      visibility: "invite_only",
      currentVisibility: "public",
      tightened: [{ id: 7, title: "Picnic", visibility: "public", registrants: 2 }],
      leaderOpened: [],
      consent: A,
    });
    setVisibility.mockResolvedValue({});
    const onSaved = vi.fn();
    render(<EventVisibilityControl eventId={7} groupName="Garden" groupVisibility="public" visibility="public" onSaved={onSaved} />);
    await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    expect(await screen.findByText("Making this event invite-only")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toHaveTextContent("2 people registered will be cancelled and notified");
    expect(screen.getByRole("dialog")).toHaveAccessibleDescription("This event is public now. Saving makes it invite-only, so fewer people will be able to see it.");
    expect(setVisibility).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Make this event invite-only" }));
    expect(setVisibility).toHaveBeenCalledWith(7, { visibility: "invite_only", reason: undefined, consent: A, resetOverrides: false });
    expect(onSaved).toHaveBeenCalled();
  });

  it("opening an event up saves without a preview, with an empty consent", async () => {
    visibilityImpact.mockResolvedValue({ visibility: "private", currentVisibility: "private", tightened: [], leaderOpened: [] });
    setVisibility.mockResolvedValue({});
    render(<EventVisibilityControl eventId={8} groupName="Garden" groupVisibility="private" visibility="private" onSaved={() => {}} />);
    await userEvent.click(screen.getByRole("radio", { name: /^Public/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    expect(visibilityImpact).toHaveBeenCalledTimes(1);
    expect(setVisibility).toHaveBeenCalledWith(8, { visibility: "public", reason: undefined, consent: "", resetOverrides: false });
  });

  it("a second Save while the preview is on its way sends no second preview (#431)", async () => {
    const tightening = { visibility: "invite_only" as const, currentVisibility: "public" as const, tightened: [{ id: 7, title: "Picnic", visibility: "public" as const, registrants: 1 }], leaderOpened: [], consent: A };
    let answer!: (impact: VisibilityImpact) => void;
    visibilityImpact.mockImplementation((_id: number, value: string) => (value === "invite_only" ? new Promise<VisibilityImpact>((resolve) => { answer = resolve; }) : Promise.resolve({ ...tightening, tightened: [] })));
    render(<EventVisibilityControl eventId={7} groupVisibility="public" visibility="public" onSaved={() => {}} />);
    await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
    const save = screen.getByRole("button", { name: "Save visibility" });
    await userEvent.click(save);
    await userEvent.click(save);
    fireEvent.submit(save.closest("form")!);
    const previews = () => visibilityImpact.mock.calls.filter((call) => call[1] === "invite_only").length;
    expect(previews()).toBe(1);
    await act(async () => { answer(tightening); });
    expect(await screen.findByText("Making this event invite-only")).toBeInTheDocument();
    expect(previews()).toBe(1);
    expect(setVisibility).not.toHaveBeenCalled();
  });

  describe("keyboard focus while Save is on its way", () => {
    const tightening = { visibility: "invite_only" as const, currentVisibility: "public" as const, tightened: [{ id: 7, title: "Picnic", visibility: "public" as const, registrants: 1 }], leaderOpened: [], consent: A };

    async function pressSave() {
      let answer!: (impact: VisibilityImpact) => void;
      let refuse!: (cause: unknown) => void;
      visibilityImpact.mockResolvedValueOnce({ ...tightening, tightened: [], visibility: "public" }).mockReturnValueOnce(new Promise<VisibilityImpact>((resolve, reject) => { answer = resolve; refuse = reject; }));
      render(<EventVisibilityControl eventId={7} groupVisibility="public" visibility="public" onSaved={() => {}} />);
      await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
      const save = screen.getByRole("button", { name: "Save visibility" });
      save.focus();
      await userEvent.keyboard("{Enter}");
      // Save is disabled under the keyboard, so the form holds focus, never the page.
      expect(save).toBeDisabled();
      expect(document.activeElement).toBe(save.closest("form"));

      return { save, answer, refuse };
    }

    it("gives focus back to Save when the preview fails", async () => {
      const { save, refuse } = await pressSave();
      await act(async () => { refuse(new Error("offline")); });
      expect(await screen.findByRole("alert")).toBeInTheDocument();
      expect(save).toHaveFocus();
    });

    it("gives focus back to Save when there is nothing to review and the save fails or succeeds", async () => {
      setVisibility.mockRejectedValueOnce(new ApiError(500, "private", { code: "server_error" })).mockResolvedValueOnce({});
      const { save, answer } = await pressSave();
      await act(async () => { answer({ ...tightening, tightened: [] }); });
      expect(await screen.findByRole("alert")).toBeInTheDocument();
      expect(save).toHaveFocus();
      visibilityImpact.mockResolvedValueOnce({ ...tightening, tightened: [] });
      await userEvent.keyboard("{Enter}");
      await waitFor(() => expect(setVisibility).toHaveBeenCalledTimes(2));
      await waitFor(() => expect(save).toBeEnabled());
      expect(save).toHaveFocus();
    });

    it("lets the review take focus when it opens", async () => {
      const { answer } = await pressSave();
      await act(async () => { answer(tightening); });
      expect(await screen.findByRole("heading", { name: "Making this event invite-only" })).toHaveFocus();
    });
  });

  it("re-opens the review from a 409's own impact and confirms with its consent, never previewing again", async () => {
    const first = { visibility: "invite_only" as const, currentVisibility: "public" as const, tightened: [{ id: 7, title: "Picnic", visibility: "public" as const, registrants: 1 }], leaderOpened: [], consent: A };
    const fresh = { ...first, tightened: [{ ...first.tightened[0], registrants: 3 }], consent: B };
    visibilityImpact.mockResolvedValue(first);
    setVisibility.mockRejectedValueOnce(decision(fresh)).mockResolvedValueOnce({});
    render(<EventVisibilityControl eventId={7} groupVisibility="public" visibility="public" onSaved={() => {}} />);
    await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    await userEvent.click(await screen.findByRole("button", { name: "Make this event invite-only" }));
    expect(await screen.findByText("We checked again before saving. Please review the changes below.")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toHaveTextContent("3 people registered will be cancelled and notified");
    expect(document.activeElement).toBe(screen.getByRole("heading", { name: "Making this event invite-only" }));
    expect(visibilityImpact).toHaveBeenCalledTimes(2);
    await userEvent.click(screen.getByRole("button", { name: "Make this event invite-only" }));
    expect(setVisibility).toHaveBeenLastCalledWith(7, { visibility: "invite_only", reason: undefined, consent: B, resetOverrides: false });
  });

  it("shows the outdated copy for an outdated answer and for a decision without a token", async () => {
    visibilityImpact.mockResolvedValue({ visibility: "invite_only", currentVisibility: "public", tightened: [{ id: 7, title: "Picnic", visibility: "public", registrants: 1 }], leaderOpened: [], consent: A });
    setVisibility.mockRejectedValueOnce(new ApiError(409, "x", { code: "visibility_review_outdated" }))
      .mockRejectedValueOnce(new ApiError(409, "x", { needsDecision: true, impact: { visibility: "invite_only", tightened: [], leaderOpened: [] } }));
    render(<EventVisibilityControl eventId={7} groupVisibility="public" visibility="public" onSaved={() => {}} />);
    await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    await userEvent.click(await screen.findByRole("button", { name: "Make this event invite-only" }));
    const copy = "Fold was updated while this page was open. Reload this page, then review the change again.";
    expect(await screen.findByText(copy)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Make this event invite-only" }));
    expect(await screen.findByText(copy)).toBeInTheDocument();
    expect(setVisibility).toHaveBeenCalledTimes(2);
  });

  it("Cancel from the keyboard lands on Save visibility with the choice and reason kept, after a review and after a 409", async () => {
    const first = { visibility: "private" as const, currentVisibility: "public" as const, tightened: [{ id: 7, title: "Picnic", visibility: "public" as const, registrants: 1 }], leaderOpened: [], consent: A };
    visibilityImpact.mockResolvedValue(first);
    setVisibility.mockRejectedValueOnce(decision({ ...first, consent: B }));
    render(<EventVisibilityControl eventId={7} visibility="public" churchWide onSaved={() => {}} />);
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    const save = screen.getByRole("button", { name: "Save visibility" });
    save.focus();
    await userEvent.keyboard("{Enter}");
    expect(await screen.findByRole("heading", { name: "Making this event private" })).toHaveFocus();
    await userEvent.tab();
    await userEvent.tab();
    expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save visibility" })).toHaveFocus();
    expect(screen.getByRole("radio", { name: /^Private/ })).toBeChecked();
    await userEvent.keyboard("{Enter}");
    expect(await screen.findByRole("heading", { name: "Making this event private" })).toHaveFocus();
    await userEvent.click(screen.getByRole("button", { name: "Make this event private" }));
    expect(await screen.findByText("We checked again before saving. Please review the changes below.")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Making this event private" })).toHaveFocus();
    screen.getByRole("button", { name: "Cancel" }).focus();
    await userEvent.keyboard("{Enter}");
    expect(screen.getByRole("button", { name: "Save visibility" })).toHaveFocus();
  });
});

describe("a reset of the event's dates", () => {
  const resetDates = [
    { occurrenceId: 51, date: "2026-12-01", title: "Picnic", visibility: "invite_only" as const },
    { occurrenceId: 52, date: "2026-12-08", title: "Picnic", visibility: "private" as const },
  ];

  it("shows the authoritative future-date count, and a reset that cancels nobody is reviewed first", async () => {
    visibilityImpact.mockResolvedValueOnce({ visibility: "public", currentVisibility: "public", futureOverrides: 2, tightened: [], leaderOpened: [] })
      .mockResolvedValueOnce({ visibility: "public", currentVisibility: "public", futureOverrides: 2, tightened: [], leaderOpened: [], resetDates, consent: A });
    setVisibility.mockResolvedValue({});
    render(<EventVisibilityControl eventId={19} visibility="public" groupVisibility="public" onSaved={() => {}} />);
    expect(await screen.findByText("2 dates have their own visibility and won't change.")).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText("Also reset those dates to the series setting"));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    expect(await screen.findByRole("heading", { name: "Reset this event's dates to the series setting" })).toHaveFocus();
    expect(setVisibility).not.toHaveBeenCalled();
    expect(screen.getByText("This resets 2 dates to the series setting (public):")).toBeInTheDocument();
    expect(screen.getByText("Tuesday, December 1. Now invite-only.")).toBeInTheDocument();
    expect(screen.getAllByText("After the reset, more people will be able to see this date.")).toHaveLength(2);
    expect(screen.getByRole("dialog").textContent).not.toContain("·");
    await userEvent.click(screen.getByRole("button", { name: "Reset 2 dates" }));
    expect(setVisibility).toHaveBeenCalledWith(19, { visibility: "public", reason: undefined, consent: A, resetOverrides: true });
  });

  it("a preview that changes nothing at all commits at once with its consent", async () => {
    visibilityImpact.mockResolvedValueOnce({ visibility: "private", currentVisibility: "private", futureOverrides: 1, tightened: [], leaderOpened: [] })
      .mockResolvedValueOnce({ visibility: "private", currentVisibility: "private", tightened: [], leaderOpened: [], resetDates: [], consent: A });
    setVisibility.mockResolvedValue({});
    render(<EventVisibilityControl eventId={19} visibility="private" groupVisibility="private" onSaved={() => {}} />);
    await userEvent.click(await screen.findByLabelText("Also reset those dates to the series setting"));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    expect(setVisibility).toHaveBeenCalledWith(19, { visibility: "private", reason: undefined, consent: A, resetOverrides: true });
  });

  it("opens the review when only the series value differs, from the server's current value", async () => {
    visibilityImpact.mockResolvedValueOnce({ visibility: "public", currentVisibility: "public", futureOverrides: 1, tightened: [], leaderOpened: [] })
      .mockResolvedValueOnce({ visibility: "public", currentVisibility: "invite_only", tightened: [], leaderOpened: [], resetDates: [], consent: A });
    render(<EventVisibilityControl eventId={19} visibility="public" groupVisibility="public" onSaved={() => {}} />);
    await userEvent.click(await screen.findByLabelText("Also reset those dates to the series setting"));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    const dialog = await screen.findByRole("dialog", { name: "Making this event public" });
    expect(dialog).toHaveAccessibleDescription("This event is invite-only now. Saving makes it public, so more people will be able to see it.");
    expect(screen.getByText("No future dates have their own visibility, so no date will be reset.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Make this event public" })).toBeInTheDocument();
    expect(setVisibility).not.toHaveBeenCalled();
  });

  it("a 409 on a reset that committed at once opens the review, named for the kept value when nothing is left", async () => {
    visibilityImpact.mockResolvedValueOnce({ visibility: "invite_only", currentVisibility: "invite_only", futureOverrides: 1, tightened: [], leaderOpened: [] })
      .mockResolvedValueOnce({ visibility: "invite_only", currentVisibility: "invite_only", tightened: [], leaderOpened: [], resetDates: [], consent: A });
    setVisibility.mockRejectedValueOnce(decision({ visibility: "invite_only", currentVisibility: "invite_only", futureOverrides: 0, tightened: [], leaderOpened: [], resetDates: [], consent: B }))
      .mockResolvedValueOnce({});
    render(<EventVisibilityControl eventId={19} visibility="invite_only" groupVisibility="invite_only" onSaved={() => {}} />);
    await userEvent.click(await screen.findByLabelText("Also reset those dates to the series setting"));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    const dialog = await screen.findByRole("dialog", { name: "No dates to reset" });
    expect(dialog).toHaveAccessibleDescription("This event is invite-only now and will stay invite-only.");
    expect(dialog.textContent).not.toContain("No dates have their own visibility");
    expect(dialog.textContent).not.toContain("Save visibility");
    await userEvent.click(screen.getByRole("button", { name: "Keep this event invite-only" }));
    expect(setVisibility).toHaveBeenLastCalledWith(19, { visibility: "invite_only", reason: undefined, consent: B, resetOverrides: true });
  });

  it("the review and the checkbox line take the newest count the server gave", async () => {
    visibilityImpact.mockResolvedValueOnce({ visibility: "private", currentVisibility: "public", futureOverrides: 2, tightened: [], leaderOpened: [] })
      .mockResolvedValueOnce({ visibility: "private", currentVisibility: "public", futureOverrides: 3, tightened: [{ id: 19, title: "Picnic", visibility: "public", registrants: 1 }], leaderOpened: [], resetDates: [...resetDates, { occurrenceId: 53, date: "2026-12-15", title: "Picnic", visibility: "public" }], consent: A });
    setVisibility.mockRejectedValueOnce(decision({ visibility: "private", currentVisibility: "public", futureOverrides: 1, tightened: [], leaderOpened: [], resetDates: resetDates.slice(0, 1), consent: B }));
    render(<EventVisibilityControl eventId={19} visibility="public" groupVisibility="public" onSaved={() => {}} />);
    expect(await screen.findByText("2 dates have their own visibility and won't change.")).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText("Also reset those dates to the series setting"));
    await userEvent.click(screen.getByRole("radio", { name: /^Private/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    expect(await screen.findByText("This resets 3 dates to the series setting (private):")).toBeInTheDocument();
    expect(screen.getByText("3 dates have their own visibility and won't change.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Make this event private" }));
    expect(await screen.findByText("This resets 1 date to the series setting (private):")).toBeInTheDocument();
    setVisibility.mockResolvedValueOnce({});
    await userEvent.click(screen.getByRole("button", { name: "Make this event private" }));
    expect(setVisibility).toHaveBeenLastCalledWith(19, { visibility: "private", reason: undefined, consent: B, resetOverrides: true });
  });
});

// VC2: a review opens (from a preview or a 409) and is confirmed only when no sign-up update on the event began,
// ended or is on its way since the request it comes from was sent; Save and confirm wait while one is on its way.
// The updates come through the session's store, as a roster's Add, Add back or Remove would start them.
describe("sign-up updates on the event", () => {
  const STALE = "A sign-up update started while we were checking. Please review again once it finishes.";
  const WAITING = "Saving waits for a sign-up update to finish.";
  const KEY = "7:2026-12-01";
  const tightening = { visibility: "invite_only" as const, currentVisibility: "public" as const, tightened: [{ id: 7, title: "Picnic", visibility: "public" as const, registrants: 2 }], leaderOpened: [], consent: A };
  const nothing = { visibility: "invite_only" as const, currentVisibility: "public" as const, tightened: [], leaderOpened: [], consent: A };

  function held<T>() {
    let resolve: (value: T) => void = () => {};
    let reject: (cause: unknown) => void = () => {};
    const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });

    return { promise, resolve, reject };
  }

  function mount(store: RosterWrites, eventId = 7) {
    const onSaved = vi.fn();
    render(<RosterWritesProvider rosterWrites={store}><EventVisibilityControl eventId={eventId} groupName="Garden" groupVisibility="public" visibility="public" onSaved={onSaved} /></RosterWritesProvider>);

    return onSaved;
  }

  const save = () => screen.getByRole("button", { name: "Save visibility" });
  const form = () => save().closest("form");

  async function choose() {
    await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
  }

  function expectDraft() {
    expect(screen.getByRole("radio", { name: /^Invite only/ })).toBeChecked();
  }

  // The mount's count answers first; the Save's preview is held.
  function holdPreview() {
    const preview = held<VisibilityImpact>();
    visibilityImpact.mockResolvedValueOnce({ ...nothing, currentVisibility: "public", visibility: "public" }).mockReturnValueOnce(preview.promise);

    return preview;
  }

  for (const [label, answer] of [["a preview with rows", tightening], ["a zero-row preview that would have committed at once", nothing]] as const) {
    it(`a begin during ${label}'s await opens no review, sends nothing and shows the stale line with focus on the form`, async () => {
      const store = createRosterWrites();
      const preview = holdPreview();
      mount(store);
      await choose();
      save().focus();
      await userEvent.keyboard("{Enter}");
      let end: (e: RosterWriteEnd) => void = () => {};

      await act(async () => { end = store.begin(KEY); preview.resolve(answer); await preview.promise; });
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(setVisibility).not.toHaveBeenCalled();
      expect(screen.getByText(STALE)).toBeInTheDocument();
      expect(document.activeElement).toBe(form());
      expectDraft();
      expect(save()).toBeDisabled();
      expect(screen.getByText(WAITING)).toBeInTheDocument();
      act(() => end("answered"));
      expect(save()).toBeEnabled();
      expect(screen.queryByText(WAITING)).not.toBeInTheDocument();
    });
  }

  for (const ending of ["stored", "answered"] as const) {
    it(`a write that began before the mark and ended ${ending} during the preview's await opens no review`, async () => {
      const store = createRosterWrites();
      const end = store.begin(KEY);
      const preview = holdPreview();
      mount(store);
      await choose();
      // Save waits while the write is on its way: nothing is previewed.
      expect(save()).toBeDisabled();
      expect(screen.getByText(WAITING)).toBeInTheDocument();
      fireEvent.submit(requiredForm());
      expect(visibilityImpact).toHaveBeenCalledTimes(1);
      act(() => end(ending));
      expect(save()).toBeEnabled();
      // A second write begins and ends while the preview is on its way.
      await userEvent.click(save());
      await act(async () => { store.begin(KEY)(ending); preview.resolve(tightening); await preview.promise; });
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(screen.getByText(STALE)).toBeInTheDocument();
      expect(setVisibility).not.toHaveBeenCalled();
      expectDraft();
      // After the end the next Save previews again and shows the current impact.
      visibilityImpact.mockResolvedValueOnce({ ...tightening, tightened: [{ ...tightening.tightened[0], registrants: 3 }], consent: B });
      await userEvent.click(save());
      expect(await screen.findByRole("dialog")).toHaveTextContent("3 people registered will be cancelled and notified");
      expect(screen.queryByText(STALE)).not.toBeInTheDocument();
      setVisibility.mockResolvedValueOnce({});
      await userEvent.click(screen.getByRole("button", { name: "Make this event invite-only" }));
      expect(setVisibility).toHaveBeenCalledExactlyOnceWith(7, { visibility: "invite_only", reason: undefined, consent: B, resetOverrides: false });
    });
  }

  it("a write on another event changes nothing", async () => {
    const store = createRosterWrites();
    const preview = holdPreview();
    mount(store);
    await choose();
    await userEvent.click(save());
    await act(async () => { store.begin("8:2026-12-01"); preview.resolve(tightening); await preview.promise; });
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(screen.queryByText(STALE)).not.toBeInTheDocument();
    expect(save()).toBeEnabled();
  });

  // The seam between the component accepting an answer and watching for writes: a write that begins and ends
  // there has to withdraw the review before anyone sees it.
  for (const ending of ["stored", "answered"] as const) {
    it(`a write that begins and ends ${ending} after the preview's check, before the review is watched, never shows it`, async () => {
      const store = createRosterWrites();
      const checks = watchChecks(store);
      const preview = holdPreview();
      mount(store);
      await choose();
      save().focus();
      await userEvent.keyboard("{Enter}");
      await writeAfterTheCheck(checks, async () => { preview.resolve(tightening); await preview.promise; }, store, KEY, ending);
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(screen.getByText(STALE)).toBeInTheDocument();
      expect(document.activeElement).toBe(form());
      expect(setVisibility).not.toHaveBeenCalled();
      expectDraft();
      expect(save()).toBeEnabled();
      // The next Save previews again and confirms the current impact, once.
      visibilityImpact.mockResolvedValueOnce({ ...tightening, tightened: [{ ...tightening.tightened[0], registrants: 3 }], consent: B });
      await userEvent.click(save());
      expect(await screen.findByRole("dialog")).toHaveTextContent("3 people registered will be cancelled and notified");
      expect(screen.queryByText(STALE)).not.toBeInTheDocument();
      setVisibility.mockResolvedValueOnce({});
      await userEvent.click(screen.getByRole("button", { name: "Make this event invite-only" }));
      expect(setVisibility).toHaveBeenCalledExactlyOnceWith(7, { visibility: "invite_only", reason: undefined, consent: B, resetOverrides: false });
    });
  }

  it("a write that begins and ends after a 409's check, on a save that opened no review, never shows the 409's review", async () => {
    const store = createRosterWrites();
    const checks = watchChecks(store);
    visibilityImpact.mockResolvedValue(nothing);
    const put = held<unknown>();
    setVisibility.mockReturnValueOnce(put.promise);
    mount(store);
    await choose();
    save().focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(setVisibility).toHaveBeenCalledOnce());
    await writeAfterTheCheck(checks, async () => { put.reject(decision({ ...tightening, consent: B })); await put.promise.catch(() => {}); }, store, KEY, "stored");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByText(STALE)).toBeInTheDocument();
    expect(screen.queryByText("We checked again before saving. Please review the changes below.")).not.toBeInTheDocument();
    expect(document.activeElement).toBe(form());
    expect(setVisibility).toHaveBeenCalledOnce();
    expectDraft();
  });

  it("a write in that seam while focus is on a roster outside the form leaves focus there", async () => {
    const store = createRosterWrites();
    const checks = watchChecks(store);
    const preview = holdPreview();
    render(<RosterWritesProvider rosterWrites={store}><button type="button">Roster</button><EventVisibilityControl eventId={7} groupName="Garden" groupVisibility="public" visibility="public" onSaved={() => {}} /></RosterWritesProvider>);
    await choose();
    await userEvent.click(save());
    const roster = screen.getByRole("button", { name: "Roster" });
    roster.focus();
    await writeAfterTheCheck(checks, async () => { preview.resolve(tightening); await preview.promise; }, store, KEY, "answered");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByText(STALE)).toBeInTheDocument();
    expect(document.activeElement).toBe(roster);
    expect(setVisibility).not.toHaveBeenCalled();
    expectDraft();
  });

  it("a begin while the review is open closes it with the stale line and sends nothing", async () => {
    const store = createRosterWrites();
    visibilityImpact.mockResolvedValue(tightening);
    mount(store);
    await choose();
    await userEvent.click(save());
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    let end: (e: RosterWriteEnd) => void = () => {};

    act(() => { end = store.begin(KEY); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByText(STALE)).toBeInTheDocument();
    expect(setVisibility).not.toHaveBeenCalled();
    // Focus was on the review's heading, which went with it: the form keeps it, never the page.
    expect(document.activeElement).toBe(form());
    expectDraft();
    act(() => end("stored"));
    expect(save()).toBeEnabled();
    expect(setVisibility).not.toHaveBeenCalled();
  });

  it("a begin during the commit's await, then a 409 decision: the review closes, none opens from the 409, no second PUT", async () => {
    const store = createRosterWrites();
    visibilityImpact.mockResolvedValue(tightening);
    const put = held<unknown>();
    setVisibility.mockReturnValueOnce(put.promise);
    mount(store);
    await choose();
    await userEvent.click(save());
    await userEvent.click(await screen.findByRole("button", { name: "Make this event invite-only" }));
    act(() => { store.begin(KEY); });
    // The commit on its way decides from its own answer.
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await act(async () => { put.reject(decision({ ...tightening, consent: B })); await put.promise.catch(() => {}); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByText(STALE)).toBeInTheDocument();
    expect(screen.queryByText("We checked again before saving. Please review the changes below.")).not.toBeInTheDocument();
    expect(document.activeElement).toBe(form());
    expect(setVisibility).toHaveBeenCalledOnce();
    expectDraft();
  });

  it("a begin during the commit's await, then a 200: saved, with no stale line", async () => {
    const store = createRosterWrites();
    visibilityImpact.mockResolvedValue(tightening);
    const put = held<unknown>();
    setVisibility.mockReturnValueOnce(put.promise);
    const onSaved = mount(store);
    await choose();
    await userEvent.click(save());
    await userEvent.click(await screen.findByRole("button", { name: "Make this event invite-only" }));
    act(() => { store.begin(KEY); });
    await act(async () => { put.resolve({}); await put.promise; });
    expect(onSaved).toHaveBeenCalledOnce();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.queryByText(STALE)).not.toBeInTheDocument();
  });

  it("a 409 decision with no sign-up update since the commit's mark opens the fresh review as before", async () => {
    const store = createRosterWrites();
    visibilityImpact.mockResolvedValue(tightening);
    setVisibility.mockRejectedValueOnce(decision({ ...tightening, consent: B }));
    mount(store);
    await choose();
    // A write that began and ended before the commit's mark does not count against the 409.
    await userEvent.click(save());
    await screen.findByRole("dialog");
    act(() => { store.begin("8:2026-12-01")("stored"); });
    await userEvent.click(screen.getByRole("button", { name: "Make this event invite-only" }));
    expect(await screen.findByText("We checked again before saving. Please review the changes below.")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("Cancel pressed as a write begins leaves focus on the form, never the page, with the draft kept", async () => {
    const store = createRosterWrites();
    visibilityImpact.mockResolvedValue(tightening);
    mount(store);
    await choose();
    await userEvent.click(save());
    const cancel = within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel" });
    cancel.focus();
    act(() => { store.begin(KEY); cancel.click(); });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(save()).toBeDisabled();
    expect(document.activeElement).toBe(form());
    expect(document.activeElement).not.toBe(document.body);
    expectDraft();
  });

  function requiredForm() {
    const f = form();

    if (!f) throw new Error("no form");

    return f;
  }
});

const { get } = vi.hoisted(() => ({ get: vi.fn() }));

stubService(Events, () => ({ get }));

// DEC-163: the save names the read the control opened with; a refusal is checked against the event as stored.
describe("EventVisibilityControl and the read it opened with", () => {
  const changed = () => new ApiError(409, "changed", { code: "event_changed" });

  const readOf = (over: Partial<EventSeries> = {}, canSetVisibility = true): EventDetailResponse => ({
    event: { ...testEvent(7), groupId: 3, groupName: "Garden", groupVisibility: "public", visibility: "private", revision: "r2", ...over },
    occurrences: [], canManage: canSetVisibility, canSetVisibility,
  });

  const tightening = { visibility: "invite_only" as const, currentVisibility: "private" as const, tightened: [{ id: 7, title: "Picnic", visibility: "private" as const, registrants: 1 }], leaderOpened: [], consent: A };

  beforeEach(() => {
    get.mockReset();
    visibilityImpact.mockResolvedValue({ visibility: "private", currentVisibility: "private", tightened: [], leaderOpened: [] });
  });

  function open(onSaved = vi.fn(), onConflict = vi.fn()) {
    render(<EventVisibilityControl eventId={7} groupName="Garden" groupVisibility="public" visibility="private" revision="r1" onSaved={onSaved} onConflict={onConflict} />);

    return { onSaved, onConflict };
  }

  async function makePublic() {
    await userEvent.click(screen.getByRole("radio", { name: /^Public/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
  }

  it("sends the revision it opened with", async () => {
    setVisibility.mockResolvedValue({});
    const { onSaved } = open();
    await makePublic();
    expect(setVisibility).toHaveBeenCalledExactlyOnceWith(7, { visibility: "public", reason: undefined, consent: "", resetOverrides: false, revision: "r1" });
    expect(onSaved).toHaveBeenCalledOnce();
    expect(get).not.toHaveBeenCalled();
  });

  it("reads once and saves again under the new revision, showing nothing, when the audience is as it opened", async () => {
    setVisibility.mockRejectedValueOnce(changed()).mockResolvedValueOnce({});
    // A room booking moved the revision; leaderOpened absent on one side reads as false.
    get.mockResolvedValue(readOf({ leaderOpened: false, location: "Room 201" }));
    const { onSaved, onConflict } = open();
    await makePublic();
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
    expect(get).toHaveBeenCalledExactlyOnceWith(7);
    expect(setVisibility.mock.calls.map(([, body]) => body.revision)).toEqual(["r1", "r2"]);
    expect(setVisibility.mock.calls[1][1]).toEqual({ ...setVisibility.mock.calls[0][1], revision: "r2" });
    expect(onConflict).not.toHaveBeenCalled();
    expect(screen.queryByText(/couldn't/)).not.toBeInTheDocument();
  });

  it.each([
    ["the audience", { visibility: "invite_only" as const }],
    ["whether a leader opened it", { leaderOpened: true }],
    ["the group's audience", { groupVisibility: "private" as const }],
  ])("hands the page a conflict, sending nothing more, when %s changed", async (_, over) => {
    setVisibility.mockRejectedValue(changed());
    get.mockResolvedValue(readOf(over));
    const { onSaved, onConflict } = open();
    await makePublic();
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
    expect(setVisibility).toHaveBeenCalledOnce();
    expect(onSaved).not.toHaveBeenCalled();
  });

  it("hands the page \"gone\" when the read no longer lets this person set the visibility", async () => {
    setVisibility.mockRejectedValue(changed());
    get.mockResolvedValue(readOf({ revision: undefined }, false));
    const { onConflict } = open();
    await makePublic();
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone"));
    expect(setVisibility).toHaveBeenCalledOnce();
  });

  it.each([403, 404])("hands the page \"gone\" when the preview itself answers %i, and shows no error of its own", async (status) => {
    visibilityImpact.mockRejectedValue(new ApiError(status, "no", { code: "x" }));
    const { onConflict } = open();
    await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("gone"));
    expect(setVisibility).not.toHaveBeenCalled();
    expect(screen.queryByText("We couldn't check this event.")).not.toBeInTheDocument();
  });

  it("a save sent again that now needs a review opens the review with the server's impact", async () => {
    visibilityImpact.mockResolvedValueOnce({ visibility: "private", currentVisibility: "private", tightened: [], leaderOpened: [] }).mockResolvedValueOnce(tightening);
    setVisibility.mockRejectedValueOnce(changed()).mockRejectedValueOnce(decision({ ...tightening, tightened: [{ ...tightening.tightened[0], registrants: 2 }], consent: B }));
    get.mockResolvedValue(readOf());
    const { onSaved, onConflict } = open();
    await userEvent.click(screen.getByRole("radio", { name: /^Invite only/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    await userEvent.click(await screen.findByRole("button", { name: "Make this event invite-only" }));
    expect(await screen.findByText("We checked again before saving. Please review the changes below.")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toHaveTextContent("2 people registered will be cancelled and notified");
    expect(setVisibility.mock.calls.map(([, body]) => [body.revision, body.consent])).toEqual([["r1", A], ["r2", A]]);
    expect(onConflict).not.toHaveBeenCalled();
    expect(onSaved).not.toHaveBeenCalled();
  });

  it("hands the page a conflict on a refused reset, which the page's read cannot show is the same", async () => {
    visibilityImpact.mockResolvedValueOnce({ visibility: "private", currentVisibility: "private", futureOverrides: 1, tightened: [], leaderOpened: [] })
      .mockResolvedValueOnce({ visibility: "private", currentVisibility: "private", tightened: [], leaderOpened: [], resetDates: [{ occurrenceId: 51, date: "2026-12-01", title: "Picnic", visibility: "public" as const }], consent: A });
    setVisibility.mockRejectedValue(changed());
    get.mockResolvedValue(readOf());
    const { onConflict } = open();
    await userEvent.click(await screen.findByLabelText("Also reset those dates to the series setting"));
    await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    await userEvent.click(await screen.findByRole("button", { name: "Reset 1 date" }));
    await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
    expect(setVisibility).toHaveBeenCalledOnce();
  });

  it("says the ordinary error after three refusals with nothing changed", async () => {
    setVisibility.mockRejectedValue(changed());
    get.mockResolvedValue(readOf());
    const { onSaved, onConflict } = open();
    await makePublic();
    expect(await screen.findByText("We couldn't change who can see this. Please try again.")).toBeInTheDocument();
    expect(setVisibility).toHaveBeenCalledTimes(3);
    expect(onConflict).not.toHaveBeenCalled();
    expect(onSaved).not.toHaveBeenCalled();
    expect(screen.getByRole("radio", { name: /^Public/ })).toBeChecked();
  });

  // DEC-163: the first press was stored and its answer lost; the second is refused and its read shows the save done.
  describe("when a save it pressed before was stored and its answer lost", () => {
    const offline = "We couldn't reach Fold. Check your connection and try again.";

    beforeEach(() => {
      setVisibility.mockRejectedValueOnce(new TypeError("Failed to fetch")).mockRejectedValueOnce(changed());
    });

    it.each([
      ["the group's own audience", "public" as const, false],
      ["wider than the group, as leader-opened", "private" as const, true],
    ])("counts the second press as saved, sending nothing more, when the read shows Public %s", async (_, groupVisibility, leaderOpened) => {
      get.mockResolvedValue(readOf({ visibility: "public", groupVisibility, leaderOpened, revision: "r3" }));
      const onSaved = vi.fn();
      const onConflict = vi.fn();
      render(<EventVisibilityControl eventId={7} groupName="Garden" groupVisibility={groupVisibility} visibility="private" revision="r1" onSaved={onSaved} onConflict={onConflict} />);
      await makePublic();
      expect(await screen.findByText(offline)).toBeInTheDocument();
      await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
      await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
      expect(onConflict).not.toHaveBeenCalled();
      expect(setVisibility.mock.calls.map(([, body]) => body.revision)).toEqual(["r1", "r1"]);
      expect(get).toHaveBeenCalledExactlyOnceWith(7);
      expect(screen.queryByText(/couldn't/)).not.toBeInTheDocument();
    });

    it("hands the page a conflict when the reason was edited between a lost press and the next", async () => {
      const wider = { visibility: "public" as const, groupVisibility: "private" as const, leaderOpened: true, revision: "r3" };
      get.mockResolvedValue(readOf(wider));
      const onSaved = vi.fn();
      const onConflict = vi.fn();
      render(<EventVisibilityControl eventId={7} groupName="Garden" groupVisibility="private" visibility="private" revision="r1" onSaved={onSaved} onConflict={onConflict} />);
      await userEvent.click(screen.getByRole("radio", { name: /^Public/ }));
      await userEvent.type(screen.getByRole("textbox", { name: /^Reason/ }), "Neighbours welcome");
      await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
      expect(await screen.findByText(offline)).toBeInTheDocument();
      // No read returns the reason: with another reason the stored save cannot be shown to be this one.
      await userEvent.type(screen.getByRole("textbox", { name: /^Reason/ }), " and their friends");
      await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
      await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
      expect(onSaved).not.toHaveBeenCalled();
      expect(setVisibility.mock.calls.map(([, body]) => body.reason)).toEqual(["Neighbours welcome", "Neighbours welcome and their friends"]);
    });

    it("counts the second press as saved when it carries the reason the lost press sent", async () => {
      get.mockResolvedValue(readOf({ visibility: "public", groupVisibility: "private", leaderOpened: true, revision: "r3" }));
      const onSaved = vi.fn();
      const onConflict = vi.fn();
      render(<EventVisibilityControl eventId={7} groupName="Garden" groupVisibility="private" visibility="private" revision="r1" onSaved={onSaved} onConflict={onConflict} />);
      await userEvent.click(screen.getByRole("radio", { name: /^Public/ }));
      await userEvent.type(screen.getByRole("textbox", { name: /^Reason/ }), "Neighbours welcome");
      await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
      expect(await screen.findByText(offline)).toBeInTheDocument();
      await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
      await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
      expect(onConflict).not.toHaveBeenCalled();
    });

    it("does not take a refused save for a lost one when the read after the refusal fails", async () => {
      setVisibility.mockReset().mockRejectedValue(changed());
      get.mockRejectedValueOnce(new TypeError("Failed to fetch")).mockResolvedValue(readOf({ visibility: "public", groupVisibility: "private", leaderOpened: true, revision: "r3" }));
      const onSaved = vi.fn();
      const onConflict = vi.fn();
      render(<EventVisibilityControl eventId={7} groupName="Garden" groupVisibility="private" visibility="private" revision="r1" onSaved={onSaved} onConflict={onConflict} />);
      await userEvent.click(screen.getByRole("radio", { name: /^Public/ }));
      await userEvent.type(screen.getByRole("textbox", { name: /^Reason/ }), "Neighbours welcome");
      await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
      expect(await screen.findByText(offline)).toBeInTheDocument();
      // The first save was refused, never stored: the audience the read now shows is someone else's, with their reason.
      await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
      await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
      expect(onSaved).not.toHaveBeenCalled();
    });

    it("hands the page a conflict when the read shows Public with a leader-opened mark the save would not leave", async () => {
      get.mockResolvedValue(readOf({ visibility: "public", leaderOpened: true, revision: "r3" }));
      const { onSaved, onConflict } = open();
      await makePublic();
      expect(await screen.findByText(offline)).toBeInTheDocument();
      await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
      await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
      expect(onSaved).not.toHaveBeenCalled();
    });

    // A reset alone leaves the event's revision, so its second press is not refused. With an audience change it is,
    // and the page's read lists 90 days of dates while a reset reaches every later one: it cannot show the reset stored.
    it("hands the page a conflict on a reset with an audience change, even when the second press's preview finds nothing left to do", async () => {
      visibilityImpact.mockResolvedValueOnce({ visibility: "private", currentVisibility: "private", futureOverrides: 1, tightened: [], leaderOpened: [] })
        .mockResolvedValueOnce({ visibility: "public", currentVisibility: "private", tightened: [], leaderOpened: [], resetDates: [{ occurrenceId: 51, date: "2026-12-01", title: "Picnic", visibility: "invite_only" as const }], consent: A })
        .mockResolvedValueOnce({ visibility: "public", currentVisibility: "public", tightened: [], leaderOpened: [], resetDates: [], consent: B });
      get.mockResolvedValue(readOf({ visibility: "public", leaderOpened: false, revision: "r3" }));
      const { onSaved, onConflict } = open();
      await userEvent.click(await screen.findByLabelText("Also reset those dates to the series setting"));
      await makePublic();
      await userEvent.click(await screen.findByRole("button", { name: "Make this event public" }));
      expect(await screen.findByText(offline)).toBeInTheDocument();
      await userEvent.click(screen.getByRole("button", { name: "Save visibility" }));
      await waitFor(() => expect(onConflict).toHaveBeenCalledExactlyOnceWith("changed"));
      expect(setVisibility.mock.calls.map(([, body]) => [body.visibility, body.consent, body.resetOverrides])).toEqual([["public", A, true], ["public", B, true]]);
      expect(onSaved).not.toHaveBeenCalled();
    });
  });
});
