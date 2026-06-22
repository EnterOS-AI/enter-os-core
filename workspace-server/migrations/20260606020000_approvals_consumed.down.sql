-- Reverse the approval-gate single-use/dedup columns.
DROP INDEX IF EXISTS approval_requests_gate_idx;
ALTER TABLE approval_requests
  DROP COLUMN IF EXISTS request_hash,
  DROP COLUMN IF EXISTS consumed_at;
