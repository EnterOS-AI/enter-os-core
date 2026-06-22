-- Persist the CP-provisioner-returned EC2 instance id on the workspace row.
-- Needed so the tenant workspace-server can resolve a workspace to its
-- backing EC2 for operations like terminal (EIC + SSH), live logs, and
-- debug introspection without re-calling the control plane on every hit.
--
-- Nullable: local-Docker workspaces never populate this column.
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS instance_id TEXT;

CREATE INDEX IF NOT EXISTS idx_workspaces_instance_id
    ON workspaces(instance_id)
    WHERE instance_id IS NOT NULL;
