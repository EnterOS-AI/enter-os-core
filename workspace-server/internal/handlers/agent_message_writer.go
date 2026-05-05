package handlers

// AgentMessageWriter is the SSOT for "agent → user" message delivery in the
// workspace-server. Every chat-bearing path that surfaces a message to the
// canvas — HTTP /notify (Notify handler), MCP tools/call
// send_message_to_user (toolSendMessageToUser), any future channel — MUST
// route through this writer rather than re-implement the broadcast +
// persist contract inline.
//
// Why: pre-consolidation, two handlers duplicated the same "broadcast then
// INSERT activity_logs" sequence. The reno-stars production data-loss
// incident (2026-05-05, RFC #2945, PR #2944) was the symptom — the
// persistence half landed for /notify but lagged for the MCP bridge by
// months, silently dropping every long-form external-agent message until
// reload. The AST gate from #2944 catches drift; this writer eliminates
// the *possibility* of drift by giving both call sites a single
// well-tested function to call.
//
// Contract:
//   1. Look up the workspace by id; ErrWorkspaceNotFound on miss so the
//      caller can return 404 with a clean message.
//   2. Broadcast a WS AGENT_MESSAGE event with {message, workspace_id,
//      name, attachments?}.
//   3. INSERT a row into activity_logs:
//        type='a2a_receive', method='notify', source_id NULL,
//        response_body={"result": message[, "parts": [file kind...]]},
//        status='ok'
//      Best-effort — INSERT failure logs only, returns nil so the broadcast
//      success isn't undone on the caller side.
//   4. Returns nil on success.
//
// The shape (especially the JSON response_body) is the wire contract the
// canvas's chat-history hydrator (canvas/src/.../historyHydration.ts)
// reads. Drift here silently breaks chat replay across all consumers, so
// changes to the JSON shape MUST be cross-verified against the hydrator
// in the same PR.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"unicode/utf8"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
)

// ErrWorkspaceNotFound is returned by AgentMessageWriter.Send when the
// workspace lookup turns up nothing (or the workspace is in
// status='removed'). Callers translate to HTTP 404 / JSON-RPC error /
// whatever surface they expose. Real DB errors (connection drop, query
// timeout) surface as wrapped errors and should be treated as 503.
var ErrWorkspaceNotFound = errors.New("agent_message: workspace not found")

// truncatePreviewRunes returns at most maxRunes runes of s, plus an ellipsis
// when truncated. Operates on the rune (codepoint) boundary instead of
// byte indices — the previous byte-slice version produced invalid UTF-8
// when maxRunes landed mid-codepoint (CJK, emoji, accented characters
// in agent-authored chat messages), and Postgres JSONB rejects invalid
// UTF-8, dropping the activity_log INSERT silently. The persistence
// failure log fires but the message vanishes from chat history — the
// exact regression class the SSOT consolidation was built to prevent.
//
// maxRunes is in runes, not bytes — `truncatePreviewRunes("你好", 1)` returns
// `"你…"`, not `"\xe4…"`. Set the cap on a UI-friendly basis (visible
// character count, not stored byte count); 80 runes covers the
// activity_logs.summary column comfortably.
func truncatePreviewRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	// Walk runes until we've consumed maxRunes; cut at that byte index.
	count := 0
	cut := len(s)
	for i := range s {
		if count == maxRunes {
			cut = i
			break
		}
		count++
	}
	return s[:cut] + "…"
}

// AgentMessageAttachment is one file attached to an agent → user
// message. Identical to handlers.NotifyAttachment in field set; kept
// distinct so the writer's API doesn't import a handler type with HTTP
// binding tags.
type AgentMessageAttachment struct {
	URI      string
	Name     string
	MimeType string
	Size     int64
}

// AgentMessageWriter persists + broadcasts agent → user messages. Construct
// once per process via NewAgentMessageWriter; pass the same instance to
// every handler that delivers chat (Notify, toolSendMessageToUser, etc.).
//
// Takes events.EventEmitter (not the *Broadcaster concrete type) so tests
// can substitute a fake emitter and producers in other packages can wrap
// the real broadcaster behind their own metrics / retries without leaking
// the concrete dependency.
type AgentMessageWriter struct {
	db          *sql.DB
	broadcaster events.EventEmitter
}

// NewAgentMessageWriter binds the writer to the platform's DB pool +
// WebSocket broadcaster.
func NewAgentMessageWriter(db *sql.DB, broadcaster events.EventEmitter) *AgentMessageWriter {
	return &AgentMessageWriter{db: db, broadcaster: broadcaster}
}

// Send delivers a single agent → user message. Look up + broadcast +
// persist in that order; ErrWorkspaceNotFound short-circuits before any
// broadcast or DB write so callers can 404 cleanly.
//
// Returns nil on success — including on DB-INSERT failure (the broadcast
// already returned successfully and the user has seen the message; the
// persistence-failure mode is logged at WARN but the caller's response
// stays 200 so the agent doesn't retry and double-broadcast).
func (w *AgentMessageWriter) Send(
	ctx context.Context,
	workspaceID, message string,
	attachments []AgentMessageAttachment,
) error {
	// 1. Workspace lookup. status='removed' filter is the same shape /notify
	//    used pre-consolidation; deleted workspaces don't get notifications.
	//
	// Distinguish sql.ErrNoRows ("workspace genuinely not present" — caller
	// should 404) from real DB errors (connection drop, statement timeout,
	// pool exhaustion — caller should 503). Pre-fix this branch returned
	// ErrWorkspaceNotFound for any error, so during a DB outage every
	// notify call surfaced as "workspace not found" and masked real
	// incidents in the alert path.
	var wsName string
	err := w.db.QueryRowContext(ctx,
		`SELECT name FROM workspaces WHERE id = $1 AND status != 'removed'`,
		workspaceID,
	).Scan(&wsName)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWorkspaceNotFound
	}
	if err != nil {
		return fmt.Errorf("agent_message: workspace lookup: %w", err)
	}

	// 2. Build broadcast payload + WS-emit. Same shape that ChatTab's
	//    AGENT_MESSAGE handler in canvas/src/store/canvas-events.ts has
	//    consumed since the canvas chat shipped — drift here would orphan
	//    every live chat panel.
	broadcastPayload := map[string]interface{}{
		"message":      message,
		"workspace_id": workspaceID,
		"name":         wsName,
	}
	if len(attachments) > 0 {
		broadcastPayload["attachments"] = attachments
	}
	w.broadcaster.BroadcastOnly(workspaceID, string(events.EventAgentMessage), broadcastPayload)

	// 3. Persist for chat-history hydration. response_body shape MUST stay
	//    in sync with extractResponseText + extractFilesFromTask in
	//    canvas/src/components/tabs/chat/historyHydration.ts:
	//      - extractResponseText reads body.result (string) → renders text
	//      - extractFilesFromTask reads body.parts[] (kind=file) → renders chips
	respPayload := map[string]interface{}{"result": message}
	if len(attachments) > 0 {
		fileParts := make([]map[string]interface{}, 0, len(attachments))
		for _, a := range attachments {
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
	preview := truncatePreviewRunes(message, 80)
	if _, err := w.db.ExecContext(ctx, `
		INSERT INTO activity_logs (workspace_id, activity_type, method, summary, response_body, status)
		VALUES ($1, 'a2a_receive', 'notify', $2, $3::jsonb, 'ok')
	`, workspaceID, "Agent message: "+preview, string(respJSON)); err != nil {
		// Best-effort: the broadcast already returned ok and the user
		// has seen the message. Logging a structured line lets operators
		// notice persistence-failure rates spike if the DB is unhealthy,
		// without breaking the tool response or causing the agent to
		// retry-and-double-broadcast.
		log.Printf("agent_message: failed to persist for %s: %v", workspaceID, err)
	}

	return nil
}
