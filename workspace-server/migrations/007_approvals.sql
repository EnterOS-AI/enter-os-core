CREATE TABLE IF NOT EXISTS approval_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID REFERENCES workspaces(id),
    task_id         TEXT,
    action          TEXT NOT NULL,
    reason          TEXT,
    context         JSONB DEFAULT '{}'::jsonb,
    status          TEXT DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'denied', 'escalated')),
    decided_by      TEXT,
    decided_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_approvals_workspace ON approval_requests(workspace_id);
CREATE INDEX IF NOT EXISTS idx_approvals_status ON approval_requests(status);
