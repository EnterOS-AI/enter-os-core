-- Memory namespace + full-text search (outcomes doc Top-5 #1).
--
-- Additive only: existing rows get namespace = 'general' automatically;
-- content_tsv is a generated column so Postgres maintains it; both indexes
-- are optional from the query side. No breaking changes for existing
-- handlers or stored memories.

ALTER TABLE agent_memories
    ADD COLUMN IF NOT EXISTS namespace   VARCHAR(50) NOT NULL DEFAULT 'general',
    ADD COLUMN IF NOT EXISTS content_tsv tsvector
        GENERATED ALWAYS AS (to_tsvector('english', content)) STORED;

-- GIN index drives ts_rank ORDER BY on content_tsv @@ plainto_tsquery
CREATE INDEX IF NOT EXISTS idx_memories_fts
    ON agent_memories USING GIN(content_tsv);

-- Composite index supports filter-by-namespace within a workspace
CREATE INDEX IF NOT EXISTS idx_memories_ns
    ON agent_memories(workspace_id, namespace);
