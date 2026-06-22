-- 20260519200000_pending_uploads_bump_size_cap.up.sql
--
-- Bumps the pending_uploads.size_bytes CHECK from 25 MB (26214400) to
-- 100 MB (104857600) so poll-mode (laptop-runtime) workspaces accept the
-- same per-file cap as push-mode (SaaS EC2 tenants).
--
-- Why this is a separate PR from mc#1588:
--   mc#1588 bumped push-mode 50→100 MB across canvas TS +
--   workspace-server Go (chat_files.go cap) + workspace Python ingest
--   (internal_chat_uploads.py) + nginx harness mirror. The poll-mode
--   staging path is a different surface — the DB CHECK on
--   pending_uploads + the Go staging constant
--   (pendinguploads.MaxFileBytes) + the workspace-side puller
--   (workspace/inbox_uploads.py MAX_FILE_BYTES). Bundling both in one
--   PR would have crossed two RFCs' worth of review surface and
--   slowed the urgent push-mode fix that landed for acme-demo.
--
-- Why 100 MB (not higher / lower):
--   Aligns with mc#1588 (chat_files.go chatUploadMaxBytes,
--   internal_chat_uploads.py CHAT_UPLOAD_MAX_FILE_BYTES). The CTO
--   directive 2026-05-19 was a one-line "make it consistent" — no
--   evidence of a load-bearing reason the laptop path needs to stay
--   smaller (the original 25 MB comment in inbox_uploads.py cited
--   "~5 Mbps × 40s" bandwidth math, but that's descriptive, not a
--   hard cap; the fetch timeout is independently tunable).
--
-- bump to 100MB to match push-mode cap per mc#1588; CANVAS_MIRROR:
-- canvas/src/components/tabs/chat/uploads.ts MAX_UPLOAD_BYTES
--
-- SSOT note: 100 MB is now duplicated FIVE times (canvas TS +
-- workspace-server Go + workspace Python ingest + workspace Python
-- pull + nginx harness + this DB CHECK). The proper fix is a
-- GET /uploads/limits endpoint so each surface reads a single source
-- — tracked separately per CTO follow-up (RFC referenced in
-- mc#1588 description).
--
-- Constraint name: the original CREATE TABLE wrote the CHECK inline
-- without an explicit name. Postgres auto-names it
-- `pending_uploads_size_bytes_check`. We DROP IF EXISTS that name
-- then re-add the constraint at the new ceiling. DROP IF EXISTS is
-- idempotent — re-running this migration after a partial failure
-- doesn't error.
--
-- Forward-only safety: ALTER TABLE … ADD CONSTRAINT … CHECK takes
-- an ACCESS EXCLUSIVE lock briefly while it validates every existing
-- row. At current row counts (<1k rows steady-state, 24h TTL) this
-- is sub-second. If pending_uploads ever grows into the millions,
-- switch to ADD CONSTRAINT … NOT VALID + VALIDATE CONSTRAINT in two
-- steps to keep the lock short.

ALTER TABLE pending_uploads
    DROP CONSTRAINT IF EXISTS pending_uploads_size_bytes_check;

ALTER TABLE pending_uploads
    ADD CONSTRAINT pending_uploads_size_bytes_check
    CHECK (size_bytes > 0 AND size_bytes <= 104857600);
