DROP INDEX IF EXISTS idx_schedules_workspace_name;
ALTER TABLE workspace_schedules DROP CONSTRAINT IF EXISTS workspace_schedules_source_check;
ALTER TABLE workspace_schedules DROP COLUMN IF EXISTS source;
