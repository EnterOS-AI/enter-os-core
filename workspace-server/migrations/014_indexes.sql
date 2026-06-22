-- Performance indexes for frequently used queries.
-- parent_id: cascade delete, hierarchy queries, peer discovery
CREATE INDEX IF NOT EXISTS idx_workspaces_parent_id ON workspaces(parent_id);

-- status: filtering online/offline workspaces, health sweep
CREATE INDEX IF NOT EXISTS idx_workspaces_status ON workspaces(status);

-- canvas_layouts: joined on every List/Get workspace query
CREATE INDEX IF NOT EXISTS idx_canvas_layouts_ws ON canvas_layouts(workspace_id);
