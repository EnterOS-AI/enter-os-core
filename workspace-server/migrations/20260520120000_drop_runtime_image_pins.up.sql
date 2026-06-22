-- Task #335 / RFC internal#617 — drop molecule-core's dead runtime_image_pins
-- table. CP (molecule-controlplane migrations/027_runtime_image_pins.up.sql)
-- is the single SSOT for runtime image digest pins.
--
-- Empirical state at the time of this migration (a6e3ff018 finding,
-- 2026-05-20): no code in any molecule-ai repo INSERTs or UPDATEs this
-- table. The reader in workspace-server/internal/handlers/runtime_image_pin.go
-- has been hitting sql.ErrNoRows on every single workspace provision since
-- mig 047 landed (PR #2276) — silently falling through to the legacy
-- :latest path. Functionally indistinguishable from removing the call entirely.
--
-- CP's parallel-named table (CP mig 027) has the writer, reader, hard-gate
-- (RFC internal#541 Step 2), seeded post-suspension digests (CP mig 028),
-- and admin endpoints. CP is now the de-facto SSOT and this migration just
-- ratifies that reality by removing the unused copy.
--
-- CARE ZONE: migration 047 ALSO added `workspaces.runtime_image_digest TEXT`
-- and `idx_workspaces_runtime_image_digest`. Per RFC internal#617 §3, that
-- column is earmarked for the canvas admin's stale-workspaces panel
-- (workspaces still on an old digest after a CP-side promotion). It has no
-- current consumer but the cost of keeping it is one nullable column + a
-- partial index, and dropping it is a separate decision out of scope here.
-- DO NOT touch the column or its index in this migration.

DROP TABLE IF EXISTS runtime_image_pins;

