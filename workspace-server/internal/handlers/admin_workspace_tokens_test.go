package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func TestAdminWorkspaceTokenHandler_Create_HappyPath(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT id FROM workspaces WHERE id = \$1 AND status <> 'removed'`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wsUUID1))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO workspace_auth_tokens`).
		WithArgs(wsUUID1, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := makeReq(t, NewAdminWorkspaceTokenHandler().Create, "POST",
		"/admin/workspaces/"+wsUUID1+"/tokens", gin.Params{{Key: "id", Value: wsUUID1}})

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		AuthToken   string `json:"auth_token"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.AuthToken == "" || body.WorkspaceID != wsUUID1 {
		t.Fatalf("unexpected body: %+v", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestAdminWorkspaceTokenHandler_Create_MissingWorkspace(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT id FROM workspaces WHERE id = \$1 AND status <> 'removed'`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	w := makeReq(t, NewAdminWorkspaceTokenHandler().Create, "POST",
		"/admin/workspaces/"+wsUUID1+"/tokens", gin.Params{{Key: "id", Value: wsUUID1}})

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminWorkspaceTokenHandler_Create_RateLimited(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT id FROM workspaces WHERE id = \$1 AND status <> 'removed'`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wsUUID1))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(maxTokensPerWorkspace))

	w := makeReq(t, NewAdminWorkspaceTokenHandler().Create, "POST",
		"/admin/workspaces/"+wsUUID1+"/tokens", gin.Params{{Key: "id", Value: wsUUID1}})

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminWorkspaceTokenHandler_Create_IssueFails(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT id FROM workspaces WHERE id = \$1 AND status <> 'removed'`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wsUUID1))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO workspace_auth_tokens`).
		WillReturnError(errors.New("disk full"))

	w := makeReq(t, NewAdminWorkspaceTokenHandler().Create, "POST",
		"/admin/workspaces/"+wsUUID1+"/tokens", gin.Params{{Key: "id", Value: wsUUID1}})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}
