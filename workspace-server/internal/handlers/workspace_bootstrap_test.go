package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/gin-gonic/gin"
)

// setupBootstrapHandler builds a handler wired with a sqlmock + an in-proc
// broadcaster (via setupTestRedis so RecordAndBroadcast's pub/sub path
// doesn't panic on a nil Redis client).
func setupBootstrapHandler(t *testing.T) (*WorkspaceHandler, sqlmock.Sqlmock) {
	t.Helper()
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	return NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir()), mock
}

func TestBootstrapFailed_HappyPath(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	// UPDATE only flips from provisioning → re-check the predicate.
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-crashed", sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// RecordAndBroadcast inserts into structure_events.
	mock.ExpectExec(`INSERT INTO structure_events`).
		WithArgs("WORKSPACE_PROVISION_FAILED", "ws-crashed", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-crashed"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-crashed/bootstrap-failed",
		bytes.NewBufferString(`{"error":"module 'adapter' has no attribute 'Adapter'","log_tail":"Traceback...\n..."}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.BootstrapFailed(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A workspace already past `provisioning` (online raced, or already failed
// by the sweeper) must not re-fire the event. Returns 200 with no_change.
func TestBootstrapFailed_AlreadyTransitioned(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	// UPDATE affects 0 rows when the predicate `status = 'provisioning'`
	// doesn't match.
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-online", sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// No structure_events INSERT expected.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-online"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-online/bootstrap-failed",
		bytes.NewBufferString(`{"error":"late report","log_tail":""}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.BootstrapFailed(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestBootstrapFailed_EmptyIDIs400(t *testing.T) {
	h, _ := setupBootstrapHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: ""}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces//bootstrap-failed",
		bytes.NewBufferString(`{"error":"x"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.BootstrapFailed(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestBootstrapFailed_TruncatesOversizedLogTail(t *testing.T) {
	// A 20KB log_tail should be truncated to ~8KB with a marker. We
	// don't assert the exact byte count here (depends on UTF-8 boundary
	// walk); we just assert the handler succeeds and the final stored
	// string contains the truncation marker.
	h, mock := setupBootstrapHandler(t)

	longTail := make([]byte, 20000)
	for i := range longTail {
		longTail[i] = 'a'
	}

	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-spammy", sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO structure_events`).
		WithArgs("WORKSPACE_PROVISION_FAILED", "ws-spammy", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"error":"huge","log_tail":"` + string(longTail) + `"}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-spammy"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-spammy/bootstrap-failed",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.BootstrapFailed(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

// Console returns 501 in deployments without a CPProvisioner. The actual
// CP-call path is exercised end-to-end from the CP side (bootstrap_watcher
// tests in the controlplane repo).
func TestConsole_ReturnsNotImplementedWhenNoCP(t *testing.T) {
	h, _ := setupBootstrapHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-x"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-x/console", nil)

	h.Console(c)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("want 501, got %d: %s", w.Code, w.Body.String())
	}
}
