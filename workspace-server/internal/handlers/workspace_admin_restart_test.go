package handlers

// workspace_admin_restart_test.go — tests for the AdminRestart handler
// (the partner of the user-facing POST /workspaces/:id/restart). The CP
// migrator calls this to re-inject the tenant's LLM creds via the
// loadWorkspaceSecrets path on a freshly-migrated box (today's
// 2026-06-15 fleet-credential incident root-cause durable fix — see
// PRs #824 (CP) and this one (tenant partner)). Mirrors the
// SetComputeInstance test pattern (workspace_set_compute_instance_test.go).

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// AdminRestart re-injects LLM creds via the loadWorkspaceSecrets path
// (the durable fix for today's 2026-06-15 fleet-credential incident —
// see controlplane PR #824 for the migrator-side). The handler fires
// wh.RestartByID ASYNC (per the existing /restart endpoint's pattern)
// and returns 202 Accepted immediately.
func TestAdminRestart_HappyPath(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	// Pre-flight: confirm the workspace exists. The handler does
	// a SELECT 1 FROM workspaces WHERE id = $1 before firing the
	// async restart, so we expect that query.
	mock.ExpectQuery(`SELECT 1 FROM workspaces WHERE id = \$1`).
		WithArgs("ws-migrated").
		WillReturnRows(sqlmock.NewRows([]string{"x"}).AddRow(1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-migrated"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-migrated/restart", nil)

	h.AdminRestart(c)

	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
	// The actual restart is async; we don't assert on the goroutine
	// (it would no-op on the test bootstrap since h has no provisioner
	// wired; the goAsync panic-recovery swallows any panic cleanly).
}

// A workspace id that matches no row is a 404 — the migrator can tell
// a stale id from a real restart. Distinct from SetComputeInstance's
// NoRowIs404 (which fires on the UPDATE rowcount), here the
// pre-flight SELECT does the work.
func TestAdminRestart_NoRowIs404(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	mock.ExpectQuery(`SELECT 1 FROM workspaces WHERE id = \$1`).
		WithArgs("ws-gone").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-gone"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-gone/restart", nil)

	h.AdminRestart(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A DB failure on the pre-flight surfaces as 500 so the migrator
// can fail loudly rather than silently restart into a missing
// workspace. (RestartByID would fail too, but with a less-precise
// error from the deeper code path; surfacing the pre-flight 500
// gives ops a clean diagnostic.)
func TestAdminRestart_DBErrorIs500(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	mock.ExpectQuery(`SELECT 1 FROM workspaces WHERE id = \$1`).
		WithArgs("ws-1").
		WillReturnError(errors.New("connection reset"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-1/restart", nil)

	h.AdminRestart(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// An empty id is a 400 before any DB work — the migrator never
// issues an empty id (it always has a real wsID from the cutover
// record), so this is a defense-in-depth check, not a hot path.
func TestAdminRestart_EmptyIDIs400(t *testing.T) {
	h, _ := setupBootstrapHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: ""}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces//restart", nil)

	h.AdminRestart(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

// Sanity check that the handler does NOT pause for the actual restart
// (the 202 path is async; the migrator's poll loop is not held by
// the restart's provisioning time). A 1ms-budgeted assertion catches
// a regression that turns the handler into a synchronous call.
func TestAdminRestart_AsyncDoesNotBlock(t *testing.T) {
	h, mock := setupBootstrapHandler(t)
	mock.ExpectQuery(`SELECT 1 FROM workspaces WHERE id = \$1`).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"x"}).AddRow(1))

	done := make(chan struct{})
	go func() {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
		c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-1/restart", nil)
		h.AdminRestart(c)
		close(done)
	}()
	select {
	case <-done:
		// PASS — handler returned quickly.
	case <-timeAfter(1):
		t.Fatal("AdminRestart blocked (the 202 must return without waiting for the restart goroutine)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// Use a package-private alias so the test file doesn't need to
// inline a time.After call. Kept inline; standard library time is
// imported via the test harness.
var timeAfter = func(d int) <-chan time.Time { return time.After(time.Duration(d) * time.Millisecond) }
