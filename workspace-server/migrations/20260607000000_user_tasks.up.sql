-- user_tasks: structured agent→user action requests ("things an agent asks
-- the user to do"). Mirrors the approval_requests shape but is a worklist
-- signal, not a destructive-action gate. See docs/design/rfc-user-tasks.md.
-- workspace_id FKs to workspaces(id) ON DELETE CASCADE — same anchor approval_requests
-- uses (007_approvals.sql), so a deleted workspace's tasks are reaped rather than
-- orphaned (an orphan row would vanish from the home list, which JOINs workspaces,
-- while still showing in the owning workspace's own List — an inconsistent ghost).
-- Inline in CREATE TABLE IF NOT EXISTS keeps the whole statement idempotent under
-- the re-apply-every-boot runner (no bare ALTER ADD CONSTRAINT, which can't be
-- IF NOT EXISTS and would crash-loop on re-apply).
CREATE TABLE IF NOT EXISTS user_tasks (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    title        TEXT NOT NULL,
    detail       TEXT,
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'done', 'dismissed')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at  TIMESTAMPTZ,
    resolved_by  TEXT
);

-- Home (cross-workspace) pending list: WHERE status='pending' ORDER BY created_at DESC.
CREATE INDEX IF NOT EXISTS idx_user_tasks_pending
    ON user_tasks (status, created_at DESC);

-- Owner-scoped reads/mutations — List/Update/Delete + the MCP user-task tools all
-- filter WHERE workspace_id = $1; index it so they don't sequential-scan at scale.
CREATE INDEX IF NOT EXISTS idx_user_tasks_workspace
    ON user_tasks (workspace_id, created_at DESC);
