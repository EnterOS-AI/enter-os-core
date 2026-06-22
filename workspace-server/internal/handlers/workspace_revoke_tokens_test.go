package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// RevokeAuthTokens revokes a workspace's live tokens so the migrated
// container's next /registry/register is bootstrap-allowed. The happy path
// runs the wsauth.RevokeAllForWorkspace UPDATE and returns 200.
func TestRevokeAuthTokens_HappyPath(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	mock.ExpectExec(`UPDATE workspace_auth_tokens`).
		WithArgs("ws-migrated").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-migrated"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-migrated/revoke-auth-tokens", nil)

	h.RevokeAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// Idempotent: revoking a workspace with no live tokens (already-revoked /
// never-registered) affects 0 rows but is still a 200 — the migrator calls
// this unconditionally on every cutover.
func TestRevokeAuthTokens_NoLiveTokensStillOK(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	mock.ExpectExec(`UPDATE workspace_auth_tokens`).
		WithArgs("ws-fresh").
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-fresh"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-fresh/revoke-auth-tokens", nil)

	h.RevokeAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// An empty :id is a 400 before any DB work.
func TestRevokeAuthTokens_EmptyIDIs400(t *testing.T) {
	h, _ := setupBootstrapHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: ""}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces//revoke-auth-tokens", nil)

	h.RevokeAuthTokens(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// A DB failure surfaces as 500 so the migrator can fail the cutover rather
// than retire the source against a workspace that will 401-wedge.
func TestRevokeAuthTokens_DBErrorIs500(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	mock.ExpectExec(`UPDATE workspace_auth_tokens`).
		WithArgs("ws-dberr").
		WillReturnError(errors.New("connection reset"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-dberr"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-dberr/revoke-auth-tokens", nil)

	h.RevokeAuthTokens(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}
