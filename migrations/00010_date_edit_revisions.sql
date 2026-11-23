-- +goose Up
-- M8.1 guarded date edit. A date edit names the revision of the date it was
-- based on and is refused when the date or its series changed since; a retried
-- edit is recognised by a keyed receipt.
--
-- edit_revision on events and event_occurrences is a counter the triggers
-- below keep, not Go, so every writer advances it, including a previous binary
-- that does not know these columns. Any UPDATE that changes a tracked column
-- (every column except the bookkeeping ones each trigger names) adds one; an
-- UPDATE that changes none of them keeps the stored value, whatever it wrote.
-- An INSERT reads the default 1. Restore safety does not come from these
-- numbers: the server binds every token and receipt to a per-process
-- generation, so a restored or rolled-back database is never matched against
-- a token from before the restart.
--
-- edit_receipt is a 32-byte HMAC under a per-process key that is never
-- stored. It holds no readable person id, reason or draft text. Any tracked
-- change to the date clears it, and it goes with the row.
--
-- Up backfills nothing. ADD COLUMN with a constant default does not rewrite
-- either table. lock_timeout bounds each lock wait.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events ADD COLUMN edit_revision bigint DEFAULT 1 NOT NULL;
ALTER TABLE event_occurrences
    ADD COLUMN edit_revision bigint DEFAULT 1 NOT NULL,
    ADD COLUMN edit_receipt bytea
        CONSTRAINT event_occurrences_edit_receipt_check
        CHECK (edit_receipt IS NULL OR octet_length(edit_receipt) = 32);

-- +goose StatementBegin
CREATE FUNCTION events_edit_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW) - ARRAY['updated_at','series_clock_dirty','edit_revision'])
       IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['updated_at','series_clock_dirty','edit_revision']) THEN
        NEW.edit_revision := OLD.edit_revision + 1;
    ELSE
        NEW.edit_revision := OLD.edit_revision;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER events_edit_revision BEFORE UPDATE ON events
    FOR EACH ROW EXECUTE FUNCTION events_edit_revision();

-- +goose StatementBegin
CREATE FUNCTION event_occurrences_edit_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW) - ARRAY['updated_at','edit_revision','edit_receipt'])
       IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['updated_at','edit_revision','edit_receipt']) THEN
        NEW.edit_revision := OLD.edit_revision + 1;
        NEW.edit_receipt := NULL;
    ELSE
        NEW.edit_revision := OLD.edit_revision;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER event_occurrences_edit_revision BEFORE UPDATE ON event_occurrences
    FOR EACH ROW EXECUTE FUNCTION event_occurrences_edit_revision();

-- +goose Down
-- Drops transient edit bookkeeping only; no date, event, registration or audit
-- row changes. Every token an open editor holds is refused afterwards anyway,
-- because the process that runs a Down is not the one that minted it.
SET LOCAL lock_timeout = '5s';
DROP TRIGGER event_occurrences_edit_revision ON event_occurrences;
DROP TRIGGER events_edit_revision ON events;
DROP FUNCTION event_occurrences_edit_revision();
DROP FUNCTION events_edit_revision();
ALTER TABLE event_occurrences DROP COLUMN edit_receipt, DROP COLUMN edit_revision;
ALTER TABLE events DROP COLUMN edit_revision;
