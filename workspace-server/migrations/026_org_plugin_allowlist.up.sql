-- Per-org plugin allowlist for tool governance (#591).
-- When an org has at least one entry in this table, workspace agents may only
-- install plugins listed here. An empty allowlist means "allow all" (backward
-- compatible with existing deployments).
--
-- org_id references the root/parent workspace that acts as the org anchor.
-- enabled_by records the workspace ID of the admin who added the entry.
CREATE TABLE IF NOT EXISTS org_plugin_allowlist (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id      UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  plugin_name TEXT        NOT NULL,
  enabled_by  TEXT        NOT NULL,
  enabled_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS org_plugin_allowlist_org_plugin
  ON org_plugin_allowlist(org_id, plugin_name);
