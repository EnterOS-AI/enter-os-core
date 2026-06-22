//go:build integration
// +build integration

// schedules_integration_test.go — REAL Postgres integration tests for
// the /workspaces/:id/schedules surface (handlers/schedules.go).
//
// Mirrors pending_uploads_integration_test.go /
// delegation_ledger_integration_test.go. Unit tests in schedules_test.go
// pin the SQL shape (sqlmock); these tests pin the OBSERVABLE row state
// against real Postgres, including:
//   - Create / List / Update / Delete round-trip
//   - Update recomputes next_run_at when cron_expr or timezone changes
//   - Update / Delete with wrong-workspace ID → 404 (IDOR protection, issue #113)
//   - RunNow returns the stored prompt verbatim (no A2A fire)
//   - History reads activity_logs filtered by request_body->>'schedule_id'
//   - Health (self-call) returns only health fields (no prompt, no cron_expr)
//
// Run with:
//
//	docker run --rm -d --name pg-integration \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	psql ... < workspace-server/migrations/001_workspaces.sql
//	psql ... < workspace-server/migrations/009_activity_logs.sql
//	psql ... < workspace-server/migrations/015_workspace_schedules.sql
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_Schedules -v

package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

func integrationDB_Schedules(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping (local devs: see file header)")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// Wipe in FK order: activity_logs first (references workspaces), then
	// workspace_schedules (references workspaces), then workspaces.
	for _, stmt := range []string{
		`DELETE FROM activity_logs WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'integ-sch-%')`,
		`DELETE FROM workspace_schedules WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'integ-sch-%')`,
		`DELETE FROM workspaces WHERE name LIKE 'integ-sch-%'`,
	} {
		if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("cleanup %q: %v", stmt, err)
		}
	}
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() {
		conn.ExecContext(context.Background(), `DELETE FROM activity_logs WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'integ-sch-%')`)
		conn.ExecContext(context.Background(), `DELETE FROM workspace_schedules WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'integ-sch-%')`)
		conn.ExecContext(context.Background(), `DELETE FROM workspaces WHERE name LIKE 'integ-sch-%'`)
		db.DB = prev
		conn.Close()
	})
	return conn
}

func seedWorkspace_Schedules(t *testing.T, conn *sql.DB, id string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status) VALUES ($1, $2, 'online')`,
		id, "integ-sch-"+id); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
}

// seedActivityLog_Schedules inserts a cron_run row directly so the
// History endpoint can find it via request_body->>'schedule_id'.
func seedActivityLog_Schedules(t *testing.T, conn *sql.DB, workspaceID, scheduleID string, status string, when time.Time) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO activity_logs (workspace_id, activity_type, request_body, status, duration_ms, created_at)
		 VALUES ($1, 'cron_run', jsonb_build_object('schedule_id', $2::text), $3, 100, $4)`,
		workspaceID, scheduleID, status, when); err != nil {
		t.Fatalf("seed activity_log: %v", err)
	}
}

// doPost is a tiny helper that fires Create against a fresh gin context.
func doPost_SchedulesCreate(t *testing.T, h *ScheduleHandler, workspaceID string, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+workspaceID+"/schedules", bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Create(c)
	return w
}

func doPatch_SchedulesUpdate(t *testing.T, h *ScheduleHandler, workspaceID, scheduleID, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}, {Key: "scheduleId", Value: scheduleID}}
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+workspaceID+"/schedules/"+scheduleID, bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Update(c)
	return w
}

// TestIntegration_Schedules_CRUDRunHistoryHealth_RoundTrip is the main
// regression gate for the schedules surface end-to-end.
func TestIntegration_Schedules_CRUDRunHistoryHealth_RoundTrip(t *testing.T) {
	conn := integrationDB_Schedules(t)
	handler := NewScheduleHandler()

	wsA := integUUID("integ-sch-ws-a")
	wsB := integUUID("integ-sch-ws-b")
	seedWorkspace_Schedules(t, conn, wsA)
	seedWorkspace_Schedules(t, conn, wsB)

	// --- Case 1: CREATE inserts a row with computed next_run_at ---
	w := doPost_SchedulesCreate(t, handler, wsA,
		`{"name":"daily-backup","cron_expr":"0 3 * * *","timezone":"UTC","prompt":"run backup"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("CREATE: status want 201, got %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		ID        string    `json:"id"`
		Status    string    `json:"status"`
		NextRunAt time.Time `json:"next_run_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("CREATE: parse: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CREATE: id empty in response")
	}
	// next_run_at must be > now (a future 3am UTC time).
	if !created.NextRunAt.After(time.Now().Add(-1 * time.Minute)) {
		t.Errorf("CREATE: next_run_at want in future, got %v", created.NextRunAt)
	}
	// Verify the row in DB has source='runtime' (issue #24).
	var source string
	if err := conn.QueryRowContext(context.Background(),
		`SELECT source FROM workspace_schedules WHERE id = $1`, created.ID).Scan(&source); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if source != "runtime" {
		t.Errorf("CREATE: source in DB want runtime, got %q", source)
	}

	// --- Case 2: LIST returns the row, plus only rows for wsA ---
	w = httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsA+"/schedules", nil)
	handler.List(c)
	if w.Code != http.StatusOK {
		t.Fatalf("LIST: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	var listed []ScheduleResponse
	json.Unmarshal(w.Body.Bytes(), &listed)
	if len(listed) != 1 {
		t.Errorf("LIST: want 1 schedule for wsA, got %d", len(listed))
	}
	if len(listed) > 0 && listed[0].ID != created.ID {
		t.Errorf("LIST: id want %q, got %q", created.ID, listed[0].ID)
	}
	if len(listed) > 0 && listed[0].Prompt != "run backup" {
		t.Errorf("LIST: prompt want %q, got %q", "run backup", listed[0].Prompt)
	}

	// --- Case 3: UPDATE with NEW cron_expr recomputes next_run_at ---
	// Read the original next_run_at, then PATCH with a different cron.
	var origNextRun time.Time
	if err := conn.QueryRowContext(context.Background(),
		`SELECT next_run_at FROM workspace_schedules WHERE id = $1`, created.ID).Scan(&origNextRun); err != nil {
		t.Fatalf("read orig next_run_at: %v", err)
	}
	// Pick a cron that lands at a noticeably different time. "0 5 * * *" = 5am UTC.
	w = doPatch_SchedulesUpdate(t, handler, wsA, created.ID,
		`{"cron_expr":"0 5 * * *"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("UPDATE cron: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	var newNextRun time.Time
	if err := conn.QueryRowContext(context.Background(),
		`SELECT next_run_at FROM workspace_schedules WHERE id = $1`, created.ID).Scan(&newNextRun); err != nil {
		t.Fatalf("read new next_run_at: %v", err)
	}
	// The UPDATE must have RECOMPUTED next_run_at from the new cron. We must NOT
	// assert newNextRun.After(origNextRun): "0 3" and "0 5" are daily crons whose
	// next-occurrence wraps every 24h, so whenever this test runs in the
	// 03:00–05:00 UTC window the 3am schedule has already rolled to *tomorrow*
	// while the 5am schedule is still *today*, making the 5am next-run EARLIER
	// than the 3am next-run and inverting any "strictly after" ordering. Instead
	// assert the deterministic invariants that hold at every wall-clock time:
	//  (a) next_run_at actually changed, and
	//  (b) it now lands at exactly 05:00 UTC, proving the new cron was applied.
	if newNextRun.Equal(origNextRun) {
		t.Errorf("UPDATE cron: next_run_at should have been recomputed (orig=%v new=%v)", origNextRun, newNextRun)
	}
	nrUTC := newNextRun.UTC()
	if nrUTC.Hour() != 5 || nrUTC.Minute() != 0 || nrUTC.Second() != 0 {
		t.Errorf("UPDATE cron: next_run_at want 05:00:00 UTC (new cron \"0 5 * * *\"), got %v", nrUTC)
	}

	// --- Case 4: UPDATE with NEW timezone also recomputes next_run_at ---
	w = doPatch_SchedulesUpdate(t, handler, wsA, created.ID,
		`{"timezone":"America/Los_Angeles"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("UPDATE tz: status want 200, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 5: UPDATE with INVALID timezone → 400, DB unchanged ---
	var beforeTZ string
	conn.QueryRowContext(context.Background(),
		`SELECT timezone FROM workspace_schedules WHERE id = $1`, created.ID).Scan(&beforeTZ)
	w = doPatch_SchedulesUpdate(t, handler, wsA, created.ID,
		`{"timezone":"Not/A/Zone"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("UPDATE bad tz: status want 400, got %d: %s", w.Code, w.Body.String())
	}
	var afterTZ string
	conn.QueryRowContext(context.Background(),
		`SELECT timezone FROM workspace_schedules WHERE id = $1`, created.ID).Scan(&afterTZ)
	if beforeTZ != afterTZ {
		t.Errorf("UPDATE bad tz mutated DB: before=%q after=%q", beforeTZ, afterTZ)
	}

	// --- Case 6: UPDATE on wrong-workspace ID (IDOR) → 404 ---
	// Try to update wsA's schedule through wsB's path.
	w = doPatch_SchedulesUpdate(t, handler, wsB, created.ID,
		`{"name":"hijacked"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("UPDATE wrong-ws: status want 404, got %d: %s", w.Code, w.Body.String())
	}
	// Verify name unchanged.
	var nameAfter string
	conn.QueryRowContext(context.Background(),
		`SELECT name FROM workspace_schedules WHERE id = $1`, created.ID).Scan(&nameAfter)
	if nameAfter == "hijacked" {
		t.Errorf("UPDATE wrong-ws: mutated DB through IDOR path (name=%q)", nameAfter)
	}

	// --- Case 7: RUNNOW returns the stored prompt, does NOT fire A2A ---
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}, {Key: "scheduleId", Value: created.ID}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsA+"/schedules/"+created.ID+"/run", nil)
	handler.RunNow(c)
	if w.Code != http.StatusOK {
		t.Fatalf("RUNNOW: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	var runNow struct {
		Status     string `json:"status"`
		WorkspaceID string `json:"workspace_id"`
		Prompt     string `json:"prompt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &runNow); err != nil {
		t.Fatalf("RUNNOW: parse: %v", err)
	}
	if runNow.Status != "fired" {
		t.Errorf("RUNNOW: status want fired, got %q", runNow.Status)
	}
	if runNow.Prompt != "run backup" {
		t.Errorf("RUNNOW: prompt want %q, got %q", "run backup", runNow.Prompt)
	}
	// Verify the prompt in the DB is unchanged (RunNow is a read).
	var promptAfter string
	conn.QueryRowContext(context.Background(),
		`SELECT prompt FROM workspace_schedules WHERE id = $1`, created.ID).Scan(&promptAfter)
	if promptAfter != "run backup" {
		t.Errorf("RUNNOW: mutated prompt in DB (got %q)", promptAfter)
	}

	// --- Case 8: HISTORY reads activity_logs filtered by request_body->>'schedule_id' ---
	// Seed two activity_log rows: one for our schedule, one for a different schedule.
	// Plus a row for a different workspace that must NOT leak.
	seedActivityLog_Schedules(t, conn, wsA, created.ID, "ok", time.Now().Add(-2*time.Minute))
	seedActivityLog_Schedules(t, conn, wsA, created.ID, "error", time.Now().Add(-1*time.Minute))
	seedActivityLog_Schedules(t, conn, wsA, "different-schedule-id", "ok", time.Now().Add(-30*time.Second))
	seedActivityLog_Schedules(t, conn, wsB, created.ID, "ok", time.Now().Add(-15*time.Second)) // different ws

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}, {Key: "scheduleId", Value: created.ID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsA+"/schedules/"+created.ID+"/history", nil)
	handler.History(c)
	if w.Code != http.StatusOK {
		t.Fatalf("HISTORY: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	// Decode into a slice of generic history entries.
	var hist []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &hist); err != nil {
		t.Fatalf("HISTORY: parse: %v", err)
	}
	// Must have exactly 2 entries (the two for our schedule in wsA).
	if len(hist) != 2 {
		t.Errorf("HISTORY: want 2 entries for our schedule+wsA, got %d: %+v", len(hist), hist)
	}

	// --- Case 9: HEALTH (self-call) returns health fields only ---
	// The self-call path (callerID == workspaceID) is always allowed —
	// no CanCommunicate check fires, no token check fires.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsA+"/schedules/health", nil)
	c.Request.Header.Set("X-Workspace-ID", wsA) // self-call
	handler.Health(c)
	if w.Code != http.StatusOK {
		t.Fatalf("HEALTH self: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	var health []ScheduleHealthResponse
	json.Unmarshal(w.Body.Bytes(), &health)
	if len(health) != 1 {
		t.Errorf("HEALTH self: want 1 entry for wsA, got %d", len(health))
	}
	if len(health) > 0 {
		// Must NOT include Prompt or CronExpr (per the comment on
		// ScheduleHealthResponse — issue #249).
		rawJSON := w.Body.String()
		if bytes.Contains([]byte(rawJSON), []byte("run backup")) {
			t.Errorf("HEALTH self: response leaked prompt (issue #249)")
		}
		if bytes.Contains([]byte(rawJSON), []byte("cron_expr")) {
			t.Errorf("HEALTH self: response leaked cron_expr field (issue #249)")
		}
	}

	// --- Case 10: HEALTH missing X-Workspace-ID → 401 ---
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsA+"/schedules/health", nil)
	// no X-Workspace-ID header
	handler.Health(c)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("HEALTH anon: status want 401, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 11: DELETE removes the row ---
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}, {Key: "scheduleId", Value: created.ID}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsA+"/schedules/"+created.ID, nil)
	handler.Delete(c)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	// Verify row is gone.
	var n int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM workspace_schedules WHERE id = $1`, created.ID).Scan(&n); err != nil {
		t.Fatalf("verify delete: %v", err)
	}
	if n != 0 {
		t.Errorf("DELETE: row still in DB (count=%d)", n)
	}

	// --- Case 12: DELETE on already-deleted schedule → 404 ---
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsA}, {Key: "scheduleId", Value: created.ID}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsA+"/schedules/"+created.ID, nil)
	handler.Delete(c)
	if w.Code != http.StatusNotFound {
		t.Errorf("DELETE gone: status want 404, got %d: %s", w.Code, w.Body.String())
	}
}
