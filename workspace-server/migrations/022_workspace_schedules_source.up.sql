-- Add `source` column to workspace_schedules so the org-import path can
-- distinguish rows it owns ('template') from rows created via the runtime
-- API/Canvas ('runtime'). DB is the source of truth; org/import is now
-- additive — it only INSERTs missing template rows and only UPDATEs rows
-- where source = 'template'. Runtime-added schedules survive re-imports.
--
-- Legacy-row policy: every row predating this migration is backfilled to
-- 'template'. Rationale — before this migration the only way to get a row
-- into workspace_schedules at scale was org/import (Canvas UI for schedules
-- was minimal); defaulting legacy rows to 'template' preserves the
-- idempotent-refresh path on re-import. Users who had runtime-created
-- schedules can reclassify them via UPDATE post-deployment.

ALTER TABLE workspace_schedules
    ADD COLUMN IF NOT EXISTS source TEXT;

UPDATE workspace_schedules SET source = 'template' WHERE source IS NULL;

ALTER TABLE workspace_schedules ALTER COLUMN source SET NOT NULL;
ALTER TABLE workspace_schedules ALTER COLUMN source SET DEFAULT 'runtime';

-- idempotent constraint add (Postgres lacks IF NOT EXISTS on ADD CONSTRAINT pre-15)
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'workspace_schedules_source_check'
    ) THEN
        ALTER TABLE workspace_schedules
            ADD CONSTRAINT workspace_schedules_source_check
            CHECK (source IN ('template', 'runtime'));
    END IF;
END$$;

COMMENT ON COLUMN workspace_schedules.source IS
    'template = seeded by org/import (refreshable); runtime = created via Canvas/API (preserved across re-imports)';

-- Required so org-import can use ON CONFLICT (workspace_id, name) DO UPDATE.
-- Schedules within a single workspace are uniquely identified by name.
CREATE UNIQUE INDEX IF NOT EXISTS idx_schedules_workspace_name
    ON workspace_schedules(workspace_id, name);
