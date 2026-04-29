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

// Tests for the since_secs query parameter on GET /workspaces/:id/activity.
// Closes #2268 — the harness runner was passing this param and it was
// silently ignored, capping the trace at most-recent-100 events. The new
// shape: parse since_secs, add a parameterised `created_at >= NOW() -
// make_interval(secs => $N)` clause, cap at 30 days, reject invalid input
// with 400.

const activityCols = `id, workspace_id, activity_type, source_id, target_id, method, ` +
	`summary, request_body, response_body, tool_trace, duration_ms, status, error_detail, created_at`

func newActivityRows() *sqlmock.Rows {
	cols := []string{
		"id", "workspace_id", "activity_type", "source_id", "target_id", "method",
		"summary", "request_body", "response_body", "tool_trace", "duration_ms", "status", "error_detail", "created_at",
	}
	return sqlmock.NewRows(cols).
		AddRow("act-1", "ws-1", "a2a_send", nil, nil, nil,
			"sent", nil, nil, nil, nil, "ok", nil,
			time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC))
}

// TestActivityHandler_SinceSecs_Accepted verifies that a valid since_secs
// query param adds the make_interval clause to the SQL with the parsed
// value as a bound parameter — exactly what the runner needs to scope a
// trace to a test window.
func TestActivityHandler_SinceSecs_Accepted(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectQuery("SELECT id, workspace_id, activity_type").
		WithArgs("ws-1", 600, 100). // workspaceID, since_secs, limit
		WillReturnRows(newActivityRows())

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?since_secs=600", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestActivityHandler_SinceSecs_ClampedAt30Days verifies the defensive
// ceiling so a paranoid client can't trigger a multi-month full-table
// scan via since_secs=999999999.
func TestActivityHandler_SinceSecs_ClampedAt30Days(t *testing.T) {
	mock := setupTestDB(t)

	const cap30Days = 30 * 24 * 60 * 60
	mock.ExpectQuery("SELECT id, workspace_id, activity_type").
		WithArgs("ws-1", cap30Days, 100).
		WillReturnRows(newActivityRows())

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?since_secs=999999999", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestActivityHandler_SinceSecs_InvalidRejected covers the loud-fail path:
// a typoed param (non-int, zero, negative) returns 400 instead of being
// silently dropped — that's the bug this whole feature is fixing.
func TestActivityHandler_SinceSecs_InvalidRejected(t *testing.T) {
	cases := []struct {
		name string
		val  string
	}{
		{"non-integer", "abc"},
		{"zero", "0"},
		{"negative", "-1"},
		{"hex-prefix", "0x10"},
		{"float", "60.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No DB call expected; bad input must be caught before the query.
			setupTestDB(t)
			broadcaster := newTestBroadcaster()
			handler := NewActivityHandler(broadcaster)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
			c.Request = httptest.NewRequest("GET",
				"/workspaces/ws-1/activity?since_secs="+tc.val, nil)

			handler.List(c)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for %q, got %d: %s", tc.val, w.Code, w.Body.String())
			}
			var resp map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["error"] == "" {
				t.Errorf("expected error message in response body for %q", tc.val)
			}
		})
	}
}

// TestActivityHandler_SinceSecs_Omitted verifies backward compat — callers
// that don't pass since_secs see the original behavior (no extra WHERE
// clause, just workspace_id + limit).
func TestActivityHandler_SinceSecs_Omitted(t *testing.T) {
	mock := setupTestDB(t)

	// Only workspace_id + limit; the query must NOT include the
	// make_interval clause. sqlmock's WithArgs is strict on count, so a
	// since_secs leak would surface as "expected 2 args, got 3".
	mock.ExpectQuery("SELECT id, workspace_id, activity_type").
		WithArgs("ws-1", 100).
		WillReturnRows(newActivityRows())

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
