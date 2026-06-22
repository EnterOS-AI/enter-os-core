-- Drop chat_session_started_at. Reversible: no other table references
-- the column, and pre-PR chat-history reads already ignored it
-- (no data loss on rollback — the filter simply goes back to being
-- absent).
ALTER TABLE workspaces
    DROP COLUMN IF EXISTS chat_session_started_at;
