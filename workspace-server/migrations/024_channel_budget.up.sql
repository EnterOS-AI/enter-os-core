-- Per-channel message budget (#368).
-- NULL means no limit. When message_count reaches channel_budget, the Send
-- handler returns 429 {"error":"channel budget exceeded"} and rejects further
-- outbound messages until the budget is cleared or raised.
ALTER TABLE workspace_channels
    ADD COLUMN IF NOT EXISTS channel_budget INTEGER DEFAULT NULL;
