-- Per-workspace LLM token usage tracking (#593 — canvas cost transparency).
-- Stores UTC-day aggregates upserted by the A2A proxy after each LLM call.
-- estimated_cost_usd is computed server-side using fixed per-model rates
-- (default: Claude Sonnet input $3/1M, output $15/1M).
CREATE TABLE IF NOT EXISTS workspace_token_usage (
  id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id       UUID         NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  period_start       TIMESTAMPTZ  NOT NULL,
  input_tokens       BIGINT       NOT NULL DEFAULT 0,
  output_tokens      BIGINT       NOT NULL DEFAULT 0,
  call_count         INTEGER      NOT NULL DEFAULT 0,
  estimated_cost_usd NUMERIC(12,6) NOT NULL DEFAULT 0,
  updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS workspace_token_usage_ws_period
  ON workspace_token_usage(workspace_id, period_start);
