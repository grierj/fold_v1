-- +goose Up
-- What a push service answers about a device changes that device only as it
-- was dispatched (dispatchedDevice, push_deliver.go). binding_generation is
-- the part of that identity that cannot repeat: it goes up by one whenever an
-- UPDATE changes the owner or either key, so a browser handed to another
-- account and back, or renewed with new keys and then the old pair again,
-- never matches an answer about an earlier binding. A clock reading can repeat
-- (a save keeps created_at, and now() is sampled before the row lock), so no
-- timestamp stands in for it.
--
-- The trigger keeps it, not Go: every writer advances it, including a previous
-- binary that does not know the column, and a value a writer supplies is
-- overwritten. An UPDATE that changes neither the owner nor a key keeps the
-- stored value. An INSERT reads the default 1, and ids are never reused, so
-- (id, binding_generation) names one binding for good.
--
-- Up backfills nothing. ADD COLUMN with a constant default does not rewrite the
-- table. lock_timeout bounds the lock wait.
SET LOCAL lock_timeout = '5s';

ALTER TABLE push_subscriptions ADD COLUMN binding_generation bigint DEFAULT 1 NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION push_subscriptions_binding_generation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.user_id, NEW.p256dh, NEW.auth) IS DISTINCT FROM (OLD.user_id, OLD.p256dh, OLD.auth) THEN
        NEW.binding_generation := OLD.binding_generation + 1;
    ELSE
        NEW.binding_generation := OLD.binding_generation;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER push_subscriptions_binding_generation BEFORE UPDATE ON push_subscriptions
    FOR EACH ROW EXECUTE FUNCTION push_subscriptions_binding_generation();

-- +goose Down
-- Drops the counter only; no device row changes, even when populated. A binary
-- that reads the column must not run on the database afterwards.
SET LOCAL lock_timeout = '5s';
DROP TRIGGER push_subscriptions_binding_generation ON push_subscriptions;
DROP FUNCTION push_subscriptions_binding_generation();
ALTER TABLE push_subscriptions DROP COLUMN binding_generation;
