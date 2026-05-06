package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/textutil"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// delegationResultInboxPushEnabled gates the RFC #2829 PR-2 result-push
// behavior: when callee POSTs `status=completed` (or `failed`) via
// /workspaces/:id/delegations/:delegation_id/update, ALSO write an
// `activity_type='a2a_receive'` row to the caller's activity_logs.
//
// Why a flag: the caller's inbox poller (workspace/inbox.py) queries
// `?type=a2a_receive` to surface inbound messages to the agent. Adding
// a2a_receive rows for delegation results is the universal-sized fix for
// the 600s message/send timeout class — long-running delegations no
// longer rely on the proxy holding the HTTP connection open. But it is
// observable behavior change (existing agents start seeing delegation
// results in their inbox where they didn't before), so we flag it for
// staging burn-in before flipping default.
//
// Default: off. Staging-canary first; flip to on after RFC #2829 PR-3
// (agent-side cutover) lands and proves the round-trip end-to-end.
func delegationResultInboxPushEnabled() bool {
	return os.Getenv("DELEGATION_RESULT_INBOX_PUSH") == "1"
}

// pushDelegationResultToInbox writes the inbox-visible row for a
// completed/failed delegation. Best-effort: a failure logs but does NOT
// fail the parent UpdateStatus — the existing delegate_result row in
// activity_logs is still authoritative for the dashboard.
//
// Caller (sourceID) is the workspace that initiated the delegation; the
// inbox row lands in their activity_logs so wait_for_message picks it up.
//
// Body shape mirrors a2a_receive rows produced by the proxy on a
// successful synchronous reply: response_body.text carries the agent's
// answer, request_body.delegation_id correlates back to the originating
// row.
func pushDelegationResultToInbox(ctx context.Context, sourceID, delegationID, status, responsePreview, errorDetail string) {
	if !delegationResultInboxPushEnabled() {
		return
	}
	respPayload := map[string]interface{}{
		"text":          responsePreview,
		"delegation_id": delegationID,
	}
	respJSON, _ := json.Marshal(respPayload)
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"delegation_id": delegationID,
	})
	logStatus := "ok"
	if status == "failed" {
		logStatus = "error"
	}
	summary := "Delegation result delivered"
	if status == "failed" {
		summary = "Delegation failed"
	}
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO activity_logs (
			workspace_id, activity_type, method, source_id,
			summary, request_body, response_body, status, error_detail
		) VALUES ($1, 'a2a_receive', 'delegate_result', $2, $3, $4::jsonb, $5::jsonb, $6, NULLIF($7, ''))
	`, sourceID, sourceID, summary, string(reqJSON), string(respJSON), logStatus, errorDetail); err != nil {
		log.Printf("Delegation %s: inbox-push insert failed: %v", delegationID, err)
	}
}

// Delegation status lifecycle:
//   pending → dispatched → received → in_progress → completed | failed
//
// pending:     stored in DB, goroutine not yet started
// dispatched:  A2A request sent to target workspace
// received:    target workspace acknowledged (200 from A2A server)
// in_progress: target agent is actively working (set via heartbeat)
// completed:   response received and stored
// failed:      error during any stage

// DelegationHandler manages async delegation between workspaces.
// Delegations are fire-and-forget: the caller gets a task_id immediately,
// and the A2A request runs in the background.
type DelegationHandler struct {
	workspace   *WorkspaceHandler
	broadcaster *events.Broadcaster
}

func NewDelegationHandler(wh *WorkspaceHandler, b *events.Broadcaster) *DelegationHandler {
	return &DelegationHandler{workspace: wh, broadcaster: b}
}

// delegateRequest is the bound POST /workspaces/:id/delegate body.
type delegateRequest struct {
	TargetID       string `json:"target_id" binding:"required"`
	Task           string `json:"task" binding:"required"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Delegate handles POST /workspaces/:id/delegate
// Sends an A2A message to the target workspace in the background.
// Returns immediately with a delegation_id.
func (h *DelegationHandler) Delegate(c *gin.Context) {
	sourceID := c.Param("id")
	ctx := c.Request.Context()

	var body delegateRequest
	if err := bindDelegateRequest(c, &body); err != nil {
		return // response already written
	}

	// #548 — prevent self-delegation: a workspace delegating to itself
	// acquires _run_lock twice on the same mutex, deadlocking permanently.
	if sourceID == body.TargetID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "self-delegation not permitted"})
		return
	}

	// #124 — idempotency. If the caller supplies an idempotency_key, return
	// the existing delegation when (workspace_id, idempotency_key) already
	// exists and is not in a failed terminal state.
	if hit := lookupIdempotentDelegation(ctx, c, sourceID, body.IdempotencyKey); hit {
		return
	}

	delegationID := uuid.New().String()

	outcome := insertDelegationRow(ctx, c, sourceID, body, delegationID)
	if outcome == insertHandledByIdempotent {
		return // idempotency-conflict response already written
	}
	// insertTrackingUnavailable means insert failed for a non-idempotency
	// reason (logged); we still dispatch the A2A request and surface the
	// warning in the response.

	// Build A2A payload. Embedding delegation_id in metadata gives the
	// queue drain path a way to look up the originating delegation row
	// when stitching the response back (issue: previously the drain
	// dispatched successfully but discarded the response, so
	// check_task_status returned status='queued' forever even after a
	// real reply landed). messageId mirrors delegation_id so the
	// platform's idempotency-key extraction also keys off the same id.
	a2aBody, _ := json.Marshal(map[string]interface{}{
		"method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":      "user",
				"messageId": delegationID,
				"parts":     []map[string]interface{}{{"type": "text", "text": body.Task}},
				"metadata":  map[string]interface{}{"delegation_id": delegationID},
			},
		},
	})

	// Fire-and-forget: send A2A in background goroutine
	go h.executeDelegation(sourceID, body.TargetID, delegationID, a2aBody)

	// Broadcast event so canvas shows delegation in real-time
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationSent), sourceID, map[string]interface{}{
		"delegation_id": delegationID,
		"target_id":     body.TargetID,
		"task_preview":  textutil.TruncateBytes(body.Task, 100),
	})

	resp := gin.H{
		"delegation_id": delegationID,
		"status":        "delegated",
		"target_id":     body.TargetID,
	}
	if outcome == insertTrackingUnavailable {
		resp["warning"] = "delegation dispatched but status tracking unavailable"
	}
	c.JSON(http.StatusAccepted, resp)
}

// bindDelegateRequest binds and validates the JSON body. On error it writes
// the 400 response and returns the error so the caller can return.
func bindDelegateRequest(c *gin.Context, body *delegateRequest) error {
	if err := c.ShouldBindJSON(body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid delegation request"})
		return err
	}
	if _, err := uuid.Parse(body.TargetID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_id must be a valid UUID"})
		return err
	}
	return nil
}

// lookupIdempotentDelegation returns true (and writes the response) when an
// existing non-failed delegation matches the (sourceID, idempotencyKey) pair.
// Failed rows are deleted to release the unique slot so the retry can take it.
// Returns false when there's no key, no existing row, or the existing row was
// failed and just deleted.
func lookupIdempotentDelegation(ctx context.Context, c *gin.Context, sourceID, idempotencyKey string) bool {
	if idempotencyKey == "" {
		return false
	}
	var existingID, existingStatus, existingTarget string
	err := db.DB.QueryRowContext(ctx, `
		SELECT request_body->>'delegation_id', status, target_id
		  FROM activity_logs
		 WHERE workspace_id = $1 AND idempotency_key = $2
		 LIMIT 1
	`, sourceID, idempotencyKey).Scan(&existingID, &existingStatus, &existingTarget)
	if err != nil || existingID == "" {
		return false
	}
	if existingStatus == "failed" {
		_, _ = db.DB.ExecContext(ctx, `
			DELETE FROM activity_logs
			 WHERE workspace_id = $1 AND idempotency_key = $2 AND status = 'failed'
		`, sourceID, idempotencyKey)
		return false
	}
	c.JSON(http.StatusOK, gin.H{
		"delegation_id":  existingID,
		"status":         existingStatus,
		"target_id":      existingTarget,
		"idempotent_hit": true,
	})
	return true
}

// insertDelegationOutcome captures the three distinct results of storing
// the pending delegation row, so callers never have to decode a positional
// (bool, bool) tuple.
type insertDelegationOutcome int

const (
	// insertOutcomeUnknown — zero-value sentinel; should never be returned
	// by insertDelegationRow. Exists so that an uninitialized
	// insertDelegationOutcome value doesn't silently alias a real outcome.
	insertOutcomeUnknown insertDelegationOutcome = iota
	// insertOK — row stored; caller continues with dispatch and does NOT
	// surface a tracking warning.
	insertOK
	// insertHandledByIdempotent — a concurrent idempotent request took the
	// slot; the winner's JSON response is already written and the caller
	// MUST return without further writes.
	insertHandledByIdempotent
	// insertTrackingUnavailable — insert failed for a non-idempotency
	// reason (logged by this function); caller continues with dispatch
	// and surfaces a tracking-unavailable warning in the response.
	insertTrackingUnavailable
)

// insertDelegationRow stores the pending delegation row. See
// insertDelegationOutcome for the three possible return values.
func insertDelegationRow(ctx context.Context, c *gin.Context, sourceID string, body delegateRequest, delegationID string) insertDelegationOutcome {
	taskJSON, _ := json.Marshal(map[string]interface{}{
		"task":          body.Task,
		"delegation_id": delegationID,
	})
	var idemArg interface{}
	if body.IdempotencyKey != "" {
		idemArg = body.IdempotencyKey
	}
	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, request_body, status, idempotency_key)
		VALUES ($1, 'delegation', 'delegate', $2, $3, $4, $5::jsonb, 'pending', $6)
	`, sourceID, sourceID, body.TargetID, "Delegating to "+body.TargetID, string(taskJSON), idemArg)
	if err == nil {
		// RFC #2829 #318 — mirror to the durable delegations ledger
		// (gated by DELEGATION_LEDGER_WRITE; default off → no-op).
		recordLedgerInsert(ctx, sourceID, body.TargetID, delegationID, body.Task, body.IdempotencyKey)
		return insertOK
	}
	// A unique-constraint hit means a concurrent request just took the
	// slot — rare, but worth surfacing as the same idempotent response
	// rather than a generic 500. Re-query to fetch the winner's id.
	if body.IdempotencyKey != "" {
		var winnerID, winnerStatus string
		if qerr := db.DB.QueryRowContext(ctx, `
			SELECT request_body->>'delegation_id', status
			  FROM activity_logs
			 WHERE workspace_id = $1 AND idempotency_key = $2
			 LIMIT 1
		`, sourceID, body.IdempotencyKey).Scan(&winnerID, &winnerStatus); qerr == nil && winnerID != "" {
			c.JSON(http.StatusOK, gin.H{
				"delegation_id":  winnerID,
				"status":         winnerStatus,
				"target_id":      body.TargetID,
				"idempotent_hit": true,
			})
			return insertHandledByIdempotent
		}
	}
	log.Printf("Delegation: failed to store: %v", err)
	return insertTrackingUnavailable
}

// executeDelegation runs in a goroutine — sends A2A and stores the result.
// Updates delegation status through: pending → dispatched → received → completed/failed
// delegationRetryDelay is the pause between the first failed proxy attempt
// and the retry. The first failure triggers `proxyA2ARequest`'s reactive
// health check (marks workspace offline, clears cached URL, triggers
// container restart). This delay gives the restart + re-register a chance
// to land a fresh URL in the cache before we try again. Fixes #74 —
// bulk restarts used to produce spurious "failed to reach workspace
// agent" errors when delegations fired within the warm-up window.
const delegationRetryDelay = 8 * time.Second

func (h *DelegationHandler) executeDelegation(sourceID, targetID, delegationID string, a2aBody []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	log.Printf("Delegation %s: %s → %s (dispatched)", delegationID, sourceID, targetID)

	// Update status: pending → dispatched
	h.updateDelegationStatus(sourceID, delegationID, "dispatched", "")
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationStatus), sourceID, map[string]interface{}{
		"delegation_id": delegationID, "target_id": targetID, "status": "dispatched",
	})

	status, respBody, proxyErr := h.workspace.proxyA2ARequest(ctx, targetID, a2aBody, sourceID, true)

	// #74: one retry after the reactive URL refresh has had a chance to
	// run. The proxyA2ARequest's health-check path on a connection error
	// marks the workspace offline, clears cached keys, and kicks off a
	// restart — all on the *next* request's benefit, not this one. A short
	// pause + second attempt catches the common restart-race case where
	// the first attempt sees a stale 127.0.0.1:<ephemeral> URL from a
	// container that was just recreated.
	if proxyErr != nil && isTransientProxyError(proxyErr) {
		log.Printf("Delegation %s: first attempt failed (%s) — retrying in %s after reactive URL refresh",
			delegationID, proxyErr.Error(), delegationRetryDelay)
		select {
		case <-ctx.Done():
			// outer timeout hit before retry window elapsed
		case <-time.After(delegationRetryDelay):
			status, respBody, proxyErr = h.workspace.proxyA2ARequest(ctx, targetID, a2aBody, sourceID, true)
		}
	}

	if proxyErr != nil {
		log.Printf("Delegation %s: failed — %s", delegationID, proxyErr.Error())
		h.updateDelegationStatus(sourceID, delegationID, "failed", proxyErr.Error())

		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, status, error_detail)
			VALUES ($1, 'delegation', 'delegate_result', $2, $3, $4, 'failed', $5)
		`, sourceID, sourceID, targetID, "Delegation failed", proxyErr.Error()); err != nil {
			log.Printf("Delegation %s: failed to insert error log: %v", delegationID, err)
		}

		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationFailed), sourceID, map[string]interface{}{
			"delegation_id": delegationID, "target_id": targetID, "error": proxyErr.Error(),
		})
		// RFC #2829 PR-2 result-push (see UpdateStatus for rationale).
		pushDelegationResultToInbox(ctx, sourceID, delegationID, "failed", "", proxyErr.Error())
		return
	}

	// 202 + {queued: true} means the target was busy and the proxy
	// enqueued the request for the next drain tick — NOT a completion.
	// Treat it as such: write a clean 'queued' activity row with no
	// JSON-as-text leakage into the summary, broadcast a status update,
	// and return. The eventual drain doesn't (yet) feed a result back
	// into this delegation, so callers polling check_task_status will
	// see status='queued' and know to retry instead of believing the
	// queued JSON is the agent's reply. Fixes the chat-leak where the
	// LLM echoed "Delegation completed (workspace agent busy ...)" to
	// the user.
	if status == http.StatusAccepted && isQueuedProxyResponse(respBody) {
		log.Printf("Delegation %s: target %s busy — queued for drain", delegationID, targetID)
		h.updateDelegationStatus(sourceID, delegationID, "queued", "")
		// Store delegation_id in response_body so DrainQueueForWorkspace's
		// stitch step can find this row by JSON-path key after the queued
		// dispatch eventually succeeds. Without the key, the drain finds
		// the row by (workspace_id, target_id, method) but can't tell
		// multiple-queued-delegations-to-same-target apart.
		queuedJSON, _ := json.Marshal(map[string]interface{}{
			"delegation_id": delegationID,
			"queued":        true,
		})
		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, response_body, status)
			VALUES ($1, 'delegation', 'delegate_result', $2, $3, $4, $5::jsonb, 'queued')
		`, sourceID, sourceID, targetID, "Delegation queued — target at capacity", string(queuedJSON)); err != nil {
			log.Printf("Delegation %s: failed to insert queued log: %v", delegationID, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationStatus), sourceID, map[string]interface{}{
			"delegation_id": delegationID, "target_id": targetID, "status": "queued",
		})
		return
	}

	// A2A returned 200 — target received and processed the task
	// Status: dispatched → received → completed (we don't have a separate "received" signal from the target yet)
	responseText := extractResponseText(respBody)
	log.Printf("Delegation %s: completed (status=%d, %d chars)", delegationID, status, len(responseText))

	// Store success (response_body must be JSONB, include delegation_id)
	respJSON, _ := json.Marshal(map[string]interface{}{
		"text":          responseText,
		"delegation_id": delegationID,
	})
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, response_body, status)
		VALUES ($1, 'delegation', 'delegate_result', $2, $3, $4, $5::jsonb, 'completed')
	`, sourceID, sourceID, targetID, "Delegation completed ("+textutil.TruncateBytes(responseText, 80)+")", string(respJSON)); err != nil {
		log.Printf("Delegation %s: failed to insert success log: %v", delegationID, err)
	}

	// RFC #2829 #318: write the ledger row with result_preview FIRST,
	// THEN updateDelegationStatus. Order matters: SetStatus has a
	// same-status replay no-op — if updateDelegationStatus's nested
	// recordLedgerStatus(completed, "", "") fires first, the outer call
	// hits the no-op branch and result_preview is never written.
	// Caught by the local-Postgres integration test in
	// delegation_ledger_integration_test.go.
	recordLedgerStatus(ctx, delegationID, "completed", "", responseText)
	h.updateDelegationStatus(sourceID, delegationID, "completed", "")
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationComplete), sourceID, map[string]interface{}{
		"delegation_id":    delegationID,
		"target_id":        targetID,
		"response_preview": textutil.TruncateBytes(responseText, 200),
	})
	// RFC #2829 PR-2 result-push (see UpdateStatus for rationale).
	pushDelegationResultToInbox(ctx, sourceID, delegationID, "completed", responseText, "")
}

// updateDelegationStatus updates the status of a delegation record in activity_logs.
func (h *DelegationHandler) updateDelegationStatus(workspaceID, delegationID, status, errorDetail string) {
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE activity_logs
		SET status = $1, error_detail = CASE WHEN $2 = '' THEN error_detail ELSE $2 END
		WHERE workspace_id = $3
		  AND method = 'delegate'
		  AND request_body->>'delegation_id' = $4
	`, status, errorDetail, workspaceID, delegationID); err != nil {
		log.Printf("Delegation %s: status update failed: %v", delegationID, err)
	}
	// RFC #2829 #318 — mirror status transition to the durable ledger
	// (gated). Note: the ledger uses different vocabulary for "pending"
	// (its initial state is `queued`); map "received" / unknown values
	// the ledger doesn't accept by skipping them rather than failing.
	switch status {
	case "queued", "dispatched", "in_progress", "completed", "failed", "stuck":
		recordLedgerStatus(ctx, delegationID, status, errorDetail, "")
	}
}

// Record handles POST /workspaces/:id/delegations/record — the agent-initiated
// "I just fired a delegation directly via A2A, please record it" endpoint (#64).
//
// The canvas-driven POST /delegate endpoint records to activity_logs AND fires
// the A2A request. Agents calling delegate_to_workspace fire A2A themselves
// (preserves OTEL trace-context propagation + retry logic) — this endpoint
// lets them register the row without double-firing the request.
//
// Body: {"target_id": "...", "task": "...", "delegation_id": "..."}
//   - delegation_id is the agent-generated task_id (matches what
//     check_delegation_status returns, so a single ID correlates the two
//     views).
func (h *DelegationHandler) Record(c *gin.Context) {
	sourceID := c.Param("id")
	ctx := c.Request.Context()

	var body struct {
		TargetID     string `json:"target_id" binding:"required"`
		Task         string `json:"task" binding:"required"`
		DelegationID string `json:"delegation_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if _, err := uuid.Parse(body.TargetID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_id must be a valid UUID"})
		return
	}

	taskJSON, _ := json.Marshal(map[string]interface{}{
		"task":          body.Task,
		"delegation_id": body.DelegationID,
	})
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, request_body, status)
		VALUES ($1, 'delegation', 'delegate', $2, $3, $4, $5::jsonb, 'dispatched')
	`, sourceID, sourceID, body.TargetID, "Delegating to "+body.TargetID, string(taskJSON)); err != nil {
		log.Printf("Delegation Record: insert failed for %s: %v", body.DelegationID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record delegation"})
		return
	}

	// RFC #2829 #318 — mirror to durable ledger (gated). Record always
	// reflects an A2A request the agent already fired itself, so the
	// initial activity_logs status is 'dispatched' — but the ledger's
	// CHECK constraint only accepts 'queued' as the initial state via
	// Insert. Insert as queued first; the very next SetStatus(...,
	// dispatched) below promotes it to dispatched on the same row.
	recordLedgerInsert(ctx, sourceID, body.TargetID, body.DelegationID, body.Task, "")
	recordLedgerStatus(ctx, body.DelegationID, "dispatched", "", "")

	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationSent), sourceID, map[string]interface{}{
		"delegation_id": body.DelegationID,
		"target_id":     body.TargetID,
		"task_preview":  textutil.TruncateBytes(body.Task, 100),
	})

	c.JSON(http.StatusAccepted, gin.H{
		"delegation_id": body.DelegationID,
		"status":        "recorded",
	})
}

// UpdateStatus handles POST /workspaces/:id/delegations/:delegation_id/update — agent
// reports completion/failure for a delegation it recorded via Record (#64).
//
// Body: {"status": "completed"|"failed", "error": "...", "response_preview": "..."}
func (h *DelegationHandler) UpdateStatus(c *gin.Context) {
	sourceID := c.Param("id")
	delegationID := c.Param("delegation_id")
	ctx := c.Request.Context()

	var body struct {
		Status          string `json:"status" binding:"required"`
		Error           string `json:"error,omitempty"`
		ResponsePreview string `json:"response_preview,omitempty"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if body.Status != "completed" && body.Status != "failed" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be 'completed' or 'failed'"})
		return
	}

	// RFC #2829 #318 — same ordering pin as executeDelegation completion:
	// write the with-preview ledger row FIRST so updateDelegationStatus's
	// inner same-status no-op doesn't clobber preview.
	if body.Status == "completed" {
		recordLedgerStatus(ctx, delegationID, "completed", "", body.ResponsePreview)
	}

	h.updateDelegationStatus(sourceID, delegationID, body.Status, body.Error)

	if body.Status == "completed" {
		respJSON, _ := json.Marshal(map[string]interface{}{
			"text":          body.ResponsePreview,
			"delegation_id": delegationID,
		})
		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, summary, response_body, status)
			VALUES ($1, 'delegation', 'delegate_result', $2, $3, $4::jsonb, 'completed')
		`, sourceID, sourceID, "Delegation completed ("+textutil.TruncateBytes(body.ResponsePreview, 80)+")", string(respJSON)); err != nil {
			log.Printf("Delegation UpdateStatus: result insert failed for %s: %v", delegationID, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationComplete), sourceID, map[string]interface{}{
			"delegation_id":    delegationID,
			"response_preview": textutil.TruncateBytes(body.ResponsePreview, 200),
		})
		// RFC #2829 PR-2 result-push: when the gate is on, also write an
		// a2a_receive row so the caller's inbox poller surfaces this to
		// the agent. Foundational for getting rid of the proxy-blocked
		// sync path that hits the 600s message/send timeout — once the
		// agent-side cutover lands, the caller polls its own inbox for
		// the result instead of holding open an HTTP connection.
		pushDelegationResultToInbox(ctx, sourceID, delegationID, "completed", body.ResponsePreview, "")
	} else {
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationFailed), sourceID, map[string]interface{}{
			"delegation_id": delegationID,
			"error":         body.Error,
		})
		pushDelegationResultToInbox(ctx, sourceID, delegationID, "failed", "", body.Error)
	}

	c.JSON(http.StatusOK, gin.H{"status": body.Status, "delegation_id": delegationID})
}

// ListDelegations handles GET /workspaces/:id/delegations
// Returns recent delegations for a workspace with their status.
func (h *DelegationHandler) ListDelegations(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, activity_type, COALESCE(source_id::text, ''), COALESCE(target_id::text, ''),
		       COALESCE(summary, ''), COALESCE(status, ''), COALESCE(error_detail, ''),
		       COALESCE(response_body->>'text', response_body::text, ''),
		       COALESCE(request_body->>'delegation_id', response_body->>'delegation_id', ''),
		       created_at
		FROM activity_logs
		WHERE workspace_id = $1 AND method IN ('delegate', 'delegate_result')
		ORDER BY created_at DESC
		LIMIT 50
	`, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	var delegations []map[string]interface{}
	for rows.Next() {
		var id, actType, sourceID, targetID, summary, status, errorDetail, responseBody, delegationID string
		var createdAt time.Time
		if err := rows.Scan(&id, &actType, &sourceID, &targetID, &summary, &status, &errorDetail, &responseBody, &delegationID, &createdAt); err != nil {
			continue
		}
		entry := map[string]interface{}{
			"id":         id,
			"type":       actType,
			"source_id":  sourceID,
			"target_id":  targetID,
			"summary":    summary,
			"status":     status,
			"created_at": createdAt,
		}
		if delegationID != "" {
			entry["delegation_id"] = delegationID
		}
		if errorDetail != "" {
			entry["error"] = errorDetail
		}
		if responseBody != "" {
			entry["response_preview"] = textutil.TruncateBytes(responseBody, 300)
		}
		delegations = append(delegations, entry)
	}

	if delegations == nil {
		delegations = []map[string]interface{}{}
	}
	c.JSON(http.StatusOK, delegations)
}

// --- helpers ---

// isTransientProxyError returns true when the proxy error is a restart-race
// condition worth retrying (connection refused, stale ephemeral-port URL after
// a container restart). Static 4xx and generic 5xx errors are NOT retried.
//
// 503 requires careful splitting (#689): the proxy emits two distinct 503 shapes
// that must be handled differently:
//   - "restarting: true" — container was dead; restart triggered. The POST body
//     was never delivered (dead container can't accept TCP). Safe to retry.
//   - "busy: true" — agent is alive, mid-synthesis on a previous request. The
//     POST body WAS likely delivered. Retrying double-delivers the message.
//     Do NOT retry; surface the 503 to the caller instead.
func isTransientProxyError(err *proxyA2AError) bool {
	if err == nil {
		return false
	}
	// 502 = "failed to reach workspace agent" (connection refused / DNS failure).
	// The message was NOT delivered. Safe to retry after reactive URL refresh (#74).
	if err.Status == http.StatusBadGateway {
		return true
	}
	// 503 with restarting:true = container died → message not delivered → retry.
	// 503 with busy:true (or no flag) = agent alive → message may be delivered → no retry.
	if err.Status == http.StatusServiceUnavailable {
		if restart, ok := err.Response["restarting"].(bool); ok && restart {
			return true
		}
		return false
	}
	return false
}

// isQueuedProxyResponse reports whether the proxy returned a body shaped like
// `{"queued": true, "queue_id": ..., "queue_depth": ..., "message": ...}` —
// the busy-target enqueue path in a2a_proxy_helpers.go. Caller checks this
// alongside HTTP 202 to distinguish a successful agent reply from a deferred
// dispatch; without the distinction we'd write the queued-message JSON into
// the delegation result row and the LLM would surface it as agent output.
func isQueuedProxyResponse(body []byte) bool {
	var resp map[string]interface{}
	if json.Unmarshal(body, &resp) != nil {
		return false
	}
	queued, _ := resp["queued"].(bool)
	return queued
}

func extractResponseText(body []byte) string {
	var resp map[string]interface{}
	if json.Unmarshal(body, &resp) != nil {
		return string(body)
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		return string(body)
	}
	// Check top-level parts
	if parts, ok := result["parts"].([]interface{}); ok {
		for _, p := range parts {
			if part, ok := p.(map[string]interface{}); ok {
				if kind, _ := part["kind"].(string); kind == "text" {
					if text, ok := part["text"].(string); ok {
						return text
					}
				}
			}
		}
	}
	// Check artifacts
	if artifacts, ok := result["artifacts"].([]interface{}); ok {
		for _, a := range artifacts {
			if art, ok := a.(map[string]interface{}); ok {
				if parts, ok := art["parts"].([]interface{}); ok {
					for _, p := range parts {
						if part, ok := p.(map[string]interface{}); ok {
							if kind, _ := part["kind"].(string); kind == "text" {
								if text, ok := part["text"].(string); ok {
									return text
								}
							}
						}
					}
				}
			}
		}
	}
	return string(body)
}

