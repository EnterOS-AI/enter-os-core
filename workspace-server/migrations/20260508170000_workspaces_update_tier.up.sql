-- workspaces.update_tier — canary vs production filter for plugin updates
-- (core#115). Composes with the version-subscription DB foundation
-- (core#113, merged) and the upcoming drift+queue+apply endpoint
-- (core#123).
--
-- Tiers:
--   'production' (default) — fan-out target; only updated AFTER canary soak
--   'canary'               — early-adopter target; updates land here first
--
-- Default 'production' so existing customers (Reno-Stars + any future
-- live tenant) are default-safe. Synthetic dogfooding workspaces opt
-- INTO 'canary' explicitly.
--
-- The column is just metadata at this layer; the actual filter logic
-- ('apply this update only to canary tier first') lives in the future
-- POST /admin/plugin-updates/:id/apply endpoint (core#123).

ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS update_tier TEXT NOT NULL DEFAULT 'production'
    CHECK (update_tier IN ('canary', 'production'));

-- Partial index for the apply endpoint's canary-tier scan: only
-- index canary rows since the apply path queries them most often
-- and the production set is the much larger default.
CREATE INDEX IF NOT EXISTS workspaces_update_tier_canary
  ON workspaces(update_tier) WHERE update_tier = 'canary';
