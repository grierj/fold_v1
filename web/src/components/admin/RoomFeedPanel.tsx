import { useEffect, useRef, useState } from "react";
import { ApiError } from "../../api/core";
import { AdminRooms } from "../../api/rooms";
import { errorCode } from "../../lib/errors";
import { RefusedSaveLine, useRefusedSave } from "../../lib/refusedSave";
import { outcomeOf } from "../../lib/rosterWrites";
import type { Room } from "../../types";
import { Button } from "../ui/Button";
import { SecretFeedLink, useSecretFeed } from "../SecretFeedLink";

function feedGone(cause: unknown): boolean {
  return cause instanceof ApiError && !cause.unreadable && [401, 403, 404].includes(cause.status);
}

export function RoomFeedPanel({ room, onClose, onGone }: { room: Room; onClose: () => void; onGone: () => void }) {
  const headingRef = useRef<HTMLHeadingElement>(null);
  const uncertainRef = useRef<HTMLParagraphElement>(null);
  const refusal = useRefusedSave();
  const [uncertain, setUncertain] = useState(false);

  function start() {
    refusal.clear();
    setUncertain(false);
  }

  const feed = useSecretFeed({
    load: async () => (await AdminRooms.getFeed(room.id)).feed,
    deps: [room.id],
    create: async ({ revision }) => {
      start();

      return (await AdminRooms.makeFeed(room.id, revision)).feed;
    },
    revoke: async ({ revision }) => {
      start();

      return (await AdminRooms.revokeFeed(room.id, revision)).feed;
    },
    failed: {
      load: "We couldn't load this room's calendar link.",
      create: "We couldn't make the calendar link.",
      revoke: "We couldn't turn off the calendar link.",
    },
    // The button that was pressed is gone; keep keyboard users in the panel.
    onRevoked: () => headingRef.current?.focus(),
    recover: async (cause, retry) => {
      if (feedGone(cause)) {
        onGone();

        return true;
      }

      const changed = errorCode(cause) === "room_feed_changed";

      if (!changed && outcomeOf(cause) !== "unsure") return false;

      if (!changed) setUncertain(true);

      const read = await retry();

      if (read === "loaded" && changed) refusal.refused("changed");

      return !changed || read !== "failed";
    },
  });

  // The button that opened the panel is gone, so focus starts on its heading.
  useEffect(() => { headingRef.current?.focus(); }, [room.id]);

  const { loaded, loadError, loadCause } = feed;
  const gone = feedGone(loadCause);
  useEffect(() => {
    if (gone) onGone();
  }, [gone, onGone]);
  useEffect(() => {
    if (uncertain) uncertainRef.current?.focus();
  }, [uncertain, loaded, loadError]);

  if (gone) return null;

  return (
    <section aria-labelledby={`room-feed-${room.id}`} className="bg-surface rounded-lg border border-border p-6 space-y-4 max-w-xl">
      <h2 id={`room-feed-${room.id}`} ref={headingRef} tabIndex={-1} className="text-lg font-bold focus:outline-none">
        Calendar feed for {room.name}
      </h2>
      <p className="text-sm text-text-muted">
        Anyone with this link can see when this room is booked. Dates you can see when signed out
        include the event title and link. Other bookings, including members-only and invite-only
        dates, appear as Reserved. In a calendar app, choose Add calendar &rarr; From URL (or
        Subscribe) and paste the link.
      </p>
      <RefusedSaveLine save={refusal} noun="calendar link status" choosing="This calendar link" />
      {uncertain && (
        <p ref={uncertainRef} role="status" tabIndex={-1} className="rounded-lg border border-border bg-sunken px-4 py-3 text-sm">
          We couldn&rsquo;t confirm whether your calendar link changed.{loaded ? " Its current status is shown below." : ""}
        </p>
      )}
      <SecretFeedLink
        feed={feed}
        id={`room-feed-link-${room.id}`}
        loadingLabel="Loading the room's calendar link…"
        linkLabel="Calendar link"
        linkHint="Save this link now; we won't show it again. Making a new link stops the old one."
        setUp={(madeOn) => <>
          A calendar link is on{madeOn ? `, made ${madeOn}` : ""}. We can&rsquo;t show it
          again, so make a new one if you need to set it up somewhere else.
        </>}
        none={<>This room doesn&rsquo;t have a calendar link yet.</>}
        confirmRemake={`Make a new calendar link for ${room.name}? The old link will stop working.`}
        confirmOff={`Turn off the calendar link for ${room.name}? Calendar apps using it will stop updating.`}
        after={<Button variant="ghost" layout="w-full sm:w-auto" onClick={onClose}>Done</Button>}
      />
    </section>
  );
}
