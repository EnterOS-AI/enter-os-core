package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ==================== validateRelPath ====================

func TestValidateRelPath_Valid(t *testing.T) {
	cases := []string{
		"config.yaml",
		"skills/my-skill/SKILL.md",
		"system-prompt.md",
		"a/b/c.txt",
	}
	for _, tc := range cases {
		if err := validateRelPath(tc); err != nil {
			t.Errorf("expected valid path %q, got error: %v", tc, err)
		}
	}
}

func TestValidateRelPath_Invalid(t *testing.T) {
	cases := []string{
		"../etc/passwd",
		"../../secrets",
		"/absolute/path",
	}
	for _, tc := range cases {
		if err := validateRelPath(tc); err == nil {
			t.Errorf("expected error for path %q, got nil", tc)
		}
	}
}

// ==================== GET /templates ====================

func TestTemplatesList_EmptyDir(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("expected empty list, got %d items", len(resp))
	}
}

func TestTemplatesList_WithTemplates(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()

	// Create a template directory with config.yaml
	tmplDir := filepath.Join(tmpDir, "test-agent")
	os.MkdirAll(tmplDir, 0755)
	configYaml := `name: Test Agent
description: A test agent
tier: 2
model: anthropic:claude-sonnet-4-20250514
skills:
  - web-search
  - code-review
`
	os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644)

	// Create a non-directory file (should be skipped)
	os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# readme"), 0644)

	// Create a directory without config.yaml (should be skipped)
	os.MkdirAll(filepath.Join(tmpDir, "no-config"), 0755)

	handler := NewTemplatesHandler(tmpDir, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 template, got %d", len(resp))
	}
	if resp[0].ID != "test-agent" {
		t.Errorf("expected ID 'test-agent', got %q", resp[0].ID)
	}
	if resp[0].Name != "Test Agent" {
		t.Errorf("expected Name 'Test Agent', got %q", resp[0].Name)
	}
	if resp[0].Tier != 2 {
		t.Errorf("expected Tier 2, got %d", resp[0].Tier)
	}
	if resp[0].SkillCount != 2 {
		t.Errorf("expected SkillCount 2, got %d", resp[0].SkillCount)
	}
}

func TestTemplatesList_RuntimeAndModelsRegistry(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "hermes")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Hermes Agent
description: test
tier: 2
runtime: hermes
runtime_config:
  model: nous-hermes-3-70b
  models:
    - id: nous-hermes-3-70b
      name: Nous Hermes 3 70B
      required_env: [HERMES_API_KEY]
    - id: minimax/minimax-m2.7
      name: MiniMax M2.7 (via OpenRouter)
      required_env: [OPENROUTER_API_KEY]
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 template, got %d", len(resp))
	}
	got := resp[0]
	if got.Runtime != "hermes" {
		t.Errorf("Runtime: want hermes, got %q", got.Runtime)
	}
	if got.Model != "nous-hermes-3-70b" {
		t.Errorf("Model: want nous-hermes-3-70b (from runtime_config.model), got %q", got.Model)
	}
	if len(got.Models) != 2 {
		t.Fatalf("Models: want 2, got %d", len(got.Models))
	}
	if got.Models[0].ID != "nous-hermes-3-70b" || got.Models[0].Name != "Nous Hermes 3 70B" {
		t.Errorf("Models[0] id/name mismatch: %+v", got.Models[0])
	}
	if len(got.Models[0].RequiredEnv) != 1 || got.Models[0].RequiredEnv[0] != "HERMES_API_KEY" {
		t.Errorf("Models[0] required_env: want [HERMES_API_KEY], got %+v", got.Models[0].RequiredEnv)
	}
	if got.Models[1].ID != "minimax/minimax-m2.7" {
		t.Errorf("Models[1].ID: got %q", got.Models[1].ID)
	}
	if len(got.Models[1].RequiredEnv) != 1 || got.Models[1].RequiredEnv[0] != "OPENROUTER_API_KEY" {
		t.Errorf("Models[1] required_env: want [OPENROUTER_API_KEY], got %+v", got.Models[1].RequiredEnv)
	}
}

// TestTemplatesList_SurfacesProviders pins the Option B PR-5 wiring:
// /templates must echo runtime_config.providers from the template's
// config.yaml into the JSON response. Canvas reads this list to
// populate the Provider override dropdown WITHOUT hardcoding any
// provider taxonomy on the frontend — that's the "data-driven from
// adapter" invariant.
//
// If a future yaml-tag rename or struct edit drops the field, every
// runtime would silently fall back to model-prefix derivation. For
// hermes specifically (default model has no clean prefix), that
// degrades the dropdown to empty and reintroduces the "No LLM
// provider configured" UX gap from 2026-05-01.
func TestTemplatesList_SurfacesProviders(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "hermes-prov")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Hermes
description: test
tier: 2
runtime: hermes
runtime_config:
  model: nousresearch/hermes-4-70b
  providers:
    - nous
    - openrouter
    - anthropic
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 template, got %d", len(resp))
	}
	got := resp[0]
	want := []string{"nous", "openrouter", "anthropic"}
	if len(got.Providers) != len(want) {
		t.Fatalf("Providers: want %v, got %v", want, got.Providers)
	}
	for i, p := range want {
		if got.Providers[i] != p {
			t.Errorf("Providers[%d]: want %q, got %q", i, p, got.Providers[i])
		}
	}

	// Cross-check the JSON wire shape directly — canvas reads the field
	// as `providers` (lowercase) and a struct-tag rename here would
	// break consumers without surfacing in the typed assertions above.
	if !strings.Contains(w.Body.String(), `"providers":["nous","openrouter","anthropic"]`) {
		t.Errorf("response missing providers JSON field: %s", w.Body.String())
	}
}

// TestTemplatesList_OmitsProvidersWhenAbsent pins the omitempty
// behavior — older templates that haven't migrated to
// runtime_config.providers yet must NOT emit `providers: null` (which
// would break canvas's array-typed parser). A template that simply
// omits the field stays absent in the response and canvas falls back
// to deriving suggestions from model-slug prefixes.
func TestTemplatesList_OmitsProvidersWhenAbsent(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "no-prov")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Legacy
runtime: langgraph
runtime_config:
  model: anthropic:claude-opus-4-7
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `"providers":`) {
		t.Errorf("response should omit providers when template has none, got: %s", w.Body.String())
	}
}

func TestTemplatesList_LegacyTopLevelModel(t *testing.T) {
	// Older templates (pre-runtime_config) declared `model:` at the top level.
	// The /templates endpoint should keep surfacing those for backward compat.
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "legacy")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYaml := `name: Legacy Agent
tier: 1
model: anthropic:claude-sonnet-4-6
skills: []
`
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	handler := NewTemplatesHandler(tmpDir, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)
	handler.List(c)

	var resp []templateSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp) != 1 || resp[0].Model != "anthropic:claude-sonnet-4-6" {
		t.Errorf("legacy top-level model not surfaced: %+v", resp)
	}
	if resp[0].Runtime != "" {
		t.Errorf("Runtime should be empty for legacy template, got %q", resp[0].Runtime)
	}
	if len(resp[0].Models) != 0 {
		t.Errorf("Models should be empty for legacy template, got %+v", resp[0].Models)
	}
}

func TestTemplatesList_NonexistentDir(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler("/nonexistent/path/to/templates", nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/templates", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp []templateSummary
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty list, got %d items", len(resp))
	}
}

// ==================== GET /workspaces/:id/files ====================

func TestListFiles_InvalidRoot(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/files?root=/etc", nil)
	// Need to set query params
	c.Request.URL.RawQuery = "root=/etc"

	handler.ListFiles(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	// Verify no DB call was made (early return before DB query)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListFiles_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-nonexist").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-nonexist"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-nonexist/files", nil)

	handler.ListFiles(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListFiles_FallbackToHost_NoTemplate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil) // nil docker = no container

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-fallback").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Unknown Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-fallback"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-fallback/files", nil)

	handler.ListFiles(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Should return empty list
	var resp []interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty file list, got %d items", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestListFiles_FallbackToHost_WithTemplate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	// Create a template matching the workspace name
	tmplDir := filepath.Join(tmpDir, "test-agent")
	os.MkdirAll(tmplDir, 0755)
	os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte("name: Test Agent\n"), 0644)
	os.WriteFile(filepath.Join(tmplDir, "system-prompt.md"), []byte("# prompt"), 0644)

	handler := NewTemplatesHandler(tmpDir, nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-tmpl").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Test Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-tmpl"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-tmpl/files", nil)

	handler.ListFiles(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) < 2 {
		t.Errorf("expected at least 2 files, got %d", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== GET /workspaces/:id/files/*path ====================

func TestReadFile_PathTraversal(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "path", Value: "/../../../etc/passwd"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/files/../../../etc/passwd", nil)

	handler.ReadFile(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReadFile_InvalidRoot(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "path", Value: "/config.yaml"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/files/config.yaml?root=/tmp", nil)
	c.Request.URL.RawQuery = "root=/tmp"

	handler.ReadFile(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReadFile_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-nf").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-nf"},
		{Key: "path", Value: "/config.yaml"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-nf/files/config.yaml", nil)

	handler.ReadFile(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestReadFile_FallbackToHost_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "reader-agent")
	os.MkdirAll(tmplDir, 0755)
	os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte("name: Reader Agent\ntier: 1\n"), 0644)

	handler := NewTemplatesHandler(tmpDir, nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-read").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Reader Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-read"},
		{Key: "path", Value: "/config.yaml"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-read/files/config.yaml", nil)

	handler.ReadFile(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["path"] != "config.yaml" {
		t.Errorf("expected path 'config.yaml', got %v", resp["path"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestReadFile_FallbackToHost_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-nofile").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("No File Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-nofile"},
		{Key: "path", Value: "/nonexistent.txt"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-nofile/files/nonexistent.txt", nil)

	handler.ReadFile(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== PUT /workspaces/:id/files/*path ====================

func TestWriteFile_PathTraversal(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "path", Value: "/../../../etc/shadow"},
	}
	body := `{"content": "malicious"}`
	c.Request = httptest.NewRequest("PUT", "/workspaces/ws-1/files/../../../etc/shadow",
		strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.WriteFile(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWriteFile_InvalidBody(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "path", Value: "/config.yaml"},
	}
	c.Request = httptest.NewRequest("PUT", "/workspaces/ws-1/files/config.yaml",
		strings.NewReader("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.WriteFile(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWriteFile_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-wf-nf").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-wf-nf"},
		{Key: "path", Value: "/config.yaml"},
	}
	body := `{"content": "name: test"}`
	c.Request = httptest.NewRequest("PUT", "/workspaces/ws-wf-nf/files/config.yaml",
		strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.WriteFile(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== DELETE /workspaces/:id/files/*path ====================

func TestDeleteFile_PathTraversal(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "path", Value: "/../../../etc/passwd"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-1/files/../../../etc/passwd", nil)

	handler.DeleteFile(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteFile_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-del-nf").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-del-nf"},
		{Key: "path", Value: "old-file.txt"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-del-nf/files/old-file.txt", nil)

	handler.DeleteFile(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== GET /workspaces/:id/shared-context ====================

func TestSharedContext_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-sc-nf").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-sc-nf"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-sc-nf/shared-context", nil)

	handler.SharedContext(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSharedContext_NoTemplate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil) // no docker

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-sc-nt").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Unknown Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-sc-nt"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-sc-nt/shared-context", nil)

	handler.SharedContext(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Should return empty array
	var resp []interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 0 {
		t.Errorf("expected empty list, got %d items", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSharedContext_WithFiles(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "ctx-agent")
	os.MkdirAll(tmplDir, 0755)
	os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte("name: Ctx Agent\nshared_context:\n  - rules.md\n  - style.md\n"), 0644)
	os.WriteFile(filepath.Join(tmplDir, "rules.md"), []byte("# Rules\nBe nice"), 0644)
	os.WriteFile(filepath.Join(tmplDir, "style.md"), []byte("# Style\nBe clear"), 0644)

	handler := NewTemplatesHandler(tmpDir, nil)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-sc-ok").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Ctx Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-sc-ok"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-sc-ok/shared-context", nil)

	handler.SharedContext(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 2 {
		t.Fatalf("expected 2 context files, got %d", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== resolveTemplateDir ====================

func TestResolveTemplateDir_ByNormalizedName(t *testing.T) {
	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "my-agent")
	os.MkdirAll(tmplDir, 0755)

	handler := NewTemplatesHandler(tmpDir, nil)
	result := handler.resolveTemplateDir("My Agent")

	if result != tmplDir {
		t.Errorf("expected %q, got %q", tmplDir, result)
	}
}

func TestResolveTemplateDir_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil)
	result := handler.resolveTemplateDir("Nonexistent Agent")

	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

// ==================== CWE-78 hardening regression (issue #2011) ====================
// These tests lock in the defence-in-depth guards for DeleteFile and SharedContext.
// The primary guard is validateRelPath (fires before any exec/file-read path);
// the exec-form path construction (filepath.Join / separate args) is defence-in-depth.

// TestCWE78_DeleteFile_TraversalVariants asserts that a range of traversal patterns
// are all rejected with 400 before any Docker exec or ephemeral container operation.
// This covers the validateRelPath guard that sits at the entry of DeleteFile.
func TestCWE78_DeleteFile_TraversalVariants(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"double dotdot", "/../../../etc/passwd"},
		{"leading dotdot", "/../secret"},
		{"mid-path traversal", "/valid/../../../etc/shadow"},
		{"absolute path", "/etc/passwd"},
		{"encoded dotdot raw", "..%2F..%2Fetc%2Fpasswd"},
		{"triple dotdot", "/../../.."},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			setupTestRedis(t)

			handler := NewTemplatesHandler(t.TempDir(), nil)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{
				{Key: "id", Value: "ws-cwe78"},
				{Key: "path", Value: tc.path},
			}
			c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-cwe78/files"+tc.path, nil)

			handler.DeleteFile(c)

			if w.Code != http.StatusBadRequest {
				t.Errorf("path %q: expected 400 (traversal blocked), got %d: %s",
					tc.path, w.Code, w.Body.String())
			}
		})
	}
}

// TestCWE78_SharedContext_SkipsTraversalPaths asserts that when a workspace's
// config.yaml lists traversal paths in shared_context, SharedContext skips them
// via validateRelPath rather than passing them to exec or os.ReadFile.
// Uses the filesystem fallback path (no docker client) so no container mock needed.
func TestCWE78_SharedContext_SkipsTraversalPaths(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	// Create a template directory that SharedContext will resolve for "Cwe Agent".
	tmplDir := filepath.Join(tmpDir, "cwe-agent")
	os.MkdirAll(tmplDir, 0755)
	// config.yaml with a mix of safe and traversal-attack paths.
	configYAML := "name: Cwe Agent\nshared_context:\n  - safe-file.md\n  - ../../etc/passwd\n  - ../shadow\n  - another-safe.md\n"
	os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte(configYAML), 0644)
	// Only write the safe files — traversal paths must not be reachable.
	os.WriteFile(filepath.Join(tmplDir, "safe-file.md"), []byte("# safe"), 0644)
	os.WriteFile(filepath.Join(tmplDir, "another-safe.md"), []byte("# also safe"), 0644)

	mock.ExpectQuery("SELECT name FROM workspaces WHERE id =").
		WithArgs("ws-cwe78-sc").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Cwe Agent"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cwe78-sc"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-cwe78-sc/shared-context", nil)

	handler := NewTemplatesHandler(tmpDir, nil) // nil docker → filesystem fallback
	handler.SharedContext(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var files []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &files); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Only the two safe files must appear; traversal paths must be absent.
	if len(files) != 2 {
		t.Errorf("expected 2 safe files, got %d: %v", len(files), files)
	}
	for _, f := range files {
		if strings.Contains(f.Path, "..") || strings.Contains(f.Path, "etc") || strings.Contains(f.Path, "shadow") {
			t.Errorf("traversal path %q must not appear in shared-context response", f.Path)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
