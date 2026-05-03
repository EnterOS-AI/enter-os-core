-- Layer 1 of the runtime-rollout plan (issue #2272).
--
-- Decouples publish from promotion: rather than every workspace pulling
-- whatever's on `:latest` at create-time, the provisioner consults a
-- promoted-digest table. Operators (or a cascade-success hook) update
-- the table to roll out a new version; existing workspaces stay on
-- whatever they were created with until they're explicitly recreated.
-- One bad publish no longer breaks every workspace simultaneously.
--
-- Two pieces:
--   1. runtime_image_pins — the lookup table. One row per template name.
--      No row = fall back to `:latest` (preserves today's behavior so
--      this migration is non-disruptive to existing deployments).
--
--   2. workspaces.runtime_image_digest — what the workspace actually
--      pulled at create/restart time. Diagnostic + foundation for
--      Layer 2's per-ring routing (canary vs stable). Nullable so
--      pre-migration workspaces show up as NULL until next restart.

CREATE TABLE IF NOT EXISTS runtime_image_pins (
    -- Template name (e.g. "claude-code", "hermes"). Matches the keys
    -- in workspace-server/internal/provisioner.RuntimeImages.
    template_name   TEXT PRIMARY KEY,

    -- Full GHCR digest including the `sha256:` prefix. Provisioner pulls
    -- ghcr.io/molecule-ai/workspace-template-<template>@sha256:<digest>.
    digest          TEXT NOT NULL CHECK (digest ~ '^sha256:[a-f0-9]{64}$'),

    -- Audit fields — populated by the admin promote/rollback endpoint
    -- (separate follow-up PR). Operators can grep history by actor
    -- when investigating a bad promotion.
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by      TEXT,
    notes           TEXT
);

ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS runtime_image_digest TEXT;

-- Index supports "show me all workspaces still on the old digest" queries
-- after a promotion (used by the canvas admin's stale-workspaces panel
-- + the recreate sweeper that nudges operators when a bad version is
-- still in service).
CREATE INDEX IF NOT EXISTS idx_workspaces_runtime_image_digest
    ON workspaces (runtime_image_digest)
    WHERE runtime_image_digest IS NOT NULL;
