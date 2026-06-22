-- 20260531000000_rescue_bundles.up.sql — RFC internal#742 Part 3.
--
-- A queryable, post-mortem-inspectable copy of the rescue bundle that
-- Part 2 (internal/rescue.Capture) collects off a boot-failed workspace
-- EC2 before the control plane reaps it.
--
-- WHY a DB table (the Part 3 read-path decision):
--   Part 2 ships the bundle via internal/audit (audit.Emit), which is
--   stdout→Vector→Loki + a best-effort local JSONL on the tenant
--   container's EPHEMERAL rootfs — NOT a queryable store. Serving
--   GET /workspaces/:id/rescue from Loki would require giving the
--   tenant process a Loki *query* client + obs read creds, which it
--   deliberately does not have (and must not — RFC internal#742 keeps
--   obs read creds out of tenants). So Part 3 ALSO persists the
--   already-redacted bundle to this small per-tenant table on capture,
--   and the read endpoint serves the latest row. The Loki stream
--   remains the cross-tenant operator firehose; this table is the
--   tenant-local, org-scoped read surface that powers the future
--   canvas "Why did this fail?" panel.
--
-- REDACTION: the `sections` payload written here is the SAME content
-- the Loki ship loop emits — i.e. already run through the SAFE-T1201
-- secret-scan (handlers.redactSecrets) at capture time. This table
-- never holds raw tokens; the read endpoint returns the stored content
-- verbatim without re-redacting.
--
-- ORG SCOPING: org_id is denormalized onto the row so the read handler
-- can filter by (workspace_id, org_id) and a row whose org doesn't
-- match the tenant's MOLECULE_ORG_ID is never returned — defense in
-- depth behind TenantGuard (which already 404s cross-org requests at
-- the routing layer).
--
-- RETENTION: bounded by RescueVolumeGrace semantics on the capture
-- side; rows are small (a redacted forensic blob, capped at capture).
-- A future sweeper can prune rows past the grace window — out of scope
-- for Part 3; the table is append-only here.

CREATE TABLE IF NOT EXISTS rescue_bundles (
    id            BIGSERIAL    PRIMARY KEY,
    workspace_id  TEXT         NOT NULL,
    org_id        TEXT         NOT NULL DEFAULT '',
    instance_id   TEXT         NOT NULL DEFAULT '',
    reason        TEXT         NOT NULL DEFAULT '',
    captured_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- sections is the ordered, already-redacted bundle:
    --   [{ "name": "config.yaml", "content": "...", "redacted": true }, ...]
    -- Stored as JSONB so the read handler returns it as a structured map
    -- and a future query can index into a single section if needed.
    sections      JSONB        NOT NULL DEFAULT '[]'::jsonb
);

-- Read hot path: "latest bundle for this workspace" — the only query
-- the GET /workspaces/:id/rescue endpoint runs.
--   SELECT ... WHERE workspace_id = $1 [AND org_id = $2]
--   ORDER BY captured_at DESC, id DESC LIMIT 1
-- Partial-free composite index; (workspace_id, captured_at DESC) covers
-- the filter + ordering. id DESC tiebreaks same-timestamp captures.
CREATE INDEX IF NOT EXISTS idx_rescue_bundles_ws_captured
    ON rescue_bundles (workspace_id, captured_at DESC, id DESC);
