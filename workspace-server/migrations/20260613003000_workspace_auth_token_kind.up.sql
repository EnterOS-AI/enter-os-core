-- Token KINDS: 'instance' (held by the workspace runtime; minted by register
-- bootstrap / docker-mode inject / external pre-register) vs 'api' (held by
-- platform callers; minted by POST /workspaces 201, TokenHandler.Create, the
-- admin first-bearer endpoint).
--
-- Why (core#1644): provisioning revokes tokens so the fresh instance can
-- bootstrap-register, but that clobbered the api token the Create 201 had
-- just returned (PR#1669) -- the platform broke its own API contract.
-- With kinds, provision revokes ONLY instance tokens and the bootstrap
-- allowance keys on live INSTANCE tokens, so caller bearers survive.
--
-- Existing rows default to 'instance' (today's behavior, runtime-held).
-- Idempotent: the migration runner re-applies all files every boot.
ALTER TABLE workspace_auth_tokens
    ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'instance';
