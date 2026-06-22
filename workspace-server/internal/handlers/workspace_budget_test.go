package handlers

// Tests for per-workspace budget_limit field and A2A enforcement (#541).
//
// Coverage:
//   - GET /workspaces/:id includes budget_limit (nil when unset, int when set)
//   - GET /workspaces/:id includes monthly_spend
//   - POST /workspaces creates workspace with budget_limit
//   - PATCH /workspaces/:id updates budget_limit (nil clears the ceiling)
//   - A2A proxy returns 429 when monthly_spend >= budget_limit
//   - A2A proxy passes through when monthly_spend < budget_limit
//   - A2A proxy passes through when budget_limit is NULL (no limit)
//   - A2A proxy fail-open on DB error during budget check

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// wsColumns is the canonical column list for scanWorkspaceRow tests.
var wsColumns = []string{
	"id", "name", "role", "tier", "status", "agent_card", "url",
	"parent_id", "active_tasks", "max_concurrent_tasks", "last_error_rate", "last_sample_error",
	"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
	"budget_limit", "monthly_spend",
	"broadcast_enabled", "talk_to_user_enabled", "compute", "kind",
}

// ==================== GET — financial fields stripped from open endpoint ====================

// TestWorkspaceBudget_Get_NilLimit verifies that budget_limit and monthly_spend
// are NOT present in GET /workspaces/:id. The endpoint is on the open router —
// any caller with a valid UUID must not read billing data. (#611 Security Auditor)
func TestWorkspaceBudget_Get_NilLimit(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("dddddddd-0005-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows(wsColumns).
			AddRow("dddddddd-0005-0000-0000-000000000000", "Free Agent", "worker", 1, "online",
				[]byte(`{}`), "http://localhost:9001",
				nil, 0, 1, 0.0, "", 0, "", "claude-code", "",
				0.0, 0.0, false,
				nil,   // budget_limit NULL
				0,     // monthly_spend 0
				false, // broadcast_enabled
				true,  // talk_to_user_enabled
				[]byte(`{}`), "workspace"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "dddddddd-0005-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-nobudget", nil)
	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	// #611: financial fields must NOT appear on the open GET endpoint.
	if _, present := resp["budget_limit"]; present {
		t.Errorf("budget_limit must not appear in open GET /workspaces/:id response")
	}
	if _, present := resp["monthly_spend"]; present {
		t.Errorf("monthly_spend must not appear in open GET /workspaces/:id response")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestWorkspaceBudget_Get_WithLimit verifies that budget_limit and monthly_spend
// are stripped from the open GET /workspaces/:id even when the DB has non-zero
// values. Financial reads go through the AdminAuth-gated budget endpoint. (#611)
func TestWorkspaceBudget_Get_WithLimit(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("dddddddd-0006-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows(wsColumns).
			AddRow("dddddddd-0006-0000-0000-000000000000", "Capped Agent", "worker", 1, "online",
				[]byte(`{}`), "http://localhost:9002",
				nil, 0, 1, 0.0, "", 0, "", "claude-code", "",
				0.0, 0.0, false,
				int64(500),  // budget_limit = $5.00 in DB
				int64(123),  // monthly_spend = $1.23 in DB
				false, true, // broadcast_enabled, talk_to_user_enabled
				[]byte(`{}`), "workspace"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "dddddddd-0006-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-limited", nil)
	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	// #611: financial fields must NOT appear on the open GET endpoint even when
	// the DB has non-zero values — they're stripped before c.JSON().
	if _, present := resp["budget_limit"]; present {
		t.Errorf("budget_limit must not appear in open GET /workspaces/:id response (got %v)", resp["budget_limit"])
	}
	if _, present := resp["monthly_spend"]; present {
		t.Errorf("monthly_spend must not appear in open GET /workspaces/:id response (got %v)", resp["monthly_spend"])
	}
	// Confirm non-financial fields are still present.
	if resp["name"] != "Capped Agent" {
		t.Errorf("expected name 'Capped Agent', got %v", resp["name"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// ==================== POST — create with budget_limit ====================

// TestWorkspaceBudget_Create_WithLimit verifies that POST /workspaces with
// a budget_limit passes the value as the 10th INSERT parameter ($10).
func TestWorkspaceBudget_Create_WithLimit(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	budgetVal := int64(1000) // $10.00
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(
			sqlmock.AnyArg(),                 // id
			"Budgeted Agent",                 // name
			nil,                              // role
			3,                                // tier (default, workspace.go create-handler)
			"claude-code",                    // runtime
			"",                               // template
			(*string)(nil),                   // parent_id
			nil,                              // workspace_dir
			"none",                           // workspace_access
			&budgetVal,                       // budget_limit ($10)
			models.DefaultMaxConcurrentTasks, // max_concurrent_tasks default
			"push",                           // delivery_mode default (#2339)
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO workspace_secrets").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WithArgs(sqlmock.AnyArg(), float64(0), float64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"Budgeted Agent","model":"anthropic:claude-opus-4-7","budget_limit":1000}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// ==================== PATCH — budget_limit silently ignored on general update ====================

// TestWorkspaceBudget_Update_SetLimit verifies that PATCH /workspaces/:id with
// budget_limit=500 does NOT issue any DB write for budget_limit. The only write
// path is the AdminAuth-gated PATCH /workspaces/:id/budget endpoint. (#611)
func TestWorkspaceBudget_Update_SetLimit(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Only the existence probe fires; no UPDATE for budget_limit.
	mock.ExpectQuery("SELECT EXISTS.*workspaces WHERE id").
		WithArgs("dddddddd-0007-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// No ExpectExec for budget_limit — sqlmock will fail if one is issued.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "dddddddd-0007-0000-0000-000000000000"}}
	body := `{"budget_limit":500}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-upd-budget", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// If a budget_limit UPDATE was issued, sqlmock would have an unexpected call.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB activity — budget_limit must not be written via general Update: %v", err)
	}
}

// TestWorkspaceBudget_Update_ClearLimit verifies that PATCH /workspaces/:id
// with budget_limit=null does NOT issue any DB write for budget_limit. (#611)
func TestWorkspaceBudget_Update_ClearLimit(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Only the existence probe fires; no UPDATE for budget_limit.
	mock.ExpectQuery("SELECT EXISTS.*workspaces WHERE id").
		WithArgs("dddddddd-0008-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// No ExpectExec — a budget_limit write here would re-open the vulnerability.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "dddddddd-0008-0000-0000-000000000000"}}
	body := `{"budget_limit":null}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-clear-budget", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB activity — budget_limit must not be written via general Update: %v", err)
	}
}

// ==================== A2A enforcement ====================

// TestWorkspaceBudget_A2A_ExceededReturns402 verifies that the A2A proxy
// returns HTTP 402 {"error":"workspace budget limit exceeded"} when
// monthly_spend equals budget_limit.
func TestWorkspaceBudget_A2A_ExceededReturns402(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Cache a URL so resolveAgentURL doesn't need a DB query after budget check
	mr.Set(fmt.Sprintf("ws:%s:url", "ws-over-budget"), "http://localhost:9999")

	// Budget check: monthly limit 500, monthly spend 500 → exceeded → 402
	mock.ExpectQuery(`SELECT COALESCE\(budget_limits`).
		WithArgs("ws-over-budget").
		WillReturnRows(sqlmock.NewRows([]string{"budget_limits"}).AddRow([]byte(`{"monthly":500}`)))
	mock.ExpectQuery(`FROM workspace_spend_events`).
		WithArgs("ws-over-budget").
		WillReturnRows(spendRows(0, 0, 0, 500))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-over-budget"}}
	body := `{"message":{"role":"user","parts":[{"text":"hello"}]}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-over-budget/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.ProxyA2A(c)

	if w.Code != http.StatusPaymentRequired {
		t.Errorf("expected 402 when budget exceeded, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "workspace budget limit exceeded" {
		t.Errorf("expected 'workspace budget limit exceeded', got %v", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestWorkspaceBudget_A2A_AboveLimitReturns402 verifies 402 when spend > limit.
func TestWorkspaceBudget_A2A_AboveLimitReturns402(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mr.Set(fmt.Sprintf("ws:%s:url", "ws-way-over"), "http://localhost:9999")

	// spend > limit
	mock.ExpectQuery(`SELECT COALESCE\(budget_limits`).
		WithArgs("ws-way-over").
		WillReturnRows(sqlmock.NewRows([]string{"budget_limits"}).AddRow([]byte(`{"monthly":100}`)))
	mock.ExpectQuery(`FROM workspace_spend_events`).
		WithArgs("ws-way-over").
		WillReturnRows(spendRows(0, 0, 0, 9999))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-way-over"}}
	body := `{"message":{"role":"user","parts":[{"text":"test"}]}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-way-over/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.ProxyA2A(c)

	if w.Code != http.StatusPaymentRequired {
		t.Errorf("expected 402 when spend > limit, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestWorkspaceBudget_A2A_UnderLimitPassesThrough verifies that A2A calls
// succeed normally when monthly_spend is below budget_limit.
func TestWorkspaceBudget_A2A_UnderLimitPassesThrough(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Stand up a minimal mock agent that returns a valid A2A response
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{"status":"ok"}}`)
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", "ws-under-budget"), agentServer.URL)

	// Budget check: monthly spend (100) < limit (500) → pass-through
	mock.ExpectQuery(`SELECT COALESCE\(budget_limits`).
		WithArgs("ws-under-budget").
		WillReturnRows(sqlmock.NewRows([]string{"budget_limits"}).AddRow([]byte(`{"monthly":500}`)))
	mock.ExpectQuery(`FROM workspace_spend_events`).
		WithArgs("ws-under-budget").
		WillReturnRows(spendRows(0, 0, 0, 100))

	// Activity log INSERT from logA2ASuccess
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-under-budget"}}
	body := `{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hello"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-under-budget/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.ProxyA2A(c)

	// Give the async logA2ASuccess goroutine a moment to fire
	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when under budget, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestWorkspaceBudget_A2A_NilLimitPassesThrough verifies that when
// budget_limit IS NULL (no ceiling set), A2A calls pass through unconditionally.
func TestWorkspaceBudget_A2A_NilLimitPassesThrough(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"2","result":{"status":"ok"}}`)
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", "ws-no-limit"), agentServer.URL)

	// no limits configured → checkWorkspaceBudget returns early (no spend query),
	// enforcement skipped regardless of spend
	mock.ExpectQuery(`SELECT COALESCE\(budget_limits`).
		WithArgs("ws-no-limit").
		WillReturnRows(sqlmock.NewRows([]string{"budget_limits"}).AddRow([]byte(`{}`)))

	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-no-limit"}}
	body := `{"jsonrpc":"2.0","id":"2","method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-no-limit/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.ProxyA2A(c)

	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when no limit set, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestWorkspaceBudget_A2A_DBErrorFailOpen verifies that a DB error during the
// budget check is fail-open — the request proceeds rather than being blocked.
func TestWorkspaceBudget_A2A_DBErrorFailOpen(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"3","result":{"status":"ok"}}`)
	}))
	defer agentServer.Close()

	mr.Set(fmt.Sprintf("ws:%s:url", "ws-db-err-budget"), agentServer.URL)

	// Budget check fails with DB error → fail-open (request proceeds)
	mock.ExpectQuery(`SELECT COALESCE\(budget_limits`).
		WithArgs("ws-db-err-budget").
		WillReturnError(sql.ErrConnDone)

	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-db-err-budget"}}
	body := `{"jsonrpc":"2.0","id":"3","method":"message/send","params":{"message":{"role":"user","parts":[{"text":"fail-open test"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-db-err-budget/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.ProxyA2A(c)

	time.Sleep(50 * time.Millisecond)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 on DB error (fail-open), got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}
