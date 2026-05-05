-- 20260505200000_pending_uploads_acked_index.up.sql
--
-- Adds the missing partial index for the acked-retention arm of the
-- pendinguploads.Sweep query. The Phase 1 migration created two
-- partial indexes both gated on `acked_at IS NULL` (workspace-fetch
-- hot path + expires_at sweep arm); the third query path —
-- `WHERE acked_at IS NOT NULL AND acked_at < now() - interval` — was
-- left to a seq scan.
--
-- For a high-traffic deployment that's a real cost: the table
-- accumulates one row per chat-attached file; the sweeper runs every
-- 5 minutes and DELETEs rows past the 1-hour ack retention. A seq
-- scan over 100K-1M acked rows holds an AccessShare lock for seconds
-- on every cycle. Partial-indexing the inverse predicate reduces
-- this to a btree range scan and lets the DELETE complete in
-- low-millisecond range.
--
-- WHERE acked_at IS NOT NULL is intentionally inverse of the other
-- two indexes — they cover the unacked working set; this covers the
-- terminal-state set the sweeper visits. Disjoint subsets, so the
-- two indexes don't overlap.
--
-- Caught in self-review on the parent RFC's Phase 4 PR; filed as
-- a follow-up rather than a Phase 1 fix because the cost only
-- materializes at a row count we don't expect to hit before the
-- sweeper has had a chance to keep up.

CREATE INDEX IF NOT EXISTS idx_pending_uploads_acked
    ON pending_uploads (acked_at)
    WHERE acked_at IS NOT NULL;
