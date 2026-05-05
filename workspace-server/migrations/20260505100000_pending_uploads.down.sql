-- 20260505100000_pending_uploads.down.sql
--
-- Drops the pending_uploads table and its indexes. Any pending file
-- uploads sitting in the table at rollback time are dropped — operators
-- on poll-mode workspaces lose those attachments, but they were never
-- fetched on the workspace side (otherwise they'd be acked + about to
-- be GC'd anyway), so the practical loss is the same as a cron sweep.

DROP INDEX IF EXISTS idx_pending_uploads_expires;
DROP INDEX IF EXISTS idx_pending_uploads_workspace_unacked;
DROP TABLE IF EXISTS pending_uploads;
