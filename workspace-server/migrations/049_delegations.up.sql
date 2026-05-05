-- RFC #2829 PR-1: durable delegations ledger.
--
-- Today, delegation state is reconstructed by GROUPing activity_logs rows
-- by delegation_id and ORDER BY created_at DESC. Three problems:
--
--   1. No queryable "what is currently in flight for this workspace" — every
--      caller has to fold the event stream itself.
--   2. No place to durably stamp last_heartbeat / deadline on a per-task
--      basis, so a stuck-task sweeper has nothing to scan.
--   3. The 600s message/send proxy timeout (the user's 2026-05-05 home-hermes
--      iteration-14/90 incident) leaves the in-flight HTTP connection holding
--      all the state — caller restart, callee restart, proxy timeout all kill
--      the delegation. activity_logs can replay the *intent* but not the
--      *current state* without the row that says "yes this is still alive".
--
-- This table is the durable ledger that PRs #2-#4 build on:
--   PR-2 — push result to caller's inbox + use this row to track readiness
--   PR-3 — sweeper joins on (status='in_progress', last_heartbeat<now-N)
--   PR-4 — operator dashboard reads SELECT * WHERE status NOT IN ('completed','failed')
--
-- Delegation lifecycle:
--   queued     — caller recorded intent, target unreachable / busy queue
--   dispatched — A2A request sent to target's HTTP server
--   in_progress — target acknowledged + started work
--   completed  — terminal: result delivered to caller
--   failed     — terminal: gave up after retries
--   stuck      — terminal-ish: sweeper couldn't reach target for >threshold;
--                operator can transition to failed via dashboard (PR-4)

CREATE TABLE IF NOT EXISTS delegations (
    -- delegation_id chosen by the caller so callee + caller agree on the key
    -- without a database round-trip. UUID, but stored as TEXT to match the
    -- existing agent-side string contract (delegation.py uses str(uuid4())).
    delegation_id  text PRIMARY KEY,

    -- Caller is the workspace that initiated the delegation. Callee is the
    -- target. Both reference workspaces, but we don't FK them — workspace
    -- delete should NOT cascade-delete delegations history (audit retention).
    -- Same posture as tenant_resources (PR #2343).
    caller_id      uuid NOT NULL,
    callee_id      uuid NOT NULL,

    -- Truncated at insertion so a 50KB prompt doesn't bloat the ledger; the
    -- full prompt lives in activity_logs.request_body for forensic replay.
    task_preview   text NOT NULL,

    status         text NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued','dispatched','in_progress','completed','failed','stuck')),

    -- Stamped by callee heartbeats (PR-3 sweeper compares to NOW()). NULL
    -- before any heartbeat — sweeper treats NULL same as last_heartbeat
    -- < (created_at) for stuckness purposes.
    last_heartbeat timestamptz,

    -- Hard deadline. Beyond this, sweeper marks `failed` regardless of
    -- heartbeat liveness — protects against agents that heartbeat forever
    -- without making progress. Default 6h matches the longest-observed legit
    -- delegation in production (memory-namespace migration runs).
    deadline       timestamptz NOT NULL DEFAULT (now() + interval '6 hours'),

    -- Truncated result preview (full result in activity_logs response_body).
    -- Set on terminal completed transition.
    result_preview text,

    -- Set on failed/stuck terminal transition.
    error_detail   text,

    -- For PR-3 retry policy. Not used in PR-1 — declared so PR-3 doesn't
    -- need a follow-on migration.
    retry_count    integer NOT NULL DEFAULT 0,

    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    -- Idempotency: the agent-side delegate_task call accepts an idempotency
    -- key. Two records of the same key collapse to one row. Indexed UNIQUE
    -- where non-null so the natural collision becomes an INSERT … ON
    -- CONFLICT no-op.
    idempotency_key text
);

-- Sweeper hot path (PR-3): list everything that's in_progress and overdue
-- for a heartbeat. Partial index on non-terminal status keeps this small.
CREATE INDEX IF NOT EXISTS idx_delegations_inflight_heartbeat
    ON delegations (last_heartbeat NULLS FIRST)
    WHERE status IN ('queued','dispatched','in_progress');

-- Operator dashboard (PR-4): per-workspace recent delegations.
CREATE INDEX IF NOT EXISTS idx_delegations_caller_created
    ON delegations (caller_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_delegations_callee_created
    ON delegations (callee_id, created_at DESC);

-- Idempotency dedupe: composite (caller_id, idempotency_key) so two
-- different callers can use the same key without colliding.
CREATE UNIQUE INDEX IF NOT EXISTS idx_delegations_idempotency
    ON delegations (caller_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
