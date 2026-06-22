DROP INDEX IF EXISTS idx_a2a_queue_next_attempt_at;
ALTER TABLE a2a_queue DROP COLUMN IF EXISTS next_attempt_at;
