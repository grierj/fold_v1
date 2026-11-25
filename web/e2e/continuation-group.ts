import * as schema from "valibot";
import type { APIRequestContext } from "@playwright/test";
import { apiAs } from "./helpers";

export async function continuationGroup(request: APIRequestContext) {
  const admin = await apiAs(request, "grier");

  const { group } = schema.parse(schema.looseObject({ group: schema.looseObject({ id: schema.number() }) }), (await admin("POST", "/groups", {
    name: "Continuation group", visibility: "public", tags: [],
  })));

  const dates = Array.from({ length: 55 }, (_, i) => new Date(Date.now() + (i + 2) * 864e5).toISOString().slice(0, 10));
  // All dates fit inside the upcoming window. Earlier hidden dates must not
  // consume the visible page's 25 slots or appear in its loaded counts.
  await admin("POST", `/groups/${group.id}/events`, {
    title: "Hidden daily series", startsLocal: `${dates[0]}T17:00`, durationMinutes: 60,
    tzid: "UTC", recurrenceRule: "FREQ=DAILY;COUNT=55", visibility: "invite_only",
  });

  const { event } = schema.parse(schema.looseObject({ event: schema.looseObject({ id: schema.number() }) }), (await admin("POST", `/groups/${group.id}/events`, {
    title: "Visible daily series", startsLocal: `${dates[0]}T19:00`, durationMinutes: 60,
    tzid: "UTC", recurrenceRule: "FREQ=DAILY;COUNT=55", visibility: "public",
  })));

  return { group, event, dates };
}
