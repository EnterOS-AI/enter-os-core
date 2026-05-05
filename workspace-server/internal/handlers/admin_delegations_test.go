package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// admin_delegations_test.go — RFC #2829 PR-4 dashboard endpoint coverage.
//
//   - List: status filter + limit defaults + bad-input rejection
//   - Stats: per-status counts + zero-fill for missing statuses

// ---------- List ----------

func TestAdminDelegations_List_DefaultStatusInFlight(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminDelegationsHandler(nil)

	now := time.Now()
	mock.ExpectQuery(`SELECT delegation_id, caller_id::text, callee_id::text, task_preview,\s+status, last_heartbeat, deadline, result_preview, error_detail,\s+retry_count, created_at, updated_at\s+FROM delegations\s+WHERE status IN \(\$1,\$2,\$3\)\s+ORDER BY created_at DESC\s+LIMIT \$4`).
		WithArgs("queued", "dispatched", "in_progress", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"delegation_id", "caller_id", "callee_id", "task_preview",
			"status", "last_heartbeat", "deadline", "result_preview", "error_detail",
			"retry_count", "created_at", "updated_at",
		}).AddRow(
			"deleg-1", "caller-uuid", "callee-uuid", "task body",
			"in_progress", now, now.Add(2*time.Hour), nil, nil,
			0, now.Add(-5*time.Minute), now.Add(-1*time.Minute),
		))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/delegations", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if got := body["count"]; got != float64(1) {
		t.Errorf("count: expected 1, got %v", got)
	}
	if got := body["status"]; got != "in_flight" {
		t.Errorf("status: expected in_flight, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestAdminDelegations_List_StatusStuck(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminDelegationsHandler(nil)

	mock.ExpectQuery(`SELECT delegation_id`).
		WithArgs("stuck", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"delegation_id", "caller_id", "callee_id", "task_preview",
			"status", "last_heartbeat", "deadline", "result_preview", "error_detail",
			"retry_count", "created_at", "updated_at",
		}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/delegations?status=stuck", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestAdminDelegations_List_StatusFailed(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminDelegationsHandler(nil)

	mock.ExpectQuery(`SELECT delegation_id`).
		WithArgs("failed", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"delegation_id", "caller_id", "callee_id", "task_preview",
			"status", "last_heartbeat", "deadline", "result_preview", "error_detail",
			"retry_count", "created_at", "updated_at",
		}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/delegations?status=failed", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestAdminDelegations_List_RejectsUnknownStatus(t *testing.T) {
	setupTestDB(t)
	h := NewAdminDelegationsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/delegations?status=garbage", nil)
	h.List(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminDelegations_List_RejectsNegativeLimit(t *testing.T) {
	setupTestDB(t)
	h := NewAdminDelegationsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/delegations?limit=-5", nil)
	h.List(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAdminDelegations_List_RejectsLimitOverCap(t *testing.T) {
	setupTestDB(t)
	h := NewAdminDelegationsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/delegations?limit=99999", nil)
	h.List(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAdminDelegations_List_AcceptsCustomLimit(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminDelegationsHandler(nil)

	mock.ExpectQuery(`SELECT delegation_id`).
		WithArgs("queued", "dispatched", "in_progress", 25).
		WillReturnRows(sqlmock.NewRows([]string{
			"delegation_id", "caller_id", "callee_id", "task_preview",
			"status", "last_heartbeat", "deadline", "result_preview", "error_detail",
			"retry_count", "created_at", "updated_at",
		}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/delegations?limit=25", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["limit"] != float64(25) {
		t.Errorf("expected limit=25 echo, got %v", body["limit"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestAdminDelegations_List_PopulatesNullableFields(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminDelegationsHandler(nil)

	now := time.Now()
	resultStr := "all done"
	mock.ExpectQuery(`SELECT delegation_id`).
		WithArgs("completed", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"delegation_id", "caller_id", "callee_id", "task_preview",
			"status", "last_heartbeat", "deadline", "result_preview", "error_detail",
			"retry_count", "created_at", "updated_at",
		}).AddRow(
			"deleg-2", "c", "ca", "t",
			"completed", now, now.Add(2*time.Hour), resultStr, nil,
			0, now, now,
		))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/delegations?status=completed", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		Delegations []struct {
			ResultPreview *string `json:"result_preview"`
			ErrorDetail   *string `json:"error_detail"`
			LastHeartbeat *string `json:"last_heartbeat"`
		} `json:"delegations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(body.Delegations) != 1 {
		t.Fatalf("expected 1 row, got %d", len(body.Delegations))
	}
	row := body.Delegations[0]
	if row.ResultPreview == nil || *row.ResultPreview != "all done" {
		t.Errorf("result_preview not populated correctly: %+v", row.ResultPreview)
	}
	if row.ErrorDetail != nil {
		t.Errorf("error_detail should be nil for completed-no-error: %+v", row.ErrorDetail)
	}
	if row.LastHeartbeat == nil {
		t.Errorf("last_heartbeat should be present (non-NULL); got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// ---------- Stats ----------

func TestAdminDelegations_Stats_ZeroFillsMissingStatuses(t *testing.T) {
	// Stats response must always include every status key. If no rows
	// exist for status='stuck', the response still shows "stuck": 0.
	mock := setupTestDB(t)
	h := NewAdminDelegationsHandler(nil)

	mock.ExpectQuery(`SELECT status, COUNT\(\*\) FROM delegations GROUP BY status`).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).
			AddRow("in_progress", 7).
			AddRow("completed", 130))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/delegations/stats", nil)
	h.Stats(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var stats map[string]int
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("parse: %v", err)
	}

	expectedKeys := []string{"queued", "dispatched", "in_progress", "completed", "failed", "stuck"}
	for _, k := range expectedKeys {
		if _, ok := stats[k]; !ok {
			t.Errorf("stats missing key %q (zero-fill contract broken)", k)
		}
	}
	if stats["in_progress"] != 7 {
		t.Errorf("in_progress count: expected 7, got %d", stats["in_progress"])
	}
	if stats["completed"] != 130 {
		t.Errorf("completed count: expected 130, got %d", stats["completed"])
	}
	if stats["stuck"] != 0 {
		t.Errorf("stuck must be zero-filled: got %d", stats["stuck"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestAdminDelegations_Stats_EmptyTable(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminDelegationsHandler(nil)

	mock.ExpectQuery(`SELECT status, COUNT\(\*\) FROM delegations GROUP BY status`).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/delegations/stats", nil)
	h.Stats(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var stats map[string]int
	_ = json.Unmarshal(w.Body.Bytes(), &stats)
	for k, v := range stats {
		if v != 0 {
			t.Errorf("empty table → all counts zero; %s=%d", k, v)
		}
	}
}

// statusFilters is a contract surface — every key here is documented in
// the endpoint comment + accepted by the validator. Pin it.
func TestStatusFiltersTableShape(t *testing.T) {
	expected := map[string][]string{
		"in_flight": {"queued", "dispatched", "in_progress"},
		"stuck":     {"stuck"},
		"failed":    {"failed"},
		"completed": {"completed"},
	}
	for k, want := range expected {
		got, ok := statusFilters[k]
		if !ok {
			t.Errorf("statusFilters missing key %q", k)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("statusFilters[%q]: want %v, got %v", k, want, got)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("statusFilters[%q][%d]: want %q, got %q", k, i, want[i], got[i])
			}
		}
	}
}
