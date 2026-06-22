BEGIN;

ALTER TABLE workspaces DROP COLUMN IF EXISTS last_register_failure_at;

COMMIT;
