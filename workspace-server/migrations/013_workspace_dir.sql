-- Per-workspace host directory mount (overrides global WORKSPACE_DIR).
-- NULL = use isolated Docker volume (default).
ALTER TABLE workspaces
ADD COLUMN IF NOT EXISTS workspace_dir TEXT;
