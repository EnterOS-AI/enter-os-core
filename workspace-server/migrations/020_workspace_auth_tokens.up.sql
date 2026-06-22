-- Phase 30.1 — per-workspace authentication tokens.
--
-- Each workspace gets one or more opaque tokens issued at register time.
-- The token travels with the agent (env var for local containers, rendered
-- into the SDK config for remote agents) and must be presented on every
-- subsequent request: heartbeat, update-card, secrets pull, A2A as caller.
--
-- Plaintext tokens are NEVER stored — we keep sha256(token) and a short
-- prefix for display/debug. This mirrors how we store global API keys.

CREATE TABLE IF NOT EXISTS workspace_auth_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    token_hash   BYTEA NOT NULL,
    prefix       TEXT  NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    UNIQUE (token_hash)
);

-- Fast lookup of a workspace's live tokens (revoked_at IS NULL) without
-- scanning the full table. Used by the middleware on every authenticated
-- request.
CREATE INDEX IF NOT EXISTS workspace_auth_tokens_live_idx
    ON workspace_auth_tokens (workspace_id)
    WHERE revoked_at IS NULL;
