ALTER TABLE workspaces
    DROP COLUMN IF EXISTS broadcast_enabled,
    DROP COLUMN IF EXISTS talk_to_user_enabled;
