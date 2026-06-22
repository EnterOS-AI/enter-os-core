-- Reverse of 20260523130000_drop_workspaces_awareness_namespace.up.sql.
--
-- Restores the workspaces.awareness_namespace column verbatim from
-- migration 010_workspace_awareness.sql so a down-cycle leaves the
-- schema bit-identical to the pre-drop state. The column will be
-- NULL on all rows after re-add — handlers no longer write to it and
-- callers no longer read it, so this is functionally inert without
-- a paired code revert.

ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS awareness_namespace TEXT;
