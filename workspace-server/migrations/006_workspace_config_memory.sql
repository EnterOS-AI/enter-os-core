CREATE TABLE IF NOT EXISTS workspace_config (
    workspace_id  UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    data          JSONB NOT NULL DEFAULT '{}',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS workspace_memory (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    key           TEXT NOT NULL,
    value         JSONB NOT NULL,
    expires_at    TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(workspace_id, key)
);
CREATE INDEX IF NOT EXISTS workspace_memory_workspace_id_idx ON workspace_memory(workspace_id);
