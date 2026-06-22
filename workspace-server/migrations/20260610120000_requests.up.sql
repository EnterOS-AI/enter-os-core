-- requests: the unified Tasks + Approvals primitive (RFC P1). Generalizes
-- user_tasks (007-style worklist "asks") and approval_requests (the destructive
-- gate) into ONE inbox keyed by kind ∈ {task, approval}, with a requester and a
-- recipient that may each be a user OR an agent. See
-- docs/design/rfc-unified-requests-inbox.md.
--
-- recipient_id / requester_id are plain TEXT with NO foreign key on purpose: an
-- agent's id is a workspaces(id) UUID, but a user's id is not a workspaces row,
-- so a cross-type FK is impossible. ListPendingForOrg LEFT JOINs workspaces to
-- decorate a name when the party is an agent (a user party simply has no match).
--
-- The migration runner tracks applied filenames in schema_migrations, but a
-- partial-failure re-run would re-apply this file, so EVERYTHING here is
-- idempotent: CREATE TABLE/INDEX IF NOT EXISTS (no bare ALTER ADD CONSTRAINT,
-- which can't be IF NOT EXISTS and would crash-loop), and the historical
-- backfills use ON CONFLICT (id) DO NOTHING so re-runs are no-ops.
CREATE TABLE IF NOT EXISTS requests (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind           TEXT NOT NULL CHECK (kind IN ('task', 'approval')),
    requester_type TEXT NOT NULL CHECK (requester_type IN ('user', 'agent')),
    requester_id   TEXT NOT NULL,
    org_id         UUID,
    recipient_type TEXT NOT NULL CHECK (recipient_type IN ('user', 'agent')),
    recipient_id   TEXT NOT NULL,
    title          TEXT NOT NULL,
    detail         TEXT,
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'info_requested', 'done', 'rejected', 'approved', 'cancelled')),
    responder_type TEXT,
    responder_id   TEXT,
    priority       SMALLINT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    responded_at   TIMESTAMPTZ
);

-- Inbox read: WHERE recipient_type=$1 AND recipient_id=$2 [AND status=$3]
-- ORDER BY created_at DESC. This is the recipient's "incoming" list (agent poll
-- or user Tasks/Approvals tab).
CREATE INDEX IF NOT EXISTS idx_requests_inbox
    ON requests (recipient_type, recipient_id, status, created_at DESC);

-- Org pending view ("all agents' incoming" tab): WHERE org_id=$1 AND status...
CREATE INDEX IF NOT EXISTS idx_requests_org_pending
    ON requests (org_id, status, created_at DESC);

-- Outgoing / async pickup: WHERE requester_type=$1 AND requester_id=$2 — the
-- requester reads back the requests it raised (check_requests).
CREATE INDEX IF NOT EXISTS idx_requests_outgoing
    ON requests (requester_type, requester_id, created_at DESC);

-- request_messages: the "More Info / chat about this" thread on a request.
CREATE TABLE IF NOT EXISTS request_messages (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id  UUID NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
    author_type TEXT NOT NULL CHECK (author_type IN ('user', 'agent')),
    author_id   TEXT NOT NULL,
    body        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Thread read: WHERE request_id=$1 ORDER BY created_at.
CREATE INDEX IF NOT EXISTS idx_request_messages_thread
    ON request_messages (request_id, created_at);

-- Idempotent backfill — copy historical user_tasks into the unified inbox so
-- the Tasks tab shows pre-cutover items. user_tasks are always agent→user asks,
-- so requester_type='agent' (requester_id = the raising workspace), recipient is
-- the human user; recipient_id '' is the "the org's user" sentinel (user ids are
-- not modelled yet in this table's source — P1 keeps it empty, the CHECK permits
-- empty TEXT). 'dismissed' maps to the unified 'rejected'.
INSERT INTO requests (
    id, kind, requester_type, requester_id, recipient_type, recipient_id,
    title, detail, status, responder_id, created_at, responded_at
)
SELECT
    id,
    'task',
    'agent',
    workspace_id::text,
    'user',
    '',
    title,
    detail,
    CASE status WHEN 'dismissed' THEN 'rejected' ELSE status END,
    resolved_by,
    created_at,
    resolved_at
FROM user_tasks
ON CONFLICT (id) DO NOTHING;

-- Idempotent backfill — copy historical approval_requests in as kind='approval'.
-- approval_requests columns (007_approvals.sql): id, workspace_id, task_id,
-- action, reason, context, status IN (pending|approved|denied|escalated),
-- decided_by, decided_at, created_at. Map status: 'denied'→'rejected',
-- 'escalated'→'pending' (still awaiting a decision, just bubbled up). title is
-- the action; detail is the reason. requester = the raising workspace (agent),
-- recipient = the user who decides.
INSERT INTO requests (
    id, kind, requester_type, requester_id, recipient_type, recipient_id,
    title, detail, status, responder_id, created_at, responded_at
)
SELECT
    id,
    'approval',
    'agent',
    workspace_id::text,
    'user',
    '',
    action,
    reason,
    CASE status
        WHEN 'denied' THEN 'rejected'
        WHEN 'escalated' THEN 'pending'
        ELSE status
    END,
    decided_by,
    created_at,
    decided_at
FROM approval_requests
WHERE workspace_id IS NOT NULL
ON CONFLICT (id) DO NOTHING;
