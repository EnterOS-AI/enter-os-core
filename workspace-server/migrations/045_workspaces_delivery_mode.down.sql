-- 045_workspaces_delivery_mode.down.sql
--
-- Drops the delivery_mode column. Any code reading it after rollback falls
-- back to push mode (the pre-#2339 behavior), so this is forward-only-safe
-- only if the matching application code is rolled back in the same release.

ALTER TABLE workspaces
    DROP COLUMN IF EXISTS delivery_mode;
