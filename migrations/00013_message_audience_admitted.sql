-- +goose Up
-- An announcement to an event audience may be read later (GET /messages/:id/read)
-- by a direct recipient a coherent audience check let in (DEC-135):
-- audience_admitted_at is when a writer that had just made that check, in one
-- snapshot, recorded the row's outcome (preparation, a claimed email row, an
-- announcement push). It is set once and never cleared; a retry, a webhook or
-- another device's outcome keeps it. A row a writer finished before or
-- without that check (an early skip, the last-attempt cleanup, the reaper)
-- leaves it NULL, and its person reads only while a date they may still see
-- puts them in the audience. A sent, delivered, bounced or complained row is a
-- delivery and stays readable whatever this says.
--
-- Up backfills nothing: no stored row proves which check it passed, so every
-- existing row reads NULL. A previous binary neither writes nor reads it, and
-- on a rollback its readers keep their own rule. ADD COLUMN without a default
-- does not rewrite the table. lock_timeout bounds the lock wait.
SET LOCAL lock_timeout = '5s';

ALTER TABLE message_recipients ADD COLUMN audience_admitted_at timestamptz;

-- +goose Down
-- Drops the marker only; no row changes status or reason, even when
-- populated. A binary that reads the column must not run on the database
-- afterwards.
SET LOCAL lock_timeout = '5s';
ALTER TABLE message_recipients DROP COLUMN audience_admitted_at;
