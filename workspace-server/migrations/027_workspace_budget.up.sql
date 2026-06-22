-- Per-workspace monthly budget limit (#541).
-- NULL means no limit. When monthly_spend reaches budget_limit, the A2A
-- proxy returns 402 {"error":"workspace budget limit exceeded"} and rejects
-- further A2A calls until budget_limit is raised or monthly_spend is reset.
--
-- Units: USD cents (integer). budget_limit=500 means $5.00/month.
-- monthly_spend is updated by the workspace via the heartbeat endpoint;
-- agents report their accumulated LLM API cost each heartbeat cycle.
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS budget_limit   BIGINT DEFAULT NULL,
    ADD COLUMN IF NOT EXISTS monthly_spend  BIGINT NOT NULL DEFAULT 0;
