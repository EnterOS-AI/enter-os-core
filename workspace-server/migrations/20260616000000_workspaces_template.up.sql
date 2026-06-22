-- Add workspaces.template to decouple installed template from engine runtime.
-- Phase 1 of RFC #2948; closes the rejected PATCH runtime=seo-agent path.
--
-- Empty string means "no installed template — resolve assets from runtime".
-- NOT NULL DEFAULT '' keeps the Go model a plain string and preserves every
-- existing workspace without a backfill step for the column itself.
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS template TEXT NOT NULL DEFAULT '';

-- Backfill template from workspace_config.data when it was already recorded
-- at create time (e.g. TemplatePalette / org import flows that stored it
-- there but did not persist it on the workspaces row).
UPDATE workspaces w
SET template = COALESCE(NULLIF(c.data ->> 'template', ''), w.template)
FROM workspace_config c
WHERE c.workspace_id = w.id
  AND w.template = ''
  AND c.data ->> 'template' IS NOT NULL;
