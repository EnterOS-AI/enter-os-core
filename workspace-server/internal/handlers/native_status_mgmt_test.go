package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestHeartbeat_NativeStatusMgmt_SkipsDegradeInference validates capability
// primitive #4: when an adapter declares native_status_mgmt, the platform's
// error-rate-based status inference DOES NOT fire. Adapter owns the
// transition; platform observes only. The wedged-branch (RuntimeState ==
// "wedged") is NOT gated — it's the adapter's own self-report, not an
// inference, and stays active.
//
// Mirrors the structure of TestHeartbeatHandler_Degraded but pre-populates
// the runtimeOverrides cache with status_mgmt=true and asserts the degrade
// UPDATE is NOT issued (so sqlmock's expectations don't include it).
func TestHeartbeat_NativeStatusMgmt_SkipsDegradeInference(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Pre-populate the override cache so the workspace under test has
	// declared native_status_mgmt. Reset after so we don't pollute
	// other tests in the package.
	runtimeOverrides.SetCapabilities("ws-native-status", map[string]bool{"status_mgmt": true})
	defer runtimeOverrides.Reset()

	// prevTask SELECT (before UPDATE)
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-native-status").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// heartbeat UPDATE — same as the non-native path
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-native-status", 0.8, "connection timeout", 0, 7200, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus SELECT — currently online, error_rate=0.8 would
	// normally fire the degrade UPDATE. Under native_status_mgmt, it
	// MUST NOT. We deliberately don't ExpectExec the degrade UPDATE
	// — sqlmock fails the test if any UPDATE happens that wasn't
	// expected, which is the regression cover.
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-native-status").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-native-status","error_rate":0.8,"sample_error":"connection timeout","active_tasks":0,"uptime_seconds":7200}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// CRITICAL: ExpectationsWereMet fails if the degrade UPDATE
	// happened (since we didn't expect it). This is the load-bearing
	// assertion for primitive #4.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (or unexpected query — likely the degrade UPDATE fired despite native_status_mgmt): %v", err)
	}
}

// TestHeartbeat_NativeStatusMgmt_SkipsRecovery validates the recovery
// branch is also gated. Without this, an adapter using native_status_mgmt
// would see the platform flip its workspace back to online whenever
// heartbeat error_rate dropped — even if the adapter's own state
// machine is currently reporting degraded for a non-error reason
// (paused, hibernating, awaiting upstream, etc.).
func TestHeartbeat_NativeStatusMgmt_SkipsRecovery(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	runtimeOverrides.SetCapabilities("ws-native-recovery", map[string]bool{"status_mgmt": true})
	defer runtimeOverrides.Reset()

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-native-recovery").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// heartbeat UPDATE — error_rate=0.05 would fire recovery
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-native-recovery", 0.05, "", 0, 7200, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus SELECT — currently degraded; recovery branch
	// would normally fire UPDATE → online + WORKSPACE_ONLINE broadcast.
	// Under native_status_mgmt, neither should run.
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-native-recovery").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("degraded"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-native-recovery","error_rate":0.05,"sample_error":"","active_tasks":0,"uptime_seconds":7200}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("recovery branch fired despite native_status_mgmt: %v", err)
	}
}

// TestHeartbeat_NativeStatusMgmt_WedgedStillRespected confirms the
// adapter's own self-reported wedge IS still honored even when
// native_status_mgmt is declared. The wedged path is the adapter's
// own signal, not platform inference — switching ownership doesn't
// silence it.
func TestHeartbeat_NativeStatusMgmt_WedgedStillRespected(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	runtimeOverrides.SetCapabilities("ws-wedged", map[string]bool{"status_mgmt": true})
	defer runtimeOverrides.Reset()

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// heartbeat UPDATE — RuntimeState="wedged" means sample_error
	// reflects the wedge reason, error_rate stays 0
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-wedged", 0.0, "SDK init timeout — restart workspace", 0, 7200, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus SELECT — currently online, wedged branch SHOULD fire
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id =").
		WithArgs("ws-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))

	// Wedged degrade UPDATE — must still happen even with native_status_mgmt
	mock.ExpectExec("UPDATE workspaces SET status = 'degraded'").
		WithArgs("ws-wedged").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// WORKSPACE_DEGRADED broadcast still fires
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-wedged","error_rate":0.0,"sample_error":"SDK init timeout — restart workspace","active_tasks":0,"uptime_seconds":7200,"runtime_state":"wedged"}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("wedged path didn't fire as expected: %v", err)
	}
}
