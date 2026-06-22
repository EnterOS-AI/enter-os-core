-- Down migration for plugin_drift_queue.
-- Reverses the two changes introduced by the up migration.

-- 1. Remove plugin_update_queue (all queued drift entries are discarded).
DROP TABLE IF EXISTS plugin_update_queue;

-- 2. Remove installed_sha column from workspace_plugins.
--   Existing drift sweeper rows are unaffected (sweeper doesn't exist yet
--   in this version of the codebase — this is a forward-only component).
ALTER TABLE workspace_plugins DROP COLUMN IF EXISTS installed_sha;
