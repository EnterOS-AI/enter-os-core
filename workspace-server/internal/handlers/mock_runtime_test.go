package handlers

// mock_runtime_test.go — locks the contract for the mock-runtime
// short-circuit added for the funding-demo "200-workspace mock org"
// template. Three invariants:
//
//   1. ProxyA2A on a workspace with runtime='mock' must return 200
//      with a JSON-RPC reply containing one text part. NO HTTP
//      dispatch, NO resolveAgentURL DB read (mock workspaces have
//      no URL — that read would 404 and break the demo).
//
//   2. The reply text must be one of the canned variants and must be
//      deterministic for a given (workspace_id, request_id) pair so
//      screen recordings replay identically.
//
//   3. Workspaces with runtime != 'mock' must NOT be affected — the
//      mock check fails fast and falls through to the existing
//      dispatch path. Same kind of regression guard the poll-mode
//      tests carry.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestProxyA2A_MockRuntime_ReturnsCannedReply is the happy-path
// contract. A workspace flagged runtime='mock' must:
//   - return 200 with JSON-RPC envelope {result:{parts:[{kind:text,text:...}]}}
//   - not dispatch HTTP (no SELECT url SQL expected)
//   - reply text is one of mockReplyVariants
func TestProxyA2A_MockRuntime_ReturnsCannedReply(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	const wsID = "ws-mock-canned"

	// Budget check fires before runtime lookup (same as the poll-mode
	// short-circuit) — keeps mock workspaces honest if a tenant ever
	// sets a budget on one. Unlikely on a demo, but the guard stays
	// uniform so future "monthly_spend on mock = 0" assertions don't
	// drift.
	expectBudgetCheck(mock, wsID)

	// lookupDeliveryMode runs first — return push so the poll
	// short-circuit doesn't fire and we hit the mock check.
	mock.ExpectQuery("SELECT delivery_mode FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode"}).AddRow("push"))

	// lookupRuntime SELECT — returns 'mock', triggering the canned-reply
	// short-circuit. CRITICAL: NO ExpectQuery for `SELECT url, status
	// FROM workspaces` (resolveAgentURL's query). If the short-circuit
	// fails to fire, sqlmock will surface "unexpected query" on the URL
	// SELECT and the test fails loudly — that's the dispatch-leak detector.
	mock.ExpectQuery("SELECT runtime FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("mock"))

	// Activity log: logA2ASuccess writes the synthetic reply to
	// activity_logs so the canvas's Agent Comms tab shows it alongside
	// real-agent traffic.
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}

	body := `{"jsonrpc":"2.0","id":"req-mock-1","method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"hello mock"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	// logA2ASuccess fires async — give it a moment to settle so
	// ExpectationsWereMet doesn't flake.
	time.Sleep(200 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp["jsonrpc"] != "2.0" {
		t.Errorf("response.jsonrpc = %v, want 2.0", resp["jsonrpc"])
	}
	if resp["id"] != "req-mock-1" {
		t.Errorf("response.id = %v, want %q (echoed from request)", resp["id"], "req-mock-1")
	}
	result, _ := resp["result"].(map[string]interface{})
	if result == nil {
		t.Fatalf("response.result missing or wrong type: %v", resp["result"])
	}
	parts, _ := result["parts"].([]interface{})
	if len(parts) != 1 {
		t.Fatalf("expected exactly one part, got %d: %v", len(parts), parts)
	}
	part, _ := parts[0].(map[string]interface{})
	if part["kind"] != "text" {
		t.Errorf("part.kind = %v, want text", part["kind"])
	}
	text, _ := part["text"].(string)
	if text == "" {
		t.Error("part.text is empty — canned reply not populated")
	}
	// Reply must be one of the variants.
	matched := false
	for _, v := range mockReplyVariants {
		if v == text {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("reply text %q is not in mockReplyVariants", text)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestProxyA2A_NonMockRuntime_NoShortCircuit verifies the symmetric
// contract: a workspace with a real runtime (claude-code, hermes, etc.)
// must NOT be affected by the mock check — it falls through to the
// real dispatch path. Without this guard, a regression in
// lookupRuntime could silently flip every workspace into mock-mode
// and start handing out canned replies in place of real-agent traffic.
func TestProxyA2A_NonMockRuntime_NoShortCircuit(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	const wsID = "ws-real-runtime"

	dispatched := false
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatched = true
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"status":"ok"}}`))
	}))
	defer agentServer.Close()
	mr.Set("ws:"+wsID+":url", agentServer.URL)

	expectBudgetCheck(mock, wsID)

	// poll-mode SELECT — return push so we proceed past the poll
	// short-circuit.
	mock.ExpectQuery("SELECT delivery_mode FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode"}).AddRow("push"))

	// runtime SELECT — return claude-code so the mock check falls
	// through.
	mock.ExpectQuery("SELECT runtime FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))

	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	body := `{"jsonrpc":"2.0","id":"real-1","method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"hi"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !dispatched {
		t.Error("non-mock runtime: expected the agent server to receive the request, but it did not — mock short-circuit may be over-firing")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestPickMockReply_Deterministic locks the determinism contract:
// the same (workspaceID, requestID) input must yield the same variant
// every call. Required for screen recordings + flake-free e2e
// snapshots.
func TestPickMockReply_Deterministic(t *testing.T) {
	cases := []struct {
		ws, req string
	}{
		{"ws-1", "req-A"},
		{"ws-1", "req-B"},
		{"ws-2", "req-A"},
		{"", ""},
	}
	for _, tc := range cases {
		first := pickMockReply(tc.ws, tc.req)
		for i := 0; i < 10; i++ {
			next := pickMockReply(tc.ws, tc.req)
			if next != first {
				t.Errorf("pickMockReply(%q,%q) is not deterministic: got %q then %q",
					tc.ws, tc.req, first, next)
			}
		}
	}
}

// TestIsMockRuntime_TrimsAndCaseInsensitive — typos and stray
// whitespace in YAML must still resolve to mock so a single
// runtime: " Mock " entry doesn't silently get dispatched.
func TestIsMockRuntime_TrimsAndCaseInsensitive(t *testing.T) {
	cases := map[string]bool{
		"mock":      true,
		"MOCK":      true,
		"  Mock  ":  true,
		"mocky":     false,
		"":          false,
		"external":  false,
		"claude-code": false,
	}
	for in, want := range cases {
		if got := IsMockRuntime(in); got != want {
			t.Errorf("IsMockRuntime(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestBuildMockA2AResponse_EchoesRequestID — JSON-RPC requires the
// reply id to match the request id so callers can correlate. Mock
// must hold this contract or canvas's correlation logic breaks.
func TestBuildMockA2AResponse_EchoesRequestID(t *testing.T) {
	out := buildMockA2AResponse("ws-x", "req-echo-7", "On it!")
	var resp map[string]interface{}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp["id"] != "req-echo-7" {
		t.Errorf("id = %v, want req-echo-7", resp["id"])
	}
	if resp["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", resp["jsonrpc"])
	}
	result, _ := resp["result"].(map[string]interface{})
	parts, _ := result["parts"].([]interface{})
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	p, _ := parts[0].(map[string]interface{})
	if p["text"] != "On it!" {
		t.Errorf("part.text = %v, want On it!", p["text"])
	}
}
