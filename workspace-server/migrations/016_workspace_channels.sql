-- Social channel integrations (Telegram, Slack, Discord, etc.)
-- Each workspace can have one channel per type.
CREATE TABLE IF NOT EXISTS workspace_channels (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    channel_type    TEXT NOT NULL,
    channel_config  JSONB NOT NULL DEFAULT '{}',
    enabled         BOOLEAN NOT NULL DEFAULT true,
    allowed_users   JSONB DEFAULT '[]',
    last_message_at TIMESTAMPTZ,
    message_count   INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_channels_workspace_type ON workspace_channels(workspace_id, channel_type);
CREATE INDEX IF NOT EXISTS idx_channels_lookup ON workspace_channels(channel_type, enabled) WHERE enabled = true;
