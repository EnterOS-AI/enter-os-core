-- Participant-kind discriminator for the org-level platform agent.
-- (RFC: docs/design/rfc-platform-agent.md)
--
-- 'workspace' (default) = an ordinary workspace / agent.
-- 'platform'            = the org-level concierge (the "platform agent"). It is
--                         the single org root (parent_id IS NULL) and the user's
--                         default A2A chat target. Exactly one per org.
--
-- There is no org_id column — an "org" is the parent_id-chain root resolved by
-- org_scope.go (orgRootID/sameOrg). "platform == org root" and "one platform
-- agent per org" are therefore enforced in the Register/create handlers, not in
-- pure SQL. This column is only the discriminator (default-target / billing
-- exclusion / UX), defined once here and mirrored by the Go constants
-- models.KindWorkspace / models.KindPlatform.
--
-- Backward-compatible: every existing row defaults to 'workspace'. The CHECK is
-- added NOT VALID then validated so the ALTER can never fail on legacy data.
ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'workspace';

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workspaces_kind_check') THEN
    ALTER TABLE workspaces
      ADD CONSTRAINT workspaces_kind_check CHECK (kind IN ('workspace', 'platform')) NOT VALID;
    ALTER TABLE workspaces VALIDATE CONSTRAINT workspaces_kind_check;
  END IF;
END $$;

-- platform == org root, enforced at the DB level (race-proof). A platform agent
-- MUST have parent_id IS NULL. NOTE: this CHECK alone does NOT bound the number
-- of platform rows — it permits multiple parentless platform roots. "At most one
-- platform agent per org" is enforced by the partial unique index added in
-- 20260607000000_one_platform_root (uniq_workspaces_one_platform_root); see that
-- migration for the privilege-escalation rationale. The handler additionally
-- pre-checks both to return a friendly 409 instead of a raw 23514/23505.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workspaces_platform_root_check') THEN
    ALTER TABLE workspaces
      ADD CONSTRAINT workspaces_platform_root_check
      CHECK (kind <> 'platform' OR parent_id IS NULL) NOT VALID;
    ALTER TABLE workspaces VALIDATE CONSTRAINT workspaces_platform_root_check;
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_workspaces_kind ON workspaces(kind);
