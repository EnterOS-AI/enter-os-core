package handlers

// Sqlmock-backed coverage for workspace_abilities.go (PatchAbilities).
// Closes #1312 — handler was at 0% coverage.

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func patchAbilitiesReq(t *testing.T, wsID string, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID+"/abilities", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	PatchAbilities(c)
	return w
}

// ---------- Validation errors ----------

func TestPatchAbilities_InvalidWorkspaceID(t *testing.T) {
	w := patchAbilitiesReq(t, "not-a-uuid", `{"broadcast_enabled":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPatchAbilities_InvalidJSON(t *testing.T) {
	w := patchAbilitiesReq(t, wsUUID1, `not json`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPatchAbilities_EmptyBody(t *testing.T) {
	w := patchAbilitiesReq(t, wsUUID1, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Not found ----------

func TestPatchAbilities_WorkspaceNotFound(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1 AND status != 'removed'\)`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	w := patchAbilitiesReq(t, wsUUID1, `{"broadcast_enabled":true}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestPatchAbilities_ExistsQueryError(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1 AND status != 'removed'\)`).
		WithArgs(wsUUID1).
		WillReturnError(errors.New("conn refused"))

	w := patchAbilitiesReq(t, wsUUID1, `{"broadcast_enabled":true}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on exists query error, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Happy paths ----------

func TestPatchAbilities_BroadcastOnly(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1 AND status != 'removed'\)`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET broadcast_enabled = \$2, updated_at = now\(\) WHERE id = \$1`).
		WithArgs(wsUUID1, true).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := patchAbilitiesReq(t, wsUUID1, `{"broadcast_enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestPatchAbilities_TalkToUserOnly(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1 AND status != 'removed'\)`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET talk_to_user_enabled = \$2, updated_at = now\(\) WHERE id = \$1`).
		WithArgs(wsUUID1, false).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := patchAbilitiesReq(t, wsUUID1, `{"talk_to_user_enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestPatchAbilities_BothFields(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1 AND status != 'removed'\)`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET broadcast_enabled = \$2, talk_to_user_enabled = \$3, updated_at = now\(\) WHERE id = \$1`).
		WithArgs(wsUUID1, true, true).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := patchAbilitiesReq(t, wsUUID1, `{"broadcast_enabled":true,"talk_to_user_enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// ---------- DB errors on update ----------

func TestPatchAbilities_BroadcastUpdateError(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1 AND status != 'removed'\)`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET broadcast_enabled = \$2, updated_at = now\(\) WHERE id = \$1`).
		WithArgs(wsUUID1, true).
		WillReturnError(errors.New("disk full"))

	w := patchAbilitiesReq(t, wsUUID1, `{"broadcast_enabled":true}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPatchAbilities_TalkToUserUpdateError(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1 AND status != 'removed'\)`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET talk_to_user_enabled = \$2, updated_at = now\(\) WHERE id = \$1`).
		WithArgs(wsUUID1, false).
		WillReturnError(errors.New("disk full"))

	w := patchAbilitiesReq(t, wsUUID1, `{"talk_to_user_enabled":false}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPatchAbilities_BothFields_UpdateError — regression for #2131. When
// both fields are supplied the handler uses a single combined UPDATE. A
// failure of that UPDATE must leave the workspace unchanged (atomic).
func TestPatchAbilities_BothFields_UpdateError(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1 AND status != 'removed'\)`).
		WithArgs(wsUUID1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET broadcast_enabled = \$2, talk_to_user_enabled = \$3, updated_at = now\(\) WHERE id = \$1`).
		WithArgs(wsUUID1, true, true).
		WillReturnError(errors.New("disk full"))

	w := patchAbilitiesReq(t, wsUUID1, `{"broadcast_enabled":true,"talk_to_user_enabled":true}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	// Because only one UPDATE is issued, there is no partial-mutation
	// path to assert against; sqlmock implicitly verifies no second
	// exec occurred.
}
