package messagestore

// postgres_store_test.go — parser-level parity tests against the
// canvas TS test fixtures in
// canvas/src/components/tabs/chat/__tests__/historyHydration.test.ts.
//
// Originally lived in handlers/chat_history_test.go (RFC #2945 PR-C);
// PR-D moved them here when the parser was extracted to this package.
// Every test case in the TS file has a Go counterpart, named after
// the TS describe/it block.
//
// Mutation guidance: when adding behavior, add the case to BOTH
// historyHydration.test.ts AND this file. The canvas TS is the
// legacy source the server replaces; divergence == regression.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const fixedTimestamp = "2026-04-25T18:00:00Z"

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return tt
}

func neverInternal(_ string) bool { return false }

// =====================================================================
// timestamp preservation (regression cover)
//
// The canvas bug that motivated extracting the helper: every reload
// re-stamped historical bubbles to render-time. Pin row.created_at
// adoption.
// =====================================================================

func TestChatHistory_UserMessageTimestampPinsToCreatedAt(t *testing.T) {
	created := mustParseTime(t, "2026-04-25T18:00:00Z")
	body := json.RawMessage(`{"params":{"message":{"parts":[{"kind":"text","text":"hello from earlier today"}]}}}`)

	msgs := activityRowToChatMessages(created, "ok", body, nil, neverInternal)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 user message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("role=%q want user", msgs[0].Role)
	}
	if !strings.HasPrefix(msgs[0].Timestamp, "2026-04-25T18:00:00") {
		t.Errorf("user message timestamp %q does NOT pin to row.created_at — regression of the 2026-04-25 bubble-collapse bug", msgs[0].Timestamp)
	}
}

func TestChatHistory_AgentMessageTimestampPinsToCreatedAt(t *testing.T) {
	created := mustParseTime(t, "2026-04-25T18:05:00Z")
	body := json.RawMessage(`{"result":"agent reply"}`)

	msgs := activityRowToChatMessages(created, "ok", nil, body, neverInternal)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 agent message, got %d", len(msgs))
	}
	if msgs[0].Role != "agent" {
		t.Errorf("role=%q want agent", msgs[0].Role)
	}
	if !strings.HasPrefix(msgs[0].Timestamp, "2026-04-25T18:05:00") {
		t.Errorf("agent message timestamp %q does NOT pin to row.created_at", msgs[0].Timestamp)
	}
}

func TestChatHistory_TwoRowsDistinctTimestamps(t *testing.T) {
	bodyA := json.RawMessage(`{"params":{"message":{"parts":[{"kind":"text","text":"first"}]}}}`)
	bodyB := json.RawMessage(`{"params":{"message":{"parts":[{"kind":"text","text":"second"}]}}}`)
	a := activityRowToChatMessages(mustParseTime(t, "2026-04-25T14:00:00Z"), "ok", bodyA, nil, neverInternal)
	b := activityRowToChatMessages(mustParseTime(t, "2026-04-25T21:01:58Z"), "ok", bodyB, nil, neverInternal)

	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected 1 message each; got %d and %d", len(a), len(b))
	}
	if a[0].Timestamp == b[0].Timestamp {
		t.Errorf("two distinct created_at values produced same timestamp: %q", a[0].Timestamp)
	}
	if !strings.HasPrefix(a[0].Timestamp, "2026-04-25T14:00:00") || !strings.HasPrefix(b[0].Timestamp, "2026-04-25T21:01:58") {
		t.Errorf("timestamps drifted: a=%q b=%q", a[0].Timestamp, b[0].Timestamp)
	}
}

// =====================================================================
// user-message extraction
// =====================================================================

func TestChatHistory_EmitsUserMessageWhenRequestHasText(t *testing.T) {
	body := json.RawMessage(`{"params":{"message":{"parts":[{"kind":"text","text":"hi agent"}]}}}`)
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", body, nil, neverInternal)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hi agent" {
		t.Errorf("role=%q content=%q want user/hi agent", msgs[0].Role, msgs[0].Content)
	}
}

func TestChatHistory_DropsInternalSelfMessages(t *testing.T) {
	body := json.RawMessage(`{"params":{"message":{"parts":[{"kind":"text","text":"Delegation results are ready..."}]}}}`)
	predicate := func(t string) bool { return strings.HasPrefix(t, "Delegation results are ready") }
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", body, nil, predicate)
	for _, m := range msgs {
		if m.Role == "user" {
			t.Errorf("internal-self message rendered as user bubble: %q", m.Content)
		}
	}
}

func TestChatHistory_NoUserMessageWhenRequestBodyNull(t *testing.T) {
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", nil, nil, neverInternal)
	for _, m := range msgs {
		if m.Role == "user" {
			t.Errorf("emitted user bubble despite null request_body: %+v", m)
		}
	}
}

func TestChatHistory_UserAttachmentsHydratedFromRequestBody(t *testing.T) {
	body := json.RawMessage(`{
	  "params": {
	    "message": {
	      "parts": [
	        {"kind":"text","text":"here's the screenshot"},
	        {"kind":"file","file":{"name":"shot.png","mimeType":"image/png","uri":"workspace:/uploads/shot.png","size":4096}}
	      ]
	    }
	  }
	}`)
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", body, nil, neverInternal)
	var user *ChatMessage
	for i := range msgs {
		if msgs[i].Role == "user" {
			user = &msgs[i]
			break
		}
	}
	if user == nil {
		t.Fatalf("no user bubble produced")
	}
	if user.Content != "here's the screenshot" {
		t.Errorf("content=%q", user.Content)
	}
	if len(user.Attachments) != 1 {
		t.Fatalf("attachments=%d want 1", len(user.Attachments))
	}
	att := user.Attachments[0]
	if att.Name != "shot.png" || att.URI != "workspace:/uploads/shot.png" || att.MimeType != "image/png" {
		t.Errorf("attachment shape wrong: %+v", att)
	}
	if att.Size == nil || *att.Size != 4096 {
		t.Errorf("size=%v want 4096", att.Size)
	}
}

func TestChatHistory_AttachmentsOnlyUserBubbleWhenTextEmpty(t *testing.T) {
	// Drag-drop a file with no caption — bubble should still render.
	body := json.RawMessage(`{
	  "params": {
	    "message": {
	      "parts": [
	        {"kind":"file","file":{"name":"report.pdf","uri":"workspace:/uploads/report.pdf"}}
	      ]
	    }
	  }
	}`)
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", body, nil, neverInternal)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 attachments-only bubble, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "" || len(msgs[0].Attachments) != 1 {
		t.Errorf("unexpected: role=%q content=%q attachments=%d", msgs[0].Role, msgs[0].Content, len(msgs[0].Attachments))
	}
	if msgs[0].Attachments[0].Name != "report.pdf" {
		t.Errorf("attachment name=%q want report.pdf", msgs[0].Attachments[0].Name)
	}
}

func TestChatHistory_InternalSelfPredicateSuppressesEvenWithAttachments(t *testing.T) {
	body := json.RawMessage(`{
	  "params": {
	    "message": {
	      "parts": [
	        {"kind":"text","text":"Delegation results are ready..."},
	        {"kind":"file","file":{"name":"x.zip","uri":"workspace:/x.zip"}}
	      ]
	    }
	  }
	}`)
	predicate := func(t string) bool { return strings.HasPrefix(t, "Delegation results are ready") }
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", body, nil, predicate)
	for _, m := range msgs {
		if m.Role == "user" {
			t.Errorf("internal-self predicate did NOT suppress user bubble despite attachments: %+v", m)
		}
	}
}

// =====================================================================
// agent-message extraction
// =====================================================================

func TestChatHistory_AgentMessageFromResultString(t *testing.T) {
	body := json.RawMessage(`{"result":"agent says hi"}`)
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", nil, body, neverInternal)
	if len(msgs) != 1 || msgs[0].Role != "agent" || msgs[0].Content != "agent says hi" {
		t.Errorf("got %+v", msgs)
	}
}

func TestChatHistory_RoleSystemWhenStatusError(t *testing.T) {
	body := json.RawMessage(`{"result":"delegation failed"}`)
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "error", nil, body, neverInternal)
	if len(msgs) != 1 || msgs[0].Role != "system" {
		t.Errorf("status=error did NOT promote role to system: %+v", msgs)
	}
}

func TestChatHistory_RoleSystemWhenAgentErrorPrefix(t *testing.T) {
	// Defense-in-depth — if a runtime returns ok status but the text
	// itself starts with "agent error", the canvas would still
	// render system role. Mirror that here.
	body := json.RawMessage(`{"result":"Agent error: ProcessError(exit=1)"}`)
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", nil, body, neverInternal)
	if len(msgs) != 1 || msgs[0].Role != "system" {
		t.Errorf("agent-error prefix did NOT promote to system: %+v", msgs)
	}
}

func TestChatHistory_AgentAttachmentsFromResponseBodyParts(t *testing.T) {
	// Notify shape: response_body = {"result":"<text>","parts":[{"kind":"file",...}]}
	body := json.RawMessage(`{
	  "result": "Done — see attached.",
	  "parts": [
	    {"kind":"file","file":{"name":"build.zip","uri":"workspace:/tmp/build.zip","size":12345}}
	  ]
	}`)
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", nil, body, neverInternal)
	var agent *ChatMessage
	for i := range msgs {
		if msgs[i].Role == "agent" {
			agent = &msgs[i]
			break
		}
	}
	if agent == nil {
		t.Fatalf("no agent bubble")
	}
	if len(agent.Attachments) != 1 || agent.Attachments[0].Name != "build.zip" {
		t.Errorf("agent attachments shape wrong: %+v", agent.Attachments)
	}
	if agent.Attachments[0].Size == nil || *agent.Attachments[0].Size != 12345 {
		t.Errorf("size=%v want 12345", agent.Attachments[0].Size)
	}
}

func TestChatHistory_NoAgentMessageWhenResponseBodyNull(t *testing.T) {
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", nil, nil, neverInternal)
	for _, m := range msgs {
		if m.Role == "agent" || m.Role == "system" {
			t.Errorf("emitted agent/system bubble despite null response_body: %+v", m)
		}
	}
}

func TestChatHistory_NoAgentMessageWhenResponseHasNoTextNoFiles(t *testing.T) {
	body := json.RawMessage(`{"unrelated":"metadata"}`)
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", nil, body, neverInternal)
	for _, m := range msgs {
		if m.Role == "agent" {
			t.Errorf("emitted agent bubble despite empty content: %+v", m)
		}
	}
}

// =====================================================================
// List() integration — sqlmock-backed end-to-end via the real handler
// =====================================================================

// TestList_WireOrderIsOldestFirstAcrossPagedRows pins the integration
// invariant: List() returns wire-display-ready messages even though
// the underlying SQL is `ORDER BY created_at DESC`. This is the
// load-bearing test for PR-C-2 — without the row-aware reversal,
// canvas would render every paired bubble in the wrong order on every
// chat reload (agent before user within each timestamp).
//
// Mutation-test cover: removing the `messages = reverseRowChunks(...)`
// call in List() must turn this test red. (The lower-level
// TestReverseRowChunks_PreservesPairOrderAcrossRows pins the helper
// itself; this test pins that List ACTUALLY CALLS the helper.)
func TestList_WireOrderIsOldestFirstAcrossPagedRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// Server's SQL is ORDER BY created_at DESC. Build mock rows in
	// THAT order so the row-aware reversal has work to do.
	rows := sqlmock.NewRows([]string{"created_at", "status", "request_body", "response_body"}).
		AddRow(mustParseTime(t, "2026-05-05T00:03:00Z"), "ok",
			`{"params":{"message":{"parts":[{"kind":"text","text":"u3"}]}}}`,
			`{"result":"a3"}`).
		AddRow(mustParseTime(t, "2026-05-05T00:02:00Z"), "ok",
			`{"params":{"message":{"parts":[{"kind":"text","text":"u2"}]}}}`,
			`{"result":"a2"}`).
		AddRow(mustParseTime(t, "2026-05-05T00:01:00Z"), "ok",
			`{"params":{"message":{"parts":[{"kind":"text","text":"u1"}]}}}`,
			`{"result":"a1"}`)

	mock.ExpectQuery(`SELECT created_at, status, request_body::text, response_body::text`).
		WillReturnRows(rows)

	store := NewPostgresMessageStore(db)
	msgs, reachedEnd, err := store.List(context.Background(), "ws-1", ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	wantContents := []string{"u1", "a1", "u2", "a2", "u3", "a3"}
	if len(msgs) != len(wantContents) {
		t.Fatalf("len(msgs)=%d want %d; got=%v", len(msgs), len(wantContents), msgs)
	}
	for i, w := range wantContents {
		if msgs[i].Content != w {
			t.Errorf("idx %d: got %q want %q (full slice ordering broken; reverseRowChunks regressed?)", i, msgs[i].Content, w)
		}
	}
	if !reachedEnd {
		t.Errorf("3 rows < limit 10 should reach end, got reachedEnd=false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// =====================================================================
// reverseRowChunks — wire-order helper added in PR-C-2
// =====================================================================

// TestReverseRowChunks_PreservesPairOrderAcrossRows pins the
// row-aware reversal that List() applies before returning. Server's
// SQL is `ORDER BY created_at DESC`, so messages come out
// newest-row-first; activityRowToChatMessages emits [user, agent]
// per row with same timestamp. A naive flat reversal of the messages
// slice would flip each pair (agent before user). reverseRowChunks
// reverses ROWS, preserving pair-internal order. Without this, canvas
// would render every paired bubble in the wrong order on every chat
// reload — the canvas-side reverse used to do the right thing because
// it reversed ROWS BEFORE flattening, but PR-C/D moved the flattening
// into the server, so the row-awareness has to live there too.
func TestReverseRowChunks_PreservesPairOrderAcrossRows(t *testing.T) {
	// Build messages newest-row-first as List() collects them. Each
	// row is a pair sharing a timestamp, with [user, agent] order.
	in := []ChatMessage{
		{Role: "user", Content: "user_3", Timestamp: "2026-05-05T00:03:00Z"},
		{Role: "agent", Content: "agent_3", Timestamp: "2026-05-05T00:03:00Z"},
		{Role: "user", Content: "user_2", Timestamp: "2026-05-05T00:02:00Z"},
		{Role: "agent", Content: "agent_2", Timestamp: "2026-05-05T00:02:00Z"},
		{Role: "user", Content: "user_1", Timestamp: "2026-05-05T00:01:00Z"},
		{Role: "agent", Content: "agent_1", Timestamp: "2026-05-05T00:01:00Z"},
	}
	got := reverseRowChunks(in)

	want := []struct {
		role, content string
	}{
		{"user", "user_1"}, {"agent", "agent_1"},
		{"user", "user_2"}, {"agent", "agent_2"},
		{"user", "user_3"}, {"agent", "agent_3"},
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d len(want)=%d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Role != w.role || got[i].Content != w.content {
			t.Errorf("idx %d: got role=%q content=%q want role=%q content=%q",
				i, got[i].Role, got[i].Content, w.role, w.content)
		}
	}
}

// TestReverseRowChunks_HandlesSingleMessageRows pins the case where
// a row has only a user OR only an agent message (e.g., agent reply
// not yet recorded, attachments-only user upload). Naive reversal
// still works for single-message chunks; the test guards against a
// future change that special-cases the 2-message-row path.
func TestReverseRowChunks_HandlesSingleMessageRows(t *testing.T) {
	in := []ChatMessage{
		{Role: "user", Content: "u3", Timestamp: "2026-05-05T00:03:00Z"},
		{Role: "user", Content: "u2", Timestamp: "2026-05-05T00:02:00Z"}, // single, no agent
		{Role: "agent", Content: "a2", Timestamp: "2026-05-05T00:02:00Z"},
		{Role: "user", Content: "u1", Timestamp: "2026-05-05T00:01:00Z"},
	}
	got := reverseRowChunks(in)
	wantContents := []string{"u1", "u2", "a2", "u3"}
	if len(got) != len(wantContents) {
		t.Fatalf("len got=%d want=%d", len(got), len(wantContents))
	}
	for i, w := range wantContents {
		if got[i].Content != w {
			t.Errorf("idx %d: got %q want %q", i, got[i].Content, w)
		}
	}
}

// TestReverseRowChunks_EmptyInput returns nil/empty without panic.
func TestReverseRowChunks_EmptyInput(t *testing.T) {
	got := reverseRowChunks(nil)
	if len(got) != 0 {
		t.Errorf("nil input should return empty, got %v", got)
	}
}

// =====================================================================
// end-to-end shape — paired user + agent with same timestamp
// =====================================================================

func TestChatHistory_PairedUserAndAgentSameTimestamp(t *testing.T) {
	created := mustParseTime(t, "2026-04-25T18:00:00Z")
	req := json.RawMessage(`{"params":{"message":{"parts":[{"kind":"text","text":"what's 2+2?"}]}}}`)
	resp := json.RawMessage(`{"result":"4"}`)
	msgs := activityRowToChatMessages(created, "ok", req, resp, neverInternal)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "what's 2+2?" {
		t.Errorf("first message wrong: %+v", msgs[0])
	}
	if msgs[1].Role != "agent" || msgs[1].Content != "4" {
		t.Errorf("second message wrong: %+v", msgs[1])
	}
	if msgs[0].Timestamp != msgs[1].Timestamp {
		t.Errorf("paired bubbles have different timestamps: %q vs %q", msgs[0].Timestamp, msgs[1].Timestamp)
	}
}

// =====================================================================
// Go-specific: defensive parsing
// =====================================================================

func TestChatHistory_MalformedJSONInRequestBodyReturnsEmpty(t *testing.T) {
	// Should NOT panic; should return no user bubble (or no message at all).
	body := json.RawMessage(`{not valid json}`)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on malformed json: %v", r)
		}
	}()
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", body, nil, neverInternal)
	for _, m := range msgs {
		if m.Role == "user" && (m.Content != "" || len(m.Attachments) > 0) {
			t.Errorf("malformed JSON yielded a non-empty user bubble: %+v", m)
		}
	}
}

func TestChatHistory_V1ProtobufFlatFileShape(t *testing.T) {
	// v1 a2a-sdk shape: flat parts with url/filename/mediaType
	body := json.RawMessage(`{
	  "result": {
	    "parts": [
	      {"url":"https://example.com/data.csv","filename":"data.csv","mediaType":"text/csv"}
	    ]
	  }
	}`)
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", nil, body, neverInternal)
	var agent *ChatMessage
	for i := range msgs {
		if msgs[i].Role == "agent" {
			agent = &msgs[i]
			break
		}
	}
	if agent == nil {
		t.Fatalf("no agent bubble for v1 shape")
	}
	if len(agent.Attachments) != 1 {
		t.Fatalf("attachments=%d want 1", len(agent.Attachments))
	}
	att := agent.Attachments[0]
	if att.Name != "data.csv" || att.URI != "https://example.com/data.csv" || att.MimeType != "text/csv" {
		t.Errorf("v1 shape extracted wrong: %+v", att)
	}
}

func TestChatHistory_TaskShapeArtifactsExtracted(t *testing.T) {
	// {"result":{"artifacts":[{"parts":[{"kind":"text","text":"..."}]}]}}
	body := json.RawMessage(`{
	  "result": {
	    "artifacts": [
	      {"parts": [{"kind":"text","text":"hermes detail line"}]}
	    ]
	  }
	}`)
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", nil, body, neverInternal)
	if len(msgs) != 1 || msgs[0].Content != "hermes detail line" {
		t.Errorf("artifact text not extracted: %+v", msgs)
	}
}

func TestChatHistory_OlderNestedRootTextShape(t *testing.T) {
	// Older shape: {parts: [{root: {text: "..."}}]}
	body := json.RawMessage(`{
	  "result": {
	    "parts": [{"root":{"text":"legacy nested text"}}]
	  }
	}`)
	msgs := activityRowToChatMessages(mustParseTime(t, fixedTimestamp), "ok", nil, body, neverInternal)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "legacy nested text") {
		t.Errorf("nested root.text not extracted: %+v", msgs)
	}
}

// =====================================================================
// IsInternalSelfMessage predicate itself
// =====================================================================

func TestChatHistory_IsInternalSelfMessage_DelegationPrefix(t *testing.T) {
	if !IsInternalSelfMessage("Delegation results are ready... <body>") {
		t.Errorf("Delegation-results prefix should be flagged internal-self")
	}
	if IsInternalSelfMessage("Delegation completed but not ready") {
		t.Errorf("non-prefix match should NOT flag")
	}
	if IsInternalSelfMessage("") {
		t.Errorf("empty text should NOT flag (legitimate attachments-only bubble)")
	}
}

// =====================================================================
// basename helper — mirrors canvas basename() semantics
// =====================================================================

func TestChatHistory_BasenameStripsSchemeAndPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"workspace:/uploads/shot.png", "shot.png"},
		{"workspace:/a/b/c/file.txt", "file.txt"},
		{"https://example.com/path/file.csv", "file.csv"},
		{"http://x/y", "y"},
		{"", "file"},
		{"workspace:", "file"}, // scheme-only collapses to "" → "file" sentinel, matches canvas basename
	}
	for _, tc := range cases {
		got := basename(tc.in)
		if got != tc.want {
			t.Errorf("basename(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}
