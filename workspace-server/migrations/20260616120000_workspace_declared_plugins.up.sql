-- workspace_declared_plugins: the DECLARED plugin set for a workspace —
-- what its template/org config says it SHOULD have, independent of what is
-- actually installed in the container right now (that's workspace_plugins).
--
-- RFC#2843 #32: agent-skills are plugins that install DYNAMICALLY after a
-- workspace boots online, via the existing plugin install pipeline — never
-- through the provisioning channel (Secrets Manager / template-asset relay).
-- The post-online reconcile (registry heartbeat → plugins_reconcile.go) reads
-- this table to learn what each workspace should have, diffs it against the
-- workspace_plugins install records, and installs anything missing.
--
-- Why a separate table from workspace_plugins:
--   - workspace_plugins is the INSTALLED record (filesystem state mirror, with
--     installed_sha for drift). A row appears only AFTER a successful install.
--   - workspace_declared_plugins is the DESIRED record, written at org/import
--     time before the box is even online. The reconcile needs the desired set
--     to know what to install; it cannot derive that from the installed set
--     (a missing install leaves no row).
--
-- source_raw stores the FULL plugin source-contract string the template put
-- in `plugins:`, e.g.
--   "gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main"
-- The reconcile passes this verbatim to the install pipeline's resolveAndStage.
--
-- plugin_name is the resolved install name (the last subpath segment, or the
-- bare local name) — the /configs/plugins/<plugin_name>/ directory key. It is
-- the join key against workspace_plugins.plugin_name for the installed-vs-
-- declared diff.
--
-- Idempotent + additive: ON CONFLICT upsert at write time; re-import refreshes
-- source_raw. ON DELETE CASCADE so removing a workspace drops its declarations.

CREATE TABLE IF NOT EXISTS workspace_declared_plugins (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id  UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  plugin_name   TEXT        NOT NULL,
  source_raw    TEXT        NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS workspace_declared_plugins_ws_name
  ON workspace_declared_plugins(workspace_id, plugin_name);

-- The reconcile reads all declarations for one workspace at a time.
CREATE INDEX IF NOT EXISTS workspace_declared_plugins_ws
  ON workspace_declared_plugins(workspace_id);
