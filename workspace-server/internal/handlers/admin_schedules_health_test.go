package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// adminHealthCols is the column set returned by the admin schedules health SELECT.
var adminHealthCols = []string{
	"workspace_id", "workspace_name",
	"schedule_id", "schedule_name",
	"cron_expr", "timezone",
	"last_run_at", "next_run_at",
}

// ==================== computeStaleThreshold unit tests ====================

// TestComputeStaleThreshold_FiveMinuteCron verifies that "*/5 * * * *" produces
// a 600 s (2 × 5 min) stale threshold.
func TestComputeStaleThreshold_FiveMinuteCron(t *testing.T) {
	threshold, err := computeStaleThreshold("*/5 * * * *", "UTC", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = 600 * time.Second
	if threshold != want {
		t.Errorf("expected %v, got %v", want, threshold)
	}
}

// TestComputeStaleThreshold_HourlyCron verifies that "0 * * * *" produces
// a 7200 s (2 h) stale threshold.
func TestComputeStaleThreshold_HourlyCron(t *testing.T) {
	threshold, err := computeStaleThreshold("0 * * * *", "UTC", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = 2 * time.Hour
	if threshold != want {
		t.Errorf("expected %v, got %v", want, threshold)
	}
}

// TestComputeStaleThreshold_DailyCron verifies that "0 9 * * *" (09:00 UTC daily)
// produces a 48 h (2 × 24 h) stale threshold.
func TestComputeStaleThreshold_DailyCron(t *testing.T) {
	threshold, err := computeStaleThreshold("0 9 * * *", "UTC", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = 48 * time.Hour
	if threshold != want {
		t.Errorf("expected %v, got %v", want, threshold)
	}
}

// TestComputeStaleThreshold_InvalidCron verifies that a malformed cron expression
// returns an error rather than silently returning zero.
func TestComputeStaleThreshold_InvalidCron(t *testing.T) {
	_, err := computeStaleThreshold("not-a-cron", "UTC", time.Now())
	if err == nil {
		t.Error("expected error for invalid cron expression, got nil")
	}
}

// TestComputeStaleThreshold_InvalidTimezone verifies that an unknown timezone
// returns an error.
func TestComputeStaleThreshold_InvalidTimezone(t *testing.T) {
	_, err := computeStaleThreshold("*/5 * * * *", "Not/ATimezone", time.Now())
	if err == nil {
		t.Error("expected error for invalid timezone, got nil")
	}
}

// ==================== classifyScheduleStatus unit tests ====================

// TestClassifyScheduleStatus_NeverRun verifies nil last_run_at → "never_run".
func TestClassifyScheduleStatus_NeverRun(t *testing.T) {
	status := classifyScheduleStatus(nil, 10*time.Minute, time.Now())
	if status != "never_run" {
		t.Errorf("expected never_run, got %q", status)
	}
}

// TestClassifyScheduleStatus_Stale verifies that a run older than the threshold
// produces "stale".
func TestClassifyScheduleStatus_Stale(t *testing.T) {
	now := time.Now()
	lastRun := now.Add(-11 * time.Minute) // older than 10-min threshold
	status := classifyScheduleStatus(&lastRun, 10*time.Minute, now)
	if status != "stale" {
		t.Errorf("expected stale, got %q", status)
	}
}

// TestClassifyScheduleStatus_OK verifies that a run within the threshold → "ok".
func TestClassifyScheduleStatus_OK(t *testing.T) {
	now := time.Now()
	lastRun := now.Add(-4 * time.Minute) // within 10-min threshold
	status := classifyScheduleStatus(&lastRun, 10*time.Minute, now)
	if status != "ok" {
		t.Errorf("expected ok, got %q", status)
	}
}

// TestClassifyScheduleStatus_ZeroThreshold_NeverStale verifies that when
// the threshold is 0 (cron parse failed), a run is never classified as stale
// — we degrade gracefully rather than false-alarming.
func TestClassifyScheduleStatus_ZeroThreshold_NeverStale(t *testing.T) {
	now := time.Now()
	lastRun := now.Add(-365 * 24 * time.Hour) // very old run
	status := classifyScheduleStatus(&lastRun, 0, now)
	if status != "ok" {
		t.Errorf("expected ok (zero threshold = no stale detection), got %q", status)
	}
}

// ==================== AdminSchedulesHealthHandler integration tests ====================

// TestAdminSchedulesHealth_Empty verifies that 200 + empty array is returned
// when no schedules exist.
func TestAdminSchedulesHealth_Empty(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAdminSchedulesHealthHandler()

	mock.ExpectQuery(`SELECT\s+w\.id`).
		WillReturnRows(sqlmock.NewRows(adminHealthCols))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/schedules/health", nil)

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []adminScheduleHealth
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("expected empty array, got %d entries", len(resp))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAdminSchedulesHealth_NeverRun verifies that a schedule with last_run_at=NULL
// is classified as "never_run" and that stale_threshold_seconds is computed
// correctly from the cron expression.
func TestAdminSchedulesHealth_NeverRun(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAdminSchedulesHealthHandler()

	nextRun := time.Now().Add(5 * time.Minute)
	mock.ExpectQuery(`SELECT\s+w\.id`).
		WillReturnRows(sqlmock.NewRows(adminHealthCols).AddRow(
			"ws-aaa", "Alpha WS",
			"sched-1", "hourly",
			"0 * * * *", "UTC",
			nil, &nextRun,
		))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/schedules/health", nil)

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []adminScheduleHealth
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(resp))
	}
	if resp[0].Status != "never_run" {
		t.Errorf("expected status=never_run, got %q", resp[0].Status)
	}
	if resp[0].LastRunAt != nil {
		t.Errorf("expected last_run_at=nil, got %v", resp[0].LastRunAt)
	}
	// "0 * * * *" → interval = 1 h → stale_threshold = 2 h = 7200 s
	if resp[0].StaleThresholdSeconds != 7200 {
		t.Errorf("expected stale_threshold_seconds=7200 for hourly cron, got %d",
			resp[0].StaleThresholdSeconds)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAdminSchedulesHealth_StaleDetection verifies that a schedule whose
// last_run_at is older than 2× its cron interval is classified as "stale".
func TestAdminSchedulesHealth_StaleDetection(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAdminSchedulesHealthHandler()

	// "*/5 * * * *" (every 5 min). Stale threshold = 2 × 5 min = 10 min.
	// Set last_run_at to 15 minutes ago → stale.
	lastRun := time.Now().Add(-15 * time.Minute)
	nextRun := time.Now().Add(5 * time.Minute)
	mock.ExpectQuery(`SELECT\s+w\.id`).
		WillReturnRows(sqlmock.NewRows(adminHealthCols).AddRow(
			"ws-bbb", "Beta WS",
			"sched-2", "every5min",
			"*/5 * * * *", "UTC",
			&lastRun, &nextRun,
		))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/schedules/health", nil)

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []adminScheduleHealth
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(resp))
	}
	if resp[0].Status != "stale" {
		t.Errorf("expected status=stale (last run 15m ago, threshold 10m), got %q",
			resp[0].Status)
	}
	// Stale threshold = 2 × 5 min = 600 s
	if resp[0].StaleThresholdSeconds != 600 {
		t.Errorf("expected stale_threshold_seconds=600, got %d",
			resp[0].StaleThresholdSeconds)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAdminSchedulesHealth_OKStatus verifies that a recently-run schedule
// (within 2× its cron interval) is classified as "ok".
func TestAdminSchedulesHealth_OKStatus(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAdminSchedulesHealthHandler()

	// "*/30 * * * *" (every 30 min). Stale threshold = 2 × 30 min = 60 min.
	// last_run_at = 20 min ago → ok.
	lastRun := time.Now().Add(-20 * time.Minute)
	nextRun := time.Now().Add(10 * time.Minute)
	mock.ExpectQuery(`SELECT\s+w\.id`).
		WillReturnRows(sqlmock.NewRows(adminHealthCols).AddRow(
			"ws-ccc", "Gamma WS",
			"sched-3", "every30min",
			"*/30 * * * *", "UTC",
			&lastRun, &nextRun,
		))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/schedules/health", nil)

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []adminScheduleHealth
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(resp))
	}
	if resp[0].Status != "ok" {
		t.Errorf("expected status=ok (20m ago, threshold 60m), got %q", resp[0].Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAdminSchedulesHealth_DBError verifies that a DB failure returns 500, not a panic.
func TestAdminSchedulesHealth_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAdminSchedulesHealthHandler()

	mock.ExpectQuery(`SELECT\s+w\.id`).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/schedules/health", nil)

	handler.Health(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on DB error, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAdminSchedulesHealth_MultipleWorkspaces verifies that schedules from
// multiple workspaces are all returned in order with correct workspace metadata
// and individual status classifications.
func TestAdminSchedulesHealth_MultipleWorkspaces(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAdminSchedulesHealthHandler()

	now := time.Now()
	recentRun := now.Add(-1 * time.Minute)  // within 2h threshold → ok
	nextRun := now.Add(59 * time.Minute)

	mock.ExpectQuery(`SELECT\s+w\.id`).
		WillReturnRows(sqlmock.NewRows(adminHealthCols).
			AddRow("ws-1", "WS One", "s1", "hourly-1", "0 * * * *", "UTC",
				&recentRun, &nextRun).
			AddRow("ws-2", "WS Two", "s2", "hourly-2", "0 * * * *", "America/New_York",
				nil, &nextRun))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/schedules/health", nil)

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []adminScheduleHealth
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(resp))
	}

	// First entry: ws-1, recently run within threshold → ok
	if resp[0].WorkspaceID != "ws-1" {
		t.Errorf("expected ws-1 first, got %q", resp[0].WorkspaceID)
	}
	if resp[0].WorkspaceName != "WS One" {
		t.Errorf("expected workspace_name=WS One, got %q", resp[0].WorkspaceName)
	}
	if resp[0].Status != "ok" {
		t.Errorf("expected ok for ws-1 schedule, got %q", resp[0].Status)
	}

	// Second entry: ws-2, never run
	if resp[1].WorkspaceID != "ws-2" {
		t.Errorf("expected ws-2 second, got %q", resp[1].WorkspaceID)
	}
	if resp[1].Status != "never_run" {
		t.Errorf("expected never_run for ws-2 schedule, got %q", resp[1].Status)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAdminSchedulesHealth_ResponseFields verifies that all required fields
// (workspace_id, workspace_name, schedule_id, schedule_name, cron_expr,
// last_run_at, expected_next_run, status, stale_threshold_seconds) are
// present in the JSON response.
func TestAdminSchedulesHealth_ResponseFields(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAdminSchedulesHealthHandler()

	lastRun := time.Now().Add(-1 * time.Minute)
	nextRun := time.Now().Add(4 * time.Minute)
	mock.ExpectQuery(`SELECT\s+w\.id`).
		WillReturnRows(sqlmock.NewRows(adminHealthCols).AddRow(
			"ws-fields", "Fields WS",
			"sched-fields", "test-schedule",
			"*/5 * * * *", "UTC",
			&lastRun, &nextRun,
		))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/schedules/health", nil)

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Parse as raw map to check field presence
	var rawResp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &rawResp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(rawResp) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(rawResp))
	}

	requiredFields := []string{
		"workspace_id", "workspace_name",
		"schedule_id", "schedule_name",
		"cron_expr", "last_run_at", "expected_next_run",
		"status", "stale_threshold_seconds",
	}
	entry := rawResp[0]
	for _, field := range requiredFields {
		if _, ok := entry[field]; !ok {
			t.Errorf("response missing required field %q", field)
		}
	}

	if entry["workspace_id"] != "ws-fields" {
		t.Errorf("workspace_id mismatch: %v", entry["workspace_id"])
	}
	if entry["schedule_name"] != "test-schedule" {
		t.Errorf("schedule_name mismatch: %v", entry["schedule_name"])
	}
	if entry["cron_expr"] != "*/5 * * * *" {
		t.Errorf("cron_expr mismatch: %v", entry["cron_expr"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// ==================== Orphans + ReapOrphans (internal#2006) ====================

// TestAdminSchedulesOrphans verifies the monitor surface lists schedules bound
// to a removed/missing workspace (the recreate-orphan failure mode).
func TestAdminSchedulesOrphans(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewAdminSchedulesHealthHandler()

	mock.ExpectQuery(`LEFT JOIN workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{
			"workspace_id", "ws_status", "id", "name", "source", "enabled", "cron_expr",
		}).AddRow("dead-ws", "removed", "sched-1", "minimax-autonomous-tick", "runtime", false, "*/5 * * * *"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/schedules/orphans", nil)

	handler.Orphans(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []orphanScheduleEntry
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 orphan, got %d", len(resp))
	}
	if resp[0].ScheduleName != "minimax-autonomous-tick" || resp[0].WorkspaceStatus != "removed" || resp[0].Source != "runtime" {
		t.Errorf("unexpected orphan entry: %+v", resp[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReapOrphans verifies the cleaner re-points runtime schedules onto a live
// successor then disables any remaining dead-bound schedules, returning counts.
func TestReapOrphans(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewAdminSchedulesHealthHandler()

	mock.ExpectExec(`UPDATE workspace_schedules s\s+SET workspace_id`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`UPDATE workspace_schedules s\s+SET enabled = false`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/schedules/reap-orphans", nil)

	handler.ReapOrphans(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]int64
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp["repointed"] != 2 || resp["disabled"] != 1 {
		t.Errorf("expected repointed=2 disabled=1, got %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
