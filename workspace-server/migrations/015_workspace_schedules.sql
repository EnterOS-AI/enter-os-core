-- Workspace cron schedules — recurring tasks that fire A2A messages on a cron expression.
CREATE TABLE IF NOT EXISTS workspace_schedules (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name          TEXT NOT NULL DEFAULT '',
    cron_expr     TEXT NOT NULL,
    timezone      TEXT NOT NULL DEFAULT 'UTC',
    prompt        TEXT NOT NULL,
    enabled       BOOLEAN NOT NULL DEFAULT true,
    last_run_at   TIMESTAMPTZ,
    next_run_at   TIMESTAMPTZ,
    run_count     INTEGER NOT NULL DEFAULT 0,
    last_status   TEXT DEFAULT '',
    last_error    TEXT DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_schedules_workspace ON workspace_schedules(workspace_id);
CREATE INDEX IF NOT EXISTS idx_schedules_next_run ON workspace_schedules(enabled, next_run_at)
    WHERE enabled = true;
