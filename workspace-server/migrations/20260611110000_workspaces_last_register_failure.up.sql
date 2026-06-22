-- #2530: track persistent register failures so heartbeat can surface
-- degraded status when a workspace cannot re-register (e.g. lost auth token
-- after container re-create).
BEGIN;

ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS last_register_failure_at TIMESTAMPTZ;

COMMIT;
