import { stubService } from "../test/service";
import { Households } from "../api/households";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const mine = vi.fn();
const addPerson = vi.fn();
const convert = vi.fn();
const resend = vi.fn();
const cancel = vi.fn();

stubService(Households, () => ({ mine: () => mine(), addPerson: (d) => addPerson(d), update: vi.fn(), updatePerson: vi.fn(), removePerson: vi.fn(), convert: (id, email) => convert(id, email), resendConversion: (id) => resend(id), cancelConversion: (id) => cancel(id) }));

import { HouseholdCard, personProblems } from "./HouseholdCard";
import { ApiError } from "../api/core";
import type { Household, HouseholdPerson } from "../types";

const household = {
  id: 1, name: "Smith household", address: "", createdBy: 1, revision: "h-1",
  accounts: [{ userId: 1, name: "Pat Smith", relation: "guardian" as const, status: "active" as const }],
  people: [{ id: 5, householdId: 1, firstName: "Kid", lastName: "Smith", name: "Kid Smith", kind: "child", birthDate: "2020-05-01", careNotes: "", photoConsent: false, linkStatus: "pending", createdBy: 1, revision: "p-5" }],
};

describe("HouseholdCard (M1.9)", () => {
  beforeEach(() => {
    mine.mockReset();
    addPerson.mockReset();
  });

  it("explains and offers to add when there is no household yet", async () => {
    mine.mockResolvedValue({ household: null });
    render(<HouseholdCard />);
    expect(await screen.findByText(/Add your children, teens/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add a household member" })).toBeInTheDocument();
  });

  it("lists people with their link status", async () => {
    mine.mockResolvedValue({ household });
    render(<HouseholdCard />);
    expect((await screen.findAllByText("Kid Smith")).length).toBeGreaterThan(0);
    expect(screen.getByText("Waiting for an admin to check")).toBeInTheDocument();
  });

  it("checks the form before sending", async () => {
    mine.mockResolvedValue({ household: null });
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Add a household member" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByText("Add a first name.")).toBeInTheDocument();
    expect(screen.getByText("Add a birth date for a child or teen.")).toBeInTheDocument();
    expect(addPerson).not.toHaveBeenCalled();
  });
});

describe("personProblems", () => {
  const base = { firstName: "A", lastName: "", kind: "adult" as const, birthDate: "", careNotes: "", photoConsent: false };
  it("lets an adult skip the birth date", () => {
    expect(personProblems(base, "2026-10-21")).toEqual({});
  });
  it("refuses a future birth date", () => {
    expect(personProblems({ ...base, birthDate: "2030-01-01" }, "2026-10-21").birthDate).toMatch(/future/);
  });
});

describe("HouseholdCard focus and states (PR 107 review)", () => {
  beforeEach(() => {
    mine.mockReset();
    addPerson.mockReset();
  });

  it("moves focus to the first bad field on a failed check", async () => {
    mine.mockResolvedValue({ household: null });
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Add a household member" }));
    expect(screen.getByLabelText("First name")).toHaveFocus();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByLabelText("First name")).toHaveFocus();
  });

  it("offers Try again when the load fails", async () => {
    mine.mockRejectedValueOnce(new Error("offline"));
    mine.mockResolvedValueOnce({ household: null });
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Try again" }));
    expect(await screen.findByText("No one in your household yet")).toBeInTheDocument();
  });

  it("formats birth dates and uses the violet badge only for Confirmed", async () => {
    mine.mockResolvedValue({
      household: { ...household, people: [{ ...household.people[0], linkStatus: "confirmed" }] },
    });
    render(<HouseholdCard />);
    expect(await screen.findByText("Born May 1, 2020")).toBeInTheDocument();
    expect(screen.getByText("Confirmed")).toBeInTheDocument();
    expect(screen.getByText("Child")).toBeInTheDocument();
  });
});

describe("household name and address", () => {
  beforeEach(() => { vi.resetAllMocks(); mine.mockResolvedValue({ household: { ...household, people: [], address: "Old address" } }); });

  it("shows an empty household's details and saves through the existing endpoint", async () => {
    const { Households } = await import("../api/households");
    vi.mocked(Households.update).mockResolvedValue({ household: { ...household, people: [], name: "New household", address: "New address", revision: "h-2" } });
    render(<HouseholdCard />);
    const rename = await screen.findByRole("button", { name: "Rename" });
    expect(screen.getByText("Old address")).toBeInTheDocument();
    await userEvent.click(rename);
    expect(screen.getByLabelText(/^Household name/)).toHaveFocus();
    await userEvent.clear(screen.getByLabelText(/^Household name/)); await userEvent.type(screen.getByLabelText(/^Household name/), "New household");
    await userEvent.clear(screen.getByLabelText(/^Address/)); await userEvent.type(screen.getByLabelText(/^Address/), "New address");
    await userEvent.click(screen.getByRole("button", { name: "Save household" }));
    expect(Households.update).toHaveBeenCalledWith({ name: "New household", address: "New address" }, "h-1");
    expect(screen.getByText("New household")).toBeInTheDocument(); expect(screen.getByText("New address")).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Household updated.");
    await vi.waitFor(() => expect(rename).toHaveFocus());
  });

  it("sends an explicit address clear and preserves valid fields on failure", async () => {
    const { Households } = await import("../api/households");
    vi.mocked(Households.update).mockRejectedValueOnce(new Error("offline"));
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit address" }));
    expect(screen.getByLabelText(/^Household name/)).toHaveAttribute("maxlength", "100");
    expect(screen.getByLabelText(/^Address/)).toHaveAttribute("maxlength", "500");
    await userEvent.clear(screen.getByLabelText(/^Address/));
    await userEvent.click(screen.getByRole("button", { name: "Save household" }));
    expect(Households.update).toHaveBeenCalledWith({ name: "Smith household", address: "" }, "h-1");
    expect(screen.getByLabelText(/^Household name/)).toHaveValue("Smith household"); expect(screen.getByLabelText(/^Address/)).toHaveValue("");
    expect(screen.getByRole("alert")).toHaveTextContent("We couldn't reach Fold.");
  });

  it("validates a required name before sending", async () => {
    const { Households } = await import("../api/households"); render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Rename" }));
    await userEvent.clear(screen.getByLabelText(/^Household name/));
    // Native validation and the shared field validation both reject it.
    screen.getByRole("form", { name: "Edit household" }).setAttribute("novalidate", "");
    await userEvent.click(screen.getByRole("button", { name: "Save household" }));
    expect(Households.update).not.toHaveBeenCalled(); expect(screen.getByLabelText(/^Household name/)).toHaveAttribute("aria-invalid", "true");
  });

  it("preserves inaccessible care notes while explicitly clearing a visible optional field", async () => {
    const { Households } = await import("../api/households");
    mine.mockResolvedValue({ household: { ...household, people: [{ ...household.people[0], careNotes: undefined }] } });
    vi.mocked(Households.updatePerson).mockRejectedValue(new Error("offline"));
    render(<HouseholdCard />); await userEvent.click(await screen.findByRole("button", { name: "Edit Kid Smith" }));
    await userEvent.clear(screen.getByLabelText(/^Last name/)); await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(Households.updatePerson).toHaveBeenCalledWith(5, expect.objectContaining({ lastName: "", careNotes: undefined }), "p-5");
    expect(screen.getByLabelText(/^Care notes/)).toHaveAccessibleDescription(expect.stringContaining("24 hours"));
  });
});

it("preserves the conversion email and focuses its error", async () => {
  mine.mockResolvedValue({ household: { ...household, people: [{ ...household.people[0], name: "Mary", kind: "adult", canConvert: true }] } });
  convert.mockRejectedValue(new ApiError(409, "private server text", { code: "household_conversion_email" }));
  render(<HouseholdCard />);
  await userEvent.click(await screen.findByRole("button", { name: "Create their own account" }));
  const email = screen.getByLabelText("Their email address");
  await userEvent.type(email, "mary@example.com");
  await userEvent.click(screen.getByRole("button", { name: "Send account invitation" }));
  expect(await screen.findByText("That email isn't available. Use a different email address.")).toBeInTheDocument();
  expect(email).toHaveValue("mary@example.com");
  expect(email).toHaveFocus();
  expect(screen.getByRole("link", { name: "Accounts and household help" })).toHaveAttribute("href", "/help/members");
});

it("shows pending destination, resend and cancellation without editing it", async () => {
  mine.mockResolvedValue({ household: { ...household, people: [{ ...household.people[0], canConvert: true, conversion: { id: 10, email: "new@example.com", expiresAt: "2026-12-01T12:00:00Z", state: "pending" } }] } });
  resend.mockResolvedValue({}); cancel.mockResolvedValue({});
  render(<HouseholdCard />);
  expect(await screen.findByText(/Invitation pending: new@example.com/)).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Resend invitation" }));
  expect(resend).toHaveBeenCalledWith(5);
  expect(await screen.findByText(/Invitation resent. The previous link/)).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Cancel invitation" }));
  expect(cancel).toHaveBeenCalledWith(5);
});

it("keeps the account button hidden until a household teen is eligible", async () => {
  mine.mockResolvedValue({ household: { ...household, people: [{ ...household.people[0], kind: "teen", canConvert: false, linkStatus: "pending" }] } });
  render(<HouseholdCard />);
  expect(await screen.findByText("Waiting for an admin to check")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Create their own account" })).not.toBeInTheDocument();
});

 it("keeps an adult editable while approval is pending and hides invitation controls", async () => {
  mine.mockResolvedValue({ household: { ...household, people: [{ ...household.people[0], kind: "adult", birthDate: "1990-01-01", linkStatus: "none", canConvert: true, conversion: { email: "new@example.com", expiresAt: "2026-12-01T12:00:00Z", state: "awaiting_approval" } }] } });
  render(<HouseholdCard />);
  expect(await screen.findByText("Account waiting for an admin")).toBeInTheDocument();
  expect(screen.getByText("Your household keeps managing their sign-ups until then.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Edit Kid Smith" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /Resend/ })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Create their own account" })).not.toBeInTheDocument();
});

it("offers a confirmed teen an invitation with the consent and guardian checks explained", async () => {
  mine.mockResolvedValue({ household: { ...household, people: [{ ...household.people[0], kind: "teen", birthDate: "2011-05-01", linkStatus: "confirmed", canConvert: true }] } });
  render(<HouseholdCard />);
  await userEvent.click(await screen.findByRole("button", { name: "Create their own account" }));
  expect(screen.getByText(/consent is granted, and an admin confirms the new guardian link/)).toBeInTheDocument();
  expect(screen.getByText(/Your household keeps managing their sign-ups until all checks are complete/)).toBeInTheDocument();
});

it("keeps care and photo controls available while a staged teen's identity is fixed", async () => {
  mine.mockResolvedValue({ household: { ...household, people: [{ ...household.people[0], kind: "teen", birthDate: "2011-05-01", linkStatus: "confirmed", canConvert: false, conversion: { email: "teen@example.com", expiresAt: "2026-12-01T12:00:00Z", state: "awaiting_consent" } }] } });
  render(<HouseholdCard />);
  expect(await screen.findByText("Account waiting for consent and guardian confirmation")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Resend invitation" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Remove Kid Smith" })).toBeDisabled();
  await userEvent.click(screen.getByRole("button", { name: "Edit Kid Smith" }));
  expect(screen.getByLabelText("First name")).toHaveAttribute("readonly");
  expect(screen.getByLabelText("Birth date")).toHaveAttribute("readonly");
  expect(screen.getByRole("radio", { name: "Teen" })).toBeDisabled();
  expect(screen.getByLabelText(/^Care notes/)).toBeEnabled();
  expect(screen.getByRole("checkbox", { name: "OK to appear in church photos" })).toBeEnabled();
});

describe("HouseholdCard under a page heading", () => {
  it("keeps the My household heading by default", async () => {
    mine.mockResolvedValue({ household });
    render(<HouseholdCard />);
    expect(await screen.findByRole("heading", { level: 3, name: "Smith household" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 2, name: "My household" })).toBeInTheDocument();
  });

  it("drops it with showHeading=false, keeps the region name and steps the sub-headings up", async () => {
    mine.mockResolvedValue({ household });
    render(<HouseholdCard showHeading={false} />);
    expect(await screen.findByRole("region", { name: "My household" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "My household" })).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 2, name: "Smith household" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Add a household member" }));
    expect(screen.getByRole("heading", { level: 2, name: "Add a household member" })).toBeInTheDocument();
  });
});

// DEC-174: a household's details and each person's edit name the read they were made from. A conflict closes the form,
// the card reads the household from nothing and one focused line says so.
describe("a household save someone else's change refused", () => {
  const kid: HouseholdPerson = { ...household.people[0], kind: "child", linkStatus: "pending", canConvert: false, registrationEligibility: "eligible", careNotes: "Peanuts" };
  const mary: HouseholdPerson = { ...kid, id: 6, firstName: "Mary", lastName: "Smith", name: "Mary Smith", kind: "adult", birthDate: "1990-01-01", careNotes: "", linkStatus: "none", revision: "p-6" };
  const home: Household = { ...household, people: [kid] };
  const personChanged = () => new ApiError(409, "changed", { code: "household_person_changed" });
  const householdChanged = () => new ApiError(409, "changed", { code: "household_changed" });
  const lost = () => new ApiError(502, "bad gateway");
  const answered = (p: HouseholdPerson) => ({ person: p });

  beforeEach(() => vi.resetAllMocks());

  async function editFirstName(name: string, to: string) {
    await userEvent.click(await screen.findByRole("button", { name: `Edit ${name}` }));
    await userEvent.clear(screen.getByLabelText("First name"));
    await userEvent.type(screen.getByLabelText("First name"), to);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
  }

  it("sends the revision it read and saves", async () => {
    const { Households } = await import("../api/households");
    mine.mockResolvedValue({ household: home });
    vi.mocked(Households.updatePerson).mockResolvedValue(answered({ ...kid, firstName: "Kit", revision: "p-5b" }));
    render(<HouseholdCard />);
    await editFirstName("Kid Smith", "Kit");
    expect(await screen.findByRole("status")).toHaveTextContent("Kit updated.");
    expect(Households.updatePerson).toHaveBeenCalledWith(5, expect.objectContaining({ firstName: "Kit" }), "p-5");
    expect(Households.updatePerson).toHaveBeenCalledTimes(1);
  });

  it("closes the form, shows the person as stored and says so, with focus on the line", async () => {
    const { Households } = await import("../api/households");
    const theirs = { ...kid, careNotes: "Peanuts and dairy", revision: "p-5b" };
    mine.mockResolvedValueOnce({ household: home }).mockResolvedValue({ household: { ...home, people: [theirs] } });
    vi.mocked(Households.updatePerson).mockRejectedValue(personChanged());
    render(<HouseholdCard />);
    await editFirstName("Kid Smith", "Kit");
    const line = await screen.findByText("This household changed while you were editing, so your changes could not be saved. This is the household as it is now.");
    await vi.waitFor(() => expect(line).toHaveFocus());
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
    expect(screen.getByText("Care notes: Peanuts and dairy")).toBeInTheDocument();
    expect(screen.queryByText("Kit")).not.toBeInTheDocument();
    expect(Households.updatePerson).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(/close|reopen|reload/i)).not.toBeInTheDocument();
    // Opening the form again starts from the person as stored, and the line goes.
    await userEvent.click(screen.getByRole("button", { name: "Edit Kid Smith" }));
    expect(screen.getByLabelText(/^Care notes/)).toHaveValue("Peanuts and dairy");
    expect(screen.queryByText(/This household changed/)).not.toBeInTheDocument();
  });

  it("names the person when the card lists several", async () => {
    const { Households } = await import("../api/households");
    mine.mockResolvedValueOnce({ household: { ...home, people: [kid, mary] } })
      .mockResolvedValue({ household: { ...home, people: [{ ...kid, lastName: "Jones", name: "Kid Jones", revision: "p-5b" }, mary] } });
    vi.mocked(Households.updatePerson).mockRejectedValue(personChanged());
    render(<HouseholdCard />);
    await editFirstName("Kid Smith", "Kit");
    expect(await screen.findByText("Kid Jones. This household changed while you were editing, so your changes could not be saved. This is the household as it is now.")).toBeInTheDocument();
  });

  it("says the changes could not be saved when the person is no longer in the household", async () => {
    const { Households } = await import("../api/households");
    mine.mockResolvedValueOnce({ household: { ...home, people: [kid, mary] } }).mockResolvedValue({ household: { ...home, people: [mary] } });
    vi.mocked(Households.updatePerson).mockRejectedValue(new ApiError(404, "household person not found"));
    render(<HouseholdCard />);
    await editFirstName("Kid Smith", "Kit");
    expect(await screen.findByText("Kid Smith. Your changes could not be saved. This is the household as it is now.")).toBeInTheDocument();
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
  });

  it("sends the same save again under the new revision when nothing it writes changed", async () => {
    const { Households } = await import("../api/households");
    mine.mockResolvedValueOnce({ household: home }).mockResolvedValue({ household: { ...home, people: [{ ...kid, revision: "p-5b" }] } });
    vi.mocked(Households.updatePerson).mockRejectedValueOnce(personChanged()).mockResolvedValueOnce(answered({ ...kid, firstName: "Kit", revision: "p-5c" }));
    render(<HouseholdCard />);
    await editFirstName("Kid Smith", "Kit");
    expect(await screen.findByRole("status")).toHaveTextContent("Kit updated.");
    expect(Households.updatePerson).toHaveBeenNthCalledWith(1, 5, expect.objectContaining({ firstName: "Kit" }), "p-5");
    expect(Households.updatePerson).toHaveBeenNthCalledWith(2, 5, expect.objectContaining({ firstName: "Kit" }), "p-5b");
    expect(screen.queryByText(/could not be saved/)).not.toBeInTheDocument();
  });

  it("ends as saved when a send whose answer was lost is what the read shows", async () => {
    const { Households } = await import("../api/households");
    mine.mockResolvedValueOnce({ household: home }).mockResolvedValue({ household: { ...home, people: [{ ...kid, firstName: "Kit", name: "Kit Smith", revision: "p-5b" }] } });
    vi.mocked(Households.updatePerson).mockRejectedValueOnce(lost()).mockRejectedValueOnce(personChanged());
    render(<HouseholdCard />);
    await editFirstName("Kid Smith", "Kit");
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("status")).toHaveTextContent("Kit updated.");
    expect(Households.updatePerson).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
  });

  it("names the last save's revision on the next one", async () => {
    const { Households } = await import("../api/households");
    mine.mockResolvedValueOnce({ household: home }).mockResolvedValue({ household: { ...home, people: [{ ...kid, firstName: "Kit", name: "Kit Smith", revision: "p-5b" }] } });
    vi.mocked(Households.updatePerson).mockResolvedValue(answered({ ...kid, firstName: "Kit", revision: "p-5b" }));
    render(<HouseholdCard />);
    await editFirstName("Kid Smith", "Kit");
    await screen.findByText("Kit Smith");
    await editFirstName("Kit Smith", "Kitty");
    await vi.waitFor(() => expect(Households.updatePerson).toHaveBeenCalledTimes(2));
    expect(Households.updatePerson).toHaveBeenNthCalledWith(2, 5, expect.objectContaining({ firstName: "Kitty" }), "p-5b");
  });

  it("is a conflict, never a second send, when it writes care notes this person can't read", async () => {
    const { Households } = await import("../api/households");
    const hidden = { ...kid, careNotes: undefined };
    mine.mockResolvedValueOnce({ household: { ...home, people: [hidden] } }).mockResolvedValue({ household: { ...home, people: [{ ...hidden, revision: "p-5b" }] } });
    vi.mocked(Households.updatePerson).mockRejectedValue(personChanged());
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit Kid Smith" }));
    await userEvent.type(screen.getByLabelText(/^Care notes/), "Bring the inhaler");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/This household changed while you were editing/)).toBeInTheDocument();
    expect(Households.updatePerson).toHaveBeenCalledTimes(1);
  });

  it("sends again when the care notes it can't read are left alone", async () => {
    const { Households } = await import("../api/households");
    const hidden = { ...kid, careNotes: undefined };
    mine.mockResolvedValueOnce({ household: { ...home, people: [hidden] } }).mockResolvedValue({ household: { ...home, people: [{ ...hidden, revision: "p-5b" }] } });
    vi.mocked(Households.updatePerson).mockRejectedValueOnce(personChanged()).mockResolvedValueOnce(answered({ ...hidden, firstName: "Kit", revision: "p-5c" }));
    render(<HouseholdCard />);
    await editFirstName("Kid Smith", "Kit");
    expect(await screen.findByRole("status")).toHaveTextContent("Kit updated.");
    expect(Households.updatePerson).toHaveBeenNthCalledWith(2, 5, expect.objectContaining({ firstName: "Kit", careNotes: undefined }), "p-5b");
  });

  it("hands nothing on when its answer comes after the card was left", async () => {
    const { Households } = await import("../api/households");
    let answer: (r: { person: HouseholdPerson }) => void = () => {};

    mine.mockResolvedValue({ household: home });
    vi.mocked(Households.updatePerson).mockReturnValue(new Promise((resolve) => { answer = resolve; }));
    const { unmount } = render(<HouseholdCard />);
    await editFirstName("Kid Smith", "Kit");
    unmount();
    render(<HouseholdCard />);
    expect(await screen.findByRole("button", { name: "Edit Kid Smith" })).toBeEnabled();
    answer(answered({ ...kid, firstName: "Kit", revision: "p-5b" }));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(mine).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("status")).toHaveTextContent("");
    expect(screen.getByRole("button", { name: "Edit Kid Smith" })).toBeEnabled();
  });

  it("offers nothing that opens another form or reads the household while a form is open", async () => {
    mine.mockResolvedValue({ household: { ...home, people: [kid, { ...mary, canConvert: true }] } });
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit Kid Smith" }));

    for (const name of ["Edit Mary Smith", "Remove Mary Smith", "Rename", "Edit address", "Add a household member", "Create their own account"]) {
      expect(screen.getByRole("button", { name })).toBeDisabled();
    }
  });

  it("closes the household form on a conflict and shows the household as stored", async () => {
    const { Households } = await import("../api/households");
    mine.mockResolvedValueOnce({ household: home }).mockResolvedValue({ household: { ...home, name: "The Smiths", address: "2 Elm St", revision: "h-1b" } });
    vi.mocked(Households.update).mockRejectedValue(householdChanged());
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit address" }));
    await userEvent.type(screen.getByLabelText(/^Address/), "1 Oak St");
    await userEvent.click(screen.getByRole("button", { name: "Save household" }));
    const line = await screen.findByText("This household changed while you were editing, so your changes could not be saved. This is the household as it is now.");
    await vi.waitFor(() => expect(line).toHaveFocus());
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
    expect(screen.getByText("The Smiths")).toBeInTheDocument();
    expect(screen.getByText("2 Elm St")).toBeInTheDocument();
    expect(Households.update).toHaveBeenCalledTimes(1);
    expect(Households.update).toHaveBeenCalledWith({ name: "Smith household", address: "1 Oak St" }, "h-1");
  });

  it("sends the household save again when only someone's person edit moved its revision, then names the answer's", async () => {
    const { Households } = await import("../api/households");
    mine.mockResolvedValueOnce({ household: home }).mockResolvedValue({ household: { ...home, revision: "h-1b" } });
    vi.mocked(Households.update).mockRejectedValueOnce(householdChanged())
      .mockResolvedValueOnce({ household: { ...home, address: "1 Oak St", revision: "h-1c" } })
      .mockResolvedValueOnce({ household: { ...home, name: "Smiths", address: "1 Oak St", revision: "h-1d" } });
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit address" }));
    await userEvent.type(screen.getByLabelText(/^Address/), "1 Oak St");
    await userEvent.click(screen.getByRole("button", { name: "Save household" }));
    expect(await screen.findByText("1 Oak St")).toBeInTheDocument();
    expect(Households.update).toHaveBeenNthCalledWith(2, { name: "Smith household", address: "1 Oak St" }, "h-1b");
    await userEvent.click(screen.getByRole("button", { name: "Rename" }));
    await userEvent.clear(screen.getByLabelText(/^Household name/));
    await userEvent.type(screen.getByLabelText(/^Household name/), "Smiths");
    await userEvent.click(screen.getByRole("button", { name: "Save household" }));
    expect(await screen.findByText("Smiths")).toBeInTheDocument();
    expect(Households.update).toHaveBeenNthCalledWith(3, { name: "Smiths", address: "1 Oak St" }, "h-1c");
  });

  it("ends the household save as saved when a lost send is what the read shows", async () => {
    const { Households } = await import("../api/households");
    mine.mockResolvedValueOnce({ household: home }).mockResolvedValue({ household: { ...home, address: "1 Oak St", revision: "h-1b" } });
    vi.mocked(Households.update).mockRejectedValueOnce(lost()).mockRejectedValueOnce(householdChanged());
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit address" }));
    await userEvent.type(screen.getByLabelText(/^Address/), "1 Oak St");
    await userEvent.click(screen.getByRole("button", { name: "Save household" }));
    await screen.findByRole("alert");
    await userEvent.click(screen.getByRole("button", { name: "Save household" }));
    expect(await screen.findByText("Household updated.")).toBeInTheDocument();
    expect(screen.getByText("1 Oak St")).toBeInTheDocument();
    expect(Households.update).toHaveBeenCalledTimes(2);
  });

  it("asks before removing, saying their upcoming sign-ups are cancelled", async () => {
    const { Households } = await import("../api/households");
    const ask = vi.spyOn(window, "confirm").mockReturnValue(false);
    mine.mockResolvedValue({ household: { ...home, people: [kid, { ...mary, conversion: { email: "mary@example.com", expiresAt: "2026-12-01T12:00:00Z", state: "pending" as const } }] } });
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Remove Kid Smith" }));
    expect(ask).toHaveBeenLastCalledWith("Remove Kid Smith from your household? Any upcoming sign-ups for them will be cancelled.");
    await userEvent.click(screen.getByRole("button", { name: "Remove Mary Smith" }));
    expect(ask).toHaveBeenLastCalledWith("Remove Mary Smith from your household? Any upcoming sign-ups for them will be cancelled. This also cancels their account invitation.");
    expect(Households.removePerson).not.toHaveBeenCalled();
    ask.mockRestore();
  });
});

describe("household actions on their way", () => {
  beforeEach(() => vi.resetAllMocks());

  it("offers no other form or action while a removal is on its way", async () => {
    const { Households } = await import("../api/households");
    const ask = vi.spyOn(window, "confirm").mockReturnValue(true);
    let answer: () => void = () => {};
    const mary = { ...household.people[0], id: 6, firstName: "Mary", name: "Mary Smith", revision: "p-6" };

    mine.mockResolvedValue({ household: { ...household, people: [household.people[0], mary] } });
    vi.mocked(Households.removePerson).mockReturnValue(new Promise((resolve) => { answer = () => resolve({}); }));
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Remove Kid Smith" }));
    expect(screen.getByRole("button", { name: "Edit Mary Smith" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Rename" })).toBeDisabled();
    answer();
    await vi.waitFor(() => expect(screen.getByRole("button", { name: "Edit Mary Smith" })).toBeEnabled());
    ask.mockRestore();
  });

  it("hands nothing on when the household save's answer comes after the card was left", async () => {
    const { Households } = await import("../api/households");
    let answer: (r: { household: Household }) => void = () => {};
    const home: Household = { ...household, people: [] };

    mine.mockResolvedValue({ household: home });
    vi.mocked(Households.update).mockReturnValue(new Promise((resolve) => { answer = resolve; }));
    const { unmount } = render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Rename" }));
    await userEvent.type(screen.getByLabelText(/^Household name/), " home");
    await userEvent.click(screen.getByRole("button", { name: "Save household" }));
    unmount();
    render(<HouseholdCard />);
    expect(await screen.findByText("Smith household")).toBeInTheDocument();
    answer({ household: { ...home, name: "Smith household home", revision: "h-2" } });
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByText("Smith household home")).not.toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("");
  });
});

describe("household edits after a person became their own account, and invitations on their way", () => {
  beforeEach(() => vi.resetAllMocks());

  it("closes the form and says so when the person now has their own account", async () => {
    const { Households } = await import("../api/households");
    const mary = { ...household.people[0], id: 6, firstName: "Mary", name: "Mary Smith", kind: "adult" as const, birthDate: "1990-01-01", linkStatus: "none" as const, revision: "p-6" };

    mine.mockResolvedValueOnce({ household: { ...household, people: [household.people[0], mary] } })
      .mockResolvedValue({ household: { ...household, people: [household.people[0]] } });
    vi.mocked(Households.updatePerson).mockRejectedValue(new ApiError(409, "has an account", { code: "household_person_converted" }));
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit Mary Smith" }));
    await userEvent.clear(screen.getByLabelText("First name"));
    await userEvent.type(screen.getByLabelText("First name"), "Maria");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const line = await screen.findByText("Mary Smith. Your changes could not be saved. This is the household as it is now.");
    await vi.waitFor(() => expect(line).toHaveFocus());
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit Mary Smith" })).not.toBeInTheDocument();
    expect(Households.updatePerson).toHaveBeenCalledTimes(1);
  });

  it("keeps the card locked after an invitation is sent until the card shows it", async () => {
    const mary: HouseholdPerson = { ...household.people[0], id: 6, firstName: "Mary", name: "Mary Smith", kind: "adult", birthDate: "1990-01-01", linkStatus: "none", canConvert: true, registrationEligibility: "eligible", revision: "p-6" };
    let refreshed: (answer: { household: Household }) => void = () => {};
    const pending = { ...mary, conversion: { email: "mary@example.com", expiresAt: "2026-12-01T12:00:00Z", state: "pending" as const } };

    mine.mockResolvedValueOnce({ household: { ...household, people: [mary] } })
      .mockReturnValueOnce(new Promise((resolve) => { refreshed = resolve; }));
    convert.mockResolvedValue({});
    render(<HouseholdCard />);
    await userEvent.click(await screen.findByRole("button", { name: "Create their own account" }));
    await userEvent.type(screen.getByLabelText("Their email address"), "mary@example.com");
    await userEvent.click(screen.getByRole("button", { name: "Send account invitation" }));
    await vi.waitFor(() => expect(mine).toHaveBeenCalledTimes(2));
    expect(screen.getByRole("button", { name: "Edit Mary Smith" })).toBeDisabled();
    refreshed({ household: { ...household, people: [pending] } });
    expect(await screen.findByText(/Invitation pending: mary@example.com/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Edit Mary Smith" })).toBeEnabled();
  });
});
