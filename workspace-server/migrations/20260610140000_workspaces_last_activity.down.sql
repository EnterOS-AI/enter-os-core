-- Reverse Agent-Liveness A2 stall-watchdog support.
-- Non-destructive to liveness behavior: dropping last_activity_at just disables
-- stall detection; the Redis TTL liveness monitor and status='failed' watchdog
-- are unaffected. The stall-state table is pure watchdog bookkeeping.
DROP INDEX IF EXISTS idx_workspaces_stall_watch;
ALTER TABLE IF EXISTS workspaces DROP COLUMN IF EXISTS last_activity_at;
DROP TABLE IF EXISTS workspace_stall_state;
