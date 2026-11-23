import { stubService } from "../test/service";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, expect, it, vi } from "vitest";
import { Calendar } from "../api/calendar";
import type { CalendarOccurrence } from "../types";
import { ComingUp } from "./ComingUp";

stubService(Calendar, () => ({ list: vi.fn() }));

const soon = (hours: number) => new Date(Date.now() + hours * 3_600_000).toISOString();
const row = (o: Partial<CalendarOccurrence>): CalendarOccurrence => ({ eventId: 7, date: "2026-12-01", startsAt: soon(2), endsAt: soon(3), title: "Supper", location: "", churchWide: false, groupName: "Supper club", remoteAvailable: false, childcareAvailable: false, myStatus: "going", ownScope: true, ...o });

beforeEach(() => { vi.resetAllMocks(); });

it("asks Just mine and links each row where the calendar says: its date page when it carries dateUrl", async () => {
  vi.mocked(Calendar.list).mockResolvedValue({ truncated: false, occurrences: [
    row({ title: "Managed date", dateUrl: "/events/7/dates/2026-12-01" }),
    row({ eventId: 8, date: "2026-12-02", startsAt: soon(4), endsAt: soon(5), title: "Listed date" }),
  ] });
  render(<MemoryRouter><ComingUp /></MemoryRouter>);
  expect(await screen.findByRole("link", { name: /Managed date/ })).toHaveAttribute("href", "/events/7/dates/2026-12-01");
  expect(screen.getByRole("link", { name: /Listed date/ })).toHaveAttribute("href", "/events/8");
  expect(Calendar.list).toHaveBeenCalledWith(expect.objectContaining({ mine: true }));
});
