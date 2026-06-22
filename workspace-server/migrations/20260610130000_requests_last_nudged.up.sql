-- requests.last_nudged_at — RFC unified-requests-inbox P4 (idle-agent nudge).
--
-- Phase 4 adds a periodic sweeper (request_nudge_sweeper.go) that pokes an
-- IDLE agent which has unhandled inbox items so it doesn't forget to process
-- them. The sweeper rate-limits itself to ≤1 nudge per request per hour by
-- stamping last_nudged_at when it enqueues a nudge.
--
-- Idempotency / ordering safety
-- -----------------------------
-- The migration runner re-applies every *.up.sql on each boot, and on some
-- orderings the P1 `requests` table may not exist yet on the box this runs
-- against. Both statements are therefore fully idempotent AND guarded with
-- IF EXISTS on the table so they no-op (rather than crash-loop) when requests
-- is absent — the column gets added once P1's CREATE TABLE has landed.
ALTER TABLE IF EXISTS requests
    ADD COLUMN IF NOT EXISTS last_nudged_at TIMESTAMPTZ;

-- Partial index supporting the sweep query's hot predicate: agent recipients
-- in a nudge-eligible status, ordered by how long ago we last nudged them.
-- Partial (only the statuses the sweeper scans) keeps it small; the COALESCE
-- in the sweep's last_nudged_at filter still benefits because Postgres can
-- range-scan this index for the recipient/status prefix. IF NOT EXISTS so the
-- re-apply is a no-op; wrapped in a DO block guarded on table existence so it
-- is skipped cleanly when requests isn't created yet.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_name = 'requests'
    ) THEN
        CREATE INDEX IF NOT EXISTS idx_requests_nudge_candidates
            ON requests (recipient_type, status, recipient_id, last_nudged_at);
    END IF;
END$$;
