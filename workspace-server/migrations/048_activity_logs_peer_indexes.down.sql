-- Reverse 048_activity_logs_peer_indexes.up.sql.
-- Drops the partial peer-conversation indexes added there.
-- chat_history queries fall back to the existing idx_activity_ws_type_time
-- + workspace-scoped seq scan / filter on the OR clause.

DROP INDEX IF EXISTS idx_activity_ws_target;
DROP INDEX IF EXISTS idx_activity_ws_source;
