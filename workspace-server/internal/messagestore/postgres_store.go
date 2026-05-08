package messagestore

// postgres_store.go — default MessageStore impl that wraps today's
// activity_logs query + the A2A-envelope parser ported in PR-C.
//
// Behavior is byte-identical to the pre-PR-D ChatHistoryHandler:
// same SQL, same role-decision rules, same v0/v1 wire-shape support.
// The only structural change is that the handler now depends on an
// interface; this file is what was the pre-PR-D handler internals.
//
// This is the baseline impl OSS operators compare against when
// writing alternative stores. Read it as the contract spec.

import (
	"context"
	"database/sql"
	"encoding/json"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PostgresMessageStore is the platform-default impl. It queries the
// activity_logs table directly and parses request_body / response_body
// JSONB columns into ChatMessage values.
type PostgresMessageStore struct {
	db *sql.DB
}

// NewPostgresMessageStore wraps a *sql.DB. The store does not own the
// pool — closing it is the caller's responsibility.
func NewPostgresMessageStore(db *sql.DB) *PostgresMessageStore {
	return &PostgresMessageStore{db: db}
}

// internalSelfPrefixes — message texts that should be filtered from
// chat history because they're internal self-triggers (heartbeats,
// scheduled-task self-fire, delegation-result self-notify), not
// user-typed messages. Mirrors canvas isInternalSelfMessage.
//
// Centralizing here means a future internal-trigger pattern is added
// in one place; alternative impls of MessageStore are expected to
// apply the same filter (or override deliberately).
var internalSelfPrefixes = []string{
	"Delegation results are ready",
}

// IsInternalSelfMessage reports whether text starts with any registered
// internal-self prefix. Empty text returns false (legitimate
// attachments-only bubble). Exported for impls that want to share the
// same predicate.
func IsInternalSelfMessage(text string) bool {
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

// List implements MessageStore. Newest-first, optionally paged by
// BeforeTS. Filters to a2a_receive activity rows from the canvas
// (source_id IS NULL) — same scope canvas applies via
// /activity?source=canvas, centralized so future API consumers don't
// need to know it.
func (s *PostgresMessageStore) List(ctx context.Context, workspaceID string, opts ListOptions) ([]ChatMessage, bool, error) {
	if opts.Limit <= 0 {
		// Caller bug. Programmers learn quickly when the store
		// fails fast on bad opts; a silent clamp would hide the bug.
		return nil, true, errInvalidLimit
	}

	rows, err := s.queryActivityRows(ctx, workspaceID, opts)
	if err != nil {
		return nil, false, err
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
			// Skip malformed row, continue. The error is logged at
			// the caller (handler) layer; an isolated bad row should
			// not abort the whole page.
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
		messages = append(messages, activityRowToChatMessages(createdAt, status, requestBody, responseBody, IsInternalSelfMessage)...)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	// Wire order: oldest-first within the page so canvas (and any
	// future client) can render chronologically without per-pair
	// reordering. The SQL is `ORDER BY created_at DESC LIMIT N` for
	// pagination correctness, and activityRowToChatMessages emits
	// [user, agent] within a row — so a naive client-side flat-reverse
	// would swap the pair (agent before user at the same timestamp).
	// Reversing ROW-AWARE here keeps the wire shape display-ready.
	//
	// Algorithm: group consecutive same-timestamp messages into row
	// chunks (1-2 messages each), reverse the chunk order, flatten.
	// Within-row [user, agent] order is preserved. Single-message
	// rows (no agent reply yet, or attachments-only) collapse to
	// 1-element chunks and still reverse correctly.
	messages = reverseRowChunks(messages)

	reachedEnd := rowCount < opts.Limit
	return messages, reachedEnd, nil
}

// reverseRowChunks groups msgs by adjacent same-Timestamp runs and
// reverses the run order, preserving within-run order. Pairs of
// (user, agent) emitted by activityRowToChatMessages share a
// timestamp, so this keeps each pair internally ordered while
// reversing the row sequence.
func reverseRowChunks(msgs []ChatMessage) []ChatMessage {
	if len(msgs) == 0 {
		return msgs
	}
	var chunks [][]ChatMessage
	cur := []ChatMessage{msgs[0]}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Timestamp == cur[len(cur)-1].Timestamp {
			cur = append(cur, msgs[i])
		} else {
			chunks = append(chunks, cur)
			cur = []ChatMessage{msgs[i]}
		}
	}
	chunks = append(chunks, cur)
	for i, j := 0, len(chunks)-1; i < j; i, j = i+1, j-1 {
		chunks[i], chunks[j] = chunks[j], chunks[i]
	}
	out := make([]ChatMessage, 0, len(msgs))
	for _, chunk := range chunks {
		out = append(out, chunk...)
	}
	return out
}

// queryActivityRows is split from List so unit tests can exercise the
// parser without spinning a real DB. Internal — alternative impls
// shouldn't depend on the SQL shape.
func (s *PostgresMessageStore) queryActivityRows(ctx context.Context, workspaceID string, opts ListOptions) (*sql.Rows, error) {
	if opts.HasBefore {
		return s.db.QueryContext(ctx, `
			SELECT created_at, status, request_body::text, response_body::text
			FROM activity_logs
			WHERE workspace_id = $1
			  AND activity_type = 'a2a_receive'
			  AND source_id IS NULL
			  AND created_at < $2
			ORDER BY created_at DESC
			LIMIT $3
		`, workspaceID, opts.BeforeTS, opts.Limit)
	}
	return s.db.QueryContext(ctx, `
		SELECT created_at, status, request_body::text, response_body::text
		FROM activity_logs
		WHERE workspace_id = $1
		  AND activity_type = 'a2a_receive'
		  AND source_id IS NULL
		ORDER BY created_at DESC
		LIMIT $2
	`, workspaceID, opts.Limit)
}

// errInvalidLimit is returned by List when opts.Limit ≤ 0.
type sentinelError string

func (e sentinelError) Error() string { return string(e) }

const errInvalidLimit sentinelError = "messagestore: List opts.Limit must be > 0"

// activityRowToChatMessages converts ONE activity_logs row into 0-2
// ChatMessages. Direct port of canvas activityRowToMessages.
//
//   - Up to 1 user-side bubble from request_body, unless internal-self.
//   - Up to 1 agent-side bubble from response_body. Role is "system"
//     when status='error' OR text starts with "agent error" (case-
//     insensitive — matches canvas predicate exactly).
//
// Both bubbles MUST adopt row.created_at as their timestamp. This
// pins the regression cover for the 2026-04-25 bubble-collapse bug.
func activityRowToChatMessages(
	createdAt time.Time,
	status string,
	requestBody json.RawMessage,
	responseBody json.RawMessage,
	internalSelf func(string) bool,
) []ChatMessage {
	var out []ChatMessage
	timestamp := createdAt.UTC().Format(time.RFC3339Nano)

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

// extractRequestText pulls the user's typed text from
// request_body.params.message.parts[0].text. Returns "" on any
// malformed shape; callers pair with extractFilesFromUserMessage to
// catch attachments-only bubbles.
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

// extractChatResponseText collects text from any of the response
// shapes canvas extractResponseText handles, joining with "\n":
//
//   - {"result": "<text>"}
//   - {"result": {"parts": [{"kind":"text","text":""}]}}
//   - {"parts": [{"root": {"text": "..."}}]}             (older nested)
//   - {"result": {"artifacts": [{"parts": [...]}]}}      (task shape)
//   - {"task": "<text>"}                                 (fallback)
//
// Why collect rather than first-source-wins: claude-code emits
// multiple text parts; hermes emits summary-in-parts +
// details-in-artifacts. The pre-collect first-wins silently
// truncated 15k-char briefs and dropped artifact details.
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
		Result json.RawMessage `json:"result"`
		Task   string          `json:"task"`
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
			if t := joinTextParts(resultObj.Parts); t != "" {
				collected = append(collected, t)
			}
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

func joinTextParts(parts []map[string]any) string {
	var texts []string
	for _, p := range parts {
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

func extractFilesFromResponse(body json.RawMessage) []ChatAttachment {
	if len(body) == 0 {
		return nil
	}
	var probe struct {
		Result json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(body, &probe)
	feed := body
	if len(probe.Result) > 0 {
		trimmed := bytesTrimSpace(probe.Result)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			feed = probe.Result
		}
	}
	return extractFilesFromTask(feed)
}

// extractFilesFromTask walks parts[] + artifacts[].parts[] +
// status.message.parts[] + message.parts[]. Mirrors canvas
// extractFilesFromTask exactly — same v0 hot path + v1 protobuf
// flat shape.
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
				file = raw
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

func basename(uri string) string {
	cleaned := strings.TrimPrefix(uri, "workspace:")
	cleaned = strings.TrimPrefix(cleaned, "https://")
	cleaned = strings.TrimPrefix(cleaned, "http://")
	if cleaned == "" {
		return "file"
	}
	return path.Base(cleaned)
}

func bytesTrimSpace(b json.RawMessage) json.RawMessage {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func newMessageID() string {
	return uuid.New().String()
}

// Compile-time assertion: PostgresMessageStore satisfies MessageStore.
// Catches any future drift between interface and impl at build time.
var _ MessageStore = (*PostgresMessageStore)(nil)
