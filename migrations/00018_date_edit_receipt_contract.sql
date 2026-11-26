-- +goose Up
-- contracts: 00010_date_edit_revisions.sql
-- DEC-164 retired the receipt before v0.1.1. The trigger still keeps the
-- date's revision for every writer.
SET LOCAL lock_timeout = '5s';

ALTER TABLE event_occurrences DROP COLUMN edit_receipt;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION event_occurrences_edit_revision() RETURNS trigger LANGUAGE plpgsql AS $$
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

-- +goose Down
-- Receipts were tied to the process that wrote them, so restore only the column.
SET LOCAL lock_timeout = '5s';

ALTER TABLE event_occurrences
    ADD COLUMN edit_receipt bytea
        CONSTRAINT event_occurrences_edit_receipt_check
        CHECK (edit_receipt IS NULL OR octet_length(edit_receipt) = 32);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION event_occurrences_edit_revision() RETURNS trigger LANGUAGE plpgsql AS $$
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
