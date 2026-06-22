-- Reverse issue #2560 ingest-row column + idempotency index.
-- Non-destructive: dropping the index + column simply reverts the
-- chat-history-mid-turn-persistence feature; no data is lost because
-- the original logA2ASuccess / logA2AReceiveQueued paths are still
-- intact (they re-INSERT the full message on completion in the
-- non-message_id-keyed shape).
DROP INDEX IF EXISTS idx_activity_logs_msg_id;
ALTER TABLE IF EXISTS activity_logs DROP COLUMN IF EXISTS message_id;
