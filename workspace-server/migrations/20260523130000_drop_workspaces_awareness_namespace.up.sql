-- Issue #1735 — drop the workspaces.awareness_namespace column.
--
-- "Awareness namespaces" were a memory-routing surface (env vars
-- AWARENESS_URL / AWARENESS_NAMESPACE) that was plumbed across the
-- platform but never wired in any production or staging environment
-- (verified 2026-05-23 via Railway GraphQL on the controlplane service:
-- AWARENESS_* unset in both env IDs 59227671-… and 639539ec-…).
--
-- The column added by migration 010_workspace_awareness.sql was only
-- ever populated with the canonical "workspace:<id>" string, which is
-- also the v2 memory namespace string (see internal/memory/namespace/
-- resolver.go:186). Removing the column does not change any agent-
-- visible memory namespace — handlers now compute the same
-- "workspace:<id>" string inline when inserting into agent_memories.
--
-- Related: #1733 (memory SSOT consolidation), #1734 (Memory tab bug).

ALTER TABLE workspaces
    DROP COLUMN IF EXISTS awareness_namespace;
