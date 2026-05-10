-- plugin_drift_queue: plugin_update_queue table + installed_sha column.
--
-- Migration order:
--   1. plugin_update_queue — new table, safe to create first.
--   2. installed_sha on workspace_plugins — added column; existing rows stay NULL
--      (no installed_sha until they are re-installed).
--
-- Why two changes in one migration: the drift detector reads from
-- workspace_plugins (needs installed_sha to compare) and writes to
-- plugin_update_queue. Both must exist before the sweeper starts.
-- The alternative — a separate migration for installed_sha — would mean
-- the sweeper could start after migration-1 but before migration-2,
-- writing queue rows with NULL installed_sha. Keeping them together
-- avoids that race without needing a schema-lock flag.

-- plugin_update_queue: records upstream drift for operator review before
-- the platform auto-applies the update.
--
-- Rows are created by the drift sweeper when workspace_plugins.tracked_ref
-- is not 'none' and the upstream SHA differs from installed_sha.
-- Rows are consumed by the admin apply endpoint (core#123).
--
-- Uniqueness: one pending row per (workspace_id, plugin_name). A new drift
-- while a row is still pending is a no-op — the existing pending row
-- reflects the same desired update.
CREATE TABLE IF NOT EXISTS plugin_update_queue (
  id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id    UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  plugin_name     TEXT        NOT NULL,
  tracked_ref     TEXT        NOT NULL,
  current_sha     TEXT        NOT NULL,  -- SHA we had installed
  latest_sha      TEXT        NOT NULL,  -- SHA upstream resolved to
  status          TEXT        NOT NULL DEFAULT 'pending',
  -- Valid statuses: pending | applied | dismissed
  -- 'pending': drift detected, awaiting operator review
  -- 'applied': operator confirmed, plugin re-installed
  -- 'dismissed': operator explicitly ignored this drift
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT plugin_update_queue_status CHECK (status IN ('pending', 'applied', 'dismissed'))
);

CREATE UNIQUE INDEX IF NOT EXISTS plugin_update_queue_pending_unique
  ON plugin_update_queue(workspace_id, plugin_name)
  WHERE status = 'pending';

-- Partial index: the GET /admin/plugin-updates-pending query filters by
-- status = 'pending' on every call.
CREATE INDEX IF NOT EXISTS plugin_update_queue_status_pending
  ON plugin_update_queue(created_at)
  WHERE status = 'pending';

-- Add installed_sha to workspace_plugins. This column stores the SHA that
-- was last successfully installed. The drift sweeper compares this against
-- the upstream-resolved SHA for the tracked_ref to detect drift.
--
-- NULL means: the row exists (was written by an install before this
-- migration) but we don't know what SHA was installed. The drift sweeper
-- treats NULL as "no drift possible yet" — it only compares rows where
-- installed_sha IS NOT NULL.
--
-- The column is updated by:
--   (a) recordWorkspacePluginInstall at install time (always set)
--   (b) the admin apply endpoint after a successful re-install (always set)
ALTER TABLE workspace_plugins ADD COLUMN installed_sha TEXT;
