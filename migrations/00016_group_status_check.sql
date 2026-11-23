-- +goose Up
-- groups.status holds one of three values: pending, active, archived
-- (docs/agents/lifecycles.md). Every release, the previous one included,
-- writes only these, so a previous binary keeps working on this schema. NULL
-- stays allowed: the column's nullability is not part of this change.
--
-- A plain ADD CONSTRAINT checks every existing row under ACCESS EXCLUSIVE on
-- groups. goose runs the file in one transaction, so NOT VALID and VALIDATE
-- would hold the same lock to the same commit. A row holding any other value
-- fails Up, which rolls back whole and leaves the version at 15. lock_timeout
-- bounds the lock wait.
SET LOCAL lock_timeout = '5s';
ALTER TABLE groups ADD CONSTRAINT groups_status_check
    CHECK (status IN ('pending', 'active', 'archived'));

-- +goose Down
-- Drops the CHECK only and changes no row.
SET LOCAL lock_timeout = '5s';
ALTER TABLE groups DROP CONSTRAINT groups_status_check;
