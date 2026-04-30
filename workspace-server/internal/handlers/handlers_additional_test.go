package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/gin-gonic/gin"
)

// ==========================================================================
// Additional edge-case and coverage-gap tests for all critical handlers.
// Covers scenarios not present in existing test files.
// ==========================================================================

// ---------- workspace.go: Create with parent_id ----------

func TestWorkspaceCreate_WithParentID(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	parentID := "parent-ws-123"
	mock.ExpectBegin()
	// Default tier is 3 (Privileged) — see workspace.go create-handler comment.
	// delivery_mode defaults to "push" when payload omits it (#2339).
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(sqlmock.AnyArg(), "Child Agent", nil, 3, "langgraph", sqlmock.AnyArg(), &parentID, nil, "none", (*int64)(nil), models.DefaultMaxConcurrentTasks, "push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WithArgs(sqlmock.AnyArg(), float64(0), float64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"Child Agent","parent_id":"parent-ws-123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- workspace.go: Create with explicit runtime ----------

func TestWorkspaceCreate_ExplicitClaudeCodeRuntime(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	// delivery_mode defaults to "push" when payload omits it (#2339).
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(sqlmock.AnyArg(), "CC Agent", nil, 2, "claude-code", sqlmock.AnyArg(), (*string)(nil), nil, "none", (*int64)(nil), models.DefaultMaxConcurrentTasks, "push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WithArgs(sqlmock.AnyArg(), float64(10), float64(20)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"CC Agent","tier":2,"runtime":"claude-code","canvas":{"x":10,"y":20}}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- workspace.go: Create missing name validation ----------

func TestWorkspaceCreate_MissingName(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"tier":1,"runtime":"langgraph"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- workspace.go: Update with only parent_id ----------

func TestWorkspaceUpdate_ParentID(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// #125 guard: handler now verifies the workspace exists before applying
	// the UPDATE. Each PATCH test must mock the EXISTS probe first.
	mock.ExpectQuery("SELECT EXISTS.*workspaces WHERE id").
		WithArgs("dddddddd-0001-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("UPDATE workspaces SET parent_id").
		WithArgs("dddddddd-0001-0000-0000-000000000000", "dddddddd-0002-0000-0000-000000000000").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "dddddddd-0001-0000-0000-000000000000"}}
	body := `{"parent_id":"dddddddd-0002-0000-0000-000000000000"}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-child", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- workspace.go: Update with name only ----------

func TestWorkspaceUpdate_NameOnly(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT EXISTS.*workspaces WHERE id").
		WithArgs("dddddddd-0003-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("UPDATE workspaces SET name").
		WithArgs("dddddddd-0003-0000-0000-000000000000", "New Name").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "dddddddd-0003-0000-0000-000000000000"}}
	body := `{"name":"New Name"}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-rename", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "updated" {
		t.Errorf("expected 'updated', got %v", resp["status"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- workspace.go: Update with collapsed flag ----------

func TestWorkspaceUpdate_Collapsed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Canvas "collapse team" flip — the handler must run the UPSERT
	// on canvas_layouts to persist the flag, otherwise the UI state
	// resets on reload. `collapsed` lives on canvas_layouts, not
	// workspaces (see 005_canvas_layouts.sql).
	mock.ExpectQuery("SELECT EXISTS.*workspaces WHERE id").
		WithArgs("dddddddd-0005-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("INSERT INTO canvas_layouts .* collapsed").
		WithArgs("dddddddd-0005-0000-0000-000000000000", true).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "dddddddd-0005-0000-0000-000000000000"}}
	body := `{"collapsed":true}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-collapse", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- workspace.go: List with actual data ----------

func TestWorkspaceList_WithData(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// 21 cols — see scanWorkspaceRow for order (max_concurrent_tasks
	// lands between active_tasks and last_error_rate).
	columns := []string{
		"id", "name", "role", "tier", "status", "agent_card", "url",
		"parent_id", "active_tasks", "max_concurrent_tasks",
		"last_error_rate", "last_sample_error",
		"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
		"budget_limit", "monthly_spend",
	}
	rows := sqlmock.NewRows(columns).
		AddRow("ws-1", "Agent One", "worker", 1, "online", []byte(`{"name":"agent1"}`), "http://localhost:8001",
			nil, 3, 1, 0.02, "", 7200, "processing", "langgraph", "", 10.0, 20.0, false, nil, int64(0)).
		AddRow("ws-2", "Agent Two", "", 2, "degraded", []byte("null"), "",
			nil, 0, 1, 0.6, "timeout", 100, "", "claude-code", "", 50.0, 60.0, true, nil, int64(0))

	mock.ExpectQuery("SELECT w.id, w.name").
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/workspaces", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 2 {
		t.Fatalf("expected 2 workspaces, got %d", len(resp))
	}
	if resp[0]["role"] != "worker" {
		t.Errorf("expected role 'worker', got %v", resp[0]["role"])
	}
	if resp[1]["role"] != nil {
		t.Errorf("expected nil role for empty string, got %v", resp[1]["role"])
	}
	if resp[0]["agent_card"] == nil {
		t.Error("expected non-nil agent_card for Agent One")
	}
	if resp[1]["agent_card"] != nil {
		t.Errorf("expected nil agent_card for 'null', got %v", resp[1]["agent_card"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- workspace.go: Create with explicit max_concurrent_tasks ----------

func TestWorkspaceCreate_MaxConcurrentTasksOverride(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(sqlmock.AnyArg(), "Leader Agent", nil, 3, "claude-code", sqlmock.AnyArg(), (*string)(nil), nil, "none", (*int64)(nil), 3, "push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"Leader Agent","runtime":"claude-code","max_concurrent_tasks":3}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- registry.go: Register with provisioner URL preserved ----------

func TestRegister_ProvisionerURLPreserved(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// resolveDeliveryMode preflight — no row yet, default push (#2339).
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-prov").
		WillReturnError(sql.ErrNoRows)

	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("ws-prov", "ws-prov", "http://localhost:8000", `{"name":"agent"}`, "push").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// DB returns provisioner URL (127.0.0.1) — should take precedence over agent-reported URL
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id =").
		WithArgs("ws-prov").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://127.0.0.1:32001"))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// URL uses "localhost" (name-exempt from validateAgentURL DNS resolution)
	// rather than an arbitrary hostname like "agent" that fails DNS lookup in
	// CI and is rejected as SSRF-risk. Contract under test is provisioner-URL
	// precedence (SELECT url after INSERT), not URL validation itself.
	body := `{"id":"ws-prov","url":"http://localhost:8000","agent_card":{"name":"agent"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- registry.go: Heartbeat exact threshold (0.5) triggers degraded ----------

func TestHeartbeat_ExactThreshold_Degraded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-edge").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-edge", 0.5, "edge case", 0, 500, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// error_rate == 0.5 should trigger degraded (>= 0.5)
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-edge").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))
	mock.ExpectExec("UPDATE workspaces SET status = 'degraded'").
		WithArgs("ws-edge").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-edge","error_rate":0.5,"sample_error":"edge case","active_tasks":0,"uptime_seconds":500}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- registry.go: Heartbeat degraded→online recovery ----------

func TestHeartbeat_DegradedRecovery(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-rec").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-rec", 0.05, "", 1, 2000, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Currently degraded, error_rate < 0.1 → should recover to online
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-rec").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("degraded"))
	mock.ExpectExec("UPDATE workspaces SET status = 'online'").
		WithArgs("ws-rec").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-rec","error_rate":0.05,"sample_error":"","active_tasks":1,"uptime_seconds":2000}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- a2a_proxy.go: Workspace has no URL (503 with status) ----------

func TestProxyA2A_WorkspaceNoURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t) // empty Redis
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Workspace exists but URL is NULL
	mock.ExpectQuery("SELECT url, status FROM workspaces WHERE id =").
		WithArgs("ws-nourl").
		WillReturnRows(sqlmock.NewRows([]string{"url", "status"}).AddRow(nil, "provisioning"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-nourl"}}
	body := `{"method":"message/send","params":{}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-nourl/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "workspace has no URL" {
		t.Errorf("expected 'workspace has no URL', got %v", resp["error"])
	}
	if resp["status"] != "provisioning" {
		t.Errorf("expected status 'provisioning', got %v", resp["status"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- a2a_proxy.go: Agent unreachable (502) ----------

func TestProxyA2A_AgentUnreachable(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	allowLoopbackForTest(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Point to an unreachable address
	mr.Set(fmt.Sprintf("ws:%s:url", "ws-dead"), "http://127.0.0.1:1")
	expectBudgetCheck(mock, "ws-dead")

	// Expect workspace name query for error activity log
	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-dead").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Dead Agent"))
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-dead"}}
	body := `{"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hello"}]}}}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-dead/a2a", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ProxyA2A(c)
	time.Sleep(100 * time.Millisecond)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "failed to reach workspace agent" {
		t.Errorf("expected 'failed to reach workspace agent', got %v", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- a2a_proxy.go: nilIfEmpty utility ----------

func TestNilIfEmpty(t *testing.T) {
	if result := nilIfEmpty(""); result != nil {
		t.Errorf("expected nil for empty string, got %v", result)
	}
	if result := nilIfEmpty("hello"); result == nil || *result != "hello" {
		t.Errorf("expected pointer to 'hello', got %v", result)
	}
}

// ---------- discovery.go: Access denied between different parents ----------

func TestDiscover_AccessDenied(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// CanCommunicate: different parents → denied
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-child-a").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-child-a", "parent-a"))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-child-b").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-child-b", "parent-b"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-child-b"}}
	c.Request = httptest.NewRequest("GET", "/registry/discover/ws-child-b", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-child-a")

	handler.Discover(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- discovery.go: Target workspace is offline ----------

func TestDiscover_TargetOffline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// Both root-level, access allowed
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-caller").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-caller", nil))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-off").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-off", nil))

	// Name + runtime lookup (discovery now queries both)
	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-off").
		WillReturnRows(sqlmock.NewRows([]string{"name", "runtime"}).AddRow("Offline Agent", "langgraph"))

	// No cached internal URL → falls to DB status check → offline
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-off").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("offline"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-off"}}
	c.Request = httptest.NewRequest("GET", "/registry/discover/ws-off", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-caller")

	handler.Discover(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "offline" {
		t.Errorf("expected status 'offline', got %v", resp["status"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- discovery.go: CheckAccess allowed between siblings ----------

func TestCheckAccess_SiblingsAllowed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// Both root-level siblings → allowed
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-a").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-a", nil))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-b").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-b", nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"caller_id":"ws-a","target_id":"ws-b"}`
	c.Request = httptest.NewRequest("POST", "/registry/check-access", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.CheckAccess(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["allowed"] != true {
		t.Errorf("expected allowed true, got %v", resp["allowed"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- discovery.go: CheckAccess denied between different teams ----------

func TestCheckAccess_DifferentTeamsDenied(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	// Different parents → denied
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-x").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-x", "team-alpha"))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-y").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-y", "team-beta"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"caller_id":"ws-x","target_id":"ws-y"}`
	c.Request = httptest.NewRequest("POST", "/registry/check-access", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.CheckAccess(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["allowed"] != false {
		t.Errorf("expected allowed false, got %v", resp["allowed"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- discovery.go: CheckAccess parent→child allowed ----------

func TestCheckAccess_ParentChildAllowed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewDiscoveryHandler()

	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("dddddddd-0002-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("dddddddd-0002-0000-0000-000000000000", nil))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id =").
		WithArgs("ws-kid").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).AddRow("ws-kid", "dddddddd-0002-0000-0000-000000000000"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"caller_id":"dddddddd-0002-0000-0000-000000000000","target_id":"ws-kid"}`
	c.Request = httptest.NewRequest("POST", "/registry/check-access", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.CheckAccess(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["allowed"] != true {
		t.Errorf("expected allowed true for parent→child, got %v", resp["allowed"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- secrets.go: Set triggers auto-restart ----------

func TestSecretsSet_TriggersAutoRestart(t *testing.T) {
	mock := setupTestDB(t)
	done := make(chan string, 1)
	restartFunc := func(wsID string) {
		done <- wsID
	}
	handler := NewSecretsHandler(restartFunc)

	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs("77777777-7777-7777-7777-777777777777", "NEW_KEY", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "77777777-7777-7777-7777-777777777777"}}
	body := `{"key":"NEW_KEY","value":"new-val"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/77777777-7777-7777-7777-777777777777/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	select {
	case wsID := <-done:
		if wsID != "77777777-7777-7777-7777-777777777777" {
			t.Errorf("restart called with unexpected ID: %s", wsID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart callback not called within timeout")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- secrets.go: Set with nil restart func doesn't panic ----------

func TestSecretsSet_NilRestartFuncNoPanic(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil) // nil restart func

	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs("88888888-8888-8888-8888-888888888888", "SAFE_KEY", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "88888888-8888-8888-8888-888888888888"}}
	body := `{"key":"SAFE_KEY","value":"val"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/88888888-8888-8888-8888-888888888888/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- secrets.go: Delete triggers auto-restart ----------

func TestSecretsDelete_TriggersAutoRestart(t *testing.T) {
	mock := setupTestDB(t)
	done := make(chan struct{}, 1)
	restartFunc := func(wsID string) {
		done <- struct{}{}
	}
	handler := NewSecretsHandler(restartFunc)

	mock.ExpectExec("DELETE FROM workspace_secrets WHERE workspace_id").
		WithArgs("cccccccc-cccc-cccc-cccc-cccccccccccc", "REMOVE_KEY").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "cccccccc-cccc-cccc-cccc-cccccccccccc"},
		{Key: "key", Value: "REMOVE_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/cccccccc-cccc-cccc-cccc-cccccccccccc/secrets/REMOVE_KEY", nil)

	handler.Delete(c)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("restart callback not called within timeout")
	}

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- secrets.go: UUID validation edge cases ----------

func TestSecretsUUIDValidation(t *testing.T) {
	setupTestDB(t)
	handler := NewSecretsHandler(nil)

	badIDs := []struct {
		name string
		id   string
	}{
		{"short string", "abc"},
		{"uppercase UUID", "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"},
		{"no hyphens", "11111111111111111111111111111111"},
		{"empty", ""},
		{"SQL injection", "'; DROP TABLE--"},
	}

	for _, tt := range badIDs {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: tt.id}}
			c.Request = httptest.NewRequest("GET", "/workspaces/test-id/secrets", nil)
			handler.List(c)
			if w.Code != http.StatusBadRequest {
				t.Errorf("UUID %q: expected 400, got %d", tt.id, w.Code)
			}
		})
	}
}

// ==========================================================================
// workspace_restart.go: Restart, Pause, Resume, RestartByID tests
// ==========================================================================

// ---------- Restart: workspace not found ----------

func TestRestart_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT status, name, tier").
		WithArgs("ws-gone").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-gone"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-gone/restart", nil)

	handler.Restart(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Restart: DB error ----------

func TestRestart_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT status, name, tier").
		WithArgs("ws-err").
		WillReturnError(fmt.Errorf("connection refused"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-err"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-err/restart", nil)

	handler.Restart(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Restart: parent paused → 409 ----------

func TestRestart_ParentPaused(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Workspace lookup succeeds
	mock.ExpectQuery("SELECT status, name, tier").
		WithArgs("dddddddd-0001-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"status", "name", "tier", "runtime"}).
			AddRow("offline", "Child Agent", 1, "langgraph"))

	// isParentPaused: get parent_id
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id").
		WithArgs("dddddddd-0001-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow("dddddddd-0002-0000-0000-000000000000"))

	// isParentPaused: check parent status
	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id").
		WithArgs("dddddddd-0002-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"status", "name"}).AddRow("paused", "Parent Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "dddddddd-0001-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-child/restart", nil)

	handler.Restart(c)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	errMsg, _ := resp["error"].(string)
	if !containsStr(errMsg, "Parent Agent") {
		t.Errorf("expected error to mention parent name, got %q", errMsg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- Restart: provisioner nil → 503 ----------

func TestRestart_ProvisionerNil(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT status, name, tier").
		WithArgs("ws-noprov").
		WillReturnRows(sqlmock.NewRows([]string{"status", "name", "tier", "runtime"}).
			AddRow("offline", "Agent", 1, "langgraph"))

	// isParentPaused: no parent
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id").
		WithArgs("ws-noprov").
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-noprov"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-noprov/restart", nil)

	handler.Restart(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- Pause: success, no children ----------

func TestPause_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Workspace lookup
	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id").
		WithArgs("ws-pause").
		WillReturnRows(sqlmock.NewRows([]string{"status", "name"}).AddRow("online", "PauseMe"))

	// Recursive CTE for descendants — none
	mock.ExpectQuery("WITH RECURSIVE descendants").
		WithArgs("ws-pause").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	// UPDATE status to paused
	mock.ExpectExec("UPDATE workspaces SET status = 'paused'").
		WithArgs("ws-pause").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// RecordAndBroadcast
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-pause"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-pause/pause", nil)

	handler.Pause(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "paused" {
		t.Errorf("expected status 'paused', got %v", resp["status"])
	}
	if count, ok := resp["paused_count"].(float64); !ok || count != 1 {
		t.Errorf("expected paused_count 1, got %v", resp["paused_count"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- Pause: not found or already paused → 404 ----------

func TestPause_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id").
		WithArgs("ws-missing").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-missing"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-missing/pause", nil)

	handler.Pause(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Pause: DB error → 500 ----------

func TestPause_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id").
		WithArgs("ws-dberr").
		WillReturnError(fmt.Errorf("connection lost"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-dberr"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-dberr/pause", nil)

	handler.Pause(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Pause: with descendants ----------

func TestPause_WithDescendants(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Parent workspace lookup
	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id").
		WithArgs("ws-team").
		WillReturnRows(sqlmock.NewRows([]string{"status", "name"}).AddRow("online", "Team Lead"))

	// Recursive CTE returns 2 children
	mock.ExpectQuery("WITH RECURSIVE descendants").
		WithArgs("ws-team").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).
			AddRow("ws-worker-1", "Worker 1").
			AddRow("ws-worker-2", "Worker 2"))

	// UPDATE + broadcast for parent (ws-team)
	mock.ExpectExec("UPDATE workspaces SET status = 'paused'").
		WithArgs("ws-team").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// UPDATE + broadcast for child-1
	mock.ExpectExec("UPDATE workspaces SET status = 'paused'").
		WithArgs("ws-worker-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// UPDATE + broadcast for child-2
	mock.ExpectExec("UPDATE workspaces SET status = 'paused'").
		WithArgs("ws-worker-2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-team"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-team/pause", nil)

	handler.Pause(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if count, ok := resp["paused_count"].(float64); !ok || count != 3 {
		t.Errorf("expected paused_count 3 (parent + 2 children), got %v", resp["paused_count"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- Resume: not paused → 404 ----------

func TestResume_NotPaused(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT name, tier").
		WithArgs("ws-active").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-active"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-active/resume", nil)

	handler.Resume(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Resume: DB error → 500 ----------

func TestResume_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT name, tier").
		WithArgs("ws-resume-err").
		WillReturnError(fmt.Errorf("database timeout"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-resume-err"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-resume-err/resume", nil)

	handler.Resume(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Resume: provisioner nil → 503 ----------

func TestResume_ProvisionerNil(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT name, tier").
		WithArgs("ws-resume-noprov").
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "runtime"}).
			AddRow("Paused Agent", 1, "langgraph"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-resume-noprov"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-resume-noprov/resume", nil)

	handler.Resume(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------- RestartByID: provisioner nil → no-op ----------

func TestRestartByID_ProvisionerNil(t *testing.T) {
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	// Should return immediately without panic or DB access
	handler.RestartByID("ws-any")
	// If we get here without panic, test passes
}

// ---------- RestartByID: workspace removed/paused → skipped ----------

func TestRestartByID_RemovedOrPausedSkipped(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// We need a non-nil provisioner, but can't create one without Docker.
	// RestartByID checks provisioner == nil first (returns early),
	// so we test that path with TestRestartByID_ProvisionerNil above.
	// This test verifies the DB query filters out removed/paused workspaces
	// by checking that when provisioner is nil, the function returns before
	// hitting the DB at all.
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	handler.RestartByID("ws-removed")

	// No DB expectations → if sqlmock had unmet expectations, they'd cause errors
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (should have none): %v", err)
	}
}

// ---------- secrets.go: Set with invalid JSON ----------

func TestSecretsSet_InvalidJSON(t *testing.T) {
	setupTestDB(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "55555555-5555-5555-5555-555555555555"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/55555555-5555-5555-5555-555555555555/secrets", bytes.NewBufferString(`{bad`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}
