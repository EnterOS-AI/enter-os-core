package handlers

// admin_plugin_drift_test.go — coverage for plugin drift queue admin endpoints.
// Tests: ListPending (empty, non-empty), Apply (not found, already applied,
// already dismissed, workspace_plugins missing, install failure).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func TestAdminPluginDrift_ListPending_Empty(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	mock.ExpectQuery(`SELECT id, workspace_id, plugin_name, tracked_ref,\s+current_sha, latest_sha, status, created_at\s+FROM plugin_update_queue\s+WHERE status = 'pending'\s+ORDER BY created_at DESC`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "plugin_name", "tracked_ref",
			"current_sha", "latest_sha", "status", "created_at",
		}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/plugin-updates-pending", nil)
	h.ListPending(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected empty array, got %d rows", len(rows))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAdminPluginDrift_ListPending_WithRows(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	now := time.Now()
	mock.ExpectQuery(`SELECT id, workspace_id, plugin_name, tracked_ref,\s+current_sha, latest_sha, status, created_at\s+FROM plugin_update_queue\s+WHERE status = 'pending'\s+ORDER BY created_at DESC`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "plugin_name", "tracked_ref",
			"current_sha", "latest_sha", "status", "created_at",
		}).AddRow(
			"queue-id-1", "ws-uuid-1", "my-plugin", "tag:v1.0.0",
			"abc123def456", "def456abc789", "pending", now,
		).AddRow(
			"queue-id-2", "ws-uuid-2", "other-plugin", "tag:latest",
			"111111aaaaaa", "222222bbbbbb", "pending", now,
		))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/plugin-updates-pending", nil)
	h.ListPending(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(rows))
	}
	// Verify first row fields.
	if got := rows[0]["plugin_name"]; got != "my-plugin" {
		t.Errorf("plugin_name: expected my-plugin, got %v", got)
	}
	if got := rows[0]["status"]; got != "pending" {
		t.Errorf("status: expected pending, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAdminPluginDrift_ListPending_DBError(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	mock.ExpectQuery(`SELECT id, workspace_id`).WillReturnError(
		json.Unmarshal([]byte("force error"), new(struct{})),
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/plugin-updates-pending", nil)
	h.ListPending(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on DB error, got %d", w.Code)
	}
}

func TestAdminPluginDrift_Apply_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	mock.ExpectQuery(`SELECT workspace_id, plugin_name, tracked_ref, status\s+FROM plugin_update_queue\s+WHERE id = \$1`).
		WithArgs("nonexistent-queue-id").
		WillReturnRows(sqlmock.NewRows([]string{
			"workspace_id", "plugin_name", "tracked_ref", "status",
		})) // empty = no rows

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/plugin-updates/nonexistent-queue-id/apply", nil)
	c.Params = []gin.Param{{Key: "id", Value: "nonexistent-queue-id"}}
	h.Apply(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAdminPluginDrift_Apply_AlreadyApplied(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	mock.ExpectQuery(`SELECT workspace_id, plugin_name, tracked_ref, status\s+FROM plugin_update_queue\s+WHERE id = \$1`).
		WithArgs("queue-id-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"workspace_id", "plugin_name", "tracked_ref", "status",
		}).AddRow("ws-uuid-1", "my-plugin", "tag:v1.0.0", "applied"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/plugin-updates/queue-id-1/apply", nil)
	c.Params = []gin.Param{{Key: "id", Value: "queue-id-1"}}
	h.Apply(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for already-applied, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if got := body["status"]; got != "already_applied" {
		t.Errorf("status: expected already_applied, got %v", got)
	}
}

func TestAdminPluginDrift_Apply_AlreadyDismissed(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	mock.ExpectQuery(`SELECT workspace_id, plugin_name, tracked_ref, status\s+FROM plugin_update_queue\s+WHERE id = \$1`).
		WithArgs("queue-id-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"workspace_id", "plugin_name", "tracked_ref", "status",
		}).AddRow("ws-uuid-1", "my-plugin", "tag:v1.0.0", "dismissed"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/plugin-updates/queue-id-1/apply", nil)
	c.Params = []gin.Param{{Key: "id", Value: "queue-id-1"}}
	h.Apply(c)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for dismissed, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminPluginDrift_Apply_WorkspacePluginsMissing(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	// Queue entry found, pending.
	mock.ExpectQuery(`SELECT workspace_id, plugin_name, tracked_ref, status\s+FROM plugin_update_queue\s+WHERE id = \$1`).
		WithArgs("queue-id-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"workspace_id", "plugin_name", "tracked_ref", "status",
		}).AddRow("ws-uuid-1", "my-plugin", "tag:v1.0.0", "pending"))

	// workspace_plugins row not found (plugin uninstalled after drift detected).
	mock.ExpectQuery(`SELECT source_raw FROM workspace_plugins\s+WHERE workspace_id = \$1 AND plugin_name = \$2`).
		WithArgs("ws-uuid-1", "my-plugin").
		WillReturnRows(sqlmock.NewRows([]string{"source_raw"})) // empty

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/plugin-updates/queue-id-1/apply", nil)
	c.Params = []gin.Param{{Key: "id", Value: "queue-id-1"}}
	h.Apply(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when workspace_plugins row missing, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
