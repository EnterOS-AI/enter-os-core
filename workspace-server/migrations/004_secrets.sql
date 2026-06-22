CREATE TABLE IF NOT EXISTS workspace_secrets (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID REFERENCES workspaces(id),
    key             TEXT NOT NULL,
    encrypted_value BYTEA NOT NULL,
    created_at      TIMESTAMPTZ DEFAULT now(),
    updated_at      TIMESTAMPTZ DEFAULT now(),
    UNIQUE(workspace_id, key)
);
