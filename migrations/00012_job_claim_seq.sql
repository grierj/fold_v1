-- +goose Up
-- A running job's outcome lands only for the claim that holds it
-- (jobs.Claim, internal/jobs/jobs.go). claim_seq is the part of that identity
-- that cannot repeat: the runner's claim UPDATE draws a new value from
-- jobs_claim_seq every time it takes a job, including a lease taken back. The
-- worker label can repeat (callers supply it; generated labels are host, PID
-- and index), and so can the attempt count (a wait gives its attempt back, an
-- admin's Retry starts it again), so neither stands in for it. A sequence never
-- hands out a number twice; it rewinds only with the database: restore.sh, which
-- stops the app first, or this file's Down, which is offline maintenance and
-- must run with every server stopped, so no claim from before can still be
-- running.
--
-- Up backfills nothing: a job running when the upgrade starts belongs to a
-- stopped process and lease recovery claims it again with a number. A previous
-- binary that does not know the column leaves it unchanged, and fences on
-- locked_by as it always did. ADD COLUMN without a default does not rewrite the
-- table. lock_timeout bounds the lock wait.
SET LOCAL lock_timeout = '5s';

CREATE SEQUENCE jobs_claim_seq AS bigint;
ALTER TABLE jobs ADD COLUMN claim_seq bigint;

-- +goose Down
-- Drops the claim numbers only; no job changes status, even when populated. Run
-- it with every server stopped (a claim held across it could match a number
-- the recreated sequence hands out again). A binary that reads the column must
-- not run on the database afterwards.
SET LOCAL lock_timeout = '5s';
ALTER TABLE jobs DROP COLUMN claim_seq;
DROP SEQUENCE jobs_claim_seq;
