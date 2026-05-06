-- TOCTOU backstop on workspaces(parent_id, name)
--
-- Origin: #2872 Critical 1 — /org/import had no per-tenant mutex,
-- advisory lock, or DB-level uniqueness, so two concurrent admin
-- POSTs (rapid double-click in canvas, retry-after-timeout, two
-- operators on the same template) both saw "not found" in
-- lookupExistingChild and both INSERT'd the same (parent_id, name)
-- row. Sweeper #2860 cleaned residual drift; this migration prevents
-- new collisions.
--
-- Why a partial index keyed on COALESCE(parent_id, sentinel):
--
--   - Postgres treats NULL ≠ NULL in a UNIQUE constraint, so root
--     workspaces (parent_id = NULL) would not collide pairwise even
--     if they shared the same name. COALESCE collapses NULLs to a
--     sentinel UUID so root collisions are caught.
--
--   - The `WHERE status != 'removed'` partial-index filter makes a
--     tombstoned row (collapsed team, deleted workspace) NOT block a
--     re-import using the same name, which preserves the existing
--     org-import semantics (lookupExistingChild already excludes
--     status='removed').
--
-- Why CONCURRENTLY:
--
--   - Builds the index without an ACCESS EXCLUSIVE lock on workspaces.
--     Production tenants serve live traffic during migration; a
--     blocking index build would cause request stalls.
--   - CONCURRENTLY MUST run outside a transaction. The migration
--     runner is configured to honour this (no BEGIN/COMMIT wrapper
--     around an idempotent CREATE INDEX CONCURRENTLY IF NOT EXISTS).
--   - IF NOT EXISTS makes this resumable: a partial build (e.g. CI
--     killed mid-flight) leaves an INVALID index; re-running CONCURRENTLY
--     after a manual REINDEX repairs without erroring on already-built.
--
-- Drift detection: companion test
-- workspaces_unique_parent_name_test.go pre-flights the index on a
-- live test DB to confirm the migration applied + the constraint is
-- enforceable.

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS workspaces_parent_name_uniq
  ON workspaces (
    COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid),
    name
  )
  WHERE status != 'removed';
