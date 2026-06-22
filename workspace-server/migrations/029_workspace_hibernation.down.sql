DROP INDEX IF EXISTS idx_workspaces_hibernation;
ALTER TABLE workspaces DROP COLUMN IF EXISTS hibernation_idle_minutes;
