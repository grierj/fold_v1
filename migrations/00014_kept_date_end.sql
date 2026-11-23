-- +goose Up
-- M8.1, DEC-156. A live grant that ends with its date (every origin but
-- manager_add, DEC-74) blocks any change that makes the date end later; the
-- manager removes that person first. The server refuses first with an answer
-- the manager can act on; this trigger refuses, in the database, an UPDATE that
-- raises a date's end while such a grant is unstamped in its snapshot. It reads
-- no clock, so it cannot tell a grant whose date has ended from a live one, and
-- a writer that stamps the grant first in the same transaction passes, so it
-- does not keep an earlier build from moving a date whose end has passed. At an
-- isolation level other than READ COMMITTED it refuses every raise, since its
-- EXISTS could read a snapshot older than an issuer's commit. The guarantee
-- covers writers that lock the date row before making a departure grant live,
-- as Fold's departure does; it claims nothing about a writer that does not. It
-- stores and backfills nothing; Down drops it.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE FUNCTION event_occurrences_kept_date_end() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'a date may end later only at read committed'
            USING ERRCODE = '23514', CONSTRAINT = 'event_occurrences_kept_date_end';
    END IF;
    IF EXISTS (SELECT 1 FROM registrations r
        WHERE r.occurrence_id = NEW.id AND r.user_id IS NOT NULL
          AND r.access_origin <> '' AND r.access_origin <> 'manager_add'
          AND r.access_granted_at IS NOT NULL AND r.access_ended_at IS NULL) THEN
        RAISE EXCEPTION 'a date kept after leaving cannot end later while that access is live'
            USING ERRCODE = '23514', CONSTRAINT = 'event_occurrences_kept_date_end';
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER event_occurrences_kept_date_end AFTER UPDATE ON event_occurrences
    FOR EACH ROW WHEN (NEW.ends_at > OLD.ends_at)
    EXECUTE FUNCTION event_occurrences_kept_date_end();

-- +goose Down
-- Drops the guard only; no row changes. The server's own check still refuses.
SET LOCAL lock_timeout = '5s';
DROP TRIGGER event_occurrences_kept_date_end ON event_occurrences;
DROP FUNCTION event_occurrences_kept_date_end();
