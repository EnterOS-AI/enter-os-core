-- Issue #2560 (chat UX: persist in-flight exchange across leave/refresh):
-- the user chat message must be written to activity_logs AT RECEIPT, not
-- only at turn completion, so a mid-turn leave/refresh doesn't drop the
-- pending message.
--
-- The completion path (logA2ASuccess) reuses the SAME activity_logs row
-- via ON CONFLICT (workspace_id, message_id) DO UPDATE — the ingest row
-- carries the user message in request_body; the completion attach stamps
-- response_body + status onto it. Idempotent on messageId (per #2560 spec)
-- means a duplicated ingest (server retry, or a poll-mode write followed
-- by a push-mode write on the same messageId) is a no-op rather than a
-- duplicate bubble in chat-history.
--
-- Column:
--   message_id TEXT — extracted from params.message.messageId in
--   normalizeA2APayload. Stored as TEXT (UUID-as-text shape) so the same
--   string the canvas sent is the same string we dedupe on, regardless
--   of UUID canonicalization. NULL for legacy rows and for activity that
--   is NOT a per-message row (task_update, agent_log, error, etc.) — the
--   unique index is partial on message_id IS NOT NULL so non-message
--   rows never collide.
ALTER TABLE IF EXISTS activity_logs
    ADD COLUMN IF NOT EXISTS message_id TEXT;

-- Partial unique index — the conflict target for ON CONFLICT (workspace_id,
-- message_id) DO NOTHING / DO UPDATE. Excludes rows where message_id is
-- NULL (non-per-message activity) so existing activity never collides.
-- Coexists with the existing (workspace_id, activity_type, created_at DESC)
-- index — the partial is a write-path key, the existing is a read-path key.
CREATE UNIQUE INDEX IF NOT EXISTS idx_activity_logs_msg_id
    ON activity_logs (workspace_id, message_id)
    WHERE message_id IS NOT NULL;
