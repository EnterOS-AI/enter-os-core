-- Reverse the participant-kind discriminator.
-- Non-destructive: dropping the column makes every workspace an ordinary
-- workspace again (the platform agent loses its marker but its row survives).
DROP INDEX IF EXISTS idx_workspaces_kind;
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_platform_root_check;
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_kind_check;
ALTER TABLE workspaces DROP COLUMN IF EXISTS kind;
