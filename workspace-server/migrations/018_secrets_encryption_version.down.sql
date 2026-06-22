-- Reverse of 018_secrets_encryption_version.up.sql
ALTER TABLE global_secrets    DROP COLUMN IF EXISTS encryption_version;
ALTER TABLE workspace_secrets DROP COLUMN IF EXISTS encryption_version;
