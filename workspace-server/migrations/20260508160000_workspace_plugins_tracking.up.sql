-- workspace_plugins: per-workspace record of installed plugins, with the
-- tracked-ref needed for the version-subscription model (core#113).
--
-- Today plugin install state is filesystem-only — `/configs/plugins/<name>/`
-- inside the workspace container. There's no DB record of "what's installed
-- where, from what source, pinned to what." That's fine until you want
-- drift detection (compare upstream tag's resolved SHA vs the installed
-- one) and that's the foundation this table provides.
--
-- This migration is purely additive: existing install paths keep working;
-- they'll write to this table on next install. Workspaces with plugins
-- already installed before this migration won't have rows until they're
-- re-installed (acceptable — the tracking is forward-looking).
--
-- tracked_ref values:
--   'none'         — no auto-update tracking (default)
--   'tag:vX.Y.Z'   — track a specific version tag
--   'tag:latest'   — track the latest tag (drift on every new tag)
--   'sha:<full>'   — pinned to a specific commit SHA (no drift ever)
--
-- A subsequent migration adds the plugin_update_queue table once drift
-- detection lands.

CREATE TABLE IF NOT EXISTS workspace_plugins (
  id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id    UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  plugin_name     TEXT        NOT NULL,
  source_raw      TEXT        NOT NULL,
  tracked_ref     TEXT        NOT NULL DEFAULT 'none',
  installed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS workspace_plugins_workspace_name
  ON workspace_plugins(workspace_id, plugin_name);

-- Partial index for the drift detector: only scan rows opted into tracking.
CREATE INDEX IF NOT EXISTS workspace_plugins_tracked_not_none
  ON workspace_plugins(tracked_ref) WHERE tracked_ref != 'none';
