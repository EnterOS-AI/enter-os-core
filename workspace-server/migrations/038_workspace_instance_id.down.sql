DROP INDEX IF EXISTS idx_workspaces_instance_id;
ALTER TABLE workspaces DROP COLUMN IF EXISTS instance_id;
