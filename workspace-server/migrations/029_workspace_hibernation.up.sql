-- 029_workspace_hibernation: opt-in automatic hibernation for idle workspaces.
--
-- When hibernation_idle_minutes is set (> 0) on a workspace, the hibernation
-- monitor will stop the container and set status = 'hibernated' after the
-- workspace has had active_tasks == 0 for that many consecutive minutes.
-- The workspace auto-wakes on the next incoming A2A message/send.
-- NULL (default) means hibernation is disabled for that workspace.

ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS hibernation_idle_minutes INT DEFAULT NULL;

-- Index so the hibernation sweep can efficiently find candidates without
-- a full table scan (only workspaces with non-NULL hibernation config).
CREATE INDEX IF NOT EXISTS idx_workspaces_hibernation
    ON workspaces (hibernation_idle_minutes)
    WHERE hibernation_idle_minutes IS NOT NULL;
