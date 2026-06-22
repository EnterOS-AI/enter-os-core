-- #1870 Phase 1: TASK-level queue for A2A delegations that hit a busy target.
--
-- Before: when the target workspace's HTTP handler errors (agent busy
-- mid-synthesis — single-threaded LLM loop), a2a_proxy_helpers.go returns
-- 503 with a Retry-After hint, the caller logs activity_type='delegation'
-- status='failed' and moves on. Delegations silently dropped; fan-out
-- storms from leads reach ~70% drop rate.
--
-- After: same failure triggers an INSERT into a2a_queue with priority=TASK.
-- Workspace's next heartbeat (up to 30s later) drains the queue if capacity
-- allows. Proxy returns 202 Accepted with {"queued": true, "queue_id", ...}
-- instead of 503, caller logs as dispatched-queued.
--
-- Phase 2 will add INFO (TTL) and CRITICAL (preempt) levels. This table's
-- priority column is wide enough for all three from day one — no migration
-- churn on next phase.

CREATE TABLE IF NOT EXISTS a2a_queue (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    caller_id       uuid,
    priority        smallint NOT NULL DEFAULT 50,     -- 100=CRITICAL, 50=TASK, 10=INFO
    body            jsonb NOT NULL,
    method          text,
    idempotency_key text,
    enqueued_at     timestamptz NOT NULL DEFAULT now(),
    dispatched_at   timestamptz,
    completed_at    timestamptz,
    expires_at      timestamptz,                      -- TTL, for future INFO level
    attempts        integer NOT NULL DEFAULT 0,
    status          text NOT NULL DEFAULT 'queued'    -- queued | dispatched | completed | dropped | failed
        CHECK (status IN ('queued','dispatched','completed','dropped','failed')),
    last_error      text
);

-- Primary drain-query index: pick oldest highest-priority queued item for a
-- workspace. Partial index on status='queued' keeps the hot path tiny.
CREATE INDEX IF NOT EXISTS idx_a2a_queue_dispatch
    ON a2a_queue (workspace_id, priority DESC, enqueued_at ASC)
    WHERE status = 'queued';

-- TTL index for future INFO cleanup (no-op today — expires_at is always NULL
-- for TASK). Still worth creating now so Phase 2 doesn't need a migration.
CREATE INDEX IF NOT EXISTS idx_a2a_queue_expiry
    ON a2a_queue (expires_at)
    WHERE status = 'queued' AND expires_at IS NOT NULL;

-- Idempotency: a caller retrying with the same idempotency_key should not
-- double-enqueue. Partial unique index only on active queue entries so
-- completed/dropped entries don't block future legitimate re-uses.
CREATE UNIQUE INDEX IF NOT EXISTS idx_a2a_queue_idempotency
    ON a2a_queue (workspace_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND status IN ('queued','dispatched');
