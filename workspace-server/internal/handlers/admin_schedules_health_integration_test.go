//go:build integration
// +build integration

// admin_schedules_health_integration_test.go — REAL Postgres integration
// tests for GET /admin/schedules/health (handlers/admin_schedules_health.go).
//
// Mirrors pending_uploads_integration_test.go /
// delegation_ledger_integration_test.go. Unit tests in
// admin_schedules_health_test.go pin the SQL shape + classification
// function; these tests pin the OBSERVABLE row state end-to-end:
//   - admin view joins workspace_schedules with non-removed workspaces
//   - status classifies as "never_run" / "ok" / "stale" against real
//     last_run_at values + real cron intervals
//   - removed workspaces are excluded from the join
//
// Run with:
//
//	docker run --rm -d --name pg-integration \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	psql ... < workspace-server/migrations/001_workspaces.sql
//	psql ... < workspace-server/migrations/015_workspace_schedules.sql
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_AdminSchedulesHealth -v

package handlers

import (
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

func integrationDB_AdminSchedulesHealth(t *testing.T) *sql.DB {
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
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM workspace_schedules WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'integ-ash-%')`); err != nil {
		t.Fatalf("cleanup schedules: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM workspaces WHERE name LIKE 'integ-ash-%'`); err != nil {
		t.Fatalf("cleanup workspaces: %v", err)
	}
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() {
		conn.ExecContext(context.Background(), `DELETE FROM workspace_schedules WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'integ-ash-%')`)
		conn.ExecContext(context.Background(), `DELETE FROM workspaces WHERE name LIKE 'integ-ash-%'`)
		db.DB = prev
		conn.Close()
	})
	return conn
}

func seedWorkspace_AdminSchedulesHealth(t *testing.T, conn *sql.DB, id string, status string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status) VALUES ($1, $2, $3)`,
		id, "integ-ash-"+id, status); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
}

// seedSchedule_AdminSchedulesHealth inserts a workspace_schedules row
// directly (bypassing the handler) so the test can pin last_run_at to
// any value, including backdated for the "stale" classification case.
func seedSchedule_AdminSchedulesHealth(t *testing.T, conn *sql.DB, workspaceID, name, cronExpr, tz string, lastRunAt *time.Time) {
	t.Helper()
	var lastRunArg interface{} = lastRunAt
	// next_run_at = now() so the row is "in-window" for the scheduler.
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspace_schedules
		    (workspace_id, name, cron_expr, timezone, prompt, enabled, last_run_at, next_run_at, run_count, last_status)
		 VALUES ($1, $2, $3, $4, 'test prompt', true, $5, now(), 1, 'ok')`,
		workspaceID, name, cronExpr, tz, lastRunArg); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
}

// TestIntegration_AdminSchedulesHealth_ClassifiesRows pins the three
// classification branches against real DB rows: never_run (last_run_at
// IS NULL), ok (recent last_run_at), stale (last_run_at well past 2×
// cron interval). Also asserts the join excludes removed workspaces.
func TestIntegration_AdminSchedulesHealth_ClassifiesRows(t *testing.T) {
	conn := integrationDB_AdminSchedulesHealth(t)
	handler := NewAdminSchedulesHealthHandler()

	// Two visible workspaces + one removed (must NOT appear in results).
	// IDs are derived from the human-readable name via integUUID so the
	// schema (UUID-typed id column) is satisfied while failure logs still
	// print a recognizable name.
	wsOK := integUUID("integ-ash-ws-ok")
	wsStale := integUUID("integ-ash-ws-stale")
	wsRemoved := integUUID("integ-ash-ws-removed")
	seedWorkspace_AdminSchedulesHealth(t, conn, wsOK, "online")
	seedWorkspace_AdminSchedulesHealth(t, conn, wsStale, "online")
	seedWorkspace_AdminSchedulesHealth(t, conn, wsRemoved, "removed")

	// --- never_run: last_run_at IS NULL ---
	// (Don't pass lastRunAt; seedSchedule inserts NULL by default if
	// we pass a nil pointer. Already handled by lastRunArg interface{}.)
	seedSchedule_AdminSchedulesHealth(t, conn, wsOK, "never_run_schedule", "0 * * * *", "UTC", nil)

	// --- ok: last_run_at within 2× cron interval (every-15-min → threshold ~30min) ---
	okLast := time.Now().Add(-2 * time.Minute) // 2 min ago, well within 30 min
	seedSchedule_AdminSchedulesHealth(t, conn, wsOK, "ok_schedule", "*/15 * * * *", "UTC", &okLast)

	// --- stale: last_run_at way past 2× cron interval (every-15-min, ran 1h ago) ---
	staleLast := time.Now().Add(-1 * time.Hour) // 1h ago, well past 30 min
	seedSchedule_AdminSchedulesHealth(t, conn, wsStale, "stale_schedule", "*/15 * * * *", "UTC", &staleLast)

	// --- removed workspace's schedule must NOT appear ---
	// Add a schedule to the removed workspace to prove it's filtered out.
	seedSchedule_AdminSchedulesHealth(t, conn, wsRemoved, "removed_schedule", "0 * * * *", "UTC", nil)

	// --- Call the handler ---
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/schedules/health", nil)
	handler.Health(c)
	if w.Code != http.StatusOK {
		t.Fatalf("Health: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	var got []adminScheduleHealth
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("Health: parse: %v", err)
	}

	// Index by schedule name for assertions.
	byName := map[string]adminScheduleHealth{}
	for _, e := range got {
		byName[e.ScheduleName] = e
	}

	// --- Assert: never_run classification ---
	if e, ok := byName["never_run_schedule"]; !ok {
		t.Errorf("never_run_schedule missing from response (got %d entries: %+v)", len(got), byName)
	} else if e.Status != "never_run" {
		t.Errorf("never_run_schedule: status want never_run, got %q", e.Status)
	}

	// --- Assert: ok classification ---
	if e, ok := byName["ok_schedule"]; !ok {
		t.Errorf("ok_schedule missing from response")
	} else if e.Status != "ok" {
		t.Errorf("ok_schedule: status want ok, got %q (last_run_at=%v threshold=%ds)",
			e.Status, e.LastRunAt, e.StaleThresholdSeconds)
	}

	// --- Assert: stale classification ---
	if e, ok := byName["stale_schedule"]; !ok {
		t.Errorf("stale_schedule missing from response")
	} else if e.Status != "stale" {
		t.Errorf("stale_schedule: status want stale, got %q (last_run_at=%v threshold=%ds)",
			e.Status, e.LastRunAt, e.StaleThresholdSeconds)
	}

	// --- Assert: removed workspace is filtered out ---
	if _, ok := byName["removed_schedule"]; ok {
		t.Errorf("removed_schedule should be filtered out (workspace status=removed)")
	}

	// --- Assert: stale threshold is 2× cron interval. */15 fires every
	// 15 minutes (900s) so the threshold is 2× 900s = 1800s. (The previous
	// version of this assertion expected ~3600 — the original test author
	// mis-read `*/15` as "every 30 minutes", which is two off-by-one errors
	// stacked. Handler computes the threshold from the next-two-fires gap
	// in budget_periods.go, which is correct.) ---
	if e, ok := byName["ok_schedule"]; ok {
		// Allow ±10s slack for runtime compute jitter (scheduler.ComputeNextRun
		// is a pure function so it's deterministic, but the "now" snapshot is
		// taken at test time, not at seed time, so the precise gap can shift
		// by a couple of seconds if the test runs across a 15-min boundary).
		if e.StaleThresholdSeconds < 1790 || e.StaleThresholdSeconds > 1810 {
			t.Errorf("ok_schedule: stale_threshold_seconds want ~1800 (2× 15min), got %d", e.StaleThresholdSeconds)
		}
	}
}
