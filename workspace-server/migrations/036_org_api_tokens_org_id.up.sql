-- Add org_id column to org_api_tokens (#1200 / F1094).
--
-- Rationale: requireCallerOwnsOrg (org_plugin_allowlist.go:116) was
-- reading created_by to determine the caller's org. But created_by
-- is a provenance label ("session", "admin-token", "org-token:<prefix>"),
-- NOT an org UUID — so every org-token caller got a non-UUID string
-- and the equality check callerOrg != targetOrgID always failed,
-- causing 403 on every org-token request.
--
-- Migration plan:
--  1. Add org_id (nullable) — existing tokens have no org anchor yet
--  2. All new tokens minted via POST /org/tokens are written with org_id
--  3. Backfill: tokens minted via session → look up session's org workspace
--     (the session-auth workspace ID is known via the CP /cp/auth/tenant-member
--      response; the CP stores org_id in the session state). This requires
--      a separate admin script since the handler doesn't have that context.
--     Tokens minted via ADMIN_TOKEN or bootstrap → leave org_id NULL
--     (deny by default; operator must set via admin API if needed).
--  4. requireCallerOwnsOrg now reads org_id instead of created_by
--     (org_id NULL → treat as "no anchor" → deny by default)
--  5. Post-Fix: admin can backfill remaining tokens via
--     PATCH /org/tokens/:id with org_id set.
ALTER TABLE org_api_tokens
  ADD COLUMN org_id UUID REFERENCES workspaces(id) ON DELETE SET NULL;

-- Tokens created before this migration cannot be backfilled automatically
-- without knowing the session's org. Mark them as "unanchored" (org_id NULL)
-- so the auth fix denies by default — safer than permitting all old tokens.
-- Ops can backfill org_id for known tokens via PATCH /org/tokens/:id.
--
-- Index for fast org-anchor lookups (used by requireCallerOwnsOrg).
CREATE INDEX IF NOT EXISTS org_api_tokens_org_id_idx
  ON org_api_tokens (org_id) WHERE org_id IS NOT NULL;