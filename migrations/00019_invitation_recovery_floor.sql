-- +goose Up
-- A level-3 build can replay 00015 after a Down. Its CREATE OR REPLACE
-- must never replace the floor for a database whose invitations were pruned.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION settings_recovery_floor_invitations() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    declared text := current_setting('fold.recovery_level', true);
BEGIN
    IF NEW.key = 'restore_fence' OR (TG_OP = 'UPDATE' AND OLD.key = 'restore_fence') THEN
        IF (CASE WHEN declared ~ '^[0-9]{1,4}$' THEN declared::int ELSE 0 END) < 4 THEN
            RAISE EXCEPTION 'this database needs Fold at recovery level 4 or later (invitation audit erasure); restore with the current release and its restore.sh --dump'
                USING ERRCODE = '55000';
        END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE OR REPLACE TRIGGER settings_recovery_floor_invitations BEFORE INSERT OR UPDATE ON settings
    FOR EACH ROW EXECUTE FUNCTION settings_recovery_floor_invitations();

-- +goose Down
-- Down cannot bring pruned invitations back. Keep the fence on settings,
-- which recovery_floor_hold already protects through every earlier Down.
