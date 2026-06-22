package handlers

// a2a_full_body_delivery_guard_test.go — regression guard for core#2175.
//
// core#2175 RCA: the long-believed "A2A truncation" was a MISDIAGNOSIS.
// A2A message delivery preserves the FULL body on every agent-facing path.
// Only HUMAN-facing DISPLAY previews are capped (activity title 80 runes,
// broadcast 120, delegation summary 80, canvas response_preview 200 bytes).
// Those caps live on display/broadcast fields, NOT on the bytes an agent
// reads off the wire.
//
// This file locks in the correct behaviour so a FUTURE change cannot
// silently reintroduce REAL truncation on the agent-facing delivery paths:
//
//   1. DequeueNext (a2a_queue.go) — the drain/read path does
//      `SELECT ... body::text ...` and returns item.Body. The delivered
//      body MUST equal the enqueued body byte-for-byte.
//
//   2. toolCheckTaskStatus (mcp_tools.go) — reads activity_logs.response_body
//      and surfaces result["result"] = extractA2AText(responseBody). The
//      returned text MUST be the COMPLETE response text, not a preview.
//
// Both bodies used here are WELL over 200 chars (> the largest preview cap,
// canvas response_preview at 200 bytes) so a regression that wired any
// display cap into a delivery path would fail loudly.
//
// Style: matches the sibling a2a_queue_test.go / mcp_tools_test.go — sqlmock,
// no integration build tag. These paths are deterministically exercisable
// against the mock because the truncation guard is about what the Go code
// does with the row value, not about Postgres-side text handling. CI's
// real-PG integration arm (a2a_*_integration tests) additionally exercises
// the live `body::text` round-trip.

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/DATA-DOG/go-sqlmock"
)

// largeA2ABody builds a syntactically valid A2A JSON-RPC message body whose
// embedded text part is `textLen` runes long, so the whole body comfortably
// exceeds every human-facing preview cap (max 200 bytes).
func largeA2ABody(textLen int) string {
	longText := strings.Repeat("A", textLen)
	return `{"jsonrpc":"2.0","method":"message/send","params":{"message":{"role":"user","messageId":"guard-2175","parts":[{"type":"text","text":"` + longText + `"}]}}}`
}

// TestDequeueNext_PreservesFullBody_NoTruncation is the guard for the queue
// drain/read path. It asserts that the body returned from DequeueNext equals
// the enqueued body byte-for-byte, even when far longer than any preview cap.
func TestDequeueNext_PreservesFullBody_NoTruncation(t *testing.T) {
	// 4000-char text part → total body well over the 200-byte canvas cap and
	// every other display preview cap.
	fullBody := largeA2ABody(4000)
	if len(fullBody) <= 200 {
		t.Fatalf("test setup error: body must exceed the largest preview cap (200); got %d", len(fullBody))
	}

	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	const wsID = "ws-guard-2175"
	const itemID = "qid-guard-2175"

	// DequeueNext runs BEGIN → SELECT ... body::text ... → UPDATE → COMMIT.
	// The mocked SELECT returns the FULL body in the body column; the guard
	// is that DequeueNext propagates it untouched into item.Body.
	mock.ExpectBegin()
	mock.ExpectQuery(
		"SELECT id, workspace_id, caller_id, priority, body::text, method, attempts FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued' AND (expires_at IS NULL OR expires_at > now()) AND (next_attempt_at IS NULL OR next_attempt_at <= now()) ORDER BY priority DESC, enqueued_at ASC FOR UPDATE SKIP LOCKED LIMIT 1").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "caller_id", "priority", "body", "method", "attempts",
		}).AddRow(
			itemID, wsID, sql.NullString{Valid: false}, PriorityTask,
			fullBody, sql.NullString{String: "message/send", Valid: true}, 0,
		))
	mock.ExpectExec(
		"UPDATE a2a_queue SET status = 'dispatched', dispatched_at = now(), attempts = attempts + 1 WHERE id = $1").
		WithArgs(itemID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	item, err := DequeueNext(context.Background(), wsID)
	if err != nil {
		t.Fatalf("DequeueNext returned error: %v", err)
	}
	if item == nil {
		t.Fatal("DequeueNext returned nil item for a non-empty queue")
	}

	if got := string(item.Body); got != fullBody {
		t.Errorf("delivered body was truncated/altered.\n  enqueued len=%d\n  delivered len=%d\n  REGRESSION: a delivery path must NOT apply a display preview cap (core#2175)",
			len(fullBody), len(got))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestToolCheckTaskStatus_ReturnsFullResponseBody_NoTruncation is the guard
// for the check_task_status agent-facing read path. It asserts that the text
// surfaced in result["result"] (via extractA2AText over response_body) is the
// COMPLETE response text — never a preview-capped slice.
func TestToolCheckTaskStatus_ReturnsFullResponseBody_NoTruncation(t *testing.T) {
	// 3000-char response text, far above any preview cap.
	fullText := strings.Repeat("B", 3000)
	responseBody := `{"jsonrpc":"2.0","result":{"artifacts":[{"parts":[{"type":"text","text":"` + fullText + `"}]}]}}`

	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	t.Cleanup(func() { mockDB.Close() })

	h := &MCPHandler{database: mockDB}

	const callerID = "ws-caller-2175"
	const targetID = "ws-target-2175"
	const taskID = "del-guard-2175"

	mock.ExpectQuery(`SELECT status, error_detail, response_body`).
		WithArgs(callerID, targetID, taskID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "error_detail", "response_body"}).
			AddRow("completed", sql.NullString{Valid: false}, []byte(responseBody)))

	out, err := h.toolCheckTaskStatus(context.Background(), callerID, map[string]interface{}{
		"workspace_id": targetID,
		"task_id":      taskID,
	})
	if err != nil {
		t.Fatalf("toolCheckTaskStatus returned error: %v", err)
	}

	// The full text must appear in the serialized result. If a future change
	// applied a preview cap (e.g. TruncateBytes(…, 200)) to the agent-facing
	// result, this substring check would fail.
	if !strings.Contains(out, fullText) {
		t.Errorf("check_task_status result was truncated.\n  expected full %d-char response text in result\n  REGRESSION: the agent-facing check_task_status path must return the COMPLETE response_body, not a display preview (core#2175)",
			len(fullText))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestExtractA2AText_FullBodyNoCap is a focused unit-level guard on the
// extractor itself: extractA2AText must return the entire text part with no
// length cap, for both supported A2A response shapes.
func TestExtractA2AText_FullBodyNoCap(t *testing.T) {
	fullText := strings.Repeat("C", 2500)

	cases := map[string]string{
		"artifacts shape": `{"result":{"artifacts":[{"parts":[{"type":"text","text":"` + fullText + `"}]}]}}`,
		"message shape":   `{"result":{"message":{"parts":[{"type":"text","text":"` + fullText + `"}]}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got := extractA2AText([]byte(body))
			if got != fullText {
				t.Errorf("extractA2AText capped/altered the text.\n  want len=%d\n  got  len=%d\n  REGRESSION: extractor must not truncate (core#2175)",
					len(fullText), len(got))
			}
		})
	}
}
