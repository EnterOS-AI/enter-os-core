-- Reverse of 20260506000000_workspaces_unique_parent_name.up.sql.
--
-- DROP CONCURRENTLY for the same reason CREATE was CONCURRENTLY:
-- avoid an ACCESS EXCLUSIVE lock during teardown on tenants under
-- live traffic. IF EXISTS makes this idempotent if the up-migration
-- never landed (e.g. resumed deploy on a fresh tenant).

DROP INDEX CONCURRENTLY IF EXISTS workspaces_parent_name_uniq;
