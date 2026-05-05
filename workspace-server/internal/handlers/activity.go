package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type ActivityHandler struct {
	broadcaster *events.Broadcaster
}

func NewActivityHandler(b *events.Broadcaster) *ActivityHandler {
	return &ActivityHandler{broadcaster: b}
}

// List handles GET /workspaces/:id/activity?type=&source=&limit=&since_secs=&since_id=
//
// since_secs filters to activity_logs.created_at >= NOW() - INTERVAL '$N seconds'.
// Optional, additive — callers that don't pass it get today's behavior (the
// most-recent N events regardless of time). The harness runner
// (scripts/measure-coordinator-task-bounds-runner.sh) uses this to scope a
// trace to a specific test window; RFC #2251 §V1.0 step 6 also depends on it.
// Capped at 30 days (2_592_000s) — anything older has typically been paged
// out anyway, and a defensive ceiling keeps a paranoid client from triggering
// a full-table scan via since_secs=99999999999. Closes #2268.
//
// since_id is a CURSOR for poll-mode workspaces (#2339 PR 3). The agent
// passes the id of the last activity_logs row it has consumed; the server
// returns rows STRICTLY AFTER that cursor in chronological (ASC) order so
// the agent processes events in the order they were recorded. Telegram
// getUpdates / Slack RTM shape — same proven pattern.
//
// Cross-workspace safety: the cursor lookup is scoped by workspace_id, so a
// caller cannot peek at another workspace's activity by guessing its UUIDs.
//
// Cursor-not-found: returns 410 Gone. The client should reset its cursor
// (omit since_id) and re-fetch the recent backlog. This avoids the silent
// loss-window where a pruned cursor silently filters everything out.
//
// since_id + since_secs together: both filters apply (AND). Output is ASC
// when since_id is set (polling order), DESC otherwise (recent feed order).
func (h *ActivityHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")
	activityType := c.Query("type")
	source := c.Query("source") // "canvas" = source_id IS NULL, "agent" = source_id IS NOT NULL
	peerID := c.Query("peer_id") // optional UUID — restrict to rows where this peer is sender OR target
	limitStr := c.DefaultQuery("limit", "100")
	sinceSecsStr := c.Query("since_secs")
	sinceID := c.Query("since_id")
	beforeTSStr := c.Query("before_ts") // optional RFC3339 — return rows strictly older than this timestamp

	// Validate peer_id as a UUID at the trust boundary so a malformed
	// caller (the agent or a downstream MCP tool) can't smuggle SQL
	// fragments into the WHERE clause via the parameter, even though
	// args are bound. UUID-shape rejection is also the cleanest 400
	// signal for the wheel-side chat_history MCP tool — clearer than a
	// generic "no rows" empty list when the agent passed an obviously
	// wrong id.
	if peerID != "" {
		if _, err := uuid.Parse(peerID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "peer_id must be a UUID"})
			return
		}
	}

	// Parse before_ts as the wall-clock paging knob for the wheel-side
	// `chat_history` MCP tool. The agent passes the oldest `created_at`
	// from a previous response to walk backward through long histories.
	// Validated as RFC3339 at the trust boundary so a typoed value
	// surfaces as a clean 400 instead of being silently ignored.
	var beforeTS time.Time
	usingBeforeTS := false
	if beforeTSStr != "" {
		t, err := time.Parse(time.RFC3339, beforeTSStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "before_ts must be an RFC3339 timestamp (e.g. 2026-05-01T00:00:00Z)",
			})
			return
		}
		beforeTS = t
		usingBeforeTS = true
	}

	limit := 100
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
		limit = n
		if limit > 500 {
			limit = 500
		}
	}

	// Parse since_secs. Reject negative or non-integer values rather than
	// silently ignoring them — a typoed param shouldn't be lost as
	// most-recent-100, that's exactly the bug this fixes.
	var sinceSecs int
	if sinceSecsStr != "" {
		n, err := strconv.Atoi(sinceSecsStr)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since_secs must be a positive integer"})
			return
		}
		const maxSinceSecs = 30 * 24 * 60 * 60 // 30 days
		if n > maxSinceSecs {
			n = maxSinceSecs
		}
		sinceSecs = n
	}

	// Resolve since_id cursor (if set) BEFORE building the main query so we
	// can 410 cleanly when the cursor row is gone — and so the cursor's
	// created_at is bound as a regular timestamp parameter (not a subquery)
	// for clean sqlmock matching and to keep the planner predictable.
	//
	// The lookup is scoped by workspace_id: a caller cannot enumerate or
	// peek at another workspace's events by passing a UUID belonging to a
	// different workspace. Mismatched-workspace cursor → 410, same as
	// "row not found" — both indicate the cursor is no longer usable for
	// this caller, no information leak.
	var cursorTime time.Time
	usingCursor := false
	if sinceID != "" {
		err := db.DB.QueryRowContext(c.Request.Context(),
			`SELECT created_at FROM activity_logs WHERE id = $1 AND workspace_id = $2`,
			sinceID, workspaceID,
		).Scan(&cursorTime)
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusGone, gin.H{
				"error": "since_id cursor not found (row may have been pruned or belongs to a different workspace); omit since_id to reset",
			})
			return
		}
		if err != nil {
			log.Printf("Activity since_id cursor lookup error for ws=%s id=%s: %v", workspaceID, sinceID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "cursor lookup failed"})
			return
		}
		usingCursor = true
	}

	// Build query with optional filters
	query := `SELECT id, workspace_id, activity_type, source_id, target_id, method,
			   summary, request_body, response_body, tool_trace, duration_ms, status, error_detail, created_at
		FROM activity_logs WHERE workspace_id = $1`
	args := []interface{}{workspaceID}
	argIdx := 2

	if activityType != "" {
		query += fmt.Sprintf(" AND activity_type = $%d", argIdx)
		args = append(args, activityType)
		argIdx++
	}
	if source == "canvas" {
		query += " AND source_id IS NULL"
	} else if source == "agent" {
		query += " AND source_id IS NOT NULL"
	} else if source != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source must be 'canvas' or 'agent'"})
		return
	}
	if peerID != "" {
		// Restrict to rows where this peer is either the sender (source_id)
		// or the recipient (target_id) of an A2A turn. This is the
		// "conversation history with peer X" view the wheel-side
		// chat_history MCP tool surfaces — agent receives a peer_agent
		// push, wants to see the prior 20 turns with that workspace
		// without paging through every other peer's traffic.
		//
		// Bound as a single arg, matched twice — keeps argIdx accurate
		// and avoids duplicate parameter binding (some drivers reject the
		// same arg slot reused, ours is fine but the explicit form is
		// clearer to read and matches the rest of the builder.)
		query += fmt.Sprintf(" AND (source_id = $%d OR target_id = $%d)", argIdx, argIdx)
		args = append(args, peerID)
		argIdx++
	}
	if usingBeforeTS {
		// Strictly older — never replay a row with the exact same
		// timestamp, mirrors the `created_at > cursorTime` shape
		// `since_id` uses for forward paging.
		query += fmt.Sprintf(" AND created_at < $%d", argIdx)
		args = append(args, beforeTS)
		argIdx++
	}
	if sinceSecs > 0 {
		// Use a parameterized interval so the value is bound, not
		// interpolated into the SQL string. `make_interval(secs => $N)`
		// avoids the lib/pq quirk where INTERVAL '$N seconds' won't
		// substitute a placeholder inside the literal.
		query += fmt.Sprintf(" AND created_at >= NOW() - make_interval(secs => $%d)", argIdx)
		args = append(args, sinceSecs)
		argIdx++
	}
	if usingCursor {
		// Strictly after — never replay the cursor row itself.
		query += fmt.Sprintf(" AND created_at > $%d", argIdx)
		args = append(args, cursorTime)
		argIdx++
	}

	// Polling clients (since_id) need oldest-first within the new window so
	// they process events in recorded order. The recent-feed view (no
	// since_id) keeps DESC — that's the canvas/UI shape and changing it
	// would surprise existing callers.
	if usingCursor {
		query += fmt.Sprintf(" ORDER BY created_at ASC LIMIT $%d", argIdx)
	} else {
		query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", argIdx)
	}
	args = append(args, limit)

	rows, err := db.DB.QueryContext(c.Request.Context(), query, args...)

	if err != nil {
		log.Printf("Activity list error for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	activities := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, wsID, actType, status string
		var sourceID, targetID, method, summary, errorDetail *string
		var reqBody, respBody, toolTrace []byte
		var durationMs *int
		var createdAt time.Time

		if err := rows.Scan(&id, &wsID, &actType, &sourceID, &targetID, &method,
			&summary, &reqBody, &respBody, &toolTrace, &durationMs, &status, &errorDetail, &createdAt); err != nil {
			log.Printf("Activity scan error: %v", err)
			continue
		}

		entry := map[string]interface{}{
			"id":            id,
			"workspace_id":  wsID,
			"activity_type": actType,
			"source_id":     sourceID,
			"target_id":     targetID,
			"method":        method,
			"summary":       summary,
			"duration_ms":   durationMs,
			"status":        status,
			"error_detail":  errorDetail,
			"created_at":    createdAt,
		}
		if reqBody != nil {
			entry["request_body"] = json.RawMessage(reqBody)
		}
		if respBody != nil {
			entry["response_body"] = json.RawMessage(respBody)
		}
		if toolTrace != nil {
			entry["tool_trace"] = json.RawMessage(toolTrace)
		}
		activities = append(activities, entry)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Activity list rows error for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query iteration failed"})
		return
	}
	c.JSON(http.StatusOK, activities)
}

// SessionSearch handles GET /workspaces/:id/session-search?q=&limit=
// It searches the workspace's own activity logs and memories without adding a new storage layer.
func (h *ActivityHandler) SessionSearch(c *gin.Context) {
	workspaceID := c.Param("id")
	query, limit := parseSessionSearchParams(c)

	sqlQuery, args := buildSessionSearchQuery(workspaceID, query, limit)

	rows, err := db.DB.QueryContext(c.Request.Context(), sqlQuery, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session search failed"})
		return
	}
	defer rows.Close()

	items, scanErr := scanSessionSearchRows(rows)
	if scanErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query iteration failed"})
		return
	}

	c.JSON(http.StatusOK, items)
}

// parseSessionSearchParams extracts the `q` and `limit` query params for SessionSearch,
// applying the default limit (50) and cap (200).
func parseSessionSearchParams(c *gin.Context) (string, int) {
	query := strings.TrimSpace(c.DefaultQuery("q", ""))
	limit := 50
	if n, err := strconv.Atoi(c.DefaultQuery("limit", "50")); err == nil && n > 0 {
		limit = n
		if limit > 200 {
			limit = 200
		}
	}
	return query, limit
}

// buildSessionSearchQuery composes the UNION-ALL SQL across activity_logs and
// agent_memories with an optional ILIKE filter, returning the SQL string and
// positional args ready for QueryContext.
func buildSessionSearchQuery(workspaceID, query string, limit int) (string, []interface{}) {
	sqlQuery := `
		WITH session_items AS (
			SELECT
				'activity' AS kind,
				id,
				workspace_id,
				activity_type AS label,
				COALESCE(summary, '') AS content,
				COALESCE(method, '') AS method,
				COALESCE(status, '') AS status,
				request_body,
				response_body,
				created_at
			FROM activity_logs
			WHERE workspace_id = $1
			UNION ALL
			SELECT
				'memory' AS kind,
				id,
				workspace_id,
				scope AS label,
				content,
				'' AS method,
				'' AS status,
				NULL::jsonb AS request_body,
				NULL::jsonb AS response_body,
				created_at
			FROM agent_memories
			WHERE workspace_id = $1
		)
		SELECT kind, id, workspace_id, label, content, method, status, request_body, response_body, created_at
		FROM session_items
	`

	args := []interface{}{workspaceID}
	if query != "" {
		sqlQuery += `
		WHERE (
			content ILIKE $2 OR
			label ILIKE $2 OR
			method ILIKE $2 OR
			status ILIKE $2 OR
			COALESCE(request_body::text, '') ILIKE $2 OR
			COALESCE(response_body::text, '') ILIKE $2
		)`
		args = append(args, "%"+query+"%")
	}

	sqlQuery += ` ORDER BY created_at DESC LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit)
	return sqlQuery, args
}

// scanSessionSearchRows materialises rows from the SessionSearch query into the
// JSON-shaped maps the endpoint returns. Per-row scan errors are logged and
// skipped (matches prior behavior); a rows.Err() failure is surfaced.
func scanSessionSearchRows(rows interface {
	Next() bool
	Scan(dest ...interface{}) error
	Err() error
}) ([]map[string]interface{}, error) {
	items := make([]map[string]interface{}, 0)
	for rows.Next() {
		var (
			kind, id, wsID, label, content, method, status string
			reqBody, respBody                              []byte
			createdAt                                      time.Time
		)
		if err := rows.Scan(&kind, &id, &wsID, &label, &content, &method, &status, &reqBody, &respBody, &createdAt); err != nil {
			log.Printf("Session search scan error: %v", err)
			continue
		}

		item := map[string]interface{}{
			"kind":         kind,
			"id":           id,
			"workspace_id": wsID,
			"label":        label,
			"content":      content,
			"method":       method,
			"status":       status,
			"created_at":   createdAt,
		}
		if reqBody != nil {
			item["request_body"] = json.RawMessage(reqBody)
		}
		if respBody != nil {
			item["response_body"] = json.RawMessage(respBody)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Session search rows error: %v", err)
		return nil, err
	}
	return items, nil
}

// NotifyAttachment is one file the agent wants to attach to its push.
// URIs come from /workspaces/:id/chat/uploads (canonical "workspace:"
// scheme) — the runtime's tool_send_message_to_user uploads any
// caller-specified file path through that endpoint first to get a
// shape the canvas can resolve via the existing Download path.
type NotifyAttachment struct {
	URI      string `json:"uri" binding:"required"`
	Name     string `json:"name" binding:"required"`
	MimeType string `json:"mimeType,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// Notify handles POST /workspaces/:id/notify — agents push messages to the canvas chat.
// This enables agents to send interim updates ("I'll check on it") and follow-up results
// without waiting for the user to poll. Messages are broadcast via WebSocket only.
//
// Attachments: optional list of file references. Each renders as a
// download chip in the canvas via the existing extractFilesFromTask
// path. The runtime tool uploads file bytes to /chat/uploads first
// and passes the returned URIs here, so this handler only stores
// metadata — never raw bytes.
func (h *ActivityHandler) Notify(c *gin.Context) {
	workspaceID := c.Param("id")
	var body struct {
		Message     string             `json:"message" binding:"required"`
		Attachments []NotifyAttachment `json:"attachments,omitempty"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message is required"})
		return
	}

	// Per-element attachment validation: gin's go-playground/validator
	// does NOT iterate slice elements without `dive`, so the inner
	// `binding:"required"` tags on NotifyAttachment.URI/Name don't
	// actually run. Without this loop, attachments: [{"uri":"","name":""}]
	// would slip through, broadcast empty-URI chips that render
	// blank/broken in the canvas, and persist them in activity_logs
	// for every page reload to re-render. Validate explicitly.
	for i, a := range body.Attachments {
		if a.URI == "" || a.Name == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("attachment[%d]: uri and name are required", i),
			})
			return
		}
	}

	// Single source of truth for chat-bearing agent → user messages —
	// see agent_message_writer.go for the contract. Pre-RFC-#2945, the
	// broadcast + INSERT pair was inlined here and again in
	// mcp_tools.go's send_message_to_user, and the duplication is what
	// produced the reno-stars data-loss regression. Both paths now
	// route through the same writer; future channels (Slack, Discord,
	// Lark) hook in here too.
	attachments := make([]AgentMessageAttachment, 0, len(body.Attachments))
	for _, a := range body.Attachments {
		attachments = append(attachments, AgentMessageAttachment{
			URI:      a.URI,
			Name:     a.Name,
			MimeType: a.MimeType,
			Size:     a.Size,
		})
	}
	writer := NewAgentMessageWriter(db.DB, h.broadcaster)
	if err := writer.Send(c.Request.Context(), workspaceID, body.Message, attachments); err != nil {
		if errors.Is(err, ErrWorkspaceNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "sent"})
}

// Report handles POST /workspaces/:id/activity — agents self-report activity logs.
func (h *ActivityHandler) Report(c *gin.Context) {
	workspaceID := c.Param("id")
	var body struct {
		ActivityType string      `json:"activity_type" binding:"required"`
		Method       string      `json:"method"`
		Summary      string      `json:"summary"`
		TargetID     string      `json:"target_id"`
		SourceID     string      `json:"source_id"`
		Status       string      `json:"status"`
		ErrorDetail  string      `json:"error_detail"`
		DurationMs   *int        `json:"duration_ms"`
		RequestBody  interface{} `json:"request_body"`
		ResponseBody interface{} `json:"response_body"`
		Metadata     interface{} `json:"metadata"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Validate activity type. memory_write was added per #125 so the
	// commit_memory tool can surface in the Canvas Agent Comms tab —
	// previously its writes were invisible outside the agent_memories
	// table.
	switch body.ActivityType {
	case "a2a_send", "a2a_receive", "task_update", "agent_log", "skill_promotion", "memory_write", "error":
		// valid
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid activity_type, must be one of: a2a_send, a2a_receive, task_update, agent_log, skill_promotion, memory_write, error"})
		return
	}

	status := body.Status
	if status == "" {
		status = "ok"
	}

	// Resolve request/response body — prefer explicit fields, fall back to metadata
	reqBody := body.RequestBody
	if reqBody == nil {
		reqBody = body.Metadata
	}
	// C2 (from #169) — source_id spoof defense. WorkspaceAuth middleware
	// already proves the caller owns :id, but that check doesn't cover the
	// body field. Without this guard, workspace A authenticated for its own
	// /activity endpoint could still set source_id=<workspace B's UUID> in
	// the payload and attribute the log to B. Reject any body where
	// source_id is non-empty AND differs from the authenticated workspace.
	// Empty source_id falls through to the default-to-self branch below.
	sourceID := body.SourceID
	if sourceID != "" && sourceID != workspaceID {
		// #234: sanitize attacker-controlled values before logging.
		// body.SourceID comes from a JSON request, and json.Unmarshal
		// decodes \n escapes into literal newlines — an authenticated
		// workspace could otherwise inject fake log lines. Use %q which
		// emits a Go-quoted string with all control characters escaped.
		log.Printf("security: source_id spoof attempt — authed_workspace=%s body_source_id=%q remote=%q",
			workspaceID, sourceID, c.ClientIP())
		c.JSON(http.StatusForbidden, gin.H{"error": "source_id must match authenticated workspace"})
		return
	}
	if sourceID == "" {
		sourceID = workspaceID
	}

	LogActivity(c.Request.Context(), h.broadcaster, ActivityParams{
		WorkspaceID:  workspaceID,
		ActivityType: body.ActivityType,
		SourceID:     &sourceID,
		TargetID:     nilIfEmpty(body.TargetID),
		Method:       nilIfEmpty(body.Method),
		Summary:      nilIfEmpty(body.Summary),
		RequestBody:  reqBody,
		ResponseBody: body.ResponseBody,
		DurationMs:   body.DurationMs,
		Status:       status,
		ErrorDetail:  nilIfEmpty(body.ErrorDetail),
	})

	c.JSON(http.StatusOK, gin.H{"status": "logged"})
}

// LogActivity inserts an activity log and optionally broadcasts via WebSocket.
// Takes events.EventEmitter (#1814) so callers passing a stub broadcaster
// in tests no longer need to construct the full *events.Broadcaster.
func LogActivity(ctx context.Context, broadcaster events.EventEmitter, params ActivityParams) {
	reqJSON, reqErr := json.Marshal(params.RequestBody)
	if reqErr != nil {
		log.Printf("LogActivity: failed to marshal request_body for %s: %v", params.WorkspaceID, reqErr)
		reqJSON = []byte("null")
	}
	respJSON, respErr := json.Marshal(params.ResponseBody)
	if respErr != nil {
		log.Printf("LogActivity: failed to marshal response_body for %s: %v", params.WorkspaceID, respErr)
		respJSON = []byte("null")
	}

	var reqStr, respStr, traceStr *string
	if params.RequestBody != nil {
		s := string(reqJSON)
		reqStr = &s
	}
	if params.ResponseBody != nil {
		s := string(respJSON)
		respStr = &s
	}
	if len(params.ToolTrace) > 0 {
		s := string(params.ToolTrace)
		traceStr = &s
	}

	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO activity_logs (workspace_id, activity_type, source_id, target_id, method, summary, request_body, response_body, tool_trace, duration_ms, status, error_detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9::jsonb, $10, $11, $12)
	`, params.WorkspaceID, params.ActivityType, params.SourceID, params.TargetID,
		params.Method, params.Summary, reqStr, respStr, traceStr,
		params.DurationMs, params.Status, params.ErrorDetail)
	if err != nil {
		log.Printf("LogActivity insert error: %v", err)
		return
	}

	// Broadcast ACTIVITY_LOGGED event
	if broadcaster != nil {
		payload := map[string]interface{}{
			"activity_type": params.ActivityType,
			"method":        params.Method,
			"summary":       params.Summary,
			"status":        params.Status,
			"source_id":     params.SourceID,
			"target_id":     params.TargetID,
			"duration_ms":   params.DurationMs,
		}
		if len(params.ToolTrace) > 0 {
			payload["tool_trace"] = json.RawMessage(params.ToolTrace)
		}
		// Include request/response bodies in the live broadcast so the
		// canvas's Agent Comms panel can render the actual task text
		// and reply text immediately, instead of falling back to the
		// "Delegating to <peer>" boilerplate. Without this, the live
		// bubble was useless until a refresh re-fetched the activity
		// row from /workspaces/:id/activity (which DOES return these
		// columns from the DB). The workspace's report_activity helper
		// caps each side at sensible sizes (4096 chars for error_detail,
		// 256 for summary; request/response are bounded by the
		// runtime's own caps — typical delegate_task payload is a few
		// hundred chars to a few KB). json.RawMessage avoids a
		// re-marshal round-trip; reqJSON/respJSON were already encoded
		// for the DB insert above.
		if reqStr != nil {
			payload["request_body"] = json.RawMessage(reqJSON)
		}
		if respStr != nil {
			payload["response_body"] = json.RawMessage(respJSON)
		}
		broadcaster.BroadcastOnly(params.WorkspaceID, "ACTIVITY_LOGGED", payload)
	}
}

type ActivityParams struct {
	WorkspaceID  string
	ActivityType string // a2a_send, a2a_receive, task_update, agent_log, skill_promotion, error
	SourceID     *string
	TargetID     *string
	Method       *string
	Summary      *string
	RequestBody  interface{}
	ResponseBody interface{}
	ToolTrace    json.RawMessage // tools/commands the agent actually invoked
	DurationMs   *int
	Status       string // ok, error, timeout
	ErrorDetail  *string
}
