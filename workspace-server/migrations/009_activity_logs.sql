-- Activity logs: comprehensive logging for A2A communication, task updates, agent actions, and errors.
CREATE TABLE IF NOT EXISTS activity_logs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    activity_type TEXT NOT NULL,  -- 'a2a_send', 'a2a_receive', 'task_update', 'agent_log', 'error'
    source_id     UUID,           -- workspace that initiated the action
    target_id     UUID,           -- workspace that received the action (for A2A)
    method        TEXT,            -- A2A method name (e.g. 'message/send') or task action
    summary       TEXT,            -- Human-readable summary
    request_body  JSONB,           -- Request payload (for A2A)
    response_body JSONB,           -- Response payload (for A2A)
    duration_ms   INTEGER,         -- How long the operation took
    status        TEXT DEFAULT 'ok', -- 'ok', 'error', 'timeout'
    error_detail  TEXT,            -- Error message if status != 'ok'
    created_at    TIMESTAMPTZ DEFAULT now()
);

-- Composite index covers (workspace_id) prefix and (workspace_id, activity_type) lookups.
-- Separate idx_activity_created_at kept for global retention cleanup queries.
CREATE INDEX IF NOT EXISTS idx_activity_ws_type_time ON activity_logs(workspace_id, activity_type, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_activity_created_at ON activity_logs(created_at);

-- Add current_task to workspaces for showing what the agent is currently working on.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS current_task TEXT;
