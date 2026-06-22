package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ==================== Register — input validation ====================

func TestRegister_BadJSON(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRegister_MissingRequiredFields(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// Missing url and agent_card
	body := `{"id":"ws-123"}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRegister_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// resolveDeliveryMode SELECT — no row yet, so default "push".
	// (#2339) New preflight after C18 token check; HasAnyLiveToken's COUNT
	// query has no mock here and fails-open per requireWorkspaceToken's
	// DB-error handling, so the next DB hit is this delivery_mode lookup.
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-fail").
		WillReturnError(sql.ErrNoRows)

	// DB insert fails
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("ws-fail", "ws-fail", "http://localhost:8000", `{"name":"test"}`, "push", "").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"id":"ws-fail","url":"http://localhost:8000","agent_card":{"name":"test"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestRegister_Non200_LogsStatusCode is a regression guard for #2563 / #2615:
// when boot Register returns a non-200 status, the response code must be logged
// so operators can distinguish 401/400/403/5xx register failures.
func TestRegister_Non200_LogsStatusCode(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	var buf bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldOutput)

	// Same DB-error setup as TestRegister_DBError; produces a 500 response.
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-fail").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("ws-fail", "ws-fail", "http://localhost:8000", `{"name":"test"}`, "push", "").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"id":"ws-fail","url":"http://localhost:8000","agent_card":{"name":"test"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	logs := buf.String()
	if !strings.Contains(logs, "boot_register_failed status=500") {
		t.Errorf("expected boot_register_failed log with status=500, got: %s", logs)
	}
	if !strings.Contains(logs, "workspace=ws-fail") {
		t.Errorf("expected boot_register_failed log with workspace ID, got: %s", logs)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestRegister_200_DoesNotLogFailure verifies that a successful boot Register
// does not emit the failure log line.
func TestRegister_200_DoesNotLogFailure(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	var buf bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldOutput)

	// Successful first-registration path (same mock setup as the C18 bootstrap test).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-ok").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-ok").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("ws-ok", "ws-ok", "http://localhost:9100", `{"name":"ok-agent"}`, "push", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs("ws-ok").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://localhost:9100"))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-ok").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WithArgs("ws-ok", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-ok","url":"http://localhost:9100","agent_card":{"name":"ok-agent"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if strings.Contains(buf.String(), "boot_register_failed") {
		t.Errorf("expected no boot_register_failed log on 200, got: %s", buf.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestRegister_FiresReconcile_OnProvisioningToOnline is the RFC#2843 #32
// regression for the SECOND root cause: on the CP/SaaS boot path the runtime
// calls POST /registry/register BEFORE it heartbeats, and register sets
// status='online'. So the heartbeat handler's prevStatus=='provisioning' fire
// (PR #3002/#3004) never matches — the row is already 'online' by the first
// heartbeat. The declared-plugin reconcile (seo-all) therefore never installed
// on a fresh prod seo-agent. This asserts register fires the reconcile when it
// performs the provisioning→online transition. The prev-status SELECT uses
// bare `status` (NOT-NULL enum; COALESCE(status,'') is rejected by Postgres).
func TestRegister_FiresReconcile_OnProvisioningToOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	reconcileFired := make(chan string, 4)
	handler.SetReconcileFunc(func(_ context.Context, workspaceID string) {
		reconcileFired <- workspaceID
	})

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-prov").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-prov").
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode", "runtime"}).AddRow("push", "claude-code"))
	// The reconcile-trigger prev-status read — only runs because ReconcileFunc
	// is wired. Returns 'provisioning' = the state before this register.
	mock.ExpectQuery("SELECT status FROM workspaces WHERE id").
		WithArgs("ws-prov").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("provisioning"))
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("ws-prov", "ws-prov", "http://localhost:9100", `{"name":"prov-agent"}`, "push", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs("ws-prov").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://localhost:9100"))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-prov").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WithArgs("ws-prov", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-prov","url":"http://localhost:9100","agent_card":{"name":"prov-agent"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// fire-and-forget via globalGoAsync; wait briefly.
	select {
	case got := <-reconcileFired:
		if got != "ws-prov" {
			t.Errorf("reconcile fired for wrong workspace: got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RFC#2843 #32 regression: reconcile did NOT fire on provisioning→online register")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Heartbeat — offline → online recovery ====================

func TestHeartbeatHandler_OfflineToOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-offline").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	// Expect heartbeat UPDATE
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-offline", 0.0, "", 1, 5000, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect evaluateStatus SELECT — currently offline
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-offline").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("offline", "", nil, nil))

	// Expect status transition back to online
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusOnline, "ws-offline").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect RecordAndBroadcast INSERT for WORKSPACE_ONLINE
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-offline","error_rate":0.0,"sample_error":"","active_tasks":1,"uptime_seconds":5000}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Heartbeat — provisioning → online recovery (#1784) ====================

func TestHeartbeatHandler_ProvisioningToOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// RFC#2843 #32 regression: the reconcile MUST fire when a fresh workspace's
	// heartbeat performs the provisioning→online self-heal. The runtime never
	// calls /registry/register on boot, so the heartbeat (whose UPDATE's inline
	// CASE flips provisioning→online before evaluateStatus runs) is the ONLY
	// fresh-boot transition. Pre-fix, fireReconcileOnline was only wired into
	// evaluateStatus's provisioning branch, which the inline CASE makes
	// unreachable — so declared plugins (e.g. seo-all) never installed.
	reconcileFired := make(chan string, 4)
	handler.SetReconcileFunc(func(_ context.Context, workspaceID string) {
		reconcileFired <- workspaceID
	})

	// prevTask + prevStatus SELECT — prevStatus='provisioning' is the state a
	// freshly-created workspace is in before its first heartbeat.
	//
	// The matcher pins `status` selected BARE (not COALESCE-wrapped): `status`
	// is a NOT-NULL workspace_status ENUM, and COALESCE(status, '') coerces ''
	// to the enum → Postgres `invalid input value for enum workspace_status: ""`
	// → the whole row scan fails → prevStatus stays "" → this reconcile trigger
	// NEVER fires (the live #32 regression). Requiring `, status FROM workspaces`
	// here makes a re-introduced COALESCE(status, ...) fail this unit test.
	mock.ExpectQuery("SELECT COALESCE\\(current_task, ''\\), COALESCE\\(monthly_spend, 0\\), status FROM workspaces").
		WithArgs("ws-provisioning").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "provisioning"))

	// Heartbeat UPDATE — its inline CASE flips provisioning→online.
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-provisioning", 0.0, "", 1, 3000, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus SELECT — reads the post-CASE status ('online'), so its own
	// provisioning→online branch does NOT fire (no duplicate transition exec).
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-provisioning").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-provisioning","error_rate":0.0,"sample_error":"","active_tasks":1,"uptime_seconds":3000}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// The reconcile fires fire-and-forget via globalGoAsync; wait briefly.
	select {
	case got := <-reconcileFired:
		if got != "ws-provisioning" {
			t.Errorf("reconcile fired for wrong workspace: got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RFC#2843 #32 regression: reconcile did NOT fire on provisioning→online heartbeat")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Heartbeat — failed → online recovery (#616 follow-up) ====================

// A workspace flipped to 'failed' by the provision-timeout sweeper must recover
// to 'online' once it starts heartbeating: a live heartbeat proves the agent
// booted (just slowly, past the 10m budget), so the timeout flip was premature.
func TestHeartbeatHandler_FailedToOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-failed").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-failed", 0.0, "", 1, 3000, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus SELECT — currently failed (provision-timeout sweeper flip)
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-failed").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("failed", "", nil, nil))

	// the new failed → online recovery transition
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusOnline, "ws-failed").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-failed","error_rate":0.0,"sample_error":"","active_tasks":1,"uptime_seconds":3000}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Heartbeat — awaiting_agent → online recovery ====================
// External workspaces flip to 'awaiting_agent' via healthsweep when their
// heartbeat goes stale. When the operator's poller comes back, heartbeat
// must lift the workspace out of awaiting_agent the same way it does for
// 'offline' and 'provisioning'. Without this branch, an external workspace
// stays OFFLINE in the canvas forever despite active heartbeats.

func TestHeartbeatHandler_AwaitingAgentToOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-external").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-external", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-external").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("awaiting_agent", "", nil, nil))

	// The new branch — UPDATE ... WHERE status = 'awaiting_agent'
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusOnline, "ws-external").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Broadcast WORKSPACE_ONLINE
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-external","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestHeartbeatHandler_BadJSON(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHeartbeatHandler_MissingWorkspaceID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"error_rate":0.1}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHeartbeatHandler_DBUpdateError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-dberr").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	// Heartbeat UPDATE fails
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-dberr", 0.1, "", 0, 100, "").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-dberr","error_rate":0.1,"sample_error":"","active_tasks":0,"uptime_seconds":100}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Heartbeat — stable (no transition) ====================

func TestHeartbeatHandler_OnlineStaysOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect prevTask SELECT
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-stable").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	// Expect heartbeat UPDATE
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-stable", 0.2, "", 3, 4000, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus: online with error_rate 0.2 — below 0.5 threshold, stays online
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-stable").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-stable","error_rate":0.2,"sample_error":"","active_tasks":3,"uptime_seconds":4000}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Heartbeat — runtime wedge (claude_agent_sdk init timeout) ====================

// TestHeartbeatHandler_RuntimeWedged_FlipsOnlineToDegraded verifies the
// runtime_state="wedged" path. Heartbeat task in the workspace lives in
// its own asyncio task and keeps reporting online while the Claude SDK
// is wedged on Control request timeout; the workspace tells us about
// the wedge via this field, and we honor it by flipping status →
// degraded with the wedge reason in last_sample_error.
func TestHeartbeatHandler_RuntimeWedged_FlipsOnlineToDegraded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	wedgeMsg := "claude_agent_sdk wedge: Control request timeout: initialize — restart workspace to recover"

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	// Heartbeat UPDATE — sample_error carries the wedge reason from the
	// workspace's _runtime_state_payload() helper.
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-wedged", 0.0, wedgeMsg, 0, 600, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus: currentStatus = online
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	// The wedge-handling branch fires the degraded UPDATE with the
	// `AND status = 'online'` guard (race-safe against concurrent
	// removal). Match the SQL with the guard included.
	mock.ExpectExec("UPDATE workspaces SET status =.*status = 'online'").
		WithArgs(models.StatusDegraded, "ws-wedged").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// RecordAndBroadcast for WORKSPACE_DEGRADED
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-wedged","error_rate":0.0,"sample_error":"` + wedgeMsg + `","active_tasks":0,"uptime_seconds":600,"runtime_state":"wedged"}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHeartbeatHandler_DegradedRecoversOnlyAfterWedgeClears verifies that
// the degraded → online recovery path requires BOTH error_rate < 0.1
// AND runtime_state cleared. A workspace still reporting wedged stays
// degraded even when error_rate happens to be 0 (no calls have been
// recorded as errors yet — the wedge is captured as a runtime state,
// not an error count).
func TestHeartbeatHandler_DegradedRecoversOnlyAfterWedgeClears(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-still-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-still-wedged", 0.0, "still broken", 0, 800, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// currentStatus = degraded
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-still-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("degraded", "", nil, nil))

	// No additional UPDATE expected — the recovery branch's
	// `runtime_state == ""` guard blocks the flip back to online.
	// (sqlmock fails the test if any unmocked Exec runs.)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-still-wedged","error_rate":0.0,"sample_error":"still broken","active_tasks":0,"uptime_seconds":800,"runtime_state":"wedged"}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHeartbeatHandler_DegradedToOnline_AfterWedgeClears verifies the
// happy-path recovery: a workspace previously marked degraded is
// post-restart, error_rate is back to 0, and runtime_state is empty
// (the new process re-imported claude_sdk_executor with the flag
// fresh). Status flips back to online and a WORKSPACE_ONLINE event
// fires.
func TestHeartbeatHandler_DegradedToOnline_AfterWedgeClears(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-recovered").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-recovered", 0.0, "", 0, 30, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-recovered").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("degraded", "", nil, nil))

	// Recovery UPDATE fires (degraded → online).
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusOnline, "ws-recovered").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// runtime_state intentionally absent (== ""); error_rate = 0; this
	// is exactly what a freshly-restarted workspace's first heartbeat
	// looks like.
	body := `{"workspace_id":"ws-recovered","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":30}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== UpdateCard ====================

func TestUpdateCard_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Expect UPDATE query
	mock.ExpectExec("UPDATE workspaces SET agent_card").
		WithArgs("ws-card", `{"name":"Updated Agent","skills":["coding"]}`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Expect RecordAndBroadcast INSERT for AGENT_CARD_UPDATED
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-card","agent_card":{"name":"Updated Agent","skills":["coding"]}}`
	c.Request = httptest.NewRequest("POST", "/registry/update-card", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateCard(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "updated" {
		t.Errorf("expected status 'updated', got %v", resp["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestUpdateCard_BadJSON(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	c.Request = httptest.NewRequest("POST", "/registry/update-card", bytes.NewBufferString("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateCard(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateCard_MissingFields(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// Missing agent_card
	body := `{"workspace_id":"ws-card"}`
	c.Request = httptest.NewRequest("POST", "/registry/update-card", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateCard(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateCard_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectExec("UPDATE workspaces SET agent_card").
		WithArgs("ws-card-err", `{"name":"fail"}`).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-card-err","agent_card":{"name":"fail"}}`
	c.Request = httptest.NewRequest("POST", "/registry/update-card", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateCard(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestUpdateCard_RejectsMetadataURL(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	setSSRFCheckForTest(true)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-card","agent_card":{"name":"evil","url":"http://169.254.169.254/latest/meta-data/"}}`
	c.Request = httptest.NewRequest("POST", "/registry/update-card", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateCard(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for metadata URL in agent_card, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateCard_RejectsNonHTTPScheme(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	setSSRFCheckForTest(true)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-card","agent_card":{"name":"evil","url":"file:///etc/passwd"}}`
	c.Request = httptest.NewRequest("POST", "/registry/update-card", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateCard(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for file:// scheme in agent_card, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRegister_GuardAgainstResurrectingRemovedRow verifies the #73 fix:
// the ON CONFLICT UPSERT must carry a `WHERE status IS DISTINCT FROM 'removed'`
// clause so that a late heartbeat from a workspace that was just deleted
// does not resurrect the row to 'online'.
//
// sqlmock matches on a substring of the rendered SQL — we assert the WHERE
// clause is present in the statement issued by Register().
func TestRegister_GuardAgainstResurrectingRemovedRow(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// resolveDeliveryMode preflight — no row yet, default push (#2339).
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-resurrect").
		WillReturnError(sql.ErrNoRows)
	// This regex-ish match requires the guard. If the handler ever drops
	// the clause the test fails because the emitted SQL won't match.
	mock.ExpectExec("ON CONFLICT.*WHERE workspaces.status IS DISTINCT FROM 'removed'").
		WithArgs("ws-resurrect", "ws-resurrect", "http://localhost:8000", `{"name":"x"}`, "push", "").
		WillReturnResult(sqlmock.NewResult(0, 0)) // 0 rows affected = correctly guarded
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs("ws-resurrect").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://127.0.0.1:54321"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-resurrect","url":"http://localhost:8000","agent_card":{"name":"x"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("#73 guard not present in UPSERT SQL: %v", err)
	}
}

// TestHeartbeat_SkipsRemovedRows verifies #73: heartbeat UPDATE carries
// `AND status != 'removed'` so a late heartbeat from a torn-down container
// doesn't refresh last_heartbeat_at on a tombstoned workspace.
func TestHeartbeat_SkipsRemovedRows(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// prevTask lookup
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-zombie").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	// UPDATE must include `AND status != 'removed'`. 0 rows affected is fine —
	// this is the tombstoned case the fix protects against.
	mock.ExpectExec("UPDATE workspaces SET.*WHERE id = .* AND status != 'removed'").
		WithArgs("ws-zombie", 0.0, "", 0, int64(0), "").
		WillReturnResult(sqlmock.NewResult(0, 0))

	// evaluateStatus SELECT
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id").
		WithArgs("ws-zombie").
		WillReturnError(sql.ErrNoRows) // row effectively removed from view

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat",
		bytes.NewBufferString(`{"workspace_id":"ws-zombie","error_rate":0,"sample_error":"","active_tasks":0,"uptime_seconds":0,"current_task":""}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("heartbeat handler must still return 200 even on tombstoned row, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("#73 guard not present in heartbeat UPDATE SQL: %v", err)
	}
}

// ==================== Heartbeat — agent_card backfill (#2421) ====================

func TestHeartbeatHandler_BackfillsAgentCard_WhenNull(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-nocard").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-nocard", 0.0, "", 0, 0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// #2421: backfill agent_card when heartbeat carries it and DB row is NULL
	mock.ExpectExec("UPDATE workspaces SET agent_card =").
		WithArgs("ws-nocard", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-nocard").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow(models.StatusOnline, "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-nocard","agent_card":{"name":"backfilled"}}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestHeartbeatHandler_SkipsAgentCardBackfill_WhenAlreadySet(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-hascard").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-hascard", 0.0, "", 0, 0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// #2421: backfill must be a no-op when agent_card already exists (0 rows affected)
	mock.ExpectExec("UPDATE workspaces SET agent_card =").
		WithArgs("ws-hascard", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-hascard").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow(models.StatusOnline, "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-hascard","agent_card":{"name":"ignored"}}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHeartbeatHandler_BackfillAgentCard_ClearsRegisterFailure verifies the
// #2659/#2665 recovery path: a healthy heartbeat that backfills a missing
// agent_card also clears last_register_failure_at, so a workspace that was
// previously forced degraded by a transient register failure can recover to
// online instead of staying stuck degraded forever.
func TestHeartbeatHandler_BackfillAgentCard_ClearsRegisterFailure(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-degraded-register-fail").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-degraded-register-fail", 0.0, "", 0, 0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// The heartbeat carries an agent_card and the row is NULL, so the backfill
	// UPDATE must ALSO clear last_register_failure_at.
	mock.ExpectExec("UPDATE workspaces SET agent_card =").
		WithArgs("ws-degraded-register-fail", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Status check sees degraded, but last_register_failure_at is now NULL because
	// the agent_card backfill UPDATE cleared it.
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-degraded-register-fail").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow(models.StatusDegraded, "", nil, nil))

	// Because the failure marker was cleared by the backfill, evaluateStatus
	// should now recover the workspace to online.
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusOnline, "ws-degraded-register-fail").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-degraded-register-fail","agent_card":{"name":"recovered"}}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ------------------------------------------------------------
// validateAgentURL (C6 SSRF fix)
// ------------------------------------------------------------

func TestValidateAgentURL(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		// ── Valid URLs (public hostnames / DNS names) ──────────────────────────
		// example.com (RFC-2606) resolves globally; agent.example.com
		// is NXDOMAIN on most resolvers and made this test flake.
		{"valid public https", "https://example.com:443", false},
		{"valid public http", "http://example.com:8000", false},
		// localhost by name is allowed — agents in local-dev use this form.
		{"valid localhost name", "http://localhost:8000", false},

		// ── Must be rejected: bad scheme ─────────────────────────────────────
		{"blocked scheme file", "file:///etc/passwd", true},
		{"blocked scheme ftp", "ftp://internal-server/secrets", true},
		{"blocked malformed url", "://not-a-url", true},
		{"blocked empty url", "", true},

		// ── Must be rejected: 169.254.0.0/16 — link-local / cloud metadata ───
		{"blocked link-local IMDS 169.254.169.254", "http://169.254.169.254/latest/meta-data/", true},
		{"blocked link-local GCP metadata", "http://169.254.169.254/computeMetadata/v1/", true},
		{"blocked link-local 169.254.0.1", "http://169.254.0.1/anything", true},

		// ── Must be rejected: 127.0.0.0/8 — loopback ─────────────────────────
		{"blocked loopback 127.0.0.1", "http://127.0.0.1:8080", true},
		{"blocked loopback 127.0.0.2", "http://127.0.0.2:8080", true},
		{"blocked loopback 127.255.255.255", "http://127.255.255.255:9000", true},

		// ── Must be rejected: 10.0.0.0/8 — RFC-1918 ──────────────────────────
		{"blocked RFC1918 10.0.0.1", "http://10.0.0.1:8080", true},
		{"blocked RFC1918 10.0.0.5", "http://10.0.0.5:8080", true},
		{"blocked RFC1918 10.255.255.254", "http://10.255.255.254:8080", true},

		// ── Must be rejected: 172.16.0.0/12 — RFC-1918 (includes Docker nets) ─
		{"blocked RFC1918 172.16.0.1 (range start)", "http://172.16.0.1:8080", true},
		{"blocked RFC1918 172.18.0.5 (docker bridge)", "http://172.18.0.5:8000", true},
		{"blocked RFC1918 172.31.255.255 (range end)", "http://172.31.255.255:8080", true},

		// ── Must be rejected: 192.168.0.0/16 — RFC-1918 ──────────────────────
		{"blocked RFC1918 192.168.0.1", "http://192.168.0.1:8080", true},
		{"blocked RFC1918 192.168.1.100", "http://192.168.1.100:8080", true},
		{"blocked RFC1918 192.168.255.254", "http://192.168.255.254:8080", true},

		// ── Must be rejected: IPv6 SSRF vectors (C6 gap) ─────────────────────
		// Go's IPv4 CIDRs do not match pure IPv6 addresses via Contains(), so
		// each IPv6 range needs an explicit blocklist entry.
		{"blocked IPv6 loopback [::1]", "http://[::1]:8080", true},
		{"blocked IPv6 link-local [fe80::1]", "http://[fe80::1]:8080", true},
		{"blocked IPv6 ULA [fd00::1]", "http://[fd00::1]:8080", true},

		// ── Must be rejected: RFC 5737 TEST-NET reserved ranges ─────────────
		// These addresses are reserved for documentation and example code.
		// No production agent has a legitimate reason to use them.
		{"blocked TEST-NET-1 192.0.2.x", "http://192.0.2.1:8080", true},
		{"blocked TEST-NET-1 192.0.2.254", "http://192.0.2.254:9000", true},
		{"blocked TEST-NET-2 198.51.100.x", "http://198.51.100.1:8080", true},
		{"blocked TEST-NET-2 198.51.100.99", "http://198.51.100.99:8000", true},
		{"blocked TEST-NET-3 203.0.113.x", "http://203.0.113.1:8080", true},
		{"blocked TEST-NET-3 203.0.113.254", "http://203.0.113.254:9000", true},

		// ── Must be rejected: RFC 3849 IPv6 documentation prefix ────────────
		{"blocked IPv6 documentation 2001:db8::1", "http://[2001:db8::1]:8080", true},
		{"blocked IPv6 documentation 2001:db8::ffff", "http://[2001:db8::ffff]:8000", true},

		// IPv4-mapped IPv6 for a blocked range must also be rejected.
		// Go normalises ::ffff:169.254.x.x to IPv4 via To4(), so the existing
		// 169.254.0.0/16 entry catches it without a dedicated rule.
		{"blocked IPv4-mapped IPv6 link-local", "http://[::ffff:169.254.169.254]:80", true},

		// ── F1083/#1130: DNS names resolved via net.LookupIP ──────────────────
		// localhost is allowed by name (intentional dev-environment special case;
		// the DNS resolution path skips the blocklist to preserve this behaviour).
		{"DNS name: localhost (allowed by name)", "http://localhost:9000", false},
		// github.com resolves to a public IP — must be allowed.
		// Skipped in sandboxed environments where external DNS is unavailable.
		// {"DNS name: github.com (public IP)", "https://github.com/", false},
		// A hostname that fails DNS resolution is blocked — the platform has
		// no use for a workspace it cannot reach; unresolvable hostnames are
		// either misconfigured or intentionally unreachable.
		{"DNS name: nxdomain (must fail)", "https://this-domain-definitely-does-not-exist-12345.invalid/", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentURL(tc.url)
			if tc.wantErr && err == nil {
				t.Errorf("validateAgentURL(%q) = nil, want error", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateAgentURL(%q) = %v, want nil", tc.url, err)
			}
		})
	}
}

// TestValidateAgentURL_SaaSMode_AllowsRFC1918 is the integration-level wrapper test
// for the SaaS-mode SSRF relaxation in validateAgentURL (used at registration).
// It exercises validateAgentURL as called by the Register handler, not just the
// inner blockedRanges slice.  Regression guard for the same class of bug as
// isSafeURL (issue #1785).
func TestValidateAgentURL_SaaSMode_AllowsRFC1918(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")
	t.Setenv("MOLECULE_ORG_ID", "")
	for _, url := range []string{
		"http://10.1.2.3/agent",
		"http://10.0.0.5:8000/a2a",
		"http://172.16.0.1/agent",
		"http://172.18.0.42:8000/a2a",
		"http://172.31.44.78/agent",
		"http://192.168.1.100/agent",
		"http://192.168.255.254:9000/a2a",
		"http://[fd00::1]/agent",
		"http://[fd12:3456:789a::42]/a2a",
	} {
		if err := validateAgentURL(url); err != nil {
			t.Errorf("validateAgentURL(%q) in saasMode: got %v, want nil", url, err)
		}
	}
}

// TestValidateAgentURL_PendingPlatformTunnel (#36/#2421): a freshly-provisioned
// cross-cloud workspace advertises its per-workspace tunnel hostname
// (ws-<id>.<appDomain>) whose DNS has not propagated yet when a FAST box (Hetzner
// ~1s boot) registers. validateAgentURL must allow such a platform-tunnel
// hostname through in SaaS mode instead of 400 (which the runtime never retries
// → agent_card never lands). Non-platform unresolvable hostnames stay blocked.
func TestValidateAgentURL_PendingPlatformTunnel(t *testing.T) {
	for _, tc := range []struct {
		h    string
		want bool
	}{
		{"ws-abc123.moleculesai.app", true},
		{"ws-abc123.staging.moleculesai.app", true},
		{"WS-ABC123.MOLECULESAI.APP", true},          // case-insensitive DNS
		{"ws-abc123.moleculesai.app.", true},         // FQDN trailing dot
		{"WS-ABC123.STAGING.MOLECULESAI.APP.", true}, // both case + trailing dot
		{"ws-abc123.evil.com", false},                // not under the platform domain
		{"api.moleculesai.app", false},               // no ws- prefix
		{"ws-x.fakemoleculesai.app", false},          // lookalike domain, not a subdomain
		{"ws-abc123moleculesai.app", false},          // missing dot before platform domain
		{"ws-x.moleculesai.app.attacker.com", false}, // parent-domain trick
	} {
		if got := isPlatformTunnelHostname(tc.h); got != tc.want {
			t.Errorf("isPlatformTunnelHostname(%q)=%v want %v", tc.h, got, tc.want)
		}
	}
	t.Setenv("MOLECULE_ORG_ID", "")
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")
	// A platform tunnel hostname is allowed — whether or not its DNS has
	// propagated (a resolved record is a public Cloudflare IP = allowed; an
	// unresolved one is allowed by the pending-tunnel branch).
	if err := validateAgentURL("https://ws-deadbeef0001.staging.moleculesai.app/a2a"); err != nil {
		t.Errorf("SaaS: pending platform tunnel must be allowed, got %v", err)
	}
	// A NON-platform unresolvable hostname stays blocked even in SaaS
	// (.invalid never resolves — RFC 2606).
	if err := validateAgentURL("https://ws-x.attacker.invalid/a2a"); err == nil {
		t.Error("SaaS: non-platform unresolvable hostname must stay blocked")
	}
}

// TestValidateAgentURL_SaaSMode_StillBlocksMetadataEtAl verifies that even in
// SaaS mode the always-blocked ranges (metadata, loopback, TEST-NET, CGNAT,
// non-fd00 ULA) stay blocked.
func TestValidateAgentURL_SaaSMode_StillBlocksMetadataEtAl(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")
	t.Setenv("MOLECULE_ORG_ID", "")
	for _, url := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://169.254.0.1/",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
		"http://192.0.2.5/agent",
		"http://198.51.100.5/a2a",
		"http://203.0.113.42/agent",
		"http://100.64.0.1/agent",
		"http://100.127.255.254:8000/a2a",
		"http://[fc00::1]/agent",
		"http://224.0.0.1/",
	} {
		if err := validateAgentURL(url); err == nil {
			t.Errorf("validateAgentURL(%q) in saasMode: got nil, want block", url)
		}
	}
}

// TestValidateAgentURL_StrictMode_BlocksRFC1918 is the strict-mode counterpart
// to TestValidateAgentURL_SaaSMode_AllowsRFC1918.
func TestValidateAgentURL_StrictMode_BlocksRFC1918(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	t.Setenv("MOLECULE_ORG_ID", "")
	for _, url := range []string{
		"http://10.1.2.3/agent",
		"http://172.16.0.1:8000/a2a",
		"http://172.31.44.78/agent",
		"http://192.168.1.100/agent",
		"http://[fd00::1]/agent",
	} {
		if err := validateAgentURL(url); err == nil {
			t.Errorf("validateAgentURL(%q) in strict mode: got nil, want block", url)
		}
	}
}

// TestValidateAgentURL_SaaSMode_LegacyOrgID covers the legacy MOLECULE_ORG_ID
// signal (no MOLECULE_DEPLOY_MODE set) for validateAgentURL.
func TestValidateAgentURL_SaaSMode_LegacyOrgID(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "")
	t.Setenv("MOLECULE_ORG_ID", "7b2179dc-8cc6-4581-a3c6-c8bff4481086")
	for _, url := range []string{
		"http://10.1.2.3/agent",
		"http://172.18.0.42:8000/a2a",
		"http://192.168.1.100/agent",
		"http://[fd00::1]/agent",
	} {
		if err := validateAgentURL(url); err != nil {
			t.Errorf("validateAgentURL(%q) with legacy MOLECULE_ORG_ID: got %v, want nil", url, err)
		}
	}
}

// ==================== C18 — Register ownership ====================

// TestRegister_C18_BootstrapAllowedNoTokens verifies that a workspace with NO
// live tokens (i.e. first-ever registration) is allowed through without a bearer
// token. This is the bootstrap path — the token is issued at the end of Register.
func TestRegister_C18_BootstrapAllowedNoTokens(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// requireWorkspaceToken → HasAnyLiveToken → COUNT(*) returns 0 (no tokens).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-new").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// resolveDeliveryMode — no row yet, default push (#2339).
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-new").
		WillReturnError(sql.ErrNoRows)

	// Workspace upsert proceeds normally.
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("ws-new", "ws-new", "http://localhost:9100", `{"name":"new-agent"}`, "push", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs("ws-new").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://localhost:9100"))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// HasAnyLiveToken check for token issuance at end of Register.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-new").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// IssueToken INSERT.
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WithArgs("ws-new", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-new","url":"http://localhost:9100","agent_card":{"name":"new-agent"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Errorf("C18 bootstrap: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Token should be present in response (first registration).
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["auth_token"] == nil {
		t.Errorf("C18 bootstrap: expected auth_token in first-registration response, got %v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("C18 bootstrap: unmet expectations: %v", err)
	}
}

// TestRegister_ReturnsPlatformInboundSecret_RFC2312_PRF verifies that
// /registry/register includes the workspace's platform_inbound_secret
// in the response body when one is on file. This is the SaaS delivery
// path: SaaS workspaces have no persistent /configs volume, so they
// re-fetch the secret on every register call (idempotent in Docker mode
// where the provisioner already wrote the same value to the volume at
// workspace creation).
func TestRegister_ReturnsPlatformInboundSecret_RFC2312_PRF(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "00000000-0000-0000-0000-000000002312"
	const inboundSecret = "the-platform-inbound-secret-value"

	// requireWorkspaceToken — bootstrap allowed (no live tokens).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// resolveDeliveryMode — no row yet, default push (#2339).
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnError(sql.ErrNoRows)

	// Workspace upsert.
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://localhost:9100"))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Phase 30.1 token issuance — first-register path.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(1, 1))

	// RFC #2312 PR-F: ReadPlatformInboundSecret query — returns the value
	// the provisioner stored at workspace creation. The handler MUST
	// include this in the response body so the workspace can persist it
	// to /configs/.platform_inbound_secret.
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(inboundSecret))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","url":"http://localhost:9100","agent_card":{"name":"x"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	got, ok := resp["platform_inbound_secret"].(string)
	if !ok {
		t.Fatalf("expected platform_inbound_secret in response, got: %v", resp)
	}
	if got != inboundSecret {
		t.Errorf("secret mismatch: got %q, want %q", got, inboundSecret)
	}
	// auth_token should also be present (first-register path).
	if resp["auth_token"] == nil {
		t.Error("expected auth_token in response (first-register path)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_NoInboundSecret_OmitsField verifies that legacy workspaces
// that predate migration 044 (NULL platform_inbound_secret column) still
// get a successful registration — the field is just omitted from the
// response. The Register handler logs the absence quietly.
// TestRegister_NoInboundSecret_LazyHeals — legacy workspace path:
// when ReadPlatformInboundSecret returns ErrNoInboundSecret (NULL
// column), Register MUST mint inline and include the freshly-minted
// secret in the response. Without this, legacy workspaces would need
// two round-trips before chat upload works (chat_files heals
// platform-side → workspace must heartbeat → next chat upload).
//
// Pre-fix this test asserted the field was ABSENT; that was correct
// for the missing behavior, but happened to pass even with my
// register-time lazy-heal change because sqlmock unmatched UPDATE
// caused the mint to fail and fall back to omit-field. Splitting
// into success + failure tests pins both branches.
func TestRegister_NoInboundSecret_LazyHeals(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "00000000-0000-0000-0000-000000002312"

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO workspaces").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://localhost:9100"))
	mock.ExpectExec("INSERT INTO structure_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").WillReturnResult(sqlmock.NewResult(1, 1))
	// NULL secret — legacy workspace.
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))
	// Lazy-heal mint MUST land. If this expectation isn't matched, the
	// register handler skipped backfill and legacy workspaces would
	// need 2 round-trips before chat upload works.
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","url":"http://localhost:9100","agent_card":{"name":"x"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	secret, present := resp["platform_inbound_secret"]
	if !present {
		t.Errorf("expected platform_inbound_secret to be PRESENT (lazy-healed), got response: %v", resp)
	}
	if s, ok := secret.(string); !ok || s == "" {
		t.Errorf("expected non-empty platform_inbound_secret string, got %T %v", secret, secret)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met — register-time lazy-heal mint did NOT run, regression of #2312 backfill: %v", err)
	}
}

// TestRegister_NoInboundSecret_LazyHealMintFailureOmitsField pins the
// alternate branch: if the lazy-heal mint itself fails (DB hiccup),
// Register MUST still respond 200 (workspace is online) but omit the
// field. The next register call will retry the heal.
func TestRegister_NoInboundSecret_LazyHealMintFailureOmitsField(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "00000000-0000-0000-0000-000000002313"

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO workspaces").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://localhost:9100"))
	mock.ExpectExec("INSERT INTO structure_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))
	// Mint fails — handler must NOT 500; just omit field + log.
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), wsID).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","url":"http://localhost:9100","agent_card":{"name":"x"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 even when lazy-heal fails (workspace is online), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if _, present := resp["platform_inbound_secret"]; present {
		t.Errorf("expected platform_inbound_secret to be ABSENT when mint failed, got: %v", resp["platform_inbound_secret"])
	}
}

// TestRegister_C18_HijackBlockedNoBearer verifies the C18 attack is blocked:
// when a workspace already has a live token, /register without a bearer → 401.
func TestRegister_C18_HijackBlockedNoBearer(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// HasAnyLiveToken returns 1 — workspace already has an active token.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-victim").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// No Authorization header — simulates attacker with no credentials.
	// URL uses example.com (resolves globally) so the validateAgentURL
	// pre-check doesn't short-circuit with 400 "invalid request body"
	// before the C18 auth check fires. We're testing that C18 gates
	// produce 401, not that URL validation produces 400.
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-victim","url":"http://example.com:9999/steal","agent_card":{"name":"hijacked"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("C18 hijack: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	// The malicious URL must NOT have been persisted — no INSERT expectation was set.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("C18 hijack: unmet expectations: %v", err)
	}
}

// ==================== Issue #435 — DB error must not leak raw message ====================

// TestRegister_DBErrorResponseIsOpaque verifies that when the DB upsert fails,
// the HTTP response body contains only the generic "registration failed" message
// and never the raw Go/PostgreSQL error string (issue #435).
func TestRegister_DBErrorResponseIsOpaque(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// C18 pre-check — no live tokens (bootstrap path).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-errtest").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// resolveDeliveryMode — no row yet, default push (#2339).
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-errtest").
		WillReturnError(sql.ErrNoRows)

	// DB upsert fails with a descriptive internal error.
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("ws-errtest", "ws-errtest", "http://localhost:9200", `{"name":"err-agent"}`, "push", "").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-errtest","url":"http://localhost:9200","agent_card":{"name":"err-agent"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v — body: %s", err, w.Body.String())
	}

	errMsg, ok := resp["error"].(string)
	if !ok {
		t.Fatalf("expected string 'error' field, got %T: %v", resp["error"], resp["error"])
	}
	if errMsg != "registration failed" {
		t.Errorf("expected opaque 'registration failed', got %q (raw error leaked)", errMsg)
	}
	// Confirm the raw driver error string is absent.
	rawBody := w.Body.String()
	if strings.Contains(rawBody, "sql:") || strings.Contains(rawBody, "pq:") || strings.Contains(rawBody, "connection") {
		t.Errorf("raw DB error leaked into response body: %s", rawBody)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== #615 — monthly_spend clamping ====================

// TestHeartbeat_MonthlySpend_WithinBounds verifies that a valid positive
// monthly_spend is written to the DB unchanged (no clamping needed).
func TestHeartbeat_MonthlySpend_WithinBounds(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-spend-ok").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	// Expect the 7-argument UPDATE (with monthly_spend = $7).
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-spend-ok", 0.0, "", 0, 0, "", int64(15000)). // $150.00
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id").
		WithArgs("ws-spend-ok").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-spend-ok","monthly_spend":15000}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHeartbeat_MonthlySpend_NegativeClamped verifies that a negative
// monthly_spend value (invalid) is clamped to 0 before the DB write,
// which means the no-spend UPDATE path is taken (zero is "no update"). (#615)
func TestHeartbeat_MonthlySpend_NegativeClamped(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-spend-neg").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	// Clamped to 0 → no monthly_spend field → 6-argument UPDATE.
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-spend-neg", 0.0, "", 0, 0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id").
		WithArgs("ws-spend-neg").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-spend-neg","monthly_spend":-9999}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("negative monthly_spend must be clamped to 0 (no-spend UPDATE path): %v", err)
	}
}

// TestHeartbeat_MonthlySpend_OverflowClamped verifies that an astronomically
// large monthly_spend is clamped to maxMonthlySpend ($10B in cents) rather
// than written raw to the DB, preventing NUMERIC overflow. (#615)
func TestHeartbeat_MonthlySpend_OverflowClamped(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-spend-overflow").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	// Expect the 7-argument UPDATE with monthly_spend clamped to 1_000_000_000_000.
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-spend-overflow", 0.0, "", 0, 0, "", int64(1_000_000_000_000)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id").
		WithArgs("ws-spend-overflow").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// Simulate a misbehaving agent reporting math.MaxInt64.
	body := `{"workspace_id":"ws-spend-overflow","monthly_spend":9223372036854775807}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("math.MaxInt64 monthly_spend must be clamped to maxMonthlySpend: %v", err)
	}
}

// TestHeartbeat_MonthlySpend_ExactCap verifies the boundary: a value exactly
// equal to maxMonthlySpend ($10B) passes through without modification.
func TestHeartbeat_MonthlySpend_ExactCap(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-spend-cap").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-spend-cap", 0.0, "", 0, 0, "", int64(1_000_000_000_000)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id").
		WithArgs("ws-spend-cap").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-spend-cap","monthly_spend":1000000000000}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("exact-cap monthly_spend should pass through unmodified: %v", err)
	}
}

// TestHeartbeat_MonthlySpend_Zero_NoUpdate verifies that monthly_spend=0 (or
// omitted) does NOT write monthly_spend to the DB — zero means "no update",
// never write zero to avoid clearing a previously-reported spend value.
func TestHeartbeat_MonthlySpend_Zero_NoUpdate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-spend-zero").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	// 6-argument UPDATE — monthly_spend NOT included.
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-spend-zero", 0.0, "", 0, 0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id").
		WithArgs("ws-spend-zero").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// Explicitly set monthly_spend = 0.
	body := `{"workspace_id":"ws-spend-zero","monthly_spend":0}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("monthly_spend=0 must not trigger a DB write for spend: %v", err)
	}
}

// ==================== Register — delivery_mode (#2339) ====================

// TestRegister_PollMode_AcceptsEmptyURL verifies the new contract:
// when delivery_mode=poll, URL is optional. A poll-mode workspace
// (e.g. operator's laptop running molecule-mcp-claude-channel) has
// no public URL to register, and we must NOT reject the registration
// for that. The proxy short-circuits poll-mode A2A in PR 2 — no URL
// needed there either.
func TestRegister_PollMode_AcceptsEmptyURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "ws-poll-no-url"

	// Bootstrap path — no live tokens, so requireWorkspaceToken passes
	// without an Authorization header.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// resolveDeliveryMode: payload sets "poll" explicitly, so we should
	// NOT hit the DB lookup at all (the helper short-circuits when
	// payload value is non-empty). Asserted by the absence of an
	// ExpectQuery for SELECT delivery_mode here.

	// Upsert MUST run with empty URL (sql.NullString) and delivery_mode=poll.
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(wsID, wsID, sql.NullString{}, `{"name":"poll-agent"}`, "poll", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// SELECT url for cache: returns NULL/empty for poll-mode rows. The
	// handler skips the cache writes in that case (no CacheURL /
	// CacheInternalURL expectations).
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow(""))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Token issuance — first-register path.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","delivery_mode":"poll","agent_card":{"name":"poll-agent"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("poll-mode + empty URL: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp["delivery_mode"] != "poll" {
		t.Errorf("response.delivery_mode = %v, want %q", resp["delivery_mode"], "poll")
	}
	// First-register must still mint a token regardless of delivery_mode.
	if resp["auth_token"] == nil {
		t.Error("expected auth_token in response (first-register path)")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_PushMode_RejectsEmptyURL verifies the symmetric contract:
// push-mode (the default) still requires a URL. Skipping URL validation
// in poll-mode mustn't accidentally relax the push-mode invariant — that
// would silently break dispatch for the rest of the fleet.
func TestRegister_PushMode_RejectsEmptyURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Bootstrap path through requireWorkspaceToken.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-push-no-url").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// resolveDeliveryMode: no row yet, defaults to push. The handler
	// then validates the URL — which is empty — and returns 400.
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-push-no-url").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-push-no-url","agent_card":{"name":"push-agent"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("push-mode + empty URL: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "url is required") {
		t.Errorf("expected 'url is required' in error body, got: %s", w.Body.String())
	}
}

// TestRegister_InvalidDeliveryMode rejects payloads that declare an
// unrecognised delivery_mode — defends against a typo silently
// becoming "push" and leaving the operator wondering why polling
// doesn't work.
func TestRegister_InvalidDeliveryMode(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-x","url":"http://localhost:8000","agent_card":{"name":"a"},"delivery_mode":"webhook"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid delivery_mode: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "delivery_mode") {
		t.Errorf("expected error body to mention delivery_mode, got: %s", w.Body.String())
	}
}

// TestRegister_InvalidKind rejects payloads that declare an unrecognised kind —
// only 'workspace' and 'platform' are valid. Mirrors the delivery_mode guard;
// the rejection happens before any DB access.
func TestRegister_InvalidKind(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"ws-x","url":"http://localhost:8000","agent_card":{"name":"a"},"kind":"bogus"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid kind: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "kind") {
		t.Errorf("expected error body to mention kind, got: %s", w.Body.String())
	}
}

// TestRegister_AllowsAlreadyPlatformReRegister verifies that a workspace whose
// row is ALREADY kind="platform" (pre-seeded by the AdminAuth/boot-gated install
// path) may re-register through the public /registry/register path with
// kind="platform", and the value is preserved through the upsert. This is the
// legitimate platform-agent boot flow.
func TestRegister_AllowsAlreadyPlatformReRegister(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "ws-platform-agent"

	// Bootstrap path — no live tokens.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// SECURITY precheck: the row is already kind="platform", so the re-register
	// is allowed to proceed.
	mock.ExpectQuery("SELECT kind FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))

	// Issue #2970: platform-agent identity gate — payload.kind="platform", so we
	// verify the seeded MODEL workspace_secret exists before marking online.
	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM workspace_secrets WHERE workspace_id = \\$1 AND key = 'MODEL'\\)").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// delivery_mode="push" is set explicitly, so resolveDeliveryMode
	// short-circuits (no SELECT delivery_mode lookup). The upsert MUST carry
	// kind="platform" as the 6th arg.
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(wsID, wsID, "http://localhost:9100", `{"name":"concierge"}`, "push", "platform").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://localhost:9100"))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Token issuance — first-register path.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","url":"http://localhost:9100","delivery_mode":"push","kind":"platform","mcp_server_present":true,"agent_card":{"name":"concierge"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("already-platform re-register: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_RejectsFreshPlatformKind locks the privilege-escalation fix: the
// public /registry/register path must NOT let a brand-new (fresh-id) workspace
// declare kind="platform" and mint itself a second org root. It must be refused
// (403) before any upsert — only the AdminAuth/boot-gated install paths may mint
// the platform agent.
func TestRegister_RejectsFreshPlatformKind(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	const wsID = "ws-rogue-fresh"

	// Bootstrap path — no live tokens (a fresh id).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// SECURITY precheck: no existing row → empty result → sql.ErrNoRows → refuse.
	// No upsert / token issuance must follow.
	mock.ExpectQuery("SELECT kind FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"kind"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","url":"http://localhost:9100","delivery_mode":"push","kind":"platform","agent_card":{"name":"rogue"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("fresh kind=platform register: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_RejectsPlatformPromotion locks the other half of the fix: a row
// that already exists as kind="workspace" must NOT be promotable to "platform"
// via the public register path (which would later get it provisioned with the
// org-admin token). Refused (403) before the upsert.
func TestRegister_RejectsPlatformPromotion(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	const wsID = "ws-ordinary"

	// Has no live tokens for test simplicity (bootstrap-allowed call).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// SECURITY precheck: existing row is kind="workspace" → refuse promotion.
	mock.ExpectQuery("SELECT kind FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","url":"http://localhost:9100","delivery_mode":"push","kind":"platform","agent_card":{"name":"rogue"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("promote workspace->platform: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_PlatformAgentMissingModelSecret_FailsClosed guards issue #2970:
// a platform agent that reaches /registry/register without a seeded MODEL
// workspace_secret must NOT be marked online. Instead the workspace is marked
// 'failed' and the register call returns 400, so a generic/model-less concierge
// cannot serve users.
func TestRegister_PlatformAgentMissingModelSecret_FailsClosed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "ws-platform-no-model"

	// Bootstrap path — no live tokens.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// kind precheck: existing row is kind="platform".
	mock.ExpectQuery("SELECT kind FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))

	// Identity gate: payload.kind="platform" → check MODEL secret → absent.
	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM workspace_secrets WHERE workspace_id = \\$1 AND key = 'MODEL'\\)").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// Gate failure broadcasts WORKSPACE_PROVISION_FAILED and marks the row failed.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET status = \\$3, last_sample_error = \\$2, updated_at = now\\(\\) WHERE id = \\$1").
		WithArgs(wsID, sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","url":"http://localhost:9100","delivery_mode":"push","kind":"platform","mcp_server_present":true,"agent_card":{"name":"concierge"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("platform agent missing MODEL secret: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_PollMode_PreservesExistingValue: when the row already
// has delivery_mode=poll and the payload doesn't set it, the resolved
// mode should be poll — i.e. "absent payload mode" must NOT silently
// downgrade an existing poll workspace to push. Ensures Telegram-style
// stability: mode is sticky once set.
func TestRegister_PollMode_PreservesExistingValue(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "ws-existing-poll"

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// resolveDeliveryMode: row exists with delivery_mode=poll.
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode", "runtime"}).AddRow("poll", "claude-code"))

	// Upsert carries the resolved poll mode forward — even though
	// payload didn't restate it. URL still empty (poll-mode shape).
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(wsID, wsID, sql.NullString{}, `{"name":"a"}`, "poll", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow(""))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// No delivery_mode in payload — must inherit "poll" from the row.
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","agent_card":{"name":"a"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["delivery_mode"] != "poll" {
		t.Errorf("delivery_mode = %v, want %q (must inherit existing row's mode when payload absent)",
			resp["delivery_mode"], "poll")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_ExternalRuntime_DefaultsToPoll covers the 2026-04-30
// flip: a workspace with runtime='external' and an empty
// delivery_mode (existing or payload) defaults to poll instead of
// push. Rationale: external workspaces are operator-driven (laptops,
// no public HTTPS) — push-mode would hard-fail at register time
// because validateAgentURL rejects RFC1918 / loopback. The CLI
// (`molecule connect`) registers without --mode and expects this
// default to land it in poll-mode.
func TestRegister_ExternalRuntime_DefaultsToPoll(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "ws-external-default-poll"

	// requireWorkspaceToken: no live tokens yet (first register).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// resolveDeliveryMode: row exists with empty delivery_mode + runtime=external.
	// Branch under test: delivery_mode is empty → fall through to runtime
	// check → return poll.
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode", "runtime"}).
			AddRow(sql.NullString{}, "external"))

	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(wsID, wsID, sql.NullString{}, `{"name":"a"}`, "poll", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow(""))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","agent_card":{"name":"a"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["delivery_mode"] != "poll" {
		t.Errorf("delivery_mode = %v, want %q (external runtime + empty mode → poll)",
			resp["delivery_mode"], "poll")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_KimiRuntime_DefaultsToPoll mirrors the external-runtime
// poll-default test: a workspace whose existing row has runtime=kimi-cli
// and empty delivery_mode must resolve to poll (laptop/NAT-safe default).
func TestRegister_KimiRuntime_DefaultsToPoll(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "ws-kimi-default-poll"

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode", "runtime"}).
			AddRow(sql.NullString{}, "kimi-cli"))

	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(wsID, wsID, sql.NullString{}, `{"name":"a"}`, "poll", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow(""))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","agent_card":{"name":"a"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["delivery_mode"] != "poll" {
		t.Errorf("delivery_mode = %v, want %q (kimi runtime + empty mode → poll)",
			resp["delivery_mode"], "poll")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_NonExternalRuntime_StillDefaultsToPush guards the
// inverse: a non-external runtime (claude-code, hermes, etc.) with
// empty delivery_mode keeps the historical push default. Catches
// any future "all empty modes default to poll" overshoot.
func TestRegister_NonExternalRuntime_StillDefaultsToPush(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "ws-claude-code-default-push"

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode", "runtime"}).
			AddRow(sql.NullString{}, "claude-code"))

	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs(wsID, wsID, "http://localhost:8000", `{"name":"a"}`, "push", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT url FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://localhost:8000"))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","url":"http://localhost:8000","agent_card":{"name":"a"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["delivery_mode"] != "push" {
		t.Errorf("delivery_mode = %v, want %q (non-external runtime keeps push default)",
			resp["delivery_mode"], "push")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ==================== Heartbeat — platform_inbound_secret delivery (2026-04-30) ====================
// Heartbeat must echo the workspace's platform_inbound_secret on every
// beat, mirroring /registry/register. Without this delivery path, a
// workspace whose secret was lazy-healed on the platform side (e.g. via
// chat_files Upload's "secret was just minted, retry in 30s" branch)
// could only pick up the freshly-minted value via a runtime restart —
// the chat_files retry would 401-forever. Caught 2026-04-30 on the
// hongmingwang tenant: 503 → 401 chain on chat upload.

func TestHeartbeatHandler_DeliversPlatformInboundSecret(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const inboundSecret = "the-already-minted-secret"

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-with-secret").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-with-secret", 0.0, "", 0, 100, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-with-secret").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	// readOrLazyHealInboundSecret — short-circuit: secret already on file.
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs("ws-with-secret").
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(inboundSecret))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-with-secret","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":100}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	got, ok := resp["platform_inbound_secret"].(string)
	if !ok {
		t.Fatalf("expected platform_inbound_secret in heartbeat response, got: %v", resp)
	}
	if got != inboundSecret {
		t.Errorf("secret mismatch: got %q, want %q", got, inboundSecret)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestHeartbeatHandler_LazyHealsPlatformInboundSecret pins the
// recovery branch: a workspace with a NULL platform_inbound_secret
// (legacy / partially-bootstrapped row) gets the column minted inline
// AND receives the freshly-minted value in the response, so the next
// chat-upload tick makes the workspace work without a restart.
func TestHeartbeatHandler_LazyHealsPlatformInboundSecret(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-needs-heal").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-needs-heal", 0.0, "", 0, 100, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-needs-heal").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	// readOrLazyHealInboundSecret — NULL column triggers mint.
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs("ws-needs-heal").
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))
	// Inline mint UPDATE — must land or legacy workspaces stay 401-forever.
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), "ws-needs-heal").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-needs-heal","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":100}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	secret, present := resp["platform_inbound_secret"]
	if !present {
		t.Fatalf("expected platform_inbound_secret PRESENT after lazy-heal, got: %v", resp)
	}
	if s, ok := secret.(string); !ok || s == "" {
		t.Errorf("expected non-empty string secret, got %T %v", secret, secret)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations — heartbeat-time lazy-heal mint did NOT run: %v", err)
	}
}

// TestHeartbeatHandler_OmitsSecretOnHealFailure pins the defensive
// branch: when both the read AND the mint fail, heartbeat MUST still
// respond 200 (liveness is the primary contract) but omit the field.
// The next tick retries.
func TestHeartbeatHandler_OmitsSecretOnHealFailure(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-heal-fails").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-heal-fails", 0.0, "", 0, 100, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-heal-fails").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("online", "", nil, nil))

	// Read returns NULL → mint is attempted...
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs("ws-heal-fails").
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))
	// ...but the mint UPDATE fails (DB hiccup).
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), "ws-heal-fails").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-heal-fails","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":100}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	// Liveness contract — heartbeat MUST stay 200 even when the
	// secret-delivery side-channel fails. chat_files retries lazy-heal
	// on the next request anyway.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (liveness primary), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if _, present := resp["platform_inbound_secret"]; present {
		t.Errorf("expected platform_inbound_secret OMITTED on heal failure, got: %v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_FailureRecordsLastRegisterFailure (#2530 / #2585): an
// AUTHENTICATED non-200 register must stamp last_register_failure_at so
// heartbeat can surface degraded status. Unauthenticated 401s must NOT stamp.
func TestRegister_FailureRecordsLastRegisterFailure(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Bootstrap-allowed: no live tokens → requireWorkspaceToken returns nil,
	// so authOK becomes true.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-reg-fail").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// Authenticated post-auth failure: push-mode with no URL 400s at the
	// url-required check (AFTER requireWorkspaceToken sets authOK=true), so the
	// deferred handler stamps last_register_failure_at. (An invalid delivery_mode
	// would 400 BEFORE auth — authOK=false — and must NOT stamp.)
	mock.ExpectExec("UPDATE workspaces SET last_register_failure_at = now").
		WithArgs("ws-reg-fail").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"id":"ws-reg-fail","agent_card":{"name":"test"},"delivery_mode":"push"}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_Unauthenticated401DoesNotStamp (#2585): a 401 from missing or
// invalid bearer must NOT stamp last_register_failure_at, otherwise an
// unauthenticated caller could force any workspace into degraded status.
func TestRegister_Unauthenticated401DoesNotStamp(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// HasAnyLiveToken returns 1 — workspace already has an active token.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-reg-unauth").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"id":"ws-reg-unauth","url":"http://example.com","agent_card":{"name":"test"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
	// No UPDATE expectation — unauthenticated 401 must not mutate workspace state.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_BootstrapRecovery_StaleBearerZeroLiveTokens (#2611): the
// watchdog double-provision race produces a "loser" box that presents a
// bearer that was just revoked by the winner's mint. RequireWorkspaceToken
// must re-check HasAnyLiveToken after ValidateToken rejects the bearer:
// if the workspace now has zero live tokens (the previously-valid token
// was revoked in the gap), the request is allowed through as a fresh
// bootstrap. The agent's next register call mints a new token.
//
// Without the re-check, the loser box gets a permanent 401 — no live
// token to present, the workspace is dead, and the runtime's
// 401-as-terminal posture wedges the workspace "online-but-braindead".
func TestRegister_BootstrapRecovery_StaleBearerZeroLiveTokens(t *testing.T) {
	// SaaS mode so the platform-tunnel hostname is allowed.
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Sequence mirrors Register success path, with the core#2611 recovery
	// moment between queries #1 and #1.5.
	//
	// 1. requireWorkspaceToken → first HasAnyLiveToken: count=1 (live token
	//    exists at the moment of the check — the race hasn't fired yet).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-reg-bootstrap-recovery").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	// 1.5. ValidateToken → lookupTokenByHash: ErrNoRows (the live token
	//      was revoked between the first HasAnyLiveToken and the
	//      ValidateToken — the double-provision race).
	mock.ExpectQuery("SELECT t.id, t.workspace_id FROM workspace_auth_tokens t JOIN workspaces w").
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)

	// 1.7. core#2611 re-check HasAnyLiveToken: count=0 (zero live tokens
	//      now — the previously-valid token was revoked, so the presented
	//      bearer is stale by definition). The recovery branch fires,
	//      returns nil, and the handler proceeds to the upsert.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-reg-bootstrap-recovery").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// 2. resolveDeliveryMode (production selects delivery_mode AND runtime).
	mock.ExpectQuery("SELECT delivery_mode, runtime FROM workspaces").
		WithArgs("ws-reg-bootstrap-recovery").
		WillReturnError(sql.ErrNoRows)

	// 3. agent_card identity reconcile (best-effort; no row yet).
	mock.ExpectQuery("SELECT name, role FROM workspaces").
		WithArgs("ws-reg-bootstrap-recovery").
		WillReturnError(sql.ErrNoRows)

	// 4. Upsert workspace row.
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 5. Read-back URL for the WORKSPACE_ONLINE broadcast (best-effort).
	mock.ExpectQuery("SELECT url FROM workspaces").
		WithArgs("ws-reg-bootstrap-recovery").
		WillReturnError(sql.ErrNoRows)

	// 6. IssueToken gate: no live tokens → mint.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-reg-bootstrap-recovery").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 7-8. Lazy-heal platform_inbound_secret.
	mock.ExpectQuery("SELECT platform_inbound_secret FROM workspaces").
		WithArgs("ws-reg-bootstrap-recovery").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("UPDATE workspaces SET platform_inbound_secret").
		WithArgs(sqlmock.AnyArg(), "ws-reg-bootstrap-recovery").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 9. Clear last_register_failure_at on success.
	mock.ExpectExec("UPDATE workspaces SET last_register_failure_at = NULL").
		WithArgs("ws-reg-bootstrap-recovery").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// Include a stale bearer so the ValidateToken path is exercised (not
	// the "missing bearer → 401" path).
	body := `{"id":"ws-reg-bootstrap-recovery","url":"http://ws-reg-bootstrap-recovery.moleculesai.app/a2a","agent_card":{"name":"test"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer stale-revoked-bearer-from-previous-incarnation")

	handler.Register(c)

	// The recovery branch should have re-opened bootstrap. The handler
	// proceeds to the upsert and returns 200, NOT 401. This is the
	// observable fix: the loser box now succeeds instead of wedging.
	if w.Code != http.StatusOK {
		t.Fatalf("core#2611 bootstrap-recovery should re-open bootstrap when zero live tokens, got %d: %s",
			w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "registered" {
		t.Errorf("status = %v, want \"registered\"", resp["status"])
	}
	// The handler must have minted a fresh token in the recovery branch.
	if _, ok := resp["auth_token"]; !ok {
		t.Error("auth_token missing from response — recovery branch should have minted a new one")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_BootstrapRecovery_StaleBearerLiveTokensRemains (#2611
// hardening): the recovery branch must NOT fire when the workspace STILL
// has live tokens after the bearer validation fails. A stolen or
// misconfigured bearer (live tokens present, presented bearer invalid)
// must still 401 — the re-check only opens bootstrap when the live
// token set is genuinely empty, never when one is still there.
func TestRegister_BootstrapRecovery_StaleBearerLiveTokensRemains(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// 1. requireWorkspaceToken → first HasAnyLiveToken: count=1.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-reg-no-recovery").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	// 1.5. ValidateToken → lookupTokenByHash: ErrNoRows (the presented
	//      bearer is wrong / stale).
	mock.ExpectQuery("SELECT t.id, t.workspace_id FROM workspace_auth_tokens t JOIN workspaces w").
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)

	// 1.7. Re-check HasAnyLiveToken: count=1 (live tokens STILL exist).
	//      The recovery branch must NOT fire — this is the C18 hardening:
	//      a stolen/rotated/misconfigured bearer is still 401, not
	//      silently re-bootstrapped.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-reg-no-recovery").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"id":"ws-reg-no-recovery","url":"http://example.com","agent_card":{"name":"test"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer wrong-bearer-but-live-tokens-exist")

	handler.Register(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("stale bearer with live tokens still present MUST 401 (C18 hardening), got %d: %s",
			w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRegister_SuccessClearsLastRegisterFailure (#2530): a successful register
// must clear last_register_failure_at so heartbeat can recover to online.
func TestRegister_SuccessClearsLastRegisterFailure(t *testing.T) {
	// SaaS mode so the platform-tunnel hostname (ws-reg-ok.moleculesai.app) is
	// allowed while its DNS settles, instead of failing the SSRF DNS lookup in CI.
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Mock sequence mirrors the actual Register success path in registry.go.
	// 1. requireWorkspaceToken → HasAnyLiveToken: no live tokens → bootstrap-allowed.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-reg-ok").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// 2. resolveDeliveryMode (production selects delivery_mode AND runtime).
	mock.ExpectQuery("SELECT delivery_mode, runtime FROM workspaces").
		WithArgs("ws-reg-ok").
		WillReturnError(sql.ErrNoRows)

	// 3. agent_card identity reconcile (best-effort; no row yet).
	mock.ExpectQuery("SELECT name, role FROM workspaces").
		WithArgs("ws-reg-ok").
		WillReturnError(sql.ErrNoRows)

	// 4. Upsert workspace row.
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 5. Read-back URL for the WORKSPACE_ONLINE broadcast (best-effort).
	mock.ExpectQuery("SELECT url FROM workspaces").
		WithArgs("ws-reg-ok").
		WillReturnError(sql.ErrNoRows)

	// 6-7. Issue token (no live tokens → mint).
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-reg-ok").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 8-9. Lazy-heal platform_inbound_secret: read misses (ErrNoRows →
	// ErrNoInboundSecret) so IssuePlatformInboundSecret mints inline.
	mock.ExpectQuery("SELECT platform_inbound_secret FROM workspaces").
		WithArgs("ws-reg-ok").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("UPDATE workspaces SET platform_inbound_secret").
		WithArgs(sqlmock.AnyArg(), "ws-reg-ok").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 10. Clear last_register_failure_at on success.
	mock.ExpectExec("UPDATE workspaces SET last_register_failure_at = NULL").
		WithArgs("ws-reg-ok").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"id":"ws-reg-ok","url":"http://ws-reg-ok.moleculesai.app/a2a","agent_card":{"name":"test"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestHeartbeat_RecentRegisterFailure_DegradesWorkspace (#2530): when
// last_register_failure_at is within the 5-minute window, heartbeat must
// flip the workspace from online to degraded.
func TestHeartbeat_RecentRegisterFailure_DegradesWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// prevTask SELECT
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-degrade-reg").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	// heartbeat UPDATE
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-degrade-reg", 0.0, "", 0, 100, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus SELECT — online with recent register failure
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-degrade-reg").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).
			AddRow("online", "", time.Now().Add(-2*time.Minute), nil))

	// Degrade UPDATE
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusDegraded, "ws-degrade-reg").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Broadcast degraded event
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-degrade-reg","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":100}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestHeartbeat_RecentRegisterFailure_BlocksRecovery (#2530): a degraded
// workspace with a recent register failure must NOT be flipped back to online
// by heartbeat, even when error_rate is low and runtime_state is empty.
func TestHeartbeat_RecentRegisterFailure_BlocksRecovery(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// prevTask SELECT
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-no-recover").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	// heartbeat UPDATE
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-no-recover", 0.0, "", 0, 100, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus SELECT — degraded with recent register failure
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-no-recover").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).
			AddRow("degraded", "", time.Now().Add(-2*time.Minute), nil))

	// NO recovery UPDATE expected — register failure blocks recovery.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-no-recover","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":100}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ==================== Cross-cloud register: url fallback to agent_card (Bug A) ====================

// TestRegister_PushModeFallsBackToAgentCardURL pins the cross-cloud delivery fix:
// an egress-only box registers with an EMPTY top-level url but advertises its
// tunnel url in the agent_card. Push-mode must NOT reject it — it must derive the
// url from the agent_card so the workspace becomes deliverable. We assert the
// derived url lands in the INSERT (then force the insert to fail to short-circuit
// the rest of the success path, exactly like TestRegister_DBError).
func TestRegister_PushModeFallsBackToAgentCardURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-xcloud").
		WillReturnError(sql.ErrNoRows)

	// The INSERT must receive the agent_card's url as the url arg ($3), even
	// though payload.url was "". A mismatch here = the fallback didn't fire.
	// (Uses localhost — validateAgentURL does a live DNS resolve, so the test
	// host must resolve; in prod the tunnel CNAME exists and resolves.)
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("ws-xcloud", "ws-xcloud", "http://localhost:8000",
			`{"name":"x","url":"http://localhost:8000"}`, "push", "").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"id":"ws-xcloud","url":"","agent_card":{"name":"x","url":"http://localhost:8000"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Register(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 (forced insert error after reaching it), got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (derived url likely wrong): %v", err)
	}
}

// TestRegister_PushModeNoURLNoCardURLStill400 proves the fallback doesn't mask a
// genuine misconfiguration: push-mode with no url anywhere still 400s.
func TestRegister_PushModeNoURLNoCardURLStill400(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-nourl").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"id":"ws-nourl","url":"","agent_card":{"name":"x"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Register(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 (no url in payload or agent_card), got %d: %s", w.Code, w.Body.String())
	}
}

// TestRegister_400_LogsDiagnosticsReason is the #2680 residual regression
// guard. When a recreated container's first /registry/register call
// returns 400, the operator needs the failing-reason key
// (invalid_json | invalid_delivery_mode | invalid_kind | url_required_for_push
// | url_validate_failed) AND the workspace's existing row state
// (url, kind, delivery_mode) to identify the drift source. The
// 400 path that fires the diagnostic must emit a single grep-able
// log line BEFORE writing the response so the next restart run
// surfaces the cause directly.
//
// Each subtest exercises one of the 5 documented 400 paths. The
// existing row state is mocked where needed to verify the
// `existing_*` log fields are populated.
func TestRegister_400_LogsDiagnosticsReason(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		expectedReason string
		expectStatus   int
		setup          func(mock sqlmock.Sqlmock, workspaceID string)
	}{
		{
			name:           "invalid_delivery_mode",
			body:           `{"id":"ws-1","url":"http://localhost:8000","delivery_mode":"foo","agent_card":{"name":"x"}}`,
			expectedReason: "invalid_delivery_mode",
			expectStatus:   http.StatusBadRequest,
		},
		{
			name:           "invalid_kind",
			body:           `{"id":"ws-1","url":"http://localhost:8000","kind":"foo","agent_card":{"name":"x"}}`,
			expectedReason: "invalid_kind",
			expectStatus:   http.StatusBadRequest,
		},
		{
			name:           "url_required_for_push",
			body:           `{"id":"ws-1","delivery_mode":"push","agent_card":{"name":"x"}}`,
			expectedReason: "url_required_for_push",
			expectStatus:   http.StatusBadRequest,
			setup: func(mock sqlmock.Sqlmock, workspaceID string) {
				// C18 token gate: fresh-register path, no live tokens.
				mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
					WithArgs(workspaceID).
					WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
				mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
					WithArgs(workspaceID).
					WillReturnError(sql.ErrNoRows)
				// Defer boot_register_failed path: UPDATE failure timestamp.
				mock.ExpectExec("UPDATE workspaces SET last_register_failure_at").
					WithArgs(workspaceID).
					WillReturnResult(sqlmock.NewResult(0, 1))
			},
		},
		{
			name:           "url_validate_failed_link_local",
			body:           `{"id":"ws-1","url":"http://169.254.169.254:8000","delivery_mode":"push","agent_card":{"name":"x"}}`,
			expectedReason: "url_validate_failed",
			expectStatus:   http.StatusBadRequest,
			setup: func(mock sqlmock.Sqlmock, workspaceID string) {
				// C18 token gate: fresh-register path, no live tokens.
				mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
					WithArgs(workspaceID).
					WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
				mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
					WithArgs(workspaceID).
					WillReturnError(sql.ErrNoRows)
				mock.ExpectExec("UPDATE workspaces SET last_register_failure_at").
					WithArgs(workspaceID).
					WillReturnResult(sqlmock.NewResult(0, 1))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			broadcaster := newTestBroadcaster()
			handler := NewRegistryHandler(broadcaster)

			var buf bytes.Buffer
			oldOutput := log.Writer()
			log.SetOutput(&buf)
			defer log.SetOutput(oldOutput)

			if tc.setup != nil {
				tc.setup(mock, "ws-1")
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")

			handler.Register(c)

			if w.Code != tc.expectStatus {
				t.Errorf("expected status %d, got %d: %s", tc.expectStatus, w.Code, w.Body.String())
			}

			logs := buf.String()
			want := "registry_register_400 workspace=ws-1 reason=" + tc.expectedReason
			if !strings.Contains(logs, want) {
				t.Errorf("expected diagnostic log %q, got: %s", want, logs)
			}
		})
	}
}

// TestRegister_400_LogsExistingRowState verifies that the diagnostic
// log line captures the workspace's existing row state (URL, kind,
// delivery_mode) at the time of the 400 — so the operator can
// compare the rejected payload against the row to identify the
// drift source. The fetchExistingWorkspaceStateForDiagnostics query
// is mocked; the test asserts the log line carries the expected
// `existing_*` values (NOT the raw URL per RC #11335).
//
// Uses the url_validate_failed path (link-local URL) because it's
// the only 400 path that runs AFTER the fetch — invalid_json,
// invalid_delivery_mode, and invalid_kind all run before the
// existingState fetch (pre-ctx) and pass an empty diagnostics.
// url_required_for_push + url_validate_failed both run post-ctx
// and carry the fetched state.
func TestRegister_400_LogsExistingRowState(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	var buf bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldOutput)

	// Existing row with the documented (recreated container) state.
	// The trigger is a 400 on the url_validate_failed path, which
	// runs AFTER the existing-state fetch — so the test exercises
	// the real fetch path.
	//
	// Order of queries in the actual code path:
	//   1. fetchExistingWorkspaceStateForDiagnostics
	//      (SELECT url, kind, delivery_mode) — happens at L398, AFTER
	//      the ShouldBindJSON + delivery_mode/kind early checks.
	//   2. C18 token gate (Phase 30.1)
	//      (SELECT COUNT(*) FROM workspace_auth_tokens) — happens at L390,
	//      AFTER the fetch. Fresh-register path, no live tokens.
	//   3. resolveDeliveryMode (SELECT delivery_mode, runtime) — happens
	//      at L427, AFTER the C18 check.
	//   4. defer boot_register_failed UPDATE — happens AFTER the 400
	//      is returned.
	//
	// fetchExistingWorkspaceStateForDiagnostics reads url, kind,
	// delivery_mode from the same row.
	mock.ExpectQuery(`SELECT url, kind, delivery_mode FROM workspaces WHERE id`).
		WithArgs("ws-existing").
		WillReturnRows(sqlmock.NewRows([]string{"url", "kind", "delivery_mode"}).
			AddRow("https://ws-existing.example.com", "workspace", "push"))
	// C18 token gate: fresh-register path, no live tokens.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs("ws-existing").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	// resolveDeliveryMode reads delivery_mode + runtime from the
	// row.
	mock.ExpectQuery(`SELECT delivery_mode, runtime FROM workspaces WHERE id`).
		WithArgs("ws-existing").
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode", "runtime"}).
			AddRow("push", "external"))
	// Defer boot_register_failed path: UPDATE failure timestamp
	// (authOK=true after the body parses).
	mock.ExpectExec("UPDATE workspaces SET last_register_failure_at").
		WithArgs("ws-existing").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// Link-local URL — always blocked by validateAgentURL in any
	// deploy mode (RC #11335's chosen test URL). This drives the
	// url_validate_failed path so the fetch is exercised.
	body := `{"id":"ws-existing","url":"http://169.254.169.254:8000","delivery_mode":"push","agent_card":{"name":"x"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	logs := buf.String()
	// The diagnostic log must include the existing row's kind
	// (workspace) and delivery_mode (push) as the basis for the
	// operator's drift analysis. existing_url is REDACTED (RC #11335)
	// — it must be the literal "present", NOT the raw URL.
	if !strings.Contains(logs, "registry_register_400") {
		t.Errorf("expected registry_register_400 diagnostic, got: %s", logs)
	}
	if !strings.Contains(logs, "reason=url_validate_failed") {
		t.Errorf("expected reason=url_validate_failed, got: %s", logs)
	}
	if !strings.Contains(logs, "workspace=ws-existing") {
		t.Errorf("expected workspace=ws-existing, got: %s", logs)
	}
	// existing_kind and existing_delivery_mode use %q in the format
	// string (quoted), so the value is wrapped in double-quotes in
	// the log line. Asserting for the substring with quotes is the
	// correct shape.
	if !strings.Contains(logs, `existing_kind="workspace"`) {
		t.Errorf("expected existing_kind=\"workspace\" (from the row), got: %s", logs)
	}
	if !strings.Contains(logs, `existing_delivery_mode="push"`) {
		t.Errorf("expected existing_delivery_mode=\"push\" (from the row), got: %s", logs)
	}
	if !strings.Contains(logs, "existing_url=present") {
		t.Errorf("expected existing_url=present (URL redacted per RC #11335), got: %s", logs)
	}
	// Critical: the raw URL MUST NOT appear in the log line
	// (RC #11335). The agent's URL is also a private link-local
	// (169.254.169.254) which is itself PII-adjacent.
	if strings.Contains(logs, "ws-existing.example.com") {
		t.Errorf("REGRESSION: raw existing URL leaked into diagnostic log line (RC #11335): %s", logs)
	}
	if strings.Contains(logs, "169.254.169.254") {
		t.Errorf("REGRESSION: raw payload URL leaked into diagnostic log line (RC #11335): %s", logs)
	}
	// The payload URL is present (the agent sent http://169.254.169.254:8000);
	// it's the row's URL that may-or-may-not be present (we set it
	// present in the mock).
	if !strings.Contains(logs, "payload_url=present") {
		t.Errorf("expected payload_url=present (agent sent a URL), got: %s", logs)
	}
	// The detail field carries validateAgentURL's friendly CIDR label,
	// NOT the raw URL. (RC #11335's allowed exception for the detail
	// field.)
	if !strings.Contains(logs, "blocked address") {
		t.Errorf("expected friendly CIDR label in detail (validateAgentURL's message), got: %s", logs)
	}
}

// TestRegister_RejectsAgentCardURL_IMDS — REMOVED. The Register
// step-C isSafeURL check (the symmetric WRITE surface to
// UpdateCard's already-tested isSafeURL check at line 1226) uses
// the SAME isSafeURL helper on the SAME field shape. The
// TestUpdateCard_Rejects{CloudMetadata,NonHTTPScheme,LinkLocalIPv6,LoopbackURL}
// tests cover the helper exhaustively. A separate Register test
// would require mocking C18 token check + existingState + kind guard
// + resolveDeliveryMode (all run BEFORE the isSafeURL check), which
// is brittle. The test coverage gap is documented; the test
// would be a redundant copy of the UpdateCard coverage.

// TestRegister_PlatformAgentMissingMCPServer_FailsClosed guards the second half
// of issue #2970: a platform agent whose runtime reports mcp_server_present=false
// (or omits the field) must NOT be marked online, even when the MODEL secret is
// present. Fail-closed on the MCP server prevents a generic-image concierge from
// booting as a routable platform agent.
func TestRegister_PlatformAgentMissingMCPServer_FailsClosed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "ws-platform-no-mcp"

	// Bootstrap path — no live tokens.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// kind precheck: existing row is kind="platform".
	mock.ExpectQuery("SELECT kind FROM workspaces WHERE id").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))

	// MODEL secret exists, but the runtime declares the MCP server absent.
	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM workspace_secrets WHERE workspace_id = \\$1 AND key = 'MODEL'\\)").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// Gate failure broadcasts WORKSPACE_PROVISION_FAILED and marks the row failed.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET status = \\$3, last_sample_error = \\$2, updated_at = now\\(\\) WHERE id = \\$1").
		WithArgs(wsID, sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/register",
		bytes.NewBufferString(`{"id":"`+wsID+`","url":"http://localhost:9100","delivery_mode":"push","kind":"platform","mcp_server_present":false,"agent_card":{"name":"concierge"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Register(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("platform agent missing MCP server: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestHeartbeat_PlatformAgentMissingMCPServer_FailsClosed guards the heartbeat
// recovery side of issue #2970: a kind='platform' workspace whose runtime reports
// mcp_server_present=false must NOT recover to 'online' on heartbeat. Instead it
// is marked 'failed' so the canvas surfaces a provision failure.
func TestHeartbeat_PlatformAgentMissingMCPServer_FailsClosed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	const wsID = "ws-platform-heartbeat-no-mcp"

	// prevTask SELECT — use loose regex to match 3-col query on main
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"current_task"}).AddRow(""))

	// Heartbeat UPDATE (MonthlySpend=0 branch)
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs(wsID, 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus SELECT — platform agent currently in provisioning
	// (the recovery path most likely to resurrect a broken concierge).
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).
			AddRow("provisioning", "platform", nil, nil))

	// MODEL secret exists, but MCP server is absent.
	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM workspace_secrets WHERE workspace_id = \\$1 AND key = 'MODEL'\\)").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// Gate failure: broadcast WORKSPACE_PROVISION_FAILED + mark failed.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET status = \\$3, last_sample_error = \\$2, updated_at = now\\(\\) WHERE id = \\$1").
		WithArgs(wsID, sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"` + wsID + `","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":false}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat mcp gate: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
