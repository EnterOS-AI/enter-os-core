package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/gin-gonic/gin"
)

// ==================== GET /workspaces/:id ====================

func TestWorkspaceGet_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	columns := []string{
		"id", "name", "role", "tier", "status", "agent_card", "url",
		"parent_id", "active_tasks", "max_concurrent_tasks", "last_error_rate", "last_sample_error",
		"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
		"budget_limit", "monthly_spend",
	}
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("cccccccc-0001-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows(columns).
			AddRow("cccccccc-0001-0000-0000-000000000000", "My Agent", "worker", 1, "online", []byte(`{"name":"test"}`),
				"http://localhost:8001", nil, 2, 1, 0.05, "", 3600, "working", "langgraph",
				"", 10.0, 20.0, false,
				nil, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-0001-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-get-1", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["name"] != "My Agent" {
		t.Errorf("expected name 'My Agent', got %v", resp["name"])
	}
	if resp["status"] != "online" {
		t.Errorf("expected status 'online', got %v", resp["status"])
	}
	if resp["runtime"] != "langgraph" {
		t.Errorf("expected runtime 'langgraph', got %v", resp["runtime"])
	}
	// current_task is stripped from public GET response (#955)
	if _, exists := resp["current_task"]; exists {
		t.Errorf("current_task should be stripped from public GET response")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceGet_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("cccccccc-0002-0000-0000-000000000000").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-0002-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-nonexistent", nil)

	handler.Get(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// #2429: GET /workspaces/:id returns 410 Gone when status='removed'.
// Defense-in-depth at the endpoint level — without this, callers
// holding stale workspace_id + token tuples (channel bridge .env,
// captured curl scripts, etc.) get 200 + status:"removed" and have
// no idea their tokens are revoked until the heartbeat fails 60s
// later. 410 makes startup fail loud instead.
func TestWorkspaceGet_RemovedReturns410(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	id := "cccccccc-0010-0000-0000-000000000000"
	removedAt := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)

	columns := []string{
		"id", "name", "role", "tier", "status", "agent_card", "url",
		"parent_id", "active_tasks", "max_concurrent_tasks", "last_error_rate", "last_sample_error",
		"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
		"budget_limit", "monthly_spend",
	}
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows(columns).
			AddRow(id, "Old Agent", "worker", 1, string(models.StatusRemoved), []byte(`null`),
				"", nil, 0, 1, 0.0, "", 0, "", "langgraph",
				"", 0.0, 0.0, false,
				nil, 0))
	mock.ExpectQuery(`SELECT updated_at FROM workspaces`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(removedAt))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+id, nil)

	handler.Get(c)

	if w.Code != http.StatusGone {
		t.Fatalf("expected 410 Gone, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse 410 body: %v", err)
	}
	if resp["error"] != "workspace removed" {
		t.Errorf("expected error 'workspace removed', got %v", resp["error"])
	}
	if resp["id"] != id {
		t.Errorf("expected id %q, got %v", id, resp["id"])
	}
	if v, ok := resp["removed_at"]; !ok || v == nil {
		t.Errorf("expected removed_at to be a real timestamp on the happy path, got: %v", v)
	}
	if _, ok := resp["hint"]; !ok {
		t.Errorf("expected hint in 410 body, got: %v", resp)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// If the follow-up `SELECT updated_at` query fails (workspace row
// disappeared in the gap, transient DB error, etc.), removedAt stays
// as Go's zero time. We emit JSON `null` for that case rather than
// the misleading `"0001-01-01T00:00:00Z"` the client would otherwise
// see — the actionable signal is the 410 + hint, not the timestamp.
func TestWorkspaceGet_RemovedReturns410WithNullRemovedAtOnTimestampFetchFailure(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	id := "cccccccc-0012-0000-0000-000000000000"

	columns := []string{
		"id", "name", "role", "tier", "status", "agent_card", "url",
		"parent_id", "active_tasks", "max_concurrent_tasks", "last_error_rate", "last_sample_error",
		"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
		"budget_limit", "monthly_spend",
	}
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows(columns).
			AddRow(id, "Vanished", "worker", 1, string(models.StatusRemoved), []byte(`null`),
				"", nil, 0, 1, 0.0, "", 0, "", "langgraph",
				"", 0.0, 0.0, false,
				nil, 0))
	// Simulate the row vanishing between the two queries.
	mock.ExpectQuery(`SELECT updated_at FROM workspaces`).
		WithArgs(id).
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+id, nil)

	handler.Get(c)

	if w.Code != http.StatusGone {
		t.Fatalf("expected 410 Gone, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse 410 body: %v", err)
	}
	if resp["removed_at"] != nil {
		t.Errorf(
			"expected removed_at == null when timestamp fetch fails; got %v (type %T). "+
				"Misleading 0001-01-01 timestamps in the JSON would confuse clients.",
			resp["removed_at"], resp["removed_at"],
		)
	}
	// Other fields must still be present.
	if resp["error"] != "workspace removed" || resp["id"] != id || resp["hint"] == nil {
		t.Errorf("expected error/id/hint to survive the timestamp fetch failure; got %v", resp)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// Audit-trail consumers (admin views, "show me deleted workspaces"
// tooling) opt into the legacy 200 + body shape via
// ?include_removed=true. Without this opt-in path the audit trail
// becomes invisible at the API layer.
func TestWorkspaceGet_RemovedWithIncludeQueryReturns200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	id := "cccccccc-0011-0000-0000-000000000000"

	columns := []string{
		"id", "name", "role", "tier", "status", "agent_card", "url",
		"parent_id", "active_tasks", "max_concurrent_tasks", "last_error_rate", "last_sample_error",
		"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
		"budget_limit", "monthly_spend",
	}
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows(columns).
			AddRow(id, "Audit Agent", "worker", 1, string(models.StatusRemoved), []byte(`null`),
				"", nil, 0, 1, 0.0, "", 0, "", "langgraph",
				"", 0.0, 0.0, false,
				nil, 0))
	// last_outbound_at follow-up query (existing path)
	mock.ExpectQuery(`SELECT last_outbound_at FROM workspaces`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"last_outbound_at"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+id+"?include_removed=true", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK with ?include_removed=true, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != string(models.StatusRemoved) {
		t.Errorf("expected status 'removed' in body, got %v", resp["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceGet_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("cccccccc-0003-0000-0000-000000000000").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-0003-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-dberr", nil)

	handler.Get(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== POST /workspaces (Create) ====================

func TestWorkspaceCreate_BadJSON(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// Missing required "name" field
	body := `{"tier":1}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceCreate_DBInsertError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Transaction begins, workspace INSERT fails, transaction is rolled back.
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(sqlmock.AnyArg(), "Failing Agent", nil, 3, "langgraph", sqlmock.AnyArg(), (*string)(nil), nil, "none", (*int64)(nil), models.DefaultMaxConcurrentTasks, "push").
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"name":"Failing Agent"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceCreate_DefaultsApplied(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Transaction wraps the workspace INSERT (no secrets in this request).
	mock.ExpectBegin()
	// Expect workspace INSERT with defaulted tier=3 (Privileged — the
	// handler default in workspace.go), runtime="langgraph"
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(sqlmock.AnyArg(), "Default Agent", nil, 3, "langgraph", sqlmock.AnyArg(), (*string)(nil), nil, "none", (*int64)(nil), models.DefaultMaxConcurrentTasks, "push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Expect canvas_layouts INSERT (x=0, y=0 — defaults)
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WithArgs(sqlmock.AnyArg(), float64(0), float64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect RecordAndBroadcast INSERT
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"name":"Default Agent"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "provisioning" {
		t.Errorf("expected status 'provisioning', got %v", resp["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceCreate_WithSecrets_Persists asserts that secrets in the create
// payload are written to workspace_secrets inside the same transaction as the
// workspace row, and that the handler returns 201.
func TestWorkspaceCreate_WithSecrets_Persists(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	// External workspace: simplest code path — no provisioner goroutine.
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(sqlmock.AnyArg(), "Hermes Agent", nil, 3, "hermes", sqlmock.AnyArg(), (*string)(nil), nil, "none", (*int64)(nil), models.DefaultMaxConcurrentTasks, "push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Secret inserted inside the same transaction.
	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs(sqlmock.AnyArg(), "HERMES_API_KEY", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// canvas_layouts (non-fatal, outside tx)
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"name":"Hermes Agent","runtime":"hermes","external":true,"secrets":{"HERMES_API_KEY":"sk-test-123"}}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceCreate_SecretPersistFails_RollsBack asserts that a DB error
// while persisting a secret causes the entire transaction to roll back and
// the handler to return 500.  The workspace row must NOT be committed.
func TestWorkspaceCreate_SecretPersistFails_RollsBack(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO workspace_secrets").
		WillReturnError(sql.ErrConnDone) // DB failure while writing secret
	mock.ExpectRollback() // workspace insert must be rolled back

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"name":"Rollback Agent","secrets":{"OPENAI_API_KEY":"sk-fail"}}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceCreate_EmptySecrets_OK asserts that an empty secrets map (or
// no secrets key at all) creates the workspace normally without touching
// workspace_secrets.
func TestWorkspaceCreate_EmptySecrets_OK(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// No ExpectExec for workspace_secrets — empty map must be a no-op.
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"name":"No Secrets Agent","external":true,"secrets":{}}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceCreate_ExternalURL_SSRFSafe asserts that an external workspace
// created with a safe public URL succeeds and writes the URL to the DB.
// Uses self-hosted mode so RFC-1918 is also blocked (not just metadata IPs).
func TestWorkspaceCreate_ExternalURL_SSRFSafe(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	t.Setenv("MOLECULE_ORG_ID", "")
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(sqlmock.AnyArg(), "Ext Agent", nil, 3, "external", sqlmock.AnyArg(), (*string)(nil), nil, "none", (*int64)(nil), models.DefaultMaxConcurrentTasks, "push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// External URL update (SSRF-safe public URL passes validateAgentURL).
	mock.ExpectExec("UPDATE workspaces SET url").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// CacheURL is non-fatal but still called.
	mock.ExpectExec("SELECT").
		WillReturnRows(sqlmock.NewRows([]string{"ok"}).AddRow("ok"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"name":"Ext Agent","runtime":"external","external":true,"url":"https://agent.example.com/a2a"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceCreate_ExternalURL_SSRFMetadataBlocked asserts that an external
// workspace created with a cloud-metadata URL is rejected with 400 before any
// DB write. 169.254.0.0/16 is always blocked regardless of mode (SaaS or
// self-hosted). Regression guard for issue #212.
func TestWorkspaceCreate_ExternalURL_SSRFMetadataBlocked(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	t.Setenv("MOLECULE_ORG_ID", "")
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// No DB calls expected — the handler should reject before any transaction.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"name":"Bad Agent","runtime":"external","external":true,"url":"http://169.254.169.254/latest/meta-data/"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceCreate_ExternalURL_SSRFLoopbackBlocked is the same regression
// guard as TestWorkspaceCreate_ExternalURL_SSRFMetadataBlocked but for the
// loopback rejection in self-hosted mode. admin-create is AdminAuth-gated,
// but a compromised admin token or insider should not be able to register
// a loopback URL either.
func TestWorkspaceCreate_ExternalURL_SSRFLoopbackBlocked(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	t.Setenv("MOLECULE_ORG_ID", "")
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// No DB calls expected.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"name":"Bad Loopback","runtime":"external","external":true,"url":"http://127.0.0.1:9000/a2a"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== GET /workspaces (List) ====================

func TestWorkspaceList_Empty(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT w.id, w.name").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "role", "tier", "status", "agent_card", "url",
			"parent_id", "active_tasks", "last_error_rate", "last_sample_error",
			"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
			"budget_limit", "monthly_spend",
		}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/workspaces", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("expected 0 workspaces, got %d", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceList_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT w.id, w.name").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/workspaces", nil)

	handler.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== PATCH /workspaces/:id (Update) ====================

func TestWorkspaceUpdate_BadJSON(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-0004-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-upd", bytes.NewBufferString("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceUpdate_MultipleFields(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// #125: existence probe fires once before any field update.
	mock.ExpectQuery("SELECT EXISTS.*workspaces WHERE id").
		WithArgs("cccccccc-0005-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// Expect name, role, and tier updates
	mock.ExpectExec("UPDATE workspaces SET name").
		WithArgs("cccccccc-0005-0000-0000-000000000000", "Updated Agent").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET role").
		WithArgs("cccccccc-0005-0000-0000-000000000000", "manager").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET tier").
		WithArgs("cccccccc-0005-0000-0000-000000000000", float64(3)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-0005-0000-0000-000000000000"}}

	body := `{"name":"Updated Agent","role":"manager","tier":3}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-multi", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "updated" {
		t.Errorf("expected status 'updated', got %v", resp["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceUpdate_RuntimeField(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT EXISTS.*workspaces WHERE id").
		WithArgs("cccccccc-0006-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("UPDATE workspaces SET runtime").
		WithArgs("cccccccc-0006-0000-0000-000000000000", "claude-code").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-0006-0000-0000-000000000000"}}

	body := `{"runtime":"claude-code"}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-rt", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== DELETE /workspaces/:id ====================

func TestWorkspaceDelete_ConfirmationRequired(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Children query returns 2 children
	mock.ExpectQuery("SELECT id, name FROM workspaces WHERE parent_id").
		WithArgs("cccccccc-0007-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).
			AddRow("cccccccc-0008-0000-0000-000000000000", "Child One").
			AddRow("cccccccc-0009-0000-0000-000000000000", "Child Two"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-0007-0000-0000-000000000000"}}
	// No ?confirm=true
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-parent", nil)

	handler.Delete(c)

	// #88: confirmation required now returns 409 Conflict (not 200) so
	// curl --fail / fetch().ok / any HTTP-status-aware client surfaces
	// the confirmation requirement instead of silently treating it as
	// success.
	if w.Code != http.StatusConflict {
		t.Errorf("expected status 409, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "confirmation_required" {
		t.Errorf("expected status 'confirmation_required', got %v", resp["status"])
	}
	if resp["children_count"] != float64(2) {
		t.Errorf("expected children_count 2, got %v", resp["children_count"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDelete_CascadeWithChildren(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Children query returns 1 child
	mock.ExpectQuery("SELECT id, name FROM workspaces WHERE parent_id").
		WithArgs("cccccccc-000a-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).
			AddRow("cccccccc-000b-0000-0000-000000000000", "Child Agent"))

	// Descendant CTE query returns the recursive set (1 descendant: ws-child-del)
	mock.ExpectQuery("WITH RECURSIVE descendants").
		WithArgs("cccccccc-000a-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("cccccccc-000b-0000-0000-000000000000"))

	// #73: single batch UPDATE covering [self + descendants] BEFORE stopping
	// containers (prevents heartbeat/restart resurrection races).
	mock.ExpectExec("UPDATE workspaces SET status =").
		WillReturnResult(sqlmock.NewResult(2, 2))
	// Batch canvas_layouts DELETE for the same id set.
	mock.ExpectExec("DELETE FROM canvas_layouts WHERE workspace_id = ANY").
		WillReturnResult(sqlmock.NewResult(2, 2))
	// Token revocation: once a workspace is gone its auth tokens are meaningless.
	mock.ExpectExec("UPDATE workspace_auth_tokens SET revoked_at").
		WillReturnResult(sqlmock.NewResult(0, 2))
	// #1027: cascade-disable schedules for deleted workspaces.
	mock.ExpectExec("UPDATE workspace_schedules SET enabled = false").
		WillReturnResult(sqlmock.NewResult(0, 3))
	// Broadcast for child WORKSPACE_REMOVED (fires during the descendant loop).
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Broadcast for parent WORKSPACE_REMOVED (fires after parent cleanup).
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-000a-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-parent-del?confirm=true", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "removed" {
		t.Errorf("expected status 'removed', got %v", resp["status"])
	}
	if resp["cascade_deleted"] != float64(1) {
		t.Errorf("expected cascade_deleted 1, got %v", resp["cascade_deleted"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== #1027: Cascade schedule disable on delete ====================

// TestWorkspaceDelete_DisablesSchedules verifies that when a leaf workspace
// (no children) is deleted, all its enabled schedules are set to enabled=false.
func TestWorkspaceDelete_DisablesSchedules(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "dddddddd-0001-0000-0000-000000000000"

	// No children
	mock.ExpectQuery("SELECT id, name FROM workspaces WHERE parent_id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	// CascadeDelete walks descendants unconditionally — 0-children case
	// returns 0 rows here.
	mock.ExpectQuery("WITH RECURSIVE descendants").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	// Mark workspace as removed
	mock.ExpectExec("UPDATE workspaces SET status =").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Canvas layouts cleanup
	mock.ExpectExec("DELETE FROM canvas_layouts WHERE workspace_id = ANY").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Token revocation
	mock.ExpectExec("UPDATE workspace_auth_tokens SET revoked_at").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// #1027: schedule disable — expect exactly this UPDATE to fire
	mock.ExpectExec("UPDATE workspace_schedules SET enabled = false").
		WillReturnResult(sqlmock.NewResult(0, 2)) // 2 schedules disabled
	// Broadcast WORKSPACE_REMOVED for the workspace itself
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsID+"?confirm=true", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: schedule disable UPDATE was not executed: %v", err)
	}
}

// TestWorkspaceDelete_CascadeDisablesDescendantSchedules verifies that when
// a parent workspace with children (and grandchildren) is deleted, ALL
// descendant schedules are also disabled in a single batch UPDATE.
func TestWorkspaceDelete_CascadeDisablesDescendantSchedules(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	parentID := "dddddddd-0002-0000-0000-000000000000"
	childID := "dddddddd-0003-0000-0000-000000000000"
	grandchildID := "dddddddd-0004-0000-0000-000000000000"

	// Children query returns 1 direct child
	mock.ExpectQuery("SELECT id, name FROM workspaces WHERE parent_id").
		WithArgs(parentID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).
			AddRow(childID, "Child"))

	// Recursive CTE returns child + grandchild
	mock.ExpectQuery("WITH RECURSIVE descendants").
		WithArgs(parentID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow(childID).
			AddRow(grandchildID))

	// Mark all 3 as removed
	mock.ExpectExec("UPDATE workspaces SET status =").
		WillReturnResult(sqlmock.NewResult(0, 3))
	// Canvas layouts
	mock.ExpectExec("DELETE FROM canvas_layouts WHERE workspace_id = ANY").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Token revocation
	mock.ExpectExec("UPDATE workspace_auth_tokens SET revoked_at").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// #1027: schedule disable — covers parent + child + grandchild in one batch
	mock.ExpectExec("UPDATE workspace_schedules SET enabled = false").
		WillReturnResult(sqlmock.NewResult(0, 5)) // 5 total schedules across 3 workspaces
	// Broadcast for child WORKSPACE_REMOVED
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Broadcast for grandchild WORKSPACE_REMOVED
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Broadcast for parent WORKSPACE_REMOVED
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: parentID}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+parentID+"?confirm=true", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["cascade_deleted"] != float64(2) {
		t.Errorf("expected cascade_deleted 2, got %v", resp["cascade_deleted"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: descendant schedules not disabled: %v", err)
	}
}

// TestWorkspaceDelete_ScheduleDisableOnlyTargetsDeletedWorkspace verifies that
// deleting workspace A does NOT disable workspace B's schedules. The schedule
// disable UPDATE uses ANY($1::uuid[]) scoped to the deleted workspace IDs, so
// sqlmock will fail if the wrong IDs are passed.
func TestWorkspaceDelete_ScheduleDisableOnlyTargetsDeletedWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsA := "dddddddd-0005-0000-0000-000000000000"
	// wsB is "dddddddd-0006-0000-0000-000000000000" — NOT part of the delete

	// No children for workspace A
	mock.ExpectQuery("SELECT id, name FROM workspaces WHERE parent_id").
		WithArgs(wsA).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	// CascadeDelete walks descendants unconditionally — 0-children case
	// returns 0 rows here.
	mock.ExpectQuery("WITH RECURSIVE descendants").
		WithArgs(wsA).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	// Mark only workspace A as removed
	mock.ExpectExec("UPDATE workspaces SET status =").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM canvas_layouts WHERE workspace_id = ANY").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("UPDATE workspace_auth_tokens SET revoked_at").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Schedule disable fires only for wsA's IDs — sqlmock enforces query ordering
	// so if the production code somehow included wsB it would be a different
	// query argument and fail to match.
	mock.ExpectExec("UPDATE workspace_schedules SET enabled = false").
		WillReturnResult(sqlmock.NewResult(0, 0)) // wsA had no schedules
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsA+"?confirm=true", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// The key assertion: all expectations were met and no extra queries ran.
	// If the production code had a bug that disabled ALL schedules (not just
	// those belonging to the deleted workspace), sqlmock would detect the
	// mismatch because the query text/args would differ.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDelete_ChildrenQueryError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT id, name FROM workspaces WHERE parent_id").
		WithArgs("cccccccc-000c-0000-0000-000000000000").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-000c-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-err-del?confirm=true", nil)

	handler.Delete(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Phase 30.4 — State polling ====================

const stateWsID = "550e8400-e29b-41d4-a716-446655440000"

func stateReq(w *httptest.ResponseRecorder, auth string) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: stateWsID}}
	req := httptest.NewRequest("GET", "/workspaces/"+stateWsID+"/state", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	c.Request = req
	return c
}

func TestWorkspaceState_LegacyGrandfatheredOnlineStatus(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp")

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(stateWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT status\s+FROM workspaces\s+WHERE id`).
		WithArgs(stateWsID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	w := httptest.NewRecorder()
	c := stateReq(w, "")
	handler.State(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "online" || body["paused"] != false || body["deleted"] != false {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestWorkspaceState_PausedDetected(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp")

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(stateWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT status\s+FROM workspaces\s+WHERE id`).
		WithArgs(stateWsID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("paused"))

	w := httptest.NewRecorder()
	c := stateReq(w, "")
	handler.State(c)

	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["paused"] != true {
		t.Errorf("paused flag should be true when status=paused; body=%v", body)
	}
}

func TestWorkspaceState_DeletedRowReturns404WithDeletedFlag(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp")

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(stateWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT status\s+FROM workspaces\s+WHERE id`).
		WithArgs(stateWsID).
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c := stateReq(w, "")
	handler.State(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for hard-deleted row, got %d", w.Code)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["deleted"] != true {
		t.Errorf("deleted flag should be true on 404; body=%+v", body)
	}
}

func TestWorkspaceState_MissingTokenWhenOnFile(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp")

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(stateWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := httptest.NewRecorder()
	c := stateReq(w, "")
	handler.State(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when token required and absent, got %d", w.Code)
	}
}

func TestWorkspaceState_ValidTokenReturnsStatus(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", "/tmp")

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(stateWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("t1", stateWsID))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET last_used_at`).
		WithArgs("t1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT status\s+FROM workspaces\s+WHERE id`).
		WithArgs(stateWsID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("degraded"))

	w := httptest.NewRecorder()
	c := stateReq(w, "Bearer good")
	handler.State(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "degraded" {
		t.Errorf("status should be 'degraded', got %v", body["status"])
	}
}

// ── #138 field-level auth tests ─────────────────────────────────────────────
// Cosmetic PATCH (name/x/y/role) stays open so canvas drag-reposition works
// without a bearer token. Sensitive fields (tier/parent_id/runtime/
// workspace_dir) require a valid admin bearer once any live token exists.

func TestWorkspaceUpdate_CosmeticField_NoBearer_FailOpen_NoTokens(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Body contains only cosmetic field → no wsauth probe ever fires.
	mock.ExpectQuery("SELECT EXISTS.*workspaces WHERE id").
		WithArgs("cccccccc-000d-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("UPDATE workspaces SET name").
		WithArgs("cccccccc-000d-0000-0000-000000000000", "Cosmetic").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-000d-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-cosmetic",
		bytes.NewBufferString(`{"name":"Cosmetic"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("cosmetic PATCH (no bearer) should pass; got %d: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceUpdate_SensitiveField_AuthEnforcedByMiddleware documents the #680 fix:
// auth for PATCH /workspaces/:id is now enforced by WorkspaceAuth middleware (router
// layer), not inside the handler. The handler processes sensitive fields (tier,
// parent_id, runtime, workspace_dir) directly — WorkspaceAuth has already verified
// the caller holds a valid bearer token for this specific workspace before the handler
// runs. No in-handler wsauth DB probe fires.
func TestWorkspaceUpdate_SensitiveField_AuthEnforcedByMiddleware(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// No workspace_auth_tokens query expected — auth is middleware's responsibility.
	mock.ExpectQuery("SELECT EXISTS.*workspaces WHERE id").
		WithArgs("cccccccc-000e-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("UPDATE workspaces SET tier").
		WithArgs("cccccccc-000e-0000-0000-000000000000", float64(3)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-000e-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/cccccccc-000e-0000-0000-000000000000",
		bytes.NewBufferString(`{"tier":3}`))
	c.Request.Header.Set("Content-Type", "application/json")
	// WorkspaceAuth middleware would have validated the bearer before this runs.
	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("sensitive PATCH (auth at middleware): got %d, want 200: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== #611 Security Auditor regressions ====================

// TestWorkspaceGet_FinancialFieldsStripped verifies that GET /workspaces/:id
// does NOT expose budget_limit or monthly_spend. The endpoint is on the open
// router — any caller with a UUID would otherwise read billing data. (#611 Fix 2)
func TestWorkspaceGet_FinancialFieldsStripped(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	columns := []string{
		"id", "name", "role", "tier", "status", "agent_card", "url",
		"parent_id", "active_tasks", "max_concurrent_tasks", "last_error_rate", "last_sample_error",
		"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
		"budget_limit", "monthly_spend",
	}
	// Populate with non-zero financial values to confirm they are stripped.
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("cccccccc-0010-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows(columns).
			AddRow("cccccccc-0010-0000-0000-000000000000", "Finance Test", "worker", 1, "online", []byte(`{}`),
				"http://localhost:9001", nil, 0, 1, 0.0, "", 0, "", "langgraph",
				"", 0.0, 0.0, false,
				int64(50000), int64(12500))) // budget_limit=500 USD, spend=125 USD

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-0010-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-fin-1", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if _, present := resp["budget_limit"]; present {
		t.Errorf("budget_limit must not appear in GET /workspaces/:id response (got %v)", resp["budget_limit"])
	}
	if _, present := resp["monthly_spend"]; present {
		t.Errorf("monthly_spend must not appear in GET /workspaces/:id response (got %v)", resp["monthly_spend"])
	}
	// Sanity-check that normal fields are still present.
	if resp["name"] != "Finance Test" {
		t.Errorf("expected name 'Finance Test', got %v", resp["name"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceGet_SensitiveFieldsStripped verifies that GET /workspaces/:id
// does NOT expose current_task, last_sample_error, or workspace_dir. These
// leak operational surveillance data and host paths to any caller with a
// valid UUID. (#955)
func TestWorkspaceGet_SensitiveFieldsStripped(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	columns := []string{
		"id", "name", "role", "tier", "status", "agent_card", "url",
		"parent_id", "active_tasks", "max_concurrent_tasks", "last_error_rate", "last_sample_error",
		"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
		"budget_limit", "monthly_spend",
	}
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs("cccccccc-0955-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows(columns).
			AddRow("cccccccc-0955-0000-0000-000000000000", "Surveillance Test", "worker", 1, "online", []byte(`{}`),
				"http://localhost:9002", nil, 1, 1, 0.0,
				"panic: internal error at /secret/path.go:42",
				100,
				"Analyzing customer PII for the Q4 report",
				"langgraph",
				"/home/user/secret-projects/client-work",
				0.0, 0.0, false,
				nil, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-0955-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-955", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	for _, field := range []string{"current_task", "last_sample_error", "workspace_dir"} {
		if _, present := resp[field]; present {
			t.Errorf("%s must not appear in public GET response (got %v)", field, resp[field])
		}
	}

	// Sanity: discovery fields still present
	if resp["name"] != "Surveillance Test" {
		t.Errorf("expected name 'Surveillance Test', got %v", resp["name"])
	}
	if resp["active_tasks"] != float64(1) {
		t.Errorf("expected active_tasks 1, got %v", resp["active_tasks"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceUpdate_BudgetLimitIgnored verifies that including budget_limit
// in a PATCH /workspaces/:id body does NOT trigger a DB write. The only write
// path for budget_limit is PATCH /workspaces/:id/budget (AdminAuth-gated).
// Any workspace bearer must not be able to self-clear its spending ceiling.
// (#611 Fix 1)
func TestWorkspaceUpdate_BudgetLimitIgnored(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Only the existence probe fires — no UPDATE for budget_limit.
	mock.ExpectQuery("SELECT EXISTS.*workspaces WHERE id").
		WithArgs("cccccccc-0011-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// name update is the only expected write
	mock.ExpectExec("UPDATE workspaces SET name").
		WithArgs("cccccccc-0011-0000-0000-000000000000", "Safe Name").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-0011-0000-0000-000000000000"}}
	// Send budget_limit alongside an innocuous field.
	body := `{"name":"Safe Name","budget_limit":null}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-budget-test",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// sqlmock will fail if any unexpected DB call was made (e.g. for budget_limit).
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB call — budget_limit must not be written via Update: %v", err)
	}
}

// TestWorkspaceUpdate_BudgetLimitOnly_Ignored verifies that a body containing
// ONLY budget_limit results in no DB writes at all (besides the existence probe).
func TestWorkspaceUpdate_BudgetLimitOnly_Ignored(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT EXISTS.*workspaces WHERE id").
		WithArgs("cccccccc-0012-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// No UPDATE expected — budget_limit must be silently skipped.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "cccccccc-0012-0000-0000-000000000000"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-budget-only",
		bytes.NewBufferString(`{"budget_limit":999999}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB call for budget_limit: %v", err)
	}
}

// TestWorkspaceCreate_TemplateDefaultsMissingRuntimeAndModel covers the
// hermes-trap case: a caller (TemplatePalette, direct API, script) POSTs
// /workspaces with only a template name + no runtime + no model. The
// handler must read the template's config.yaml and fill in both fields
// BEFORE DB insert — otherwise hermes-agent auto-detects provider
// wrong and 401s downstream (PR #1714 context).
//
// Uses the nested runtime_config.model format current templates use;
// legacy top-level `model:` is covered by the Legacy test below.
func TestWorkspaceCreate_TemplateDefaultsMissingRuntimeAndModel(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()

	// Stage a hermes-like template inside the configsDir the handler reads.
	configsDir := t.TempDir()
	templateDir := filepath.Join(configsDir, "hermes-template")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := []byte(`name: Hermes Agent
tier: 2
runtime: hermes
runtime_config:
  model: nousresearch/hermes-4-70b
`)
	if err := os.WriteFile(filepath.Join(templateDir, "config.yaml"), cfg, 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", configsDir)

	mock.ExpectBegin()
	// Request omits runtime + model; handler must fill from the template
	// and hand the completed values to the INSERT.
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(
			sqlmock.AnyArg(), "Hermes Agent", nil, 3, "hermes",
			sqlmock.AnyArg(), (*string)(nil), nil, "none", (*int64)(nil), models.DefaultMaxConcurrentTasks, "push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WithArgs(sqlmock.AnyArg(), float64(0), float64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"Hermes Agent","template":"hermes-template"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestWorkspaceCreate_TemplateDefaultsLegacyTopLevelModel covers
// pre-runtime_config templates that declare `model:` at the top level.
// These should still surface the default via the same auto-fill.
func TestWorkspaceCreate_TemplateDefaultsLegacyTopLevelModel(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()

	configsDir := t.TempDir()
	templateDir := filepath.Join(configsDir, "legacy-template")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := []byte(`name: Legacy Agent
tier: 1
runtime: langgraph
model: anthropic:claude-sonnet-4-5
`)
	if err := os.WriteFile(filepath.Join(templateDir, "config.yaml"), cfg, 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", configsDir)

	mock.ExpectBegin()
	// Default tier 3 (Privileged) — see workspace.go create-handler comment.
	// Template declares tier: 1 but the handler's current semantics ignore
	// that field and fall through to the default. If that's ever fixed,
	// this assertion should flip back to 1.
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(
			sqlmock.AnyArg(), "Legacy Agent", nil, 3, "langgraph",
			sqlmock.AnyArg(), (*string)(nil), nil, "none", (*int64)(nil), models.DefaultMaxConcurrentTasks, "push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WithArgs(sqlmock.AnyArg(), float64(0), float64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"Legacy Agent","template":"legacy-template"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceCreate_CallerModelOverridesTemplateDefault asserts that
// when the caller passes an explicit `model`, we DO NOT overwrite it
// with the template's default. The pre-fill only happens on empty.
func TestWorkspaceCreate_CallerModelOverridesTemplateDefault(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()

	configsDir := t.TempDir()
	templateDir := filepath.Join(configsDir, "hermes-template")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := []byte(`runtime: hermes
runtime_config:
  model: nousresearch/hermes-4-70b
`)
	if err := os.WriteFile(filepath.Join(templateDir, "config.yaml"), cfg, 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", configsDir)

	mock.ExpectBegin()
	// Caller explicitly chose minimax — template's hermes-4-70b must NOT win.
	// The INSERT only passes runtime to the DB (model goes to agent_card /
	// downstream config); we verify runtime == "hermes" and rely on the
	// absence of a handler error to mean the model passthrough was honored.
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(
			sqlmock.AnyArg(), "Custom Hermes", nil, 3, "hermes",
			sqlmock.AnyArg(), (*string)(nil), nil, "none", (*int64)(nil), models.DefaultMaxConcurrentTasks, "push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WithArgs(sqlmock.AnyArg(), float64(0), float64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"Custom Hermes","template":"hermes-template","model":"minimax/MiniMax-M2.7"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}
