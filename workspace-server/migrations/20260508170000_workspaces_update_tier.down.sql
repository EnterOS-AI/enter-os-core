DROP INDEX IF EXISTS workspaces_update_tier_canary;
ALTER TABLE workspaces DROP COLUMN IF EXISTS update_tier;
