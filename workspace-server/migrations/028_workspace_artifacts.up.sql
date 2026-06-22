-- 028_workspace_artifacts: store Cloudflare Artifacts repo linkage per workspace.
--
-- Each workspace can be linked to exactly one Cloudflare Artifacts repo
-- (the primary snapshot store). Additional repos (forks) are ephemeral and
-- tracked only via the CF API — not in this table.
--
-- Remote URLs are stored for informational display only; callers must
-- call POST /workspaces/:id/artifacts/token to obtain a fresh git credential.

CREATE TABLE IF NOT EXISTS workspace_artifacts (
    id           UUID        NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    cf_repo_name TEXT        NOT NULL,
    cf_namespace TEXT        NOT NULL,
    -- remote_url is the base Git remote (without embedded credentials).
    -- Credentials are obtained on-demand via POST /tokens.
    remote_url   TEXT,
    description  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT workspace_artifacts_pkey PRIMARY KEY (id)
);

-- Each workspace may have at most one linked CF Artifacts repo.
CREATE UNIQUE INDEX IF NOT EXISTS uq_workspace_artifacts_workspace_id
    ON workspace_artifacts (workspace_id);

-- Allow fast lookup by CF repo name within a namespace.
CREATE INDEX IF NOT EXISTS idx_workspace_artifacts_cf_repo
    ON workspace_artifacts (cf_namespace, cf_repo_name);
