DROP INDEX IF EXISTS activity_logs_idempotency_uniq;
ALTER TABLE activity_logs DROP COLUMN IF EXISTS idempotency_key;
