package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gopkg.in/yaml.v3"
	"github.com/gin-gonic/gin"
)

// ==================== normalizeName ====================

func TestNormalizeName(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"My Agent", "my-agent"},
		{"SEO Agent", "seo-agent"},
		{"hello_world", "hello_world"},
		{"Agent v2.0", "agent-v20"},
		{"UPPER CASE", "upper-case"},
		{"a-b-c", "a-b-c"},
		{"../hack", "hack"},
		{"", "unnamed"},
		{"$$$", "unnamed"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			result := normalizeName(tc.input)
			if result != tc.expected {
				t.Errorf("normalizeName(%q) = %q, want %q", tc.input, result, tc.expected)
			}
		})
	}
}

// ==================== generateDefaultConfig ====================

func TestGenerateDefaultConfig_WithFiles(t *testing.T) {
	files := map[string]string{
		"system-prompt.md":          "# System prompt",
		"rules.md":                  "# Rules",
		"skills/search/SKILL.md":    "Search skill",
		"skills/review/SKILL.md":    "Review skill",
		"skills/review/templates.md": "Templates",
	}

	cfg := generateDefaultConfig("Test Agent", files, 3)

	// Name is emitted as a double-quoted scalar (#221 sanitizer).
	if !strings.Contains(cfg, `name: "Test Agent"`) {
		t.Errorf("config should contain quoted agent name, got:\n%s", cfg)
	}
	if !strings.Contains(cfg, "tier: 3") {
		t.Error("config should default to tier 3 (Privileged) — matches workspace.go create handler default")
	}
	// Should detect prompt files
	if !strings.Contains(cfg, "system-prompt.md") {
		t.Error("config should include system-prompt.md as prompt file")
	}
	if !strings.Contains(cfg, "rules.md") {
		t.Error("config should include rules.md as prompt file")
	}
	// Should detect skills
	if !strings.Contains(cfg, "search") {
		t.Error("config should include 'search' skill")
	}
	if !strings.Contains(cfg, "review") {
		t.Error("config should include 'review' skill")
	}
}

func TestGenerateDefaultConfig_Empty(t *testing.T) {
	files := map[string]string{
		"data/something.json": `{"key": "value"}`,
	}

	cfg := generateDefaultConfig("Empty Agent", files, 3)

	if !strings.Contains(cfg, `name: "Empty Agent"`) {
		t.Errorf("config should contain quoted agent name, got:\n%s", cfg)
	}
	// Should fallback to default prompt file
	if !strings.Contains(cfg, "system-prompt.md") {
		t.Error("config should include default system-prompt.md")
	}
}

// TestGenerateDefaultConfig_YAMLInjection verifies that a crafted workspace
// name cannot inject arbitrary YAML keys into the generated config. Regression
// test for issue #221.
//
// Structural assertion: parse the output as YAML and verify the `name` scalar
// contains the full literal attacker input AND no banned top-level keys slipped
// in. Substring-based checks fail because escaped \n still contains the
// attacker's key text as a byte fragment.
func TestGenerateDefaultConfig_YAMLInjection(t *testing.T) {
	adversarialCases := []struct {
		desc       string
		name       string
		bannedKeys []string // top-level YAML keys that must NOT appear
	}{
		{
			desc:       "newline followed by new key",
			name:       "legit-agent\nmodel: malicious:model",
			bannedKeys: []string{}, // `model` is a legitimate default key too, test via name-scalar integrity
		},
		{
			desc:       "CRLF injection",
			name:       "legit-agent\r\nmalicious_key: value",
			bannedKeys: []string{"malicious_key"},
		},
		{
			desc:       "multiple newlines with multiple keys",
			name:       "x\nfoo: bar\nbaz: qux",
			bannedKeys: []string{"foo", "baz"},
		},
		{
			desc:       "double-quote escape attempt",
			name:       `"; evil_key: pwned; #`,
			bannedKeys: []string{"evil_key"},
		},
	}

	for _, tc := range adversarialCases {
		t.Run(tc.desc, func(t *testing.T) {
			cfg := generateDefaultConfig(tc.name, map[string]string{}, 3)
			var parsed map[string]interface{}
			if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
				t.Fatalf("sanitized config does not parse as YAML: %v\n--- config ---\n%s", err, cfg)
			}
			// 1. name key must equal the original attacker input (escaping preserved payload)
			if got, _ := parsed["name"].(string); got != tc.name {
				t.Errorf("name scalar mismatch:\n  got:  %q\n  want: %q", got, tc.name)
			}
			// 2. No banned top-level keys injected
			for _, k := range tc.bannedKeys {
				if _, exists := parsed[k]; exists {
					t.Errorf("YAML injection: banned key %q leaked to parsed config", k)
				}
			}
			// 3. Legitimate default keys still present — escaping didn't corrupt the rest
			for _, expected := range []string{"name", "description", "version", "tier", "model"} {
				if _, exists := parsed[expected]; !exists {
					t.Errorf("missing legitimate key %q in parsed config", expected)
				}
			}
		})
	}
}

// ==================== writeFiles ====================

func TestWriteFiles_Success(t *testing.T) {
	tmpDir := t.TempDir()
	files := map[string]string{
		"config.yaml":            "name: Test\n",
		"skills/test/SKILL.md":   "# Test Skill",
		"system-prompt.md":       "# System Prompt",
	}

	if err := writeFiles(tmpDir, files); err != nil {
		t.Fatalf("writeFiles failed: %v", err)
	}

	// Verify files exist
	for path, expectedContent := range files {
		data, err := os.ReadFile(filepath.Join(tmpDir, path))
		if err != nil {
			t.Errorf("expected file %s to exist: %v", path, err)
			continue
		}
		if string(data) != expectedContent {
			t.Errorf("file %s content mismatch: got %q, want %q", path, string(data), expectedContent)
		}
	}
}

func TestWriteFiles_PathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	files := map[string]string{
		"../escape.txt": "malicious",
	}

	err := writeFiles(tmpDir, files)
	if err == nil {
		t.Error("expected error for path traversal")
	}
}

// ==================== POST /templates/import ====================

func TestImport_Success(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil, nil)

	body := `{
		"name": "New Agent",
		"files": {
			"system-prompt.md": "# System Prompt\nYou are a helpful assistant",
			"skills/test/SKILL.md": "# Test Skill"
		}
	}`

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/templates/import", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Import(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "imported" {
		t.Errorf("expected status 'imported', got %v", resp["status"])
	}
	if resp["id"] != "new-agent" {
		t.Errorf("expected id 'new-agent', got %v", resp["id"])
	}

	// Verify config.yaml was auto-generated
	if _, err := os.Stat(filepath.Join(tmpDir, "new-agent", "config.yaml")); os.IsNotExist(err) {
		t.Error("config.yaml should have been auto-generated")
	}
}

func TestImport_MissingName(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

	body := `{"files": {"test.md": "content"}}`

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/templates/import", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Import(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestImport_TooManyFiles(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

	files := make(map[string]string)
	for i := 0; i <= maxUploadFiles; i++ {
		files[fmt.Sprintf("file%d.txt", i)] = "content"
	}

	bodyBytes, _ := json.Marshal(map[string]interface{}{
		"name":  "Big Agent",
		"files": files,
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/templates/import", bytes.NewBuffer(bodyBytes))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Import(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestImport_AlreadyExists(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	os.MkdirAll(filepath.Join(tmpDir, "existing-agent"), 0755)

	handler := NewTemplatesHandler(tmpDir, nil, nil)

	body := `{"name": "Existing Agent", "files": {"test.md": "content"}}`

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/templates/import", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Import(c)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestImport_WithConfigYaml(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	handler := NewTemplatesHandler(tmpDir, nil, nil)

	body := `{
		"name": "Custom Agent",
		"files": {
			"config.yaml": "name: Custom Agent\ntier: 3\nmodel: gpt-4",
			"system-prompt.md": "# You are custom"
		}
	}`

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/templates/import", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Import(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the provided config.yaml was used (not auto-generated)
	data, err := os.ReadFile(filepath.Join(tmpDir, "custom-agent", "config.yaml"))
	if err != nil {
		t.Fatalf("failed to read config.yaml: %v", err)
	}
	if !strings.Contains(string(data), "tier: 3") {
		t.Error("config.yaml should contain provided tier: 3, not auto-generated content")
	}
}

// ==================== PUT /workspaces/:id/files (ReplaceFiles) ====================

func TestReplaceFiles_MissingBody(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/ws-1/files", bytes.NewBufferString("{}"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ReplaceFiles(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReplaceFiles_TooManyFiles(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

	files := make(map[string]string)
	for i := 0; i <= maxUploadFiles; i++ {
		files[fmt.Sprintf("file%d.txt", i)] = "content"
	}
	bodyBytes, _ := json.Marshal(map[string]interface{}{"files": files})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/ws-1/files", bytes.NewBuffer(bodyBytes))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ReplaceFiles(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReplaceFiles_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

	// ReplaceFiles now selects (name, instance_id, runtime) for the
	// restart-cascade. Match the full column list rather than just the
	// name so the sqlmock regex pins the whole statement.
	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-rf-nf").
		WillReturnError(sql.ErrNoRows)

	body := `{"files": {"test.md": "content"}}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-rf-nf"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/ws-rf-nf/files", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ReplaceFiles(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestReplaceFiles_PathTraversal(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-rf-pt").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).AddRow("Test Agent", "", ""))

	body := `{"files": {"../../../etc/passwd": "malicious"}}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-rf-pt"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/ws-rf-pt/files", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ReplaceFiles(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestReplaceFiles_OfflineDocker_Returns503 ensures that when the workspace
// container is offline AND the ephemeral Docker write fails (docker unavailable),
// ReplaceFiles returns 503 instead of silently falling back to the template
// directory. A template-dir write would be invisible after restart because
// the restart handler reads from the Docker volume, not the template dir (#151).
func TestReplaceFiles_OfflineDocker_Returns503(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// nil docker → TemplatesHandler.docker == nil → findContainer returns "" (offline)
	// → writeViaEphemeral returns error (docker not available) → handler should
	// return 503, NOT fall back to the host-side template dir.
	handler := NewTemplatesHandler(t.TempDir(), nil, nil)

	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-offline").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).AddRow("Offline Agent", "", ""))

	body := `{"files": {"initial-prompt.md": "updated prompt"}}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-offline"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/ws-offline/files", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ReplaceFiles(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}

	if !strings.Contains(w.Body.String(), "offline") {
		t.Errorf("response should mention 'offline': %s", w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
