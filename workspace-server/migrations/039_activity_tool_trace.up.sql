-- Add tool_trace column to activity_logs for platform-level observability.
-- Stores the list of tools/commands an agent actually invoked during an A2A
-- call, extracted from the A2A response metadata. Enables verifying agent
-- claims ("I checked X") against what tools were actually called.
ALTER TABLE activity_logs ADD COLUMN IF NOT EXISTS tool_trace JSONB;

-- Index for querying which agents used specific tools
CREATE INDEX IF NOT EXISTS idx_activity_logs_tool_trace
    ON activity_logs USING gin (tool_trace) WHERE tool_trace IS NOT NULL;
