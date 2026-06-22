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
	"sync"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/textutil"
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
			ExpiresInSeconds interface{} `json:"expires_in_seconds"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0
	}
	var seconds int
	switch v := envelope.Params.ExpiresInSeconds.(type) {
	case float64:
		seconds = int(v)
	default:
		return 0
	}
	if seconds < 0 {
		return 0
	}
	return seconds
}

const (
	PriorityCritical = 100
	PriorityTask     = 50
	PriorityInfo     = 10
)

// A2A queue sweeper constants (#2930). The sweeper is an independent periodic
// drain fallback so that queued requests are not stranded when a workspace
// stops heartbeating (e.g., after the restart-trigger in #2929).
const (
	a2aQueueSweeperInterval    = 10 * time.Second
	a2aQueueSweeperBatchCap    = 8
	a2aQueueSweeperStatusAlert = 10 // log a warning every N stranded items
)

// transientRetryBackoffSecs is how long a MarkQueueItemTransientRetry
// row remains ineligible for re-dispatch, expressed in seconds (the
// integer form that PostgreSQL's make_interval(secs => $N) accepts).
//
// #3127 follow-up (Researcher REQUEST_CHANGES) — the transient-retry
// path requeues with status='queued' but the same DrainQueueForWorkspace
// for-loop can iterate up to capacity times. Without this backoff, a
// capacity>1 drain would re-claim the just-requeued row on the next
// iteration and hit the same gateway failure again, in a tight loop.
// 5s is long enough to break that loop (sweeper interval is 10s,
// heartbeats typically every 5-30s) and short enough that recovery on
// the next heartbeat is not perceptibly delayed.
const transientRetryBackoffSecs = 5

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
// Idempotency: when idempotencyKey is non-empty, a duplicate active enqueue
// for the same (workspace, key) is collapsed rather than double-buffered. On
// a duplicate this returns the existing row's ID so the caller's log still
// points at the live queue entry.
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
	// Normalize the callerID the same way nilIfEmpty does in
	// a2a_proxy_helpers.go: system-caller prefixes (webhook:,
	// system:, test:, channel:) are non-UUID routing markers, not real
	// workspace ids. Persisting them to a2a_queue.caller_id (a
	// UUID-typed column per migrations/042_a2a_queue.up.sql:21) would
	// trip a Postgres UUID cast failure → "invalid input syntax for
	// type uuid" → EnqueueA2A returns an error → the busy-A2A path
	// falls through to a 503 instead of queueing. See #2694 RC
	// #99248 for the symptom + #2693 for the broader #2680 lineage.
	//
	// Real workspace UUIDs are passed through unchanged so the
	// queue-row attribution is preserved.
	var callerArg interface{}
	if callerID != "" && !isSystemCaller(callerID) {
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

	// Supersede any already-expired pending row for this same key before we
	// insert. The drain path skips expired pending rows, so such a row never
	// completes on its own — it lingers in the active set and would block the
	// conflict check below, silently swallowing this fresh enqueue. Retiring
	// it here (a) frees the active set so the insert below proceeds and (b)
	// cleans the stale row up so expired rows don't accumulate. Scoped to the
	// idempotency key so unrelated traffic is untouched.
	if idempotencyKey != "" {
		if _, supErr := db.DB.ExecContext(ctx, `
			UPDATE a2a_queue
			SET status = 'dropped',
			    last_error = 'superseded: expired before drain; replaced by a fresh enqueue'
			WHERE workspace_id = $1
			  AND idempotency_key = $2
			  AND status = 'queued'
			  AND expires_at IS NOT NULL
			  AND expires_at <= now()
		`, workspaceID, idempotencyKey); supErr != nil {
			// Non-fatal: if the cleanup fails we still attempt the insert. Worst
			// case the conflict path returns the (stale) existing row's id, which
			// is the pre-fix behaviour — no new breakage introduced here.
			log.Printf("A2AQueue: supersede-expired cleanup failed for workspace %s key %s: %v",
				workspaceID, idempotencyKey, supErr)
		}
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
	if err := db.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM a2a_queue
		WHERE workspace_id = $1 AND status = 'queued'
	`, workspaceID).Scan(&depth); err != nil {
		log.Printf("A2AQueue: depth query failed for workspace %s: %v", workspaceID, err)
	}

	log.Printf("A2AQueue: enqueued %s for workspace %s (priority=%d, depth=%d)", id, workspaceID, priority, depth)
	return id, depth, nil
}

// DequeueNext claims the next queued item for a workspace and marks it
// 'dispatched'. Uses SELECT ... FOR UPDATE SKIP LOCKED so two concurrent
// drain calls don't both claim the same row.
//
// Honors a per-row next_attempt_at backoff (added in #3127 follow-up
// migration 20260621120000). Rows whose next_attempt_at is in the future
// are SKIPPED — they remain 'queued' but are not eligible for dispatch
// until the backoff expires. This is the gate that breaks the
// capacity>1 tight-retry loop on a flapping gateway: when
// MarkQueueItemTransientRetry sets next_attempt_at = now() + 5s, the
// same for-loop iteration that just requeued the row cannot re-dequeue
// it on the very next iteration even if the row is still highest
// priority.
//
// Returns (nil, nil) when the queue is empty (or all eligible rows are
// backoff-gated) — not an error.
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
		  AND (next_attempt_at IS NULL OR next_attempt_at <= now())
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
// drain dispatch. responseBody is persisted so callers polling
// GET /workspaces/:id/a2a/queue/:queue_id can retrieve the actual agent reply
// for non-delegation A2A queue items (e.g. message/send that got queued because
// the target was busy). Pass nil when no payload exists (re-queued drain).
func MarkQueueItemCompleted(ctx context.Context, id string, responseBody []byte) {
	var respBody any
	if len(responseBody) > 0 {
		// Store as a JSONB value. The column accepts text that parses as JSON;
		// passing the raw bytes through the driver is driver-dependent, so we
		// hand it a string explicitly.
		respBody = string(responseBody)
	}
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE a2a_queue SET status = 'completed', completed_at = now(), response_body = $2 WHERE id = $1`,
		id, respBody,
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

// MarkQueueItemTransientRetry returns a dispatched item to 'queued' WITHOUT
// burning the 5-attempt terminal cap. Used by DrainQueueForWorkspace for
// transient gateway-origin failures (Cloudflare 502, push-route blip, "no
// healthy upstream") where the workspace is online and heartbeating — the
// failure is in the path BETWEEN the platform and the agent, not in the
// agent itself. The PM 2026-06-21 RCA caught that the previous behaviour
// (always MarkQueueItemFailed) consumed the cap on healthy workspaces and
// stranded queued requests until TTL.
//
// Mechanism: DequeueNext (line 256-262 of this file) increments `attempts`
// at dispatch time under FOR UPDATE SKIP LOCKED. MarkQueueItemTransientRetry
// undoes that increment so a transient retry does not advance the cap
// counter. The row stays in 'queued' status with dispatched_at = NULL, so
// the next sweep / heartbeat-drain picks it up naturally.
//
// Backoff (Researcher #3127 REQUEST_CHANGES follow-up): sets
// next_attempt_at = now() + make_interval(secs => transientRetryBackoffSecs)
// so the row is backoff-gated against re-dispatch for the window. This
// is the gate that prevents a capacity>1 DrainQueueForWorkspace from
// tight-looping on the same row (the just-requeued row would otherwise
// be eligible for re-claim on the very next for-loop iteration, and
// would hit the same gateway failure again without ever burning an
// attempt or being delayed). DequeueNext's WHERE clause skips rows
// whose next_attempt_at is still in the future. The seconds count is
// passed as a parameter (rather than inlined as `interval '5 seconds'`)
// so the transientRetryBackoff Go constant drives the SQL behavior
// directly — golangci-lint flagged the previous unused-const shape.
//
// Race-safety note: between DequeueNext's COMMIT and this UPDATE, the row
// is in 'dispatched' status, so a concurrent DequeueNext call (sweeper
// tick, second heartbeat in flight) cannot re-claim it. The status='queued'
// transition is the only window during which re-claim is possible, and it
// is bounded by the time this UPDATE takes to commit.
func MarkQueueItemTransientRetry(ctx context.Context, id, errMsg string) {
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE a2a_queue
		SET status = 'queued',
		    attempts = GREATEST(attempts - 1, 0),
		    last_error = $2,
		    dispatched_at = NULL,
		    next_attempt_at = now() + make_interval(secs => $3)
		WHERE id = $1
	`, id, errMsg, transientRetryBackoffSecs); err != nil {
		log.Printf("A2AQueue: failed to mark %s for transient retry: %v", id, err)
	}
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

// DrainQueueForWorkspace pulls queued items (up to `capacity`) and dispatches
// each via the same ProxyA2ARequest path a live caller would use. Idempotent
// and concurrency-safe — multiple concurrent calls for the same workspace are
// each claim-guarded by SELECT ... FOR UPDATE SKIP LOCKED in DequeueNext.
//
// Called from the Heartbeat handler's goroutine when the workspace reports
// spare capacity, and from the periodic A2A queue sweeper as a fallback when
// heartbeats stop (#2930). Errors here are logged but not returned — callers
// are fire-and-forget goroutines.
//
// #2026-06-21 PM RCA: distinguish GATEWAY-ORIGIN failures (transient
// Cloudflare 502 / push-route blip / "no healthy upstream") from TRUE
// dead-agent failures. Healthy workspaces that happened to get a 502
// from the CDN were terminal-failing the queue item under the previous
// behaviour — MarkQueueItemFailed increments attempts each tick, so a
// transient blip that lasted 5 ticks would burn the cap and strand the
// request at 'failed'. Now: gateway-origin failures with a recent
// heartbeat invalidate the cached URL, re-queue via
// MarkQueueItemTransientRetry (which DOES NOT advance the 5-attempt
// counter), and let the next sweep retry. Only confirmed-dead agents
// (Classification="upstream_dead") or non-gateway failures continue
// through MarkQueueItemFailed.
func (h *WorkspaceHandler) DrainQueueForWorkspace(ctx context.Context, workspaceID string, capacity int) {
	if capacity <= 0 {
		return
	}
	for i := 0; i < capacity; i++ {
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
		// Resolve the agent URL up front so every drain log line carries it.
		// resolveAgentURL swallows its own errors into a proxyA2AError, so a
		// resolution failure here is rare — usually a workspace with no URL
		// row. Empty string is fine for the log; the dispatch below will
		// produce the structured error and we already log it.
		resolvedURL, _ := h.resolveAgentURL(ctx, workspaceID)
		log.Printf("A2AQueue drain: dispatching queue_id=%s workspace_id=%s url=%s attempt=%d",
			item.ID, workspaceID, resolvedURL, item.Attempts)

		// logActivity=false: the original EnqueueA2A callsite already logged
		// the dispatch attempt; re-logging here would double-count events.
		status, respBody, proxyErr := h.proxyA2ARequest(ctx, workspaceID, item.Body, callerID, false, false)

		// 202 Accepted = the dispatch was itself queued again (target still busy).
		// That's not a failure — the queued item just stays queued naturally on
		// the next drain tick. Mark this attempt completed so we don't double-
		// count attempts; the new (re-)queue row already exists.
		if status == http.StatusAccepted {
			MarkQueueItemCompleted(ctx, item.ID, nil)
			log.Printf("A2AQueue drain: queue_id=%s workspace_id=%s re-queued (target still busy)",
				item.ID, workspaceID)
			continue
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
			classification := proxyErr.Classification

			// #2026-06-21 PM RCA: transient gateway-origin failure (CF 5xx,
			// push-route blip, "no healthy upstream") on a workspace that is
			// still heartbeating → re-queue without burning the 5-attempt cap.
			// The agent is alive; the path between us and the agent is not.
			// Invalidate the cached URL so the next retry re-resolves, and
			// hand off to MarkQueueItemTransientRetry which undoes the
			// DequeueNext attempts-increment.
			if isGatewayOriginFailure(proxyErr) && h.hasRecentHeartbeat(ctx, workspaceID) {
				h.invalidateCachedURLForDrain(ctx, workspaceID)
				MarkQueueItemTransientRetry(ctx, item.ID,
					fmt.Sprintf("transient gateway origin (%s, status=%d): %s",
						classificationOrUnknown(classification), proxyErr.Status, errMsg))
				log.Printf("A2AQueue drain: queue_id=%s workspace_id=%s url=%s transient gateway failure "+
					"(status=%d classification=%s) — re-queued without burning attempt cap (attempts preserved at %d)",
					item.ID, workspaceID, resolvedURL, proxyErr.Status, classificationOrUnknown(classification), item.Attempts)
				continue
			}

			MarkQueueItemFailed(ctx, item.ID, errMsg)
			log.Printf("A2AQueue drain: queue_id=%s workspace_id=%s url=%s dispatch failed "+
				"(attempt=%d status=%d classification=%s): %s",
				item.ID, workspaceID, resolvedURL, item.Attempts, proxyErr.Status,
				classificationOrUnknown(classification), errMsg)
			continue
		}
		MarkQueueItemCompleted(ctx, item.ID, respBody)
		log.Printf("A2AQueue drain: queue_id=%s workspace_id=%s url=%s dispatched (attempt=%d)",
			item.ID, workspaceID, resolvedURL, item.Attempts)

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
}

// classificationOrUnknown renders an empty proxyA2AError.Classification as
// the literal "unknown" so the structured drain log line never has an empty
// classification field — makes log-scrapers and human readers happier than
// trailing whitespace.
func classificationOrUnknown(c string) string {
	if c == "" {
		return "unknown"
	}
	return c
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
	respJSON, marshalErr := json.Marshal(map[string]interface{}{
		"text":          responseText,
		"delegation_id": delegationID,
	})
	if marshalErr != nil {
		log.Printf("a2aQueue stitch %s: json.Marshal respJSON failed: %v", delegationID, marshalErr)
		return
	}
	res, err := db.DB.ExecContext(ctx, `
		UPDATE activity_logs
		   SET status        = 'completed',
		       summary       = $1,
		       response_body = $2::jsonb
		 WHERE workspace_id   = $3
		   AND method         = 'delegate_result'
		   AND target_id      = $4
		   AND response_body->>'delegation_id' = $5
	`, "Delegation completed ("+textutil.TruncateBytes(responseText, 80)+")", string(respJSON),
		sourceID, targetID, delegationID)
	if err != nil {
		log.Printf("A2AQueue drain stitch: update failed for delegation %s: %v", delegationID, err)
		return
	}
	rows, err := res.RowsAffected()
	if err != nil {
		log.Printf("A2AQueue drain stitch: RowsAffected error for delegation %s: %v", delegationID, err)
		return
	}
	if rows == 0 {
		log.Printf("A2AQueue drain stitch: no delegate_result row for delegation %s (queued-row may not exist yet)", delegationID)
		return
	}
	log.Printf("A2AQueue drain stitch: delegation %s queued → completed (%d chars)", delegationID, len(responseText))

	// Broadcast DELEGATION_COMPLETE so the canvas chat feed flips the
	// "⏸ queued" line to "✓ completed" in real time. Without this the
	// transition only surfaces after the user reloads or polls activity.
	if h.broadcaster != nil {
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationComplete), sourceID, map[string]interface{}{
			"delegation_id":    delegationID,
			"target_id":        targetID,
			"response_preview": textutil.TruncateBytes(responseText, 200),
			"via":              "queue_drain",
		})
	}
}

// StartA2AQueueSweeper starts the independent periodic drain fallback required
// by #2930. It is intentionally decoupled from the target workspace's heartbeat:
// if a workspace stops heartbeating (offline, flapping, restart wedge), queued
// requests would otherwise sit until TTL and then be silently dropped.
//
// The sweeper runs on a fixed interval, scans for workspaces with pending
// non-expired queue rows and status 'online' or 'degraded', and drains up to
// max_concurrent_tasks items per workspace per tick. Drain dispatches are run
// via globalGoAsync so they are detached from the caller and use the same
// ProxyA2ARequest path as heartbeat-driven drains.
func (h *WorkspaceHandler) StartA2AQueueSweeper(ctx context.Context) {
	if !h.HasProvisioner() {
		// No provisioner means there is no local runtime to drain to (external/
		// mock-only deployment). Skip the sweeper rather than spin no-ops.
		return
	}
	log.Println("A2AQueue sweeper: starting independent periodic drain fallback")
	go func() {
		ticker := time.NewTicker(a2aQueueSweeperInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Println("A2AQueue sweeper: shutting down")
				return
			case <-ticker.C:
				h.sweepA2AQueue(ctx)
			}
		}
	}()
}

// sweepA2AQueue finds online/degraded workspaces with pending queued items and
// drains each in a detached goroutine.
func (h *WorkspaceHandler) sweepA2AQueue(ctx context.Context) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT q.workspace_id, COALESCE(w.max_concurrent_tasks, 1) AS capacity
		FROM a2a_queue q
		JOIN workspaces w ON w.id = q.workspace_id
		WHERE q.status = 'queued'
		  AND (q.expires_at IS NULL OR q.expires_at > now())
		  AND w.status IN ('online', 'degraded')
		GROUP BY q.workspace_id, w.max_concurrent_tasks
	`)
	if err != nil {
		log.Printf("A2AQueue sweeper: scan failed: %v", err)
		return
	}
	defer rows.Close()

	var wg sync.WaitGroup
	for rows.Next() {
		var workspaceID string
		var capacity int
		if err := rows.Scan(&workspaceID, &capacity); err != nil {
			log.Printf("A2AQueue sweeper: scan row failed: %v", err)
			continue
		}
		if capacity > a2aQueueSweeperBatchCap {
			capacity = a2aQueueSweeperBatchCap
		}
		if capacity <= 0 {
			continue
		}
		// Bound per-tick work: the heartbeat path already drains up to
		// (max_concurrent - active_tasks) on every beat. The sweeper is a
		// safety net, not a throughput pump.
		wg.Add(1)
		globalGoAsync(func(ws string, cap int) func() {
			return func() {
				defer wg.Done()
				h.DrainQueueForWorkspace(ctx, ws, cap)
			}
		}(workspaceID, capacity))
	}
	if err := rows.Err(); err != nil {
		log.Printf("A2AQueue sweeper: row iteration failed: %v", err)
	}
	// Wait for this tick's drains before returning so shutdown is clean.
	wg.Wait()
}

// CountStrandedQueueItems returns the number of queued items for workspaces
// that are not online/degraded (e.g., offline or provisioning). Used for
// alerting/metrics rather than silently dropping at TTL.
func CountStrandedQueueItems(ctx context.Context) (int, error) {
	var count int
	err := db.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM a2a_queue q
		JOIN workspaces w ON w.id = q.workspace_id
		WHERE q.status = 'queued'
		  AND (q.expires_at IS NULL OR q.expires_at > now())
		  AND w.status NOT IN ('online', 'degraded')
	`).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}
