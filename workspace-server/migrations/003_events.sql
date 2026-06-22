CREATE TABLE IF NOT EXISTS structure_events (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type    TEXT NOT NULL,
    workspace_id  UUID,
    agent_id      UUID,
    target_id     UUID,
    payload       JSONB,
    created_at    TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_events_workspace_id ON structure_events(workspace_id);
CREATE INDEX IF NOT EXISTS idx_events_created_at ON structure_events(created_at);
