CREATE TABLE IF NOT EXISTS agents (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID REFERENCES workspaces(id),
    model           TEXT,
    status          TEXT DEFAULT 'active',
    removed_at      TIMESTAMPTZ,
    removal_reason  TEXT,
    created_at      TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_agents_workspace_id ON agents(workspace_id);
