-- Rename workspace_secrets rows MODEL_PROVIDER → MODEL.
--
-- Root cause: the column-name MODEL_PROVIDER was misleading — it never
-- held a provider slug, only a picked model id (e.g.
-- "minimax/MiniMax-M2.7"). Application code (workspace-server
-- applyRuntimeModelEnv) read MODEL_PROVIDER as a fallback that could
-- overwrite a legitimate MODEL persona-env secret with whatever literal
-- string lived in MODEL_PROVIDER — often a provider slug like "minimax"
-- or a runtime name like "claude-code", neither of which is a valid
-- model id. The wrong shape then propagated into CP user-data and the
-- workspace adapter wedged at SDK initialize (see failed-workspace
-- 95ed3ff2 2026-05-02 and the Researcher/Reviewer poisoning 2026-05-19).
--
-- Pairs with the secrets.go + workspace_provision.go rename in this
-- PR (fix/workspace-server-rename-MODEL_PROVIDER-to-MODEL) and the
-- CP-side slot-separation already landed in cp#213 + cp#220.
--
-- Conflict handling: a workspace_secrets row already keyed MODEL takes
-- precedence (persona-env files commonly write MODEL=... directly), so
-- the MODEL_PROVIDER row is deleted instead of overwriting MODEL. The
-- WHERE NOT EXISTS guard makes the migration idempotent — re-running
-- it on an already-renamed schema is a no-op.

UPDATE workspace_secrets
   SET key = 'MODEL', updated_at = now()
 WHERE key = 'MODEL_PROVIDER'
   AND NOT EXISTS (
       SELECT 1 FROM workspace_secrets ws2
        WHERE ws2.workspace_id = workspace_secrets.workspace_id
          AND ws2.key = 'MODEL'
   );

-- Drop any leftover MODEL_PROVIDER rows where a MODEL row already
-- exists (MODEL wins — see above).
DELETE FROM workspace_secrets
 WHERE key = 'MODEL_PROVIDER';
