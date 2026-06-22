-- Workspace abilities: opt-in flags that gate platform-level behaviours.
--
-- broadcast_enabled (default FALSE): when TRUE the workspace may call
--   POST /workspaces/:id/broadcast to send a message to every non-removed
--   agent workspace in the org. Off by default — only privileged
--   orchestrator workspaces should hold this ability.
--
-- talk_to_user_enabled (default TRUE): when FALSE the workspace is not
--   allowed to deliver messages to the canvas user via send_message_to_user /
--   POST /notify. The platform returns HTTP 403 so the agent can forward its
--   update to a parent workspace instead. Default TRUE preserves existing
--   behaviour for all current workspaces.

ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS broadcast_enabled    BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS talk_to_user_enabled BOOLEAN NOT NULL DEFAULT TRUE;
