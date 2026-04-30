package handlers

// a2a_queue_status.go — RFC #2331 Tier 1: public per-queue-id status endpoint.
//
// Closes the gap surfaced in #2329 item 5: callers receive `queue_id` in
// the 202 enqueue response but had no public lookup endpoint. The only
// observability path was through `check_task_status` which joins via
// `request_body->>'delegation_id'` in `activity_logs` — works only for
// delegation-flavored A2A. Cross-workspace peer-direct A2A had no
// observability after enqueue.
//
// Auth model:
//
//   - The caller's workspace token must match the `caller_id` recorded
//     on the queue row at enqueue time, OR the caller's token must be
//     for the target workspace_id (target can see what's queued for it),
//     OR an org-level token (canvas/admin) can see anything.
//
//   - 404 — not 403 — when the caller has no read access. The queue_id
//     UUID is the access token; revealing "this queue_id exists but
//     you can't see it" leaks the existence-of-other-callers' state.
//
// What the response body excludes:
//
//   - `body` (the original JSON-RPC request body) — could contain
//     prompts/PII the caller's authority shouldn't include in poll-loop
//     responses. The body is only relevant to the dispatching agent.
//   - `caller_id` — exposes the existence of other callers.
//
// What it includes:
//
//   - status, attempts, last_error, enqueued_at, dispatched_at,
//     completed_at, expires_at, priority — the delivery state machine
//     observables.
//   - response_body when status == completed — so the caller can
//     retrieve the response without polling check_task_status.

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
	"github.com/gin-gonic/gin"
)

// QueueStatus is the public projection of an a2a_queue row.
type QueueStatus struct {
	ID           string  `json:"queue_id"`
	WorkspaceID  string  `json:"workspace_id"`
	Status       string  `json:"status"`
	Priority     int     `json:"priority"`
	Attempts     int     `json:"attempts"`
	LastError    *string `json:"last_error,omitempty"`
	EnqueuedAt   string  `json:"enqueued_at"`
	DispatchedAt *string `json:"dispatched_at,omitempty"`
	CompletedAt  *string `json:"completed_at,omitempty"`
	ExpiresAt    *string `json:"expires_at,omitempty"`
	ResponseBody []byte  `json:"response_body,omitempty"`
}

// QueueStatusByID looks up the queue row and projects it for the public
// endpoint. Returns ErrNoQueueRow when the row doesn't exist OR the
// caller has no read access — collapsing the two surfaces a single 404
// from the handler so an attacker can't probe queue_id existence.
//
// Access rules — caller must satisfy at least one of:
//
//	(a) callerID == queue.caller_id        (sender can read own enqueue)
//	(b) callerID == queue.workspace_id     (target can read queued-for-me)
//	(c) isAdmin == true                    (canvas/admin token)
//
// Internal helper; the HTTP handler enforces the auth checks before
// calling this — by the time we get here we already know the caller
// is authorized, so this just runs the SELECT.
func QueueStatusByID(ctx context.Context, queueID string) (*QueueStatus, error) {
	var qs QueueStatus
	var lastError, dispatchedAt, completedAt, expiresAt sql.NullString
	var responseBody []byte

	// response_body lives on activity_logs (the stitched delegation row), not
	// on a2a_queue itself. We pull both here in one round-trip via LEFT JOIN
	// so a completed delegation surfaces its result inline — non-delegation
	// queue rows simply won't have a matching activity_logs row and the field
	// stays null.
	err := db.DB.QueryRowContext(ctx, `
		SELECT
			q.id,
			q.workspace_id,
			q.status,
			q.priority,
			q.attempts,
			q.last_error,
			q.enqueued_at::text,
			q.dispatched_at::text,
			q.completed_at::text,
			q.expires_at::text,
			al.response_body::text
		FROM a2a_queue q
		LEFT JOIN activity_logs al
			ON al.method = 'delegate_result'
			AND al.target_id = q.workspace_id
			AND al.workspace_id = q.caller_id
			AND al.response_body->>'delegation_id' = (q.body->'params'->'message'->'metadata'->>'delegation_id')
		WHERE q.id = $1
	`, queueID).Scan(
		&qs.ID, &qs.WorkspaceID, &qs.Status, &qs.Priority, &qs.Attempts,
		&lastError, &qs.EnqueuedAt, &dispatchedAt, &completedAt, &expiresAt,
		&responseBody,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}

	if lastError.Valid && lastError.String != "" {
		s := lastError.String
		qs.LastError = &s
	}
	if dispatchedAt.Valid && dispatchedAt.String != "" {
		s := dispatchedAt.String
		qs.DispatchedAt = &s
	}
	if completedAt.Valid && completedAt.String != "" {
		s := completedAt.String
		qs.CompletedAt = &s
	}
	if expiresAt.Valid && expiresAt.String != "" {
		s := expiresAt.String
		qs.ExpiresAt = &s
	}
	if len(responseBody) > 0 && qs.Status == "completed" {
		qs.ResponseBody = responseBody
	}

	return &qs, nil
}

// queueRowAuthFields returns the (caller_id, workspace_id) of the queue row
// for access control. Separate from QueueStatusByID so the handler can do
// the auth check without first projecting the public response.
func queueRowAuthFields(ctx context.Context, queueID string) (callerID, workspaceID string, err error) {
	var callerNS, workspaceNS sql.NullString
	err = db.DB.QueryRowContext(ctx,
		`SELECT caller_id, workspace_id FROM a2a_queue WHERE id = $1`,
		queueID,
	).Scan(&callerNS, &workspaceNS)
	if err != nil {
		return "", "", err
	}
	return callerNS.String, workspaceNS.String, nil
}

// GetA2AQueueStatus handles GET /workspaces/:id/a2a/queue/:queue_id.
//
// The :id path param is the workspace context (matches the proxy pattern
// /workspaces/:id/a2a). :queue_id is the row's UUID returned from the
// 202 enqueue response.
//
// Auth flow:
//
//  1. Extract caller's workspace from bearer (org tokens grant org-wide
//     access and short-circuit the per-row check).
//  2. Look up queue row's (caller_id, workspace_id).
//  3. Allow when caller's workspace == queue.caller_id OR
//     == queue.workspace_id, OR caller has org-level access.
//  4. Otherwise 404 (not 403) — see file-header rationale.
func (h *WorkspaceHandler) GetA2AQueueStatus(c *gin.Context) {
	ctx := c.Request.Context()
	queueID := c.Param("queue_id")
	if queueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "queue_id required"})
		return
	}

	// Org-level token (canvas/admin)? Bypass per-row caller match.
	_, isOrg := c.Get("org_token_id")

	// Derive caller workspace from bearer when not org-token.
	callerWorkspace := c.GetHeader("X-Workspace-ID")
	if !isOrg && callerWorkspace == "" {
		if tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization")); tok != "" {
			if wsID, err := wsauth.WorkspaceFromToken(ctx, db.DB, tok); err == nil {
				callerWorkspace = wsID
			}
		}
	}
	if !isOrg && callerWorkspace == "" {
		// No identity — treat as not-found rather than 401, matching the
		// file-header existence-non-inference policy. A 401 would tell
		// an attacker that the queue_id at least might exist.
		c.JSON(http.StatusNotFound, gin.H{"error": "queue item not found"})
		return
	}

	rowCallerID, rowWorkspaceID, err := queueRowAuthFields(ctx, queueID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "queue item not found"})
		return
	}
	if err != nil {
		log.Printf("GetA2AQueueStatus: row lookup failed for %s: %v", queueID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	// Access check.
	if !isOrg && callerWorkspace != rowCallerID && callerWorkspace != rowWorkspaceID {
		// Collapse to 404 — see header.
		c.JSON(http.StatusNotFound, gin.H{"error": "queue item not found"})
		return
	}

	status, err := QueueStatusByID(ctx, queueID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "queue item not found"})
		return
	}
	if err != nil {
		log.Printf("GetA2AQueueStatus: status fetch failed for %s: %v", queueID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "status fetch failed"})
		return
	}

	c.JSON(http.StatusOK, status)
}
