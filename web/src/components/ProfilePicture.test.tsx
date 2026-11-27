import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { ApiError } from "../api/core";
import { Profile } from "../api/profile";
import { testUser } from "../test/fixtures";
import { deferred } from "../test/keptPanel";
import { stubService } from "../test/service";
import type { User } from "../types";
import { ProfilePicture } from "./ProfilePicture";

stubService(Profile, () => ({ update: vi.fn() }));

const user = testUser({ avatarUrl: "/uploads/avatars/1/one.png", avatarFocalX: 50, avatarFocalY: 60 });

function renderPicture(read: () => User) {
  function Editor() {
    const [shown, setShown] = useState(user);

    return <>
      <ProfilePicture user={shown} refresh={async () => { setShown(read()); }} />
      <button>Other setting</button>
    </>;
  }

  return render(<Editor />);
}

it("shows the latest adjustment at once and persists it after the first request settles", async () => {
  const first = deferred<void>();
  let stored = user;

  const update = vi.mocked(Profile.update).mockReset().mockImplementation(async ({ avatarFocal }) => {
    if (avatarFocal?.x === 51) await first.promise;
    stored = { ...stored, avatarFocalX: avatarFocal?.x ?? 0, avatarFocalY: avatarFocal?.y ?? 0 };

    return { user: stored };
  });

  const view = renderPicture(() => stored);
  const left = screen.getByRole("slider", { name: "Focal point from left" });
  const top = screen.getByRole("slider", { name: "Focal point from top" });

  fireEvent.change(left, { target: { value: "51" } });
  await waitFor(() => expect(update).toHaveBeenCalledExactlyOnceWith({ avatarFocal: { x: 51, y: 60, avatarUrl: user.avatarUrl } }));
  fireEvent.change(top, { target: { value: "72" } });
  fireEvent.change(left, { target: { value: "50" } });

  expect(top).toHaveAttribute("aria-valuetext", "72% from the top");
  expect(view.container.querySelector("img")).toHaveStyle({ objectPosition: "50% 72%" });
  expect(screen.getByRole("button", { name: "Remove picture" })).toBeDisabled();
  expect(update).toHaveBeenCalledTimes(1);

  await act(async () => { first.resolve(); });
  expect(update.mock.calls.slice(1)).toEqual([[{ avatarFocal: { x: 50, y: 72, avatarUrl: user.avatarUrl } }]]);
  expect(await screen.findByText("Picture saved.")).toBeInTheDocument();
  expect(view.container.querySelector("img")).toHaveStyle({ objectPosition: "50% 72%" });
});

it.each([
  { change: "replaced", focus: "left", expected: "left" },
  { change: "replaced", focus: "top", expected: "top" },
  { change: "removed", focus: "top", expected: "chooser" },
  { change: "replaced", focus: "other", expected: "other" },
])("keeps focus on $expected when a save names a picture that was $change", async ({ change, focus, expected }) => {
  const replacement = { ...user, avatarUrl: change === "removed" ? "" : "/uploads/avatars/1/two.png", avatarFocalX: 22 };
  const first = deferred<{ user: User }>();
  const update = vi.mocked(Profile.update).mockReset().mockReturnValue(first.promise);
  const view = renderPicture(() => replacement);
  const left = () => screen.getByRole("slider", { name: "Focal point from left" });
  const top = () => screen.getByRole("slider", { name: "Focal point from top" });
  const other = screen.getByRole("button", { name: "Other setting" });

  left().focus();
  fireEvent.change(left(), { target: { value: "90" } });
  const focused = focus === "other" ? other : focus === "top" ? top() : left();
  focused.focus();
  expect(focused).toHaveFocus();
  await act(async () => { first.reject(new ApiError(409, "the profile picture changed", { code: "avatar_changed" })); });

  expect(await screen.findByText("Your profile picture changed before its position could be saved.")).toBeInTheDocument();
  expect(update).toHaveBeenCalledExactlyOnceWith({ avatarFocal: { x: 90, y: 60, avatarUrl: user.avatarUrl } });

  if (change === "removed") {
    expect(screen.queryByRole("slider")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Choose photo" })).toHaveFocus();
  } else {
    expect(left()).toHaveValue("22");
    expect(top()).toHaveValue("60");
    expect(view.container.querySelector("img")).toHaveAttribute("src", replacement.avatarUrl);
    expect(view.container.querySelector("img")).toHaveStyle({ objectPosition: "22% 60%" });
    expect(expected === "other" ? other : expected === "top" ? top() : left()).toHaveFocus();
  }
});
