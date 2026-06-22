-- #3127 (Researcher follow-up): backoff for transient gateway-origin
-- queue retries. Without a per-row "not before" gate, DrainQueueForWorkspace
-- with capacity>1 can re-dispatch the same item inside the same call
-- immediately after MarkQueueItemTransientRetry (because the row's
-- status='queued' AND the for-loop in DrainQueueForWorkspace iterates up
-- to capacity times). MarkQueueItemTransientRetry sets the new column to
-- now() + 5s; DequeueNext's WHERE clause skips rows whose next_attempt_at
-- is still in the future. This breaks the tight retry loop without
-- requiring a schema-foreign "stop draining" branch.
--
-- Column is nullable: NULL = no backoff constraint (default state for
-- rows that have never been transient-retried, and for the legacy
-- MarkQueueItemFailed path that doesn't touch this column).
--
-- Index strategy: a NEW partial index on next_attempt_at IS NOT NULL
-- keyed by (workspace_id, next_attempt_at, priority DESC, enqueued_at ASC).
-- The predicate is intentionally STABLE (no now() — PostgreSQL rejects
-- volatile functions in index predicates and the previous iteration of
-- this migration used `next_attempt_at > now()` which fails DDL). The
-- planner uses this index for the rare gated-row case; the existing
-- idx_a2a_queue_dispatch covers the common NULL case via row-filter.
-- next_attempt_at is included as a key column (not in the predicate)
-- so the planner can range-scan it during the gated case.
--
-- #3127 PR follow-up (Researcher/CR2 REQUEST_CHANGES on 7df1b5e9):
-- replaced the originally-proposed `next_attempt_at > now()` partial
-- predicate with the stable `next_attempt_at IS NOT NULL` predicate.
-- The predicate must reference only IMMUTABLE expressions; now() is
-- STABLE, not IMMUTABLE, so the original index could not be created
-- at deploy time.

ALTER TABLE a2a_queue ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_a2a_queue_next_attempt_at
    ON a2a_queue (workspace_id, next_attempt_at, priority DESC, enqueued_at ASC)
    WHERE status = 'queued'
      AND next_attempt_at IS NOT NULL;

