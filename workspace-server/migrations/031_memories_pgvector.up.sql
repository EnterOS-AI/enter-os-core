-- 031_memories_pgvector.up.sql
--
-- Adds a dense-vector embedding column to agent_memories to power semantic
-- (cosine-similarity) memory recall alongside the existing FTS path.
--
-- Requires the pgvector Postgres extension. The DO block is a no-op guard:
-- if the extension is unavailable this migration exits early so a boot
-- without pgvector installed does not break the migration sweep.
--
-- Issue: #576

DO $migrate$
BEGIN
  CREATE EXTENSION IF NOT EXISTS vector;

  -- Nullable: rows written before pgvector is active have NULL embedding and
  -- are excluded from cosine-similarity queries automatically.
  ALTER TABLE agent_memories ADD COLUMN IF NOT EXISTS embedding vector(1536);

  -- ivfflat approximate nearest-neighbour index for cosine similarity.
  -- lists=100 is a reasonable default for tables up to ~1M rows.
  CREATE INDEX IF NOT EXISTS agent_memories_embedding_idx
    ON agent_memories USING ivfflat (embedding vector_cosine_ops)
    WHERE embedding IS NOT NULL;

EXCEPTION WHEN OTHERS THEN
  RAISE NOTICE 'pgvector not available — 031_memories_pgvector skipped: %', SQLERRM;
END $migrate$;
