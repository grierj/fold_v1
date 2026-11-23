import { describe, expect, it, vi } from "vitest";
import { createRef, useRef } from "react";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { GroupVisibilityReview } from "./GroupVisibilityReview";
import type { VisibilityImpact } from "../types";

const noFocus = () => createRef<HTMLElement>();

describe("GroupVisibilityReview", () => {
  const impact: VisibilityImpact = {
    visibility: "invite_only",
    currentVisibility: "public",
    tightened: [{ id: 1, title: "Soup night", visibility: "private", registrants: 2 }],
    leaderOpened: [{ id: 2, title: "Bake sale", visibility: "public", registrants: 1 }],
  };

  it("lists the events and waits for a choice on each opened one", () => {
    const onConfirm = vi.fn();
    render(<GroupVisibilityReview impact={impact} busy={false} onConfirm={onConfirm} onCancel={() => {}} returnFocus={noFocus()} />);
    expect(screen.getByText(/2 people registered will be cancelled and notified/)).toBeInTheDocument();
    const confirm = screen.getByRole("button", { name: "Make the group invite-only" });
    expect(confirm).toBeDisabled();
    fireEvent.click(screen.getByRole("radio", { name: /Make it invite-only/ }));
    expect(confirm).toBeEnabled();
    fireEvent.click(confirm);
    expect(onConfirm).toHaveBeenCalledWith({ "2": "tighten" });
  });

  it("needs no choice when only inherited events change", () => {
    const onConfirm = vi.fn();
    render(
      <GroupVisibilityReview impact={{ ...impact, leaderOpened: [] }} busy={false} onConfirm={onConfirm} onCancel={() => {}} returnFocus={noFocus()} />
    );
    fireEvent.click(screen.getByRole("button", { name: "Make the group invite-only" }));
    expect(onConfirm).toHaveBeenCalledWith({});
  });

  it.each([
    ["public", "invite_only", "The group is public now. Saving makes it invite-only, so fewer people will be able to see it."],
    ["private", "public", "The group is private now. Saving makes it public, so more people will be able to see it."],
    ["private", "private", "The group is private now and will stay private."],
  ] as const)("describes the group going from %s to %s", (current, next, sentence) => {
    render(<GroupVisibilityReview impact={{ ...impact, visibility: next, currentVisibility: current, leaderOpened: [] }} busy={false} onConfirm={() => {}} onCancel={() => {}} returnFocus={noFocus()} />);
    expect(screen.getByRole("dialog")).toHaveAccessibleDescription(sentence);
    expect(screen.queryByText(/reset/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/won't change/)).not.toBeInTheDocument();
  });

  it.each([
    ["public", "invite_only", "This event is public now. Saving makes it invite-only, so fewer people will be able to see it."],
    ["invite_only", "public", "This event is invite-only now. Saving makes it public, so more people will be able to see it."],
    ["invite_only", "invite_only", "This event is invite-only now and will stay invite-only."],
  ] as const)("describes the event going from %s to %s", (current, next, sentence) => {
    render(<GroupVisibilityReview subject="event" impact={{ visibility: next, currentVisibility: current, tightened: [], leaderOpened: [] }} busy={false} onConfirm={() => {}} onCancel={() => {}} returnFocus={noFocus()} />);
    expect(screen.getByRole("dialog")).toHaveAccessibleDescription(sentence);
  });

  it("gives two reviews on one page their own title and description ids", () => {
    render(<>
      <GroupVisibilityReview impact={impact} busy={false} onConfirm={() => {}} onCancel={() => {}} returnFocus={noFocus()} />
      <GroupVisibilityReview subject="event" impact={{ visibility: "private", currentVisibility: "public", tightened: [], leaderOpened: [] }} busy={false} onConfirm={() => {}} onCancel={() => {}} returnFocus={noFocus()} />
    </>);
    const [group, event] = screen.getAllByRole("dialog");
    expect(group.getAttribute("aria-labelledby")).not.toBe(event.getAttribute("aria-labelledby"));
    expect(group.getAttribute("aria-describedby")).not.toBe(event.getAttribute("aria-describedby"));
    expect(group).toHaveAccessibleName("Making the group invite-only changes some events");
    expect(event).toHaveAccessibleName("Making this event private");
  });

  it("hands focus back to the opener before it closes, and skips a disabled one", () => {
    function Host({ disabled, onCancel }: { disabled: boolean; onCancel: () => void }) {
      const save = useRef<HTMLButtonElement>(null);

      return (<>
        <button ref={save} disabled={disabled}>Save group</button>
        <GroupVisibilityReview impact={impact} busy={false} onConfirm={() => {}} onCancel={onCancel} returnFocus={save} />
      </>);
    }

    let focusedAtCancel: Element | null = null;
    const onCancel = vi.fn(() => { focusedAtCancel = document.activeElement; });
    const { unmount } = render(<Host disabled={false} onCancel={onCancel} />);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(focusedAtCancel).toBe(screen.getByRole("button", { name: "Save group" }));
    unmount();
    render(<Host disabled onCancel={onCancel} />);
    const cancel = screen.getByRole("button", { name: "Cancel" });
    cancel.focus();
    fireEvent.click(cancel);
    expect(focusedAtCancel).not.toBe(screen.getByRole("button", { name: "Save group" }));
  });

  it("waiting disables confirm without relabelling it, says why in a live region, and keeps focus and choices", () => {
    const onConfirm = vi.fn();
    const only = { ...impact, leaderOpened: [] };
    const { rerender } = render(<GroupVisibilityReview impact={only} busy={false} onConfirm={onConfirm} onCancel={() => {}} returnFocus={noFocus()} />);
    const heading = screen.getByRole("heading", { name: "Making the group invite-only changes some events" });
    expect(document.activeElement).toBe(heading);
    const live = screen.getByRole("dialog").querySelector("[aria-live='polite']");
    expect(live).toBeEmptyDOMElement();
    rerender(<GroupVisibilityReview impact={only} busy={false} waiting onConfirm={onConfirm} onCancel={() => {}} returnFocus={noFocus()} />);
    const confirm = screen.getByRole("button", { name: "Make the group invite-only" });
    expect(confirm).toBeDisabled();
    expect(live).toHaveTextContent("Saving waits for a sign-up update to finish.");
    expect(screen.getByRole("button", { name: "Cancel" })).toBeEnabled();
    expect(document.activeElement).toBe(heading);
    fireEvent.click(confirm);
    expect(onConfirm).not.toHaveBeenCalled();
    // Busy says Saving… and the waiting line gives way to it.
    rerender(<GroupVisibilityReview impact={only} busy waiting onConfirm={onConfirm} onCancel={() => {}} returnFocus={noFocus()} />);
    expect(screen.getByRole("button", { name: "Saving…" })).toBeDisabled();
    expect(live).toBeEmptyDOMElement();
    rerender(<GroupVisibilityReview impact={only} busy={false} onConfirm={onConfirm} onCancel={() => {}} returnFocus={noFocus()} />);
    expect(screen.getByRole("button", { name: "Make the group invite-only" })).toBeEnabled();
    expect(live).toBeEmptyDOMElement();
  });

  it("focuses its heading on mount, keeps it through a same-key render and moves it with a new key", () => {
    const one = { visibility: "private" as const, currentVisibility: "public" as const, tightened: [], leaderOpened: [] };
    const { rerender } = render(<GroupVisibilityReview key="a" subject="event" impact={one} busy={false} onConfirm={() => {}} onCancel={() => {}} returnFocus={noFocus()} />);
    expect(document.activeElement).toBe(screen.getByRole("heading", { name: "Making this event private" }));
    screen.getByRole("button", { name: "Cancel" }).focus();
    rerender(<GroupVisibilityReview key="a" subject="event" impact={one} busy onConfirm={() => {}} onCancel={() => {}} returnFocus={noFocus()} />);
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Cancel" }));
    const two = { ...one, visibility: "invite_only" as const };
    rerender(<GroupVisibilityReview key="b" subject="event" impact={two} busy={false} onConfirm={() => {}} onCancel={() => {}} returnFocus={noFocus()} />);
    expect(document.activeElement).toBe(screen.getByRole("heading", { name: "Making this event invite-only" }));
  });
});

describe("the event reset review", () => {
  const dates = [
    { occurrenceId: 51, date: "2026-12-01", title: "Supper", visibility: "invite_only" as const },
    { occurrenceId: 52, date: "2026-12-08", title: "", visibility: "public" as const },
  ];

  it("lists each date on its own lines, with no separator dots", () => {
    render(<GroupVisibilityReview subject="event" reset impact={{ visibility: "public", currentVisibility: "public", futureOverrides: 2, tightened: [], leaderOpened: [], resetDates: dates }} busy={false} onConfirm={() => {}} onCancel={() => {}} returnFocus={noFocus()} />);
    expect(screen.getByRole("heading", { name: "Reset this event's dates to the series setting" })).toBeInTheDocument();
    expect(screen.getByText("This resets 2 dates to the series setting (public):")).toBeInTheDocument();
    const region = screen.getByRole("region", { name: "Dates that will be reset" });
    const items = within(region).getAllByRole("listitem");
    expect(items).toHaveLength(2);
    expect(within(items[0]).getByText("Supper")).toBeInTheDocument();
    expect(within(items[0]).getByText("Tuesday, December 1. Now invite-only.")).toBeInTheDocument();
    expect(within(items[0]).getByText("After the reset, more people will be able to see this date.")).toBeInTheDocument();
    expect(within(items[1]).getByText("Untitled event")).toBeInTheDocument();
    expect(within(items[1]).queryByText(/After the reset/)).not.toBeInTheDocument();
    expect(region.textContent).not.toContain("·");
    expect(screen.getByRole("button", { name: "Reset 2 dates" })).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toHaveAccessibleDescription("This event is public now and will stay public.");
  });

  it("says one date in the singular", () => {
    render(<GroupVisibilityReview subject="event" reset impact={{ visibility: "public", currentVisibility: "public", tightened: [], leaderOpened: [], resetDates: dates.slice(0, 1) }} busy={false} onConfirm={() => {}} onCancel={() => {}} returnFocus={noFocus()} />);
    expect(screen.getByText("This resets 1 date to the series setting (public):")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reset 1 date" })).toBeInTheDocument();
  });

  it("takes reset-only from the server's current value, never the page", () => {
    render(<GroupVisibilityReview subject="event" reset impact={{ visibility: "public", currentVisibility: "invite_only", tightened: [], leaderOpened: [], resetDates: dates.slice(0, 1) }} busy={false} onConfirm={() => {}} onCancel={() => {}} returnFocus={noFocus()} />);
    expect(screen.getByRole("heading", { name: "Making this event public" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Make this event public" })).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toHaveAccessibleDescription("This event is invite-only now. Saving makes it public, so more people will be able to see it.");
  });

  it("names the kept value when nothing is left to reset", () => {
    render(<GroupVisibilityReview subject="event" reset impact={{ visibility: "invite_only", currentVisibility: "invite_only", tightened: [], leaderOpened: [], resetDates: [] }} busy={false} onConfirm={() => {}} onCancel={() => {}} returnFocus={noFocus()} />);
    const dialog = screen.getByRole("dialog", { name: "No dates to reset" });
    expect(dialog).toHaveAccessibleDescription("This event is invite-only now and will stay invite-only.");
    expect(screen.getByText("No future dates have their own visibility, so no date will be reset.")).toBeInTheDocument();
    expect(dialog.textContent).not.toContain("No dates have their own visibility");
    expect(within(dialog).getByRole("button", { name: "Keep this event invite-only" })).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Save visibility" })).not.toBeInTheDocument();
  });

  it("without reset, says the dates with their own visibility won't change", () => {
    render(<GroupVisibilityReview subject="event" impact={{ visibility: "private", currentVisibility: "public", futureOverrides: 1, tightened: [], leaderOpened: [] }} busy={false} onConfirm={() => {}} onCancel={() => {}} returnFocus={noFocus()} />);
    expect(screen.getByText("1 date has its own visibility and won't change.")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Dates that will be reset" })).not.toBeInTheDocument();
  });
});

it("keys date decisions separately from their series and formats the date", () => {
  const onConfirm = vi.fn();
  render(<GroupVisibilityReview impact={{ visibility: "invite_only", currentVisibility: "public", tightened: [], leaderOpened: [{ id: 7, title: "Welcome evening", visibility: "public", registrants: 0, date: "2026-12-01", occurrenceId: 51, decisionKey: "date:51" }] }} busy={false} onConfirm={onConfirm} onCancel={() => {}} returnFocus={noFocus()} />);
  expect(screen.getByText(/Tuesday, December 1/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("radio", { name: /^Keep it public/ }));
  fireEvent.click(screen.getByRole("button", { name: "Make the group invite-only" }));
  expect(onConfirm).toHaveBeenCalledWith({ "date:51": "keep" });
});
