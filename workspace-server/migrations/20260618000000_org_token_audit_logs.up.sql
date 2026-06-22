-- org_token_audit_logs: append-only audit trail for org API token lifecycle.
-- Captures mint, revoke, and validation-failure events so operators can
-- answer "who minted what, when" and detect brute-force / replay attempts.
--
-- token_id is nullable because a validate_fail event may reference a
-- token that never existed or was already revoked (we only know the hash
-- or plaintext prefix, not the UUID).
CREATE TABLE IF NOT EXISTS org_token_audit_logs (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    token_id    UUID        REFERENCES org_api_tokens(id) ON DELETE SET NULL,
    action      TEXT        NOT NULL,                       -- 'mint' | 'revoke' | 'validate_fail'
    actor       TEXT        NOT NULL,                       -- e.g. 'admin-token', 'session:<workos_id>', 'org-token:<prefix>'
    org_id      UUID        REFERENCES workspaces(id) ON DELETE SET NULL,
    ip_address  TEXT,
    user_agent  TEXT,
    metadata    JSONB,
    created_at  TIMESTAMPTZ   NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_org_token_audit_logs_token_id  ON org_token_audit_logs (token_id);
CREATE INDEX IF NOT EXISTS idx_org_token_audit_logs_action    ON org_token_audit_logs (action);
CREATE INDEX IF NOT EXISTS idx_org_token_audit_logs_actor     ON org_token_audit_logs (actor);
CREATE INDEX IF NOT EXISTS idx_org_token_audit_logs_created_at ON org_token_audit_logs (created_at DESC);
