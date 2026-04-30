package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/gin-gonic/gin"
)

// ==================== POST /workspaces/:id/restart — additional coverage ====================

func TestRestartHandler_WorkspaceNotFoundReturns404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT status, name, tier, COALESCE").
		WithArgs("ws-nonexistent").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-nonexistent"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-nonexistent/restart", nil)

	handler.Restart(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestRestartHandler_DBConnectionError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT status, name, tier, COALESCE").
		WithArgs("ws-conn-err").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-conn-err"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-conn-err/restart", nil)

	handler.Restart(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestRestartHandler_AncestorPausedBlocksRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// Lookup workspace
	mock.ExpectQuery("SELECT status, name, tier, COALESCE").
		WithArgs("ws-grandchild").
		WillReturnRows(sqlmock.NewRows([]string{"status", "name", "tier", "runtime"}).
			AddRow("offline", "Grandchild Agent", 1, "langgraph"))

	// isParentPaused: get parent_id of grandchild -> child
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs("ws-grandchild").
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow("ws-mid"))

	// isParentPaused: check child's status (online, not paused)
	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id =").
		WithArgs("ws-mid").
		WillReturnRows(sqlmock.NewRows([]string{"status", "name"}).AddRow("online", "Middle Agent"))

	// Recursive: isParentPaused for ws-mid -> ws-root
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs("ws-mid").
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow("ws-root"))

	// isParentPaused: ws-root is paused
	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id =").
		WithArgs("ws-root").
		WillReturnRows(sqlmock.NewRows([]string{"status", "name"}).AddRow("paused", "Root Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-grandchild"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-grandchild/restart", nil)

	handler.Restart(c)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if errMsg, ok := resp["error"].(string); !ok || !strings.Contains(errMsg, "paused") {
		t.Errorf("expected error about paused grandparent, got %v", resp["error"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestRestartHandler_ExternalRuntimeNoOps(t *testing.T) {
	// Manual Restart on a runtime=external workspace must short-circuit:
	// no Stop, no provision, no token revoke. Pre-fix, this path ran the
	// full re-provision pipeline and silently revoked the operator's
	// bearer token on every click.
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT status, name, tier, COALESCE").
		WithArgs("ws-external").
		WillReturnRows(sqlmock.NewRows([]string{"status", "name", "tier", "runtime"}).
			AddRow("offline", "External Agent", 1, "external"))

	// isParentPaused: no parent
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs("ws-external").
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}))

	// No further expectations — Restart must NOT touch the DB or the
	// provisioner from this point. sqlmock will fail the test if any
	// unexpected query runs (UPDATE workspaces SET status=..., the
	// RevokeAllForWorkspace DELETE, etc.).

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-external"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-external/restart", nil)

	handler.Restart(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got, _ := resp["status"].(string); got != "noop" {
		t.Errorf("expected status=noop, got %v", resp["status"])
	}
	if got, _ := resp["runtime"].(string); got != "external" {
		t.Errorf("expected runtime=external, got %v", resp["runtime"])
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "operator-driven") {
		t.Errorf("expected message about operator-driven, got %v", resp["message"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (or unexpected queries — token revoke or status update would trigger this): %v", err)
	}
}

func TestRestartHandler_NilProvisionerReturns503(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT status, name, tier, COALESCE").
		WithArgs("ws-no-prov").
		WillReturnRows(sqlmock.NewRows([]string{"status", "name", "tier", "runtime"}).
			AddRow("offline", "Test Agent", 1, "langgraph"))

	// isParentPaused: no parent
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs("ws-no-prov").
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-no-prov"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-no-prov/restart", nil)

	handler.Restart(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== POST /workspaces/:id/pause — additional coverage ====================

func TestPauseHandler_WorkspaceNotFoundReturns404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id =").
		WithArgs("ws-pause-gone").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-pause-gone"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-pause-gone/pause", nil)

	handler.Pause(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestPauseHandler_DBConnectionError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id =").
		WithArgs("ws-pause-dberr").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-pause-dberr"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-pause-dberr/pause", nil)

	handler.Pause(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestPauseHandler_SuccessNoChildren(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id =").
		WithArgs("ws-pause-ok").
		WillReturnRows(sqlmock.NewRows([]string{"status", "name"}).AddRow("online", "Agent A"))

	mock.ExpectQuery("WITH RECURSIVE descendants").
		WithArgs("ws-pause-ok").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs("ws-pause-ok").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-pause-ok"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-pause-ok/pause", nil)

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
}

// ==================== POST /workspaces/:id/resume — additional coverage ====================

func TestResumeHandler_NotPausedReturns404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT name, tier, COALESCE").
		WithArgs("ws-resume-notpaused").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-resume-notpaused"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-resume-notpaused/resume", nil)

	handler.Resume(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestResumeHandler_DBConnectionError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT name, tier, COALESCE").
		WithArgs("ws-resume-dberr").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-resume-dberr"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-resume-dberr/resume", nil)

	handler.Resume(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestResumeHandler_NilProvisionerReturns503(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT name, tier, COALESCE").
		WithArgs("ws-resume-noprov").
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "runtime"}).
			AddRow("Test Agent", 1, "langgraph"))

	// provisioner nil check happens BEFORE isParentPaused, so no parent query expected

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-resume-noprov"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-resume-noprov/resume", nil)

	handler.Resume(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// Note: TestResumeHandler_ParentPausedBlocksResume requires a non-nil provisioner
// (Resume checks provisioner before isParentPaused). This is covered in
// handlers_additional_test.go's integration-style tests.

// ==================== HibernateWorkspace — TOCTOU fix (#819) ====================

// TestHibernateWorkspace_ActiveTasksNotHibernated verifies that a workspace
// with active_tasks > 0 is NOT hibernated: the atomic UPDATE WHERE active_tasks=0
// returns 0 rows, and the function returns without calling Stop or the final
// status update.
func TestHibernateWorkspace_ActiveTasksNotHibernated(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	var stopCalls int32
	handler.stopFnOverride = func(_ context.Context, _ string) {
		atomic.AddInt32(&stopCalls, 1)
	}

	// The atomic claim UPDATE returns 0 rows because active_tasks > 0 fails the WHERE.
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-active", models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 0)) // rowsAffected = 0

	handler.HibernateWorkspace(context.Background(), "ws-active")

	if got := atomic.LoadInt32(&stopCalls); got != 0 {
		t.Errorf("provisioner.Stop called %d times; want 0 (active_tasks > 0 must prevent hibernation)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHibernateWorkspace_AlreadyHibernatingNotHibernated verifies that a
// workspace already in status 'hibernating' (claimed by a concurrent caller)
// is skipped: the atomic UPDATE returns 0 rows because status no longer
// matches IN ('online','degraded').
func TestHibernateWorkspace_AlreadyHibernatingNotHibernated(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	var stopCalls int32
	handler.stopFnOverride = func(_ context.Context, _ string) {
		atomic.AddInt32(&stopCalls, 1)
	}

	// Another goroutine already transitioned the workspace to 'hibernating',
	// so this UPDATE finds nothing matching the WHERE clause.
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-already", models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 0))

	handler.HibernateWorkspace(context.Background(), "ws-already")

	if got := atomic.LoadInt32(&stopCalls); got != 0 {
		t.Errorf("provisioner.Stop called %d times; want 0 (concurrent claim should abort this call)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHibernateWorkspace_SuccessPath verifies the happy path: atomic claim
// succeeds (rowsAffected=1), Stop is called exactly once, and the final
// 'hibernated' UPDATE is executed.
func TestHibernateWorkspace_SuccessPath(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	var stopCalls int32
	handler.stopFnOverride = func(_ context.Context, _ string) {
		atomic.AddInt32(&stopCalls, 1)
	}

	// Step 1: atomic claim succeeds
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-ok", models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1)) // rowsAffected = 1

	// Name/tier fetch after claim
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs("ws-ok").
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("My Agent", 1))

	// Step 3: final hibernated UPDATE
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusHibernated, "ws-ok").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// broadcaster INSERT
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	handler.HibernateWorkspace(context.Background(), "ws-ok")

	if got := atomic.LoadInt32(&stopCalls); got != 1 {
		t.Errorf("provisioner.Stop called %d times; want exactly 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHibernateWorkspace_ConcurrentOnlyOneStop verifies the core TOCTOU guarantee:
// when two callers race to hibernate the same workspace, the DB atomicity ensures
// only one proceeds (rowsAffected=1) and only one Stop() is issued.
//
// The real Postgres guarantee (only one UPDATE wins) is modelled here by running
// both calls sequentially against the same mock, with FIFO expectations:
//   - First call wins   → rowsAffected=1 → Stop is called
//   - Second call loses → rowsAffected=0 → Stop is NOT called
//
// This directly verifies the invariant "at most one Stop per workspace across
// any number of concurrent hibernate attempts."
func TestHibernateWorkspace_ConcurrentOnlyOneStop(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	var stopCalls int32
	handler.stopFnOverride = func(_ context.Context, _ string) {
		atomic.AddInt32(&stopCalls, 1)
	}

	// ── Caller A wins the race ────────────────────────────────────────────────
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-race", models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs("ws-race").
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("Race Agent", 2))
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusHibernated, "ws-race").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// ── Caller B loses — workspace is already 'hibernating' ───────────────────
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-race", models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Execute sequentially (sqlmock is not safe for concurrent goroutines);
	// the test models the serialized DB outcome that Postgres enforces.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); handler.HibernateWorkspace(context.Background(), "ws-race") }()
	wg.Wait()

	wg.Add(1)
	go func() { defer wg.Done(); handler.HibernateWorkspace(context.Background(), "ws-race") }()
	wg.Wait()

	if got := atomic.LoadInt32(&stopCalls); got != 1 {
		t.Errorf("provisioner.Stop called %d times; want exactly 1 across two hibernate attempts", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHibernateWorkspace_DBErrorOnClaim verifies that a DB error on the
// atomic claim UPDATE aborts the hibernation without calling Stop.
func TestHibernateWorkspace_DBErrorOnClaim(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	var stopCalls int32
	handler.stopFnOverride = func(_ context.Context, _ string) {
		atomic.AddInt32(&stopCalls, 1)
	}

	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-dberr", models.StatusHibernating).
		WillReturnError(sql.ErrConnDone)

	handler.HibernateWorkspace(context.Background(), "ws-dberr")

	if got := atomic.LoadInt32(&stopCalls); got != 0 {
		t.Errorf("provisioner.Stop called %d times on DB error; want 0", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
