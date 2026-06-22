-- Agent-Liveness RFC, Layer 3 (A2): stall-watchdog support.
--
-- last_activity_at — wall-clock of the most recent activity_logs write for a
-- workspace, stamped write-through by logActivityExec (handlers/activity.go).
-- The stall-watchdog sweeper (handlers/stall_watchdog.go) compares this against
-- now() to detect a workspace that is status='online' AND active_tasks>0 but
-- has produced NO activity for too long — the "busy but silently hung" case
-- the Redis TTL liveness monitor (which only catches a DEAD/offline agent) and
-- the operator status='failed' watchdog both miss. This is what let JRS sit
-- dead for 2.5h: it was 'online' with an active task, just not advancing.
--
-- NULL = no activity yet observed (freshly provisioned, or pre-migration rows).
-- The sweeper treats NULL as "not stale" so a just-provisioned workspace that
-- hasn't logged anything yet is never probed/restarted on the basis of a NULL.
ALTER TABLE IF EXISTS workspaces ADD COLUMN IF NOT EXISTS last_activity_at TIMESTAMPTZ;

-- Partial index over the exact predicate the sweeper scans: online + busy.
-- Keeps the 3-min sweep a cheap indexed range scan instead of a seq scan over
-- every workspace. Partial so the index stays tiny (only online+busy rows).
CREATE INDEX IF NOT EXISTS idx_workspaces_stall_watch
    ON workspaces (last_activity_at)
    WHERE status = 'online' AND active_tasks > 0;

-- Per-workspace two-stage stall state for the watchdog's probe→restart state
-- machine + anti-flap cooldown. Separated from the workspaces row so the
-- watchdog's bookkeeping writes never contend with the hot heartbeat UPDATE
-- on workspaces, and so a DROP of this table fully reverts the feature.
--
--   state         — 'probed' after a liveness probe was enqueued; the row is
--                    deleted (state cleared) once activity resumes or after a
--                    restart fires, so presence of a 'probed' row == "awaiting
--                    the agent's response to a probe".
--   probed_at     — when the probe was enqueued; PROBE_GRACE is measured from
--                    here before escalating to a soft-restart.
--   probed_activity_at — snapshot of workspaces.last_activity_at AT probe time.
--                    The next sweep compares the live last_activity_at against
--                    this: if it advanced, the agent acted → clear the state
--                    (it was just slow). If unchanged → escalate.
--   last_action_at — when the most recent soft-restart fired; drives the
--                    COOLDOWN anti-flap gate (no re-restart within the window).
CREATE TABLE IF NOT EXISTS workspace_stall_state (
    workspace_id        TEXT PRIMARY KEY,
    state               TEXT NOT NULL,
    probed_at           TIMESTAMPTZ,
    probed_activity_at  TIMESTAMPTZ,
    last_action_at      TIMESTAMPTZ,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
