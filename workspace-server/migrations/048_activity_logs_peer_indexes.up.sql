-- Add per-peer indexes on activity_logs to make chat_history queries
-- index-driven instead of seq-scan-driven on workspaces with thousands
-- of accumulated rows. #2478.
--
-- chat_history hits:
--
--   SELECT ... FROM activity_logs
--   WHERE workspace_id = $1
--     AND activity_type = 'a2a_receive'
--     AND (source_id = $2 OR target_id = $2)
--   ORDER BY created_at DESC LIMIT 20;
--
-- The existing idx_activity_ws_type_time covers workspace_id+type
-- prefix but the (source_id = $X OR target_id = $X) clause then forces
-- a workspace-scoped seq-scan-and-filter. Two separate indexes (one per
-- nullable column) let Postgres BitmapOr them into a workspace-scoped
-- BitmapAnd against the existing index.
--
-- Partial WHERE NOT NULL because most activity rows (heartbeats,
-- agent_log, memory_write, etc.) have NULL source_id/target_id and
-- shouldn't bloat the index. Per-row index size drops from ~all rows
-- to ~A2A-only rows.
--
-- Anti-pattern caveat from the issue: a single compound (a, b) index
-- can't serve `a OR b` — Postgres can only use compound for prefix
-- match. Two separate indexes + BitmapOr is the right shape.
--
-- CONCURRENTLY would be ideal for online deploys, but goose runs
-- migrations in a single transaction by default which doesn't allow
-- CONCURRENTLY. The alternative (annotating the migration to skip the
-- transaction wrapper) is a per-runner concern; leaving as plain
-- CREATE INDEX so this works under any goose config. activity_logs is
-- typically <O(100k) rows per workspace × <O(100) workspaces in
-- production today; the lock is sub-second-scale.

CREATE INDEX IF NOT EXISTS idx_activity_ws_source
  ON activity_logs(workspace_id, source_id)
  WHERE source_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_activity_ws_target
  ON activity_logs(workspace_id, target_id)
  WHERE target_id IS NOT NULL;
