-- Organization-scoped API tokens.
--
-- Unlike workspace_auth_tokens (which bind a bearer to a single
-- workspace UUID), these grant admin-level access to everything on
-- the tenant platform: all workspaces, all settings, all admin
-- endpoints. One token = full org admin.
--
-- Designed for the beta growth phase:
--   - Mint named tokens from the canvas UI (settings → API keys)
--   - Hand the plaintext to an agent, CLI, or external integration
--   - Revoke with one click; compromised token is immediately dead
--
-- This is the user-visible replacement for the single ADMIN_TOKEN
-- env var. ADMIN_TOKEN still works (CLI + bootstrap flows), but
-- operators prefer these because they're named, revocable, and
-- audited. Future work: role-scoping (ADMIN vs READ-ONLY vs
-- WORKSPACE-WRITER) — for now every token is full-admin.
--
-- Plaintext NEVER stored — sha256 hash + prefix only. Matches the
-- workspace_auth_tokens pattern so tooling that handles one works
-- for the other.
CREATE TABLE IF NOT EXISTS org_api_tokens (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash    BYTEA NOT NULL,
    prefix        TEXT  NOT NULL, -- first 8 plaintext chars for UI display
    name          TEXT,           -- user-supplied label ("zapier", "my-ci", ...)
    created_by    TEXT,           -- WorkOS user_id who minted it (nullable: ADMIN_TOKEN/CLI path)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    UNIQUE (token_hash)
);

-- Hot path: every authed request that arrives with a bearer runs
-- SELECT id FROM org_api_tokens WHERE token_hash=? AND revoked_at IS NULL.
-- Partial index keeps live-token lookups O(log live) instead of
-- O(log all-tokens-ever-minted).
CREATE INDEX IF NOT EXISTS org_api_tokens_live_idx
    ON org_api_tokens (token_hash)
    WHERE revoked_at IS NULL;
