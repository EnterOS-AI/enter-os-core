-- 20260519200000_pending_uploads_bump_size_cap.down.sql
--
-- Restores the pre-bump 25 MB (26214400) CHECK on
-- pending_uploads.size_bytes.
--
-- DESTRUCTIVE on existing data: any row with size_bytes > 26214400
-- will cause the ALTER TABLE … ADD CONSTRAINT below to fail (Postgres
-- validates the new CHECK against every existing row before applying
-- it). That is the correct behaviour — silently dropping such rows
-- would lose evidence-of-receipt for files the workspace might still
-- pull. Operator running this rollback MUST first decide what to do
-- with any 25–100 MB rows (sweep them out via the normal expires_at
-- path, or accept failed rollback + investigate).
--
-- In practice this DOWN is for migration-tool symmetry only; the
-- pending_uploads table has a 24h hard TTL via expires_at, so waiting
-- one cycle drains any oversized rows naturally.

ALTER TABLE pending_uploads
    DROP CONSTRAINT IF EXISTS pending_uploads_size_bytes_check;

ALTER TABLE pending_uploads
    ADD CONSTRAINT pending_uploads_size_bytes_check
    CHECK (size_bytes > 0 AND size_bytes <= 26214400);
