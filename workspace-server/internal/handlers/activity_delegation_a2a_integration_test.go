//go:build integration
// +build integration

// activity_delegation_a2a_integration_test.go — REAL Postgres integration tests
// for Activity, Delegation, and A2A Queue handlers (#2151 CHUNK 1).
//
// Run with:
//
//	docker run --rm -d --name pg-integration \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_
//
// CI (.gitea/workflows/handlers-postgres-integration.yml) runs this on every
// PR that touches workspace-server/internal/handlers/**.

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// integrationDB_ActivityDelegationA2A opens a connection from
// $INTEGRATION_DB_URL (failing the test if unset), wipes the tables
// used by these tests, hot-swaps the package-level db.DB, and registers
// a Cleanup that restores the previous db.DB + closes the connection.
//
// NOT SAFE FOR `t.Parallel()` — each test gets the tables to itself.
func integrationDB_ActivityDelegationA2A(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Fatal("INTEGRATION_DB_URL not set; failing (local devs: see file header)")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if _, err := conn.ExecContext(ctx2, `
		DELETE FROM a2a_queue WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'test-2151-%');
		DELETE FROM activity_logs WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'test-2151-%');
		DELETE FROM delegations WHERE caller_id IN (SELECT id FROM workspaces WHERE name LIKE 'test-2151-%');
		DELETE FROM workspaces WHERE name LIKE 'test-2151-%';
	`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	prev := db.DB
	db.DB = conn
	t.Cleanup(func() {
		db.DB = prev
		conn.Close()
	})
	return conn
}

// seedWorkspace inserts a workspace with the given name and returns its id.
func seedWorkspace(t *testing.T, conn *sql.DB, name string) string {
	t.Helper()
	var id string
	if err := conn.QueryRowContext(context.Background(), `
		INSERT INTO workspaces (id, name, status)
		VALUES (gen_random_uuid(), $1, 'online')
		RETURNING id
	`, name).Scan(&id); err != nil {
		t.Fatalf("seedWorkspace %q: %v", name, err)
	}
	return id
}

// seedActivityLog inserts an activity_logs row and returns its id.
func seedActivityLog(t *testing.T, conn *sql.DB, workspaceID, activityType, method, status string, sourceID, targetID *string) string {
	t.Helper()
	var id string
	reqBody := map[string]interface{}{"test": true}
	reqJSON, _ := json.Marshal(reqBody)
	if err := conn.QueryRowContext(context.Background(), `
		INSERT INTO activity_logs (workspace_id, activity_type, method, status, source_id, target_id, request_body)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		RETURNING id
	`, workspaceID, activityType, method, status, sourceID, targetID, string(reqJSON)).Scan(&id); err != nil {
		t.Fatalf("seedActivityLog: %v", err)
	}
	return id
}

// seedA2AQueueItem inserts an a2a_queue row and returns its id.
func seedA2AQueueItem(t *testing.T, conn *sql.DB, workspaceID, callerID string, priority int, body []byte, status string) string {
	t.Helper()
	var id string
	if err := conn.QueryRowContext(context.Background(), `
		INSERT INTO a2a_queue (workspace_id, caller_id, priority, body, status)
		VALUES ($1, $2, $3, $4::jsonb, $5)
		RETURNING id
	`, workspaceID, callerID, priority, string(body), status).Scan(&id); err != nil {
		t.Fatalf("seedA2AQueueItem: %v", err)
	}
	return id
}

// noOpEmitter is a test-only stub that satisfies events.EventEmitter.
type noOpEmitter struct{}

func (noOpEmitter) RecordAndBroadcast(ctx context.Context, eventType string, workspaceID string, payload interface{}) error {
	return nil
}
func (noOpEmitter) BroadcastOnly(workspaceID string, eventType string, payload interface{}) {}

// newTestGinContext creates a gin.Context with an httptest recorder.
func newTestGinContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return c, w
}

// ---------- Activity handler integration tests ----------

func TestIntegration_ActivityList_Basic(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-activity-list")
	seedActivityLog(t, conn, wsID, "agent_log", "test_method", "ok", nil, nil)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 activity, got %d", len(resp))
	}
}

func TestIntegration_ActivityReport_SourceIDSpoofGuard(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-activity-spoof")
	otherWS := seedWorkspace(t, conn, "test-2151-activity-victim")

	h := NewActivityHandler(noOpEmitter{})
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/activity", strings.NewReader(`{
		"activity_type": "agent_log",
		"source_id": "`+otherWS+`"
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Report(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("Report with spoofed source_id returned %d, want 403", w.Code)
	}
}

func TestIntegration_ActivityReport_ValidType(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-activity-valid")

	h := NewActivityHandler(noOpEmitter{})
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/activity", strings.NewReader(`{
		"activity_type": "agent_log",
		"summary": "test"
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Report(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Report valid activity returned %d, want 200", w.Code)
	}
}

func TestIntegration_ActivityList_FilterByType(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-activity-filter")
	seedActivityLog(t, conn, wsID, "agent_log", "method1", "ok", nil, nil)
	seedActivityLog(t, conn, wsID, "a2a_receive", "method2", "ok", nil, nil)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/activity?type=agent_log", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 activity, got %d", len(resp))
	}
	if resp[0]["activity_type"] != "agent_log" {
		t.Fatalf("expected agent_log, got %v", resp[0]["activity_type"])
	}
}

func TestIntegration_SessionSearch_Basic(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-session-search")
	seedActivityLog(t, conn, wsID, "agent_log", "test_method", "ok", nil, nil)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/session?q=test", nil)
	h.SessionSearch(c)

	if w.Code != http.StatusOK {
		t.Fatalf("SessionSearch returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp))
	}
}

func TestIntegration_SessionSearch_EmptyQuery(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-session-empty")
	seedActivityLog(t, conn, wsID, "agent_log", "method", "ok", nil, nil)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/session", nil)
	h.SessionSearch(c)

	if w.Code != http.StatusOK {
		t.Fatalf("SessionSearch returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp))
	}
}

func TestIntegration_Notify_Basic(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-notify-basic")

	h := NewActivityHandler(noOpEmitter{})
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/notify", strings.NewReader(`{
		"message": "hello user"
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Notify(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Notify returned %d, want 200", w.Code)
	}
	var count int
	if err := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM activity_logs WHERE workspace_id = $1 AND method = 'notify'`, wsID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 notify row, got %d", count)
	}
}

func TestIntegration_Notify_InvalidAttachment(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-notify-attach")

	h := NewActivityHandler(noOpEmitter{})
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/notify", strings.NewReader(`{
		"message": "hi",
		"attachments": [{"uri":"","name":""}]
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Notify(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Notify with empty attachment returned %d, want 400", w.Code)
	}
}

func TestIntegration_ActivityList_FilterBySourceCanvas(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-source-canvas")
	seedActivityLog(t, conn, wsID, "agent_log", "m1", "ok", nil, nil) // canvas (source_id IS NULL)
	peerID := seedWorkspace(t, conn, "test-2151-peer-canvas")
	seedActivityLog(t, conn, wsID, "a2a_receive", "m2", "ok", &peerID, nil) // agent

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/activity?source=canvas", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 canvas row, got %d", len(resp))
	}
	if resp[0]["method"] != "m1" {
		t.Fatalf("expected method m1, got %v", resp[0]["method"])
	}
}

func TestIntegration_ActivityList_FilterBySourceAgent(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-source-agent")
	seedActivityLog(t, conn, wsID, "agent_log", "m1", "ok", nil, nil)
	peerID := seedWorkspace(t, conn, "test-2151-peer-agent")
	seedActivityLog(t, conn, wsID, "a2a_receive", "m2", "ok", &peerID, nil)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/activity?source=agent", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 agent row, got %d", len(resp))
	}
	if resp[0]["method"] != "m2" {
		t.Fatalf("expected method m2, got %v", resp[0]["method"])
	}
}

func TestIntegration_ActivityList_InvalidSource(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-invalid-source")

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/activity?source=invalid", nil)
	h.List(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("List returned %d, want 400", w.Code)
	}
}

func TestIntegration_ActivityList_SinceSecs(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-since-secs")
	seedActivityLog(t, conn, wsID, "agent_log", "old", "ok", nil, nil)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/activity?since_secs=1", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The seed row was inserted just now, so it should be within 1 second.
	if len(resp) != 1 {
		t.Fatalf("expected 1 row within 1s, got %d", len(resp))
	}
}

func TestIntegration_ActivityList_SinceIDCursor(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-since-id")
	id1 := seedActivityLog(t, conn, wsID, "agent_log", "first", "ok", nil, nil)
	seedActivityLog(t, conn, wsID, "agent_log", "second", "ok", nil, nil)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/activity?since_id="+id1, nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 row after cursor, got %d", len(resp))
	}
	if resp[0]["method"] != "second" {
		t.Fatalf("expected method 'second', got %v", resp[0]["method"])
	}
}

func TestIntegration_ActivityList_SinceIDCursorGone(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-since-id-gone")
	otherWS := seedWorkspace(t, conn, "test-2151-other-cursor")
	cursorID := seedActivityLog(t, conn, otherWS, "agent_log", "cursor", "ok", nil, nil)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/activity?since_id="+cursorID, nil)
	h.List(c)

	if w.Code != http.StatusGone {
		t.Fatalf("List returned %d, want 410", w.Code)
	}
}

func TestIntegration_ActivityList_PeerID(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-peer-id")
	peerID := seedWorkspace(t, conn, "test-2151-peer-target")
	seedActivityLog(t, conn, wsID, "agent_log", "no-peer", "ok", nil, nil)
	seedActivityLog(t, conn, wsID, "a2a_send", "peer-source", "ok", &peerID, nil)
	seedActivityLog(t, conn, wsID, "a2a_receive", "peer-target", "ok", nil, &peerID)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/activity?peer_id="+peerID, nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("expected 2 peer rows, got %d", len(resp))
	}
}

func TestIntegration_ActivityList_InvalidPeerID(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-invalid-peer")

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/activity?peer_id=not-a-uuid", nil)
	h.List(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("List returned %d, want 400", w.Code)
	}
}

func TestIntegration_ActivityList_IncludePeerInfo(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-include-peer")
	peerID := seedWorkspace(t, conn, "test-2151-peer-enriched")
	if _, err := conn.ExecContext(context.Background(), `UPDATE workspaces SET role = $1 WHERE id = $2`, "test-peer", peerID); err != nil {
		t.Fatalf("set peer role: %v", err)
	}
	seedActivityLog(t, conn, wsID, "a2a_receive", "m1", "ok", &peerID, nil)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/activity?include=peer_info", nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 row, got %d", len(resp))
	}
	if _, ok := resp[0]["peer_name"]; !ok {
		t.Fatalf("expected peer_name in response")
	}
	if _, ok := resp[0]["peer_role"]; !ok {
		t.Fatalf("expected peer_role in response")
	}
}

func TestIntegration_ActivityList_BeforeTS(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-before-ts")
	seedActivityLog(t, conn, wsID, "agent_log", "old", "ok", nil, nil)

	// Capture a timestamp between the two rows so only "old" is before it.
	// Use nanosecond precision and a generous gap to avoid second-truncation
	// or clock-skew between Go time.Now() and Postgres now().
	time.Sleep(200 * time.Millisecond)
	beforeTS := time.Now().UTC().Format(time.RFC3339Nano)
	time.Sleep(200 * time.Millisecond)
	seedActivityLog(t, conn, wsID, "agent_log", "new", "ok", nil, nil)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/activity?before_ts="+url.QueryEscape(beforeTS), nil)
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 row before ts, got %d", len(resp))
	}
	if resp[0]["method"] != "old" {
		t.Fatalf("expected method 'old', got %v", resp[0]["method"])
	}
}

func TestIntegration_ActivityList_InvalidBeforeTS(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-invalid-before")

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/activity?before_ts=not-a-timestamp", nil)
	h.List(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("List returned %d, want 400", w.Code)
	}
}

// ---------- Delegation handler integration tests ----------

func TestIntegration_DelegationList_Basic(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-delegation-list")

	// Insert a delegation row via the legacy activity_logs path
	taskJSON, _ := json.Marshal(map[string]interface{}{"task": "hello", "delegation_id": "del-test-1"})
	respJSON, _ := json.Marshal(map[string]interface{}{"delegation_id": "del-test-1"})
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, request_body, response_body, status)
		VALUES ($1, 'delegation', 'delegate', $1, $1, 'Delegating to test', $2::jsonb, $3::jsonb, 'pending')
	`, wsID, string(taskJSON), string(respJSON)); err != nil {
		t.Fatalf("seed delegation: %v", err)
	}

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	dh := NewDelegationHandler(wh, noOpEmitter{})
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	dh.ListDelegations(c)

	if w.Code != http.StatusOK {
		t.Fatalf("ListDelegations returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 delegation, got %d", len(resp))
	}
}

func TestIntegration_Delegate_SelfDelegationGuard(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-delegate-self")

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	dh := NewDelegationHandler(wh, noOpEmitter{})
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/delegate", strings.NewReader(`{
		"target_id": "`+wsID+`",
		"task": "do something"
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	dh.Delegate(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Delegate self-delegation returned %d, want 400", w.Code)
	}
}

func TestIntegration_Delegate_Idempotency(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-delegate-idem")
	targetID := seedWorkspace(t, conn, "test-2151-delegate-idem-target")

	taskJSON, _ := json.Marshal(map[string]interface{}{"task": "hello", "delegation_id": "del-idem-1"})
	respJSON, _ := json.Marshal(map[string]interface{}{"delegation_id": "del-idem-1"})
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, request_body, response_body, status, idempotency_key)
		VALUES ($1, 'delegation', 'delegate', $1, $2, 'Delegating to test', $3::jsonb, $4::jsonb, 'pending', 'idem-key-delegate')
	`, wsID, targetID, string(taskJSON), string(respJSON)); err != nil {
		t.Fatalf("seed idempotent delegation: %v", err)
	}

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	dh := NewDelegationHandler(wh, noOpEmitter{})
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/delegate", strings.NewReader(`{
		"target_id": "`+targetID+`",
		"task": "do something",
		"idempotency_key": "idem-key-delegate"
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	dh.Delegate(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Delegate idempotency returned %d, want 200", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["idempotent_hit"] != true {
		t.Fatalf("expected idempotent_hit=true, got %v", resp["idempotent_hit"])
	}
	if resp["delegation_id"] != "del-idem-1" {
		t.Fatalf("expected delegation_id=del-idem-1, got %v", resp["delegation_id"])
	}
}

func TestIntegration_Delegate_SuccessPath(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-delegate-ok")
	targetID := seedWorkspace(t, conn, "test-2151-delegate-ok-target")

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	dh := NewDelegationHandler(wh, noOpEmitter{})
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/delegate", strings.NewReader(`{
		"target_id": "`+targetID+`",
		"task": "do something"
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	dh.Delegate(c)

	if w.Code != http.StatusAccepted {
		t.Fatalf("Delegate returned %d, want 202", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	delegationID, ok := resp["delegation_id"].(string)
	if !ok || delegationID == "" {
		t.Fatal("expected non-empty delegation_id")
	}

	// Verify a row exists for this delegation (status may have been updated by the background goroutine)
	var count int
	if err := conn.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM activity_logs
		WHERE workspace_id = $1 AND method = 'delegate' AND request_body->>'delegation_id' = $2
	`, wsID, delegationID).Scan(&count); err != nil {
		t.Fatalf("select delegation row: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 delegation row, got %d", count)
	}

	// Drain the background goroutine so it doesn't race with the next test's db.DB swap
	wh.waitAsyncForTest()
}

func TestIntegration_Record_Basic(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-record-basic")
	targetID := seedWorkspace(t, conn, "test-2151-record-target")

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	dh := NewDelegationHandler(wh, noOpEmitter{})
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/delegations/record", strings.NewReader(`{
		"target_id": "`+targetID+`",
		"task": "recorded task",
		"delegation_id": "del-record-1"
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	dh.Record(c)

	if w.Code != http.StatusAccepted {
		t.Fatalf("Record returned %d, want 202", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "recorded" {
		t.Fatalf("expected status recorded, got %v", resp["status"])
	}

	var status string
	if err := conn.QueryRowContext(context.Background(), `
		SELECT status FROM activity_logs
		WHERE workspace_id = $1 AND method = 'delegate' AND request_body->>'delegation_id' = 'del-record-1'
	`, wsID).Scan(&status); err != nil {
		t.Fatalf("select record row: %v", err)
	}
	if status != "dispatched" {
		t.Fatalf("expected status dispatched, got %s", status)
	}
}

func TestIntegration_UpdateStatus_Completed(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-update-completed")
	targetID := seedWorkspace(t, conn, "test-2151-update-completed-target")

	taskJSON, _ := json.Marshal(map[string]interface{}{"task": "hello", "delegation_id": "del-update-1"})
	respJSON, _ := json.Marshal(map[string]interface{}{"delegation_id": "del-update-1"})
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, request_body, response_body, status)
		VALUES ($1, 'delegation', 'delegate', $1, $2, 'Delegating to test', $3::jsonb, $4::jsonb, 'dispatched')
	`, wsID, targetID, string(taskJSON), string(respJSON)); err != nil {
		t.Fatalf("seed delegation: %v", err)
	}

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	dh := NewDelegationHandler(wh, noOpEmitter{})
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "delegation_id", Value: "del-update-1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/delegations/del-update-1/update", strings.NewReader(`{
		"status": "completed",
		"response_preview": "done"
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	dh.UpdateStatus(c)

	if w.Code != http.StatusOK {
		t.Fatalf("UpdateStatus returned %d, want 200", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "completed" {
		t.Fatalf("expected status completed, got %v", resp["status"])
	}

	var status string
	if err := conn.QueryRowContext(context.Background(), `
		SELECT status FROM activity_logs
		WHERE workspace_id = $1 AND method = 'delegate' AND request_body->>'delegation_id' = 'del-update-1'
	`, wsID).Scan(&status); err != nil {
		t.Fatalf("select: %v", err)
	}
	if status != "completed" {
		t.Fatalf("expected status completed, got %s", status)
	}

	var count int
	if err := conn.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM activity_logs
		WHERE workspace_id = $1 AND method = 'delegate_result' AND status = 'completed'
	`, wsID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 delegate_result row, got %d", count)
	}
}

func TestIntegration_UpdateStatus_Failed(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-update-failed")
	targetID := seedWorkspace(t, conn, "test-2151-update-failed-target")

	taskJSON, _ := json.Marshal(map[string]interface{}{"task": "hello", "delegation_id": "del-update-fail-1"})
	respJSON, _ := json.Marshal(map[string]interface{}{"delegation_id": "del-update-fail-1"})
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, request_body, response_body, status)
		VALUES ($1, 'delegation', 'delegate', $1, $2, 'Delegating to test', $3::jsonb, $4::jsonb, 'dispatched')
	`, wsID, targetID, string(taskJSON), string(respJSON)); err != nil {
		t.Fatalf("seed delegation: %v", err)
	}

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	dh := NewDelegationHandler(wh, noOpEmitter{})
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "delegation_id", Value: "del-update-fail-1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/delegations/del-update-fail-1/update", strings.NewReader(`{
		"status": "failed",
		"error": "something went wrong"
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	dh.UpdateStatus(c)

	if w.Code != http.StatusOK {
		t.Fatalf("UpdateStatus returned %d, want 200", w.Code)
	}

	var status string
	if err := conn.QueryRowContext(context.Background(), `
		SELECT status FROM activity_logs
		WHERE workspace_id = $1 AND method = 'delegate' AND request_body->>'delegation_id' = 'del-update-fail-1'
	`, wsID).Scan(&status); err != nil {
		t.Fatalf("select: %v", err)
	}
	if status != "failed" {
		t.Fatalf("expected status failed, got %s", status)
	}
}

// ---------- A2A Queue integration tests ----------

func TestIntegration_A2AQueue_EnqueueAndDepth(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-a2a-queue")
	callerID := seedWorkspace(t, conn, "test-2151-a2a-caller")

	body := []byte(`{"method":"message/send","params":{"message":{"text":"hi"}}}`)
	id, depth, err := EnqueueA2A(context.Background(), wsID, callerID, PriorityTask, body, "message/send", "", nil)
	if err != nil {
		t.Fatalf("EnqueueA2A: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty queue id")
	}
	if depth != 1 {
		t.Fatalf("expected depth 1, got %d", depth)
	}

	// Verify row exists
	var status string
	if err := conn.QueryRowContext(context.Background(), `SELECT status FROM a2a_queue WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("select queue row: %v", err)
	}
	if status != "queued" {
		t.Fatalf("expected status queued, got %s", status)
	}

	// Verify depth via direct query (QueueDepth helper removed in dead-code sweep)
	var d int
	if err := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued'`, wsID).Scan(&d); err != nil {
		t.Fatalf("depth count: %v", err)
	}
	if d != 1 {
		t.Fatalf("expected queue depth 1, got %d", d)
	}
}

func TestIntegration_A2AQueue_DequeueNext(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-a2a-dequeue")
	body := []byte(`{"test":true}`)
	seedA2AQueueItem(t, conn, wsID, "00000000-0000-0000-0000-000000000001", PriorityTask, body, "queued")

	item, err := DequeueNext(context.Background(), wsID)
	if err != nil {
		t.Fatalf("DequeueNext: %v", err)
	}
	if item == nil {
		t.Fatal("expected item, got nil")
	}

	// Verify status flipped to dispatched
	var status string
	if err := conn.QueryRowContext(context.Background(), `SELECT status FROM a2a_queue WHERE id = $1`, item.ID).Scan(&status); err != nil {
		t.Fatalf("select: %v", err)
	}
	if status != "dispatched" {
		t.Fatalf("expected dispatched, got %s", status)
	}
}

func TestIntegration_A2AQueue_IdempotencyConflict(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-a2a-idem")
	callerID := seedWorkspace(t, conn, "test-2151-a2a-idem-caller")
	body := []byte(`{"method":"message/send","params":{"message":{"text":"hi"}}}`)

	id1, depth1, err := EnqueueA2A(context.Background(), wsID, callerID, PriorityTask, body, "message/send", "idem-key-1", nil)
	if err != nil {
		t.Fatalf("EnqueueA2A first: %v", err)
	}
	if depth1 != 1 {
		t.Fatalf("expected depth 1, got %d", depth1)
	}

	id2, depth2, err := EnqueueA2A(context.Background(), wsID, callerID, PriorityTask, body, "message/send", "idem-key-1", nil)
	if err != nil {
		t.Fatalf("EnqueueA2A second: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("idempotency mismatch: %s vs %s", id1, id2)
	}
	if depth2 != 1 {
		t.Fatalf("expected depth still 1 after idempotent re-enqueue, got %d", depth2)
	}
}

func TestIntegration_A2AQueue_MarkCompletedAndFailed(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-a2a-lifecycle")
	body := []byte(`{"test":true}`)
	qid := seedA2AQueueItem(t, conn, wsID, "00000000-0000-0000-0000-000000000001", PriorityTask, body, "dispatched")

	MarkQueueItemCompleted(context.Background(), qid, nil)
	var status string
	if err := conn.QueryRowContext(context.Background(), `SELECT status FROM a2a_queue WHERE id = $1`, qid).Scan(&status); err != nil {
		t.Fatalf("select after completed: %v", err)
	}
	if status != "completed" {
		t.Fatalf("expected completed, got %s", status)
	}

	// Seed another item to test failed path with max attempts.
	// Pre-set attempts=5 so the first MarkQueueItemFailed sees attempts>=5
	// and transitions straight to failed (MarkQueueItemFailed increments
	// attempts on dispatch via DequeueNext, but we call it directly here).
	qid2 := seedA2AQueueItem(t, conn, wsID, "00000000-0000-0000-0000-000000000001", PriorityTask, body, "dispatched")
	if _, err := conn.ExecContext(context.Background(), `UPDATE a2a_queue SET attempts = 5 WHERE id = $1`, qid2); err != nil {
		t.Fatalf("set attempts: %v", err)
	}
	for i := 0; i < 6; i++ {
		MarkQueueItemFailed(context.Background(), qid2, "transient error")
	}
	var status2 string
	var lastErr string
	if err := conn.QueryRowContext(context.Background(), `SELECT status, last_error FROM a2a_queue WHERE id = $1`, qid2).Scan(&status2, &lastErr); err != nil {
		t.Fatalf("select after failed: %v", err)
	}
	if status2 != "failed" {
		t.Fatalf("expected failed after max attempts, got %s", status2)
	}
	if lastErr == "" {
		t.Fatal("expected last_error set")
	}
}

func TestIntegration_A2AQueue_DropStaleQueueItems(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-a2a-stale")
	body := []byte(`{"test":true}`)

	// Insert a stale queued item by backdating enqueued_at
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO a2a_queue (id, workspace_id, priority, body, status, enqueued_at)
		VALUES (gen_random_uuid(), $1, $2, $3::jsonb, 'queued', now() - interval '10 minutes')
	`, wsID, PriorityTask, string(body)); err != nil {
		t.Fatalf("seed stale item: %v", err)
	}

	dropped, err := DropStaleQueueItems(context.Background(), wsID, 5)
	if err != nil {
		t.Fatalf("DropStaleQueueItems: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("expected 1 dropped, got %d", dropped)
	}

	var count int
	if err := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued'`, wsID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 queued items after drop, got %d", count)
	}
}

// ---------- A2A Queue Status endpoint integration tests ----------

func TestIntegration_A2AQueueStatus_CallerMatchesCallerID(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-status-caller-ws")
	callerID := seedWorkspace(t, conn, "test-2151-status-caller")
	body := []byte(`{"test":true}`)
	qid := seedA2AQueueItem(t, conn, wsID, callerID, PriorityTask, body, "queued")

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "queue_id", Value: qid}}
	c.Request.Header.Set("X-Workspace-ID", callerID)
	wh.GetA2AQueueStatus(c)

	if w.Code != http.StatusOK {
		t.Fatalf("GetA2AQueueStatus returned %d, want 200", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["queue_id"] != qid {
		t.Fatalf("expected queue_id %s, got %v", qid, resp["queue_id"])
	}
	// Verify sensitive fields excluded
	if _, ok := resp["body"]; ok {
		t.Fatal("response should not include body")
	}
	if _, ok := resp["caller_id"]; ok {
		t.Fatal("response should not include caller_id")
	}
}

func TestIntegration_A2AQueueStatus_CallerMatchesWorkspaceID(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-status-ws")
	callerID := seedWorkspace(t, conn, "test-2151-status-ws-caller")
	body := []byte(`{"test":true}`)
	qid := seedA2AQueueItem(t, conn, wsID, callerID, PriorityTask, body, "queued")

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "queue_id", Value: qid}}
	c.Request.Header.Set("X-Workspace-ID", wsID)
	wh.GetA2AQueueStatus(c)

	if w.Code != http.StatusOK {
		t.Fatalf("GetA2AQueueStatus returned %d, want 200", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["queue_id"] != qid {
		t.Fatalf("expected queue_id %s, got %v", qid, resp["queue_id"])
	}
}

func TestIntegration_A2AQueueStatus_OrgTokenBypass(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-status-org")
	callerID := seedWorkspace(t, conn, "test-2151-status-org-caller")
	body := []byte(`{"test":true}`)
	qid := seedA2AQueueItem(t, conn, wsID, callerID, PriorityTask, body, "queued")

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "queue_id", Value: qid}}
	c.Set("org_token_id", "test-org-token")
	wh.GetA2AQueueStatus(c)

	if w.Code != http.StatusOK {
		t.Fatalf("GetA2AQueueStatus returned %d, want 200", w.Code)
	}
}

func TestIntegration_A2AQueueStatus_MismatchedCaller(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-status-mismatch-ws")
	callerID := seedWorkspace(t, conn, "test-2151-status-mismatch-caller")
	otherWS := seedWorkspace(t, conn, "test-2151-status-mismatch-other")
	body := []byte(`{"test":true}`)
	qid := seedA2AQueueItem(t, conn, wsID, callerID, PriorityTask, body, "queued")

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "queue_id", Value: qid}}
	c.Request.Header.Set("X-Workspace-ID", otherWS)
	wh.GetA2AQueueStatus(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("GetA2AQueueStatus returned %d, want 403", w.Code)
	}
}

func TestIntegration_A2AQueueStatus_MissingIdentity(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-status-no-id")
	callerID := seedWorkspace(t, conn, "test-2151-status-no-id-caller")
	body := []byte(`{"test":true}`)
	qid := seedA2AQueueItem(t, conn, wsID, callerID, PriorityTask, body, "queued")

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "queue_id", Value: qid}}
	// No X-Workspace-ID, no org_token_id
	wh.GetA2AQueueStatus(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("GetA2AQueueStatus returned %d, want 401", w.Code)
	}
}

func TestIntegration_A2AQueueStatus_NonExistentQueueID(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-status-missing")
	callerID := seedWorkspace(t, conn, "test-2151-status-missing-caller")

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "queue_id", Value: uuid.New().String()}}
	c.Request.Header.Set("X-Workspace-ID", callerID)
	wh.GetA2AQueueStatus(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GetA2AQueueStatus returned %d, want 404", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if retryable, _ := resp["retryable"].(bool); !retryable {
		t.Fatalf("expected retryable=true for missing row, got %v", resp)
	}
}
