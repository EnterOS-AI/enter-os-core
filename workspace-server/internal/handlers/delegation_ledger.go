package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
)

// delegation_ledger.go — durable per-task ledger for A2A delegation
// (RFC #2829 PR-1).
//
// activity_logs is an event stream — one row per state transition. Replaying
// the stream gives you history. This file's table (delegations) is the
// folded current state — one row per delegation_id with a single status,
// last_heartbeat, deadline, and result_preview.
//
// Why both: PR-3 needs a sweeper that joins on
//   (status='in_progress' AND last_heartbeat < now() - interval '10 minutes')
// which is impossible to express against the event stream without a window
// function over every (delegation_id, latest event) pair — a planner-killing
// query at scale. The dedicated table makes the sweeper an indexed scan.
//
// Writes go to BOTH tables. activity_logs remains the audit-grade record
// for forensics; delegations is the queryable view for dashboards + sweeper
// joins. Symmetric-write pattern — same posture as tenant_resources (PR
// #2343), per memory `reference_tenant_resources_audit`.

// DelegationLedger writes the per-task durable row alongside the existing
// activity_logs event-stream writes. All methods are best-effort: a ledger
// write failure logs but does NOT propagate up — activity_logs remains the
// audit-grade source of truth.
//
// Same shape as `tenant_resources` reconciler (PR #2343): orchestration
// continues even when the ledger write fails, and the next status update
// (or PR-3 reconciler) will heal the ledger.
type DelegationLedger struct {
	db *sql.DB
}

// NewDelegationLedger returns a ledger backed by the package db handle.
// Tests can construct one with a sqlmock-backed *sql.DB.
func NewDelegationLedger(handle *sql.DB) *DelegationLedger {
	if handle == nil {
		handle = db.DB
	}
	return &DelegationLedger{db: handle}
}

// truncatePreview caps stored preview at 4KB. The full prompt/response is
// already in activity_logs.{request,response}_body — this is the at-a-glance
// view for the dashboard, not a forensic record.
//
// Rune-safe: previous byte-slice form (s[:previewCap]) split on a byte
// boundary, which on a multi-byte codepoint at byte 4096 produced
// invalid UTF-8 — Postgres JSONB rejects → ledger row not inserted →
// audit gap. Issue #2962. Walks the string by rune, stops at the last
// rune-boundary index that fits inside the cap. ASCII-only strings hit
// the cap exactly; CJK/emoji strings stop slightly under the cap,
// never over.
//
// Mirrors the truncatePreviewRunes fix from agent_message_writer.go
// (#2959). Both call sites should consume a shared helper after both
// fixes have landed — followup deduplication tracked in #2962's body.
const previewCap = 4096

func truncatePreview(s string) string {
	if len(s) <= previewCap {
		return s
	}
	// Range over a string yields rune-boundary byte indices. Walk
	// until the next index would exceed previewCap; the previous
	// index is the safe truncation point.
	end := 0
	for i := range s {
		if i > previewCap {
			break
		}
		end = i
	}
	return s[:end]
}

// InsertOpts is the agent's record-of-intent. Caller, callee, task preview,
// and the chosen delegation_id are required; idempotency_key is optional.
type InsertOpts struct {
	DelegationID   string
	CallerID       string
	CalleeID       string
	TaskPreview    string
	IdempotencyKey string // empty → NULL
	// Deadline defaults to now + 6h when zero. Callers can pass a tighter
	// per-task deadline (cron, interactive request) by setting it.
	Deadline time.Time
}

// Insert writes the queued row. ON CONFLICT (delegation_id) DO NOTHING so
// the agent's retry-on-restart codepath is naturally idempotent — a duplicate
// Insert with the same delegation_id is a no-op. (Idempotency_key dedupe is
// a separate UNIQUE index handled by the same DO NOTHING.)
func (l *DelegationLedger) Insert(ctx context.Context, opts InsertOpts) {
	if opts.DelegationID == "" || opts.CallerID == "" || opts.CalleeID == "" {
		log.Printf("delegation_ledger Insert: missing required field, skipping")
		return
	}
	deadline := opts.Deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(6 * time.Hour)
	}
	idemArg := sql.NullString{String: opts.IdempotencyKey, Valid: opts.IdempotencyKey != ""}
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO delegations (
			delegation_id, caller_id, callee_id, task_preview,
			status, deadline, idempotency_key
		) VALUES ($1, $2, $3, $4, 'queued', $5, $6)
		ON CONFLICT (delegation_id) DO NOTHING
	`, opts.DelegationID, opts.CallerID, opts.CalleeID,
		truncatePreview(opts.TaskPreview), deadline, idemArg)
	if err != nil {
		log.Printf("delegation_ledger Insert(%s): %v", opts.DelegationID, err)
	}
}

// allowedTransitions enforces the lifecycle in code as defense-in-depth on
// the schema CHECK. Terminal states (completed, failed, stuck) reject any
// further status update — once a delegation is done, it stays done.
//
// The "queued → in_progress" jump (skipping dispatched) is allowed: lazy
// callers that don't ack the dispatched stage shouldn't be penalised,
// since the agent ultimately cares about whether work started, not which
// HTTP layer happened to ack first.
var allowedTransitions = map[string]map[string]bool{
	"queued":      {"dispatched": true, "in_progress": true, "failed": true},
	"dispatched":  {"in_progress": true, "completed": true, "failed": true},
	"in_progress": {"completed": true, "failed": true, "stuck": true},
}

// ErrInvalidTransition is returned by SetStatus when the transition would
// move out of a terminal state. Callers SHOULD ignore (it's a duplicate
// terminal write) but they're surfaced for tests.
var ErrInvalidTransition = errors.New("delegation ledger: invalid status transition")

// SetStatus is the catch-all updater. Status MUST be one of the lifecycle
// values. errorDetail is non-empty only for failed/stuck. resultPreview is
// non-empty only for completed.
//
// Idempotent: re-applying the same terminal status with the same payload
// returns nil; transitioning back out of a terminal state returns
// ErrInvalidTransition. (Forward-only protection — once 'completed' you
// don't get to revise to 'failed'.)
func (l *DelegationLedger) SetStatus(ctx context.Context,
	delegationID, status, errorDetail, resultPreview string,
) error {
	if delegationID == "" || status == "" {
		return errors.New("delegation ledger: missing required field")
	}

	// Read current status to validate the transition. We accept the rare
	// race where two updaters both observe the same prior status — Postgres
	// CHECK constraint catches truly-invalid status values; our forward-only
	// check is best-effort.
	var current string
	err := l.db.QueryRowContext(ctx,
		`SELECT status FROM delegations WHERE delegation_id = $1`,
		delegationID,
	).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		// Insert was lost or wasn't called. Defensively NO-OP — the next
		// agent retry will re-Insert and the next SetStatus will land.
		log.Printf("delegation_ledger SetStatus(%s, %s): row missing, skipping",
			delegationID, status)
		return nil
	}
	if err != nil {
		return err
	}

	// Same-status replay (e.g. duplicate completion notification): no-op,
	// don't bump updated_at, no error.
	if current == status {
		return nil
	}

	// Forward-only on terminal states.
	if next, ok := allowedTransitions[current]; !ok || !next[status] {
		// Terminal already — refuse to revise.
		return ErrInvalidTransition
	}

	_, err = l.db.ExecContext(ctx, `
		UPDATE delegations
		SET status = $2,
		    error_detail = NULLIF($3, ''),
		    result_preview = NULLIF($4, ''),
		    updated_at = now()
		WHERE delegation_id = $1
	`, delegationID, status, errorDetail, truncatePreview(resultPreview))
	return err
}

// Heartbeat stamps last_heartbeat = now() for an in-flight delegation. Used
// by the callee whenever it makes progress; PR-3's sweeper compares to
// NOW() to decide stuckness. No-op on terminal-state delegations.
//
// Best-effort: failure logs but doesn't propagate.
func (l *DelegationLedger) Heartbeat(ctx context.Context, delegationID string) {
	if delegationID == "" {
		return
	}
	_, err := l.db.ExecContext(ctx, `
		UPDATE delegations
		SET last_heartbeat = now(), updated_at = now()
		WHERE delegation_id = $1
		  AND status NOT IN ('completed','failed','stuck')
	`, delegationID)
	if err != nil {
		log.Printf("delegation_ledger Heartbeat(%s): %v", delegationID, err)
	}
}
