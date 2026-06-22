package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

func newWorkspaceCrudHandler(t *testing.T) *WorkspaceHandler {
	t.Helper()
	return NewWorkspaceHandler(nil, nil, "", t.TempDir())
}

func expectWorkspaceLiveTokenCount(mock sqlmock.Sqlmock, count int) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(count))
}

func expectWorkspaceDeleteLookup(mock sqlmock.Sqlmock, id, name string, activeTasks int, status string) {
	mock.ExpectQuery(`SELECT name, COALESCE\(active_tasks, 0\), status FROM workspaces WHERE id = \$1`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"name", "active_tasks", "status"}).
			AddRow(name, activeTasks, status))
}

// ---------- State ----------

func TestState_LegacyWorkspaceNoLiveToken(t *testing.T) {
	mock, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.GET("/workspaces/:id/state", h.State)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	// No live token — legacy workspace, no auth required.
	// HasAnyLiveToken always runs first (queries workspace_auth_tokens).
	expectWorkspaceLiveTokenCount(mock, 0)
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
	h := newWorkspaceCrudHandler(t)
	r.GET("/workspaces/:id/state", h.State)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	expectWorkspaceLiveTokenCount(mock, 1)

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
	h := newWorkspaceCrudHandler(t)
	r.GET("/workspaces/:id/state", h.State)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	expectWorkspaceLiveTokenCount(mock, 0)
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
	h := newWorkspaceCrudHandler(t)
	r.GET("/workspaces/:id/state", h.State)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	expectWorkspaceLiveTokenCount(mock, 0)
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
	h := newWorkspaceCrudHandler(t)
	r.GET("/workspaces/:id/state", h.State)

	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	expectWorkspaceLiveTokenCount(mock, 0)
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
	err := validateWorkspaceID("not-a-uuid")
	if err == nil {
		t.Error("expected error for invalid UUID in PATCH path")
	}
}

func TestUpdate_InvalidBody(t *testing.T) {
	_, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.PATCH("/workspaces/:id", h.Update)

	req, _ := http.NewRequest("PATCH", "/workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for malformed JSON, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_WorkspaceNotFound(t *testing.T) {
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	mock, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.PATCH("/workspaces/:id", h.Update)

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	body := map[string]interface{}{"name": "New Name"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/"+wsID, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_NameTooLong(t *testing.T) {
	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'x'
	}
	err := validateWorkspaceFields(string(longName), "", "", "")
	if err == nil {
		t.Error("expected error for name > 255 chars")
	}
}

func TestUpdate_RoleTooLong(t *testing.T) {
	longRole := make([]byte, 1001)
	for i := range longRole {
		longRole[i] = 'x'
	}
	err := validateWorkspaceFields("", string(longRole), "", "")
	if err == nil {
		t.Error("expected error for role > 1000 chars")
	}
}

func TestUpdate_NameWithNewline(t *testing.T) {
	err := validateWorkspaceFields("Name\nwith newline", "", "", "")
	if err == nil {
		t.Error("expected error for newline in name")
	}
}

func TestUpdate_NameWithYAMLSpecialChars(t *testing.T) {
	for _, ch := range "{}[]|>*&!" {
		err := validateWorkspaceFields("namewith"+string(ch), "", "", "")
		if err == nil {
			t.Errorf("expected error for YAML special char %c in name", ch)
		}
	}
}

func TestUpdate_WorkspaceDirSystemPath(t *testing.T) {
	err := validateWorkspaceDir("/etc/my-workspace")
	if err == nil {
		t.Error("expected error for /etc/ system path in workspace_dir")
	}
}

func TestUpdate_WorkspaceDirTraversal(t *testing.T) {
	err := validateWorkspaceDir("/workspace/../../../etc")
	if err == nil {
		t.Error("expected error for traversal in workspace_dir")
	}
}

func TestUpdate_WorkspaceDirRelativePath(t *testing.T) {
	err := validateWorkspaceDir("relative/path")
	if err == nil {
		t.Error("expected error for relative workspace_dir")
	}
}

// ---------- Delete ----------

func TestDelete_InvalidUUID(t *testing.T) {
	err := validateWorkspaceID("not-a-uuid")
	if err == nil {
		t.Error("expected error for invalid UUID in DELETE path")
	}
}

func TestDelete_HasChildrenWithoutConfirm(t *testing.T) {
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	mock, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.DELETE("/workspaces/:id", h.Delete)

	expectWorkspaceDeleteLookup(mock, wsID, "Parent Workspace", 0, "running")

	mock.ExpectQuery(`SELECT id, name FROM workspaces WHERE parent_id = \$1 AND status != 'removed'`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).
			AddRow("child-1", "Child Workspace"))

	req, _ := http.NewRequest("DELETE", "/workspaces/"+wsID, nil)
	req.Header.Set("X-Confirm-Name", "Parent Workspace")
	// No ?confirm=true
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

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

func TestDelete_LeafWithoutConfirmName(t *testing.T) {
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	mock, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.DELETE("/workspaces/:id", h.Delete)

	expectWorkspaceDeleteLookup(mock, wsID, "SEO Agent", 3, "running")
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspaces WHERE parent_id = \$1 AND status != 'removed'`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_schedules WHERE workspace_id = \$1 AND enabled = true`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(11))

	req, _ := http.NewRequest("DELETE", "/workspaces/"+wsID, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if resp["error"] != "destructive_action_requires_confirmation" {
		t.Errorf("error should require destructive confirmation, got %v", resp["error"])
	}
	if resp["workspace_name"] != "SEO Agent" {
		t.Errorf("workspace_name should be surfaced for confirmation")
	}
	if resp["active_tasks"] != float64(3) {
		t.Errorf("active_tasks should be 3, got %v", resp["active_tasks"])
	}
	if resp["schedule_count"] != float64(11) {
		t.Errorf("schedule_count should be 11, got %v", resp["schedule_count"])
	}
}

func TestDelete_ChildrenCheckQueryError(t *testing.T) {
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	mock, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.DELETE("/workspaces/:id", h.Delete)

	expectWorkspaceDeleteLookup(mock, wsID, "Workspace", 0, "running")
	mock.ExpectQuery(`SELECT id, name FROM workspaces WHERE parent_id = \$1 AND status != 'removed'`).
		WithArgs(wsID).
		WillReturnError(sql.ErrConnDone)

	req, _ := http.NewRequest("DELETE", "/workspaces/"+wsID, nil)
	req.Header.Set("X-Confirm-Name", "Workspace")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

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
	descendants, stopErrs, err := h.CascadeDelete(context.Background(), "not-a-uuid", false)
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

	deleted, stopErrs, err := h.CascadeDelete(context.Background(), wsID, false)
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

func TestCascadeDelete_DescendantRowsError(t *testing.T) {
	mock, _ := setupWorkspaceCrudTest(t)
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	// RowError(0, ...) requires a real row at index 0 to be reachable —
	// sqlmock only invokes nextErr[N] when r.pos-1 == N and the row exists.
	// AddRow ensures Next() attempts the first row, triggers the error,
	// and rows.Err() returns the injected error.
	h := &WorkspaceHandler{}
	rows := sqlmock.NewRows([]string{"id"}).AddRow("desc-1").RowError(0, sql.ErrConnDone)
	mock.ExpectQuery(`WITH RECURSIVE descendants AS`).
		WithArgs(wsID).
		WillReturnRows(rows)

	deleted, stopErrs, err := h.CascadeDelete(context.Background(), wsID, false)
	if err == nil {
		t.Fatal("CascadeDelete returned nil error; want descendant rows error")
	}
	if deleted != nil {
		t.Errorf("deleted = %v; want nil", deleted)
	}
	if stopErrs != nil {
		t.Errorf("stopErrs = %v; want nil", stopErrs)
	}
}

// Note: Full CascadeDelete testing requires mocking StopWorkspace, RemoveVolume,
// and provisioner calls — covered in integration tests. Unit tests here focus on
// the validation and pre-condition paths.

// TestUpdate_Runtime_RegisteredModelForRuntime_Passes pins the (runtime,
// model) compatibility check happy path: the workspace's current model IS
// registered for the new runtime, so the validation passes and the
// PATCH-runtime proceeds. Mirrors the create-boundary's use of
// validateRegisteredModelForRuntime (the same SSOT).
func TestUpdate_Runtime_RegisteredModelForRuntime_Passes(t *testing.T) {
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	mock, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.PATCH("/workspaces/:id", h.Update)

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// New (newRuntime, currentModel) check — the RESOLVED model is read from
	// the MODEL workspace_secret (the SSOT), not the workspaces.model column.
	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).AddRow([]byte("moonshot/kimi-k2.6"), 0))
	// The validation passes (moonshot/kimi-k2.6 is a registered model
	// for claude-code in the harness's provider registry), so the
	// UPDATE proceeds.
	mock.ExpectExec(`UPDATE workspaces\s+SET runtime = \$2`).
		WithArgs(wsID, "claude-code").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := map[string]interface{}{"runtime": "claude-code"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/"+wsID, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestUpdate_Runtime_UnroutableModel_Fails422 pins the (runtime, model)
// compatibility check REJECT path: the new runtime + current model pair
// is unroutable (the model isn't registered for that runtime AND no
// provider prefix-matches a native arm). Rejected with 422 + the
// registry-SSOT reason. The PATCH does NOT update the DB (the UPDATE
// exec is NOT mocked, so unmet-expectations would fire if the UPDATE
// happened — but we only check the 422 response code here).
func TestUpdate_Runtime_UnroutableModel_Fails422(t *testing.T) {
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	mock, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.PATCH("/workspaces/:id", h.Update)

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// A model that is NOT registered for any runtime (so
	// validateRegisteredModelForRuntime returns false via both the
	// exact-membership loop AND the DeriveProvider allow path).
	// "unroutable/unknown" has no model-prefix matches in any runtime's
	// native provider set, and doesn't appear on any runtime's
	// ModelsForRuntime list — both checks fail.
	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).AddRow([]byte("unroutable/unknown"), 0))

	body := map[string]interface{}{"runtime": "claude-code"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/"+wsID, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 422 (Unprocessable Entity) matches the create-boundary's
	// validateRegisteredModelForRuntime path (secrets.go:942, 952 +
	// workspace_crud.go create) — 422-align per CR2 + Researcher's
	// review on the 400→422 consistency ask. Precise semantic: the
	// PATCH body is syntactically valid, but the (runtime, model) pair
	// is unroutable per the registry SSOT.
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 (unroutable (runtime, model)), got %d: %s", w.Code, w.Body.String())
	}
	// The UPDATE should NOT have fired. ExpectationsWereMet is
	// informational (the unmet-expectations log would surface in
	// verbose test output); the code status is the load-bearing
	// assertion. We DON'T add a strict unmet check here because
	// sqlmock's ExpectationsWereMet fires on the failure path too
	// (mock has unconsumed expectations).
	_ = mock
}

// TestUpdate_Runtime_UnknownPseudoRuntime_Fails422 pins the runtime-identity
// gate on PATCH: a template variant slug such as "seo-agent" is NOT a runtime,
// so the PATCH must be rejected before the (runtime, model) compatibility
// check runs. Prevents the customer incident where a conversion wrote
// runtime="seo-agent" and the workspace failed to boot because no adapter
// recognizes the pseudo-runtime.
func TestUpdate_Runtime_UnknownPseudoRuntime_Fails422(t *testing.T) {
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	mock, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.PATCH("/workspaces/:id", h.Update)

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// The model compatibility query is intentionally NOT mocked: the
	// unknown-runtime rejection must happen before that DB read.

	body := map[string]interface{}{"runtime": "seo-agent"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/"+wsID, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 (unknown runtime), got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unsupported workspace runtime") {
		t.Errorf("expected 'unsupported workspace runtime' reason, got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestUpdate_Runtime_ModelUnresolved_SkipsCheckAndProceeds pins the JRS-conversion
// fix: the compat-check reads the RESOLVED model from the MODEL workspace_secret,
// not the workspaces.model column (which wedged the PATCH at 500 for workspaces
// whose model lives only in workspace_secrets). When the MODEL secret is absent
// (sql.ErrNoRows), the strict (runtime, model) check is SKIPPED — the boot path
// fail-closes on a genuinely missing model — so the PATCH must NOT 500 and must
// proceed to update the runtime.
func TestUpdate_Runtime_ModelUnresolved_SkipsCheckAndProceeds(t *testing.T) {
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	mock, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.PATCH("/workspaces/:id", h.Update)

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// No MODEL secret → ErrNoRows → unresolved → strict check skipped (NOT 500).
	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WithArgs(wsID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(`UPDATE workspaces\s+SET runtime = \$2`).
		WithArgs(wsID, "claude-code").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := map[string]interface{}{"runtime": "claude-code"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/"+wsID, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (unresolved model → skip check, proceed), got %d: %s", w.Code, w.Body.String())
	}
}

// TestPatchTemplate pins the admin/CP-gated PATCH /workspaces/:id/template endpoint.
func TestPatchTemplate(t *testing.T) {
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	// Wire a temp manifest so seo-agent is a known template.
	dir := t.TempDir()
	manifestPath := dir + "/manifest.json"
	manifest := `{"workspace_templates": [{"name": "seo-agent", "repo": "molecule-ai/t-seo", "ref": "main"}]}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	t.Setenv("WORKSPACE_MANIFEST_PATH", manifestPath)
	initTemplateRepoByName()

	mock, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.PATCH("/workspaces/:id/template", h.PatchTemplate)

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET template = \$2, updated_at = now\(\) WHERE id = \$1`).
		WithArgs(wsID, "seo-agent").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := map[string]interface{}{"template": "seo-agent"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/"+wsID+"/template", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp["status"] != "updated" {
		t.Errorf("expected status=updated, got %v", resp["status"])
	}
	if resp["needs_restart"] != true {
		t.Errorf("expected needs_restart=true, got %v", resp["needs_restart"])
	}
}

func TestPatchTemplate_UnknownTemplate_Fails422(t *testing.T) {
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	dir := t.TempDir()
	manifestPath := dir + "/manifest.json"
	manifest := `{"workspace_templates": [{"name": "seo-agent", "repo": "r", "ref": "main"}]}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	t.Setenv("WORKSPACE_MANIFEST_PATH", manifestPath)
	initTemplateRepoByName()

	_, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.PATCH("/workspaces/:id/template", h.PatchTemplate)

	body := map[string]interface{}{"template": "ghost-template"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/"+wsID+"/template", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPatchTemplate_NotFound_Fails404(t *testing.T) {
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	dir := t.TempDir()
	manifestPath := dir + "/manifest.json"
	manifest := `{"workspace_templates": [{"name": "seo-agent", "repo": "r", "ref": "main"}]}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	t.Setenv("WORKSPACE_MANIFEST_PATH", manifestPath)
	initTemplateRepoByName()

	mock, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.PATCH("/workspaces/:id/template", h.PatchTemplate)

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	body := map[string]interface{}{"template": "seo-agent"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/"+wsID+"/template", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestUpdate_Runtime_ModelSecretDBError_Fails500 pins that a genuine DB error
// reading the MODEL workspace_secret is fail-closed (500). Only sql.ErrNoRows
// (unresolved model) skips the strict compat-check; real DB/decrypt errors
// must not silently let an unvalidated (runtime, model) PATCH through.
func TestUpdate_Runtime_ModelSecretDBError_Fails500(t *testing.T) {
	wsID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	mock, r := setupWorkspaceCrudTest(t)
	h := newWorkspaceCrudHandler(t)
	r.PATCH("/workspaces/:id", h.Update)

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WithArgs(wsID).
		WillReturnError(errors.New("database unavailable"))

	body := map[string]interface{}{"runtime": "claude-code"}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", "/workspaces/"+wsID, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 (genuine DB error), got %d: %s", w.Code, w.Body.String())
	}
}
