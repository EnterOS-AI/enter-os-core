-- #124 — delegation idempotency.
--
-- Adds an optional idempotency_key column on activity_logs so callers
-- (notably agents that may retry after a container-restart race) can
-- safely re-issue the same delegation without producing duplicate work.
--
-- The unique index is partial — only enforced for non-NULL keys so the
-- column is fully backwards compatible with existing rows.

ALTER TABLE activity_logs
    ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS activity_logs_idempotency_uniq
    ON activity_logs (workspace_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
