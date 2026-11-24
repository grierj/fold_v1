-- +goose Up
-- The group form names the read it was made from (DEC-163, DEC-166): a save
-- made from an older read of the group is refused. groups.edit_revision is a
-- counter the trigger below keeps, not Go, so every writer advances it,
-- including a previous binary that does not know the column and the cover
-- endpoints, which write image_url outside the form's save.
--
-- Any UPDATE that changes a tracked column adds one; an UPDATE that changes
-- none keeps the stored value, whatever it wrote. Every column is tracked
-- except the bookkeeping ones:
--   updated_at     GORM stamps it on every write, one that changes nothing
--                  included, so it says nothing about what a read showed.
--   edit_revision  the counter itself; a writer that names it is ignored.
-- leader_id, status, deletion_requested and deleted_at stay tracked: they
-- are what other people do to the group, and the form that opened before
-- them reads again and sends once more when nothing it writes has changed.
-- An INSERT reads the default 1. Restore safety does not come from these
-- numbers: the server binds every token to a per-process generation, so a
-- restored or rolled-back database is never matched against a token from
-- before the restart.
--
-- Up backfills nothing. ADD COLUMN with a constant default does not rewrite
-- the table. lock_timeout bounds the lock wait.
SET LOCAL lock_timeout = '5s';

ALTER TABLE groups ADD COLUMN edit_revision bigint DEFAULT 1 NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION groups_edit_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW) - ARRAY['updated_at','edit_revision'])
       IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['updated_at','edit_revision']) THEN
        NEW.edit_revision := OLD.edit_revision + 1;
    ELSE
        NEW.edit_revision := OLD.edit_revision;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER groups_edit_revision BEFORE UPDATE ON groups
    FOR EACH ROW EXECUTE FUNCTION groups_edit_revision();

-- +goose Down
-- Drops the counter and its trigger only; no group row changes. Every token
-- an open form holds is refused afterwards anyway, because the process that
-- runs a Down is not the one that minted it.
SET LOCAL lock_timeout = '5s';
DROP TRIGGER groups_edit_revision ON groups;
DROP FUNCTION groups_edit_revision();
ALTER TABLE groups DROP COLUMN edit_revision;
