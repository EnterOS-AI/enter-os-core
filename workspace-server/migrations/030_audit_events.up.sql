-- 029_audit_events.up.sql
-- Append-only HMAC-chained agent event log for EU AI Act Annex III compliance.
-- Art. 12 record-keeping + Art. 13 transparency.
--
-- Each row is signed with HMAC-SHA256 and chained to the preceding row for
-- the same agent_id via prev_hmac, making the log tamper-evident.
-- See: molecule_audit/ledger.py and platform/internal/handlers/audit.go

CREATE TABLE IF NOT EXISTS audit_events (
    id                   TEXT        NOT NULL,
    timestamp            TIMESTAMPTZ NOT NULL,
    agent_id             TEXT        NOT NULL,
    session_id           TEXT        NOT NULL,
    operation            TEXT        NOT NULL,   -- task_start|llm_call|tool_call|task_end
    input_hash           TEXT,                   -- SHA-256 of input (privacy-preserving)
    output_hash          TEXT,                   -- SHA-256 of output
    model_used           TEXT,                   -- gen_ai.request.model or tool name
    human_oversight_flag BOOLEAN     NOT NULL DEFAULT false,
    risk_flag            BOOLEAN     NOT NULL DEFAULT false,
    prev_hmac            TEXT,                   -- HMAC of prior row for this agent_id
    hmac                 TEXT        NOT NULL,   -- HMAC of this row's canonical JSON
    workspace_id         UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    CONSTRAINT audit_events_pkey PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_audit_events_agent_id   ON audit_events (agent_id);
CREATE INDEX IF NOT EXISTS idx_audit_events_session_id ON audit_events (session_id);
CREATE INDEX IF NOT EXISTS idx_audit_events_workspace  ON audit_events (workspace_id);
CREATE INDEX IF NOT EXISTS idx_audit_events_timestamp  ON audit_events (timestamp DESC);
