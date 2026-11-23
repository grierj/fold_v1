-- +goose Up
-- M8.1 date page, DEC-155: the clock evidence the series writer produced for
-- each date, so the date page compares a date with its series without walking
-- the recurrence rule.
--
-- occurrence_series_starts holds, per occurrence, every distinct instant the
-- writer's own expansion generated for that original date (one for an
-- ordinary date, one per time on a genuinely multi-time day), with the first
-- in the writer's order marked is_first. A kept date the rule provably does
-- not generate has one row: the series' written wall clock on that day
-- (written_local) and the instant it resolves to. tzid is the zone the
-- instants were generated in, which the page compares in. No rows means the
-- clock is unknown. The rows are derived scheduling data owned by the date
-- (ON DELETE CASCADE) and hold no person, room or group data.
--
-- events.series_clock_dirty is cache invalidation for a binary that predates
-- this table. Any UPDATE that names starts_local, tzid, rrule or rrule_until
-- sets it, including a full row save where those values compare equal: the
-- previous binary's series edit saves the whole row and may retime dates of a
-- multi-time or zero-DTSTART rule without changing them. The current binary
-- clears it with an UPDATE of that column alone, after it has replaced or
-- discarded the evidence in the same transaction, and the daily
-- occurrences.extend job repairs what a previous binary left. The trigger
-- writes no audit row and reads no child row.
--
-- Up backfills nothing and changes no occurrence: every existing date reads
-- unknown until the repair job runs. ADD COLUMN with a constant default does
-- not rewrite events. lock_timeout bounds each lock wait.
--
-- Down drops the trigger, its function, the flag and the table. The evidence
-- for past dates and for zero-DTSTART series cannot be rebuilt by the repair,
-- so a Down followed by Up loses it; only a backup taken before the Down
-- brings those values back (docs/DEPLOY-DIGITALOCEAN.md section 6).
SET LOCAL lock_timeout = '5s';

ALTER TABLE events ADD COLUMN series_clock_dirty boolean DEFAULT false NOT NULL;

CREATE TABLE occurrence_series_starts (
    occurrence_id bigint NOT NULL REFERENCES event_occurrences (id) ON DELETE CASCADE,
    starts_at timestamp with time zone NOT NULL,
    is_first boolean DEFAULT false NOT NULL,
    tzid text NOT NULL,
    written_local text,
    PRIMARY KEY (occurrence_id, starts_at),
    CONSTRAINT occurrence_series_starts_written_local_check
        CHECK (written_local IS NULL OR (is_first AND written_local ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}$'))
);
CREATE UNIQUE INDEX idx_occurrence_series_starts_first ON occurrence_series_starts (occurrence_id) WHERE is_first;

-- +goose StatementBegin
CREATE FUNCTION events_series_clock_dirty() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.series_clock_dirty := true;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER events_series_clock_dirty BEFORE UPDATE OF starts_local, tzid, rrule, rrule_until ON events
    FOR EACH ROW EXECUTE FUNCTION events_series_clock_dirty();

-- +goose Down
-- Drops derived evidence only; no date, event or registration changes.
SET LOCAL lock_timeout = '5s';
DROP TRIGGER events_series_clock_dirty ON events;
DROP FUNCTION events_series_clock_dirty();
DROP TABLE occurrence_series_starts;
ALTER TABLE events DROP COLUMN series_clock_dirty;
