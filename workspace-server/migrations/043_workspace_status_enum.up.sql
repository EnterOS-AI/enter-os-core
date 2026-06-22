-- 043_workspace_status_enum.up.sql
--
-- Convert workspaces.status from free-form TEXT to a real Postgres
-- ENUM type. The previous shape (TEXT DEFAULT 'provisioning' with no
-- CHECK constraint, set by 001_workspaces.sql) let any handler write
-- any string, including typos and stale values from older code paths.
-- Locking the value set forces every writer to use one of the agreed
-- states and lets us add a new state (`degraded`, used by the SDK
-- wedge detector landing in this same change) without losing type
-- safety on the column.
--
-- Value set covers every status the production codebase actually writes:
--
--   provisioning  — workspace row exists, container is being created
--                   (initial INSERT default)
--   online        — heartbeat fresh + last response was successful
--   offline       — Redis liveness key expired (ws-side dead) or
--                   the proxy detected an unreachable upstream
--   degraded      — runtime is alive but reporting trouble (heartbeat
--                   error_rate >= 0.5, OR new in this change:
--                   workspace explicitly reported runtime_state="wedged")
--   failed        — provisioning never completed, or workspace marked
--                   itself failed via bundle import / runtime crash
--   removed       — soft-delete tombstone; the row stays so foreign-
--                   key references survive but no operations target it
--   paused        — operator-initiated suspend via workspace_restart's
--                   pause path (workspace_restart.go:406)
--   hibernated    — auto-suspended after idle threshold; container
--                   stopped but row preserved (workspace_restart.go:283,
--                   introduced by migration 029_workspace_hibernation)
--
-- Sweep of every `UPDATE workspaces SET status = 'X'` in the
-- workspace-server/internal/ tree (excluding tests) verified the
-- value set. Adding a new state in the future requires both updating
-- this enum (a separate `ALTER TYPE … ADD VALUE` migration) AND any
-- writers — the enum will reject unknown strings at insert/update
-- time, which is the exact failure mode this migration is meant to
-- give us.
--
-- Deployment: `ALTER TABLE … ALTER COLUMN TYPE` takes ACCESS
-- EXCLUSIVE on workspaces. A long-running SELECT against the table
-- will block the migration; the migration will then block every
-- writer behind it. `SET lock_timeout` aborts the migration in 5s
-- if it can't acquire the lock — preferable to stalling the whole
-- workspace fleet behind one slow query.

BEGIN;

SET LOCAL lock_timeout = '5s';

CREATE TYPE workspace_status AS ENUM (
    'provisioning',
    'online',
    'offline',
    'degraded',
    'failed',
    'removed',
    'paused',
    'hibernated'
);

-- The two-step ALTER (DROP DEFAULT then change type then SET DEFAULT)
-- is required because Postgres rejects an ALTER COLUMN TYPE on a
-- column that has a DEFAULT whose expression doesn't match the new
-- type. The intermediate moment with no default is fine — no INSERT
-- happens between these statements inside the same transaction.
--
-- The `USING status::workspace_status` cast is the type-conversion
-- expression Postgres needs when the source and target types aren't
-- assignment-compatible. If any existing row has a status value
-- outside the enum's set, this statement aborts the transaction and
-- the migration leaves the table untouched — that's the correct
-- behavior (we'd want to know about the rogue value before locking
-- the type).
ALTER TABLE workspaces
    ALTER COLUMN status DROP DEFAULT;

ALTER TABLE workspaces
    ALTER COLUMN status TYPE workspace_status USING status::workspace_status;

ALTER TABLE workspaces
    ALTER COLUMN status SET DEFAULT 'provisioning'::workspace_status;

COMMIT;
