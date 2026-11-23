import { expect, it, vi } from "vitest";

const heard = (channel: BroadcastChannel) => {
  const got: unknown[] = [];
  channel.onmessage = (event) => got.push(event.data);

  return got;
};

// Another test file's channel, as Node's process-wide BroadcastChannel would carry it from another worker.
it("channels in one file hear each other, and a channel of the same bare name elsewhere in the process does not", async () => {
  const ProcessChannel: typeof BroadcastChannel = Object.getPrototypeOf(BroadcastChannel);
  expect(ProcessChannel).not.toBe(BroadcastChannel);
  const tab = new BroadcastChannel("fold-credential");
  const otherTab = new BroadcastChannel("fold-credential");
  const elsewhere = new ProcessChannel("fold-credential");
  const atTab = heard(tab);
  const atElsewhere = heard(elsewhere);

  try {
    // Sent first, so it would arrive before the one this file waits for.
    elsewhere.postMessage("another file");
    otherTab.postMessage("this file");
    await vi.waitFor(() => expect(atTab).toContain("this file"));
    expect(atTab).toEqual(["this file"]);
    expect(atElsewhere).toEqual([]);
  } finally {
    for (const channel of [tab, otherTab, elsewhere]) channel.close();
  }
});
