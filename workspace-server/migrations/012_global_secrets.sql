-- Global secrets: platform-wide API keys that apply to all workspaces.
-- Workspace-level secrets (workspace_secrets) override globals with the same key.
CREATE TABLE IF NOT EXISTS global_secrets (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key             TEXT NOT NULL UNIQUE,
    encrypted_value BYTEA NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
