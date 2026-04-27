package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/gin-gonic/gin"
)

type ActivityHandler struct {
	broadcaster *events.Broadcaster
}

func NewActivityHandler(b *events.Broadcaster) *ActivityHandler {
	return &ActivityHandler{broadcaster: b}
}

// List handles GET /workspaces/:id/activity?type=&limit=
func (h *ActivityHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")
	activityType := c.Query("type")
	source := c.Query("source") // "canvas" = source_id IS NULL, "agent" = source_id IS NOT NULL
	limitStr := c.DefaultQuery("limit", "100")

	limit := 100
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
		limit = n
		if limit > 500 {
			limit = 500
		}
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

	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", argIdx)
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

	// Verify workspace exists
	var wsName string
	err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT name FROM workspaces WHERE id = $1 AND status != 'removed'`, workspaceID,
	).Scan(&wsName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	broadcastPayload := map[string]interface{}{
		"message":      body.Message,
		"workspace_id": workspaceID,
		"name":         wsName,
	}
	if len(body.Attachments) > 0 {
		broadcastPayload["attachments"] = body.Attachments
	}
	h.broadcaster.BroadcastOnly(workspaceID, "AGENT_MESSAGE", broadcastPayload)

	// Persist to activity_logs so the chat history loader restores this
	// message after a page reload. Pre-fix, send_message_to_user pushes
	// were broadcast-only — survived the WebSocket session but vanished
	// when the user refreshed because nothing wrote them to the DB.
	//
	// Shape chosen to match the existing loader query
	// (`type=a2a_receive&source=canvas`):
	//   - activity_type='a2a_receive' so it joins the same query path
	//   - source_id=NULL so the canvas-source filter accepts it
	//   - method='notify' to distinguish from real A2A receives in audits
	//   - request_body=NULL so the loader doesn't append a duplicate
	//     "user message" bubble for it
	//   - response_body={"result": "<text>"} matches extractResponseText's
	//     simplest branch ({result: string} → take verbatim)
	//
	// Errors are logged-only — broadcast already succeeded, the user
	// sees the message; persistence failure just means the message
	// won't survive reload (pre-fix behavior). Don't fail the whole
	// notify on a DB hiccup.
	// response_body shape — chosen to feed BOTH:
	//   - extractResponseText: looks at body.result (string) and returns it
	//   - extractFilesFromTask: looks at body.parts[] for kind=file
	// so a chat reload after a notify-with-attachments restores both
	// the text bubble AND the download chips.
	respPayload := map[string]interface{}{"result": body.Message}
	if len(body.Attachments) > 0 {
		fileParts := make([]map[string]interface{}, 0, len(body.Attachments))
		for _, a := range body.Attachments {
			fileMeta := map[string]interface{}{"uri": a.URI, "name": a.Name}
			if a.MimeType != "" {
				fileMeta["mimeType"] = a.MimeType
			}
			if a.Size > 0 {
				fileMeta["size"] = a.Size
			}
			fileParts = append(fileParts, map[string]interface{}{
				"kind": "file",
				"file": fileMeta,
			})
		}
		respPayload["parts"] = fileParts
	}
	respJSON, _ := json.Marshal(respPayload)
	preview := body.Message
	if len(preview) > 80 {
		preview = preview[:80] + "…"
	}
	if _, err := db.DB.ExecContext(c.Request.Context(), `
		INSERT INTO activity_logs (workspace_id, activity_type, method, summary, response_body, status)
		VALUES ($1, 'a2a_receive', 'notify', $2, $3::jsonb, 'ok')
	`, workspaceID, "Agent message: "+preview, string(respJSON)); err != nil {
		log.Printf("Notify: failed to persist message for %s: %v", workspaceID, err)
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
