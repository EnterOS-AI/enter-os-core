-- Reverse of 20260519000000_workspace_secrets_model_provider_rename.up.sql.
--
-- This rolls MODEL → MODEL_PROVIDER. Note: the up migration deleted any
-- conflicting MODEL_PROVIDER rows when a MODEL row already existed, so
-- this down migration is intentionally lossy in that direction — it
-- cannot reconstruct rows the up migration discarded. Acceptable
-- because:
--
--   1. The discarded rows were duplicates with the same workspace_id;
--      the surviving MODEL row carries the correct semantic value.
--   2. The application code post-rename never writes MODEL_PROVIDER, so
--      any rollback after live traffic would produce duplicate-key
--      conflicts on re-up anyway — discarding here is the only sane
--      shape.
--
-- Provided for migration-tool symmetry; in practice the up direction is
-- the canonical fix and rollback should not happen.

UPDATE workspace_secrets
   SET key = 'MODEL_PROVIDER', updated_at = now()
 WHERE key = 'MODEL';
