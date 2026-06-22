package handlers

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// Tests for the since_id cursor on GET /workspaces/:id/activity (#2339 PR 3).
//
// Cursor shape: Telegram getUpdates / Slack RTM. The polling agent passes
// the id of the last activity_logs row it processed; the server returns
// rows STRICTLY AFTER that cursor in ASC order. Cross-workspace lookups
// return 410 to prevent UUID-guessing peeks at other workspaces' events.

// TestActivityHandler_SinceID_ReturnsNewerASC: with a valid cursor the
// handler does the cursor lookup, then queries with the cursor's
// created_at as a > filter and ASC ordering — the polling shape.
func TestActivityHandler_SinceID_ReturnsNewerASC(t *testing.T) {
	mock := setupTestDB(t)

	cursorID := "act-cursor-42"
	cursorTime := time.Date(2026, 4, 30, 5, 0, 0, 0, time.UTC)
	cursorSeq := int64(42)

	// Step 1: cursor lookup — must include workspace_id scope so a UUID
	// from another workspace can't be used. Now resolves BOTH ordering-key
	// components (created_at, seq) so the strictly-after filter can compare
	// the full tuple.
	mock.ExpectQuery(`SELECT created_at, seq FROM activity_logs WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(cursorID, "ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "seq"}).AddRow(cursorTime, cursorSeq))

	// Step 2: main query with the cursor's (created_at, seq) as a tuple
	// strictly-after filter, (created_at, seq) ASC ordering.
	// Args: workspace_id, cursorTime, cursorSeq, limit.
	mock.ExpectQuery("SELECT id, workspace_id, activity_type").
		WithArgs("ws-1", cursorTime, cursorSeq, 100).
		WillReturnRows(newActivityRows())

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?since_id="+cursorID, nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestActivityHandler_SinceID_CursorNotFound_410: cursor row doesn't exist
// (pruned, never existed, or wrong UUID). Server returns 410 Gone so the
// client knows to reset its cursor — silent empty results would cause a
// stuck-poll bug where the agent never sees new events.
func TestActivityHandler_SinceID_CursorNotFound_410(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectQuery(`SELECT created_at, seq FROM activity_logs WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs("act-gone", "ws-1").
		WillReturnError(sql.ErrNoRows)

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?since_id=act-gone", nil)

	handler.List(c)

	if w.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestActivityHandler_SinceID_CrossWorkspaceCursor_410: a caller passes a
// UUID that belongs to a different workspace. The cursor lookup is scoped
// by workspace_id so the row is "not found" from this caller's perspective —
// same 410 path as the pruned case. No information leak (caller cannot tell
// whether the UUID belongs to nobody or to another workspace).
func TestActivityHandler_SinceID_CrossWorkspaceCursor_410(t *testing.T) {
	mock := setupTestDB(t)

	// Cursor exists in DB but the WHERE workspace_id = $2 filter excludes
	// it — sqlmock returns no rows, which is what Postgres would do.
	mock.ExpectQuery(`SELECT created_at, seq FROM activity_logs WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs("act-other-ws", "ws-1").
		WillReturnError(sql.ErrNoRows)

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/activity?since_id=act-other-ws", nil)

	handler.List(c)

	if w.Code != http.StatusGone {
		t.Fatalf("cross-workspace cursor: expected 410, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestActivityHandler_SinceID_CombinedWithSinceSecs: both filters apply
// together (AND). Argument order in the main query: workspace_id,
// since_secs, cursorTime, cursorSeq, limit. Sanity-checks the placeholder
// index arithmetic in the query builder (the cursor now binds TWO args —
// the (created_at, seq) tuple — so since_secs no longer shifts the tail by
// one but by two).
func TestActivityHandler_SinceID_CombinedWithSinceSecs(t *testing.T) {
	mock := setupTestDB(t)

	cursorID := "act-c"
	cursorTime := time.Date(2026, 4, 30, 4, 0, 0, 0, time.UTC)
	cursorSeq := int64(7)

	mock.ExpectQuery(`SELECT created_at, seq FROM activity_logs WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(cursorID, "ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "seq"}).AddRow(cursorTime, cursorSeq))

	mock.ExpectQuery("SELECT id, workspace_id, activity_type").
		WithArgs("ws-1", 600, cursorTime, cursorSeq, 100).
		WillReturnRows(newActivityRows())

	broadcaster := newTestBroadcaster()
	handler := NewActivityHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET",
		"/workspaces/ws-1/activity?since_secs=600&since_id="+cursorID, nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
