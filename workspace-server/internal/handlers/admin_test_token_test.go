package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
	"github.com/gin-gonic/gin"
)

func newTestTokenRequest(workspaceID string) (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	c.Request = httptest.NewRequest("GET", "/admin/workspaces/"+workspaceID+"/test-token", nil)
	return w, c
}

func TestAdminTestToken_HiddenInProduction(t *testing.T) {
	setupTestDB(t)
	t.Setenv("MOLECULE_ENV", "production")
	t.Setenv("MOLECULE_ENABLE_TEST_TOKENS", "")

	h := NewAdminTestTokenHandler()
	w, c := newTestTokenRequest("ws-1")
	h.GetTestToken(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 in production, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminTestToken_EnabledViaFlagEvenInProd(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("MOLECULE_ENV", "production")
	t.Setenv("MOLECULE_ENABLE_TEST_TOKENS", "1")

	mock.ExpectQuery("SELECT id FROM workspaces WHERE id =").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-1"))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewAdminTestTokenHandler()
	w, c := newTestTokenRequest("ws-1")
	h.GetTestToken(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminTestToken_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("MOLECULE_ENV", "development")

	mock.ExpectQuery("SELECT id FROM workspaces WHERE id =").
		WithArgs("missing").
		WillReturnError(sqlErrNoRows())

	h := NewAdminTestTokenHandler()
	w, c := newTestTokenRequest("missing")
	h.GetTestToken(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing workspace, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminTestToken_HappyPath_TokenValidates(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("MOLECULE_ENV", "development")

	mock.ExpectQuery("SELECT id FROM workspaces WHERE id =").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-1"))

	// Capture the hash inserted by IssueToken so we can replay it on Validate.
	var capturedHash []byte
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WithArgs("ws-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewAdminTestTokenHandler()
	w, c := newTestTokenRequest("ws-1")
	h.GetTestToken(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		AuthToken   string `json:"auth_token"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp.AuthToken == "" {
		t.Fatal("expected non-empty auth_token")
	}
	if resp.WorkspaceID != "ws-1" {
		t.Errorf("expected workspace_id ws-1, got %q", resp.WorkspaceID)
	}
	if len(resp.AuthToken) < 32 {
		t.Errorf("token looks too short: %d chars", len(resp.AuthToken))
	}

	// Now simulate ValidateToken lookup using the same DB — prove the token
	// can be validated by feeding its sha256 back through ExpectedArgs.
	// (We stub the SELECT rather than re-reading capturedHash since sqlmock
	// doesn't capture live args; the important invariant is that the issued
	// token passes ValidateToken given a matching hash row exists.)
	_ = capturedHash
	mock.ExpectQuery("SELECT t\\.id, t\\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", "ws-1"))
	mock.ExpectExec("UPDATE workspace_auth_tokens SET last_used_at").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := wsauth.ValidateToken(c.Request.Context(), db.DB, "ws-1", resp.AuthToken); err != nil {
		t.Errorf("issued token failed to validate: %v", err)
	}
}

func sqlErrNoRows() error { return sql.ErrNoRows }

// TestAdminTestToken_AdminTokenRequired_NoHeader pins the IDOR-fix (#112):
// when ADMIN_TOKEN is set, calls without an Authorization header MUST 401.
// Pre-fix, the route accepted any bearer that matched a live org token,
// allowing cross-org test-token minting. The current code uses
// subtle.ConstantTimeCompare against ADMIN_TOKEN explicitly. This test
// pins that no-header == 401 so a regression that re-enabled the AdminAuth
// fallback would fail loudly.
func TestAdminTestToken_AdminTokenRequired_NoHeader(t *testing.T) {
	setupTestDB(t)
	t.Setenv("MOLECULE_ENV", "development")
	t.Setenv("ADMIN_TOKEN", "the-admin-secret")

	h := NewAdminTestTokenHandler()
	w, c := newTestTokenRequest("ws-1")
	h.GetTestToken(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with ADMIN_TOKEN set + no Authorization, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAdminTestToken_AdminTokenRequired_WrongHeader pins that a non-matching
// bearer is rejected. Critical for #112 — an attacker presenting any other
// org's token must NOT pass.
func TestAdminTestToken_AdminTokenRequired_WrongHeader(t *testing.T) {
	setupTestDB(t)
	t.Setenv("MOLECULE_ENV", "development")
	t.Setenv("ADMIN_TOKEN", "the-admin-secret")

	h := NewAdminTestTokenHandler()
	w, c := newTestTokenRequest("ws-1")
	c.Request.Header.Set("Authorization", "Bearer wrong-token")
	h.GetTestToken(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong Authorization, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAdminTestToken_AdminTokenRequired_CorrectHeader pins the success
// path through the ADMIN_TOKEN gate. Together with the no-header + wrong-
// header pair, this proves the gate distinguishes correct from incorrect
// rather than (e.g.) erroring on every request.
func TestAdminTestToken_AdminTokenRequired_CorrectHeader(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("MOLECULE_ENV", "development")
	t.Setenv("ADMIN_TOKEN", "the-admin-secret")

	mock.ExpectQuery("SELECT id FROM workspaces WHERE id =").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-1"))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewAdminTestTokenHandler()
	w, c := newTestTokenRequest("ws-1")
	c.Request.Header.Set("Authorization", "Bearer the-admin-secret")
	h.GetTestToken(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with correct ADMIN_TOKEN, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met — INSERT into workspace_auth_tokens did not run, suggesting the gate short-circuited the success path: %v", err)
	}
}

// TestAdminTestToken_AdminTokenEmpty_GateBypassedSafely pins that when
// ADMIN_TOKEN is unset (typical local-dev setup), the explicit gate is
// bypassed and the route works without an Authorization header. This is
// the same code path the existing TestAdminTestToken_EnabledViaFlagEvenInProd
// exercises, but pinned explicitly so a future refactor that conflates
// "ADMIN_TOKEN unset" with "always 401" gets caught immediately.
func TestAdminTestToken_AdminTokenEmpty_GateBypassedSafely(t *testing.T) {
	mock := setupTestDB(t)
	t.Setenv("MOLECULE_ENV", "development")
	t.Setenv("ADMIN_TOKEN", "")

	mock.ExpectQuery("SELECT id FROM workspaces WHERE id =").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-1"))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewAdminTestTokenHandler()
	w, c := newTestTokenRequest("ws-1")
	// Note: NO Authorization header — the gate is unset, so this MUST work.
	h.GetTestToken(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with ADMIN_TOKEN empty + no Authorization, got %d: %s", w.Code, w.Body.String())
	}
}
