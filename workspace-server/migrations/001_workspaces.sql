CREATE TABLE IF NOT EXISTS workspaces (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    role            TEXT,
    tier            INTEGER DEFAULT 1,
    status          TEXT DEFAULT 'provisioning',
    source_bundle_id TEXT,
    agent_card      JSONB,
    url             TEXT,
    parent_id       UUID REFERENCES workspaces(id),
    forwarded_to    UUID REFERENCES workspaces(id),
    last_heartbeat_at  TIMESTAMPTZ,
    last_error_rate    FLOAT DEFAULT 0,
    last_sample_error  TEXT,
    active_tasks       INTEGER DEFAULT 0,
    uptime_seconds     INTEGER DEFAULT 0,
    created_at      TIMESTAMPTZ DEFAULT now(),
    updated_at      TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_workspaces_parent_id ON workspaces(parent_id);
CREATE INDEX IF NOT EXISTS idx_workspaces_status ON workspaces(status);
