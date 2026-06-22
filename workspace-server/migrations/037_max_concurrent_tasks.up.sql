-- Per-workspace concurrency limit (#1408).
-- Default 1 preserves current behavior (single-task). Leaders can be
-- configured with higher values to accept A2A delegations while a cron runs.
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS max_concurrent_tasks INTEGER NOT NULL DEFAULT 1;
