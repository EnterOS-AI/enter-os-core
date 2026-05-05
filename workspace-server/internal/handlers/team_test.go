package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

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
