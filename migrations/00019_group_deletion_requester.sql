-- +goose Up
-- A group remembers who opened its current deletion request even when there
-- was no other admin to receive a task. Older requests start without an id.
SET LOCAL lock_timeout = '5s';

ALTER TABLE groups ADD COLUMN deletion_requester_id bigint REFERENCES users(id) ON DELETE SET NULL;

-- A previous binary can close and reopen a request, or soft-delete its group.
-- Clear the id on close so a later request cannot inherit it.
-- +goose StatementBegin
CREATE FUNCTION clear_closed_group_requester() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.deletion_requester_id := NULL;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER groups_clear_closed_requester BEFORE INSERT OR UPDATE ON groups
    FOR EACH ROW WHEN (NEW.deletion_requester_id IS NOT NULL AND (NOT NEW.deletion_requested OR NEW.deleted_at IS NOT NULL))
    EXECUTE FUNCTION clear_closed_group_requester();

-- Account erasure soft-deletes the user, including when a previous binary runs it.
-- The requester may already have left the group and need no leadership handoff.
-- +goose StatementBegin
CREATE FUNCTION erase_group_requester() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE groups SET deletion_requester_id = NULL, updated_at = clock_timestamp() WHERE deletion_requester_id = NEW.id;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER users_erase_group_requester AFTER UPDATE OF deleted_at ON users
    FOR EACH ROW WHEN (OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL)
    EXECUTE FUNCTION erase_group_requester();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER users_erase_group_requester ON users;
DROP FUNCTION erase_group_requester();
DROP TRIGGER groups_clear_closed_requester ON groups;
DROP FUNCTION clear_closed_group_requester();
ALTER TABLE groups DROP COLUMN deletion_requester_id;
