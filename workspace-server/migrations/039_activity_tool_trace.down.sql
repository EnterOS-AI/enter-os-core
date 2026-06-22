DROP INDEX IF EXISTS idx_activity_logs_tool_trace;
ALTER TABLE activity_logs DROP COLUMN IF EXISTS tool_trace;
