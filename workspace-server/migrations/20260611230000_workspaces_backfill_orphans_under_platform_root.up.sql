-- core#2609: backfill parent_id-NULL orphan workspaces under the org's
-- platform root.
--
-- The workspace-create path could (pre-fix) insert rows with parent_id NULL
-- when no explicit parent was given — e.g. every workspace the org concierge
-- provisioned through the management surface. A NULL-parent row is an orphan
-- ROOT outside the org subtree: A2A denies it ("workspaces cannot communicate
-- per hierarchy rules") and the canvas renders it depth-1 beside the root
-- (#2601). The create path now defaults parent_id to the platform root; this
-- backfill repairs rows created before the fix.
--
-- Guards (same semantics as the install path, which re-parents NULL roots
-- under the concierge):
--   * only runs when the DB has EXACTLY ONE live kind='platform' root —
--     a multi-root self-host DB is ambiguous and is left untouched;
--   * never touches the platform root itself or removed rows.
-- Idempotent: after the first run no qualifying NULL-parent rows remain.
UPDATE workspaces w
SET parent_id = r.id, updated_at = now()
FROM (
  SELECT id FROM workspaces
  WHERE COALESCE(kind, 'workspace') = 'platform' AND status != 'removed'
) r
WHERE w.parent_id IS NULL
  AND w.id <> r.id
  AND COALESCE(w.kind, 'workspace') <> 'platform'
  AND w.status != 'removed'
  AND (SELECT count(*) FROM workspaces
       WHERE COALESCE(kind, 'workspace') = 'platform' AND status != 'removed') = 1;
