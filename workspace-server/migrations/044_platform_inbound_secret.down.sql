-- Reverse 044_platform_inbound_secret.up.sql
--
-- Drops the per-workspace platform→workspace shared secret column.
--
-- Destructive: any current value is irrecoverable. Re-running the .up.sql
-- adds the column back as NULL; existing workspaces would then need a
-- reprovision (or a separate backfill) to mint a fresh secret.
ALTER TABLE workspaces
    DROP COLUMN IF EXISTS platform_inbound_secret;
