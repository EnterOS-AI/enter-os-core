package handlers

// Integration tests for the workspace hibernation feature (issue #711 / PR #724).
// Updated for the atomic TOCTOU fix (issue #819).
//
// Coverage:
//   - HibernateWorkspace(): atomic claim, container stop, DB status update, Redis key clear, event broadcast
//   - POST /workspaces/:id/hibernate HTTP handler: online→200, not-eligible→404, DB error→500
//   - resolveAgentURL(): hibernated workspace → 503 + Retry-After: 15 + waking: true
//
// The A2A auto-wake path (resolveAgentURL) is tested via TestResolveAgentURL_HibernatedWorkspace_*
// added to a2a_proxy_test.go to keep related resolveAgentURL tests co-located.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/gin-gonic/gin"
)

// ──────────────────────────────────────────────────────────────────────────────
// HibernateWorkspace unit tests
// ──────────────────────────────────────────────────────────────────────────────

// TestHibernateWorkspace_OnlineWorkspace_Success verifies the happy-path with
// the 3-step atomic pattern (#819):
//   - Atomic claim UPDATE returns rowsAffected=1 (workspace was online/degraded + active_tasks=0)
//   - Name/tier SELECT runs after the claim
//   - Final UPDATE sets status='hibernated', url=''
//   - Redis keys ws:{id}, ws:{id}:url, ws:{id}:internal_url are deleted
//   - WORKSPACE_HIBERNATED event is broadcast (INSERT INTO structure_events)
func TestHibernateWorkspace_OnlineWorkspace_Success(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-idle-online"

	// Pre-populate Redis keys that ClearWorkspaceKeys should remove.
	mr.Set(fmt.Sprintf("ws:%s", wsID), "some-value")
	mr.Set(fmt.Sprintf("ws:%s:url", wsID), "http://agent.internal:8000")
	mr.Set(fmt.Sprintf("ws:%s:internal_url", wsID), "http://172.17.0.5:8000")

	// Step 1: atomic claim UPDATE succeeds.
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Post-claim SELECT for name/tier.
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("Idle Agent", 1))

	// Step 3: final UPDATE to 'hibernated'.
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusHibernated, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Broadcaster inserts a structure_events row.
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	handler.HibernateWorkspace(context.Background(), wsID)

	// All DB expectations were exercised.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}

	// Redis keys must all be gone.
	for _, suffix := range []string{"", ":url", ":internal_url"} {
		key := fmt.Sprintf("ws:%s%s", wsID, suffix)
		if _, err := mr.Get(key); err == nil {
			t.Errorf("expected Redis key %q to be deleted, but it still exists", key)
		}
	}
}

// TestHibernateWorkspace_NotEligible_NoOp verifies that when the atomic claim
// UPDATE returns rowsAffected=0 (workspace not in online/degraded state, or
// active_tasks > 0), HibernateWorkspace returns immediately — no Stop, no
// final UPDATE, no Redis clear, no broadcast.
func TestHibernateWorkspace_NotEligible_NoOp(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-already-offline"

	// Atomic claim finds nothing matching WHERE (workspace offline, paused, etc.).
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Set a Redis key to confirm it is NOT cleared by early return.
	mr.Set(fmt.Sprintf("ws:%s:url", wsID), "http://still-here:8000")

	handler.HibernateWorkspace(context.Background(), wsID)

	// Only the one ExecContext expectation; no further DB operations.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}

	// Redis key must still exist — HibernateWorkspace returned early.
	if _, err := mr.Get(fmt.Sprintf("ws:%s:url", wsID)); err != nil {
		t.Errorf("expected Redis key to still exist after no-op, but it was deleted: %v", err)
	}
}

// TestHibernateWorkspace_DBUpdateFails_NoCrash verifies that a DB error on the
// final status UPDATE does not panic — the function logs and returns silently.
func TestHibernateWorkspace_DBUpdateFails_NoCrash(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-update-fail"

	// Step 1: atomic claim succeeds.
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Post-claim SELECT.
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("Flaky Agent", 2))

	// Step 3: final UPDATE fails.
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusHibernated, wsID).
		WillReturnError(fmt.Errorf("db: connection refused"))

	// Must not panic — test will catch a panic via t.Fatal.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("HibernateWorkspace panicked on UPDATE error: %v", r)
		}
	}()

	handler.HibernateWorkspace(context.Background(), wsID)

	// Claim + SELECT + failing UPDATE; no INSERT INTO structure_events expected.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// POST /workspaces/:id/hibernate HTTP handler tests
// ──────────────────────────────────────────────────────────────────────────────

// hibernateRequest fires POST /workspaces/{id}/hibernate against the handler
// and returns the response recorder.
func hibernateRequest(t *testing.T, handler *WorkspaceHandler, wsID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/hibernate", nil)
	handler.Hibernate(c)
	return w
}

// hibernateRequestWithQuery is like hibernateRequest but appends a query string.
func hibernateRequestWithQuery(t *testing.T, handler *WorkspaceHandler, wsID, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/hibernate?"+query, nil)
	handler.Hibernate(c)
	return w
}

// TestHibernateHandler_Online_Returns200 verifies that an online workspace
// that is eligible for hibernation returns 200 {"status":"hibernated"}.
// With the 3-step fix: handler SELECT → atomic claim UPDATE → name/tier SELECT
// → final UPDATE → broadcaster INSERT.
func TestHibernateHandler_Online_Returns200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-handler-online"

	// Hibernate() handler eligibility SELECT — checks status IN ('online','degraded').
	mock.ExpectQuery(`SELECT name, tier, active_tasks FROM workspaces WHERE id = .* AND status IN`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "active_tasks"}).AddRow("Online Bot", 1, 0))

	// HibernateWorkspace() step 1: atomic claim.
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Post-claim SELECT for name/tier.
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "active_tasks"}).AddRow("Online Bot", 1, 0))

	// Step 3: final UPDATE.
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusHibernated, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Broadcaster INSERT.
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := hibernateRequest(t, handler, wsID)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "hibernated" {
		t.Errorf(`expected {"status":"hibernated"}, got %v`, resp)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestHibernateHandler_NotActive_Returns404 verifies that a workspace not in
// online/degraded state (e.g. offline, paused, already hibernated) returns 404.
func TestHibernateHandler_NotActive_Returns404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-handler-paused"

	// Handler's eligibility SELECT returns no rows — workspace is not online/degraded.
	mock.ExpectQuery(`SELECT name, tier, active_tasks FROM workspaces WHERE id = .* AND status IN`).
		WithArgs(wsID).
		WillReturnError(sql.ErrNoRows)

	w := hibernateRequest(t, handler, wsID)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !strings.Contains(fmt.Sprint(resp["error"]), "not found") {
		t.Errorf("expected error mentioning 'not found', got %v", resp)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestHibernateHandler_ActiveTasks_Returns409 verifies that hibernating a
// workspace with active_tasks > 0 returns 409 unless ?force=true is passed.
// (#822)
func TestHibernateHandler_ActiveTasks_Returns409(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-busy"

	mock.ExpectQuery(`SELECT name, tier, active_tasks FROM workspaces WHERE id = .* AND status IN`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "active_tasks"}).AddRow("Busy Bot", 1, 3))

	w := hibernateRequest(t, handler, wsID)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if active, _ := resp["active_tasks"].(float64); active != 3 {
		t.Errorf("expected active_tasks=3 in response, got %v", resp["active_tasks"])
	}
}

// TestHibernateHandler_ActiveTasks_ForceTrue_Returns200 verifies that
// ?force=true overrides the 409 guard and proceeds with hibernation. (#822)
func TestHibernateHandler_ActiveTasks_ForceTrue_Returns200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-force-hibernate"

	mock.ExpectQuery(`SELECT name, tier, active_tasks FROM workspaces WHERE id = .* AND status IN`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "active_tasks"}).AddRow("Force Bot", 1, 2))

	// HibernateWorkspace claim
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Post-claim SELECT
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("Force Bot", 1))

	// Final UPDATE to hibernated
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusHibernated, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Broadcaster
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := hibernateRequestWithQuery(t, handler, wsID, "force=true")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHibernateHandler_DBError_Returns500 verifies that an unexpected DB error
// on the eligibility SELECT returns 500.
func TestHibernateHandler_DBError_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-handler-dberror"

	mock.ExpectQuery(`SELECT name, tier, active_tasks FROM workspaces WHERE id = .* AND status IN`).
		WithArgs(wsID).
		WillReturnError(fmt.Errorf("db: connection reset"))

	w := hibernateRequest(t, handler, wsID)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}
