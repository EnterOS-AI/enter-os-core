package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/handlers"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/middleware"
	"github.com/gin-gonic/gin"
)

// admin_delegations_route_test.go — pin the RFC #2829 PR-4 wiring.
//
// Both the List and Stats endpoints must:
//   1. Be registered at the documented path
//   2. Be gated by AdminAuth (caller without a valid admin token → 401)
//
// Without this gate test, a future router refactor could silently drop
// AdminAuth on these endpoints — the operator dashboard would still work
// for the operator, but unauthenticated callers could pull the in-flight
// delegation list including caller_id, callee_id, and task previews.

func buildAdminDelegationsEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	adH := handlers.NewAdminDelegationsHandler(db.DB)
	r.GET("/admin/delegations", middleware.AdminAuth(db.DB), adH.List)
	r.GET("/admin/delegations/stats", middleware.AdminAuth(db.DB), adH.Stats)
	return r
}

// Both tests use the existing AdminAuth pattern: set ADMIN_TOKEN to disable
// the dev-mode fail-open branch, and have HasAnyLiveTokenGlobal return ≥1
// so AdminAuth enforces auth (rather than fail-open on fresh install).
// Without these two switches AdminAuth would return 200 + invoke the
// handler — defeating the gate test.

func TestAdminDelegationsRoute_List_RequiresAdminAuth(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-secret-not-presented-by-caller")
	mock := setupRouterTestDB(t)
	mock.ExpectQuery("SELECT COUNT.*FROM workspace_auth_tokens").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := buildAdminDelegationsEngine(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/delegations", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated request, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock unmet: %v", err)
	}
}

func TestAdminDelegationsRoute_Stats_RequiresAdminAuth(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-secret-not-presented-by-caller")
	mock := setupRouterTestDB(t)
	mock.ExpectQuery("SELECT COUNT.*FROM workspace_auth_tokens").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := buildAdminDelegationsEngine(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/delegations/stats", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated request, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock unmet: %v", err)
	}
}
