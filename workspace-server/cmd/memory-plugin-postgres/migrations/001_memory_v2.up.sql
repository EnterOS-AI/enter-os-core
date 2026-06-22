-- Memory v2 plugin schema (RFC #2728).
--
-- These tables are owned by the built-in postgres memory plugin, NOT
-- by workspace-server. When an operator swaps in a different memory
-- plugin (Pinecone, Letta, custom), these tables become orphaned —
-- not auto-dropped. Operator drops them when they're confident they
-- don't want to switch back.
--
-- Lives under cmd/memory-plugin-postgres/migrations/ (NOT
-- workspace-server/migrations/) to make the ownership boundary
-- visible: workspace-server has zero knowledge of these tables.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS memory_namespaces (
    name        TEXT PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('workspace','team','org','custom')),
    expires_at  TIMESTAMPTZ,
    metadata    JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS memory_records (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace    TEXT NOT NULL REFERENCES memory_namespaces(name) ON DELETE CASCADE,
    content      TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('fact','summary','checkpoint')),
    source       TEXT NOT NULL CHECK (source IN ('agent','runtime','user')),
    expires_at   TIMESTAMPTZ,
    propagation  JSONB,
    pin          BOOLEAN NOT NULL DEFAULT false,
    embedding    vector(1536),
    content_tsv  tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Indexes:
--  - namespace: every search filters by namespace list
--  - content_tsv: FTS path
--  - embedding: semantic search (partial because most rows have no embedding)
--  - expires_at: TTL janitor scans
CREATE INDEX IF NOT EXISTS idx_memory_records_namespace ON memory_records(namespace);
CREATE INDEX IF NOT EXISTS idx_memory_records_fts ON memory_records USING GIN (content_tsv);
CREATE INDEX IF NOT EXISTS idx_memory_records_embedding ON memory_records
    USING ivfflat (embedding) WHERE embedding IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_memory_records_expires ON memory_records (expires_at)
    WHERE expires_at IS NOT NULL;
