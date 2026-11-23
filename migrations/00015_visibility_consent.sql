-- +goose Up
-- DEC-158. Keeps Fold builds without exact visibility consent off this
-- database, and inserts the consent epoch that `fold serve` rotates at start.
-- 1. Start fence. Every `fold serve` since the restore barrier writes the
--    restore_fence settings row in its boot transaction before it serves
--    (restore_fence.go). settings_recovery_floor refuses that write unless the
--    transaction declared fold.recovery_level 3 or more, which only builds with
--    exact consent do. An earlier build therefore exits at start.
-- 2. Audit fence. A build that was already running when this file was applied
--    can still serve. v0.1.0, the level-0 lane and this release record a
--    visibility cancellation with one audit row (registration.cancelled, reason
--    event_more_private), and every change to an event's or a date's visibility
--    with an event.visibility_changed or event.visibility_override row, in the
--    changing transaction, and return the insert's error.
--    audit_logs_visibility_consent refuses a cancellation row unless the
--    transaction passed a consent check (fold.visibility_consent 'v1'), and any
--    event visibility row unless it ran the consent rule ('v1' or
--    'v1:no-review'), so an earlier build changes no visibility at all.
-- 3. The hold. No Down removes either fence, on any database: a user count
--    cannot tell a scratch database from an install before its first setup.
--    The baseline Down drops settings and audit_logs with plain DROP TABLE,
--    which would take the triggers with them. recovery_floor_hold depends on
--    both tables, so that DROP fails and the baseline Down rolls back whole.
-- Up runs again after any Down, over everything the Down kept, so every
-- statement here is CREATE OR REPLACE or ON CONFLICT DO NOTHING.
-- Neither fence proves what a check compared; exactness is the server's
-- (internal/consent). Both settings are declarations, not secrets.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION settings_recovery_floor() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    declared text := coalesce(current_setting('fold.recovery_level', true), '');
BEGIN
    IF NEW.key = 'restore_fence' OR (TG_OP = 'UPDATE' AND OLD.key = 'restore_fence') THEN
        IF (CASE WHEN declared ~ '^[0-9]{1,4}$' THEN declared::int ELSE 0 END) < 3 THEN
            RAISE EXCEPTION 'this database needs Fold at recovery level 3 or later (exact visibility consent, DEC-158), and this Fold release is older; start the current release, and restore data only with its restore.sh --dump'
                USING ERRCODE = '55000';
        END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audit_logs_visibility_consent() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    declared text := coalesce(current_setting('fold.visibility_consent', true), '');
BEGIN
    IF declared = 'v1'
       OR (declared = 'v1:no-review' AND NEW.action <> 'registration.cancelled') THEN
        RETURN NULL;
    END IF;
    RAISE EXCEPTION 'a visibility change can be saved only by a Fold release that checks visibility consent (DEC-158), and can cancel registrations only after the exact change was reviewed'
        USING ERRCODE = '23514', CONSTRAINT = 'audit_logs_visibility_consent';
END $$;
-- +goose StatementEnd
CREATE OR REPLACE TRIGGER settings_recovery_floor BEFORE INSERT OR UPDATE ON settings
    FOR EACH ROW EXECUTE FUNCTION settings_recovery_floor();
CREATE OR REPLACE TRIGGER audit_logs_visibility_consent AFTER INSERT ON audit_logs
    FOR EACH ROW WHEN (
        (NEW.action = 'registration.cancelled' AND NEW.meta_json->>'reason' = 'event_more_private')
        OR NEW.action IN ('event.visibility_changed', 'event.visibility_override'))
    EXECUTE FUNCTION audit_logs_visibility_consent();
CREATE OR REPLACE VIEW recovery_floor_hold AS
    SELECT 1 AS held FROM settings, audit_logs WHERE false;
COMMENT ON VIEW recovery_floor_hold IS
    'DEC-158: keeps settings and audit_logs, which carry the recovery floor triggers, from being dropped by a Down. Always empty; nothing reads it.';
INSERT INTO settings (key, value) VALUES ('visibility_consent_epoch', gen_random_uuid()::text)
    ON CONFLICT (key) DO NOTHING;

-- +goose Down
-- DEC-158: this Down removes nothing, on any database, with users or without.
-- It moves only goose's version row, so an earlier build started after any
-- number of `fold migrate down-one` steps is still refused at start, and this
-- release's Up, run again at its next start, finds everything in place. Tests
-- that need a pristine schema remove the fences with releaseRecoveryFloor in
-- migrations/testhelpers_test.go, never through this file.
