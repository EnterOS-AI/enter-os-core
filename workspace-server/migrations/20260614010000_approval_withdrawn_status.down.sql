-- Reverse: remove 'withdrawn' from approval_requests.status CHECK.
--
-- Step 1: delete any rows that are in 'withdrawn' state. The endpoint
-- was new in the up migration; rolling back the schema means rolling
-- back the data semantics, so any rows the endpoint wrote must go
-- away to keep the CHECK constraint satisfiable.
--
-- Step 2: narrow the CHECK back to the original 4-value enum.
--
-- This is the safe-rollback path: a deploy that runs the up
-- migration, exercises the endpoint, and then rolls back will
-- cleanly drop the 'withdrawn' rows and restore the original
-- constraint. The trade-off is loss of audit history for the
-- 'withdrawn' rows in the rollback window — acceptable because
-- the endpoint is new in the same deploy, and any 'withdrawn'
-- row that exists is at most a few hours old.

DELETE FROM approval_requests WHERE status = 'withdrawn';

ALTER TABLE approval_requests
    DROP CONSTRAINT IF EXISTS approval_requests_status_check;

ALTER TABLE approval_requests
    ADD CONSTRAINT approval_requests_status_check
    CHECK (status IN ('pending', 'approved', 'denied', 'escalated'));
