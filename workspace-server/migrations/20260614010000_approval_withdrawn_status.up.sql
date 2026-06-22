-- Add 'withdrawn' to the approval_requests.status CHECK constraint.
-- This is the additive + reversible counterpart to issue #66's
-- requester-initiated withdraw endpoint (POST /workspaces/:id/approvals/:approvalId/withdraw).
--
-- Why a new value (vs. reusing 'denied'):
--   - 'denied' is approver-initiated (a human Decides the request is wrong).
--   - 'withdrawn' is requester-initiated (the agent that raised the
--     approval decides it no longer needs the destructive op and pulls
--     the request back before any approver acts on it).
--   - Collapsing them would lose the audit signal: 'why did this approval
--     disappear' is a load-bearing question for the human approver who
--     sees the inbox change. Distinguishing the two paths lets the
--     events log + future analytics separate the two intent classes.
--
-- Why additive + reversible:
--   - The migration only widens the CHECK enum; existing rows are
--     untouched (no existing row is in 'withdrawn' before the
--     endpoint exists).
--   - The down migration narrows the CHECK back to the original 4-value
--     enum AND deletes any 'withdrawn' rows, so a rollback is safe
--     even if the endpoint has been exercised in the meantime
--     (acceptable loss: the audit history of a row we just rewrote
--     in this deploy window).
--   - PM/Researcher guardrail (7600d2ed): migration must be additive
--     + reversible so the change can be held in the deploy pipeline
--     without locking the table in a partial state.
ALTER TABLE approval_requests
    DROP CONSTRAINT IF EXISTS approval_requests_status_check;

ALTER TABLE approval_requests
    ADD CONSTRAINT approval_requests_status_check
    CHECK (status IN ('pending', 'approved', 'denied', 'escalated', 'withdrawn'));

-- Index for the "pending + recent first" query path that ListAll /
-- List use (existing idx_approvals_status covers the prefix; the
-- partial index is unnecessary — the existing btree is fine for the
-- current row count).
