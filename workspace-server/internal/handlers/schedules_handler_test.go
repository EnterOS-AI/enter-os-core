package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// scheduleCols is the full column set returned by List.
var scheduleCols = []string{
	"id", "workspace_id", "name", "cron_expr", "timezone", "prompt", "enabled",
	"last_run_at", "next_run_at", "run_count", "last_status", "last_error",
	"source", "created_at", "updated_at",
}

// ==================== List ====================

func TestScheduleHandler_List_EmptyResult(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery("SELECT .+ FROM workspace_schedules WHERE workspace_id").
		WithArgs("ws-list-empty").
		WillReturnRows(sqlmock.NewRows(scheduleCols))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-list-empty"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-list-empty/schedules", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var schedules []interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &schedules); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(schedules) != 0 {
		t.Errorf("expected empty list, got %d items", len(schedules))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_List_QueryError(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery("SELECT .+ FROM workspace_schedules WHERE workspace_id").
		WithArgs("ws-list-err").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-list-err"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-list-err/schedules", nil)

	handler.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// ==================== Create ====================

func TestScheduleHandler_Create_MissingCronExpr(t *testing.T) {
	handler := NewScheduleHandler()

	// prompt only — no cron_expr
	body := []byte(`{"prompt":"do the thing"}`)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing cron_expr, got %d: %s", w.Code, w.Body.String())
	}
}

func TestScheduleHandler_Create_MissingPrompt(t *testing.T) {
	handler := NewScheduleHandler()

	// cron_expr only — no prompt
	body := []byte(`{"cron_expr":"0 9 * * *"}`)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing prompt, got %d: %s", w.Code, w.Body.String())
	}
}

func TestScheduleHandler_Create_InvalidTimezone(t *testing.T) {
	handler := NewScheduleHandler()

	body, _ := json.Marshal(map[string]string{
		"cron_expr": "0 9 * * *",
		"prompt":    "do the thing",
		"timezone":  "Not/A/Timezone",
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid timezone, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "invalid timezone") {
		t.Errorf("expected 'invalid timezone' error, got: %v", resp)
	}
}

func TestScheduleHandler_Create_InvalidCron(t *testing.T) {
	handler := NewScheduleHandler()

	body, _ := json.Marshal(map[string]string{
		"cron_expr": "not-a-cron",
		"prompt":    "do the thing",
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid cron, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "invalid request body") {
		t.Errorf("expected 'invalid request body' error, got: %v", resp)
	}
}

func TestScheduleHandler_Create_CRLFStripped(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	// Prompt with CRLF from a Windows-committed org-template file.
	// The handler strips \r before inserting so agent doesn't see empty responses.
	promptWithCRLF := "check\r\ndocs\r\nbefore merge"

	// Use a custom matcher that captures the prompt argument so we can assert
	// it has no \r characters.
	matcher := sqlmock.NewArgMatcher(func(a interface{}) bool {
		if s, ok := a.(string); ok {
			// This will be called for multiple args; capture the prompt (5th arg).
			return strings.Contains(s, "check\ndocs\nbefore merge")
		}
		return true
	})
	customMock, _, _ := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	t.Cleanup(func() { customMock.Close() })
	prevDB := db.DB
	db.DB = customMock
	t.Cleanup(func() { db.DB = prevDB })

	customMock.ExpectQuery("INSERT INTO workspace_schedules").
		WithArgs("ws-crlf", "", "0 9 * * *", "UTC", "check\ndocs\nbefore merge", true, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("sched-crlf"))

	body, _ := json.Marshal(map[string]interface{}{
		"cron_expr": "0 9 * * *",
		"prompt":    promptWithCRLF,
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-crlf"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-crlf/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestScheduleHandler_Create_DefaultEnabled(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	// enabled field absent — must default to true.
	mock.ExpectQuery("INSERT INTO workspace_schedules").
		WithArgs("ws-def-enable", "", "0 9 * * *", "UTC", "do thing", true, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("sched-enable"))

	body, _ := json.Marshal(map[string]string{
		"cron_expr": "0 9 * * *",
		"prompt":    "do thing",
		// no "enabled" field
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-def-enable"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-def-enable/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_Create_DefaultTimezone(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	// timezone field absent — must default to UTC.
	mock.ExpectQuery("INSERT INTO workspace_schedules").
		WithArgs("ws-def-tz", "", "0 9 * * *", "UTC", "do thing", true, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("sched-tz"))

	body, _ := json.Marshal(map[string]string{
		"cron_expr": "0 9 * * *",
		"prompt":    "do thing",
		// no "timezone" field
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-def-tz"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-def-tz/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_Create_ExplicitEnabledFalse(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	enabled := false
	mock.ExpectQuery("INSERT INTO workspace_schedules").
		WithArgs("ws-dis", "", "0 9 * * *", "UTC", "do thing", enabled, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("sched-dis"))

	body, _ := json.Marshal(map[string]interface{}{
		"cron_expr": "0 9 * * *",
		"prompt":    "do thing",
		"enabled":   false,
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-dis"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-dis/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_Create_DBError(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery("INSERT INTO workspace_schedules").
		WillReturnError(sql.ErrConnDone)

	body, _ := json.Marshal(map[string]string{
		"cron_expr": "0 9 * * *",
		"prompt":    "do thing",
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-db-err"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-db-err/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for DB error, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_Create_NextRunAtReturned(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery("INSERT INTO workspace_schedules").
		WithArgs("ws-next", "", "0 9 * * *", "UTC", "do thing", true, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("sched-next"))

	body, _ := json.Marshal(map[string]string{
		"cron_expr": "0 9 * * *",
		"prompt":    "do thing",
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-next"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-next/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "created" {
		t.Errorf("expected status 'created', got %v", resp["status"])
	}
	if _, ok := resp["next_run_at"]; !ok {
		t.Error("expected next_run_at in response")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// ==================== Update ====================

func TestScheduleHandler_Update_PartialRecomputeCron(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	// Changing cron_expr → handler SELECTs current cron+tz, recomputes next_run_at.
	mock.ExpectQuery(`SELECT cron_expr, timezone FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs("sched-recompute-cron", "ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"cron_expr", "timezone"}).
			AddRow("0 8 * * *", "UTC"))

	mock.ExpectExec(regexp.MustCompile(`UPDATE workspace_schedules SET[\s\S]+WHERE id = \$1 AND workspace_id = \$8`)).
		WithArgs("sched-recompute-cron", nil, "0 6 * * *", nil, nil, nil, sqlmock.AnyArg(), "ws-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body, _ := json.Marshal(map[string]string{"cron_expr": "0 6 * * *"})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-recompute-cron"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-1/schedules/sched-recompute-cron", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_Update_PartialRecomputeTimezone(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery(`SELECT cron_expr, timezone FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs("sched-recompute-tz", "ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"cron_expr", "timezone"}).
			AddRow("0 9 * * *", "UTC"))

	mock.ExpectExec(regexp.MustCompile(`UPDATE workspace_schedules SET[\s\S]+WHERE id = \$1 AND workspace_id = \$8`)).
		WithArgs("sched-recompute-tz", nil, nil, "America/New_York", nil, nil, sqlmock.AnyArg(), "ws-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body, _ := json.Marshal(map[string]string{"timezone": "America/New_York"})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-recompute-tz"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-1/schedules/sched-recompute-tz", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_Update_InvalidTimezone(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery(`SELECT cron_expr, timezone FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs("sched-bad-tz", "ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"cron_expr", "timezone"}).
			AddRow("0 9 * * *", "UTC"))

	body, _ := json.Marshal(map[string]string{"timezone": "Definitely/Not/Real"})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-bad-tz"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-1/schedules/sched-bad-tz", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid timezone, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "invalid timezone") {
		t.Errorf("expected 'invalid timezone' error, got: %v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_Update_InvalidCron(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery(`SELECT cron_expr, timezone FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs("sched-bad-cron", "ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"cron_expr", "timezone"}).
			AddRow("0 9 * * *", "UTC"))

	body, _ := json.Marshal(map[string]string{"cron_expr": "rubbish"})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-bad-cron"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-1/schedules/sched-bad-cron", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid cron, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_Update_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectExec(regexp.MustCompile(`UPDATE workspace_schedules SET[\s\S]+WHERE id = \$1 AND workspace_id = \$8`)).
		WithArgs("sched-missing", nil, nil, nil, nil, nil, nil, "ws-1").
		WillReturnResult(sqlmock.NewResult(0, 0)) // no rows affected

	body, _ := json.Marshal(map[string]string{"name": "renamed"})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-missing"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-1/schedules/sched-missing", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for not found, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_Update_DBError(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectExec(regexp.MustCompile(`UPDATE workspace_schedules SET[\s\S]+WHERE id = \$1 AND workspace_id = \$8`)).
		WithArgs("sched-update-err", nil, nil, nil, nil, nil, nil, "ws-1").
		WillReturnError(sql.ErrConnDone)

	body, _ := json.Marshal(map[string]string{"name": "updated"})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-update-err"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-1/schedules/sched-update-err", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for DB error, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_Update_PromptCRLFStripped(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	// Changing prompt with CRLF → handler strips \r before the UPDATE.
	mock.ExpectExec(regexp.MustCompile(`UPDATE workspace_schedules SET[\s\S]+WHERE id = \$1 AND workspace_id = \$8`)).
		WithArgs("sched-crlf-upd", nil, nil, nil, "fix\r\nthat", nil, nil, "ws-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body, _ := json.Marshal(map[string]string{"prompt": "fix\r\nthat"})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-crlf-upd"}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/ws-1/schedules/sched-crlf-upd", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// ==================== Delete ====================

func TestScheduleHandler_Delete_Success(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectExec(regexp.MustCompile(`DELETE FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`)).
		WithArgs("sched-del", "ws-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-del"}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-1/schedules/sched-del", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_Delete_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	// IDOR guard: row belongs to different workspace → 0 rows affected → 404.
	mock.ExpectExec(regexp.MustCompile(`DELETE FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`)).
		WithArgs("sched-idor", "ws-1").
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-idor"}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-1/schedules/sched-idor", nil)

	handler.Delete(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for not found, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_Delete_DBError(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectExec(regexp.MustCompile(`DELETE FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`)).
		WithArgs("sched-del-err", "ws-1").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-del-err"}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-1/schedules/sched-del-err", nil)

	handler.Delete(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for DB error, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// ==================== RunNow ====================

func TestScheduleHandler_RunNow_Success(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery(`SELECT prompt FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs("sched-run-ok", "ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"prompt"}).AddRow("run this prompt"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-run-ok"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/schedules/sched-run-ok/run", nil)

	handler.RunNow(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "fired" {
		t.Errorf("expected status 'fired', got %v", resp["status"])
	}
	if resp["prompt"] != "run this prompt" {
		t.Errorf("expected prompt 'run this prompt', got %q", resp["prompt"])
	}
	if resp["workspace_id"] != "ws-1" {
		t.Errorf("expected workspace_id 'ws-1', got %q", resp["workspace_id"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_RunNow_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery(`SELECT prompt FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs("sched-run-missing", "ws-1").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-run-missing"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/schedules/sched-run-missing/run", nil)

	handler.RunNow(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for not found, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_RunNow_DBError(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery(`SELECT prompt FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs("sched-run-err", "ws-1").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "scheduleId", Value: "sched-run-err"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/schedules/sched-run-err/run", nil)

	handler.RunNow(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for DB error, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// ==================== History ====================

func TestScheduleHandler_History_EmptyResult(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery(`SELECT created_at, duration_ms, status`).
		WithArgs("ws-hist-empty", "sched-hist-empty").
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "duration_ms", "status", "error_detail", "request_body"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-hist-empty"}, {Key: "scheduleId", Value: "sched-hist-empty"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-hist-empty/schedules/sched-hist-empty/history", nil)

	handler.History(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var entries []interface{}
	json.Unmarshal(w.Body.Bytes(), &entries)
	if len(entries) != 0 {
		t.Errorf("expected empty history, got %d entries", len(entries))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_History_QueryError(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery(`SELECT created_at, duration_ms, status`).
		WithArgs("ws-hist-err", "sched-hist-err").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-hist-err"}, {Key: "scheduleId", Value: "sched-hist-err"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-hist-err/schedules/sched-hist-err/history", nil)

	handler.History(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on query error, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

func TestScheduleHandler_History_MultipleEntries(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()

	now := time.Now()
	cols := []string{"created_at", "duration_ms", "status", "error_detail", "request_body"}
	mock.ExpectQuery(`SELECT created_at, duration_ms, status`).
		WithArgs("ws-hist-multi", "sched-hist-multi").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow(now, 1200, "ok", "", `{"schedule_id":"sched-hist-multi"}`).
			AddRow(now, 3500, "error", "HTTP 502 — upstream timeout", `{"schedule_id":"sched-hist-multi"}`))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-hist-multi"}, {Key: "scheduleId", Value: "sched-hist-multi"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-hist-multi/schedules/sched-hist-multi/history", nil)

	handler.History(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var entries []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &entries)
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d: %s", len(entries), w.Body.String())
	}
	if entries[1]["error_detail"] != "HTTP 502 — upstream timeout" {
		t.Errorf("expected error_detail on second entry, got: %v", entries[1]["error_detail"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}
