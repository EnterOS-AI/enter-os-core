-- #795: Track consecutive empty cron responses to detect phantom-producing schedules.
-- When consecutive_empty_runs >= 3, the scheduler sets last_status='stale' instead of 'ok',
-- making it visible in /admin/schedules/health and the PM silence-detector.
ALTER TABLE workspace_schedules ADD COLUMN IF NOT EXISTS consecutive_empty_runs INTEGER NOT NULL DEFAULT 0;
