import { describe, expect, it } from "vitest";
import { changeSentence, visibilityRank } from "./visibilityChange";

describe("changeSentence", () => {
  it.each([
    ["event", "This event"],
    ["date", "This date"],
    ["group", "The group"],
  ] as const)("names %s as %s in every form", (subject, who) => {
    expect(changeSentence(subject, "public", "invite_only")).toBe(`${who} is public now. Saving makes it invite-only, so fewer people will be able to see it.`);
    expect(changeSentence(subject, "invite_only", "private")).toBe(`${who} is invite-only now. Saving makes it private, so more people will be able to see it.`);
    expect(changeSentence(subject, "private", "private")).toBe(`${who} is private now and will stay private.`);
  });
});

describe("visibilityRank", () => {
  it("orders public above private above invite-only", () => {
    expect(visibilityRank("public")).toBeGreaterThan(visibilityRank("private"));
    expect(visibilityRank("private")).toBeGreaterThan(visibilityRank("invite_only"));
  });
});
