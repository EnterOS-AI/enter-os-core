-- Reverse workspace_stall_state.workspace_id UUID alignment.
ALTER TABLE IF EXISTS workspace_stall_state
    ALTER COLUMN workspace_id TYPE TEXT;
