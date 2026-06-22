-- 20260505100000_pending_uploads.up.sql
--
-- RFC: poll-mode chat upload (counterpart to delivery_mode='poll' messaging).
--
-- Today, chat_files.go's Upload handler refuses delivery_mode != 'push'
-- with HTTP 422 "workspace has no callback URL" — external runtime
-- workspaces (laptop / behind NAT) cannot receive file attachments at all.
-- The only escape was "register with ngrok / Cloudflare tunnel + push
-- mode," which forces every external operator into infra plumbing they
-- shouldn't need.
--
-- This table is the platform-side staging layer that lets canvas → external
-- workspace file uploads ride the same poll loop the inbox already uses for
-- text messages:
--
--   1. Canvas POSTs multipart to workspace-server.
--   2. workspace-server parses multipart, stores each file as one
--      pending_uploads row, AND inserts a matching activity_logs row
--      (type='chat_upload_receive', request_body={file_id, filename, ...}).
--   3. Workspace's existing inbox poller picks up the activity row.
--   4. Workspace fetches bytes via GET /workspaces/:id/pending-uploads/:fid/content,
--      writes to /workspace/.molecule/chat-uploads/, ACKs via POST.
--   5. Sweep cron deletes rows past expires_at OR acked_at + N hours.
--
-- Why a separate table and not bytea-on-activity_logs:
--
--   * activity_logs is text/JSON-shaped today; mixing 25 MB binary blobs
--     into request_body inflates every JOIN, every since_id scan, every
--     pgdump.  The bytes need their own home.
--   * Lifecycle differs: activity_logs is durable audit history (90d+);
--     pending_uploads is transient buffer (24h default) that GCs hard.
--     Keeping them split lets each table's retention policy run
--     independently.
--   * A future PR (RFC #2789) will migrate the bytes column to S3 keys
--     without touching the activity_logs schema or the metadata columns
--     here. That migration is one ALTER + one backfill rather than a
--     cross-table rewrite.
--
-- No FK to workspaces:
--   workspace delete should NOT cascade-purge pending_uploads — those
--   rows are evidence-of-receipt and should expire on their own TTL.
--   Same posture as tenant_resources (PR #2343) and delegations (PR #2829).

CREATE TABLE IF NOT EXISTS pending_uploads (
    -- Server-generated so the canvas can include the URI in the chat
    -- message it sends right after the upload POST. Workspace fetches
    -- by this id, no name collisions across workspaces.
    file_id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Target workspace. NOT a FK (see header).
    workspace_id uuid NOT NULL,

    -- Content lives inline today via bytea. The Go-side storage interface
    -- (PendingUploadStorage) abstracts read/write so a future PR can
    -- relocate this column's job to S3 (RFC #2789) by adding an `s3_key
    -- text NULL` column, dual-writing for one release, then dropping
    -- `content` once the backfill drains. The CHECK below pins the same
    -- 25 MB per-file cap the workspace-side ingest_handler enforces
    -- (workspace/internal_chat_uploads.py:198) — discrepancy between
    -- the two would let the platform accept files the workspace would
    -- 413 on after pull.
    content      bytea NOT NULL,
    size_bytes   bigint NOT NULL CHECK (size_bytes > 0 AND size_bytes <= 26214400),

    -- Filename + mimetype mirror the workspace-side ChatUploadedFile
    -- shape so the eventual InboxMessage hand-off needs no translation.
    -- Filename is sanitized at write-time (matches sanitize_filename in
    -- workspace/internal_chat_uploads.py); 100 char cap is the same.
    filename     text NOT NULL CHECK (length(filename) > 0 AND length(filename) <= 100),
    mimetype     text NOT NULL DEFAULT '',

    created_at   timestamptz NOT NULL DEFAULT now(),

    -- Stamped on the GET /content request. Lets Phase 3 sweeper detect
    -- "fetched but never acked" — distinct failure mode from "never
    -- fetched" (workspace offline) so dashboards can split them.
    fetched_at   timestamptz,

    -- Stamped on the POST /ack request. Terminal state for the happy
    -- path. Sweep cron deletes acked rows past acked_at + retention.
    acked_at     timestamptz,

    -- Hard TTL: rows past this are deleted regardless of ack state.
    -- 24h matches the longest-observed legitimate "operator stepped
    -- away from laptop" gap; tunable later via app-level config without
    -- a migration. NOT acked_at + 24h — that would let a stuck-fetched
    -- row live forever.
    expires_at   timestamptz NOT NULL DEFAULT (now() + interval '24 hours')
);

-- Hot path: workspace's poll cycle pulls "give me my unacked uploads
-- in chronological order." Partial-index because acked rows are GC
-- candidates and shouldn't bloat the working set.
CREATE INDEX IF NOT EXISTS idx_pending_uploads_workspace_unacked
    ON pending_uploads (workspace_id, created_at)
    WHERE acked_at IS NULL;

-- Phase 3 GC sweep hot path: list rows past expires_at, partial-indexed
-- on unacked because acked rows have a different (shorter) retention
-- and GC-via-acked_at is a separate query.
CREATE INDEX IF NOT EXISTS idx_pending_uploads_expires
    ON pending_uploads (expires_at)
    WHERE acked_at IS NULL;
