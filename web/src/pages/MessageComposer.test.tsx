import { requiredElement } from "../test/dom";
import { testSite } from "../test/fixtures";
import { renderWithAuth } from "../test/auth";
import { stubService } from "../test/service";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RouterProvider, createMemoryRouter } from "react-router-dom";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../api/core";
import { Site } from "../api/site";
import { Messages } from "../api/messages";
import { UnsavedChangesBlocker } from "../lib/unsavedChanges";
import { ERROR_COPY } from "../lib/errors";
import { polyfillEditorLayout } from "../test/editor";
import { adminRecipients, composeOptions, message, recipients, reminderMessage, summary } from "../test/compose";
import { wallClock, wallToInstant } from "../lib/when";
import type { AudienceChange, MessageReview } from "../types";
import { MessageComposerPage } from "./MessageComposer";

// Two days ahead on the church's clock, within the year the API allows.
const soonDay = () => wallClock(new Date(Date.now() + 2 * 864e5).toISOString(), "America/Boise").day;

const viewer = vi.hoisted(() => ({ isAdmin: false }));

const render = renderWithAuth(() => ({ user: { id: 2, name: "Lee Leader", displayName: "", email: "lee@example.com" }, loading: false, isAdmin: viewer.isAdmin, canCompose: true }));

stubService(Site, () => ({ get: vi.fn().mockResolvedValue({ name: "Grace", timezone: "America/Boise" }) }));

stubService(Messages, () => ({ options: vi.fn(), audiencePreview: vi.fn(), get: vi.fn(), create: vi.fn(), update: vi.fn(), review: vi.fn(), schedule: vi.fn(), cancel: vi.fn(), preview: vi.fn(), test: vi.fn(), recipients: vi.fn(), retry: vi.fn(), uploadImage: vi.fn() }));

beforeAll(polyfillEditorLayout);

function open(path: string) {
  const router = createMemoryRouter(
    [
      { path: "/messages/new", element: <><UnsavedChangesBlocker /><MessageComposerPage /></> },
      { path: "/messages/:id", element: <><UnsavedChangesBlocker /><MessageComposerPage /></> },
      { path: "/messages", element: <p>Messages list</p> },
    ],
    { initialEntries: [path] },
  );

  render(<RouterProvider router={router} />);

  return router;
}

beforeEach(() => {
  vi.mocked(Messages.options).mockResolvedValue(composeOptions());
  vi.mocked(Messages.audiencePreview).mockResolvedValue({ summary: summary() });
  vi.mocked(Messages.get).mockResolvedValue({ message: message() });
  let saves = 0;
  vi.mocked(Messages.update).mockImplementation(async (id, d) => ({ message: message({ id: Number(id), subject: d.subject, bodyHtml: d.bodyHtml, audience: d.audience, pushAlert: d.pushAlert, revision: `1.r${++saves}` }) }));
  vi.mocked(Messages.create).mockImplementation(async (d) => ({ message: message({ id: 9, subject: d.subject, bodyHtml: d.bodyHtml, audience: d.audience, pushAlert: d.pushAlert, revision: `1.r${++saves}` }) }));
  vi.mocked(Messages.review).mockResolvedValue({ review: { summary: summary(), reached: 2, token: "review-1" } });
  vi.mocked(Messages.preview).mockResolvedValue({ preview: { subject: "Bring a friend", text: "Hi Lee\n\nYou're getting messages from leaders" } });
  vi.mocked(Messages.test).mockResolvedValue({ ok: true });
  vi.mocked(Messages.recipients).mockResolvedValue(recipients());
});

afterEach(() => { vi.clearAllMocks(); viewer.isAdmin = false; });

const editorReady = () => screen.findByRole("textbox", { name: "Message" });

describe("MessageComposerPage", () => {
  it("starts from the group it was opened for, with a live count", async () => {
    open("/messages/new?group=7");
    expect(await screen.findByRole("heading", { name: "New message", level: 1 })).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Send to" })).toHaveValue("group:7");
    expect(await screen.findByText("3 people; 1 won't get it: 1 turned off messages from leaders")).toBeInTheDocument();
    expect(Messages.audiencePreview).toHaveBeenCalledWith({ type: "group", groupId: 7, roles: ["member", "leader"] });
    // One primary action.
    await editorReady();
    expect(screen.getByRole("button", { name: "Send now" })).toHaveClass("btn-primary");
    expect(document.querySelectorAll(".btn-primary")).toHaveLength(1);
  });

  it("says what is missing under each field and focuses the first", async () => {
    const user = userEvent.setup();
    open("/messages/new");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    const to = screen.getByRole("combobox", { name: "Send to" });
    expect(to).toHaveAccessibleDescription(expect.stringContaining(ERROR_COPY.message_audience));
    expect(to).toHaveFocus();
    expect(screen.getByRole("textbox", { name: "Subject" })).toHaveAccessibleDescription(expect.stringContaining("Add a subject."));
    expect(screen.getByRole("textbox", { name: "Message" })).toHaveAccessibleDescription(expect.stringContaining("Write your message."));
    expect(Messages.create).not.toHaveBeenCalled();
  });

  it("saves a new draft and moves to its address without reloading it", async () => {
    const user = userEvent.setup();
    const router = open("/messages/new?event=3");
    await editorReady();
    await user.type(screen.getByRole("textbox", { name: "Subject" }), "Snacks");
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    expect(await screen.findByText("Draft saved.")).toBeInTheDocument();
    expect(Messages.create).toHaveBeenCalledWith({ subject: "Snacks", bodyHtml: "", audience: { type: "event_series", eventId: 3, statuses: ["going", "waitlisted"] }, pushAlert: true });
    await waitFor(() => expect(router.state.location.pathname).toBe("/messages/9"));
    expect(Messages.get).not.toHaveBeenCalled();
    expect(screen.getByRole("textbox", { name: "Subject" })).toHaveValue("Snacks");
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    // The next save names the read the first one left.
    await waitFor(() => expect(Messages.update).toHaveBeenCalledWith(9, expect.objectContaining({ subject: "Snacks" }), "1.r1", undefined));
  });

  it("adds a picture to a new message by saving the draft first", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.uploadImage).mockResolvedValue({ key: "messages/9/aa.png", url: "/uploads/messages/9/aa.png" });
    const router = open("/messages/new?group=7");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Add image" }));
    await user.upload(document.querySelector<HTMLInputElement>('input[type="file"]')!, new File(["png"], "picnic.png", { type: "image/png" }));
    await user.type(screen.getByRole("textbox", { name: "What the picture shows" }), "Our picnic table");
    await user.click(screen.getByRole("button", { name: "Add picture" }));
    await waitFor(() => expect(Messages.uploadImage).toHaveBeenCalledWith(9, expect.any(File)));
    expect(Messages.create).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(router.state.location.pathname).toBe("/messages/9"));
    await waitFor(() => expect(document.querySelector('.rich-text-editable img[src="/uploads/messages/9/aa.png"]')).toHaveAttribute("alt", "Our picnic table"));
    // The next one goes straight to the saved message.
    await user.click(screen.getByRole("button", { name: "Add image" }));
    await user.upload(document.querySelector<HTMLInputElement>('input[type="file"]')!, new File(["png"], "two.png", { type: "image/png" }));
    await user.type(screen.getByRole("textbox", { name: "What the picture shows" }), "Chairs");
    await user.click(screen.getByRole("button", { name: "Add picture" }));
    await waitFor(() => expect(Messages.uploadImage).toHaveBeenCalledTimes(2));
    expect(Messages.create).toHaveBeenCalledTimes(1);
  });

  it("needs someone to send to before it can save a draft for a picture", async () => {
    const user = userEvent.setup();
    open("/messages/new");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Add image" }));
    await user.upload(document.querySelector<HTMLInputElement>('input[type="file"]')!, new File(["png"], "picnic.png", { type: "image/png" }));
    await user.type(screen.getByRole("textbox", { name: "What the picture shows" }), "Table");
    await user.click(screen.getByRole("button", { name: "Add picture" }));
    expect(await screen.findByText("Choose who this message is for first. A picture is added to a saved draft.")).toBeInTheDocument();
    expect(Messages.create).not.toHaveBeenCalled();
    expect(Messages.uploadImage).not.toHaveBeenCalled();
  });

  it("previews the saved email in a sandboxed frame, with the plain text", async () => {
    const user = userEvent.setup();
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Preview" }));
    const frame = await screen.findByTitle("Email preview");
    expect(Messages.update).toHaveBeenCalled();
    expect(frame).toHaveAttribute("src", "/api/v1/messages/5/preview.html");
    expect(frame).toHaveAttribute("sandbox", "allow-popups allow-popups-to-escape-sandbox");
    expect(screen.getByText("Plain-text version")).toBeInTheDocument();
    expect(screen.getByText(/You're getting messages from leaders/)).toBeInTheDocument();
  });

  it("sends a test to the author and says where", async () => {
    const user = userEvent.setup();
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send a test to me" }));
    expect(await screen.findByText("Sent to you at lee@example.com.")).toBeInTheDocument();
    expect(Messages.test).toHaveBeenCalledWith(5);
  });

  it("explains a test send refused by the notification settings", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.test).mockRejectedValue(new ApiError(400, "this kind of email is turned off in your notification settings", { code: "message_test_opted_out" }));
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send a test to me" }));
    const error = await screen.findByText(ERROR_COPY.message_test_opted_out);
    expect(within(screen.getByRole("region", { name: "What it says" })).getByText(ERROR_COPY.message_test_opted_out)).toBe(error);
    expect(screen.queryByText(/Sent to you/)).not.toBeInTheDocument();
    expect(Messages.test).toHaveBeenCalledWith(5);
  });

  it("shows the API's error in words, never its text", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.test).mockRejectedValue(new ApiError(400, "you've sent 10 tests in the last hour", { code: "message_test_limit" }));
    vi.mocked(Messages.update).mockRejectedValueOnce(new ApiError(400, "event merge fields only work…", { code: "message_merge_event" }));
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    expect(await screen.findByText(ERROR_COPY.message_merge_event)).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "Message" })).toHaveAttribute("aria-invalid", "true");
    await user.click(screen.getByRole("button", { name: "Send a test to me" }));
    expect(await screen.findByText(ERROR_COPY.message_test_limit)).toBeInTheDocument();
    expect(screen.queryByText(/you've sent 10 tests in the last hour$/)).toBeNull();
  });

  it("schedules for a picked time after confirming the count", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.schedule).mockImplementation(async (id, send) => ({ message: message({ id: Number(id), status: "scheduled", scheduledAt: send.scheduledAt ?? null, revision: "1.sched" }) }));
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("radio", { name: "Pick a date and time" }));
    expect(screen.getByText(/Times are in Mountain time/)).toBeInTheDocument();
    const day = soonDay();
    fireEvent.change(screen.getByLabelText("Date"), { target: { value: day } });
    fireEvent.change(screen.getByLabelText("Time"), { target: { value: "09:30" } });
    await user.click(screen.getByRole("button", { name: "Schedule" }));
    const ask = await screen.findByRole("group", { name: /^Send to 2 people on / });
    expect(Messages.schedule).not.toHaveBeenCalled();
    await user.click(within(ask).getByRole("button", { name: "Schedule" }));
    expect(Messages.schedule).toHaveBeenCalledWith(5, { revision: "1.r1", review: "review-1", scheduledAt: wallToInstant(day, "09:30", "America/Boise") });
    expect(await screen.findByText(/^Scheduled for /)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Edit" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel message" })).toHaveClass("btn-danger-outline");
  });

  it("asks before sending now, and Go back leaves it a draft", async () => {
    const user = userEvent.setup();
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    const ask = await screen.findByRole("group", { name: "Send to 2 people now?" });
    expect(within(ask).getByRole("button", { name: "Send now" })).toHaveFocus();
    await user.click(within(ask).getByRole("button", { name: "Go back" }));
    expect(screen.queryByRole("group", { name: "Send to 2 people now?" })).toBeNull();
    expect(Messages.schedule).not.toHaveBeenCalled();
  });

  it("refuses a time in the past before saving", async () => {
    const user = userEvent.setup();
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("radio", { name: "Pick a date and time" }));
    fireEvent.change(screen.getByLabelText("Date"), { target: { value: "2020-01-01" } });
    await user.click(screen.getByRole("button", { name: "Schedule" }));
    expect(screen.getByLabelText("Date")).toHaveAccessibleDescription(ERROR_COPY.message_when);
    expect(Messages.update).not.toHaveBeenCalled();
  });

  it("says no one would get it instead of asking", async () => {
    const user = userEvent.setup();
    open("/messages/5");
    await editorReady();
    vi.mocked(Messages.review).mockResolvedValue({ review: { summary: summary({ people: 1, eligible: 0, teens: 0, skipped: { "no email address": 1 } }), reached: 0, token: "review-1" } });
    await user.click(screen.getByRole("button", { name: "Send now" }));
    expect(await screen.findByText(ERROR_COPY.message_nobody)).toBeInTheDocument();
  });

  it("shows a scheduled message with Edit and Cancel message, and cancels it", async () => {
    const user = userEvent.setup();
    vi.spyOn(window, "confirm").mockReturnValue(true);
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "scheduled", scheduledAt: "2030-11-05T16:30:00Z" }) });
    vi.mocked(Messages.cancel).mockResolvedValue({ message: message({ status: "cancelled", scheduledAt: "2030-11-05T16:30:00Z", editable: false }) });
    open("/messages/5");
    expect(await screen.findByRole("heading", { name: "Bring a friend", level: 1 })).toBeInTheDocument();
    expect(screen.getByText(/^Scheduled for /)).toBeInTheDocument();
    expect(screen.getByText("Book club (Members, Leaders)")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit" }));
    expect(await editorReady()).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Pick a date and time" })).toBeChecked();
    expect(screen.getByLabelText("Date")).toHaveValue("2030-11-05");
    expect(screen.getByText("Your changes save when you preview or send a test. The message stays scheduled.")).toBeInTheDocument();
    const asked = vi.mocked(window.confirm).mock.calls.length;
    await user.click(screen.getByRole("button", { name: "Done editing" }));
    // Nothing changed, so nothing to ask about.
    expect(vi.mocked(window.confirm).mock.calls.length).toBe(asked);
    await user.click(screen.getByRole("button", { name: "Cancel message" }));
    expect(await screen.findByText("Cancelled")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  });

  it("withdraws the question when anything changes after it is asked", async () => {
    const user = userEvent.setup();
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    const ask = await screen.findByRole("group", { name: "Send to 2 people now?" });
    expect(scroll).toHaveBeenCalledWith({ block: "center" });
    await user.type(screen.getByRole("textbox", { name: "Subject" }), "!");
    expect(ask).not.toBeInTheDocument();
    expect(screen.queryByRole("group", { name: /^Send to / })).toBeNull();
    expect(screen.getByRole("button", { name: "Send now" })).toHaveAttribute("type", "submit");
    // The same for the audience and the when.
    await user.click(screen.getByRole("button", { name: "Send now" }));
    await screen.findByRole("group", { name: "Send to 2 people now?" });
    await user.click(screen.getByRole("button", { name: "Leaders" }));
    expect(screen.queryByRole("group", { name: /^Send to / })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    await screen.findByRole("group", { name: "Send to 2 people now?" });
    await user.click(screen.getByRole("radio", { name: "Pick a date and time" }));
    expect(screen.queryByRole("group", { name: /^Send to / })).toBeNull();
    expect(Messages.schedule).not.toHaveBeenCalled();
  });

  it("scrolls the preview into view when it opens", async () => {
    const user = userEvent.setup();
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Preview" }));
    await screen.findByTitle("Email preview");
    expect(scroll).toHaveBeenCalledWith({ block: "start" });
  });

  it("shows a message that started sending meanwhile read only", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.update).mockRejectedValueOnce(new ApiError(409, "this message is already sending or sent", { code: "message_locked" }));
    open("/messages/5");
    await editorReady();
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "sending", editable: false }) });
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    expect(await screen.findByText("Sending")).toBeInTheDocument();
    expect(screen.getByText(ERROR_COPY.message_locked)).toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "Subject" })).toBeNull();
    expect(Messages.get).toHaveBeenCalledTimes(2);
  });

  it("reads a message again when it changed under the send press, and says so", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.schedule).mockRejectedValueOnce(new ApiError(409, "this message changed somewhere else", { code: "message_changed" }));
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    const ask = await screen.findByRole("group", { name: "Send to 2 people now?" });
    vi.mocked(Messages.get).mockClear();
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ subject: "The office's words", revision: "1.other" }) });
    await user.click(within(ask).getByRole("button", { name: "Send now" }));
    // The form was saved before the send was asked about, so the line says only that it was not sent.
    const line = await screen.findByText("This message changed somewhere else, so it was not sent. This is the message as it is now.");
    expect(line).toHaveAttribute("role", "status");
    expect(line).toHaveFocus();
    expect(screen.getByRole("heading", { name: "The office's words", level: 1 })).toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "Subject" })).toBeNull();
    expect(Messages.get).toHaveBeenCalledWith(5);
    expect(Messages.schedule).toHaveBeenCalledTimes(1);
  });

  it("waits for the church's zone before filling a scheduled time", async () => {
    const user = userEvent.setup();
    let answer!: (value: Awaited<ReturnType<typeof Site.get>>) => void;
    const site = new Promise<Awaited<ReturnType<typeof Site.get>>>((r) => { answer = r; });
    vi.mocked(Site.get).mockReturnValue(site);
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "scheduled", scheduledAt: "2030-11-05T16:30:00Z" }) });
    open("/messages/5");
    await waitFor(() => expect(Messages.get).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    await act(async () => { answer({ name: "Grace", timezone: "Asia/Kolkata", selfSignup: "off", roomsEnabled: false, pushEnabled: false }); });
    await user.click(await screen.findByRole("button", { name: "Edit" }));
    await editorReady();
    expect(screen.getByLabelText("Date")).toHaveValue("2030-11-05");
    expect(screen.getByLabelText("Time")).toHaveValue("22:00");
    vi.mocked(Site.get).mockResolvedValue(testSite({ name: "Grace", timezone: "America/Boise" }));
  });

  it("discards a saved draft after asking", async () => {
    const user = userEvent.setup();
    const ask = vi.spyOn(window, "confirm").mockReturnValue(true);
    vi.mocked(Messages.cancel).mockResolvedValue({ message: message({ status: "cancelled", editable: false }) });
    const router = open("/messages/5");
    await editorReady();
    const discard = screen.getByRole("button", { name: "Discard draft" });
    expect(discard).toHaveClass("btn-danger-outline");
    await user.click(discard);
    expect(ask).toHaveBeenCalledWith("Discard this draft? You can't undo this.");
    expect(Messages.cancel).toHaveBeenCalledWith(5);
    await waitFor(() => expect(router.state.location.pathname).toBe("/messages"));
    ask.mockRestore();
  });

  it("offers no Discard draft before the first save", async () => {
    open("/messages/new?group=7");
    await editorReady();
    expect(screen.queryByRole("button", { name: "Discard draft" })).toBeNull();
  });

  it("guards edits made after the first save moves to the draft's address", async () => {
    const user = userEvent.setup();
    const ask = vi.spyOn(window, "confirm").mockReturnValue(false);
    const router = open("/messages/new?group=7");
    await editorReady();
    await user.type(screen.getByRole("textbox", { name: "Subject" }), "Snacks");
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/messages/9"));
    expect(ask).not.toHaveBeenCalled();
    await user.type(screen.getByRole("textbox", { name: "Subject" }), " now");
    await act(async () => { await router.navigate("/messages"); });
    expect(ask).toHaveBeenCalledWith("Leave without saving? Your changes will be lost.");
    expect(router.state.location.pathname).toBe("/messages/9");
    ask.mockRestore();
  });

  it("is read only once sent", async () => {
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "sent", sentAt: "2026-10-30T16:30:00Z", editable: false }) });
    open("/messages/5");
    expect(await screen.findByText(/^Sent (?!to )/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(await screen.findByText("{{first_name}}", { exact: false })).toBeInTheDocument();
  });

  it("shows an automatic reminder read only, with its date in place of an author (M3.6b)", async () => {
    const r = reminderMessage().reminder!;
    vi.mocked(Messages.get).mockResolvedValue({ message: reminderMessage({ reminder: { ...r, startsAt: "2030-11-06T02:00:00Z" }, displaySubject: "Reminder: Book night on Tue, Nov 5 · 7:00 PM MST" }) });
    open("/messages/9");
    // The heading has the event filled in, as the emails did; the body stays as written.
    expect(await screen.findByRole("heading", { level: 1, name: "Reminder: Book night on Tue, Nov 5 · 7:00 PM MST" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { level: 1, name: /\{\{/ })).toBeNull();
    const line = screen.getByText(/^Automatic reminder/);
    expect(line).toHaveTextContent("Automatic reminder for Book night, Tue, Nov 5 at 7:00 PM");
    expect(within(line).getByRole("link")).toHaveAttribute("href", "/events/3/dates/2026-11-05");
    expect(screen.getByText("Everyone going")).toBeInTheDocument();
    expect(screen.queryByText("Written by")).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Cancel message" })).toBeNull();
    expect(await screen.findByText("{{first_name}}", { exact: false })).toBeInTheDocument();
    // A leader came from the date page; it isn't on their Messages list.
    expect(screen.getByRole("link", { name: "‹ Back to Book night" })).toHaveAttribute("href", "/events/3/dates/2026-11-05");
  });

  it("takes an admin back to the automatic reminders list from a reminder", async () => {
    viewer.isAdmin = true;
    vi.mocked(Messages.get).mockResolvedValue({ message: reminderMessage() });
    open("/messages/9");
    expect(await screen.findByRole("link", { name: "‹ Back to automatic reminders" })).toHaveAttribute("href", "/messages?show=reminders");
  });

  it("tells a member the composer isn't theirs", async () => {
    vi.mocked(Messages.options).mockResolvedValue(composeOptions({ canCompose: false, groups: [], events: [] }));
    open("/messages/new");
    expect(await screen.findByRole("heading", { name: "Messages are for leaders" })).toBeInTheDocument();
  });

  it("says a message it can't find isn't here", async () => {
    vi.mocked(Messages.get).mockRejectedValue(new ApiError(404, "message not found", { code: "not_found" }));
    open("/messages/77");
    expect(await screen.findByRole("heading", { name: "This message isn't here" })).toBeInTheDocument();
  });
});

// After it goes out (M3.5b): the counts in words, who got it, a failed
// send's error with Try again, and polling while it sends.
describe("a sent message's history", () => {
  const sent = (over: Partial<Parameters<typeof message>[0]> = {}) =>
    message({ status: "sent", sentAt: "2026-10-30T16:30:00Z", editable: false, ...over });

  const counts = (over: Partial<ReturnType<typeof recipients>["counts"]>) => ({ ...recipients().counts, ...over });

  it("words the counts and lists who got it with Teen badges, guardian copies only as a count", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.get).mockResolvedValue({ message: sent() });
    vi.mocked(Messages.recipients).mockResolvedValue(recipients({ counts: counts({ bounced: 1 }) }));
    open("/messages/5");
    expect(await screen.findByText("Sent to 2 people · 1 bounced · 1 skipped (1 turned off messages from leaders)")).toBeInTheDocument();
    expect(screen.getByText("1 of the people it was for is a teen.")).toBeInTheDocument();
    expect(screen.getByText("1 guardian copy sent")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    await user.click(screen.getByText("See who got it"));
    const rows = within(screen.getByRole("list")).getAllByRole("listitem");
    expect(rows.map((r) => r.textContent)).toEqual([
      "Ann Adult· Sent",
      "Opal Opted· Didn't get it",
      "Tia TeenTeen· Sent",
    ]);
    expect(screen.queryByText(/Copy for/)).toBeNull();
  });

  it("shows an admin each guardian's copy, each person's reason and how many more there are", async () => {
    viewer.isAdmin = true;
    const user = userEvent.setup();
    vi.mocked(Messages.get).mockResolvedValue({ message: sent() });
    vi.mocked(Messages.recipients).mockResolvedValue(adminRecipients({ counts: counts({ sent: 250 }), more: true }));
    open("/messages/5");
    await user.click(await screen.findByText("See who got it"));
    const rows = within(screen.getByRole("list")).getAllByRole("listitem");
    expect(rows.map((r) => r.textContent)).toEqual([
      "Ann Adult· Sent",
      "Opal Opted· Didn't get it: turned off messages from leaders",
      "Pat ParentCopy for Tia Teen· Sent",
      "Tia TeenTeen· Sent",
      // 250 sent, 1 skipped and 1 copy, less the 4 shown.
      "And 248 more",
    ]);
  });

  it("shows a failed send's error under the title, and Try again asks before resending", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "failed", editable: false, failedCount: 4, sentAt: "2026-10-31T15:30:00Z", error: "The email service didn't take it. Try again in a little while. If it keeps happening, ask an admin to open this message to see what went wrong." }) });
    vi.mocked(Messages.recipients).mockResolvedValue(recipients({
      counts: counts({ sent: 0, failed: 4, skipped: 0, teens: 0, guardianCopies: 0 }), skippedBy: {}, canRetry: true,
      recipients: [{ name: "Ann Adult", teen: false, status: "missed" }],
    }));
    open("/messages/5");
    // The problem is announced; Try again sits outside the announced part.
    const alert = requiredElement((await screen.findByText("This message didn't go out")).closest("[role=alert]"), HTMLElement);
    expect(alert).toHaveTextContent("The email service didn't take it.");
    expect(within(alert).queryByRole("button")).toBeNull();
    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(await screen.findByText("No emails went out · 4 didn't get it")).toBeInTheDocument();
    const again = await screen.findByRole("button", { name: "Try again" });
    expect(again).toHaveClass("btn-primary");
    expect(document.querySelectorAll(".btn-primary")).toHaveLength(1);

    const ask = vi.spyOn(window, "confirm").mockReturnValueOnce(false).mockReturnValueOnce(true);
    await user.click(again);
    expect(ask).toHaveBeenLastCalledWith("Send again to the 4 people it didn't reach?");
    expect(Messages.retry).not.toHaveBeenCalled();
    vi.mocked(Messages.retry).mockResolvedValue({ message: message({ status: "sending", editable: false, sentAt: "2026-10-31T15:30:00Z", updatedAt: "2026-10-31T16:00:00Z" }) });
    vi.mocked(Messages.recipients).mockResolvedValue(recipients({ counts: counts({ sent: 0, queued: 4, skipped: 0 }), skippedBy: {} }));
    await user.click(again);
    expect(Messages.retry).toHaveBeenCalledWith(5);
    expect(await screen.findByText("Sending again… 0 of 4 so far")).toBeInTheDocument();
    // Checking again: nothing is announced until it settles again.
    expect(document.querySelector(".sr-only[role=status]")?.textContent).toBe("");
    expect(screen.queryByText("This message didn't go out")).toBeNull();
    ask.mockRestore();
  });

  it("shows the retry limit beside Try again", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "failed", editable: false, failedCount: 4 }) });
    vi.mocked(Messages.recipients).mockResolvedValue(recipients({ counts: counts({ sent: 0, failed: 4 }), canRetry: true }));
    vi.mocked(Messages.retry).mockRejectedValueOnce(new ApiError(400, "daily limit", { code: "message_retry_daily_limit" }));
    const confirm = vi.spyOn(window, "confirm").mockReturnValueOnce(true);
    open("/messages/5");
    await user.click(await screen.findByRole("button", { name: "Try again" }));
    const error = await screen.findByText(ERROR_COPY.message_retry_daily_limit);
    const button = screen.getByRole("button", { name: "Try again" });
    expect(button.parentElement).toContainElement(error);
    confirm.mockRestore();
  });

  it("counts a partly sent message's people and guardian copies apart, in the banner and the confirm", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.get).mockResolvedValue({ message: sent({ failedCount: 2, error: "The email service didn't take it." }) });
    vi.mocked(Messages.recipients).mockResolvedValue(recipients({ counts: counts({ failed: 1, guardianCopiesMissed: 1, guardianCopiesFailed: 1 }), canRetry: true }));
    open("/messages/5");
    // Some went out: a status, not an alert.
    expect((await screen.findByText("1 person didn't get this")).closest("[role=status]")).not.toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText("1 guardian copy didn't go out")).toBeInTheDocument();
    expect(screen.getByText(/^Partly sent /)).toBeInTheDocument();
    const ask = vi.spyOn(window, "confirm").mockReturnValue(false);
    await user.click(await screen.findByRole("button", { name: "Try again" }));
    expect(ask).toHaveBeenCalledWith("Send again to the 1 person and 1 guardian copy it didn't reach?");
    ask.mockRestore();
  });

  it("checks every 5 seconds while sending, and says once that it settled", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });

    try {
      vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "sending", editable: false }) });
      vi.mocked(Messages.recipients).mockResolvedValue(recipients({ counts: counts({ sent: 1, queued: 2, skipped: 0 }) }));
      open("/messages/5");
      expect(await screen.findByText("Sending… 1 of 3 so far")).toBeInTheDocument();
      const announced = () => document.querySelector(".sr-only[role=status]")?.textContent;
      vi.mocked(Messages.recipients).mockResolvedValue(recipients({ counts: counts({ sent: 2, queued: 1, skipped: 0 }) }));
      await act(() => vi.advanceTimersByTimeAsync(5000));
      expect(await screen.findByText("Sending… 2 of 3 so far")).toBeInTheDocument();
      expect(announced()).toBe("");
      vi.mocked(Messages.get).mockResolvedValue({ message: sent({ updatedAt: "2026-10-31T16:00:00Z" }) });
      vi.mocked(Messages.recipients).mockResolvedValue(recipients({ counts: counts({ skipped: 0 }) }));
      await act(() => vi.advanceTimersByTimeAsync(5000));
      await waitFor(() => expect(announced()).toBe("Sent to 2 people"));
      expect(screen.getAllByText("Sent to 2 people")).toHaveLength(2);
      const gets = vi.mocked(Messages.get).mock.calls.length;
      await act(() => vi.advanceTimersByTimeAsync(20000));
      expect(Messages.get).toHaveBeenCalledTimes(gets);
    } finally {
      vi.useRealTimers();
    }
  });

  it("waits while the tab is hidden and checks again when it comes back", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const hidden = vi.spyOn(document, "hidden", "get").mockReturnValue(true);

    try {
      vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "sending", editable: false }) });
      open("/messages/5");
      expect(await screen.findByText(/^Sending… /)).toBeInTheDocument();
      const gets = vi.mocked(Messages.get).mock.calls.length;
      await act(() => vi.advanceTimersByTimeAsync(30000));
      expect(Messages.get).toHaveBeenCalledTimes(gets);
      hidden.mockReturnValue(false);
      await act(async () => { document.dispatchEvent(new Event("visibilitychange")); });
      await waitFor(() => expect(Messages.get).toHaveBeenCalledTimes(gets + 1));
    } finally {
      hidden.mockRestore();
      vi.useRealTimers();
    }
  });

  it("starts checking when a scheduled message comes due with the page open", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });

    try {
      const at = new Date(Date.now() + 10 * 60_000).toISOString();
      vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "scheduled", scheduledAt: at, editable: true }) });
      open("/messages/5");
      expect(await screen.findByText(/^Scheduled for /)).toBeInTheDocument();
      const gets = vi.mocked(Messages.get).mock.calls.length;
      await act(() => vi.advanceTimersByTimeAsync(8 * 60_000));
      expect(Messages.get).toHaveBeenCalledTimes(gets);
      vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "sending", editable: false, scheduledAt: at, updatedAt: "2026-10-31T16:00:00Z" }) });
      await act(() => vi.advanceTimersByTimeAsync(2 * 60_000));
      // The page catches up once it is due, then checks 5 seconds later.
      await act(() => vi.advanceTimersByTimeAsync(5000));
      expect(await screen.findByText(/^Sending… /)).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it("stops after 30 minutes with nothing changing and offers to check again", async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    vi.useFakeTimers({ shouldAdvanceTime: true });

    try {
      vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "sending", editable: false }) });
      open("/messages/5");
      expect(await screen.findByText(/^Sending… /)).toBeInTheDocument();
      await act(() => vi.advanceTimersByTimeAsync(31 * 60_000));
      const refresh = await screen.findByRole("button", { name: "Refresh to check again" });
      const gets = vi.mocked(Messages.get).mock.calls.length;
      await act(() => vi.advanceTimersByTimeAsync(60_000));
      expect(Messages.get).toHaveBeenCalledTimes(gets);
      await user.click(refresh);
      await act(() => vi.advanceTimersByTimeAsync(5000));
      expect(Messages.get).toHaveBeenCalledTimes(gets + 1);
      expect(screen.queryByRole("button", { name: "Refresh to check again" })).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("stops checking when the page goes away", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });

    try {
      vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "sending", editable: false }) });
      const router = open("/messages/5");
      expect(await screen.findByText(/^Sending… /)).toBeInTheDocument();
      await act(() => router.navigate("/messages"));
      const gets = vi.mocked(Messages.get).mock.calls.length;
      await act(() => vi.advanceTimersByTimeAsync(20000));
      expect(Messages.get).toHaveBeenCalledTimes(gets);
    } finally {
      vi.useRealTimers();
    }
  });
});

// "Push notification too" (M3.13d2, DEC-123): on by default, hidden when
// the church turned notifications off, counted in the summary, and its
// pushes listed apart in the history.
describe("push notification too", () => {
  const pushSummary = summary({
    push: 2,
    recipients: [{ name: "Ann Adult", teen: false, push: true }, { name: "Opal Opted", teen: false, skipped: "won't get this", push: true }, { name: "Tia Teen", teen: true }],
  });

  it("is on by default, counts who also gets a push and marks them in words", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.audiencePreview).mockResolvedValue({ summary: pushSummary });
    open("/messages/new?group=7");
    const box = await screen.findByRole("checkbox", { name: /Push notification too/ });
    expect(box).toBeChecked();
    expect(box).toHaveAccessibleName(expect.stringContaining("the subject and the start of the message"));
    expect(await screen.findByText("2 also get a push notification.")).toBeInTheDocument();
    await user.click(screen.getByText("See who's on the list"));
    const rows = within(screen.getByRole("list")).getAllByRole("listitem");
    // The phone glyph is decorative; the words carry it (never the glyph alone).
    expect(rows[0]).toHaveTextContent("Ann AdultPush too");
    expect(rows[0].querySelector("svg")).toHaveAttribute("aria-hidden", "true");
    // Turned off email from leaders, push on: the push still goes (DEC-122).
    expect(rows[1]).toHaveTextContent("Opal OptedPush only· Won't get the email");
    expect(rows[2]).not.toHaveTextContent("Push");
    // Off: no count, no marks, and the draft says so.
    await user.click(box);
    expect(screen.queryByText("2 also get a push notification.")).toBeNull();
    expect(screen.queryByText(/^Push (too|only)$/)).toBeNull();
    expect(rows[1]).toHaveTextContent("Opal Opted· Won't get this");
    await editorReady();
    await user.type(screen.getByRole("textbox", { name: "Subject" }), "Snacks");
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    await waitFor(() => expect(Messages.create).toHaveBeenCalledWith(expect.objectContaining({ pushAlert: false })));
  });

  it("is hidden when the church turned notifications off, and keeps the saved value", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.options).mockResolvedValue(composeOptions({ pushEnabled: false }));
    vi.mocked(Messages.audiencePreview).mockResolvedValue({ summary: pushSummary });
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ pushAlert: false }) });
    open("/messages/5");
    await editorReady();
    expect(await screen.findByText(/^3 people;/)).toBeInTheDocument();
    expect(screen.queryByRole("checkbox", { name: /Push notification too/ })).toBeNull();
    expect(screen.queryByText(/also gets? a push notification/)).toBeNull();
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    await waitFor(() => expect(Messages.update).toHaveBeenCalledWith(5, expect.objectContaining({ pushAlert: false }), "1.r0", undefined));
  });

  it("says how many also get a push when it asks before sending", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.review).mockResolvedValue({ review: { summary: summary({ push: 2 }), reached: 2, token: "review-1" } });
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ subject: "Snacks", bodyHtml: "<p>Bring some</p>" }) });
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    expect(await screen.findByText("2 people get a push notification too.")).toBeInTheDocument();
  });

  it("asks about everyone the message reaches, someone who gets only the push included", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.review).mockResolvedValue({ review: { summary: summary({ eligible: 2, push: 3 }), reached: 3, token: "review-1" } });
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ subject: "Snacks", bodyHtml: "<p>Bring some</p>" }) });
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    expect(await screen.findByText("Send to 3 people now?")).toBeInTheDocument();
    expect(screen.getByText("3 of them get a push notification; 1 gets only the push.")).toBeInTheDocument();
  });

  it("shows each person's push on their own row, with a count apart from the email", async () => {
    viewer.isAdmin = true;
    const user = userEvent.setup();
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "sent", sentAt: "2026-10-30T16:30:00Z", editable: false }) });
    const base = adminRecipients();
    vi.mocked(Messages.recipients).mockResolvedValue({
      ...base,
      counts: { ...base.counts, sent: 3, skipped: 1, push: { sent: 2, waiting: [{ until: "8:00 AM", n: 1 }], missed: { "device gone": 1 } } },
      more: true,
      recipients: [
        { name: "Ann Adult", teen: false, status: "sent", push: { status: "sent" } },
        { name: "Bo Late", teen: false, status: "sent", push: { status: "waiting", until: "8:00 AM" } },
        { name: "Cy Gone", teen: false, status: "sent", push: { status: "missed", reason: "device gone" } },
        { name: "Opal Opted", teen: false, status: "missed", reason: "turned off messages from leaders", push: { status: "sent" } },
      ],
    });
    open("/messages/5");
    expect(await screen.findByText("Push notifications: 2 pushed · 1 waiting until 8:00 AM · 1 device gone")).toBeInTheDocument();
    // Pushes never change the email's count or offer Try again.
    expect(screen.getByText("Sent to 3 people · 1 skipped (1 turned off messages from leaders)")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    await user.click(screen.getByText("See who got it"));
    const rows = within(screen.getByRole("list")).getAllByRole("listitem");
    // One row a person: "And N more" counts people, not pushes.
    expect(rows.map((r) => r.textContent)).toEqual([
      "Ann Adult· SentPush sent",
      "Bo Late· SentPush waiting until 8:00 AM",
      "Cy Gone· SentPush didn't go out: device gone",
      "Opal Opted· Didn't get it: turned off messages from leaders" + "Push sent",
      "And 1 more",
    ]);
  });
});

// An edit and a send name what the sender saw (DEC-169): the save names the read it was made from, the send the text
// it was pressed on and the review its confirmation showed.
describe("the message names what was saved and reviewed", () => {
  const changed = (over: { review: MessageReview; change: AudienceChange }) => new ApiError(409, "who gets this message changed", { code: "message_list_changed", ...JSON.parse(JSON.stringify(over)) });

  it("names each save's read: a landed save moves it, a refusal with nothing changed goes again by itself", async () => {
    const user = userEvent.setup();
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    expect(await screen.findByText("Draft saved.")).toBeInTheDocument();
    expect(Messages.update).toHaveBeenLastCalledWith(5, expect.anything(), "1.r0", undefined);
    // Something the form does not write moved the token meanwhile: read once, nothing the form writes changed, so the
    // same save goes again under the new one.
    await user.type(screen.getByRole("textbox", { name: "Subject" }), "!");
    vi.mocked(Messages.update).mockRejectedValueOnce(new ApiError(409, "changed", { code: "message_changed" }));
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ revision: "1.other" }) });
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    await waitFor(() => expect(Messages.update).toHaveBeenLastCalledWith(5, expect.anything(), "1.other", undefined));
    expect(Messages.update).toHaveBeenNthCalledWith(2, 5, expect.anything(), "1.r1", undefined);
    expect(await screen.findByText("Draft saved.")).toBeInTheDocument();
    expect(screen.queryByText(/could not be saved/)).toBeNull();
  });

  it("closes the editor and says so when someone else changed the message", async () => {
    const user = userEvent.setup();
    open("/messages/5");
    await editorReady();
    await user.type(screen.getByRole("textbox", { name: "Subject" }), "!");
    vi.mocked(Messages.update).mockRejectedValueOnce(new ApiError(409, "changed", { code: "message_changed" }));
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ subject: "The office's words", revision: "1.other" }) });
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    const line = await screen.findByText("This message changed while you were editing, so your changes could not be saved. This is the message as it is now.");
    expect(line).toHaveAttribute("role", "status");
    expect(line).toHaveFocus();
    expect(screen.getByRole("heading", { name: "The office's words", level: 1 })).toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "Subject" })).toBeNull();
    expect(Messages.update).toHaveBeenCalledTimes(1);
    // Editing again starts from what is stored, and the line goes.
    await user.click(screen.getByRole("button", { name: "Edit" }));
    expect(await screen.findByRole("textbox", { name: "Subject" })).toHaveValue("The office's words");
    expect(screen.queryByText(/could not be saved/)).toBeNull();
  });

  it("ends a save whose answer was lost as already stored, never as a second save", async () => {
    const user = userEvent.setup();
    open("/messages/5");
    await editorReady();
    await user.clear(screen.getByRole("textbox", { name: "Subject" }));
    await user.type(screen.getByRole("textbox", { name: "Subject" }), "Snacks");
    vi.mocked(Messages.update).mockRejectedValueOnce(new ApiError(502, "bad gateway", {}, true));
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    expect(await screen.findByText(/We couldn't save the message/)).toBeInTheDocument();
    // It was stored: the next press is refused, and the read shows this save.
    const stored = message({ subject: "Snacks", revision: "1.stored" });
    vi.mocked(Messages.update).mockRejectedValueOnce(new ApiError(409, "changed", { code: "message_changed" }));
    vi.mocked(Messages.get).mockResolvedValue({ message: stored });
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    expect(await screen.findByText("Draft saved.")).toBeInTheDocument();
    expect(Messages.update).toHaveBeenCalledTimes(2);
    // The next save names what the read found.
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    await waitFor(() => expect(Messages.update).toHaveBeenLastCalledWith(5, expect.anything(), "1.stored", undefined));
  });

  it("never starts a save twice", async () => {
    const user = userEvent.setup();
    let answer!: (v: Awaited<ReturnType<typeof Messages.update>>) => void;
    vi.mocked(Messages.update).mockReturnValueOnce(new Promise((r) => { answer = r; }));
    open("/messages/5");
    await editorReady();
    const draft = screen.getByRole("button", { name: "Save draft" });
    await user.click(draft);
    expect(await screen.findByRole("button", { name: "Saving…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Send now" })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "Saving…" }));
    fireEvent.submit(screen.getByRole("form"));
    expect(Messages.update).toHaveBeenCalledTimes(1);
    await act(async () => answer({ message: message({ revision: "1.r9" }) }));
    expect(await screen.findByText("Draft saved.")).toBeInTheDocument();
  });

  it("does nothing with a refusal that arrives after the person left", async () => {
    const user = userEvent.setup();
    let refuse!: (e: ApiError) => void;
    vi.mocked(Messages.update).mockReturnValueOnce(new Promise((_, r) => { refuse = r; }));
    const router = open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    await act(async () => { await router.navigate("/messages"); });
    vi.mocked(Messages.get).mockClear();
    await act(async () => refuse(new ApiError(409, "changed", { code: "message_changed" })));
    expect(Messages.get).not.toHaveBeenCalled();
    expect(screen.getByText("Messages list")).toBeInTheDocument();
  });

  it("names the save's revision and the review on the send", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.schedule).mockResolvedValue({ message: message({ status: "scheduled", scheduledAt: new Date().toISOString(), revision: "1.sched" }) });
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    await user.click(within(await screen.findByRole("group", { name: "Send to 2 people now?" })).getByRole("button", { name: "Send now" }));
    expect(Messages.review).toHaveBeenCalledWith(5);
    expect(Messages.schedule).toHaveBeenCalledWith(5, { revision: "1.r1", review: "review-1", scheduledAt: undefined });
    expect(await screen.findByText(/^Waiting to send|^Scheduled for|^Sending/)).toBeInTheDocument();
  });

  it("shows how the list changed, and sends to it as it is now; a second change shows again", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.schedule)
      .mockRejectedValueOnce(changed({ review: { summary: summary({ eligible: 3 }), reached: 3, token: "review-2" }, change: { added: ["Pat Lee"], addedMore: 0, dropped: ["Sam Roe"], droppedUnnamed: 1 } }))
      .mockRejectedValueOnce(changed({ review: { summary: summary({ eligible: 4 }), reached: 4, token: "review-3" }, change: { added: ["Kim Ng"], addedMore: 0, dropped: [], droppedUnnamed: 0 } }))
      .mockResolvedValueOnce({ message: message({ status: "scheduled", scheduledAt: new Date().toISOString(), revision: "1.sched" }) });
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    await user.click(within(await screen.findByRole("group", { name: "Send to 2 people now?" })).getByRole("button", { name: "Send now" }));
    const step = await screen.findByRole("group", { name: "The list of people who get this has changed since you looked." });
    expect(step).toHaveTextContent("Pat Lee will now get this.");
    expect(step).toHaveTextContent("2 people will no longer get it: Sam Roe and 1 other.");
    expect(step).toHaveTextContent("Send to 3 people now?");
    expect(step).toHaveAccessibleDescription(expect.stringContaining("Pat Lee will now get this."));
    const go = within(step).getByRole("button", { name: "Send to the list as it is now" });
    expect(go).toHaveClass("btn-primary");
    // Focus is on the line that says what changed, so a key still held from the refused press chooses nothing.
    expect(within(step).getByText("The list of people who get this has changed since you looked.")).toHaveFocus();
    expect(document.querySelectorAll(".btn-primary")).toHaveLength(1);
    await user.click(go);
    expect(Messages.schedule).toHaveBeenLastCalledWith(5, { revision: "1.r1", review: "review-2", scheduledAt: undefined });
    const again = await screen.findByRole("group", { name: "The list of people who get this has changed since you looked." });
    expect(again).toHaveTextContent("Kim Ng will now get this.");
    expect(again).not.toHaveTextContent("no longer");
    await user.click(within(again).getByRole("button", { name: "Send to the list as it is now" }));
    expect(Messages.schedule).toHaveBeenLastCalledWith(5, { revision: "1.r1", review: "review-3", scheduledAt: undefined });
    expect(await screen.findByRole("heading", { name: "Bring a friend", level: 1 })).toBeInTheDocument();
    expect(Messages.schedule).toHaveBeenCalledTimes(3);
  });

  it("goes back to the whole list of the fresh review, open, when the sender asks to review it, with no count to wait for", async () => {
    const user = userEvent.setup();
    const fresh = summary({ people: 2, eligible: 2, skipped: {}, recipients: [{ name: "Pat Lee", teen: false }, { name: "Tia Teen", teen: true }] });
    vi.mocked(Messages.schedule).mockRejectedValueOnce(changed({ review: { summary: fresh, reached: 2, token: "review-2" }, change: { added: [], addedMore: 0, dropped: [], droppedUnnamed: 1 } }));
    open("/messages/5");
    await editorReady();
    await screen.findByText(/^3 people;/);
    await user.click(screen.getByRole("button", { name: "Send now" }));
    await user.click(within(await screen.findByRole("group", { name: "Send to 2 people now?" })).getByRole("button", { name: "Send now" }));
    const step = await screen.findByRole("group", { name: "The list of people who get this has changed since you looked." });
    expect(step).toHaveTextContent("1 person will no longer get it.");
    const counted = vi.mocked(Messages.audiencePreview).mock.calls.length;
    await user.click(within(step).getByRole("button", { name: "Review the whole list" }));
    expect(screen.queryByRole("group", { name: /changed since you looked/ })).toBeNull();
    const list = screen.getByText("See who's on the list");
    expect(list.closest("details")).toHaveAttribute("open");
    expect(list).toHaveFocus();
    expect(within(requiredElement(list.closest("details"), HTMLDetailsElement)).getByText("Pat Lee")).toBeInTheDocument();
    expect(screen.getByText("2 people; everyone on the list will get it.")).toBeInTheDocument();
    expect(vi.mocked(Messages.audiencePreview).mock.calls.length).toBe(counted);
    expect(Messages.schedule).toHaveBeenCalledTimes(1);
  });

  it("asks again with a fresh review when the server needs one", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.schedule)
      .mockRejectedValueOnce(new ApiError(409, "check who gets this", { code: "message_review_needed", review: JSON.parse(JSON.stringify({ summary: summary({ eligible: 2 }), reached: 2, token: "review-2" })) }))
      .mockResolvedValueOnce({ message: message({ status: "scheduled", scheduledAt: new Date().toISOString(), revision: "1.sched" }) });
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    await user.click(within(await screen.findByRole("group", { name: "Send to 2 people now?" })).getByRole("button", { name: "Send now" }));
    const ask = await screen.findByRole("group", { name: "Send to 2 people now?" });
    expect(ask).toHaveTextContent("We checked who gets it again, so this is the list as it is now.");
    await user.click(within(ask).getByRole("button", { name: "Send now" }));
    expect(Messages.schedule).toHaveBeenLastCalledWith(5, { revision: "1.r1", review: "review-2", scheduledAt: undefined });
  });

  it("settles a send whose answer was lost as sent on the next press, never sending twice", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.schedule)
      .mockRejectedValueOnce(new ApiError(502, "bad gateway", {}, true))
      .mockRejectedValueOnce(new ApiError(409, "changed", { code: "message_changed" }));
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    await user.click(within(await screen.findByRole("group", { name: "Send to 2 people now?" })).getByRole("button", { name: "Send now" }));
    expect(await screen.findByText(/couldn't tell whether it went out/)).toBeInTheDocument();
    const ask = screen.getByRole("group", { name: "Send to 2 people now?" });
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "scheduled", scheduledAt: new Date().toISOString(), editable: true, revision: "1.sched" }) });
    await user.click(within(ask).getByRole("button", { name: "Send now" }));
    expect(await screen.findByRole("heading", { name: "Bring a friend", level: 1 })).toBeInTheDocument();
    expect(screen.queryByText(/could not be saved/)).toBeNull();
    expect(screen.queryByRole("textbox", { name: "Subject" })).toBeNull();
    expect(Messages.schedule).toHaveBeenCalledTimes(2);
  });

  it("settles a lost send now that is already going out without an error", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.schedule)
      .mockRejectedValueOnce(new TypeError("Failed to fetch"))
      .mockRejectedValueOnce(new ApiError(409, "sending", { code: "message_locked" }));
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    await user.click(within(await screen.findByRole("group", { name: "Send to 2 people now?" })).getByRole("button", { name: "Send now" }));
    const ask = await screen.findByRole("group", { name: "Send to 2 people now?" });
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "sending", scheduledAt: new Date().toISOString(), editable: false, revision: undefined }) });
    await user.click(within(ask).getByRole("button", { name: "Send now" }));
    expect(await screen.findByText("Sending")).toBeInTheDocument();
    expect(screen.queryByText(ERROR_COPY.message_locked)).toBeNull();
  });

  it("after a send refused because someone changed the message, Edit starts from what is stored and saves on one press", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.schedule).mockRejectedValueOnce(new ApiError(409, "changed", { code: "message_changed" }));
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    const ask = await screen.findByRole("group", { name: "Send to 2 people now?" });
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ subject: "The office's words", revision: "1.other" }) });
    await user.click(within(ask).getByRole("button", { name: "Send now" }));
    await screen.findByText(/changed somewhere else, so it was not sent/);
    vi.mocked(Messages.update).mockClear();
    await user.click(screen.getByRole("button", { name: "Edit" }));
    const subject = await screen.findByRole("textbox", { name: "Subject" });
    expect(subject).toHaveValue("The office's words");
    await user.type(subject, "!");
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    expect(await screen.findByText("Draft saved.")).toBeInTheDocument();
    expect(Messages.update).toHaveBeenCalledTimes(1);
    expect(Messages.update).toHaveBeenCalledWith(5, expect.objectContaining({ subject: "The office's words!" }), "1.other", undefined);
    expect(screen.queryByText(/could not be saved/)).toBeNull();
  });

  it("says the message is not here when a save finds it gone, even after an earlier read", async () => {
    const user = userEvent.setup();
    open("/messages/5");
    await editorReady();
    await user.type(screen.getByRole("textbox", { name: "Subject" }), "!");
    vi.mocked(Messages.update).mockRejectedValueOnce(new ApiError(409, "changed", { code: "message_changed" }));
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ revision: "1.other" }) });
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    expect(await screen.findByText("Draft saved.")).toBeInTheDocument();
    vi.mocked(Messages.update).mockRejectedValueOnce(new ApiError(404, "message not found", { code: "not_found" }));
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    expect(await screen.findByText("This message isn't here")).toBeInTheDocument();
    expect(screen.getByText("Your changes could not be saved.")).toHaveFocus();
  });

  it("does nothing with a refusal that arrives after another message was opened", async () => {
    const user = userEvent.setup();
    let refuse!: (e: ApiError) => void;
    vi.mocked(Messages.update).mockReturnValueOnce(new Promise((_, r) => { refuse = r; }));
    const router = open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Save draft" }));
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ id: 6, subject: "Another one", revision: "1.six" }) });
    await act(async () => { await router.navigate("/messages/6"); });
    expect(await screen.findByRole("textbox", { name: "Subject" })).toHaveValue("Another one");
    vi.mocked(Messages.get).mockClear();
    await act(async () => refuse(new ApiError(409, "changed", { code: "message_changed" })));
    expect(Messages.get).not.toHaveBeenCalled();
    expect(screen.getByRole("textbox", { name: "Subject" })).toHaveValue("Another one");
    expect(screen.queryByText(/could not be saved/)).toBeNull();
  });

  it("does nothing with a send's answer that arrives after the person left", async () => {
    const user = userEvent.setup();
    let refuse!: (e: ApiError) => void;
    vi.mocked(Messages.schedule).mockReturnValueOnce(new Promise((_, r) => { refuse = r; }));
    const router = open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    await user.click(within(await screen.findByRole("group", { name: "Send to 2 people now?" })).getByRole("button", { name: "Send now" }));
    await act(async () => { await router.navigate("/messages"); });
    vi.mocked(Messages.get).mockClear();
    vi.mocked(Messages.review).mockClear();
    await act(async () => refuse(new ApiError(409, "changed", { code: "message_list_changed" })));
    expect(Messages.get).not.toHaveBeenCalled();
    expect(Messages.review).not.toHaveBeenCalled();
    expect(screen.getByText("Messages list")).toBeInTheDocument();
  });
});

describe("every name and count on screen is the review's", () => {
  const changed = (over: { review: MessageReview; change: AudienceChange }) => new ApiError(409, "who gets this message changed", { code: "message_list_changed", ...JSON.parse(JSON.stringify(over)) });

  it("shows the review's list in the confirmation and the picker, until the audience is changed", async () => {
    const user = userEvent.setup();
    const pat = summary({ recipients: [{ name: "Pat Lee", teen: false }, { name: "Opal Opted", teen: false, skipped: "won't get this" }, { name: "Tia Teen", teen: true }] });
    vi.mocked(Messages.review).mockResolvedValue({ review: { summary: pat, reached: 2, token: "review-1" } });
    open("/messages/5");
    await editorReady();
    await screen.findByText(/^3 people;/);
    await user.click(screen.getByText("See who's on the list"));
    expect(screen.getByText("Ann Adult")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    const ask = await screen.findByRole("group", { name: "Send to 2 people now?" });
    await user.click(within(ask).getByText("See who's on the list"));
    expect(within(ask).getByText("Pat Lee")).toBeInTheDocument();
    // The picker shows the same review: Ann, whom the token does not hold, is gone from the screen.
    expect(screen.queryByText("Ann Adult")).toBeNull();
    expect(screen.getAllByText("Pat Lee")).toHaveLength(2);
    // Changing the audience withdraws the question, and the picker counts the new audience itself.
    vi.mocked(Messages.audiencePreview).mockResolvedValue({ summary: summary({ people: 1, eligible: 1, skipped: {}, recipients: [{ name: "Lee Leader", teen: false }] }) });
    await user.click(screen.getByRole("button", { name: "Members" }));
    expect(screen.queryByRole("group", { name: "Send to 2 people now?" })).toBeNull();
    expect(await screen.findByText("1 person; everyone on the list will get it.")).toBeInTheDocument();
    expect(screen.queryByText("Pat Lee")).toBeNull();
  });

  it("counts the people the changed list reaches, as many as it names, a push-only joiner included", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.schedule).mockRejectedValueOnce(changed({ review: { summary: summary({ eligible: 2, push: 1 }), reached: 3, token: "review-2" }, change: { added: ["Pat Lee"], addedMore: 0, dropped: [], droppedUnnamed: 0 } }));
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    await user.click(within(await screen.findByRole("group", { name: "Send to 2 people now?" })).getByRole("button", { name: "Send now" }));
    const step = await screen.findByRole("group", { name: "The list of people who get this has changed since you looked." });
    expect(step).toHaveTextContent("Pat Lee will now get this.");
    expect(step).toHaveTextContent("Send to 3 people now?");
  });

  it("chooses nothing for a key still held when a refusal replaces the question", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.schedule)
      .mockRejectedValueOnce(changed({ review: { summary: summary({ eligible: 3 }), reached: 3, token: "review-2" }, change: { added: ["Pat Lee"], addedMore: 0, dropped: [], droppedUnnamed: 0 } }))
      .mockRejectedValueOnce(new ApiError(409, "check who gets this", { code: "message_review_needed", review: JSON.parse(JSON.stringify({ summary: summary({ eligible: 3 }), reached: 3, token: "review-3" })) }));
    open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Send now" }));
    await screen.findByRole("group", { name: "Send to 2 people now?" });
    expect(screen.getByRole("button", { name: "Send now" })).toHaveFocus();
    await user.keyboard("{Enter}");
    const step = await screen.findByRole("group", { name: "The list of people who get this has changed since you looked." });
    await user.keyboard("{Enter}{Enter}");
    expect(Messages.schedule).toHaveBeenCalledTimes(1);
    expect(step).toBeInTheDocument();
    await user.click(within(step).getByRole("button", { name: "Send to the list as it is now" }));
    const again = await screen.findByRole("group", { name: "Send to 3 people now?" });
    expect(within(again).getByText("We checked who gets it again, so this is the list as it is now.")).toHaveFocus();
    await user.keyboard("{Enter}{Enter}");
    expect(Messages.schedule).toHaveBeenCalledTimes(2);
  });

  it("treats a schedule moved somewhere else as a conflict, never sending it back to the old time", async () => {
    const user = userEvent.setup();
    const at = new Date(Date.now() + 3 * 864e5).toISOString();
    const scheduled = message({ status: "scheduled", scheduledAt: at });
    vi.mocked(Messages.get).mockResolvedValue({ message: scheduled });
    vi.mocked(Messages.update).mockImplementation(async (_id, d) => ({ message: { ...scheduled, subject: d.subject, revision: "1.r1" } }));
    vi.mocked(Messages.schedule).mockRejectedValueOnce(new ApiError(409, "changed", { code: "message_changed" }));
    open("/messages/5");
    await user.click(await screen.findByRole("button", { name: "Edit" }));
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Schedule" }));
    const ask = await screen.findByRole("group", { name: /^Send to 2 people/ });
    // Someone moves it three hours later; nothing else changes.
    vi.mocked(Messages.get).mockResolvedValue({ message: { ...scheduled, scheduledAt: new Date(Date.parse(at) + 3 * 36e5).toISOString(), revision: "1.moved" } });
    await user.click(within(ask).getByRole("button", { name: "Schedule" }));
    expect(await screen.findByText("This message changed somewhere else, so it was not sent. This is the message as it is now.")).toHaveFocus();
    expect(Messages.review).toHaveBeenCalledTimes(1);
    expect(Messages.schedule).toHaveBeenCalledTimes(1);
  });
});

describe("a scheduled message's new list is the sender's to confirm", () => {
  const at = () => new Date(Date.now() + 3 * 864e5).toISOString();
  const needed = (review: MessageReview) => new ApiError(409, "check who gets this", { code: "message_review_needed", review: JSON.parse(JSON.stringify(review)) });
  const proposal: MessageReview = { summary: summary({ people: 1, eligible: 1, skipped: {}, recipients: [{ name: "Lee Leader", teen: false }] }), reached: 1, token: "proposal-1" };

  async function editLeadersOnly(user: ReturnType<typeof userEvent.setup>) {
    const scheduled = message({ status: "scheduled", scheduledAt: at() });
    vi.mocked(Messages.get).mockResolvedValue({ message: scheduled });
    vi.mocked(Messages.update).mockImplementation(async (_id, d, _revision, review) => {
      if (!review) throw needed(proposal);

      return { message: { ...scheduled, audience: d.audience, revision: "1.r1" }, review: "saved-list" };
    });
    open("/messages/5");
    await user.click(await screen.findByRole("button", { name: "Edit" }));
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Members" }));
  }

  for (const press of [
    { button: "Preview", check: () => expect(Messages.preview).toHaveBeenCalledWith(5) },
    { button: "Send a test to me", check: () => expect(Messages.test).toHaveBeenCalledWith(5) },
  ]) {
    it(`asks before ${press.button} saves it, says it stays scheduled, then goes on`, async () => {
      const user = userEvent.setup();
      await editLeadersOnly(user);
      await user.click(screen.getByRole("button", { name: press.button }));
      const ask = await screen.findByRole("group", { name: "Change who gets this to 1 person?" });
      expect(ask).toHaveTextContent(/It isn't sent now\. It stays scheduled on .* and goes to whoever is on the list then\./);
      expect(within(ask).getByText("Change who gets this to 1 person?")).toHaveFocus();
      expect(Messages.preview).not.toHaveBeenCalled();
      expect(Messages.test).not.toHaveBeenCalled();
      await user.keyboard("{Enter}");
      expect(Messages.update).toHaveBeenCalledTimes(1);
      await user.click(within(ask).getByRole("button", { name: "Save the change" }));
      await waitFor(press.check);
      expect(Messages.update).toHaveBeenLastCalledWith(5, expect.objectContaining({ audience: { type: "group", groupId: 7, roles: ["leader"] } }), "1.r0", "proposal-1");
      expect(Messages.schedule).not.toHaveBeenCalled();
      expect(screen.queryByRole("group", { name: /Change who gets this/ })).toBeNull();
    });
  }

  it("counts everyone the new list reaches in its question", async () => {
    const user = userEvent.setup();
    await editLeadersOnly(user);
    vi.mocked(Messages.update).mockRejectedValue(needed({ summary: summary({ eligible: 2, push: 1 }), reached: 3, token: "proposal-2" }));
    await user.click(screen.getByRole("button", { name: "Preview" }));
    expect(await screen.findByRole("group", { name: "Change who gets this to 3 people?" })).toBeInTheDocument();
  });

  it("asks once before Schedule saves the new list, then schedules with the list it confirmed", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.schedule).mockImplementation(async (id, send) => ({ message: message({ id: Number(id), status: "scheduled", scheduledAt: send.scheduledAt ?? null, revision: "1.sched" }) }));
    await editLeadersOnly(user);
    await user.click(screen.getByRole("button", { name: "Schedule" }));
    const ask = await screen.findByRole("group", { name: /^Send to 1 person on / });
    await user.click(within(ask).getByRole("button", { name: "Schedule" }));
    await waitFor(() => expect(Messages.schedule).toHaveBeenCalledWith(5, expect.objectContaining({ revision: "1.r1", review: "saved-list" })));
    expect(Messages.review).not.toHaveBeenCalled();
    expect(await screen.findByRole("heading", { name: "Bring a friend", level: 1 })).toBeInTheDocument();
  });

  it("keeps the schedule's question on screen when its answer is lost after a confirmed new list, and the next press ends as sent", async () => {
    const user = userEvent.setup();
    vi.mocked(Messages.schedule)
      .mockRejectedValueOnce(new ApiError(502, "bad gateway", {}, true))
      .mockRejectedValueOnce(new ApiError(409, "changed", { code: "message_changed" }));
    await editLeadersOnly(user);
    await user.click(screen.getByRole("button", { name: "Schedule" }));
    await user.click(within(await screen.findByRole("group", { name: /^Send to 1 person on / })).getByRole("button", { name: "Schedule" }));
    expect(await screen.findByText(/couldn't tell whether it went out/)).toBeInTheDocument();
    const ask = screen.getByRole("group", { name: /^Send to 1 person on / });
    const formAt = vi.mocked(Messages.schedule).mock.calls[0][1].scheduledAt ?? null;
    vi.mocked(Messages.get).mockResolvedValue({ message: message({ status: "scheduled", scheduledAt: formAt, audience: { type: "group", groupId: 7, roles: ["leader"] }, revision: "1.sched" }) });
    await user.click(within(ask).getByRole("button", { name: "Schedule" }));
    expect(await screen.findByRole("heading", { name: "Bring a friend", level: 1 })).toBeInTheDocument();
    expect(screen.queryByText(/could not be/)).toBeNull();
    expect(Messages.schedule).toHaveBeenCalledTimes(2);
    expect(Messages.schedule).toHaveBeenLastCalledWith(5, expect.objectContaining({ revision: "1.r1", review: "saved-list" }));
    expect(Messages.update).toHaveBeenCalledTimes(2);
  });

  it("leaves it scheduled for its old list when the sender goes back, with the edit still in the form", async () => {
    const user = userEvent.setup();
    await editLeadersOnly(user);
    await user.click(screen.getByRole("button", { name: "Preview" }));
    const ask = await screen.findByRole("group", { name: "Change who gets this to 1 person?" });
    await user.click(within(ask).getByRole("button", { name: "Go back" }));
    expect(screen.queryByRole("group", { name: /Change who gets this/ })).toBeNull();
    expect(screen.getByRole("button", { name: "Members" })).toHaveAttribute("aria-pressed", "false");
    expect(Messages.update).toHaveBeenCalledTimes(1);
    expect(Messages.preview).not.toHaveBeenCalled();
  });

  it("saves the words alone without asking", async () => {
    const user = userEvent.setup();
    const scheduled = message({ status: "scheduled", scheduledAt: at() });
    vi.mocked(Messages.get).mockResolvedValue({ message: scheduled });
    vi.mocked(Messages.update).mockResolvedValue({ message: { ...scheduled, subject: "Bring a friend!", revision: "1.r1" } });
    open("/messages/5");
    await user.click(await screen.findByRole("button", { name: "Edit" }));
    await editorReady();
    await user.type(screen.getByRole("textbox", { name: "Subject" }), "!");
    await user.click(screen.getByRole("button", { name: "Preview" }));
    await waitFor(() => expect(Messages.preview).toHaveBeenCalledWith(5));
    expect(screen.queryByRole("group", { name: /Change who gets this/ })).toBeNull();
  });
});

describe("an editor the person moved away from", () => {
  it("is gone as soon as the address names another message, and its late discard goes nowhere", async () => {
    const user = userEvent.setup();
    vi.spyOn(window, "confirm").mockReturnValue(true);
    let discarded!: (v: Awaited<ReturnType<typeof Messages.cancel>>) => void;
    vi.mocked(Messages.cancel).mockReturnValueOnce(new Promise((r) => { discarded = r; }));
    const router = open("/messages/5");
    await editorReady();
    await user.click(screen.getByRole("button", { name: "Discard draft" }));
    let opened!: (v: Awaited<ReturnType<typeof Messages.get>>) => void;
    vi.mocked(Messages.get).mockReturnValueOnce(new Promise((r) => { opened = r; }));
    await act(async () => { await router.navigate("/messages/6"); });
    // Message 6 is still loading: nothing of message 5's editor is there to press.
    expect(screen.queryByRole("textbox", { name: "Subject" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Send now" })).toBeNull();
    await act(async () => discarded({ message: message({ status: "cancelled", editable: false }) }));
    expect(router.state.location.pathname).toBe("/messages/6");
    await act(async () => opened({ message: message({ id: 6, subject: "Another one", revision: "1.six" }) }));
    expect(await screen.findByRole("textbox", { name: "Subject" })).toHaveValue("Another one");
    expect(router.state.location.pathname).toBe("/messages/6");
  });

  it("opens a blank message after a saved one when the address moves to a new one", async () => {
    const router = open("/messages/5");
    expect(await editorReady()).toBeInTheDocument();
    await act(async () => { await router.navigate("/messages/new"); });
    expect(await screen.findByRole("heading", { name: "New message", level: 1 })).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "Subject" })).toHaveValue("");
  });
});
