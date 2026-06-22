-- Rollback for 20260604000000_activity_logs_seq.up.sql.
-- Drops the feed-ordering index and the monotonic seq column.
-- Run manually by an operator via psql; the boot-time runner never applies
-- *.down.sql (see RunMigrations in internal/db/postgres.go, issue #211).

DROP INDEX IF EXISTS idx_activity_ws_created_seq;

ALTER TABLE activity_logs
    DROP COLUMN IF EXISTS seq;
