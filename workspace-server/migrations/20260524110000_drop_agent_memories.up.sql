-- Phase A3 (issue #1792): drop the legacy v1 memory table.
--
-- Pre-requisite (verified 2026-05-24 production): cmd/memory-backfill
-- ran on every active tenant and parity is exact between agent_memories
-- (frozen by #1794) and memory_plugin.memory_records (live writes go
-- here). 2,052 rows mirrored, zero errors.
--
-- After this migration:
--   - agent_memories table no longer exists on any tenant
--   - all writes go through the v2 plugin (post-#1747 + #1794)
--   - the legacy MemoriesHandler.Search/Update/Delete HTTP routes are
--     also gone in this PR — they're the only remaining v1 readers
--   - activity.go's session-search UNION no longer includes memories
--
-- Down migration recreates the table empty for rollback safety, but
-- without data — a rollback would not restore the pre-A2 content. A2
-- backfill ran one-way; the source-of-truth is now memory_plugin
-- exclusively.

DROP TABLE IF EXISTS agent_memories;
