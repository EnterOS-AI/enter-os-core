package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
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
	var result []Instruction
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected 0 instructions, got %d", len(result))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInstructionsHandler_List_WithScopeFilter(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()

	rows := sqlmock.NewRows([]string{
		"id", "scope", "scope_target", "title", "content", "priority", "enabled", "created_at", "updated_at",
	}).AddRow("inst-1", "global", nil, "Be kind", "Always be kind", 10, true,
		time.Now(), time.Now())

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, scope, scope_target, title, content, priority, enabled, created_at, updated_at FROM platform_instructions WHERE 1=1 AND scope = $1 ORDER BY scope, priority DESC, created_at")).
		WithArgs("global").
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/instructions?scope=global", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result []Instruction
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 instruction, got %d", len(result))
	}
	if result[0].Scope != "global" {
		t.Errorf("expected scope 'global', got %q", result[0].Scope)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInstructionsHandler_List_WithWorkspaceID(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()
	wsID := "ws-test-123"

	rows := sqlmock.NewRows([]string{
		"id", "scope", "scope_target", "title", "content", "priority", "enabled", "created_at", "updated_at",
	}).AddRow("inst-1", "global", nil, "Global rule", "Stay safe", 5, true,
		time.Now(), time.Now()).
		AddRow("inst-2", "workspace", &wsID, "WS rule", "Use HTTPS", 10, true,
			time.Now(), time.Now())

	mock.ExpectQuery("SELECT id, scope, scope_target, title, content, priority, enabled, created_at, updated_at FROM platform_instructions WHERE enabled = true AND \\(").
		WithArgs(wsID).
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/instructions?workspace_id="+wsID, nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result []Instruction
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 instructions, got %d", len(result))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInstructionsHandler_List_QueryError(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()

	mock.ExpectQuery("SELECT id, scope, scope_target, title, content, priority, enabled, created_at, updated_at FROM platform_instructions WHERE 1=1").
		WillReturnError(context.DeadlineExceeded)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/instructions", nil)

	handler.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ── Create ──────────────────────────────────────────────────────────────────────

func TestInstructionsHandler_Create_Success(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()

	mock.ExpectQuery("INSERT INTO platform_instructions").
		WithArgs("global", nil, "Be kind", "Always be kind", 5).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("new-inst-id"))

	body, _ := json.Marshal(map[string]interface{}{
		"scope":    "global",
		"title":    "Be kind",
		"content":  "Always be kind",
		"priority": 5,
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/instructions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

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

func TestInstructionsHandler_Create_WorkspaceScopeMissingScopeTarget(t *testing.T) {
	setupTestDB(t)
	handler := NewInstructionsHandler()

	body, _ := json.Marshal(map[string]interface{}{
		"scope":   "workspace",
		"title":   "Test",
		"content": "Test content",
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/instructions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionsHandler_Create_ContentTooLong(t *testing.T) {
	setupTestDB(t)
	handler := NewInstructionsHandler()

	longContent := string(bytes.Repeat([]byte("x"), 8193))
	body, _ := json.Marshal(map[string]interface{}{
		"scope":   "global",
		"title":   "Test",
		"content": longContent,
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/instructions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionsHandler_Create_TitleTooLong(t *testing.T) {
	setupTestDB(t)
	handler := NewInstructionsHandler()

	longTitle := string(bytes.Repeat([]byte("x"), 201))
	body, _ := json.Marshal(map[string]interface{}{
		"scope":   "global",
		"title":   longTitle,
		"content": "Short content",
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/instructions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

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
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInstructionsHandler_Update_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()

	mock.ExpectExec(regexp.QuoteMeta("UPDATE platform_instructions SET\n\t\t\t\ttitle = COALESCE($2, title),\n\t\t\t\tcontent = COALESCE($3, content),\n\t\t\t\tpriority = COALESCE($4, priority),\n\t\t\t\tenabled = COALESCE($5, enabled),\n\t\t\t\tupdated_at = NOW()\n\t\t\t\tWHERE id = $1")).
		WithArgs("nonexistent", sqlmock.AnyArg(), nil, nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 0))

	body, _ := json.Marshal(map[string]interface{}{"title": "Updated title"})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "nonexistent"}}
	c.Request = httptest.NewRequest("PUT", "/instructions/nonexistent", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInstructionsHandler_Update_ContentTooLong(t *testing.T) {
	setupTestDB(t)
	handler := NewInstructionsHandler()

	longContent := string(bytes.Repeat([]byte("x"), 8193))
	body, _ := json.Marshal(map[string]interface{}{"content": longContent})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "inst-1"}}
	c.Request = httptest.NewRequest("PUT", "/instructions/inst-1", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstructionsHandler_Update_TitleTooLong(t *testing.T) {
	setupTestDB(t)
	handler := NewInstructionsHandler()

	longTitle := string(bytes.Repeat([]byte("x"), 201))
	body, _ := json.Marshal(map[string]interface{}{"title": longTitle})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "inst-1"}}
	c.Request = httptest.NewRequest("PUT", "/instructions/inst-1", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ── Delete ─────────────────────────────────────────────────────────────────────

func TestInstructionsHandler_Delete_Success(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM platform_instructions WHERE id = $1")).
		WithArgs("inst-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "inst-1"}}
	c.Request = httptest.NewRequest("DELETE", "/instructions/inst-1", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInstructionsHandler_Delete_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM platform_instructions WHERE id = $1")).
		WithArgs("nonexistent").
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "nonexistent"}}
	c.Request = httptest.NewRequest("DELETE", "/instructions/nonexistent", nil)

	handler.Delete(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// ── Resolve ────────────────────────────────────────────────────────────────────

func TestInstructionsHandler_Resolve_Empty(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()
	wsID := "ws-resolve-1"

	mock.ExpectQuery("SELECT scope, title, content FROM platform_instructions WHERE enabled = true AND").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"scope", "title", "content"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/instructions/resolve", nil)

	handler.Resolve(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["workspace_id"] != wsID {
		t.Errorf("expected workspace_id %q, got %v", wsID, resp["workspace_id"])
	}
	if resp["instructions"] != "" {
		t.Errorf("expected empty instructions, got %q", resp["instructions"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInstructionsHandler_Resolve_WithInstructions(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()
	wsID := "ws-resolve-2"

	rows := sqlmock.NewRows([]string{"scope", "title", "content"}).
		AddRow("global", "Be safe", "No SSRF").
		AddRow("workspace", "WS Rule", "Use HTTPS")

	mock.ExpectQuery("SELECT scope, title, content FROM platform_instructions WHERE enabled = true AND").
		WithArgs(wsID).
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/instructions/resolve", nil)

	handler.Resolve(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	instructions, ok := resp["instructions"].(string)
	if !ok {
		t.Fatalf("instructions field is not a string: %T", resp["instructions"])
	}
	if instructions == "" {
		t.Fatalf("expected non-empty instructions")
	}
	// Verify scope headers are present
	if !bytes.Contains([]byte(instructions), []byte("Platform-Wide Rules")) {
		t.Errorf("expected 'Platform-Wide Rules' header in instructions")
	}
	if !bytes.Contains([]byte(instructions), []byte("Role-Specific Rules")) {
		t.Errorf("expected 'Role-Specific Rules' header in instructions")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInstructionsHandler_Resolve_MissingWorkspaceID(t *testing.T) {
	setupTestDB(t)
	handler := NewInstructionsHandler()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: ""}}
	c.Request = httptest.NewRequest("GET", "/workspaces//instructions/resolve", nil)

	handler.Resolve(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// scanInstructions is called by the List handler — verify it handles
// rows.Err() gracefully without panicking.
func TestInstructionsHandler_List_ScanErrorContinues(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()

	rows := sqlmock.NewRows([]string{
		"id", "scope", "scope_target", "title", "content", "priority", "enabled", "created_at", "updated_at",
	}).AddRow("inst-1", "global", nil, "Good", "Content here", 5, true, time.Now(), time.Now()).
		RowError(1, context.DeadlineExceeded) // error on row 2 (if it existed)

	mock.ExpectQuery("SELECT id, scope, scope_target, title, content, priority, enabled, created_at, updated_at FROM platform_instructions WHERE 1=1").
		WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/instructions", nil)

	handler.List(c)

	// Should still return 200 and the one valid row
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result []Instruction
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// The valid row should still be returned (error is logged, not fatal)
	if len(result) != 1 {
		t.Fatalf("expected 1 instruction despite row error, got %d", len(result))
	}
}
