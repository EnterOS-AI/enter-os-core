package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// makeTeamConfigDir creates a temporary configs directory with a named
// subdirectory containing a config.yaml file.
func makeTeamConfigDir(t *testing.T, workspaceName string, yamlContent string) string {
	t.Helper()
	dir := t.TempDir()
	subDir := filepath.Join(dir, workspaceName)
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "config.yaml"), []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write config.yaml: %v", err)
	}
	return dir
}

// ---------- TeamHandler: Collapse ----------

func TestTeamCollapse_NoChildren(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewTeamHandler(broadcaster, NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir()), "http://localhost:8080", "/tmp/configs")

	// No children
	mock.ExpectQuery("SELECT id, name FROM workspaces WHERE parent_id").
		WithArgs("ws-parent").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	// WORKSPACE_COLLAPSED broadcast
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-parent"}}
	c.Request = httptest.NewRequest("POST", "/", nil)

	handler.Collapse(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "collapsed" {
		t.Errorf("expected status 'collapsed', got %v", resp["status"])
	}
}

func TestTeamCollapse_WithChildren(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewTeamHandler(broadcaster, NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir()), "http://localhost:8080", "/tmp/configs")

	// Two children
	mock.ExpectQuery("SELECT id, name FROM workspaces WHERE parent_id").
		WithArgs("ws-parent").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).
			AddRow("child-1", "Worker A").
			AddRow("child-2", "Worker B"))

	// UPDATE + DELETE + broadcast for child-1
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs("child-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM canvas_layouts").
		WithArgs("child-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// UPDATE + DELETE + broadcast for child-2
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs("child-2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM canvas_layouts").
		WithArgs("child-2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// WORKSPACE_COLLAPSED broadcast for parent
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-parent"}}
	c.Request = httptest.NewRequest("POST", "/", nil)

	handler.Collapse(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	removed, ok := resp["removed"].([]interface{})
	if !ok || len(removed) != 2 {
		t.Errorf("expected 2 removed children, got %v", resp["removed"])
	}
}

// ---------- TeamHandler: Expand ----------

func TestTeamExpand_WorkspaceNotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewTeamHandler(newTestBroadcaster(), NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir()), "http://localhost:8080", "/tmp/configs")

	mock.ExpectQuery("SELECT name, tier, status FROM workspaces WHERE id").
		WithArgs("ws-missing").
		WillReturnError(sqlmock.ErrCancelled)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-missing"}}
	c.Request = httptest.NewRequest("POST", "/", nil)

	handler.Expand(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestTeamExpand_NoConfigFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewTeamHandler(newTestBroadcaster(), NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir()), "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT name, tier, status FROM workspaces WHERE id").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "status"}).
			AddRow("UnknownAgent", 1, "online"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/", nil)

	handler.Expand(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTeamExpand_EmptySubWorkspaces(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	configDir := makeTeamConfigDir(t, "myagent", "name: MyAgent\nsub_workspaces: []\n")
	handler := NewTeamHandler(newTestBroadcaster(), NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir()), "http://localhost:8080", configDir)

	mock.ExpectQuery("SELECT name, tier, status FROM workspaces WHERE id").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "status"}).
			AddRow("myagent", 1, "online"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/", nil)

	handler.Expand(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 (no sub_workspaces), got %d: %s", w.Code, w.Body.String())
	}
}

func TestTeamExpand_WithSubWorkspaces(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()

	yaml := `name: TeamLead
sub_workspaces:
  - name: Worker-A
    role: data-analyst
  - name: Worker-B
    role: code-reviewer
`
	configDir := makeTeamConfigDir(t, "teamlead", yaml)
	handler := NewTeamHandler(broadcaster, NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir()), "http://localhost:8080", configDir)

	mock.ExpectQuery("SELECT name, tier, status FROM workspaces WHERE id").
		WithArgs("ws-lead").
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "status"}).
			AddRow("teamlead", 2, "online"))

	// INSERT for Worker-A
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// INSERT for Worker-B
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// WORKSPACE_EXPANDED broadcast
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-lead"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(""))

	handler.Expand(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	children, ok := resp["children"].([]interface{})
	if !ok || len(children) != 2 {
		t.Errorf("expected 2 children, got %v", resp["children"])
	}
}

// ---------- findTemplateDirByName helper ----------

func TestFindTemplateDirByName_DirectMatch(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "mybot")
	os.MkdirAll(subDir, 0755)
	os.WriteFile(filepath.Join(subDir, "config.yaml"), []byte("name: MyBot"), 0644)

	result := findTemplateDirByName(dir, "mybot")
	if result != subDir {
		t.Errorf("expected %s, got %s", subDir, result)
	}
}

func TestFindTemplateDirByName_NotFound(t *testing.T) {
	dir := t.TempDir()
	result := findTemplateDirByName(dir, "nonexistent")
	if result != "" {
		t.Errorf("expected empty string, got %s", result)
	}
}

func TestFindTemplateDirByName_InvalidConfigsDir(t *testing.T) {
	result := findTemplateDirByName("/nonexistent/path", "anything")
	if result != "" {
		t.Errorf("expected empty string for invalid dir, got %s", result)
	}
}
