package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ---------- UserTasksHandler: Create ----------

func TestUserTasks_Create_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewUserTasksHandler(broadcaster)

	// Insert user_task → returns id
	mock.ExpectQuery("INSERT INTO user_tasks").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ut-1"))
	// RecordAndBroadcast for USER_TASK_REQUESTED
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	body := `{"title":"Review the launch draft","detail":"posts/launch.md"}`
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["user_task_id"] != "ut-1" {
		t.Errorf("expected user_task_id ut-1, got %v", resp["user_task_id"])
	}
	if resp["status"] != "pending" {
		t.Errorf("expected status 'pending', got %v", resp["status"])
	}
}

func TestUserTasks_Create_MissingTitle(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewUserTasksHandler(newTestBroadcaster())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"detail":"no title"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing title, got %d", w.Code)
	}
}

// ---------- UserTasksHandler: Resolve ----------

func TestUserTasks_Resolve_Done(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewUserTasksHandler(broadcaster)

	// Update user_task → 1 row affected
	mock.ExpectExec("UPDATE user_tasks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// RecordAndBroadcast for USER_TASK_RESOLVED
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "taskId", Value: "ut-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"status":"done"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Resolve(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "done" {
		t.Errorf("expected status 'done', got %v", resp["status"])
	}
}

func TestUserTasks_Resolve_InvalidStatus(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewUserTasksHandler(newTestBroadcaster())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "taskId", Value: "ut-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"status":"maybe"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Resolve(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid status, got %d", w.Code)
	}
}

// ---------- UserTasksHandler: List / Update / Delete (workspace-owned) ----------

func TestUserTasks_List_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewUserTasksHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT id, title, detail, status, created_at, resolved_at, resolved_by FROM user_tasks WHERE workspace_id").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "detail", "status", "created_at", "resolved_at", "resolved_by"}).
			AddRow("ut-1", "Review draft", nil, "pending", "2026-06-07T00:00:00Z", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 || resp[0]["id"] != "ut-1" {
		t.Errorf("expected one task ut-1, got %v", resp)
	}
}

func TestUserTasks_Update_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewUserTasksHandler(newTestBroadcaster())

	mock.ExpectExec("UPDATE user_tasks SET").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "taskId", Value: "ut-1"}}
	c.Request = httptest.NewRequest("PATCH", "/", bytes.NewBufferString(`{"title":"Updated"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUserTasks_Update_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewUserTasksHandler(newTestBroadcaster())

	mock.ExpectExec("UPDATE user_tasks SET").
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "taskId", Value: "nope"}}
	c.Request = httptest.NewRequest("PATCH", "/", bytes.NewBufferString(`{"title":"x"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestUserTasks_Delete_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewUserTasksHandler(newTestBroadcaster())

	mock.ExpectExec("DELETE FROM user_tasks").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "taskId", Value: "ut-1"}}
	c.Request = httptest.NewRequest("DELETE", "/", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
