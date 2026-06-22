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

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// Issue #24 — DB is the source of truth; org/import is additive on
// template-source rows only. Runtime-added schedules survive re-imports.

// TestRuntimeSchedule_HasSourceRuntime asserts that POST /workspaces/:id/schedules
// writes source='runtime' so that re-imports of the org template never touch
// these user-created rows (preserved across re-imports).
func TestRuntimeSchedule_HasSourceRuntime(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	// Match the literal 'runtime' source baked into the INSERT and capture
	// the workspace id arg. The inserted row id is returned via RETURNING.
	mock.ExpectQuery("INSERT INTO workspace_schedules .* VALUES .* 'runtime'").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "test", "*/5 * * * *", "UTC", "do thing", true, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("11111111-1111-1111-1111-111111111111"))

	body := []byte(`{"name":"test","cron_expr":"*/5 * * * *","prompt":"do thing"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/schedules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestImport_OrgScheduleSQLShape verifies the SQL emitted by the org/import
// path for schedules. It MUST be an INSERT ... ON CONFLICT (workspace_id, name)
// DO UPDATE ... WHERE source='template' with VALUES ... 'template'. Together
// these guarantee that re-import is:
//   - additive (new template rows are inserted),
//   - idempotent (existing template rows are refreshed),
//   - non-destructive of runtime rows (the WHERE filter skips them),
//   - never DELETE-based (additive only).
//
// This is a structural assertion against the source — cheap and catches a
// regression that would silently break user-created schedules across
// re-imports without needing a full provisioner harness.
func TestImport_OrgScheduleSQLShape(t *testing.T) {
	got := orgImportScheduleSQL

	// Single test covers four CEO requirements at once: additive seed
	// (template marker), idempotent refresh (ON CONFLICT DO UPDATE),
	// runtime-row preservation (WHERE source='template'), and never-DELETE.
	mustContain := []string{
		"INSERT INTO workspace_schedules",
		"source",
		"'template'",
		"ON CONFLICT (workspace_id, name) DO UPDATE",
		"WHERE workspace_schedules.source = 'template'",
	}
	for _, s := range mustContain {
		if !strings.Contains(got, s) {
			t.Errorf("org/import schedule SQL missing fragment %q\n--- SQL ---\n%s", s, got)
		}
	}
	if regexp.MustCompile(`(?i)\bDELETE\b\s+FROM\s+workspace_schedules`).MatchString(got) {
		t.Error("org/import schedule SQL must never DELETE — additive only")
	}
}

// TestList_IncludesSourceColumn asserts GET /workspaces/:id/schedules
// returns the source field so Canvas can render template/runtime badges.
func TestList_IncludesSourceColumn(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	cols := []string{
		"id", "workspace_id", "name", "cron_expr", "timezone", "prompt", "enabled",
		"last_run_at", "next_run_at", "run_count", "last_status", "last_error",
		"source", "created_at", "updated_at",
	}
	now := time.Now()
	mock.ExpectQuery("SELECT .* source, created_at, updated_at\\s+FROM workspace_schedules").
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("id1", "550e8400-e29b-41d4-a716-446655440000", "tmpl-sched", "0 * * * *", "UTC", "p", true,
				nil, nil, 0, "", "", "template", now, now).
			AddRow("id2", "550e8400-e29b-41d4-a716-446655440000", "user-sched", "*/5 * * * *", "UTC", "p2", true,
				nil, nil, 0, "", "", "runtime", now, now))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/550e8400-e29b-41d4-a716-446655440000/schedules", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"source":"template"`) {
		t.Errorf(`response missing "source":"template": %s`, body)
	}
	if !strings.Contains(body, `"source":"runtime"`) {
		t.Errorf(`response missing "source":"runtime": %s`, body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestHistory_IncludesErrorDetail — #152 problem B coverage. The history
// endpoint must surface error_detail from activity_logs so clients know
// why a cron run failed (not just that it failed). Writes a fake cron_run
// row via sqlmock with a non-empty error_detail and asserts it reaches
// the JSON response.
func TestHistory_IncludesErrorDetail(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	workspaceID := "550e8400-e29b-41d4-a716-446655440000"
	scheduleID := "11111111-1111-1111-1111-111111111111"
	now := time.Now()

	cols := []string{"created_at", "duration_ms", "status", "error_detail", "request_body"}
	mock.ExpectQuery("SELECT created_at, duration_ms, status").
		WithArgs(workspaceID, scheduleID).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow(now, 4200, "error", "HTTP 500 — workspace agent OOM", `{"schedule_id":"`+scheduleID+`"}`).
			AddRow(now, 1500, "ok", "", `{"schedule_id":"`+scheduleID+`"}`))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: workspaceID},
		{Key: "scheduleId", Value: scheduleID},
	}
	c.Request = httptest.NewRequest("GET",
		"/workspaces/"+workspaceID+"/schedules/"+scheduleID+"/history", nil)

	handler.History(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"error_detail":"HTTP 500 — workspace agent OOM"`) {
		t.Errorf("history response missing populated error_detail: %s", body)
	}
	if !strings.Contains(body, `"error_detail":""`) {
		t.Errorf("history response missing empty error_detail on ok row: %s", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// ==================== Health — issue #249 ====================
//
// GET /workspaces/:id/schedules/health is accessible to CanCommunicate peers
// without workspace bearer auth. The handler mirrors the A2A proxy's auth
// pattern: X-Workspace-ID + caller token + CanCommunicate gate.

const healthWorkspaceID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
const healthCallerID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

// healthCols is the column set returned by the Health SELECT.
var healthCols = []string{"id", "name", "enabled", "last_run_at", "next_run_at", "run_count", "last_status", "last_error"}

// TestScheduleHealth_MissingCallerID_Rejected verifies that requests without
// X-Workspace-ID are rejected with 401 — anonymous peer reads are not allowed.
func TestScheduleHealth_MissingCallerID_Rejected(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: healthWorkspaceID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+healthWorkspaceID+"/schedules/health", nil)

	handler.Health(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing caller, got %d: %s", w.Code, w.Body.String())
	}
}

// TestScheduleHealth_SelfCall_Allowed verifies that when callerID == workspaceID
// (self-call) the request is allowed and health fields are returned without any
// CanCommunicate DB lookups.
func TestScheduleHealth_SelfCall_Allowed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	now := time.Now().UTC().Truncate(time.Second)
	// Self-call: no token check, no CanCommunicate queries.
	// Expect only the health SELECT.
	mock.ExpectQuery(`SELECT id, name, enabled, last_run_at, next_run_at, run_count, last_status, last_error\s+FROM workspace_schedules`).
		WithArgs(healthWorkspaceID).
		WillReturnRows(sqlmock.NewRows(healthCols).
			AddRow("sched-1", "nightly", true, &now, &now, 42, "ok", ""))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: healthWorkspaceID}}
	req := httptest.NewRequest("GET", "/workspaces/"+healthWorkspaceID+"/schedules/health", nil)
	req.Header.Set("X-Workspace-ID", healthWorkspaceID) // self-call
	c.Request = req

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for self-call, got %d: %s", w.Code, w.Body.String())
	}

	var resp []ScheduleHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 1 || resp[0].ID != "sched-1" || resp[0].RunCount != 42 {
		t.Errorf("unexpected health response: %+v", resp)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestScheduleHealth_CanCommunicatePeer_LegacyNoToken verifies that a legacy
// peer (no live tokens on file for the caller) is grandfathered through the
// token check and can read health when CanCommunicate is satisfied.
func TestScheduleHealth_CanCommunicatePeer_LegacyNoToken(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	now := time.Now().UTC().Truncate(time.Second)

	// 1. validateCallerToken: caller has zero live tokens → grandfather through.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(healthCallerID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// 2. CanCommunicate: caller and target share the same parent (siblings → allowed).
	mockCanCommunicate(mock, healthCallerID, healthWorkspaceID, true)

	// 3. Health SELECT.
	mock.ExpectQuery(`SELECT id, name, enabled, last_run_at, next_run_at, run_count, last_status, last_error\s+FROM workspace_schedules`).
		WithArgs(healthWorkspaceID).
		WillReturnRows(sqlmock.NewRows(healthCols).
			AddRow("sched-2", "hourly", true, &now, &now, 7, "ok", ""))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: healthWorkspaceID}}
	req := httptest.NewRequest("GET", "/workspaces/"+healthWorkspaceID+"/schedules/health", nil)
	req.Header.Set("X-Workspace-ID", healthCallerID)
	c.Request = req

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for peer with no tokens, got %d: %s", w.Code, w.Body.String())
	}

	var resp []ScheduleHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 1 || resp[0].RunCount != 7 {
		t.Errorf("unexpected response: %+v", resp)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestScheduleHealth_AccessDenied_NonPeer verifies that a workspace which fails
// CanCommunicate (different org branch) receives 403 — not 401 or 500.
func TestScheduleHealth_AccessDenied_NonPeer(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	// 1. validateCallerToken: no live tokens → grandfather.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(healthCallerID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// 2. CanCommunicate: different parents → denied.
	mockCanCommunicate(mock, healthCallerID, healthWorkspaceID, false)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: healthWorkspaceID}}
	req := httptest.NewRequest("GET", "/workspaces/"+healthWorkspaceID+"/schedules/health", nil)
	req.Header.Set("X-Workspace-ID", healthCallerID)
	c.Request = req

	handler.Health(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-peer, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestScheduleHealth_SystemCaller_Allowed verifies that system callers
// (webhook:*, system:*, test:*) bypass token + CanCommunicate checks.
func TestScheduleHealth_SystemCaller_Allowed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	now := time.Now().UTC().Truncate(time.Second)

	// No token check, no CanCommunicate queries — just the health SELECT.
	mock.ExpectQuery(`SELECT id, name, enabled, last_run_at, next_run_at, run_count, last_status, last_error\s+FROM workspace_schedules`).
		WithArgs(healthWorkspaceID).
		WillReturnRows(sqlmock.NewRows(healthCols).
			AddRow("sched-3", "weekly", false, nil, &now, 0, "", ""))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: healthWorkspaceID}}
	req := httptest.NewRequest("GET", "/workspaces/"+healthWorkspaceID+"/schedules/health", nil)
	req.Header.Set("X-Workspace-ID", "system:monitor")
	c.Request = req

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for system caller, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestScheduleHealth_NoPromptExposed verifies that the health response never
// includes prompt or cron_expr — only execution-state fields are returned.
func TestScheduleHealth_NoPromptExposed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	now := time.Now().UTC().Truncate(time.Second)

	// No token check, no CanCommunicate queries for system caller.
	mock.ExpectQuery(`SELECT id, name, enabled, last_run_at, next_run_at, run_count, last_status, last_error\s+FROM workspace_schedules`).
		WithArgs(healthWorkspaceID).
		WillReturnRows(sqlmock.NewRows(healthCols).
			AddRow("sched-4", "daily", true, &now, &now, 3, "ok", ""))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: healthWorkspaceID}}
	req := httptest.NewRequest("GET", "/workspaces/"+healthWorkspaceID+"/schedules/health", nil)
	req.Header.Set("X-Workspace-ID", "system:test")
	c.Request = req

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	rawBody := w.Body.String()
	for _, forbidden := range []string{"prompt", "cron_expr", "timezone"} {
		if strings.Contains(rawBody, forbidden) {
			t.Errorf("health response must not contain %q field: %s", forbidden, rawBody)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestScheduleHealth_DBError_Returns500 verifies that a DB failure on the health
// SELECT produces a 500, not a panic.
func TestScheduleHealth_DBError_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	// No token check, no CanCommunicate queries for system caller.
	mock.ExpectQuery(`SELECT id, name, enabled, last_run_at, next_run_at, run_count, last_status, last_error\s+FROM workspace_schedules`).
		WithArgs(healthWorkspaceID).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: healthWorkspaceID}}
	req := httptest.NewRequest("GET", "/workspaces/"+healthWorkspaceID+"/schedules/health", nil)
	req.Header.Set("X-Workspace-ID", "system:test")
	c.Request = req

	handler.Health(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on DB error, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
