package handlers

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// evalStatusRows builds the 4-column row returned by the evaluateStatus
// top query (status, kind, last_register_failure_at, mcp_unloaded_since).
func evalStatusRows(status, kind string, lastRegisterFailure, mcpUnloadedSince interface{}) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).
		AddRow(status, kind, lastRegisterFailure, mcpUnloadedSince)
}

// TestHeartbeatHandler_PlatformManagementMCPMissing_SustainedDegrades
// verifies core#3082 (CR2 #12653 fix) AND the grace-window flap fix: a
// platform concierge that reports loaded_mcp_tools but does NOT include the
// literal required tool identifier `mcp__molecule-platform__create_workspace`
// is marked degraded — BUT ONLY once the absence has persisted past
// managementMCPUnloadedGrace. Here mcp_unloaded_since is set well in the past,
// so the grace window has elapsed and the gate degrades (intent preserved:
// sustained-missing DOES degrade).
func TestHeartbeatHandler_PlatformManagementMCPMissing_SustainedDegrades(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Initial heartbeat UPDATE.
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-mcp-missing").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-mcp-missing", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus: currentStatus=online, kind=platform, unloaded since 5min ago.
	sustained := time.Now().Add(-5 * time.Minute)
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-mcp-missing").
		WillReturnRows(evalStatusRows("online", "platform", nil, sustained))

	// platformAgentHasModelSecret: model secret exists.
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-mcp-missing").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// platformAgentManagementMCPLoaded: listDeclaredPlugins returns management MCP.
	mock.ExpectQuery("SELECT plugin_name, source_raw FROM workspace_declared_plugins").
		WithArgs("ws-mcp-missing").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow(conciergePlatformMCPName, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main"))

	// Degraded UPDATE — required tool absent past the grace window.
	mock.ExpectExec("UPDATE workspaces SET status =.*status = 'online'").
		WithArgs(models.StatusDegraded, "platform agent management MCP declared but not loaded; marking degraded (core#3082)", "ws-mcp-missing").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// WORKSPACE_DEGRADED broadcast.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// loaded_mcp_tools has plenty of tools but NOT the literal required one.
	body := `{"workspace_id":"ws-mcp-missing","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","mcp__other-server__other-tool"]}`
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

// TestHeartbeatHandler_PlatformManagementMCPMissing_WithinGrace_NoDegrade is
// the flap fix: the FIRST heartbeat observing an absent management MCP must
// NOT degrade. It stamps mcp_unloaded_since (starting the grace window) and
// leaves the agent online. This is the warmup case the ~50/50 flap came from.
func TestHeartbeatHandler_PlatformManagementMCPMissing_WithinGrace_NoDegrade(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-mcp-warmup").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-mcp-warmup", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// mcp_unloaded_since is NULL → first observation → stamp, no degrade.
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-mcp-warmup").
		WillReturnRows(evalStatusRows("online", "platform", nil, nil))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-mcp-warmup").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	mock.ExpectQuery("SELECT plugin_name, source_raw FROM workspace_declared_plugins").
		WithArgs("ws-mcp-warmup").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow(conciergePlatformMCPName, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main"))

	// Stamp the first-seen-unloaded time. NO degrade UPDATE, NO broadcast.
	mock.ExpectExec("UPDATE workspaces SET mcp_unloaded_since = COALESCE").
		WithArgs("ws-mcp-warmup").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-mcp-warmup","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","mcp__other-server__other-tool"]}`
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

// TestHeartbeatHandler_PlatformManagementMCPLoaded_ClearsStampStaysOnline
// verifies that a platform concierge reporting the literal required
// create_workspace tool stays online AND that an outstanding
// mcp_unloaded_since stamp is cleared (so a future absence starts a fresh
// grace window instead of degrading instantly).
func TestHeartbeatHandler_PlatformManagementMCPLoaded_ClearsStampStaysOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-mcp-ok").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-mcp-ok", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// A stale stamp is present (the agent was warming up); it must be cleared.
	staleStamp := time.Now().Add(-30 * time.Second)
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-mcp-ok").
		WillReturnRows(evalStatusRows("online", "platform", nil, staleStamp))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-mcp-ok").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	mock.ExpectQuery("SELECT plugin_name, source_raw FROM workspace_declared_plugins").
		WithArgs("ws-mcp-ok").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow(conciergePlatformMCPName, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main"))

	// Clear the stamp now that the management MCP is loaded.
	mock.ExpectExec("UPDATE workspaces SET mcp_unloaded_since = NULL").
		WithArgs("ws-mcp-ok").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-mcp-ok","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","` + conciergePlatformMCPCreateWorkspaceTool + `"]}`
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

// TestHeartbeatHandler_RuntimeEmitsServerPresentButNoLoadedTools_SustainedDegraded
// pins the CR2+Researcher fail-loud behavior under the grace window: a runtime
// that speaks the #147 contract (mcp_server_present=true) but does NOT report
// loaded_mcp_tools is treated as unloaded, and degrades once the absence
// outlasts the grace window. Here mcp_unloaded_since is old enough to degrade.
func TestHeartbeatHandler_RuntimeEmitsServerPresentButNoLoadedTools_SustainedDegraded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-server-present-no-tools").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-server-present-no-tools", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	sustained := time.Now().Add(-5 * time.Minute)
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-server-present-no-tools").
		WillReturnRows(evalStatusRows("online", "platform", nil, sustained))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-server-present-no-tools").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// Degraded UPDATE — runtime spoke server-present but omitted loaded_mcp_tools
	// and the absence has outlasted the grace window.
	mock.ExpectExec("UPDATE workspaces SET status =.*status = 'online'").
		WithArgs(models.StatusDegraded, "platform agent runtime did not report loaded_mcp_tools on a mcp_server_present=true heartbeat; cannot verify create_workspace tool is loaded — marking degraded (core#3082)", "ws-server-present-no-tools").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-server-present-no-tools","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true}`
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

// TestHeartbeatHandler_RuntimeEmitsServerPresentButNoLoadedTools_WithinGrace
// pins the flap fix for the absent-loaded_mcp_tools path: on the first such
// heartbeat (mcp_unloaded_since NULL) the gate stamps and does NOT degrade.
// This is precisely the live test1 case — the runtime never reports
// loaded_mcp_tools because the producer is not wired, and the old gate
// degraded on every online heartbeat, causing the oscillation.
func TestHeartbeatHandler_RuntimeEmitsServerPresentButNoLoadedTools_WithinGrace(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-no-tools-warmup").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-no-tools-warmup", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-no-tools-warmup").
		WillReturnRows(evalStatusRows("online", "platform", nil, nil))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-no-tools-warmup").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// Stamp only — no degrade, no broadcast.
	mock.ExpectExec("UPDATE workspaces SET mcp_unloaded_since = COALESCE").
		WithArgs("ws-no-tools-warmup").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-no-tools-warmup","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true}`
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

// TestHeartbeatHandler_DegradedNotRecoveredWhileMCPUnloaded verifies Bug B:
// a platform agent currently 'degraded' with low error_rate must NOT be
// recovered to online while THIS heartbeat still observes the management MCP
// as unloaded past the grace window. Without the managementMCPUnloaded guard
// on the recovery branch, a genuinely MCP-less concierge would oscillate
// degraded->online forever. The #3082 block has already run on a 'degraded'
// row (its degrade UPDATE is a no-op since status != 'online'), set
// managementMCPUnloaded=true, and the recovery branch must be skipped.
func TestHeartbeatHandler_DegradedNotRecoveredWhileMCPUnloaded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-stuck-degraded").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "degraded"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-stuck-degraded", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// currentStatus=degraded; mcp_unloaded_since well past the grace window.
	sustained := time.Now().Add(-5 * time.Minute)
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-stuck-degraded").
		WillReturnRows(evalStatusRows("degraded", "platform", nil, sustained))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-stuck-degraded").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	mock.ExpectQuery("SELECT plugin_name, source_raw FROM workspace_declared_plugins").
		WithArgs("ws-stuck-degraded").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow(conciergePlatformMCPName, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main"))

	// The #3082 degrade UPDATE still runs (guarded by status='online' in SQL, so
	// it is a no-op here) and the WORKSPACE_DEGRADED broadcast fires.
	mock.ExpectExec("UPDATE workspaces SET status =.*status = 'online'").
		WithArgs(models.StatusDegraded, "platform agent management MCP declared but not loaded; marking degraded (core#3082)", "ws-stuck-degraded").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// CRITICAL: NO "UPDATE ... SET status=online ... status='degraded'" recovery
	// UPDATE is expected — the recovery branch is gated on !managementMCPUnloaded.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// Functional agent (error_rate 0, no wedge) but management MCP unloaded.
	body := `{"workspace_id":"ws-stuck-degraded","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a"]}`
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

// TestHeartbeatHandler_Pre147RuntimeNoMCPServerPresent_StaysOnline pins the
// backward-compat path: a runtime that predates the #147 contract (neither
// mcp_server_present nor loaded_mcp_tools) does NOT trigger the #3082 gate.
func TestHeartbeatHandler_Pre147RuntimeNoMCPServerPresent_StaysOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-pre-147").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-pre-147", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-pre-147").
		WillReturnRows(evalStatusRows("online", "platform", nil, nil))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-pre-147").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// No listDeclaredPlugins query — the #3082 gate is skipped entirely for
	// pre-#147 runtimes (mcp_server_present nil ⇒ platformAgentMCPServerPresent
	// returns true under nil-tolerance; the new gate requires
	// mcp_server_present != nil && *mcp_server_present).

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-pre-147","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60}`
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

// TestHeartbeatHandler_PlatformManagementMCPLookupError_FlipsOnlineToDegraded
// verifies that a failure to read workspace_declared_plugins is fail-loud and
// is NOT subject to the grace window: the workspace is marked degraded
// immediately rather than staying online with an unverified management MCP.
func TestHeartbeatHandler_PlatformManagementMCPLookupError_FlipsOnlineToDegraded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-mcp-lookup-err").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-mcp-lookup-err", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// No prior stamp — lookup error degrades regardless of the grace window.
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-mcp-lookup-err").
		WillReturnRows(evalStatusRows("online", "platform", nil, nil))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-mcp-lookup-err").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// platformAgentManagementMCPLoaded: listDeclaredPlugins fails.
	mock.ExpectQuery("SELECT plugin_name, source_raw FROM workspace_declared_plugins").
		WithArgs("ws-mcp-lookup-err").
		WillReturnError(errors.New("connection refused"))

	// Degraded UPDATE — lookup failure must not silently look healthy. Use
	// AnyArg for the message so the test is not brittle against the wrapped
	// error string.
	mock.ExpectExec("UPDATE workspaces SET status =.*status = 'online'").
		WithArgs(models.StatusDegraded, sqlmock.AnyArg(), "ws-mcp-lookup-err").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-mcp-lookup-err","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["` + conciergePlatformMCPCreateWorkspaceTool + `"]}`
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
