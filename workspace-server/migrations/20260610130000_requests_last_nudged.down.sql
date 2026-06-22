-- Reverse of 20260610130000_requests_last_nudged.up.sql. IF EXISTS guards so
-- the down migration is safe whether or not requests / the column / the index
-- are present.
DROP INDEX IF EXISTS idx_requests_nudge_candidates;

ALTER TABLE IF EXISTS requests
    DROP COLUMN IF EXISTS last_nudged_at;
