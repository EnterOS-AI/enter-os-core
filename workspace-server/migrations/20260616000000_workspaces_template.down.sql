-- Revert workspaces.template addition.
ALTER TABLE workspaces
    DROP COLUMN IF EXISTS template;
