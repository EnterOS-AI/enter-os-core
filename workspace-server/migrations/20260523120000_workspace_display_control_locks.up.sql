CREATE TABLE IF NOT EXISTS workspace_display_control_locks (
    workspace_id uuid PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    controller text NOT NULL CHECK (controller IN ('user', 'agent')),
    controlled_by text NOT NULL CHECK (length(controlled_by) > 0 AND length(controlled_by) <= 200),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_workspace_display_control_locks_expires
    ON workspace_display_control_locks (expires_at);
