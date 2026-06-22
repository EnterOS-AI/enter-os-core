-- Rollback for Phase A3 — recreates the table EMPTY (no data restore).
-- Schema mirrors the pre-drop shape from migrations 010-018:
--   - 010_agent_memories: base table (id, workspace_id, content, created_at)
--   - 011_agent_memories_scope_namespace: added scope, namespace
--   - 017_memories_fts_namespace: added content_tsv (tsvector)
--   - 018_memories_embedding: added embedding (pgvector)
--
-- This rollback exists for migration-tool symmetry only. The Phase A2
-- backfill ran one-way; rollback would not restore data. If you need
-- the historical rows back, query memory_plugin.memory_records directly.

CREATE TABLE IF NOT EXISTS agent_memories (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    content     text NOT NULL,
    scope       varchar NOT NULL DEFAULT 'LOCAL',
    namespace   varchar NOT NULL DEFAULT 'general',
    created_at  timestamp with time zone NOT NULL DEFAULT now(),
    updated_at  timestamp with time zone NOT NULL DEFAULT now(),
    content_tsv tsvector,
    embedding   vector(1536)
);

CREATE INDEX IF NOT EXISTS idx_agent_memories_workspace_id ON agent_memories(workspace_id);
CREATE INDEX IF NOT EXISTS idx_agent_memories_scope ON agent_memories(scope);
