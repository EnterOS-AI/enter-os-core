-- Enforce AT MOST ONE platform agent (org root) per tenant DB — race-proof, in
-- pure SQL. (RFC: docs/design/rfc-platform-agent.md; security fix.)
--
-- The prior 20260606000000_workspaces_kind migration added
-- workspaces_platform_root_check = CHECK (kind <> 'platform' OR parent_id IS NULL),
-- which only enforces "a platform row must be parentless". It does NOT prevent a
-- SECOND parentless platform row. That gap is exploitable: POST /registry/register
-- is bootstrap-allowed for a fresh workspace id, and the upsert wrote the
-- caller-supplied kind — so an ordinary in-VPC workspace could register a new UUID
-- as {"kind":"platform"} (parent_id defaults NULL → CHECK satisfied), mint itself a
-- second org root, then POST /workspaces/:id/restart it. The shared provision path
-- injects the tenant org-admin credential (MOLECULE_API_KEY=ADMIN_TOKEN) into any
-- kind='platform' workspace, so that rogue root would gain full org-admin reach —
-- a privilege escalation past the "only the concierge gets the org MCP + admin
-- token" invariant.
--
-- A partial UNIQUE index over (kind) WHERE kind='platform' permits exactly one
-- platform row table-wide. Because the per-tenant DB is single-org, that is "one
-- platform agent per org". The legitimate install paths (InstallPlatformAgent /
-- EnsureSelfHostedPlatformAgent) upsert the platform row by its fixed id, so they
-- are unaffected; only a SECOND, differently-id'd platform row is rejected (23505),
-- which the Register handler maps to a friendly 409. The handler also rejects the
-- create/promote-via-register at the app layer; this index is the structural
-- backstop that holds regardless of code path.
CREATE UNIQUE INDEX IF NOT EXISTS uniq_workspaces_one_platform_root
  ON workspaces (kind)
  WHERE kind = 'platform';
