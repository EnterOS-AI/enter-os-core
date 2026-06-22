-- Per-workspace repo-access mode (fixes #65).
--
-- Values:
--   'none'       — no repo bind-mount; agent gets an isolated Docker volume at /workspace
--   'read_only'  — bind-mount workspace_dir at /workspace:ro (agent can read but not write)
--   'read_write' — bind-mount workspace_dir at /workspace (full read/write, current PM behaviour)
--
-- Requires workspace_dir to be set when value is 'read_only' or 'read_write'.
-- Validation enforced at the handler layer (org.go + workspace.go POST).

ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS workspace_access VARCHAR(20) NOT NULL DEFAULT 'none'
    CHECK (workspace_access IN ('none', 'read_only', 'read_write'));
