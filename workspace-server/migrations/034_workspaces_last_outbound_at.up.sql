-- Issue #817 (sub of #795): track last outbound A2A activity per workspace so
-- PM/Dev Lead can detect workspaces that have gone silent despite being online.
-- The orchestrator compares this against now() in its pulse; > 2 hours with an
-- active cron triggers a phantom-busy warning.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS last_outbound_at TIMESTAMPTZ;
