-- Multi-period per-workspace LLM budget (hourly/daily/weekly/monthly).
-- Extends the single monthly budget_limit (027). `budget_limits` is the SSOT
-- for the per-period ceilings: a JSONB map {"hourly":N,"daily":N,"weekly":N,
-- "monthly":N} in USD cents; a key that is absent or null = no limit for that
-- period. Per-period SPEND is computed from the workspace_spend_events ledger
-- over a rolling window (NOT the legacy self-reported monthly_spend cumulative,
-- which can't express sub-month periods).
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS budget_limits JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Backfill: carry an existing monthly ceiling into the new map so the feature
-- is continuous across the rollout (027's budget_limit stays for back-compat).
UPDATE workspaces
   SET budget_limits = jsonb_build_object('monthly', budget_limit)
 WHERE budget_limit IS NOT NULL
   AND NOT (budget_limits ? 'monthly');

-- Server-owned spend ledger: one row per heartbeat-observed spend INCREMENT
-- (delta = new cumulative - prev). Per-period spend =
--   SUM(delta_cents) WHERE workspace_id=$1 AND occurred_at > now() - <window>.
-- Makes the SERVER the SSOT for windowing; the agent keeps reporting its
-- cumulative figure unchanged (the heartbeat derives the delta).
CREATE TABLE IF NOT EXISTS workspace_spend_events (
    id           BIGSERIAL PRIMARY KEY,
    workspace_id TEXT        NOT NULL,
    delta_cents  BIGINT      NOT NULL CHECK (delta_cents > 0),
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_workspace_spend_events_ws_time
    ON workspace_spend_events (workspace_id, occurred_at DESC);
