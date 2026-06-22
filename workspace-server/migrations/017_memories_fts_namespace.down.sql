-- Reverse of 017_memories_fts_namespace.up.sql
DROP INDEX IF EXISTS idx_memories_ns;
DROP INDEX IF EXISTS idx_memories_fts;
ALTER TABLE agent_memories DROP COLUMN IF EXISTS content_tsv;
ALTER TABLE agent_memories DROP COLUMN IF EXISTS namespace;
