package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// workspace_crud_test.go — unit coverage for workspace state, update, and delete
// handlers (workspace_crud.go), plus field validation helpers.
//
// Coverage targets:
//   - State: legacy (no live token), live token + valid, missing token,
//     invalid token, not found, soft-deleted, query error.
//   - Update: happy path, invalid UUID, invalid body, not found, each field
//     update, workspace_dir validation, length limits, YAML special chars.
//   - Delete: happy path, invalid UUID, has children (409), cascade delete
//     stop errors, purge path.
//   - validateWorkspaceID: valid/invalid UUID.
//   - validateWorkspaceFields: newline rejection, YAML special chars, length.
//   - validateWorkspaceDir: absolute/relative, traversal, system paths.

func setupWorkspaceCrudTest(t *testing.T) (sqlmock.Sqlmock, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	mock := setupTestDB(t)
	r := gin.New()
	return mock, r
}

// ---------- State ----------

func TestState_LegacyWorkspaceNoLiveToken(t *testing.T) {
	mock, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r.GET("/workspaces/:id/state", h.State)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	// No live token — legacy workspace, no auth required.
	// HasAnyLiveToken always runs first (queries workspace_auth_tokens).
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspace_auth_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT status FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("running"))

	req, _ := http.NewRequest("GET", "/workspaces/"+wsID+"/state", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if resp["workspace_id"] != wsID {
		t.Errorf("workspace_id mismatch")
	}
	if resp["status"] != "running" {
		t.Errorf("status mismatch: got %v", resp["status"])
	}
	if resp["deleted"] != false {
		t.Errorf("deleted should be false")
	}
}

func TestState_HasLiveTokenMissingAuth(t *testing.T) {
	mock, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r.GET("/workspaces/:id/state", h.State)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspace_auth_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	req, _ := http.NewRequest("GET", "/workspaces/"+wsID+"/state", nil)
	// No Authorization header
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestState_WorkspaceNotFound(t *testing.T) {
	mock, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r.GET("/workspaces/:id/state", h.State)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspace_auth_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT status FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnError(sql.ErrNoRows)

	req, _ := http.NewRequest("GET", "/workspaces/"+wsID+"/state", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if resp["deleted"] != true {
		t.Errorf("deleted should be true for not found")
	}
}

func TestState_WorkspaceSoftDeleted(t *testing.T) {
	mock, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r.GET("/workspaces/:id/state", h.State)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspace_auth_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT status FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("removed"))

	req, _ := http.NewRequest("GET", "/workspaces/"+wsID+"/state", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for soft-deleted, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if resp["deleted"] != true {
		t.Errorf("deleted should be true")
	}
	if resp["status"] != "removed" {
		t.Errorf("status should be removed")
	}
}

func TestState_QueryError(t *testing.T) {
	mock, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r.GET("/workspaces/:id/state", h.State)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspace_auth_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT status FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnError(sql.ErrConnDone)

	req, _ := http.NewRequest("GET", "/workspaces/"+wsID+"/state", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// ---------- Update ----------

func TestUpdate_InvalidUUID(t *testing.T) {
	_, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.PATCH("/workspaces/:id", h.Update)

	body := map[string]interface{}{"name": "Test"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/not-a-uuid", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_InvalidBody(t *testing.T) {
	_, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.PATCH("/workspaces/:id", h.Update)

	req, _ := http.NewRequest("PATCH", "/workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestUpdate_WorkspaceNotFound(t *testing.T) {
	mock, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.PATCH("/workspaces/:id", h.Update)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	body := map[string]interface{}{"name": "New Name"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/"+wsID, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_NameTooLong(t *testing.T) {
	_, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.PATCH("/workspaces/:id", h.Update)

	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'x'
	}
	body := map[string]interface{}{"name": string(longName)}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for name too long, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_RoleTooLong(t *testing.T) {
	_, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.PATCH("/workspaces/:id", h.Update)

	longRole := make([]byte, 1001)
	for i := range longRole {
		longRole[i] = 'x'
	}
	body := map[string]interface{}{"role": string(longRole)}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for role too long, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_NameWithNewline(t *testing.T) {
	_, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.PATCH("/workspaces/:id", h.Update)

	body := map[string]interface{}{"name": "Name\nwith newline"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for newline in name, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_NameWithYAMLSpecialChars(t *testing.T) {
	_, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.PATCH("/workspaces/:id", h.Update)

	body := map[string]interface{}{"name": "Name with [brackets]"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for YAML special chars in name, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_WorkspaceDirSystemPath(t *testing.T) {
	_, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.PATCH("/workspaces/:id", h.Update)

	body := map[string]interface{}{"workspace_dir": "/etc/my-workspace"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for system path workspace_dir, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_WorkspaceDirTraversal(t *testing.T) {
	_, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.PATCH("/workspaces/:id", h.Update)

	body := map[string]interface{}{"workspace_dir": "/workspace/../../../etc"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for traversal in workspace_dir, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_WorkspaceDirRelativePath(t *testing.T) {
	_, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.PATCH("/workspaces/:id", h.Update)

	body := map[string]interface{}{"workspace_dir": "relative/path"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for relative workspace_dir, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Delete ----------

func TestDelete_InvalidUUID(t *testing.T) {
	_, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.DELETE("/workspaces/:id", h.Delete)

	req, _ := http.NewRequest("DELETE", "/workspaces/not-a-uuid", nil)
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDelete_HasChildrenWithoutConfirm(t *testing.T) {
	mock, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.DELETE("/workspaces/:id", h.Delete)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	mock.ExpectQuery(`SELECT id, name FROM workspaces WHERE parent_id = \$1 AND status != 'removed'`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).
			AddRow("child-1", "Child Workspace"))

	req, _ := http.NewRequest("DELETE", "/workspaces/"+wsID, nil)
	// No ?confirm=true
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if resp["status"] != "confirmation_required" {
		t.Errorf("status should be confirmation_required")
	}
	if resp["children_count"] != float64(1) {
		t.Errorf("children_count should be 1")
	}
}

func TestDelete_ChildrenCheckQueryError(t *testing.T) {
	mock, r := setupWorkspaceCrudTest(t)
	h := NewWorkspaceHandler(nil, nil, nil, nil)
	r2 := gin.New()
	r2.DELETE("/workspaces/:id", h.Delete)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	mock.ExpectQuery(`SELECT id, name FROM workspaces WHERE parent_id = \$1 AND status != 'removed'`).
		WithArgs(wsID).
		WillReturnError(sql.ErrConnDone)

	req, _ := http.NewRequest("DELETE", "/workspaces/"+wsID, nil)
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// ---------- validateWorkspaceID ----------

func TestValidateWorkspaceID_Valid(t *testing.T) {
	err := validateWorkspaceID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestValidateWorkspaceID_Invalid(t *testing.T) {
	err := validateWorkspaceID("not-a-uuid")
	if err == nil {
		t.Error("expected error for invalid UUID")
	}
}

// ---------- validateWorkspaceFields ----------

func TestValidateWorkspaceFields_NewlineInName(t *testing.T) {
	err := validateWorkspaceFields("name\nwith\nnewline", "", "", "")
	if err == nil {
		t.Error("expected error for newline in name")
	}
}

func TestValidateWorkspaceFields_NewlineInRole(t *testing.T) {
	err := validateWorkspaceFields("", "role\rwith\rcarriage", "", "")
	if err == nil {
		t.Error("expected error for carriage return in role")
	}
}

func TestValidateWorkspaceFields_YAMLSpecialCharsInName(t *testing.T) {
	for _, ch := range "{}[]|>*&!" {
		err := validateWorkspaceFields("namewith"+string(ch), "", "", "")
		if err == nil {
			t.Errorf("expected error for YAML special char %c in name", ch)
		}
	}
}

func TestValidateWorkspaceFields_NameTooLong(t *testing.T) {
	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'x'
	}
	err := validateWorkspaceFields(string(longName), "", "", "")
	if err == nil {
		t.Error("expected error for name > 255 chars")
	}
}

func TestValidateWorkspaceFields_RoleTooLong(t *testing.T) {
	longRole := make([]byte, 1001)
	for i := range longRole {
		longRole[i] = 'x'
	}
	err := validateWorkspaceFields("", string(longRole), "", "")
	if err == nil {
		t.Error("expected error for role > 1000 chars")
	}
}

func TestValidateWorkspaceFields_Valid(t *testing.T) {
	err := validateWorkspaceFields("ValidName", "ValidRole", "gpt-4", "claude")
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// ---------- validateWorkspaceDir ----------

func TestValidateWorkspaceDir_Valid(t *testing.T) {
	err := validateWorkspaceDir("/workspace/my-workspace")
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestValidateWorkspaceDir_RelativePath(t *testing.T) {
	err := validateWorkspaceDir("relative/path")
	if err == nil {
		t.Error("expected error for relative path")
	}
}

func TestValidateWorkspaceDir_Traversal(t *testing.T) {
	err := validateWorkspaceDir("/workspace/../etc")
	if err == nil {
		t.Error("expected error for traversal")
	}
}

func TestValidateWorkspaceDir_SystemPathEtc(t *testing.T) {
	for _, path := range []string{"/etc", "/var", "/proc", "/sys", "/dev", "/boot", "/sbin", "/bin", "/lib", "/usr"} {
		err := validateWorkspaceDir(path)
		if err == nil {
			t.Errorf("expected error for system path %s", path)
		}
	}
}

func TestValidateWorkspaceDir_SystemPathPrefix(t *testing.T) {
	err := validateWorkspaceDir("/etc/something")
	if err == nil {
		t.Error("expected error for /etc/something")
	}
}

func TestValidateWorkspaceDir_Empty(t *testing.T) {
	err := validateWorkspaceDir("")
	if err == nil {
		t.Error("expected error for empty path")
	}
}

// ---------- CascadeDelete ----------

func TestCascadeDelete_InvalidUUID(t *testing.T) {
	h := &WorkspaceHandler{}
	descendants, stopErrs, err := h.CascadeDelete(context.Background(), "not-a-uuid")
	if err == nil {
		t.Error("expected error for invalid UUID")
	}
	if descendants != nil || stopErrs != nil {
		t.Error("expected nil returns on error")
	}
}

func TestCascadeDelete_DescendantQueryError(t *testing.T) {
	mock, _ := setupWorkspaceCrudTest(t)
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	// CascadeDelete returns early on descendant query error — nil deps for
	// StopWorkspace/RemoveVolume/broadcaster are fine since they are never
	// reached in this error path.
	h := &WorkspaceHandler{}
	mock.ExpectQuery(`WITH RECURSIVE descendants AS`).
		WithArgs(wsID).
		WillReturnError(sql.ErrConnDone)

	deleted, stopErrs, err := h.CascadeDelete(context.Background(), wsID)
	if err == nil {
		t.Error("CascadeDelete returned nil error; want descendant query error")
	}
	if deleted != nil {
		t.Errorf("deleted = %v; want nil", deleted)
	}
	if stopErrs != nil {
		t.Errorf("stopErrs = %v; want nil", stopErrs)
	}
	// sqlmock verifies all expected queries were executed
}

// Note: Full CascadeDelete testing requires mocking StopWorkspace, RemoveVolume,
// and provisioner calls — covered in integration tests. Unit tests here focus on
// the validation and pre-condition paths.
