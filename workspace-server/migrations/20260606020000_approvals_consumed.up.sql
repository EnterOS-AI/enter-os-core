-- Single-use + dedup support for the destructive-op approval gate.
-- (RFC docs/design/rfc-platform-agent.md — Phase 4)
--
-- consumed_at: an approval is single-use. Once a destructive op consumes an
--   approved request, consumed_at is stamped so the same approval can't be
--   replayed for a second destructive call.
-- request_hash: a stable hash of (workspace_id, action, context) so a repeated
--   destructive attempt matches its own pending/approved request instead of
--   flooding the table with duplicates.
ALTER TABLE approval_requests
  ADD COLUMN IF NOT EXISTS consumed_at  TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS request_hash TEXT;

-- Hot path: the gate looks up an approved + unconsumed row matching
-- (workspace_id, action, request_hash). Partial index keeps that O(log live).
CREATE INDEX IF NOT EXISTS approval_requests_gate_idx
  ON approval_requests (workspace_id, action, request_hash)
  WHERE status = 'approved' AND consumed_at IS NULL;
