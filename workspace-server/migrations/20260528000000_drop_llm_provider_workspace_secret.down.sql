-- Reverse of 20260528000000: a no-op.
--
-- The LLM_PROVIDER rows were retired with no remaining consumer.
-- Rolling back the migration cannot reconstitute the rows (they were
-- deleted, not soft-deleted) AND there is no live code path that
-- writes them anymore — SetProvider / setProviderSecret / Create's
-- write are all removed. A genuine revert needs an application-code
-- revert, not just a migration.
SELECT 1;
