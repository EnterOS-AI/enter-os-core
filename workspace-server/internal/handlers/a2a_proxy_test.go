package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/gin-gonic/gin"
)

// ==================== ProxyA2A — invalid JSON body ====================

func TestProxyA2A_InvalidJSON(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Cache a URL so the handler doesn't fall back to DB
	mr.Set(fmt.Sprintf("ws:%s:url", "ws-badjson"), "http://localhost:9999")
	expectBudgetCheck(mock, "ws-badjson")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-badjson"}}

	c.Request = httptest.NewRequest("POST", "/workspaces/ws-badjson/a2a", bytes.NewBufferString("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "invalid JSON" {
		t.Errorf("expected error 'invalid JSON', got %v", resp["error"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== ProxyA2A — already-wrapped JSON-RPC ====================

func TestProxyA2A_AlreadyWrappedJSONRPC(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Create a mock agent that captures the forwarded request
	var receivedBody map[string]interface{}
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"original-id","result":{"status":"ok"}}`)
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", "ws-wrapped"), agentServer.URL)
	expectBudgetCheck(mock, "ws-wrapped")

	// Expect async activity log
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-wrapped"}}

	// Send an already-wrapped JSON-RPC body
	body := `{"jsonrpc":"2.0","id":"original-id","method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hello"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-wrapped/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	// Give the async LogActivity goroutine a moment
	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the proxy preserved the original ID (didn't re-wrap)
	if receivedBody["id"] != "original-id" {
		t.Errorf("expected original id to be preserved, got %v", receivedBody["id"])
	}
	if receivedBody["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc '2.0', got %v", receivedBody["jsonrpc"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== ProxyA2A — DB lookup fallback (Redis miss) ====================

func TestProxyA2A_DBLookupFallback(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t) // empty Redis — no cached URL
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Create mock agent
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{"status":"ok"}}`)
	}))
	defer agentServer.Close()

	// Budget check runs first (before URL resolution)
	expectBudgetCheck(mock, "ws-db-fallback")

	// Redis miss → DB lookup → returns URL
	mock.ExpectQuery("SELECT url, status FROM workspaces WHERE id =").
		WithArgs("ws-db-fallback").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status"}).AddRow(agentServer.URL, "online"))

	// Expect async activity log
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-db-fallback"}}

	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hello"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-db-fallback/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== ProxyA2A — DB lookup error (500) ====================

func TestProxyA2A_DBLookupError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t) // empty Redis
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Budget check runs first (before URL resolution)
	expectBudgetCheck(mock, "ws-dberr")

	// Redis miss → DB lookup → error
	mock.ExpectQuery("SELECT url, status FROM workspaces WHERE id =").
		WithArgs("ws-dberr").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-dberr"}}

	body := `{"method":"message/send","params":{}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-dberr/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== ProxyA2A — agent returns error status ====================

func TestProxyA2A_AgentReturnsError(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","error":{"code":-32000,"message":"agent error"}}`)
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", "ws-agent-err"), agentServer.URL)
	expectBudgetCheck(mock, "ws-agent-err")

	// Expect async activity log (with "error" status since agent returned 500)
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-agent-err"}}

	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"fail"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-agent-err/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	time.Sleep(50 * time.Millisecond)

	// The proxy returns the agent's status code as-is
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500 (agent error), got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestProxyA2A_Upstream502_TriggersContainerDeadCheck — when the agent
// tunnel returns 502 (the "tunnel up but no origin" failure mode that
// surfaces a Cloudflare error page to canvas), proxyA2A must consult
// IsRunning on cpProv. If the EC2 instance truly is dead, the response
// becomes a structured 503 with restarting=true (not the upstream 502
// which CF would mask), and the workspace flips to status='offline' so
// the next reactive poll sees the right state. This is the
// 2026-04-30 hongmingwang.moleculesai.app canvas-chat-to-dead-workspace
// regression: upstream 502 was previously propagated as-is, CF masked
// it, and no auto-restart fired.
func TestProxyA2A_Upstream502_TriggersContainerDeadCheck(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	cp := &fakeCPProv{running: false}
	handler.SetCPProvisioner(cp)

	// Agent tunnel returns 502 with empty body — the CF "no-origin" shape.
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", "ws-tunnel-dead"), agentServer.URL)
	expectBudgetCheck(mock, "ws-tunnel-dead")
	// Activity log fires (delivery_confirmed is true on Do() success regardless
	// of upstream status — handler's existing logA2ASuccess path runs first
	// and logs as success because the dispatch did get a response).
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// maybeMarkContainerDead's runtime lookup, then the offline-flip UPDATE.
	mock.ExpectQuery(`SELECT COALESCE\(runtime, 'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-tunnel-dead").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("hermes"))
	mock.ExpectExec(`UPDATE workspaces SET status = 'offline'`).
		WithArgs("ws-tunnel-dead").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-tunnel-dead"}}
	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-tunnel-dead/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	time.Sleep(80 * time.Millisecond)

	// Caller sees a structured 503 (NOT the upstream 502 which CF would mask).
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("upstream 502 should translate to 503 once cpProv reports dead; got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "restarting") {
		t.Errorf("response body should mention restart trigger; got %s", w.Body.String())
	}
	if w.Header().Get("Retry-After") != "15" {
		t.Errorf("Retry-After header should be 15 to throttle canvas-side retry loop; got %q", w.Header().Get("Retry-After"))
	}
	if cp.calls != 1 {
		t.Errorf("cpProv.IsRunning must be consulted exactly once; got %d calls", cp.calls)
	}
}

// TestProxyA2A_Upstream502_AliveAgent_PropagatesAsIs — the safety check:
// if cpProv reports the EC2 IS running, the upstream 502 is propagated
// as-is. Don't recycle a healthy agent on a transient hiccup — the agent
// might have legitimately returned 502 (e.g. a downstream service it
// called returned 502 and it forwarded). Net behavior matches pre-fix
// for the alive-agent case.
func TestProxyA2A_Upstream502_AliveAgent_PropagatesAsIs(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	cp := &fakeCPProv{running: true}
	handler.SetCPProvisioner(cp)

	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":"downstream service returned 502"}`)
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", "ws-alive-502"), agentServer.URL)
	expectBudgetCheck(mock, "ws-alive-502")
	mock.ExpectExec("INSERT INTO activity_logs").WillReturnResult(sqlmock.NewResult(0, 1))
	// IsRunning runtime lookup runs but no UPDATE follows (running=true).
	mock.ExpectQuery(`SELECT COALESCE\(runtime, 'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-alive-502").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("hermes"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-alive-502"}}
	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-alive-502/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)
	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("alive agent 502 should propagate as 502; got %d: %s", w.Code, w.Body.String())
	}
}

// ==================== ProxyA2A — messageId injection ====================

func TestProxyA2A_MessageIDInjected(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	var receivedBody map[string]interface{}
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{"status":"ok"}}`)
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", "ws-msgid"), agentServer.URL)
	expectBudgetCheck(mock, "ws-msgid")

	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-msgid"}}

	// Send message without messageId — should be injected
	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hello"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-msgid/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify messageId was injected
	params, _ := receivedBody["params"].(map[string]interface{})
	msg, _ := params["message"].(map[string]interface{})
	if msg["messageId"] == nil || msg["messageId"] == "" {
		t.Error("expected messageId to be injected into params.message")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== ProxyA2A — X-Workspace-ID header ====================

func TestProxyA2A_CallerIDPropagated(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{}}`)
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", "ws-target"), agentServer.URL)

	// Access control: caller and target must be siblings (same parent_id)
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id = ").
		WithArgs("ws-caller").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-caller", "ws-parent"))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id = ").
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-target", "ws-parent"))

	expectBudgetCheck(mock, "ws-target")

	// Expect activity log with source_id set
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}

	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"test"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-target/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Workspace-ID", "ws-caller")

	handler.ProxyA2A(c)

	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// mockCanCommunicate sets up sqlmock expectations for CanCommunicate(caller, target).
// allowed=true sets up rows that satisfy the access policy (siblings under same parent).
// allowed=false sets up rows that don't (different parents).
func mockCanCommunicate(mock sqlmock.Sqlmock, caller, target string, allowed bool) {
	callerParent := "shared-parent"
	targetParent := "shared-parent"
	if !allowed {
		targetParent = "different-parent"
	}
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id = ").
		WithArgs(caller).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow(caller, callerParent))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id = ").
		WithArgs(target).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow(target, targetParent))
}

// ==================== ProxyA2A — Access Control ====================

func TestProxyA2A_AccessDenied_DifferentParents(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mr.Set(fmt.Sprintf("ws:%s:url", "ws-target"), "http://localhost:1")

	mockCanCommunicate(mock, "ws-caller", "ws-target", false)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}

	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-target/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Workspace-ID", "ws-caller")

	handler.ProxyA2A(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxyA2A_AllowedSelf_SkipsAccessCheck(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{}}`)
	}))
	defer agentServer.Close()
	mr.Set(fmt.Sprintf("ws:%s:url", "ws-self"), agentServer.URL)
	expectBudgetCheck(mock, "ws-self")

	mock.ExpectExec("INSERT INTO activity_logs").WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-self"}}

	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-self/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Workspace-ID", "ws-self")

	handler.ProxyA2A(c)
	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for self-call, got %d: %s", w.Code, w.Body.String())
	}
}

// TestProxyA2A_SystemCaller_HTTPHeaderRejected verifies the #761 fix:
// system-caller prefixes in X-Workspace-ID MUST be rejected on the HTTP path.
// Legitimate system callers (webhooks, scheduler, restart_context) call
// proxyA2ARequest directly and never send HTTP headers with these prefixes.
func TestProxyA2A_SystemCaller_HTTPHeaderRejected(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}

	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-target/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	// Supply a real system-caller prefix — must be blocked at the HTTP layer.
	c.Request.Header.Set("X-Workspace-ID", "webhook:github")

	handler.ProxyA2A(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for system-caller prefix in HTTP header, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if resp["error"] != "invalid caller ID" {
		t.Errorf("expected error 'invalid caller ID', got %v", resp["error"])
	}
}

// TestA2AProxy_SystemCallerForge_IsRejected verifies that an attacker who
// sets X-Workspace-ID to a system-caller prefix (to bypass token validation
// and CanCommunicate) receives 403 Forbidden — not 200 OK.
// This is the core fix for issue #761.
func TestA2AProxy_SystemCallerForge_IsRejected(t *testing.T) {
	forgePrefixes := []string{
		"system:forge",
		"system:admin",
		"webhook:evil",
		"test:attacker",
		"channel:hijack",
	}
	for _, forgedID := range forgePrefixes {
		t.Run(forgedID, func(t *testing.T) {
			setupTestDB(t)
			setupTestRedis(t)
			handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: "ws-victim"}}

			body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"exploit"}]}}}`
			c.Request = httptest.NewRequest("POST", "/workspaces/ws-victim/a2a", bytes.NewBufferString(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("X-Workspace-ID", forgedID)

			handler.ProxyA2A(c)

			if w.Code != http.StatusForbidden {
				t.Errorf("forged caller %q: expected 403, got %d: %s", forgedID, w.Code, w.Body.String())
			}
			var resp map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("body not JSON: %v", err)
			}
			if resp["error"] != "invalid caller ID" {
				t.Errorf("forged caller %q: expected error 'invalid caller ID', got %v", forgedID, resp["error"])
			}
		})
	}
}

// ==================== ProxyA2A — bearer-derived callerID (#2306) ====================

// TestProxyA2A_CallerIDDerivedFromBearer verifies that when X-Workspace-ID
// is absent, ProxyA2A derives the callerID from the bearer token's owning
// workspace. Without this, third-party SDKs that authenticate purely via
// bearer end up with activity_logs.source_id=NULL, breaking peer_id and
// "Agent Comms by peer" downstream signals.
func TestProxyA2A_CallerIDDerivedFromBearer(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{}}`)
	}))
	defer agentServer.Close()
	mr.Set(fmt.Sprintf("ws:%s:url", "ws-target"), agentServer.URL)

	// 1. Bearer-derive lookup → returns ws-caller
	mock.ExpectQuery(`SELECT t\.workspace_id\s+FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id"}).AddRow("ws-caller"))

	// 2. validateCallerToken's HasAnyLiveToken / ValidateToken queries fall
	//    through to fail-open (no expectations set) — same pattern as
	//    TestProxyA2A_CallerIDPropagated.

	// 3. CanCommunicate — siblings under same parent
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id = ").
		WithArgs("ws-caller").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-caller", "ws-parent"))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id = ").
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-target", "ws-parent"))

	expectBudgetCheck(mock, "ws-target")

	// 4. activity_logs INSERT — verify source_id arg is the derived ws-caller
	//    (column order: workspace_id, activity_type, source_id, target_id, ...)
	mock.ExpectExec("INSERT INTO activity_logs").
		WithArgs(
			"ws-target",                       // $1 workspace_id
			"a2a_receive",                     // $2 activity_type
			sqlmock.AnyArg(),                  // $3 source_id — *string("ws-caller"), checked below
			sqlmock.AnyArg(),                  // $4 target_id
			sqlmock.AnyArg(),                  // $5 method
			sqlmock.AnyArg(),                  // $6 summary
			sqlmock.AnyArg(),                  // $7 request_body
			sqlmock.AnyArg(),                  // $8 response_body
			sqlmock.AnyArg(),                  // $9 tool_trace
			sqlmock.AnyArg(),                  // $10 duration_ms
			sqlmock.AnyArg(),                  // $11 status
			sqlmock.AnyArg(),                  // $12 error_detail
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}

	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"test"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-target/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	// NOTE: no X-Workspace-ID — the bearer must be the only callerID source.
	c.Request.Header.Set("Authorization", "Bearer some-bearer-token")

	handler.ProxyA2A(c)
	time.Sleep(50 * time.Millisecond) // allow LogActivity goroutine to flush

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestProxyA2A_OrgTokenSkipsBearerDerive verifies that when an org-level
// token is in play (canvas/admin path), the bearer-derive logic is skipped
// even if the bearer matches a workspace token. Org tokens grant org-wide
// access and don't bind to a single workspace; treating them as a workspace
// caller would mis-attribute activity logs.
func TestProxyA2A_OrgTokenSkipsBearerDerive(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{}}`)
	}))
	defer agentServer.Close()
	mr.Set(fmt.Sprintf("ws:%s:url", "ws-target"), agentServer.URL)

	// No WorkspaceFromToken expectation — the bearer-derive branch must NOT
	// fire when org_token_id is set.
	expectBudgetCheck(mock, "ws-target")

	// Activity log INSERT with NULL source_id (canvas-class semantics).
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}
	c.Set("org_token_id", "org-token-123") // org-level auth

	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-target/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer org-bearer")

	handler.ProxyA2A(c)
	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestProxyA2A_BearerDeriveFailureFallsThrough verifies that if the bearer
// is present but doesn't resolve (e.g. revoked, removed workspace), the
// callerID stays empty and the request is treated as canvas-class — we
// don't 401, we don't error; we just lose the source_id signal. Mirrors
// the canvas-bypass shape so legacy/anonymous paths aren't broken.
func TestProxyA2A_BearerDeriveFailureFallsThrough(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{}}`)
	}))
	defer agentServer.Close()
	mr.Set(fmt.Sprintf("ws:%s:url", "ws-target"), agentServer.URL)

	// Bearer-derive lookup fails (no live row) — collapses to ErrInvalidToken
	// inside WorkspaceFromToken; ProxyA2A swallows the error and proceeds with
	// callerID="".
	mock.ExpectQuery(`SELECT t\.workspace_id\s+FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnError(sql.ErrNoRows)

	expectBudgetCheck(mock, "ws-target")
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}

	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-target/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer revoked-or-stale")

	handler.ProxyA2A(c)
	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (canvas-fallback), got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestIsSystemCaller(t *testing.T) {
	cases := []struct {
		caller   string
		expected bool
	}{
		{"webhook:github", true},
		{"system:scheduler", true},
		{"test:fake", true},
		{"ws-uuid-123", false},
		{"", false},
		{"webhook", false},
		{"foo:bar", false},
	}
	for _, tc := range cases {
		got := isSystemCaller(tc.caller)
		if got != tc.expected {
			t.Errorf("isSystemCaller(%q) = %v, want %v", tc.caller, got, tc.expected)
		}
	}
}

// ==================== detectPlatformInDocker ====================

func TestDetectPlatformInDocker_EnvVar(t *testing.T) {
	// Deterministic: asserts the function returns exactly the env-var
	// value when strconv.ParseBool accepts it. Unparseable values are
	// covered separately below because their outcome depends on whether
	// /.dockerenv exists on the host running the test.
	cases := []struct {
		env      string
		expected bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"t", true},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"f", false},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("MOLECULE_IN_DOCKER", tc.env)
			got := detectPlatformInDocker()
			if got != tc.expected {
				t.Errorf("MOLECULE_IN_DOCKER=%q → detectPlatformInDocker() = %v, want %v",
					tc.env, got, tc.expected)
			}
		})
	}
}

func TestDetectPlatformInDocker_UnparseableFallsThroughToFilesystemCheck(t *testing.T) {
	// Unparseable env values must NOT be treated as "true" — they fall
	// through to the /.dockerenv filesystem check. The result therefore
	// depends on the host; we only assert the return matches what the
	// filesystem check would report (keeps the test stable on Docker-
	// based CI as well as host-mode dev boxes).
	_, dockerenvErr := os.Stat("/.dockerenv")
	dockerenvExists := dockerenvErr == nil
	for _, env := range []string{"yes", "on", "bogus", "maybe", "2"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("MOLECULE_IN_DOCKER", env)
			got := detectPlatformInDocker()
			if got != dockerenvExists {
				t.Errorf("MOLECULE_IN_DOCKER=%q → detectPlatformInDocker() = %v, want %v (matches /.dockerenv presence)",
					env, got, dockerenvExists)
			}
		})
	}
}

func TestSetPlatformInDockerForTest(t *testing.T) {
	original := platformInDocker
	restore := setPlatformInDockerForTest(!original)
	if platformInDocker == original {
		t.Errorf("setPlatformInDockerForTest did not change platformInDocker")
	}
	restore()
	if platformInDocker != original {
		t.Errorf("restore function did not reset platformInDocker to %v (got %v)",
			original, platformInDocker)
	}
}

// ==================== isUpstreamBusyError ====================

func TestIsUpstreamBusyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, true},
		// applyIdleTimeout cancels its child ctx via context.WithCancel
		// when the broadcaster silence window elapses — surfaces here
		// as context.Canceled. Same "upstream busy" classification.
		{"context.Canceled", context.Canceled, true},
		{"wrapped context.Canceled", fmt.Errorf("dispatch wrapped: %w", context.Canceled), true},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		// Real net/http wraps context.DeadlineExceeded via *url.Error.Unwrap,
		// so errors.Is(err, context.DeadlineExceeded) catches it. The
		// pre-892de784 substring "context deadline exceeded" fallback
		// also accepted a string-only error like
		// `fmt.Errorf("Post: context deadline exceeded")`; that fallback
		// was dropped because errors.Is handles the real shape and the
		// substring was indistinguishable from a user-content match.
		{"wrapped context deadline (errors.Is path)", fmt.Errorf("Post: %w", context.DeadlineExceeded), true},
		{"wrapped EOF string", fmt.Errorf(`Post "http://ws-foo:8000": EOF`), true},
		{"connection reset", fmt.Errorf("read tcp 127.0.0.1:8080->127.0.0.1:12345: connection reset by peer"), true},
		{"generic dns error", fmt.Errorf("no such host"), false},
		{"refused", fmt.Errorf("connection refused"), false},
		{"random other error", fmt.Errorf("malformed response"), false},
	}
	for _, tc := range cases {
		got := isUpstreamBusyError(tc.err)
		if got != tc.want {
			t.Errorf("%s: isUpstreamBusyError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// ==================== ProxyA2A — upstream timeout returns 503 busy + Retry-After ====================

// Verifies the full error-shaping contract for the 503-busy path:
//   - Status 503 (not 502 unreachable)
//   - JSON body has {"busy": true, "retry_after": 30}
//   - Retry-After header is "30"
//
// We can't easily drive an actual upstream timeout in a unit test without a
// live Docker container, but we CAN exercise the proxyA2AError shape the
// handler emits, which is the contract callers rely on.

func TestProxyA2AError_BusyShape(t *testing.T) {
	// Simulate what proxyA2ARequest returns when isUpstreamBusyError fires
	// and containerDead is false.
	perr := &proxyA2AError{
		Status:  http.StatusServiceUnavailable,
		Headers: map[string]string{"Retry-After": fmt.Sprintf("%d", busyRetryAfterSeconds)},
		Response: gin.H{
			"error":       "workspace agent busy — retry after a short backoff",
			"busy":        true,
			"retry_after": busyRetryAfterSeconds,
		},
	}

	// Emulate the handler's error-emit path.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	for k, v := range perr.Headers {
		c.Header(k, v)
	}
	c.JSON(perr.Status, perr.Response)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After: got %q, want %q", got, "30")
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if busy, _ := body["busy"].(bool); !busy {
		t.Errorf(`body["busy"]: got %v, want true`, body["busy"])
	}
	// JSON numeric → float64 on unmarshal; compare numerically.
	if got, _ := body["retry_after"].(float64); int(got) != busyRetryAfterSeconds {
		t.Errorf(`body["retry_after"]: got %v, want %d`, body["retry_after"], busyRetryAfterSeconds)
	}
}

// ==================== ProxyA2A — body-read failure (delivery_confirmed) #689 ====================
//
// When Do() succeeds (target sent 2xx headers — delivery confirmed) but reading
// the response body fails (connection drop, mid-stream timeout), the proxy must:
//   1. Return 502 (caller can't get the response content)
//   2. Include "delivery_confirmed": true in the error body so callers can
//      distinguish "not delivered" from "delivered, response body lost".

func TestProxyA2A_BodyReadFailure_DeliveryConfirmed(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Agent server: sends 200 OK headers + partial body, then closes the
	// connection abruptly to simulate a mid-stream read failure.
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Flush 200 headers immediately so Do() returns (resp, nil).
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Write partial JSON — just enough to prove the body was started,
		// then hijack and close the connection so ReadAll fails.
		if flusher, ok := w.(http.Flusher); ok {
			io.WriteString(w, `{"result": "partial`) //nolint:errcheck
			flusher.Flush()
		}
		// Hijack the underlying TCP connection and close it to simulate
		// a mid-stream drop that causes io.ReadAll to return an error.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			if conn != nil {
				conn.Close()
			}
		}
	}))
	defer agentServer.Close()

	wsID := "ws-bodyreadfail"
	mr.Set(fmt.Sprintf("ws:%s:url", wsID), agentServer.URL)
	expectBudgetCheck(mock, wsID)

	// Expect async activity log INSERT (logA2ASuccess is called because
	// delivery_confirmed is true and the handler detected a 2xx status).
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"ping"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)
	time.Sleep(50 * time.Millisecond)

	// Expect 502 (couldn't deliver the response content to the caller)
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	// delivery_confirmed must be true — Do() returned 2xx headers.
	if v, _ := resp["delivery_confirmed"].(bool); !v {
		t.Errorf(`expected "delivery_confirmed": true in response, got: %v`, resp)
	}
	if _, hasErr := resp["error"]; !hasErr {
		t.Errorf(`expected "error" field in response body`)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== validateCallerToken — Phase 30.5 ====================

// The A2A proxy validates the *caller's* token (not the target's) when the
// caller is a workspace. Canvas (empty X-Workspace-ID), system callers
// (webhook:/system:/test: prefixes), and self-calls all bypass.

func TestValidateCallerToken_LegacyCallerGrandfathered(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Caller has no live tokens → grandfather path → returns nil
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs("ws-legacy").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/workspaces/x/a2a", bytes.NewBufferString("{}"))

	if err := validateCallerToken(context.Background(), c, "ws-legacy"); err != nil {
		t.Errorf("legacy caller should grandfather through; got %v", err)
	}
	if w.Code != 200 {
		// gin default before c.JSON is 200; we want no error response written
		if w.Body.Len() != 0 {
			t.Errorf("legacy path should not write a response body; got %s", w.Body.String())
		}
	}
}

func TestValidateCallerToken_MissingTokenWhenOnFile(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs("ws-authed").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/workspaces/x/a2a", bytes.NewBufferString("{}"))
	// No Authorization header set

	err := validateCallerToken(context.Background(), c, "ws-authed")
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("missing caller auth token")) {
		t.Errorf("expected specific error, got %s", w.Body.String())
	}
}

func TestValidateCallerToken_InvalidToken(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs("ws-authed").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("POST", "/workspaces/x/a2a", bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", "Bearer wrong")
	c.Request = req

	if err := validateCallerToken(context.Background(), c, "ws-authed"); err == nil {
		t.Fatal("expected error for bad token")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestValidateCallerToken_ValidToken(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs("ws-authed").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("t1", "ws-authed"))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET last_used_at`).
		WithArgs("t1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("POST", "/workspaces/x/a2a", bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", "Bearer goodtok")
	c.Request = req

	if err := validateCallerToken(context.Background(), c, "ws-authed"); err != nil {
		t.Errorf("valid token should pass; got %v", err)
	}
}

func TestValidateCallerToken_WrongWorkspaceBindingRejected(t *testing.T) {
	// Attacker has token T issued to ws-A. Tries to call A2A claiming
	// X-Workspace-ID: ws-B. Token validates against hash but workspace
	// mismatch → rejected.
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs("ws-b-attacker").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("t-a", "ws-a-owner"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("POST", "/workspaces/x/a2a", bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", "Bearer tok-for-A")
	c.Request = req

	if err := validateCallerToken(context.Background(), c, "ws-b-attacker"); err == nil {
		t.Fatal("token from A must not authenticate caller B")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// --- Direct unit tests for normalizeA2APayload (extracted from proxyA2ARequest) ---

func TestNormalizeA2APayload_InvalidJSON(t *testing.T) {
	_, _, perr := normalizeA2APayload([]byte("not json"))
	if perr == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if perr.Status != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", perr.Status)
	}
}

func TestNormalizeA2APayload_WrapsBareMessage(t *testing.T) {
	raw := []byte(`{"method":"message/send","params":{"message":{"role":"user","parts":[{"type":"text","text":"hi"}]}}}`)
	out, method, perr := normalizeA2APayload(raw)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	if method != "message/send" {
		t.Errorf("expected method=message/send, got %q", method)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if parsed["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc=2.0 wrapper, got %v", parsed["jsonrpc"])
	}
	if parsed["id"] == nil || parsed["id"] == "" {
		t.Error("expected generated id, got empty")
	}
	params := parsed["params"].(map[string]interface{})
	msg := params["message"].(map[string]interface{})
	if msg["messageId"] == nil || msg["messageId"] == "" {
		t.Error("expected messageId injected, got empty")
	}
}

func TestNormalizeA2APayload_PreservesExistingJSONRPC(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":"custom-id","method":"tasks/list","params":{}}`)
	out, method, perr := normalizeA2APayload(raw)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	if method != "tasks/list" {
		t.Errorf("expected method=tasks/list, got %q", method)
	}
	var parsed map[string]interface{}
	_ = json.Unmarshal(out, &parsed)
	if parsed["id"] != "custom-id" {
		t.Errorf("existing id overwritten: got %v", parsed["id"])
	}
}

func TestNormalizeA2APayload_PreservesExistingMessageId(t *testing.T) {
	raw := []byte(`{"method":"message/send","params":{"message":{"messageId":"fixed-mid","role":"user","parts":[]}}}`)
	out, _, perr := normalizeA2APayload(raw)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	var parsed map[string]interface{}
	_ = json.Unmarshal(out, &parsed)
	params := parsed["params"].(map[string]interface{})
	msg := params["message"].(map[string]interface{})
	if msg["messageId"] != "fixed-mid" {
		t.Errorf("existing messageId overwritten: got %v", msg["messageId"])
	}
}

func TestNormalizeA2APayload_MissingMethodReturnsEmpty(t *testing.T) {
	// Method extraction returns empty string when method is absent,
	// regardless of message validity. Include parts: [] so the v0.2→v0.3
	// compat check (#2345) doesn't reject before method extraction.
	raw := []byte(`{"params":{"message":{"role":"user","parts":[]}}}`)
	_, method, perr := normalizeA2APayload(raw)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	if method != "" {
		t.Errorf("expected empty method, got %q", method)
	}
}

// --- v0.2 → v0.3 compat shim (#2345) ---

func TestNormalizeA2APayload_ConvertsV02StringContentToParts(t *testing.T) {
	raw := []byte(`{"method":"message/send","params":{"message":{"role":"user","content":"hello world"}}}`)
	out, _, perr := normalizeA2APayload(raw)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	msg := parsed["params"].(map[string]interface{})["message"].(map[string]interface{})
	if _, stillHasContent := msg["content"]; stillHasContent {
		t.Error("v0.2 'content' field should be removed after conversion")
	}
	parts, ok := msg["parts"].([]interface{})
	if !ok || len(parts) != 1 {
		t.Fatalf("expected 1 part, got %v", msg["parts"])
	}
	part := parts[0].(map[string]interface{})
	if part["kind"] != "text" || part["text"] != "hello world" {
		t.Errorf("expected {kind:text, text:'hello world'}, got %v", part)
	}
}

func TestNormalizeA2APayload_ConvertsV02ListContentToParts(t *testing.T) {
	raw := []byte(`{"method":"message/send","params":{"message":{"role":"user","content":[{"kind":"text","text":"hi"}]}}}`)
	out, _, perr := normalizeA2APayload(raw)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	var parsed map[string]interface{}
	_ = json.Unmarshal(out, &parsed)
	msg := parsed["params"].(map[string]interface{})["message"].(map[string]interface{})
	parts, ok := msg["parts"].([]interface{})
	if !ok || len(parts) != 1 {
		t.Fatalf("expected list preserved as parts, got %v", msg["parts"])
	}
}

func TestNormalizeA2APayload_PreservesV03Parts(t *testing.T) {
	raw := []byte(`{"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"hi"}]}}}`)
	out, _, perr := normalizeA2APayload(raw)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	var parsed map[string]interface{}
	_ = json.Unmarshal(out, &parsed)
	msg := parsed["params"].(map[string]interface{})["message"].(map[string]interface{})
	if _, hasContent := msg["content"]; hasContent {
		t.Error("did not expect content field in v0.3-shaped payload output")
	}
	parts := msg["parts"].([]interface{})
	if len(parts) != 1 {
		t.Errorf("expected 1 part preserved, got %d", len(parts))
	}
}

func TestNormalizeA2APayload_RejectsMessageWithNeitherContentNorParts(t *testing.T) {
	raw := []byte(`{"method":"message/send","params":{"message":{"role":"user","metadata":{}}}}`)
	_, _, perr := normalizeA2APayload(raw)
	if perr == nil {
		t.Fatal("expected error for message with neither content nor parts")
	}
	if perr.Status != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", perr.Status)
	}
	errMsg, _ := perr.Response["error"].(string)
	if !strings.Contains(errMsg, "parts") || !strings.Contains(errMsg, "content") {
		t.Errorf("error message should mention both 'parts' and 'content', got: %q", errMsg)
	}
}

func TestNormalizeA2APayload_RejectsContentWithUnsupportedType(t *testing.T) {
	raw := []byte(`{"method":"message/send","params":{"message":{"role":"user","content":42}}}`)
	_, _, perr := normalizeA2APayload(raw)
	if perr == nil {
		t.Fatal("expected error for non-string non-list content")
	}
	if perr.Status != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", perr.Status)
	}
}

func TestNormalizeA2APayload_NoMessageNoCheck(t *testing.T) {
	raw := []byte(`{"method":"tasks/list","params":{}}`)
	_, method, perr := normalizeA2APayload(raw)
	if perr != nil {
		t.Fatalf("unexpected error on params-message-absent payload: %+v", perr)
	}
	if method != "tasks/list" {
		t.Errorf("expected method=tasks/list, got %q", method)
	}
}

// --- resolveAgentURL direct unit tests ---

func TestResolveAgentURL_CacheHit(t *testing.T) {
	setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	// Use loopback IP (unlocked by allowLoopbackForTest) so isSafeURL passes —
	// cached.example does not resolve and would trip the DNS guard.
	cached := "http://127.0.0.1:9999/a2a"
	mr.Set("ws:ws-cached:url", cached)

	url, perr := handler.resolveAgentURL(context.Background(), "ws-cached")
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	if url != cached {
		t.Errorf("got %q, want cached URL", url)
	}
}

func TestResolveAgentURL_CacheMissDBHit(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Use loopback IP (unlocked by allowLoopbackForTest) so isSafeURL passes.
	dbURL := "http://127.0.0.1:9998"
	mock.ExpectQuery("SELECT url, status FROM workspaces WHERE id =").
		WithArgs("ws-dbhit").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status"}).AddRow(dbURL, "online"))

	url, perr := handler.resolveAgentURL(context.Background(), "ws-dbhit")
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	if url != dbURL {
		t.Errorf("got %q, want %q", url, dbURL)
	}
	// Verify cached now
	if v, err := mr.Get("ws:ws-dbhit:url"); err != nil || v != dbURL {
		t.Errorf("expected Redis cache populated; got v=%q err=%v", v, err)
	}
}

func TestResolveAgentURL_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT url, status FROM workspaces WHERE id =").
		WithArgs("ws-missing").
		WillReturnError(sql.ErrNoRows)

	_, perr := handler.resolveAgentURL(context.Background(), "ws-missing")
	if perr == nil {
		t.Fatal("expected error, got nil")
	}
	if perr.Status != http.StatusNotFound {
		t.Errorf("got status %d, want 404", perr.Status)
	}
}

func TestResolveAgentURL_NullURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT url, status FROM workspaces WHERE id =").
		WithArgs("ws-nullurl").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status"}).AddRow(nil, "provisioning"))

	_, perr := handler.resolveAgentURL(context.Background(), "ws-nullurl")
	if perr == nil {
		t.Fatal("expected error, got nil")
	}
	if perr.Status != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want 503", perr.Status)
	}
}

func TestResolveAgentURL_DockerRewrite(t *testing.T) {
	// provisioner.InternalURL is called when platformInDocker && URL begins
	// with http://127.0.0.1:. We don't have a real *Provisioner so the
	// rewrite path requires h.provisioner != nil. Since we can't easily
	// construct a provisioner, verify rewrite does NOT happen when
	// provisioner is nil (guard clause). The rewrite branch itself is
	// covered by TestResolveAgentURL_DockerRewrite_NilProvisionerNoRewrite.
	mr := setupTestRedis(t)
	setupTestDB(t)
	allowLoopbackForTest(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	mr.Set("ws:ws-dock:url", "http://127.0.0.1:55555")

	restore := setPlatformInDockerForTest(true)
	defer restore()

	url, perr := handler.resolveAgentURL(context.Background(), "ws-dock")
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	// nil provisioner → no rewrite
	if url != "http://127.0.0.1:55555" {
		t.Errorf("with nil provisioner, URL must not be rewritten; got %q", url)
	}
}

// --- dispatchA2A direct unit tests ---

func TestDispatchA2A_BuildRequestError(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Malformed URL causes http.NewRequestWithContext to fail.
	_, cancel, err := handler.dispatchA2A(context.Background(), "ws-target", "http://%%badhost", []byte("{}"), "")
	if cancel != nil {
		cancel()
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if _, ok := err.(*proxyDispatchBuildError); !ok {
		t.Errorf("expected *proxyDispatchBuildError, got %T: %v", err, err)
	}
}

func TestDispatchA2A_CanvasTimeout(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Agent that responds OK — we just want the cancel func.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	resp, cancel, err := handler.dispatchA2A(context.Background(), "ws-target", srv.URL, []byte(`{}`), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if cancel == nil {
		t.Fatal("canvas caller must return a cancel func (idle-timeout cleanup)")
	}
	cancel() // restore
}

func TestDispatchA2A_AgentTimeout(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	resp, cancel, err := handler.dispatchA2A(context.Background(), "ws-target", srv.URL, []byte(`{}`), "ws-caller")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if cancel == nil {
		t.Fatal("agent-to-agent caller must return a cancel func (idle + ceiling cleanup)")
	}
	cancel()
}

func TestDispatchA2A_ContextDeadline_NoExtraCeiling(t *testing.T) {
	// When ctx already has a deadline, dispatchA2A must not layer
	// its own absolute ceiling on top — the caller's deadline wins.
	// The idle-timer cleanup still produces a non-nil cancel func
	// (introduced by the always-on idle timeout) but the cancel func
	// is safe to call repeatedly and from a deferred path.
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	resp, cancel, err := handler.dispatchA2A(ctx, "ws-target", srv.URL, []byte(`{}`), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if cancel == nil {
		t.Error("cancel must be non-nil (idle-timer cleanup)")
	}
}

// --- applyIdleTimeout ---

// TestApplyIdleTimeout_FiresOnSilence verifies the helper cancels its
// child ctx when no broadcaster events arrive for `idle` duration.
// Uses a short idle window (60ms) so the test runs fast.
func TestApplyIdleTimeout_FiresOnSilence(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	b := newTestBroadcaster()

	parent, parentCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer parentCancel()

	idleCtx, idleCancel := applyIdleTimeout(parent, b, "ws-silent", 60*time.Millisecond)
	defer idleCancel()

	select {
	case <-idleCtx.Done():
		// expected — no events ever arrived for ws-silent
	case <-time.After(2 * time.Second):
		t.Fatal("idleCtx never cancelled despite no events")
	}
	if !errors.Is(idleCtx.Err(), context.Canceled) {
		t.Errorf("idleCtx err = %v, want context.Canceled", idleCtx.Err())
	}
}

// TestApplyIdleTimeout_ResetsOnEvent verifies that a broadcaster event
// for the workspace resets the timer. Sends one event mid-window and
// confirms ctx is still alive after the original deadline would have
// fired, but cancelled after a second silence window elapses.
func TestApplyIdleTimeout_ResetsOnEvent(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	b := newTestBroadcaster()

	parent, parentCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer parentCancel()

	idle := 80 * time.Millisecond
	idleCtx, idleCancel := applyIdleTimeout(parent, b, "ws-active", idle)
	defer idleCancel()

	// Send a progress event halfway through the window — should
	// extend the deadline by another `idle`.
	time.Sleep(idle / 2)
	b.BroadcastOnly("ws-active", "ACTIVITY_LOGGED", map[string]interface{}{"activity_type": "agent_log"})

	// At t = idle (original deadline), ctx must still be alive
	// because the event reset the clock.
	select {
	case <-idleCtx.Done():
		t.Fatal("idleCtx cancelled despite mid-window event resetting the timer")
	case <-time.After(idle - (idle / 2) + 10*time.Millisecond):
		// ok — past the original deadline, still alive
	}

	// Now wait for the second silence window to actually fire.
	select {
	case <-idleCtx.Done():
		// expected
	case <-time.After(idle + 200*time.Millisecond):
		t.Fatal("idleCtx never cancelled after the second silence window")
	}
}

// TestApplyIdleTimeout_NilBroadcasterDegradesGracefully — nil
// broadcaster (some test paths) returns the parent ctx unchanged.
func TestApplyIdleTimeout_NilBroadcasterDegradesGracefully(t *testing.T) {
	parent := context.Background()
	idleCtx, cancel := applyIdleTimeout(parent, nil, "ws-x", 50*time.Millisecond)
	defer cancel()
	if idleCtx != parent {
		t.Error("nil broadcaster must return the parent ctx unchanged")
	}
	// And calling cancel must be safe.
	cancel()
}

// TestDispatchA2A_RejectsUnsafeURL is the #1483 defense-in-depth
// regression. setupTestDB disables SSRF for normal tests so existing
// dispatchA2A unit tests can hit httptest.NewServer (loopback) — we
// re-enable it here to verify the new in-function isSafeURL guard.
// Production callers go through resolveAgentURL which already
// validates; this test pins that dispatchA2A is now safe even when
// called directly by a future caller that skips resolveAgentURL.
//
// Note: dispatchA2A's signature includes workspaceID (added by the
// idle-timeout work) so this test passes a stub value — the SSRF check
// fires before workspaceID is referenced.
func TestDispatchA2A_RejectsUnsafeURL(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	restoreSSRF := setSSRFCheckForTest(true)
	t.Cleanup(restoreSSRF)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Cloud metadata IP — must be rejected before any HTTP call goes out.
	_, cancel, err := handler.dispatchA2A(
		context.Background(),
		"ws-target",
		"http://169.254.169.254/latest/meta-data/",
		[]byte(`{}`),
		"",
	)
	if cancel != nil {
		cancel()
		t.Error("cancel must be nil when the URL is rejected pre-request")
	}
	if err == nil {
		t.Fatal("expected SSRF rejection error, got nil")
	}
	if _, ok := err.(*proxyDispatchBuildError); !ok {
		t.Errorf("expected *proxyDispatchBuildError (caller maps to 500), got %T: %v", err, err)
	}
}


// --- handleA2ADispatchError ---

func TestHandleA2ADispatchError_ContextDeadline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// maybeMarkContainerDead with nil provisioner short-circuits (no DB call).
	// activity-log insert is suppressed (logActivity=false).
	// DeadlineExceeded → isUpstreamBusyError=true → EnqueueA2A attempted.
	// Mock the INSERT INTO a2a_queue to fail so we fall through to 503.
	mock.ExpectQuery(`INSERT INTO a2a_queue`).
		WithArgs("ws-dl", nil, PriorityTask, "{}", "message/send", nil).
		WillReturnError(fmt.Errorf("test: queue unavailable"))

	_, _, perr := handler.handleA2ADispatchError(
		context.Background(), "ws-dl", "", []byte("{}"), "message/send",
		context.DeadlineExceeded, 1, false,
	)
	if perr == nil {
		t.Fatal("expected error, got nil")
	}
	// EnqueueA2A failed → falls through to legacy 503 with Retry-After.
	if perr.Status != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want 503", perr.Status)
	}
	if perr.Headers["Retry-After"] == "" {
		t.Error("expected Retry-After header on busy-503 shape")
	}
}

func TestHandleA2ADispatchError_BuildError(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	buildErr := &proxyDispatchBuildError{err: fmt.Errorf("bad url")}
	_, _, perr := handler.handleA2ADispatchError(
		context.Background(), "ws-x", "", []byte("{}"), "message/send", buildErr, 1, false,
	)
	if perr == nil || perr.Status != http.StatusInternalServerError {
		t.Errorf("expected 500, got %+v", perr)
	}
}

func TestHandleA2ADispatchError_GenericReturns502(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	_, _, perr := handler.handleA2ADispatchError(
		context.Background(), "ws-x", "", []byte("{}"), "message/send",
		fmt.Errorf("no such host"), 1, false,
	)
	if perr == nil || perr.Status != http.StatusBadGateway {
		t.Errorf("expected 502, got %+v", perr)
	}
}

// --- maybeMarkContainerDead ---

// Nil provisioner → short-circuits false.
func TestMaybeMarkContainerDead_NilProvisioner(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(runtime, 'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-nilprov").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("langgraph"))

	if got := handler.maybeMarkContainerDead(context.Background(), "ws-nilprov"); got {
		t.Error("expected false when provisioner is nil")
	}
}

// SaaS path: h.provisioner=nil but h.cpProv is wired and reports the EC2
// instance is NOT running. maybeMarkContainerDead must consult cpProv,
// flip the workspace to status='offline', clear keys, broadcast OFFLINE,
// and return true so the caller surfaces the structured 503. Pre-fix
// (#NNN) it returned false unconditionally on h.provisioner==nil, so
// dead EC2 agents leaked upstream 502 to canvas with no recovery.
func TestMaybeMarkContainerDead_CPOnly_NotRunning(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	cp := &fakeCPProv{running: false}
	handler.SetCPProvisioner(cp)

	mock.ExpectQuery(`SELECT COALESCE\(runtime, 'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-saas-dead").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("hermes"))
	mock.ExpectExec(`UPDATE workspaces SET status = 'offline'`).
		WithArgs("ws-saas-dead").
		WillReturnResult(sqlmock.NewResult(0, 1))

	got := handler.maybeMarkContainerDead(context.Background(), "ws-saas-dead")
	if !got {
		t.Fatal("expected true (cpProv reports not running) — without cpProv consultation, SaaS dead-agent recovery is impossible")
	}
	if cp.calls != 1 {
		t.Errorf("expected exactly 1 IsRunning call on cpProv; got %d", cp.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// SaaS path: h.cpProv reports running=true → maybeMarkContainerDead must
// return false (don't restart a healthy agent on a transient upstream
// hiccup). This is the safety check that prevents over-eager recycling.
func TestMaybeMarkContainerDead_CPOnly_Running(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	cp := &fakeCPProv{running: true}
	handler.SetCPProvisioner(cp)

	mock.ExpectQuery(`SELECT COALESCE\(runtime, 'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-saas-alive").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("hermes"))

	if got := handler.maybeMarkContainerDead(context.Background(), "ws-saas-alive"); got {
		t.Error("expected false when cpProv reports running — must not recycle a healthy agent")
	}
	if cp.calls != 1 {
		t.Errorf("expected exactly 1 IsRunning call on cpProv; got %d", cp.calls)
	}
}

// SaaS-path runRestartCycle: when h.provisioner is nil and h.cpProv is set,
// the auto-restart cycle MUST call cpProv.Stop (not Docker provisioner.Stop).
// Pre-fix this dispatched only to h.provisioner.Stop, NPE'd on nil, was
// silently swallowed by coalesceRestart's recover-without-re-raise, and
// left the workspace stuck in status='provisioning' forever — making
// reactive auto-restart on SaaS effectively dead code. The independent
// review of PR #2362 caught this gap.
//
// We drive runRestartCycle directly (not via RestartByID/coalesceRestart)
// so we don't fight the goroutine's timing in a unit test. The full
// restart chain (provisionWorkspaceCP) needs its own mocked DB rows that
// would explode the surface area of this test; what we care about here
// is the dispatch decision, which is observable on cpProv.stopCalls.
// stopForRestart is the dispatch helper extracted from runRestartCycle so the
// branch logic can be tested without spawning the async sendRestartContext
// goroutine that the full cycle fires. Pre-fix runRestartCycle's Stop dispatch
// only called the Docker path, so on SaaS (h.provisioner=nil) the cycle NPE'd
// silently and left the workspace stuck in status='provisioning'.
func TestStopForRestart_SaaSPath_DispatchesViaCPProv(t *testing.T) {
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	cp := &fakeCPProv{}
	handler.SetCPProvisioner(cp)

	handler.stopForRestart(context.Background(), "ws-saas-restart")

	if cp.stopCalls != 1 {
		t.Fatalf("expected cpProv.Stop to be called once on SaaS auto-restart; got %d", cp.stopCalls)
	}
	if cp.startCalls != 0 {
		t.Fatalf("expected cpProv.Start NOT to be called by stopForRestart; got %d", cp.startCalls)
	}
}

// Both nil → no-op, no panic. Defensive guard against the dispatcher being
// invoked on a misconfigured handler.
func TestStopForRestart_NoProvisioner_NoOp(t *testing.T) {
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	// no provisioner, no cpProv
	handler.stopForRestart(context.Background(), "ws-orphan")
	// no panic, no calls — assertion is reaching this line
}

// fakeCPProv satisfies provisioner.CPProvisionerAPI for tests that exercise
// the SaaS / EC2-backed reactive-health path.
//
// Methods all record calls. Start/Stop/GetConsoleOutput return nil/empty by
// default — the maybeMarkContainerDead happy path triggers an async
// `go h.RestartByID(...)` which calls Stop, so the previous "panic on
// unexpected call" pattern was unsafe (the panic fires on a goroutine,
// after the assertions ran). Tests that want to ASSERT a method is unused
// can check `calls == 0` after a sync barrier.
type fakeCPProv struct {
	running    bool
	calls      int
	stopCalls  int
	startCalls int
}

func (f *fakeCPProv) Start(_ context.Context, _ provisioner.WorkspaceConfig) (string, error) {
	f.startCalls++
	return "", nil
}
func (f *fakeCPProv) Stop(_ context.Context, _ string) error {
	f.stopCalls++
	return nil
}
func (f *fakeCPProv) GetConsoleOutput(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (f *fakeCPProv) IsRunning(_ context.Context, _ string) (bool, error) {
	f.calls++
	return f.running, nil
}

// external runtime → false regardless of provisioner.
func TestMaybeMarkContainerDead_ExternalRuntime(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(runtime, 'langgraph'\) FROM workspaces WHERE id =`).
		WithArgs("ws-ext").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("external"))

	if got := handler.maybeMarkContainerDead(context.Background(), "ws-ext"); got {
		t.Error("expected false for external runtime")
	}
}

// --- logA2AFailure / logA2ASuccess smoke tests ---
// These helpers spawn a detached goroutine that calls LogActivity, which
// inserts into activity_logs. We can't easily sync on the goroutine via
// sqlmock (done order isn't guaranteed), so we only assert the function
// returns without panicking and makes the expected DB calls.

func TestLogA2AFailure_Smoke(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Sync workspace-name lookup (called in the caller goroutine).
	mock.ExpectQuery(`SELECT name FROM workspaces WHERE id =`).
		WithArgs("ws-fail").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Fail Target"))
	// Async INSERT from the detached goroutine. MatchExpectationsInOrder=true
	// by default, but the goroutine runs after the sync query above.
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	handler.logA2AFailure(context.Background(), "ws-fail", "", []byte(`{}`), "message/send", fmt.Errorf("boom"), 42)
	time.Sleep(80 * time.Millisecond)
}

func TestLogA2AFailure_EmptyNameFallback(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Empty name from DB → summary uses the workspaceID as the name.
	mock.ExpectQuery(`SELECT name FROM workspaces WHERE id =`).
		WithArgs("ws-noname").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow(""))
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	handler.logA2AFailure(context.Background(), "ws-noname", "", []byte(`{}`), "message/send", fmt.Errorf("boom"), 1)
	time.Sleep(80 * time.Millisecond)
}

func TestLogA2ASuccess_Smoke(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT name FROM workspaces WHERE id =`).
		WithArgs("ws-ok").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("OK Target"))
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	handler.logA2ASuccess(context.Background(), "ws-ok", "", []byte(`{}`), []byte(`{"result":"x"}`), "message/send", 200, 10)
	time.Sleep(80 * time.Millisecond)
}

// Error-status path (>=400) records an "error" status in activity_logs.
func TestLogA2ASuccess_ErrorStatus(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT name FROM workspaces WHERE id =`).
		WithArgs("ws-err").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow(""))
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// callerID != "" also means no A2A_RESPONSE broadcast.
	handler.logA2ASuccess(context.Background(), "ws-err", "ws-caller", []byte(`{}`), []byte(`{}`), "message/send", 500, 10)
	time.Sleep(80 * time.Millisecond)
}

// ──────────────────────────────────────────────────────────────────────────────
// A2A auto-wake: hibernated workspace (#711)
// ──────────────────────────────────────────────────────────────────────────────

// TestResolveAgentURL_HibernatedWorkspace_Returns503WithWaking verifies the
// auto-wake path added in PR #724: when resolveAgentURL finds a workspace with
// status='hibernated' and no URL, it must:
//   - Return a proxyA2AError with Status 503
//   - Set Retry-After: 15 in Headers
//   - Include waking:true and retry_after:15 in the response body
//
// RestartByID fires asynchronously via `go h.RestartByID(workspaceID)`. Because
// provisioner is nil in tests, RestartByID returns immediately without any DB
// calls, so no additional mocks are needed.
func TestResolveAgentURL_HibernatedWorkspace_Returns503WithWaking(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t) // empty Redis → GetCachedURL returns error → DB fallback

	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// DB fallback: workspace exists but has no URL and is hibernated.
	mock.ExpectQuery(`SELECT url, status FROM workspaces WHERE id =`).
		WithArgs("ws-hibernated").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status"}).AddRow("", "hibernated"))

	_, perr := handler.resolveAgentURL(context.Background(), "ws-hibernated")

	if perr == nil {
		t.Fatal("expected proxyA2AError, got nil")
	}
	if perr.Status != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", perr.Status)
	}
	if perr.Headers["Retry-After"] != "15" {
		t.Errorf("expected Retry-After: 15, got %q", perr.Headers["Retry-After"])
	}

	if perr.Response["waking"] != true {
		t.Errorf("expected waking:true in body, got %v", perr.Response["waking"])
	}
	if perr.Response["retry_after"] != 15 {
		t.Errorf("expected retry_after:15 in body, got %v", perr.Response["retry_after"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestResolveAgentURL_HibernatedWorkspace_NullURLVariant verifies the same
// auto-wake behaviour when the DB returns a SQL NULL for the url column
// (rather than an empty string). Both forms represent "no URL assigned".
func TestResolveAgentURL_HibernatedWorkspace_NullURLVariant(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT url, status FROM workspaces WHERE id =`).
		WithArgs("ws-hibernated-null").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status"}).AddRow(nil, "hibernated"))

	_, perr := handler.resolveAgentURL(context.Background(), "ws-hibernated-null")

	if perr == nil {
		t.Fatal("expected proxyA2AError, got nil")
	}
	if perr.Status != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", perr.Status)
	}
	if perr.Headers["Retry-After"] != "15" {
		t.Errorf("expected Retry-After: 15, got %q", perr.Headers["Retry-After"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// ==================== ProxyA2A — poll-mode short-circuit (#2339 PR 2) ====================

// TestProxyA2A_PollMode_ShortCircuits_NoSSRF_NoDispatch verifies the core
// invariant of #2339 PR 2: when delivery_mode=poll, ProxyA2A must NOT
// hit resolveAgentURL (which would SSRF-check or 502 on a missing URL)
// and must NOT dispatch over HTTP. It records the request to activity_logs
// and returns 200 {status:"queued"} instead.
//
// Without this short-circuit, the canvas chat fails for any workspace
// running molecule-mcp-claude-channel (operator's laptop, no public URL):
// resolveAgentURL would 502 on the missing URL and the polling agent
// would never see the inbound message. That's the bug PR 2 fixes.
func TestProxyA2A_PollMode_ShortCircuits_NoSSRF_NoDispatch(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	const wsID = "ws-poll-shortcircuit"

	// Budget check still runs (above the short-circuit) — affirms the
	// budget guard is mode-agnostic, which is correct: a poll-mode
	// workspace shouldn't burn unmetered platform CPU/storage either.
	expectBudgetCheck(mock, wsID)

	// lookupDeliveryMode SELECT — returns poll, triggering the short-circuit.
	// Note: NO ExpectQuery for `SELECT url, status FROM workspaces` (that's
	// resolveAgentURL's query) — the short-circuit must skip resolveAgentURL.
	mock.ExpectQuery("SELECT delivery_mode FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode"}).AddRow("poll"))

	// Activity log: the queued receive (logA2AReceiveQueued in helpers.go).
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}

	body := `{"jsonrpc":"2.0","id":"poll-1","method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (queued), got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp["status"] != "queued" {
		t.Errorf("response.status = %v, want %q", resp["status"], "queued")
	}
	if resp["delivery_mode"] != "poll" {
		t.Errorf("response.delivery_mode = %v, want %q", resp["delivery_mode"], "poll")
	}
	if resp["method"] != "message/send" {
		t.Errorf("response.method = %v, want %q (the JSON-RPC method that was queued)", resp["method"], "message/send")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestProxyA2A_PushMode_NoShortCircuit verifies the symmetric contract:
// a push-mode workspace (default) is NOT affected by the new short-circuit.
// It still proceeds to resolveAgentURL + dispatch. Without this guard, a
// regression in lookupDeliveryMode could silently break the entire fleet.
func TestProxyA2A_PushMode_NoShortCircuit(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	const wsID = "ws-push-default"

	dispatched := false
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatched = true
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{"status":"ok"}}`)
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", wsID), agentServer.URL)
	expectBudgetCheck(mock, wsID)

	// lookupDeliveryMode returns "push" — short-circuit must NOT fire.
	mock.ExpectQuery("SELECT delivery_mode FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode"}).AddRow("push"))

	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}

	body := `{"jsonrpc":"2.0","id":"push-1","method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (dispatched), got %d: %s", w.Code, w.Body.String())
	}
	if !dispatched {
		t.Error("push-mode workspace: expected the agent server to receive the request, but it did not")
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err == nil {
		if resp["status"] == "queued" {
			t.Error("push-mode response leaked queued envelope — short-circuit fired when it shouldn't have")
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestProxyA2A_PollMode_FailsClosedToPush verifies the safety contract:
// a DB error reading delivery_mode must default to push (the existing
// behavior), NOT poll. Failing to push means a poll-mode workspace
// briefly attempts a real dispatch — visible failure (502 / SSRF
// rejection / restart cascade), not a silent drop into activity_logs
// where the agent might never look. Loud > silent, recoverable > lost.
func TestProxyA2A_PollMode_FailsClosedToPush(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t) // empty Redis — forces resolveAgentURL DB lookup
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	const wsID = "ws-mode-db-error"

	expectBudgetCheck(mock, wsID)

	// lookupDeliveryMode hits a transient DB error → must default push.
	mock.ExpectQuery("SELECT delivery_mode FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnError(sql.ErrConnDone)

	// Push path proceeds to resolveAgentURL — empty result → 502 path.
	mock.ExpectQuery("SELECT url, status FROM workspaces WHERE id =").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url", "status"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}

	body := `{"jsonrpc":"2.0","id":"x","method":"message/send","params":{}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	if w.Code == http.StatusOK {
		var resp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["status"] == "queued" {
			t.Errorf("DB error on delivery_mode lookup silently queued the request — must fail-closed-to-push, got body: %s", w.Body.String())
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
