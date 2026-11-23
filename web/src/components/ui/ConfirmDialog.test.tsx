import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, RouterProvider, createMemoryRouter } from "react-router-dom";
import { Suspense, lazy, startTransition, useState, type ComponentType } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Button } from "./Button";
import { ConfirmProvider, ConfirmShown, useConfirm, usePageFacts, type ConfirmRequest, type PendingAction } from "./ConfirmDialog";

const question = {
  title: "Delete “Choir night”?",
  body: "This removes every date and its registrations.",
  confirmLabel: "Delete event",
  tone: "danger",
  native: "Delete “Choir night”? This removes every date and its registrations.",
} satisfies ConfirmRequest;

function Asker({ authority, request = question, onAnswer }: { authority?: string; request?: ConfirmRequest; onAnswer: (confirmed: boolean) => void }) {
  const begin = useConfirm(authority);

  return <Button variant="danger-outline" onClick={() => void begin().ask(request).then(onAnswer)}>Delete</Button>;
}

const override = {
  title: "Add Lee over capacity?",
  body: "This date's in-person seats filled.",
  confirmLabel: "Add over capacity",
  tone: "normal",
  native: "This date's in-person seats filled. Add Lee over capacity?",
} satisfies ConfirmRequest;

// Starts an action on click and keeps it, so a test can change the facts before it asks or writes.
function Starter({ authority, onStart }: { authority: string; onStart: (action: PendingAction) => void }) {
  const begin = useConfirm(authority);

  return <Button variant="secondary" onClick={() => onStart(begin())}>Start</Button>;
}

function only(actions: PendingAction[]): PendingAction {
  const [started] = actions;

  if (actions.length !== 1 || !started) throw new Error(`Expected one action, got ${actions.length}.`);

  return started;
}

function inScope(asker: React.ReactNode) {
  return render(<MemoryRouter><ConfirmProvider>{asker}</ConfirmProvider></MemoryRouter>);
}

beforeEach(() => {
  // Inside a provider the browser's own prompt must never be used.
  vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("native confirm"); });
});

afterEach(() => vi.restoreAllMocks());

it("asks with the browser's prompt, word for word, where no route supplies the dialog", async () => {
  vi.mocked(window.confirm).mockReturnValue(false);
  const answer = vi.fn();
  render(<MemoryRouter><Asker onAnswer={answer} /></MemoryRouter>);
  await userEvent.click(screen.getByRole("button", { name: "Delete" }));
  expect(window.confirm).toHaveBeenCalledWith("Delete “Choir night”? This removes every date and its registrations.");
  await waitFor(() => expect(answer).toHaveBeenCalledWith(false));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("asks in a labelled dialog that starts on Cancel, and Escape answers no", async () => {
  const answer = vi.fn();
  inScope(<Asker onAnswer={answer} />);
  const opener = screen.getByRole("button", { name: "Delete" });
  await userEvent.click(opener);
  const dialog = screen.getByRole("dialog", { name: "Delete “Choir night”?" });
  expect(dialog).toHaveAccessibleDescription("This removes every date and its registrations.");
  expect(within(dialog).getByRole("button", { name: "Cancel" })).toHaveFocus();
  await userEvent.keyboard("{Escape}");
  await waitFor(() => expect(answer).toHaveBeenCalledWith(false));
  expect(answer).toHaveBeenCalledOnce();
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(opener).toHaveFocus();
  expect(window.confirm).not.toHaveBeenCalled();
});

it("answers yes exactly once, however often it is activated", async () => {
  const answer = vi.fn();
  inScope(<Asker onAnswer={answer} />);
  await userEvent.click(screen.getByRole("button", { name: "Delete" }));
  const yes = within(screen.getByRole("dialog")).getByRole("button", { name: "Delete event" });
  act(() => { yes.click(); yes.click(); });
  await waitFor(() => expect(answer).toHaveBeenCalledWith(true));
  expect(answer).toHaveBeenCalledOnce();
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("keeps one question open: a second ask while it is open answers no", async () => {
  const first = vi.fn();
  const second = vi.fn();
  inScope(<><Asker onAnswer={first} /><Asker onAnswer={second} /></>);
  const [one, two] = screen.getAllByRole("button", { name: "Delete" });
  act(() => { one.click(); two.click(); });
  await waitFor(() => expect(second).toHaveBeenCalledWith(false));
  expect(screen.getAllByRole("dialog")).toHaveLength(1);
  await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete event" }));
  await waitFor(() => expect(first).toHaveBeenCalledWith(true));
});

it("answers no and closes when the asking component goes away", async () => {
  const answer = vi.fn();
  const view = inScope(<Asker onAnswer={answer} />);
  await userEvent.click(screen.getByRole("button", { name: "Delete" }));
  view.rerender(<MemoryRouter><ConfirmProvider>{null}</ConfirmProvider></MemoryRouter>);
  await waitFor(() => expect(answer).toHaveBeenCalledWith(false));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("answers no and closes when the facts it was asked under change", async () => {
  const answer = vi.fn();
  const view = inScope(<Asker authority="manager" onAnswer={answer} />);
  await userEvent.click(screen.getByRole("button", { name: "Delete" }));
  const yes = within(screen.getByRole("dialog")).getByRole("button", { name: "Delete event" });
  view.rerender(<MemoryRouter><ConfirmProvider><Asker authority="member" onAnswer={answer} /></ConfirmProvider></MemoryRouter>);
  await waitFor(() => expect(answer).toHaveBeenCalledWith(false));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(yes).not.toBeInTheDocument();
});

it("answers no when the page moves to another address or the provider goes away", async () => {
  const answer = vi.fn();
  const router = createMemoryRouter([{ path: "/", element: <ConfirmProvider><Asker onAnswer={answer} /></ConfirmProvider> }]);
  const view = render(<RouterProvider router={router} />);
  await userEvent.click(screen.getByRole("button", { name: "Delete" }));
  await act(() => router.navigate("/?manage=2026-12-01"));
  await waitFor(() => expect(answer).toHaveBeenCalledWith(false));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

  await userEvent.click(screen.getByRole("button", { name: "Delete" }));
  expect(screen.getByRole("dialog")).toBeInTheDocument();
  view.unmount();
  await waitFor(() => expect(answer).toHaveBeenCalledTimes(2));
  expect(answer).toHaveBeenLastCalledWith(false);
});

it("starts an ordinary override on its own answer (DEC-63), and Escape still answers no", async () => {
  const answer = vi.fn();
  inScope(<Asker request={override} onAnswer={answer} />);
  await userEvent.click(screen.getByRole("button", { name: "Delete" }));
  const yes = within(screen.getByRole("dialog", { name: "Add Lee over capacity?" })).getByRole("button", { name: "Add over capacity" });
  expect(yes).toHaveFocus();
  expect(yes).toHaveClass("btn-primary");
  await userEvent.keyboard("{Escape}");
  await waitFor(() => expect(answer).toHaveBeenCalledWith(false));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("styles a destructive yes as solid danger and an ordinary one as primary", async () => {
  inScope(<Asker onAnswer={vi.fn()} />);
  await userEvent.click(screen.getByRole("button", { name: "Delete" }));
  const asked = screen.getByRole("dialog");
  expect(within(asked).getByRole("button", { name: "Delete event" })).toHaveClass("btn-danger");
  expect(within(asked).getByRole("button", { name: "Cancel" })).toHaveClass("btn-secondary");
});

it("asks nothing for an action whose facts changed before it asked, and stops it writing", async () => {
  const actions: PendingAction[] = [];
  const view = inScope(<Starter authority="full" onStart={(started) => actions.push(started)} />);
  await userEvent.click(screen.getByRole("button", { name: "Start" }));
  const started = only(actions);
  expect(started.current()).toBe(true);
  view.rerender(<MemoryRouter><ConfirmProvider><Starter authority="seat free" onStart={vi.fn()} /></ConfirmProvider></MemoryRouter>);
  expect(started.current()).toBe(false);
  // Still the page it started on: what follows a write that already happened may go on.
  expect(started.here()).toBe(true);
  await expect(started.ask(override)).resolves.toBe(false);
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("stops an action whose component went away while its request was on its way", async () => {
  const actions: PendingAction[] = [];
  const view = inScope(<Starter authority="full" onStart={(started) => actions.push(started)} />);
  await userEvent.click(screen.getByRole("button", { name: "Start" }));
  const started = only(actions);
  view.rerender(<MemoryRouter><ConfirmProvider>{null}</ConfirmProvider></MemoryRouter>);
  expect(started.current()).toBe(false);
  expect(started.here()).toBe(false);
  await expect(started.ask(override)).resolves.toBe(false);
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("never goes stale where the browser's prompt asks, exactly as before", async () => {
  vi.mocked(window.confirm).mockReturnValue(true);
  const actions: PendingAction[] = [];
  const view = render(<MemoryRouter><Starter authority="full" onStart={(started) => actions.push(started)} /></MemoryRouter>);
  await userEvent.click(screen.getByRole("button", { name: "Start" }));
  const started = only(actions);
  view.rerender(<MemoryRouter><Starter authority="seat free" onStart={vi.fn()} /></MemoryRouter>);
  expect(started.current()).toBe(true);
  await expect(started.ask(override)).resolves.toBe(true);
  expect(window.confirm).toHaveBeenCalledWith("This date's in-person seats filled. Add Lee over capacity?");
});

it("gives focus back to the control that started the action, after it is enabled again", async () => {
  function Busy() {
    const begin = useConfirm();
    const [busy, setBusy] = useState(false);

    return <Button variant="secondary" disabled={busy} onClick={() => {
      const action = begin();
      setBusy(true);
      // The question comes after a refusal: the button is disabled the whole time it is open.
      void Promise.resolve().then(() => action.ask(override)).then(() => setBusy(false));
    }}>Add back Lee</Button>;
  }

  inScope(<Busy />);
  const opener = screen.getByRole("button", { name: "Add back Lee" });
  await userEvent.click(opener);
  const asked = await screen.findByRole("dialog", { name: "Add Lee over capacity?" });
  expect(opener).toBeDisabled();
  await userEvent.click(within(asked).getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(opener).toBeEnabled());
  await waitFor(() => expect(opener).toHaveFocus());
});

it("stays stale after the address goes away and comes back to the same history entry", async () => {
  const actions: PendingAction[] = [];
  const router = createMemoryRouter([{ path: "/events/:id", element: <ConfirmProvider><Starter authority="full" onStart={(started) => actions.push(started)} /></ConfirmProvider> }], { initialEntries: ["/events/7"] });
  render(<RouterProvider router={router} />);
  const key = router.state.location.key;
  await userEvent.click(screen.getByRole("button", { name: "Start" }));
  const started = only(actions);
  await act(() => router.navigate("/events/7?manage=2026-12-01"));
  await act(() => router.navigate(-1));
  expect(router.state.location.key).toBe(key);
  // The same page, the same entry and the same facts as when it started: a round trip still ended it.
  expect(started.current()).toBe(false);
  expect(started.here()).toBe(false);
  await expect(started.ask(override)).resolves.toBe(false);
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("stays stale after its own facts change and change back", async () => {
  const actions: PendingAction[] = [];
  const at = (authority: string) => <MemoryRouter><ConfirmProvider><Starter authority={authority} onStart={(started) => actions.push(started)} /></ConfirmProvider></MemoryRouter>;
  const view = render(at("form open"));
  await userEvent.click(screen.getByRole("button", { name: "Start" }));
  const started = only(actions);
  view.rerender(at("form closed"));
  view.rerender(at("form open"));
  expect(started.current()).toBe(false);
  await expect(started.ask(override)).resolves.toBe(false);
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

function FactsPage({ facts, onStart, onAnswer }: { facts: string; onStart: (action: PendingAction) => void; onAnswer: (confirmed: boolean) => void }) {
  usePageFacts(facts);
  const begin = useConfirm();
  const beginLeave = useConfirm(undefined, { followsPage: false });

  return <>
    <Button variant="secondary" onClick={() => onStart(begin())}>Start</Button>
    <Button variant="secondary" onClick={() => void begin().ask(override).then(onAnswer)}>Ask</Button>
    <Button variant="secondary" onClick={() => onStart(beginLeave())}>Start leaving</Button>
  </>;
}

it("makes every action started under the page's facts stale when they change, even back to what they were", async () => {
  const actions: PendingAction[] = [];
  const answer = vi.fn();
  const at = (facts: string) => <MemoryRouter><ConfirmProvider><FactsPage facts={facts} onStart={(started) => actions.push(started)} onAnswer={answer} /></ConfirmProvider></MemoryRouter>;
  const view = render(at("dates 1,2,3"));
  await userEvent.click(screen.getByRole("button", { name: "Start" }));
  await userEvent.click(screen.getByRole("button", { name: "Start leaving" }));
  const [started, leaving] = actions;
  view.rerender(at("dates 1,2,3,4"));
  view.rerender(at("dates 1,2,3"));
  expect(started.current()).toBe(false);
  // Still the page it started on, and a question about the draft rather than the data stays current.
  expect(started.here()).toBe(true);
  expect(leaving.current()).toBe(true);

  // A question open when the page's facts change closes as no.
  await userEvent.click(screen.getByRole("button", { name: "Ask" }));
  expect(screen.getByRole("dialog", { name: "Add Lee over capacity?" })).toBeInTheDocument();
  view.rerender(at("dates 1,2,3,4"));
  await waitFor(() => expect(answer).toHaveBeenCalledWith(false));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("leaves an action current when the page reloads the same facts", async () => {
  const actions: PendingAction[] = [];
  const at = (facts: string) => <MemoryRouter><ConfirmProvider><FactsPage facts={facts} onStart={(started) => actions.push(started)} onAnswer={vi.fn()} /></ConfirmProvider></MemoryRouter>;
  const view = render(at("dates 1,2,3"));
  await userEvent.click(screen.getByRole("button", { name: "Start" }));
  view.rerender(at(`dates ${[1, 2, 3].join(",")}`));
  expect(only(actions).current()).toBe(true);
});

// The page's facts and this component's own facts move from A to B in a transition whose new content suspends,
// so React renders B and keeps showing A until the content arrives.
function TransitionPage({ Held, onStart, onStartOwn, onAnswer }: { Held: ComponentType; onStart: (action: PendingAction) => void; onStartOwn: (action: PendingAction) => void; onAnswer: (confirmed: boolean) => void }) {
  const [facts, setFacts] = useState("A");
  usePageFacts(facts);
  const begin = useConfirm();
  const beginOwn = useConfirm(facts, { followsPage: false });

  return <>
    <Button variant="secondary" onClick={() => onStart(begin())}>Start</Button>
    <Button variant="secondary" onClick={() => onStartOwn(beginOwn())}>Start own</Button>
    <Button variant="secondary" onClick={() => void begin().ask(override).then(onAnswer)}>Ask</Button>
    <Button variant="secondary" onClick={() => startTransition(() => setFacts("B"))}>Show B</Button>
    <Suspense fallback={<p>Loading</p>}>{facts === "A" ? <p>Showing A</p> : <Held />}</Suspense>
  </>;
}

it("keeps an action started from the tree still showing under the facts it shows, and stales it when new facts commit", async () => {
  let arrive!: () => void;
  const content = new Promise<{ default: ComponentType }>((resolve) => { arrive = () => resolve({ default: () => <p>Showing B</p> }); });
  const load = vi.fn(() => content);
  const Held = lazy(load);
  const actions: PendingAction[] = [];
  const own: PendingAction[] = [];
  const answer = vi.fn();
  inScope(<TransitionPage Held={Held} onStart={(action) => actions.push(action)} onStartOwn={(action) => own.push(action)} onAnswer={answer} />);

  await userEvent.click(screen.getByRole("button", { name: "Show B" }));
  // B has rendered (its content was asked for) and is held: the person still sees A.
  expect(load).toHaveBeenCalledOnce();
  expect(screen.getByText("Showing A")).toBeInTheDocument();
  expect(screen.queryByText("Showing B")).not.toBeInTheDocument();

  await userEvent.click(screen.getByRole("button", { name: "Start" }));
  await userEvent.click(screen.getByRole("button", { name: "Start own" }));
  await userEvent.click(screen.getByRole("button", { name: "Ask" }));
  expect(screen.getByRole("dialog", { name: "Add Lee over capacity?" })).toBeInTheDocument();
  expect(only(actions).current()).toBe(true);
  expect(only(own).current()).toBe(true);

  await act(async () => arrive());
  expect(await screen.findByText("Showing B")).toBeInTheDocument();
  expect(only(actions).current()).toBe(false);
  expect(only(actions).here()).toBe(true);
  expect(only(own).current()).toBe(false);
  await waitFor(() => expect(answer).toHaveBeenCalledWith(false));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  await expect(only(actions).ask(override)).resolves.toBe(false);
});

it("stales an action when the list it is in hides it, and keeps it stale when it is shown again", async () => {
  const actions: PendingAction[] = [];
  const answer = vi.fn();

  const at = (shown: boolean) => <MemoryRouter><ConfirmProvider><ConfirmShown shown={shown}>
    <Starter authority="form open" onStart={(started) => actions.push(started)} />
    <Asker onAnswer={answer} />
  </ConfirmShown></ConfirmProvider></MemoryRouter>;

  const view = render(at(true));
  await userEvent.click(screen.getByRole("button", { name: "Start" }));
  view.rerender(at(false));
  view.rerender(at(true));
  expect(only(actions).current()).toBe(false);
  await expect(only(actions).ask(override)).resolves.toBe(false);
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

  // An open question closes as no when its item is hidden.
  await userEvent.click(screen.getByRole("button", { name: "Delete" }));
  expect(screen.getByRole("dialog", { name: "Delete “Choir night”?" })).toBeInTheDocument();
  view.rerender(at(false));
  await waitFor(() => expect(answer).toHaveBeenCalledWith(false));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("never makes current an action started while its list hides it, and tells each watcher once when an action goes stale", async () => {
  const actions: PendingAction[] = [];

  const at = (shown: boolean) => <MemoryRouter><ConfirmProvider><ConfirmShown shown={shown}>
    <Starter authority="form open" onStart={(started) => actions.push(started)} />
  </ConfirmShown></ConfirmProvider></MemoryRouter>;

  // A hidden item's control can still be reached by a picker it opened in a portal, or by a script.
  const view = render(at(false));
  act(() => { screen.getByRole("button", { name: "Start" }).click(); });
  const hidden = only(actions);
  expect(hidden.current()).toBe(false);
  await expect(hidden.ask(override)).resolves.toBe(false);
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  const told = vi.fn();
  hidden.whenStale(told);
  expect(told).toHaveBeenCalledOnce();

  view.rerender(at(true));
  await userEvent.click(screen.getByRole("button", { name: "Start" }));
  const visible = actions[1];
  expect(visible.current()).toBe(true);
  const stale = vi.fn();
  const stopped = vi.fn();
  visible.whenStale(stale);
  const stop = visible.whenStale(stopped);
  stop();
  expect(stale).not.toHaveBeenCalled();

  view.rerender(at(false));
  expect(stale).toHaveBeenCalledOnce();
  view.rerender(at(true));
  view.rerender(at(false));
  expect(stale).toHaveBeenCalledOnce();
  expect(stopped).not.toHaveBeenCalled();
});

it("tells only the watchers still watching when an action goes stale, even when one stops or starts another", async () => {
  const actions: PendingAction[] = [];

  const at = (shown: boolean) => <MemoryRouter><ConfirmProvider><ConfirmShown shown={shown}>
    <Starter authority="form open" onStart={(started) => actions.push(started)} />
  </ConfirmShown></ConfirmProvider></MemoryRouter>;

  const view = render(at(true));
  await userEvent.click(screen.getByRole("button", { name: "Start" }));
  const action = only(actions);
  const stoppedBySibling = vi.fn();
  const startedBySibling = vi.fn();
  let stopSibling = () => {};

  // The first watcher stops the second and starts a third while the provider is telling them.
  const first = vi.fn(() => {
    stopSibling();
    action.whenStale(startedBySibling);
  });

  action.whenStale(first);
  stopSibling = action.whenStale(stoppedBySibling);
  view.rerender(at(false));
  expect(first).toHaveBeenCalledOnce();
  expect(stoppedBySibling).not.toHaveBeenCalled();
  expect(startedBySibling).toHaveBeenCalledOnce();

  view.rerender(at(true));
  view.rerender(at(false));
  expect(first).toHaveBeenCalledOnce();
  expect(startedBySibling).toHaveBeenCalledOnce();
});
