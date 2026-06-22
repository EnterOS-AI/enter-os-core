-- Fix workspace_stall_state.workspace_id type mismatch.
--
-- workspaces.id is UUID; the stall-state table was created with workspace_id
-- TEXT, causing the LEFT JOIN in handlers/stall_watchdog.go to fail with:
--   pq: operator does not exist: text = uuid
-- Align the column type with workspaces.id so the sweep query can join.
ALTER TABLE IF EXISTS workspace_stall_state
    ALTER COLUMN workspace_id TYPE UUID USING workspace_id::UUID;
