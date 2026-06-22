-- 031_memories_pgvector.down.sql
DROP INDEX IF EXISTS agent_memories_embedding_idx;
ALTER TABLE agent_memories DROP COLUMN IF EXISTS embedding;
