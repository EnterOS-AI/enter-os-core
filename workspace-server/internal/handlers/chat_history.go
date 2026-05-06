package handlers

// chat_history.go — server-side rendering of activity_logs rows into the
// canonical ChatMessage shape (RFC #2945 PR-C, issue #3017).
//
// Replaces the canvas-side TS parsing in
// canvas/src/components/tabs/chat/historyHydration.ts +
// canvas/src/components/tabs/chat/message-parser.ts so:
//
//   - Single source of truth for A2A-envelope walking. A future API
//     consumer (mobile, third-party integration, RFC #2945 PR-D's
//     OSS MessageStore) consumes a typed surface instead of re-
//     implementing the same shape walk.
//
//   - Wire-format evolution (a2a-sdk v0 → v1 protobuf flat shape) is
//     handled in one place. Today the TS parser handles both shapes;
//     this Go parser mirrors that contract exactly.
//
//   - PR-D unblocked: MessageStore returns []ChatMessage typed values,
//     not raw activity_logs rows. The interface is meaningless if
//     parsing still has to happen client-side.
//
// Endpoint: GET /workspaces/:id/chat-history?limit=N&before_ts=T
//
// Auth: same wsAuth chain as /workspaces/:id/activity (tenant
// ADMIN_TOKEN + X-Molecule-Org-Id header). No new trust boundary.
//
// Behavioral contract: every test case in
// canvas/src/components/tabs/chat/__tests__/historyHydration.test.ts
// (11 cases) has a Go-side parity test in chat_history_test.go.
// Mutation-tested by reverting individual branches and confirming
// the corresponding Go test fires red.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ChatMessage is the canonical shape returned by GET /chat-history.
// Mirrors canvas/src/components/tabs/chat/types.ts:ChatMessage so
// the canvas can render it without per-row mapping.
//
// NOTE: id is server-generated (UUID v4) per row pair — clients should
// NOT depend on these ids being stable across requests since the
// activity_log row itself doesn't carry message-shaped ids. Use
// (timestamp, role, content) for cross-request deduping; the id is
// just a React key.
type ChatMessage struct {
	ID          string           `json:"id"`
	Role        string           `json:"role"` // "user" | "agent" | "system"
	Content     string           `json:"content"`
	Attachments []ChatAttachment `json:"attachments,omitempty"`
	Timestamp   string           `json:"timestamp"` // RFC3339 — pinned to row.created_at
}

// ChatAttachment mirrors canvas's ChatAttachment / ParsedFilePart.
type ChatAttachment struct {
	Name     string  `json:"name"`
	URI      string  `json:"uri"`
	MimeType string  `json:"mimeType,omitempty"`
	Size     *int64  `json:"size,omitempty"`
}

// ChatHistoryResponse is the wire shape for GET /chat-history.
type ChatHistoryResponse struct {
	Messages   []ChatMessage `json:"messages"`
	ReachedEnd bool          `json:"reached_end"`
}

// ChatHistoryHandler exposes the typed chat-history endpoint. It does
// not need a broadcaster — read-only.
type ChatHistoryHandler struct{}

// NewChatHistoryHandler returns a fresh handler. Stateless on purpose:
// no caching, no per-request handler state. The DB query is the
// expensive part; cache control is handled at HTTP layer.
func NewChatHistoryHandler() *ChatHistoryHandler {
	return &ChatHistoryHandler{}
}

// internalSelfPrefixes — message texts that should be filtered out of
// chat history because they're internal self-triggers (heartbeats,
// scheduled-task self-fire, delegation-result self-notify) rather than
// user-typed messages. Mirrors canvas's isInternalSelfMessage. Centring
// here means a future internal-trigger pattern only needs to be added
// in one place, not in every consumer.
var internalSelfPrefixes = []string{
	"Delegation results are ready",
}

// isInternalSelfMessage reports whether text starts with any registered
// internal-self prefix. Empty text returns false (only filter on
// matched prefixes — empty/missing text is a legitimate
// attachments-only bubble).
func isInternalSelfMessage(text string) bool {
	if text == "" {
		return false
	}
	for _, prefix := range internalSelfPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// List handles GET /workspaces/:id/chat-history?limit=N&before_ts=T.
//
// Query parameters mirror /activity for caller convenience:
//
//   - limit (default 100, max 1000) — page size, newest-first from the
//     server's POV. Caller reverses for chronological display.
//   - before_ts (RFC3339, optional) — paginate by walking strictly
//     older than this timestamp. Identical semantics to /activity's
//     before_ts: matches what canvas uses for lazy-loading older
//     batches.
//
// The handler scopes to activity_type='a2a_receive' AND source_id IS
// NULL (canvas-source rows only) — the same filter canvas applies via
// `?type=a2a_receive&source=canvas`. Centralizing here means a future
// caller (mobile, public API) doesn't need to know the filter.
func (h *ChatHistoryHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")
	if _, err := uuid.Parse(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id must be a UUID"})
		return
	}

	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}

	var beforeTS time.Time
	usingBeforeTS := false
	if v := c.Query("before_ts"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "before_ts must be an RFC3339 timestamp (e.g. 2026-05-01T00:00:00Z)",
			})
			return
		}
		beforeTS = t
		usingBeforeTS = true
	}

	// Newest-first ordering matches /activity. Caller reverses for
	// chronological display. Same semantics across both endpoints
	// keeps the canvas's lazy-history pagination logic stable.
	rows, err := h.queryActivityRows(c.Request.Context(), workspaceID, limit, usingBeforeTS, beforeTS)
	if err != nil {
		// Errors here are infra (DB unreachable) — surface as 502 so
		// the canvas can retry vs. treating as "no rows".
		c.JSON(http.StatusBadGateway, gin.H{"error": "chat history unavailable"})
		return
	}
	defer rows.Close()

	var messages []ChatMessage
	rowCount := 0
	for rows.Next() {
		var (
			createdAt   time.Time
			status      string
			rawRequest  sql.NullString
			rawResponse sql.NullString
		)
		if err := rows.Scan(&createdAt, &status, &rawRequest, &rawResponse); err != nil {
			continue
		}
		rowCount++
		var requestBody, responseBody json.RawMessage
		if rawRequest.Valid {
			requestBody = json.RawMessage(rawRequest.String)
		}
		if rawResponse.Valid {
			responseBody = json.RawMessage(rawResponse.String)
		}
		messages = append(messages, activityRowToChatMessages(createdAt, status, requestBody, responseBody, isInternalSelfMessage)...)
	}

	c.JSON(http.StatusOK, ChatHistoryResponse{
		Messages:   messages,
		ReachedEnd: rowCount < limit,
	})
}

// queryActivityRows pulls the raw a2a_receive rows for a workspace.
// Split out so unit tests can mock the DB layer without spinning a
// full request context. Canvas-source rows only (source_id IS NULL).
func (h *ChatHistoryHandler) queryActivityRows(ctx interface {
	Done() <-chan struct{}
	Err() error
	Deadline() (time.Time, bool)
	Value(any) any
}, workspaceID string, limit int, usingBeforeTS bool, beforeTS time.Time) (*sql.Rows, error) {
	if usingBeforeTS {
		return db.DB.QueryContext(ctx, `
			SELECT created_at, status, request_body::text, response_body::text
			FROM activity_logs
			WHERE workspace_id = $1
			  AND activity_type = 'a2a_receive'
			  AND source_id IS NULL
			  AND created_at < $2
			ORDER BY created_at DESC
			LIMIT $3
		`, workspaceID, beforeTS, limit)
	}
	return db.DB.QueryContext(ctx, `
		SELECT created_at, status, request_body::text, response_body::text
		FROM activity_logs
		WHERE workspace_id = $1
		  AND activity_type = 'a2a_receive'
		  AND source_id IS NULL
		ORDER BY created_at DESC
		LIMIT $2
	`, workspaceID, limit)
}

// activityRowToChatMessages converts ONE activity_logs row into 0-2
// ChatMessages. Direct port of canvas's activityRowToMessages.
//
//   - Up to 1 user-side bubble from request_body, unless internal-self.
//   - Up to 1 agent-side bubble from response_body. Role is "system"
//     when status='error' OR text starts with "agent error" (case-
//     insensitive — matches canvas predicate exactly).
//
// Both bubbles MUST adopt row.created_at as their timestamp. The
// canvas hydration regression that motivated extracting the helper
// (every reload re-stamping bubbles to render-time) is regression-
// covered in chat_history_test.go.
//
// Defensive: any malformed JSON is silently dropped (text becomes "",
// attachments []) — chat falls through to text-only rather than
// surfacing a 500.
func activityRowToChatMessages(
	createdAt time.Time,
	status string,
	requestBody json.RawMessage,
	responseBody json.RawMessage,
	internalSelf func(string) bool,
) []ChatMessage {
	var out []ChatMessage
	timestamp := createdAt.UTC().Format(time.RFC3339Nano)

	// USER side — extract from request_body.params.message
	userText := extractRequestText(requestBody)
	userAttachments := extractFilesFromUserMessage(requestBody)
	if !internalSelf(userText) && (userText != "" || len(userAttachments) > 0) {
		out = append(out, ChatMessage{
			ID:          newMessageID(),
			Role:        "user",
			Content:     userText,
			Attachments: userAttachments,
			Timestamp:   timestamp,
		})
	}

	// AGENT side — extract from response_body
	if len(responseBody) > 0 {
		agentText := extractChatResponseText(responseBody)
		agentAttachments := extractFilesFromResponse(responseBody)
		if agentText != "" || len(agentAttachments) > 0 {
			role := "agent"
			if status == "error" || strings.HasPrefix(strings.ToLower(agentText), "agent error") {
				role = "system"
			}
			out = append(out, ChatMessage{
				ID:          newMessageID(),
				Role:        role,
				Content:     agentText,
				Attachments: agentAttachments,
				Timestamp:   timestamp,
			})
		}
	}

	return out
}

// extractRequestText pulls the user's typed text from the canonical
// A2A request envelope. Returns "" on any malformed shape; callers
// pair this with extractFilesFromUserMessage to catch attachments-
// only bubbles.
//
//   request_body = {"params": {"message": {"parts": [{"kind":"text", "text":"..."}, ...]}}}
//
// Mirrors canvas's extractRequestText. Currently returns ONLY parts[0]
// to match canvas exactly; multi-text-part user messages would
// require both parsers to evolve in lockstep (track via PR-C-2).
func extractRequestText(body json.RawMessage) string {
	if len(body) == 0 {
		return ""
	}
	var env struct {
		Params struct {
			Message struct {
				Parts []map[string]any `json:"parts"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	for _, p := range env.Params.Message.Parts {
		if t, ok := p["text"].(string); ok && t != "" {
			return t
		}
	}
	return ""
}

// extractFilesFromUserMessage walks the same request_body envelope as
// extractRequestText and collects file parts.
func extractFilesFromUserMessage(body json.RawMessage) []ChatAttachment {
	if len(body) == 0 {
		return nil
	}
	var env struct {
		Params struct {
			Message json.RawMessage `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	if len(env.Params.Message) == 0 {
		return nil
	}
	return extractFilesFromTask(env.Params.Message)
}

// extractChatResponseText collects text from any of the response shapes
// canvas's extractChatResponseText handles, joining with "\n":
//
//   - {"result": "<text>"}                              — string
//   - {"result": {"parts": [{"kind":"text", "text":""}]}}  — A2A JSON-RPC
//   - {"parts": [{"root": {"text": "..."}}]}            — older nested shape
//   - {"result": {"artifacts": [{"parts": [...]}]}}     — task shape
//   - {"task": "<text>"}                                — fallback
//
// Why collect rather than first-source-wins: claude-code emits multiple
// text parts; hermes emits summary-in-parts + details-in-artifacts. The
// pre-collect "first wins" silently truncated 15k-char briefs to their
// leading line and dropped artifact details. Matches canvas behavior
// exactly.
func extractChatResponseText(body json.RawMessage) string {
	if len(body) == 0 {
		return ""
	}

	// {"result": "string"}
	var asString struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(body, &asString); err == nil && asString.Result != "" {
		return asString.Result
	}

	// {"result": {object}} — try the structured shapes
	var asObject struct {
		Result    json.RawMessage `json:"result"`
		Task      string          `json:"task"`
	}
	if err := json.Unmarshal(body, &asObject); err != nil {
		return ""
	}

	var collected []string

	if len(asObject.Result) > 0 {
		var resultObj struct {
			Parts     []map[string]any  `json:"parts"`
			Artifacts []json.RawMessage `json:"artifacts"`
		}
		if err := json.Unmarshal(asObject.Result, &resultObj); err == nil {
			// A2A JSON-RPC: parts[].text
			if t := joinTextParts(resultObj.Parts); t != "" {
				collected = append(collected, t)
			}
			// Older nested: parts[].root.text
			var rootTexts []string
			for _, p := range resultObj.Parts {
				if root, ok := p["root"].(map[string]any); ok {
					if t, ok := root["text"].(string); ok && t != "" {
						rootTexts = append(rootTexts, t)
					}
				}
			}
			if len(rootTexts) > 0 {
				collected = append(collected, strings.Join(rootTexts, "\n"))
			}
			// Task shape: artifacts[].parts[].text
			for _, raw := range resultObj.Artifacts {
				var art struct {
					Parts []map[string]any `json:"parts"`
				}
				if err := json.Unmarshal(raw, &art); err == nil {
					if t := joinTextParts(art.Parts); t != "" {
						collected = append(collected, t)
					}
				}
			}
		}
	}

	if len(collected) > 0 {
		return strings.Join(collected, "\n")
	}

	if asObject.Task != "" {
		return asObject.Task
	}
	return ""
}

// joinTextParts returns a "\n"-joined concatenation of every text part
// in parts[]. Empty if no text parts. Matches canvas extractTextsFromParts.
func joinTextParts(parts []map[string]any) string {
	var texts []string
	for _, p := range parts {
		// Accept both "type":"text" (older) and "kind":"text" (current).
		isText := false
		if k, ok := p["kind"].(string); ok && k == "text" {
			isText = true
		}
		if t, ok := p["type"].(string); ok && t == "text" {
			isText = true
		}
		if !isText {
			continue
		}
		if t, ok := p["text"].(string); ok && t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n")
}

// extractFilesFromResponse collects file parts from the response_body
// across the same shape variants as extractChatResponseText. Mirrors
// canvas extractFilesFromTask, except the canvas function takes "the
// task object" while this takes the wire-level response_body and
// dispatches:
//
//   - {"result": {object}}   → unwrap result, walk parts/artifacts
//   - {"result": "<text>", "parts": [...]}  → notify shape, walk top-level parts
//   - {"message": {"parts": [...]}}  → some A2A servers wrap as a message
func extractFilesFromResponse(body json.RawMessage) []ChatAttachment {
	if len(body) == 0 {
		return nil
	}
	// Determine which container to feed extractFilesFromTask:
	//   - if result is an object, feed the result object
	//   - else feed the top-level body (notify shape with parts at root)
	var probe struct {
		Result json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(body, &probe)

	feed := body
	if len(probe.Result) > 0 {
		// Is result an object? (vs a string)
		trimmed := bytes_trim_space(probe.Result)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			feed = probe.Result
		}
	}
	return extractFilesFromTask(feed)
}

// extractFilesFromTask walks parts[] + artifacts[].parts[] + status.message.parts[]
// + message.parts[] and pulls out file parts. Mirrors canvas's
// extractFilesFromTask exactly — same two wire shapes (v0 hot path,
// v1 protobuf flat shape).
//
// Defensive: any error inside the walk is recovered and partial
// results returned. A malformed shape should never fail the whole
// chat reload — degraded UX is better than 500.
func extractFilesFromTask(taskJSON json.RawMessage) []ChatAttachment {
	if len(taskJSON) == 0 {
		return nil
	}
	var task struct {
		Parts     []map[string]any  `json:"parts"`
		Artifacts []json.RawMessage `json:"artifacts"`
		Status    json.RawMessage   `json:"status"`
		Message   json.RawMessage   `json:"message"`
	}
	if err := json.Unmarshal(taskJSON, &task); err != nil {
		return nil
	}
	var out []ChatAttachment
	out = appendFilesFromParts(out, task.Parts)
	for _, raw := range task.Artifacts {
		var art struct {
			Parts []map[string]any `json:"parts"`
		}
		if err := json.Unmarshal(raw, &art); err == nil {
			out = appendFilesFromParts(out, art.Parts)
		}
	}
	if len(task.Status) > 0 {
		var st struct {
			Message struct {
				Parts []map[string]any `json:"parts"`
			} `json:"message"`
		}
		if err := json.Unmarshal(task.Status, &st); err == nil {
			out = appendFilesFromParts(out, st.Message.Parts)
		}
	}
	if len(task.Message) > 0 {
		var msg struct {
			Parts []map[string]any `json:"parts"`
		}
		if err := json.Unmarshal(task.Message, &msg); err == nil {
			out = appendFilesFromParts(out, msg.Parts)
		}
	}
	return out
}

// appendFilesFromParts handles the v0 hot path (kind/type=file with
// nested file{}) and the v1 flat path (url+filename+mediaType).
func appendFilesFromParts(out []ChatAttachment, parts []map[string]any) []ChatAttachment {
	for _, raw := range parts {
		v0 := false
		if k, ok := raw["kind"].(string); ok && k == "file" {
			v0 = true
		}
		if t, ok := raw["type"].(string); ok && t == "file" {
			v0 = true
		}
		v1URL, _ := raw["url"].(string)

		if !v0 && v1URL == "" {
			continue
		}

		var att ChatAttachment
		if v0 {
			file, _ := raw["file"].(map[string]any)
			if file == nil {
				file = raw // some emitters flatten; defensive
			}
			uri, _ := file["uri"].(string)
			if uri == "" {
				continue
			}
			att.URI = uri
			if name, _ := file["name"].(string); name != "" {
				att.Name = name
			} else {
				att.Name = basename(uri)
			}
			if mt, ok := file["mimeType"].(string); ok {
				att.MimeType = mt
			}
			if sz, ok := numericSize(file["size"]); ok {
				att.Size = &sz
			}
		} else {
			att.URI = v1URL
			if name, _ := raw["filename"].(string); name != "" {
				att.Name = name
			} else {
				att.Name = basename(v1URL)
			}
			if mt, ok := raw["mediaType"].(string); ok {
				att.MimeType = mt
			}
		}
		out = append(out, att)
	}
	return out
}

// numericSize coerces JSON's number type (always float64 in
// json.Unmarshal of map[string]any) to int64 for the Size field.
// Returns (0, false) for non-numeric or absent values.
func numericSize(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// basename strips scheme + path components, returning the trailing
// segment (or "file" if empty). Mirrors canvas basename helper.
func basename(uri string) string {
	cleaned := strings.TrimPrefix(uri, "workspace:")
	cleaned = strings.TrimPrefix(cleaned, "https://")
	cleaned = strings.TrimPrefix(cleaned, "http://")
	if cleaned == "" {
		return "file"
	}
	return path.Base(cleaned)
}

// bytes_trim_space — minimal whitespace stripper for json.RawMessage
// peeking. Avoids importing bytes for one tiny helper. Internal-only.
func bytes_trim_space(b json.RawMessage) json.RawMessage {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// newMessageID generates a fresh UUID per ChatMessage. Server-minted
// because activity_logs rows don't carry message-shaped ids, and the
// canvas only needs a React-key-stable id (it dedupes by content+role+
// timestamp window, not by id).
func newMessageID() string {
	return uuid.New().String()
}

// ensureNoUnusedImports avoids lint complaining about `errors` if a
// future refactor removes the only consumer. errors is reserved for
// the inevitable wrap-aware DB-error handling once we add a
// distinguishable "DB outage vs no rows" path.
var _ = errors.Is
