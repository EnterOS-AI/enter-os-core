package handlers

// mock_runtime.go — "mock" runtime: a virtual workspace that has no
// container, no EC2, no LLM, just hardcoded canned A2A replies. Built
// for the funding-demo "200-workspace mock org" so hongming can show
// investors a CEO/VPs/Managers/ICs hierarchy at scale without burning
// 200 EC2 instances or 200 Anthropic keys.
//
// Wire model:
//   - org template declares `runtime: mock` on every workspace
//   - createWorkspaceTree skips provisioning, sets status='online'
//     directly (mirrors the `external` short-circuit, minus the URL +
//     awaiting_agent dance)
//   - proxyA2ARequest short-circuits on a mock-runtime target and
//     returns a canned JSON-RPC reply; never calls resolveAgentURL,
//     never opens an HTTP connection, never touches Docker/EC2
//
// The reply is JSON-RPC 2.0 + a2a-sdk v0.3 shape so the canvas's
// extractAgentText / extractTextsFromParts read it without any
// special-casing. We rotate over a small variant pool so a screen
// full of replies doesn't all read identical — gives the demo a bit
// of life without pretending to be a real agent.

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// MockRuntimeName is the canonical runtime string a workspace row
// carries to opt into the canned-reply short-circuit. Kept as a const
// so the proxy's runtime-check + the org-import skip-block reference
// the same literal.
const MockRuntimeName = "mock"

// mockReplyVariants is the pool of canned strings the mock runtime
// rotates through. Picked to read like a busy-but-short reply from a
// real human in a hierarchy — a CEO would NOT respond with "On it!",
// but for the demo every node is shown to be reachable, so we lean
// into the variety. Variant selection is deterministic per
// (workspaceID, request-id) pair so a screen recording replays the
// same reply for the same input.
var mockReplyVariants = []string{
	"On it!",
	"Got it, on it now.",
	"On it, boss.",
	"Working on it.",
	"Acknowledged — on it.",
	"On it, will report back.",
	"Roger that, on it.",
	"Copy that. On it.",
	"On it — ETA shortly.",
	"On it. Standby for update.",
}

// pickMockReply returns a canned reply for the given workspaceID +
// requestID. Deterministic so the same (workspace, message-id) pair
// always picks the same variant — useful for screen recordings and
// flake-free e2e snapshots. Falls back to variant[0] if the inputs
// are empty.
func pickMockReply(workspaceID, requestID string) string {
	if len(mockReplyVariants) == 0 {
		return "On it!"
	}
	if workspaceID == "" && requestID == "" {
		return mockReplyVariants[0]
	}
	h := sha1.Sum([]byte(workspaceID + ":" + requestID))
	idx := int(binary.BigEndian.Uint32(h[0:4]) % uint32(len(mockReplyVariants)))
	return mockReplyVariants[idx]
}

// lookupRuntime returns the workspace's runtime string. Empty when the
// row is missing / DB hiccup so callers fall through to the existing
// dispatch path (which will then 404 / 502 normally). Fail-open here
// because a transient DB error must not silently flip a real workspace
// into mock-mode and start handing out canned replies in place of
// genuine agent traffic.
func lookupRuntime(ctx context.Context, workspaceID string) string {
	var runtime sql.NullString
	err := db.DB.QueryRowContext(ctx,
		`SELECT runtime FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&runtime)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("ProxyA2A: lookupRuntime(%s) failed (%v) — falling through to dispatch path", workspaceID, err)
		}
		return ""
	}
	if !runtime.Valid {
		return ""
	}
	return runtime.String
}

// buildMockA2AResponse synthesises a JSON-RPC 2.0 success envelope that
// matches the a2a-sdk v0.3 reply shape the canvas's extractAgentText
// already understands: `{result: {parts: [{kind: "text", text: ...}]}}`.
// `requestID` is the JSON-RPC `id` of the inbound request — A2A
// implementations echo it on the reply so callers can correlate. We
// extract it from the normalized payload in the caller and pass it in
// here so this function stays JSON-only (no payload parsing).
//
// Returns marshalled bytes ready to write straight to the HTTP body.
// Marshal failure is logged + a tiny fallback envelope returned, since
// failing the whole request because of a JSON encoding hiccup on a
// constant-shaped payload would defeat the "mock always works" guarantee.
func buildMockA2AResponse(workspaceID, requestID, replyText string) []byte {
	if requestID == "" {
		requestID = uuid.New().String()
	}
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"result": map[string]any{
			"parts": []map[string]any{
				{"kind": "text", "text": replyText},
			},
		},
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		log.Printf("ProxyA2A: mock-runtime response marshal failed for %s: %v — emitting fallback", workspaceID, err)
		// Hand-rolled minimal envelope. Safe because every value is a
		// hardcoded constant string with no characters that need
		// escaping in a JSON string literal.
		fallback := fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%q,"result":{"parts":[{"kind":"text","text":%q}]}}`,
			requestID, replyText,
		)
		return []byte(fallback)
	}
	return out
}

// extractRequestID pulls the JSON-RPC `id` out of an already-normalized
// A2A payload. Returns "" when the field is absent or not a string —
// caller substitutes a fresh UUID. Tolerant of every shape
// normalizeA2APayload could produce.
func extractRequestID(body []byte) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return ""
	}
	raw, ok := top["id"]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// JSON-RPC permits numeric IDs too; canvas issues UUIDs but be
	// defensive against alternative SDKs.
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// handleMockA2A is the proxy short-circuit for mock-runtime workspaces.
// Returns (status, body, true) when the target is mock — caller writes
// the response and returns. Returns (_, _, false) when the target is
// not mock — caller continues to the real dispatch path.
//
// Side-effects: writes a synthetic activity_logs row via logA2ASuccess
// when logActivity is true so the canvas's "Agent Comms" tab shows the
// mock reply in the trace alongside real-agent traffic. Without this
// the demo would render messages on the canvas chat panel but a peer
// node clicking through to its activity tab would see an empty list.
func (h *WorkspaceHandler) handleMockA2A(ctx context.Context, workspaceID, callerID string, body []byte, a2aMethod string, logActivity bool) (int, []byte, bool) {
	if lookupRuntime(ctx, workspaceID) != MockRuntimeName {
		return 0, nil, false
	}
	requestID := extractRequestID(body)
	replyText := pickMockReply(workspaceID, requestID)
	respBody := buildMockA2AResponse(workspaceID, requestID, replyText)

	// Tiny artificial delay so the canvas chat UI has time to render
	// the user's outgoing bubble before the agent reply appears.
	// Without it the reply lands the same animation frame and feels
	// robotic. 80ms is too fast to look "real" but masks the React
	// double-render race that drops the user bubble entirely on slow
	// machines (observed locally on M1 Air, 2026-05-07). Below 200ms
	// keeps a 200-node demo snappy when investors fan out 30 messages
	// at once.
	time.Sleep(80 * time.Millisecond)

	if logActivity {
		// Reuse the existing success-logger so the activity feed shape
		// is identical to a real agent reply. Status 200 + duration 0
		// is the "synthesised reply" marker; activity_logs.duration_ms
		// being 0 is harmless (real fast paths can hit 0 too).
		h.logA2ASuccess(ctx, workspaceID, callerID, body, respBody, a2aMethod, http.StatusOK, 0)
	}
	return http.StatusOK, respBody, true
}

// IsMockRuntime is a small public helper for callers outside this
// package (tests, the org importer) that need to ask the question
// without depending on the unexported constant. Trims + lower-cases
// so a typoed YAML cell like "  Mock " still resolves correctly.
func IsMockRuntime(runtime string) bool {
	return strings.EqualFold(strings.TrimSpace(runtime), MockRuntimeName)
}

// gin import is unused at file scope but kept as a tag so a future
// addition of a thin HTTP handler (e.g. POST /workspaces/:id/mock/replies
// for an admin-set custom reply pool) doesn't need an import re-order.
var _ = gin.H{}
