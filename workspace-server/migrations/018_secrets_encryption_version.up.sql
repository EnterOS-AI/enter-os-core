-- Encryption version tag on secret tables (fixes #85).
--
-- Problem: workspace_secrets.encrypted_value and global_secrets.encrypted_value
-- are bytea columns that hold plaintext when the platform ran without
-- SECRETS_ENCRYPTION_KEY (crypto.Encrypt short-circuits to plaintext). Turning
-- on encryption later makes every pre-existing secret unreadable — crypto.Decrypt
-- runs GCM on plaintext bytes, fails, and the provisioner silently drops the
-- row, cascading into "workspace boots with missing OAuth token" crash loops.
--
-- Fix: tag each row with how it was written. 0 = plaintext (historical),
-- 1 = AES-256-GCM. Decrypt only runs GCM when version=1.
--
-- All existing rows are assumed plaintext (version=0). Operators with a
-- long-running encrypted install can re-run an opt-in migration later to
-- bump rows that actually decrypt — out of scope here.

ALTER TABLE workspace_secrets
    ADD COLUMN IF NOT EXISTS encryption_version INT NOT NULL DEFAULT 0;

ALTER TABLE global_secrets
    ADD COLUMN IF NOT EXISTS encryption_version INT NOT NULL DEFAULT 0;
