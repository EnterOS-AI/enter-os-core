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
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/gin-gonic/gin"
)

// ── List ─────────────────────────────────────────────────────────────────────────

func TestInstructionsHandler_List_EmptyResult(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()

	mock.ExpectQuery("SELECT id, scope, scope_target, title, content, priority, enabled, created_at, updated_at FROM platform_instructions WHERE 1=1 ORDER BY scope, priority DESC, created_at").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "scope", "scope_target", "title", "content", "priority", "enabled", "created_at", "updated_at",
		}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/instructions", nil)

	handler.List(c)

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
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["id"] != "new-inst-id" {
		t.Errorf("expected id 'new-inst-id', got %q", resp["id"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInstructionsHandler_Create_InvalidScope(t *testing.T) {
	setupTestDB(t)
	handler := NewInstructionsHandler()

	body, _ := json.Marshal(map[string]interface{}{
		"scope":   "team",
		"title":   "Test",
		"content": "Test content",
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/instructions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.BadRequest {
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

func TestInstructionsHandler_Create_WorkspaceScopeWithScopeTarget(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()
	wsID := "ws-abc-123"

	mock.ExpectQuery("INSERT INTO platform_instructions").
		WithArgs("workspace", &wsID, "WS rule", "Use HTTPS", 10).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-inst-1"))

	body, _ := json.Marshal(map[string]interface{}{
		"scope":        "workspace",
		"scope_target": wsID,
		"title":        "WS rule",
		"content":      "Use HTTPS",
		"priority":      10,
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/instructions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// ── Update ────────────────────────────────────────────────────────────────────

func TestInstructionsHandler_Update_Success(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewInstructionsHandler()
	title := "Updated title"

	mock.ExpectExec(regexp.QuoteMeta("UPDATE platform_instructions SET\n\t\t\t\ttitle = COALESCE($2, title),\n\t\t\t\tcontent = COALESCE($3, content),\n\t\t\t\tpriority = COALESCE($4, priority),\n\t\t\t\tenabled = COALESCE($5, enabled),\n\t\t\t\tupdated_at = NOW()\n\t\t\t\tWHERE id = $1")).
		WithArgs(&title, "inst-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body, _ := json.Marshal(map[string]interface{}{"title": "Updated title"})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "inst-1"}}
	c.Request = httptest.NewRequest("PUT", "/instructions/inst-1", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

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
	title := "Updated title"

	mock.ExpectExec(regexp.QuoteMeta("UPDATE platform_instructions SET\n\t\t\t\ttitle = COALESCE($2, title),\n\t\t\t\tcontent = COALESCE($3, content),\n\t\t\t\tpriority = COALESCE($4, priority),\n\t\t\t\tenabled = COALESCE($5, enabled),\n\t\t\t\tupdated_at = NOW()\n\t\t\t\tWHERE id = $1")).
		WithArgs(&title, "nonexistent").
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
