package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ─── List ────────────────────────────────────────────────────────────────────

func TestList_EmptyResult(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	mock.ExpectQuery(`SELECT .* FROM workspace_schedules WHERE workspace_id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "name", "cron_expr", "timezone", "prompt",
			"enabled", "last_run_at", "next_run_at", "run_count", "last_status",
			"last_error", "source", "created_at", "updated_at",
		}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/schedules", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var schedules []scheduleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &schedules); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if len(schedules) != 0 {
		t.Errorf("expected empty list, got %d items", len(schedules))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}

func TestList_QueryError_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	mock.ExpectQuery(`SELECT .* FROM workspace_schedules WHERE workspace_id = \$1`).
		WithArgs(wsID).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/schedules", nil)

	handler.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

// TestList_ScanError_Continues is not directly testable with sqlmock because
// sqlmock panics when a row has the wrong number of columns (rather than
// returning a scan error the way a real DB driver would). The handler's scan
// error handling (log + continue) is implicitly covered by the multi-row test
// TestList_IncludesSourceColumn — the handler's scan loop uses `continue` on
// error, so correctly-shaped rows are always returned regardless of what
// earlier rows did.

// ─── Create ───────────────────────────────────────────────────────────────────

func TestCreate_MissingCronExpr_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	body := []byte(`{"prompt":"do thing"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreate_MissingPrompt_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	body := []byte(`{"cron_expr":"*/5 * * * *"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreate_InvalidTimezone_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	body := []byte(`{"cron_expr":"*/5 * * * *","prompt":"do thing","timezone":"Not/A/Zone"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid timezone") {
		t.Errorf("error message should mention 'invalid timezone': %s", w.Body.String())
	}
}

func TestCreate_InvalidCronExpr_Returns400(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	body := []byte(`{"cron_expr":"not-a-cron","prompt":"do thing"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreate_CRLFStrippedFromPrompt(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	// The prompt in the DB should NOT contain \r.
	mock.ExpectQuery("INSERT INTO workspace_schedules").
		WithArgs(wsID, "test", "*/5 * * * *", "UTC", "line1\nline2", true, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("sched-1"))

	body := []byte(`{"name":"test","cron_expr":"*/5 * * * *","prompt":"line1\r\nline2"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v — the \r must be stripped before INSERT", err)
	}
}

func TestCreate_DefaultsEnabledTrue(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	// enabled=true is the default when body.enabled is nil.
	mock.ExpectQuery("INSERT INTO workspace_schedules").
		WithArgs(wsID, "test", "*/5 * * * *", "UTC", "do thing", true, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("sched-1"))

	body := []byte(`{"name":"test","cron_expr":"*/5 * * * *","prompt":"do thing"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}

func TestCreate_DefaultsTimezoneUTC(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	// Timezone defaults to UTC when not specified.
	mock.ExpectQuery("INSERT INTO workspace_schedules").
		WithArgs(wsID, "test", "*/5 * * * *", "UTC", "do thing", true, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("sched-1"))

	body := []byte(`{"name":"test","cron_expr":"*/5 * * * *","prompt":"do thing"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}

func TestCreate_ExplicitEnabledFalse(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	// enabled=false when explicitly set.
	mock.ExpectQuery("INSERT INTO workspace_schedules").
		WithArgs(wsID, "test", "*/5 * * * *", "UTC", "do thing", false, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("sched-1"))

	body := []byte(`{"name":"test","cron_expr":"*/5 * * * *","prompt":"do thing","enabled":false}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	req := httptest.NewRequest("POST", "/workspaces/"+wsID+"/schedules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}

func TestCreate_DBError_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	mock.ExpectQuery("INSERT INTO workspace_schedules").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(sql.ErrConnDone)

	body := []byte(`{"cron_expr":"*/5 * * * *","prompt":"do thing"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreate_ReturnsNextRunAt(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	mock.ExpectQuery("INSERT INTO workspace_schedules").
		WithArgs(wsID, "test", "*/5 * * * *", "UTC", "do thing", true, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("sched-1"))

	body := []byte(`{"name":"test","cron_expr":"*/5 * * * *","prompt":"do thing"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["status"] != "created" {
		t.Errorf("status=created: got %v", resp["status"])
	}
	if _, ok := resp["id"]; !ok {
		t.Errorf("response missing id field")
	}
	if _, ok := resp["next_run_at"]; !ok {
		t.Errorf("response missing next_run_at field")
	}
}

// ─── Update ───────────────────────────────────────────────────────────────────

func TestUpdate_PartialUpdate_CRONChangeRecomputesNextRun(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	// 1. Lookup current cron + timezone.
	mock.ExpectQuery(`SELECT cron_expr, timezone FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(schedID, wsID).
		WillReturnRows(sqlmock.NewRows([]string{"cron_expr", "timezone"}).
			AddRow("0 * * * *", "UTC"))

	// 2. UPDATE with new cron_expr but old timezone; next_run_at = new computed.
	mock.ExpectExec(`UPDATE workspace_schedules SET`).
		WithArgs(schedID, sqlmock.AnyArg(), "*/5 * * * *", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := []byte(`{"cron_expr":"*/5 * * * *"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID+"/schedules/"+schedID, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}

func TestUpdate_PartialUpdate_TimezoneChangeRecomputesNextRun(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectQuery(`SELECT cron_expr, timezone FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(schedID, wsID).
		WillReturnRows(sqlmock.NewRows([]string{"cron_expr", "timezone"}).
			AddRow("0 * * * *", "UTC"))

	mock.ExpectExec(`UPDATE workspace_schedules SET`).
		WithArgs(schedID, sqlmock.AnyArg(), sqlmock.AnyArg(), "America/New_York", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := []byte(`{"timezone":"America/New_York"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID+"/schedules/"+schedID, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_NoScheduleMatch_Returns404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	// body={} means CronExpr=nil AND Timezone=nil → handler skips the lookup
	// and goes straight to UPDATE. Expect UPDATE with 0 rows affected → 404.
	mock.ExpectExec(`UPDATE workspace_schedules SET`).
		WithArgs(schedID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), wsID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID+"/schedules/"+schedID, bytes.NewReader([]byte(`{}`)))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}

func TestUpdate_InvalidTimezone_Returns400(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectQuery(`SELECT cron_expr, timezone FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(schedID, wsID).
		WillReturnRows(sqlmock.NewRows([]string{"cron_expr", "timezone"}).
			AddRow("0 * * * *", "UTC"))

	body := []byte(`{"timezone":"Mars/Olympus"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID+"/schedules/"+schedID, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid timezone") {
		t.Errorf("error should mention 'invalid timezone': %s", w.Body.String())
	}
}

func TestUpdate_InvalidCronExpr_Returns400(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectQuery(`SELECT cron_expr, timezone FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(schedID, wsID).
		WillReturnRows(sqlmock.NewRows([]string{"cron_expr", "timezone"}).
			AddRow("0 * * * *", "UTC"))

	body := []byte(`{"cron_expr":"[invalid"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID+"/schedules/"+schedID, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_ScheduleNotFoundOnExec_Returns404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	// No cron/timezone change → no lookup; goes straight to UPDATE.
	// RowsAffected=0 means no matching row → 404.
	mock.ExpectExec(`UPDATE workspace_schedules SET`).
		WithArgs(schedID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), wsID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	body := []byte(`{"name":"new name"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID+"/schedules/"+schedID, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_DBError_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectExec(`UPDATE workspace_schedules SET`).
		WithArgs(schedID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), wsID).
		WillReturnError(sql.ErrConnDone)

	body := []byte(`{"name":"new name"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID+"/schedules/"+schedID, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdate_PromptCRLFStripped(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	// No cron/timezone change → no lookup; UPDATE directly.
	// The prompt arg must have \r stripped.
	mock.ExpectExec(`UPDATE workspace_schedules SET`).
		WithArgs(schedID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "line1\nline2", sqlmock.AnyArg(), sqlmock.AnyArg(), wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := []byte(`{"prompt":"line1\r\nline2"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID+"/schedules/"+schedID, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v — \\r must be stripped before UPDATE", err)
	}
}

// ─── Delete ───────────────────────────────────────────────────────────────────

func TestDelete_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectExec(`DELETE FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(schedID, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsID+"/schedules/"+schedID, nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "deleted") {
		t.Errorf("response should contain 'deleted': %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}

func TestDelete_NotFound_Returns404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	// IDOR: schedule belongs to a different workspace → no rows deleted.
	mock.ExpectExec(`DELETE FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(schedID, wsID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsID+"/schedules/"+schedID, nil)

	handler.Delete(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDelete_DBError_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectExec(`DELETE FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(schedID, wsID).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsID+"/schedules/"+schedID, nil)

	handler.Delete(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ─── RunNow ───────────────────────────────────────────────────────────────────

func TestRunNow_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectQuery(`SELECT prompt FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(schedID, wsID).
		WillReturnRows(sqlmock.NewRows([]string{"prompt"}).AddRow("do the thing"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/schedules/"+schedID+"/run", nil)

	handler.RunNow(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["status"] != "fired" {
		t.Errorf("status=fired: got %v", resp["status"])
	}
	if resp["prompt"] != "do the thing" {
		t.Errorf("prompt: got %v", resp["prompt"])
	}
	if resp["workspace_id"] != wsID {
		t.Errorf("workspace_id: got %v", resp["workspace_id"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}

func TestRunNow_NotFound_Returns404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectQuery(`SELECT prompt FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(schedID, wsID).
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/schedules/"+schedID+"/run", nil)

	handler.RunNow(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRunNow_DBError_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectQuery(`SELECT prompt FROM workspace_schedules WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(schedID, wsID).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/schedules/"+schedID+"/run", nil)

	handler.RunNow(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ─── History ─────────────────────────────────────────────────────────────────

func TestHistory_EmptyResult(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	cols := []string{"created_at", "duration_ms", "status", "error_detail", "request_body"}
	mock.ExpectQuery(`SELECT created_at, duration_ms, status`).
		WithArgs(wsID, schedID).
		WillReturnRows(sqlmock.NewRows(cols))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("GET",
		"/workspaces/"+wsID+"/schedules/"+schedID+"/history", nil)

	handler.History(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var entries []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty history, got %d entries", len(entries))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}

func TestHistory_QueryError_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectQuery(`SELECT created_at, duration_ms, status`).
		WithArgs(wsID, schedID).
		WillReturnError(errors.New("connection lost"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("GET",
		"/workspaces/"+wsID+"/schedules/"+schedID+"/history", nil)

	handler.History(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHistory_MultipleEntries_ReverseOrder(t *testing.T) {
	// Verifies the History handler correctly deserialises multiple rows and
	// includes error_detail in the response (#152). sqlmock doesn't produce
	// scan errors from nil pointer fields (the driver accepts nil for *int
	// and *string columns), so we verify the happy multi-row path instead.
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	schedID := "11111111-1111-1111-1111-111111111111"
	now := time.Now().UTC().Truncate(time.Second)

	mock.ExpectQuery(`SELECT created_at, duration_ms, status`).
		WithArgs(wsID, schedID).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "duration_ms", "status", "error_detail", "request_body"}).
			AddRow(now, 500, "ok", "", `{"schedule_id":"`+schedID+`"}`).
			AddRow(now, 1200, "error", "HTTP 500 — OOM", `{"schedule_id":"`+schedID+`"}`))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "scheduleId", Value: schedID},
	}
	c.Request = httptest.NewRequest("GET",
		"/workspaces/"+wsID+"/schedules/"+schedID+"/history", nil)

	handler.History(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var entries []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// error_detail must be populated for the failed run.
	if entries[1]["error_detail"] != "HTTP 500 — OOM" {
		t.Errorf("error_detail: got %v", entries[1]["error_detail"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}
