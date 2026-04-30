package handlers

// a2a_queue.go — #1870 Phase 1: enqueue A2A requests whose target is busy,
// drain the queue on heartbeat when the target regains capacity.
//
// Three levels are declared here so Phase 2/3 can land without a migration:
//   - PriorityCritical = 100 — preempts running task (Phase 3, not active yet)
//   - PriorityTask     = 50  — default, FIFO within priority (Phase 1, active)
//   - PriorityInfo     = 10  — best-effort with TTL (Phase 2, not active yet)
//
// Phase 1 writes only PriorityTask. The `priority` column tolerates all three.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
)

// extractIdempotencyKey pulls params.message.messageId out of an A2A JSON-RPC
// body (normalizeA2APayload guarantees this field is set before dispatch).
// Empty string on parse failure — callers treat that as "no idempotency".
func extractIdempotencyKey(body []byte) string {
	var envelope struct {
		Params struct {
			Message struct {
				MessageID string `json:"messageId"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return envelope.Params.Message.MessageID
}

// extractExpiresInSeconds pulls params.expires_in_seconds out of an A2A
// JSON-RPC body and returns it as a positive integer. A zero return means
// "no caller-specified TTL" — caller should leave expires_at NULL on the
// queue row, preserving today's infinite-TTL behaviour (the
// DropStaleQueueItems admin sweeper still drops entries past the
// platform-default age). Negative values and parse errors collapse to 0.
//
// Why params-level (not metadata): expires_in_seconds is a delivery
// directive, not a peer-to-peer message attribute. Putting it under
// `params` keeps it adjacent to other delivery hints (priority,
// idempotency) and out of `params.message.metadata` which the receiving
// agent can read.
func extractExpiresInSeconds(body []byte) int {
	var envelope struct {
		Params struct {
			ExpiresInSeconds int `json:"expires_in_seconds"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0
	}
	if envelope.Params.ExpiresInSeconds < 0 {
		return 0
	}
	return envelope.Params.ExpiresInSeconds
}

const (
	PriorityCritical = 100
	PriorityTask     = 50
	PriorityInfo     = 10
)

// QueuedItem is what the heartbeat drain path pulls off the queue.
type QueuedItem struct {
	ID          string
	WorkspaceID string
	CallerID    sql.NullString
	Priority    int
	Body        []byte
	Method      sql.NullString
	Attempts    int
}

// EnqueueA2A inserts a busy-retry-eligible A2A request into a2a_queue and
// returns the new row ID + current queue depth. Caller MUST have already
// determined the target is busy — this function does not check.
//
// Idempotency: when idempotencyKey is non-empty, the partial unique index
// `idx_a2a_queue_idempotency` prevents duplicate active rows for the same
// (workspace_id, idempotency_key). On conflict this returns the existing
// row's ID so the caller's log still points at the live queue entry.
func EnqueueA2A(
	ctx context.Context,
	workspaceID, callerID string,
	priority int,
	body []byte,
	method, idempotencyKey string,
	expiresAt *time.Time,
) (id string, depth int, err error) {
	var keyArg interface{}
	if idempotencyKey != "" {
		keyArg = idempotencyKey
	}
	var callerArg interface{}
	if callerID != "" {
		callerArg = callerID
	}
	var methodArg interface{}
	if method != "" {
		methodArg = method
	}
	// expiresAtArg stays NULL when caller didn't specify a TTL. DequeueNext's
	// `expires_at IS NULL OR expires_at > now()` filter then preserves today's
	// infinite-TTL semantics for un-flagged messages.
	var expiresAtArg interface{}
	if expiresAt != nil {
		expiresAtArg = *expiresAt
	}

	// INSERT ... ON CONFLICT DO NOTHING RETURNING id. The conflict target
	// must reference the partial unique INDEX columns + WHERE clause directly
	// (Postgres can't reference partial unique indexes by name in
	// ON CONFLICT — only true CONSTRAINTs work for that). On conflict we
	// then look up the existing row's id so the caller always receives a
	// valid queue entry reference.
	err = db.DB.QueryRowContext(ctx, `
		INSERT INTO a2a_queue (workspace_id, caller_id, priority, body, method, idempotency_key, expires_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
		ON CONFLICT (workspace_id, idempotency_key)
			WHERE idempotency_key IS NOT NULL AND status IN ('queued','dispatched')
			DO NOTHING
		RETURNING id
	`, workspaceID, callerArg, priority, string(body), methodArg, keyArg, expiresAtArg).Scan(&id)

	if errors.Is(err, sql.ErrNoRows) && idempotencyKey != "" {
		// Conflict — look up the existing active row and use its id.
		err = db.DB.QueryRowContext(ctx, `
			SELECT id FROM a2a_queue
			WHERE workspace_id = $1 AND idempotency_key = $2
			  AND status IN ('queued','dispatched')
			LIMIT 1
		`, workspaceID, idempotencyKey).Scan(&id)
		if err != nil {
			return "", 0, err
		}
	} else if err != nil {
		return "", 0, err
	}

	// Return current queue depth for the caller's visibility.
	_ = db.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM a2a_queue
		WHERE workspace_id = $1 AND status = 'queued'
	`, workspaceID).Scan(&depth)

	log.Printf("A2AQueue: enqueued %s for workspace %s (priority=%d, depth=%d)", id, workspaceID, priority, depth)
	return id, depth, nil
}

// DequeueNext claims the next queued item for a workspace and marks it
// 'dispatched'. Uses SELECT ... FOR UPDATE SKIP LOCKED so two concurrent
// drain calls don't both claim the same row.
//
// Returns (nil, nil) when the queue is empty — not an error.
func DequeueNext(ctx context.Context, workspaceID string) (*QueuedItem, error) {
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var item QueuedItem
	var body string
	err = tx.QueryRowContext(ctx, `
		SELECT id, workspace_id, caller_id, priority, body::text, method, attempts
		FROM a2a_queue
		WHERE workspace_id = $1 AND status = 'queued'
		  AND (expires_at IS NULL OR expires_at > now())
		ORDER BY priority DESC, enqueued_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`, workspaceID).Scan(
		&item.ID, &item.WorkspaceID, &item.CallerID, &item.Priority,
		&body, &item.Method, &item.Attempts,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item.Body = []byte(body)

	if _, err := tx.ExecContext(ctx, `
		UPDATE a2a_queue
		SET status = 'dispatched', dispatched_at = now(), attempts = attempts + 1
		WHERE id = $1
	`, item.ID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &item, nil
}

// MarkQueueItemCompleted flips the queue row to 'completed' on a successful
// drain dispatch.
func MarkQueueItemCompleted(ctx context.Context, id string) {
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE a2a_queue SET status = 'completed', completed_at = now() WHERE id = $1`, id,
	); err != nil {
		log.Printf("A2AQueue: failed to mark %s completed: %v", id, err)
	}
}

// MarkQueueItemFailed returns a dispatched item back to 'queued' with an
// incremented attempts counter so the next drain tick picks it up. Hits
// an upper bound (5 attempts) to avoid wedging a stuck item in the queue
// forever.
func MarkQueueItemFailed(ctx context.Context, id, errMsg string) {
	const maxAttempts = 5
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE a2a_queue
		SET status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'queued' END,
		    last_error = $3,
		    dispatched_at = NULL
		WHERE id = $1
	`, id, maxAttempts, errMsg); err != nil {
		log.Printf("A2AQueue: failed to mark %s failed: %v", id, err)
	}
}

// QueueDepth returns the number of currently-queued (not dispatched/completed)
// items for a workspace. Used by the busy-return response body so callers
// can see how many ahead of them.
func QueueDepth(ctx context.Context, workspaceID string) int {
	var n int
	_ = db.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued'`,
		workspaceID,
	).Scan(&n)
	return n
}

// DropStaleQueueItems marks queued items older than maxAge as 'dropped' with a
// system-generated reason so PM agents stop processing stale post-incident noise.
// Called with a workspaceID to scope cleanup to one workspace, or empty to sweep
// all workspaces.
//
// Returns the number of items dropped for visibility/audit logging.
func DropStaleQueueItems(ctx context.Context, workspaceID string, maxAgeMinutes int) (int, error) {
	var rows int64
	var err error
	if workspaceID != "" {
		err = db.DB.QueryRowContext(ctx, `
			WITH dropped AS (
				UPDATE a2a_queue
				SET status = 'dropped',
				    last_error = last_error ||
				        E'\n[DropStaleQueueItems] auto-dropped: queue item age exceeded the post-incident TTL. '
				        || 'Dropped at ' || now()::text
				WHERE id IN (
					SELECT id FROM a2a_queue
					WHERE workspace_id = $1
					  AND status = 'queued'
					  AND enqueued_at < now() - interval '1 minute' * $2
					FOR UPDATE SKIP LOCKED
				)
				RETURNING id
			)
			SELECT count(*) FROM dropped
		`, workspaceID, maxAgeMinutes).Scan(&rows)
	} else {
		err = db.DB.QueryRowContext(ctx, `
			WITH dropped AS (
				UPDATE a2a_queue
				SET status = 'dropped',
				    last_error = last_error ||
				        E'\n[DropStaleQueueItems] auto-dropped: queue item age exceeded the post-incident TTL. '
				        || 'Dropped at ' || now()::text
				WHERE id IN (
					SELECT id FROM a2a_queue
					WHERE status = 'queued'
					  AND enqueued_at < now() - interval '1 minute' * $1
					FOR UPDATE SKIP LOCKED
				)
				RETURNING id
			)
			SELECT count(*) FROM dropped
		`, maxAgeMinutes).Scan(&rows)
	}
	if err != nil {
		return 0, fmt.Errorf("DropStaleQueueItems: %w", err)
	}
	return int(rows), nil
}

// DrainQueueForWorkspace pulls one queued item and dispatches it via the
// same ProxyA2ARequest path a live caller would use. Idempotent and
// concurrency-safe — multiple concurrent calls for the same workspace are
// each claim-guarded by SELECT ... FOR UPDATE SKIP LOCKED in DequeueNext.
//
// Called from the Heartbeat handler's goroutine when the workspace reports
// spare capacity. Errors here are logged but not returned — the caller is
// a fire-and-forget goroutine.
func (h *WorkspaceHandler) DrainQueueForWorkspace(ctx context.Context, workspaceID string) {
	item, err := DequeueNext(ctx, workspaceID)
	if err != nil {
		log.Printf("A2AQueue drain: dequeue failed for %s: %v", workspaceID, err)
		return
	}
	if item == nil {
		return // queue empty, no work
	}

	callerID := ""
	if item.CallerID.Valid {
		callerID = item.CallerID.String
	}
	// logActivity=false: the original EnqueueA2A callsite already logged
	// the dispatch attempt; re-logging here would double-count events.
	status, respBody, proxyErr := h.proxyA2ARequest(ctx, workspaceID, item.Body, callerID, false)

	// 202 Accepted = the dispatch was itself queued again (target still busy).
	// That's not a failure — the queued item just stays queued naturally on
	// the next drain tick. Mark this attempt completed so we don't double-
	// count attempts; the new (re-)queue row already exists.
	if status == http.StatusAccepted {
		MarkQueueItemCompleted(ctx, item.ID)
		log.Printf("A2AQueue drain: %s re-queued (target still busy)", item.ID)
		return
	}

	if proxyErr != nil {
		// Defensive: proxyErr.Response is gin.H (map[string]interface{}). The
		// "error" key is conventionally a string but can be missing or non-
		// string in edge paths (e.g. a future error builder using a typed
		// struct). Cast safely so a missing key doesn't crash the platform —
		// today's outage was caused by an unchecked .(string) here.
		errMsg, _ := proxyErr.Response["error"].(string)
		if errMsg == "" {
			errMsg = http.StatusText(proxyErr.Status)
			if errMsg == "" {
				errMsg = "unknown drain dispatch error"
			}
		}
		MarkQueueItemFailed(ctx, item.ID, errMsg)
		log.Printf("A2AQueue drain: dispatch for %s failed (attempt=%d): %s",
			item.ID, item.Attempts, errMsg)
		return
	}
	MarkQueueItemCompleted(ctx, item.ID)
	log.Printf("A2AQueue drain: dispatched %s to workspace %s (attempt=%d)",
		item.ID, workspaceID, item.Attempts)

	// Stitch the response back to the originating delegation row, if this
	// queue item was a delegation. Without this, check_task_status would
	// see status='queued' (set by the executeDelegation queued-branch) and
	// the LLM would think the work was never done. We embed delegation_id
	// in params.message.metadata at Delegate-handler time; pull it out
	// here and UPDATE the delegate_result row so the original caller can
	// observe the real reply.
	if delegationID := extractDelegationIDFromBody(item.Body); delegationID != "" {
		h.stitchDrainResponseToDelegation(ctx, callerID, item.WorkspaceID, delegationID, respBody)
	}
}

// extractDelegationIDFromBody pulls params.message.metadata.delegation_id
// out of an A2A JSON-RPC body. Empty string when absent — drain treats
// that as "this queue item didn't originate from /workspaces/:id/delegate"
// and skips the stitch, so non-delegation queue uses (cross-workspace
// peer-direct A2A) aren't affected.
func extractDelegationIDFromBody(body []byte) string {
	var envelope struct {
		Params struct {
			Message struct {
				Metadata struct {
					DelegationID string `json:"delegation_id"`
				} `json:"metadata"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return envelope.Params.Message.Metadata.DelegationID
}

// stitchDrainResponseToDelegation writes the drained response into the
// delegation's existing delegate_result row (created with status='queued'
// by executeDelegation when the proxy first returned queued). This is the
// other half of the loop that closes "queued → completed" so the LLM's
// check_task_status reflects ground truth.
//
// Errors are logged-only — drain is fire-and-forget from Heartbeat, and a
// stitch failure shouldn't block other queued items. The delegation will
// just remain stuck at 'queued' in this case, which is the pre-fix
// behaviour (no regression vs. shipping nothing).
func (h *WorkspaceHandler) stitchDrainResponseToDelegation(ctx context.Context, sourceID, targetID, delegationID string, respBody []byte) {
	if sourceID == "" || delegationID == "" {
		return
	}
	responseText := extractResponseText(respBody)
	respJSON, _ := json.Marshal(map[string]interface{}{
		"text":          responseText,
		"delegation_id": delegationID,
	})
	res, err := db.DB.ExecContext(ctx, `
		UPDATE activity_logs
		   SET status        = 'completed',
		       summary       = $1,
		       response_body = $2::jsonb
		 WHERE workspace_id   = $3
		   AND method         = 'delegate_result'
		   AND target_id      = $4
		   AND response_body->>'delegation_id' = $5
	`, "Delegation completed ("+truncate(responseText, 80)+")", string(respJSON),
		sourceID, targetID, delegationID)
	if err != nil {
		log.Printf("A2AQueue drain stitch: update failed for delegation %s: %v", delegationID, err)
		return
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		log.Printf("A2AQueue drain stitch: no delegate_result row for delegation %s (queued-row may not exist yet)", delegationID)
		return
	}
	log.Printf("A2AQueue drain stitch: delegation %s queued → completed (%d chars)", delegationID, len(responseText))

	// Broadcast DELEGATION_COMPLETE so the canvas chat feed flips the
	// "⏸ queued" line to "✓ completed" in real time. Without this the
	// transition only surfaces after the user reloads or polls activity.
	if h.broadcaster != nil {
		h.broadcaster.RecordAndBroadcast(ctx, "DELEGATION_COMPLETE", sourceID, map[string]interface{}{
			"delegation_id":    delegationID,
			"target_id":        targetID,
			"response_preview": truncate(responseText, 200),
			"via":              "queue_drain",
		})
	}
}
