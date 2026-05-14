package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ─── request helpers ───────────────────────────────────────────────────────────

func newPostRequest(path string, body interface{}) (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	raw, _ := json.Marshal(body)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	return w, c
}

func newPutRequest(path string, body interface{}) (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	raw, _ := json.Marshal(body)
	c.Request = httptest.NewRequest(http.MethodPut, path, bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	return w, c
}

func newDeleteRequest(path string) (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, path, nil)
	return w, c
}

func newGetRequest(path string) (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, path, nil)
	return w, c
}

// ─── mock row helpers ─────────────────────────────────────────────────────────

// instructionCols matches the SELECT in List/Resolve.
var instructionCols = []string{
	"id", "scope", "scope_target", "title", "content",
	"priority", "enabled", "created_at", "updated_at",
}

// resolveCols matches the SELECT in Resolve (scope, title, content).
var resolveCols = []string{"scope", "title", "content"}

// ─── List ────────────────────────────────────────────────────────────────────

func TestInstructionsList_ByWorkspaceID(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	wsID := "ws-123-abc"
	w, c := newGetRequest("/instructions?workspace_id=" + wsID)
	c.Request = httptest.NewRequest(http.MethodGet, "/instructions?workspace_id="+wsID, nil)

	rows := sqlmock.NewRows(instructionCols).
		AddRow("inst-1", "global", nil, "Be helpful", "Always be helpful.", 10, true, time.Now(), time.Now()).
		AddRow("inst-2", "workspace", &wsID, "Use Claude", "Use Claude Code.", 5, true, time.Now(), time.Now())
	mock.ExpectQuery("SELECT id, scope, scope_target, title, content, priority, enabled, created_at, updated_at").
		WithArgs(wsID).
		WillReturnRows(rows)

	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out []Instruction
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("expected 2 instructions, got %d", len(out))
	}
	if out[0].Scope != "global" {
		t.Errorf("first row scope: expected global, got %s", out[0].Scope)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsList_ByScope(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	w, c := newGetRequest("/instructions?scope=global")
	c.Request = httptest.NewRequest(http.MethodGet, "/instructions?scope=global", nil)

	rows := sqlmock.NewRows(instructionCols).
		AddRow("inst-g", "global", nil, "Global Rule", "Follow policy.", 10, true, time.Now(), time.Now())
	mock.ExpectQuery("SELECT id, scope, scope_target, title, content, priority, enabled, created_at, updated_at FROM platform_instructions WHERE 1=1").
		WithArgs("global").
		WillReturnRows(rows)

	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out []Instruction
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	if len(out) != 1 || out[0].Scope != "global" {
		t.Errorf("unexpected response: %v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsList_AllNoParams(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	w, c := newGetRequest("/instructions")

	rows := sqlmock.NewRows(instructionCols)
	mock.ExpectQuery("SELECT id, scope, scope_target, title, content, priority, enabled, created_at, updated_at FROM platform_instructions WHERE 1=1").
		WillReturnRows(rows)

	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out []Instruction
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	// Empty slice, not nil
	if out == nil {
		t.Error("expected empty slice, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsList_DBError(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	w, c := newGetRequest("/instructions")
	c.Request = httptest.NewRequest(http.MethodGet, "/instructions", nil)

	mock.ExpectQuery("SELECT id, scope, scope_target, title, content, priority, enabled, created_at, updated_at FROM platform_instructions WHERE 1=1").
		WillReturnError(errors.New("connection refused"))

	h.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ─── Create ───────────────────────────────────────────────────────────────────

func TestInstructionsCreate_ValidGlobal(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope":    "global",
		"title":    "Be Helpful",
		"content":  "Always be helpful to the user.",
		"priority": 10,
	})

	mock.ExpectQuery("INSERT INTO platform_instructions").
		WithArgs("global", nil, "Be Helpful", "Always be helpful to the user.", 10).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("new-inst-1"))

	h.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	if out["id"] != "new-inst-1" {
		t.Errorf("expected id new-inst-1, got %s", out["id"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsCreate_ValidWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()
	wsTarget := "ws-xyz-789"

	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope":        "workspace",
		"scope_target": wsTarget,
		"title":        "Use Claude Code",
		"content":      "Prefer Claude Code for all tasks.",
		"priority":     5,
	})

	mock.ExpectQuery("INSERT INTO platform_instructions").
		WithArgs("workspace", &wsTarget, "Use Claude Code", "Prefer Claude Code for all tasks.", 5).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-inst-2"))

	h.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsCreate_MissingScope(t *testing.T) {
	setupTestDB(t)
	h := NewInstructionsHandler()

	w, c := newPostRequest("/instructions", map[string]interface{}{
		"title":   "Missing Scope",
		"content": "This has no scope.",
	})

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionsCreate_MissingTitle(t *testing.T) {
	setupTestDB(t)
	h := NewInstructionsHandler()

	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope":   "global",
		"content": "Has no title.",
	})

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionsCreate_MissingContent(t *testing.T) {
	setupTestDB(t)
	h := NewInstructionsHandler()

	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope": "global",
		"title": "Has no content",
	})

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionsCreate_InvalidScope(t *testing.T) {
	setupTestDB(t)
	h := NewInstructionsHandler()

	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope":   "team",
		"title":   "Bad Scope",
		"content": "Team scope is not supported yet.",
	})

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionsCreate_WorkspaceScopeNoTarget(t *testing.T) {
	setupTestDB(t)
	h := NewInstructionsHandler()

	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope":   "workspace",
		"title":   "Missing Target",
		"content": "Workspace scope without scope_target.",
	})

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionsCreate_ContentTooLong(t *testing.T) {
	setupTestDB(t)
	h := NewInstructionsHandler()

	// Build a string longer than maxInstructionContentLen (8192).
	longContent := string(make([]byte, maxInstructionContentLen+1))

	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope":   "global",
		"title":   "Too Long",
		"content": longContent,
	})

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionsCreate_TitleTooLong(t *testing.T) {
	setupTestDB(t)
	h := NewInstructionsHandler()

	longTitle := string(make([]byte, 201))

	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope":   "global",
		"title":   longTitle,
		"content": "Short content.",
	})

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionsCreate_DBError(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope":   "global",
		"title":   "DB Error",
		"content": "This will fail.",
	})

	mock.ExpectQuery("INSERT INTO platform_instructions").
		WillReturnError(errors.New("connection refused"))

	h.Create(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ─── Update ──────────────────────────────────────────────────────────────────

func TestInstructionsUpdate_ValidPartial(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	instID := "inst-update-1"
	newTitle := "Updated Title"
	w, c := newPutRequest("/instructions/"+instID, map[string]interface{}{
		"title": newTitle,
	})
	c.Params = []gin.Param{{Key: "id", Value: instID}}

	mock.ExpectExec("UPDATE platform_instructions SET").
		WithArgs(instID, &newTitle, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsUpdate_AllFields(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	instID := "inst-update-2"
	title := "Full Update"
	content := "New content body."
	priority := 20
	enabled := false
	w, c := newPutRequest("/instructions/"+instID, map[string]interface{}{
		"title":    title,
		"content":  content,
		"priority": priority,
		"enabled":  enabled,
	})
	c.Params = []gin.Param{{Key: "id", Value: instID}}

	mock.ExpectExec("UPDATE platform_instructions SET").
		WithArgs(instID, &title, &content, &priority, &enabled).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsUpdate_ContentTooLong(t *testing.T) {
	setupTestDB(t)
	h := NewInstructionsHandler()

	instID := "inst-too-long"
	longContent := string(make([]byte, maxInstructionContentLen+1))
	w, c := newPutRequest("/instructions/"+instID, map[string]interface{}{
		"content": longContent,
	})
	c.Params = []gin.Param{{Key: "id", Value: instID}}

	h.Update(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionsUpdate_TitleTooLong(t *testing.T) {
	setupTestDB(t)
	h := NewInstructionsHandler()

	instID := "inst-title-long"
	longTitle := string(make([]byte, 201))
	w, c := newPutRequest("/instructions/"+instID, map[string]interface{}{
		"title": longTitle,
	})
	c.Params = []gin.Param{{Key: "id", Value: instID}}

	h.Update(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionsUpdate_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	instID := "inst-missing"
	w, c := newPutRequest("/instructions/"+instID, map[string]interface{}{
		"title": "New Title",
	})
	c.Params = []gin.Param{{Key: "id", Value: instID}}

	mock.ExpectExec("UPDATE platform_instructions SET").
		WillReturnResult(sqlmock.NewResult(0, 0))

	h.Update(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsUpdate_DBError(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	instID := "inst-db-err"
	w, c := newPutRequest("/instructions/"+instID, map[string]interface{}{
		"title": "Error Update",
	})
	c.Params = []gin.Param{{Key: "id", Value: instID}}

	mock.ExpectExec("UPDATE platform_instructions SET").
		WillReturnError(errors.New("connection refused"))

	h.Update(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ─── Delete ───────────────────────────────────────────────────────────────────

func TestInstructionsDelete_Valid(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	instID := "inst-delete-1"
	w, c := newDeleteRequest("/instructions/" + instID)
	c.Params = []gin.Param{{Key: "id", Value: instID}}

	mock.ExpectExec(`DELETE FROM platform_instructions WHERE id = \$1`).
		WithArgs(instID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.Delete(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsDelete_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	instID := "inst-not-there"
	w, c := newDeleteRequest("/instructions/" + instID)
	c.Params = []gin.Param{{Key: "id", Value: instID}}

	mock.ExpectExec(`DELETE FROM platform_instructions WHERE id = \$1`).
		WithArgs(instID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	h.Delete(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsDelete_DBError(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	instID := "inst-del-err"
	w, c := newDeleteRequest("/instructions/" + instID)
	c.Params = []gin.Param{{Key: "id", Value: instID}}

	mock.ExpectExec(`DELETE FROM platform_instructions WHERE id = \$1`).
		WithArgs(instID).
		WillReturnError(errors.New("connection refused"))

	h.Delete(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ─── Resolve ──────────────────────────────────────────────────────────────────

func TestInstructionsResolve_GlobalThenWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	wsID := "ws-resolve-1"
	w, c := newGetRequest("/workspaces/" + wsID + "/instructions/resolve")
	c.Params = []gin.Param{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/instructions/resolve", nil)

	rows := sqlmock.NewRows(resolveCols).
		AddRow("global", "Be Helpful", "Always help the user.").
		AddRow("global", "Stay on Topic", "Don't diverge.").
		AddRow("workspace", "Use Claude Code", "Claude Code is the default runtime.")
	mock.ExpectQuery("SELECT scope, title, content FROM platform_instructions").
		WithArgs(wsID).
		WillReturnRows(rows)

	h.Resolve(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		WorkspaceID   string `json:"workspace_id"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	if out.WorkspaceID != wsID {
		t.Errorf("expected workspace_id %s, got %s", wsID, out.WorkspaceID)
	}
	// Global section must come before workspace section.
	if !bytes.Contains([]byte(out.Instructions), []byte("Platform-Wide Rules")) {
		t.Error("instructions should contain 'Platform-Wide Rules' section")
	}
	if !bytes.Contains([]byte(out.Instructions), []byte("Role-Specific Rules")) {
		t.Error("instructions should contain 'Role-Specific Rules' section")
	}
	// Global instructions must appear before workspace instructions.
	idxGlobal := bytes.Index([]byte(out.Instructions), []byte("Platform-Wide Rules"))
	idxWorkspace := bytes.Index([]byte(out.Instructions), []byte("Role-Specific Rules"))
	if idxGlobal >= idxWorkspace {
		t.Error("global section should appear before workspace section")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsResolve_EmptyWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	wsID := "ws-empty"
	w, c := newGetRequest("/workspaces/" + wsID + "/instructions/resolve")
	c.Params = []gin.Param{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/instructions/resolve", nil)

	rows := sqlmock.NewRows(resolveCols)
	mock.ExpectQuery("SELECT scope, title, content FROM platform_instructions").
		WithArgs(wsID).
		WillReturnRows(rows)

	h.Resolve(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	// No rows → builder writes nothing; empty string returned.
	if out.Instructions != "" {
		t.Errorf("expected empty instructions for empty workspace, got: %q", out.Instructions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsResolve_DBError(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	wsID := "ws-err"
	w, c := newGetRequest("/workspaces/" + wsID + "/instructions/resolve")
	c.Params = []gin.Param{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/instructions/resolve", nil)

	mock.ExpectQuery("SELECT scope, title, content FROM platform_instructions").
		WithArgs(wsID).
		WillReturnError(errors.New("connection refused"))

	h.Resolve(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestInstructionsResolve_MissingWorkspaceID(t *testing.T) {
	setupTestDB(t)
	h := NewInstructionsHandler()

	w, c := newGetRequest("/workspaces//instructions/resolve")
	c.Params = []gin.Param{{Key: "id", Value: ""}}

	h.Resolve(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ─── scanInstructions edge cases ───────────────────────────────────────────────

// NOTE: TestScanInstructions_ScanError was removed — go-sqlmock v1.5.2 does not
// implement Go 1.25's sql.Rows.Next([]byte) bool method, so *sqlmock.Rows cannot
// satisfy scanInstructions' interface. The test needs a sqlmock upgrade or a
// different mocking strategy (tracked: internal issue).

// ─── maxInstructionContentLen boundary ────────────────────────────────────────

func TestInstructionsCreate_ContentExactlyAtLimit(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	exactContent := string(make([]byte, maxInstructionContentLen))
	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope":   "global",
		"title":   "At Limit",
		"content": exactContent,
	})

	mock.ExpectQuery("INSERT INTO platform_instructions").
		WithArgs("global", nil, "At Limit", exactContent, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("at-limit-1"))

	h.Create(c)

	// Exactly at limit must succeed (8192 chars is acceptable).
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for content at limit, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ─── priority defaults ────────────────────────────────────────────────────────

func TestInstructionsCreate_PriorityDefaultsToZero(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	// Body omits priority — expect it defaults to 0.
	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope":   "global",
		"title":   "No Priority",
		"content": "Default priority body.",
	})

	mock.ExpectQuery("INSERT INTO platform_instructions").
		WithArgs("global", nil, "No Priority", "Default priority body.", 0).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("no-prio-1"))

	h.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ─── nil scope_target for global instructions ─────────────────────────────────

func TestInstructionsCreate_GlobalScopeNilTarget(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope":   "global",
		"title":   "Global Nil Target",
		"content": "Global instruction.",
	})

	// For global scope, scope_target must be SQL NULL.
	mock.ExpectQuery("INSERT INTO platform_instructions").
		WithArgs("global", nil, "Global Nil Target", "Global instruction.", 0).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("global-nil-1"))

	h.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ─── workspace scope with empty string target (rejected) ─────────────────────

func TestInstructionsCreate_WorkspaceScopeEmptyStringTarget(t *testing.T) {
	setupTestDB(t)
	h := NewInstructionsHandler()

	empty := ""
	w, c := newPostRequest("/instructions", map[string]interface{}{
		"scope":        "workspace",
		"scope_target": empty,
		"title":        "Empty Target",
		"content":      "Empty workspace target.",
	})

	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty string scope_target, got %d: %s", w.Code, w.Body.String())
	}
}

// ─── Resolve: scope label transitions ────────────────────────────────────────

func TestInstructionsResolve_ScopeTransitionOnlyGlobal(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	wsID := "ws-only-global"
	w, c := newGetRequest("/workspaces/" + wsID + "/instructions/resolve")
	c.Params = []gin.Param{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/instructions/resolve", nil)

	rows := sqlmock.NewRows(resolveCols).
		AddRow("global", "Rule One", "First rule.").
		AddRow("global", "Rule Two", "Second rule.")
	mock.ExpectQuery("SELECT scope, title, content FROM platform_instructions").
		WithArgs(wsID).
		WillReturnRows(rows)

	h.Resolve(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	// Two global instructions share one section header.
	if bytes.Count([]byte(out.Instructions), []byte("Platform-Wide Rules")) != 1 {
		t.Error("expect exactly one 'Platform-Wide Rules' header for consecutive global rows")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ─── Update: empty body (all nil — no-op update) ─────────────────────────────

func TestInstructionsUpdate_EmptyBody(t *testing.T) {
	mock := setupTestDB(t)
	h := NewInstructionsHandler()

	instID := "inst-empty-update"
	w, c := newPutRequest("/instructions/"+instID, map[string]interface{}{})
	c.Params = []gin.Param{{Key: "id", Value: instID}}

	// COALESCE(nil, ...) = unchanged; still updates updated_at.
	// Args order: ($1=id, $2=title, $3=content, $4=priority, $5=enabled)
	mock.ExpectExec("UPDATE platform_instructions SET").
		WithArgs(instID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty body, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
