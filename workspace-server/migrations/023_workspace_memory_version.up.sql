-- Optimistic-locking version column for workspace_memory.
--
-- Purpose: two agents can race a read → modify → write against the same
-- (workspace_id, key) pair. Current INSERT ... ON CONFLICT UPDATE has
-- last-writer-wins semantics — the first writer's work is silently
-- overwritten. This matters for orchestrators (PM, Dev Lead) that keep
-- structured running state in memory (task queues, delegation-result
-- ledgers) and for the `research-backlog:*` keys that multiple idle
-- loops can touch concurrently.
--
-- The version column advances on every successful write. The memory
-- handler accepts an optional `if_match_version` on write; when set,
-- the UPDATE is guarded by `WHERE version = $expected` and returns 409
-- Conflict on mismatch so the caller can re-read + retry. When absent,
-- behaviour is unchanged from pre-migration (last-write-wins), so every
-- existing agent tool keeps working without modification.
--
-- Baseline: existing rows start at version 1. New rows default to 1.
-- IF NOT EXISTS keeps this re-runnable. The platform's migration runner
-- has no schema_migrations tracking table (#TODO follow-up issue) and
-- replays every .up.sql on every boot, so any non-idempotent ALTER will
-- crash boot on the second start against an existing volume.
ALTER TABLE workspace_memory
    ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

COMMENT ON COLUMN workspace_memory.version IS
    'Monotonic revision counter. Incremented on every successful write. '
    'Clients doing read-modify-write loops pass this value as if_match_version '
    'on the next write to get 409 on conflict instead of silent last-write-wins.';
