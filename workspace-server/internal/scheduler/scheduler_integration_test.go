//go:build integration
// +build integration

// scheduler_integration_test.go — REAL Postgres integration tests for the
// workspace-server cron scheduler firing loop. Regression coverage for
// molecule-core issue #2149 (filed under SOP rule internal#765).
//
// Run with:
//
//	docker run --rm -d --name pg-integration \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	# apply every migration up.sql / legacy .sql in lexicographic order
//	for f in $(ls workspace-server/migrations/*.sql | grep -v '\.down\.sql$' | sort); do \
//	  psql "postgres://postgres:test@localhost:55432/molecule?sslmode=disable" -f "$f"; done
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/scheduler/ -run '^TestIntegration_'
//
// CI: .gitea/workflows/handlers-postgres-integration.yml runs these on every
// PR/push that touches workspace-server/internal/scheduler/ (the
// `handlers-postgres` detect-changes profile was extended to include the
// scheduler package + this workflow file).
//
// Why these are NOT the existing sqlmock unit tests (scheduler_test.go)
// --------------------------------------------------------------------
// The strict-sqlmock unit tests pin which SQL statements fire — fast, no DB.
// But sqlmock CANNOT validate:
//   - the activity_logs `$3::jsonb` cast (#2026 wedge) — sqlmock never parses
//     the payload, so an invalid-UTF-8 jsonb body that wedges a real INSERT
//     looks "green" under mock.ExpectExec(`INSERT INTO activity_logs`).
//   - the ROW STATE after tick()/fireSchedule run: that last_run_at,
//     next_run_at, run_count, last_status actually landed on the row.
//   - sweepPhantomBusy's NOT IN (SELECT … activity_logs) subquery semantics
//     against real rows — it has no unit test at all (#2149).
//
// A SQL regression here = a fleet-wide silent cron outage (#85 ran 12h before
// detection). These tests boot a real Postgres, insert real rows, run the
// production tick()/sweepPhantomBusy, and SELECT the rows back to assert the
// observable end state — the gap sqlmock structurally cannot cover.
//
// Watch-fail intent: each test is written to FAIL on a regression of the
// behavior under test (e.g. drop the activity_logs INSERT, drop the
// write-back UPDATE, drop the UTF-8 sanitize, or break the phantom-busy
// subquery) and to PASS against the current-correct scheduler.go.

package scheduler

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	mdb "github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	_ "github.com/lib/pq"
)

// ── test doubles ──────────────────────────────────────────────────────────

// recordingProxy is an A2AProxy that records each fire and returns a
// configurable response. Used to assert that tick()/fireSchedule actually
// reached the A2A boundary for the due schedule.
type recordingProxy struct {
	status int
	body   []byte
	err    error

	fires       int
	lastBody    []byte
	lastCaller  string
	lastLogFlag bool
	lastWSID    string

	// enqueue tracking — the busy path calls EnqueueA2A instead of firing.
	enqueues    int
	lastEnqBody []byte
	lastEnqKey  string
	enqQueueID  string
	enqDepth    int
	enqErr      error
}

func (p *recordingProxy) ProxyA2ARequest(
	_ context.Context, workspaceID string, body []byte, callerID string, logActivity bool,
) (int, []byte, error) {
	p.fires++
	p.lastWSID = workspaceID
	p.lastBody = body
	p.lastCaller = callerID
	p.lastLogFlag = logActivity
	if p.err != nil {
		return 0, nil, p.err
	}
	return p.status, p.body, nil
}

// EnqueueA2A records the busy-path enqueue so tests can assert that a tick on a
// busy workspace was buffered (not fired, not skipped).
func (p *recordingProxy) EnqueueA2A(
	_ context.Context, workspaceID, callerID string, _ int, body []byte, _ string, idempotencyKey string, _ *time.Time,
) (string, int, error) {
	p.enqueues++
	p.lastWSID = workspaceID
	p.lastCaller = callerID
	p.lastEnqBody = body
	p.lastEnqKey = idempotencyKey
	if p.enqErr != nil {
		return "", 0, p.enqErr
	}
	if p.enqQueueID == "" {
		p.enqQueueID = "q-rec-1"
	}
	return p.enqQueueID, p.enqDepth, nil
}

// ── connection + fixture helpers ──────────────────────────────────────────

// integrationDB returns the configured integration-test connection or skips
// the test if INTEGRATION_DB_URL is unset. Hot-swaps the package-level
// mdb.DB so the production scheduler helpers (tick, fireSchedule,
// sweepPhantomBusy) operate on this connection; restores it via t.Cleanup.
//
// NOT SAFE FOR t.Parallel(): the package-global swap races across tests.
func integrationDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping (local devs: see file header)")
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
	// Clean slate. activity_logs + workspace_schedules cascade off workspaces,
	// but we DELETE explicitly (and in FK order) so a partial prior run can't
	// leave orphan rows that perturb the next test's assertions.
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	for _, q := range []string{
		`DELETE FROM activity_logs`,
		`DELETE FROM workspace_schedules`,
		`DELETE FROM workspaces`,
	} {
		if _, err := conn.ExecContext(cctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
	prev := mdb.DB
	mdb.DB = conn
	t.Cleanup(func() {
		mdb.DB = prev
		conn.Close()
	})
	return conn
}

// insertWorkspace inserts a workspace row and returns its UUID. active is the
// initial active_tasks value; status defaults to 'online' (valid workspace_status enum).
func insertWorkspace(t *testing.T, conn *sql.DB, name string, active int) string {
	t.Helper()
	var id string
	err := conn.QueryRowContext(context.Background(), `
		INSERT INTO workspaces (name, status, active_tasks, max_concurrent_tasks)
		VALUES ($1, 'online', $2, 1)
		RETURNING id
	`, name, active).Scan(&id)
	if err != nil {
		t.Fatalf("insertWorkspace(%s): %v", name, err)
	}
	return id
}

// insertSchedule inserts an enabled workspace_schedules row whose next_run_at
// is in the past (so tick() picks it up immediately) and returns its UUID.
func insertSchedule(t *testing.T, conn *sql.DB, wsID, name, cronExpr, prompt string) string {
	t.Helper()
	var id string
	err := conn.QueryRowContext(context.Background(), `
		INSERT INTO workspace_schedules
		    (workspace_id, name, cron_expr, timezone, prompt, enabled, next_run_at, source)
		VALUES ($1, $2, $3, 'UTC', $4, true, now() - interval '1 minute', 'runtime')
		RETURNING id
	`, wsID, name, cronExpr, prompt).Scan(&id)
	if err != nil {
		t.Fatalf("insertSchedule(%s): %v", name, err)
	}
	return id
}

type scheduleState struct {
	lastRunAt  sql.NullTime
	nextRunAt  sql.NullTime
	runCount   int
	lastStatus string
	lastError  string
}

func readScheduleState(t *testing.T, conn *sql.DB, id string) scheduleState {
	t.Helper()
	var st scheduleState
	var status, errStr sql.NullString
	err := conn.QueryRowContext(context.Background(), `
		SELECT last_run_at, next_run_at, run_count, last_status, last_error
		FROM workspace_schedules WHERE id = $1
	`, id).Scan(&st.lastRunAt, &st.nextRunAt, &st.runCount, &status, &errStr)
	if err != nil {
		t.Fatalf("readScheduleState(%s): %v", id, err)
	}
	st.lastStatus = status.String
	st.lastError = errStr.String
	return st
}

// ── TestIntegration_TickFiresAndWritesBack (#2149 core) ───────────────────
//
// Insert one due schedule, run tick() once, and assert the full firing
// loop landed against a REAL Postgres:
//   - the A2A proxy was invoked exactly once for the schedule's workspace
//   - the post-fire UPDATE wrote last_run_at (was NULL), advanced next_run_at
//     into the future, bumped run_count to 1, set last_status='ok'
//   - a cron_run activity_logs row was inserted with VALID jsonb request_body
//     (the `$3::jsonb` cast #2026 path) carrying the schedule metadata
//
// Regression watch-fail: if a refactor drops the write-back UPDATE, the
// activity_logs INSERT, or breaks the jsonb cast, this test fails where every
// sqlmock unit test stays green.
func TestIntegration_TickFiresAndWritesBack(t *testing.T) {
	conn := integrationDB(t)

	wsID := insertWorkspace(t, conn, "cron-fire-ws", 0)
	schedID := insertSchedule(t, conn, wsID, "hourly-audit", "0 * * * *", "run the hourly audit")

	proxy := &recordingProxy{
		status: 200,
		body:   []byte(`{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"done"}]},"id":"1"}`),
	}
	s := New(proxy, nil)
	s.tick(context.Background())

	// 1. A2A boundary reached exactly once for the right workspace.
	if proxy.fires != 1 {
		t.Fatalf("proxy fires = %d, want 1 (tick must fire the one due schedule)", proxy.fires)
	}
	if proxy.lastWSID != wsID {
		t.Errorf("proxy fired for workspace %q, want %q", proxy.lastWSID, wsID)
	}
	// Empty callerID = canvas-style (bypasses access control); logActivity=true.
	if proxy.lastCaller != "" {
		t.Errorf("callerID = %q, want empty (canvas-style scheduler fire)", proxy.lastCaller)
	}
	if !proxy.lastLogFlag {
		t.Error("logActivity flag = false, want true")
	}

	// 2. Row write-back.
	st := readScheduleState(t, conn, schedID)
	if !st.lastRunAt.Valid {
		t.Error("last_run_at is NULL after fire, want set (write-back UPDATE did not land)")
	}
	if !st.nextRunAt.Valid {
		t.Fatal("next_run_at is NULL after fire, want a future timestamp")
	}
	if !st.nextRunAt.Time.After(time.Now()) {
		t.Errorf("next_run_at = %v, want a time in the future (schedule would tight-loop otherwise)", st.nextRunAt.Time)
	}
	if st.runCount != 1 {
		t.Errorf("run_count = %d, want 1", st.runCount)
	}
	if st.lastStatus != "ok" {
		t.Errorf("last_status = %q, want \"ok\"", st.lastStatus)
	}

	// 3. activity_logs cron_run row with valid jsonb request_body.
	var actCount int
	var summary, status string
	var reqBody []byte
	err := conn.QueryRowContext(context.Background(), `
		SELECT count(*) OVER (), summary, status, request_body
		FROM activity_logs
		WHERE workspace_id = $1 AND activity_type = 'cron_run'
		LIMIT 1
	`, wsID).Scan(&actCount, &summary, &status, &reqBody)
	if err == sql.ErrNoRows {
		t.Fatal("no cron_run activity_logs row inserted after fire (#152/#2026 path missing)")
	}
	if err != nil {
		t.Fatalf("read activity_logs: %v", err)
	}
	if actCount != 1 {
		t.Errorf("cron_run activity_logs rows = %d, want 1", actCount)
	}
	if status != "ok" {
		t.Errorf("activity_logs.status = %q, want \"ok\"", status)
	}
	// request_body must be valid jsonb carrying the schedule_id — proves the
	// `$3::jsonb` cast accepted the payload (the #2026 wedge surface).
	var sid string
	if err := conn.QueryRowContext(context.Background(), `
		SELECT request_body->>'schedule_id'
		FROM activity_logs WHERE workspace_id = $1 AND activity_type = 'cron_run' LIMIT 1
	`, wsID).Scan(&sid); err != nil {
		t.Fatalf("request_body is not queryable jsonb: %v", err)
	}
	if sid != schedID {
		t.Errorf("activity_logs request_body->>'schedule_id' = %q, want %q", sid, schedID)
	}
}

// ── TestIntegration_InvalidUTF8PromptSanitizedIntoJsonb (#2026 / #2149) ────
//
// The agent-editable prompt can carry raw invalid-UTF-8 bytes. Postgres jsonb
// columns REJECT invalid UTF-8, which (pre-#2026) wedged the activity_logs
// INSERT and held the transaction open — stalling the whole scheduler.
// fireSchedule now sanitizeUTF8()s every string before the `$3::jsonb` insert.
//
// Postgres TEXT columns (workspace_schedules.prompt) also reject invalid UTF-8
// in a UTF-8 database, so we cannot INSERT the bad bytes through the fixture.
// Instead we insert a valid prompt, then call fireSchedule directly with a
// scheduleRow whose Prompt field contains the invalid bytes — this simulates
// the real regression path (e.g. truncation splitting a multi-byte rune, or
// an agent-edited template arriving via a path that bypasses DB validation).
//
// Assertions:
//   - the fire still completed (write-back UPDATE landed)
//   - the cron_run activity_logs row was inserted (the jsonb cast accepted
//     the SANITIZED payload — the INSERT did not wedge)
//   - the stored request_body is queryable jsonb (valid UTF-8 on disk)
//
// Watch-fail: remove the sanitizeUTF8() wrapping around the jsonb payload and
// this test fails on a real Postgres (INSERT errors / row absent), while the
// sqlmock unit test that only checks "an INSERT fired" stays green.
func TestIntegration_InvalidUTF8PromptSanitizedIntoJsonb(t *testing.T) {
	conn := integrationDB(t)

	wsID := insertWorkspace(t, conn, "utf8-ws", 0)
	// Insert with valid UTF-8 — Postgres TEXT rejects 0x80/0xff.
	schedID := insertSchedule(t, conn, wsID, "utf8-job", "0 * * * *", "valid prompt")

	// Prompt with invalid UTF-8: orphan continuation byte + bare 0xff.
	badPrompt := "audit \x80 report \xff end"
	row := scheduleRow{
		ID:          schedID,
		WorkspaceID: wsID,
		Name:        "utf8-job",
		CronExpr:    "0 * * * *",
		Timezone:    "UTC",
		Prompt:      badPrompt,
	}

	proxy := &recordingProxy{
		status: 200,
		body:   []byte(`{"result":{"kind":"message","parts":[{"kind":"text","text":"ok"}]}}`),
	}
	s := New(proxy, nil)
	s.fireSchedule(context.Background(), row)

	if proxy.fires != 1 {
		t.Fatalf("proxy fires = %d, want 1", proxy.fires)
	}

	// Write-back must have landed despite the bad prompt bytes.
	st := readScheduleState(t, conn, schedID)
	if st.runCount != 1 || st.lastStatus != "ok" {
		t.Errorf("post-fire state run_count=%d last_status=%q, want 1/\"ok\" "+
			"(invalid-UTF-8 prompt must not block the fire)", st.runCount, st.lastStatus)
	}

	// The cron_run activity_logs row MUST exist — proving the `$3::jsonb`
	// INSERT accepted the sanitized payload (did not wedge on invalid UTF-8).
	var n int
	if err := conn.QueryRowContext(context.Background(), `
		SELECT count(*) FROM activity_logs
		WHERE workspace_id = $1 AND activity_type = 'cron_run'
	`, wsID).Scan(&n); err != nil {
		t.Fatalf("count cron_run rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("cron_run activity_logs rows = %d, want 1 — the jsonb INSERT wedged "+
			"on invalid UTF-8 (the #2026 regression)", n)
	}

	// The stored prompt inside request_body must be queryable + valid UTF-8.
	var storedPrompt string
	if err := conn.QueryRowContext(context.Background(), `
		SELECT request_body->>'prompt'
		FROM activity_logs WHERE workspace_id = $1 AND activity_type = 'cron_run' LIMIT 1
	`, wsID).Scan(&storedPrompt); err != nil {
		t.Fatalf("request_body->>'prompt' not queryable jsonb: %v", err)
	}
	if storedPrompt == "" {
		t.Error("stored prompt is empty, want the sanitized prompt text")
	}
	// Round-trip through Postgres jsonb guarantees valid UTF-8; assert the
	// replacement character replaced the bad bytes rather than them surviving.
	for i := 0; i < len(storedPrompt); i++ {
		if storedPrompt[i] == 0x80 || storedPrompt[i] == 0xff {
			t.Fatalf("stored prompt still contains raw invalid byte 0x%x at %d", storedPrompt[i], i)
		}
	}
}

// ── TestIntegration_TickErrorStatusWriteBack (#2149) ──────────────────────
//
// When the A2A proxy returns a transport error, fireSchedule must still write
// back: last_status='error', last_error populated, next_run_at advanced (so
// the schedule does not get stuck re-firing), run_count bumped. Verifies the
// error path persists to a real row, not just that "an UPDATE fired".
func TestIntegration_TickErrorStatusWriteBack(t *testing.T) {
	conn := integrationDB(t)

	wsID := insertWorkspace(t, conn, "err-ws", 0)
	schedID := insertSchedule(t, conn, wsID, "err-job", "0 * * * *", "do work")

	proxy := &recordingProxy{err: context.DeadlineExceeded}
	s := New(proxy, nil)
	s.tick(context.Background())

	st := readScheduleState(t, conn, schedID)
	if st.lastStatus != "error" {
		t.Errorf("last_status = %q, want \"error\"", st.lastStatus)
	}
	if st.lastError == "" {
		t.Error("last_error is empty, want the proxy error text persisted (#152)")
	}
	if st.runCount != 1 {
		t.Errorf("run_count = %d, want 1 (run still counted on error)", st.runCount)
	}
	if !st.nextRunAt.Valid || !st.nextRunAt.Time.After(time.Now()) {
		t.Errorf("next_run_at not advanced to future on error path (= %v) — schedule would tight-loop", st.nextRunAt)
	}
	// The error activity_logs row must carry status='error' + error_detail.
	var status, errDetail string
	if err := conn.QueryRowContext(context.Background(), `
		SELECT status, COALESCE(error_detail,'') FROM activity_logs
		WHERE workspace_id = $1 AND activity_type = 'cron_run' LIMIT 1
	`, wsID).Scan(&status, &errDetail); err != nil {
		t.Fatalf("read error activity_logs: %v", err)
	}
	if status != "error" {
		t.Errorf("activity_logs.status = %q, want \"error\"", status)
	}
	if errDetail == "" {
		t.Error("activity_logs.error_detail empty on error fire, want the error message (#152)")
	}
}

// ── TestIntegration_SweepPhantomBusy (#2149 — no prior test) ──────────────
//
// sweepPhantomBusy resets active_tasks=0 for workspaces stuck busy with NO
// activity_logs row in the last phantomStaleThreshold window, and must LEAVE
// ALONE workspaces that have recent activity. The NOT IN (SELECT DISTINCT
// workspace_id FROM activity_logs WHERE created_at > now() - interval) subquery
// is exactly the kind of set-semantics that sqlmock cannot validate — there is
// no unit test for this method at all (#2149).
//
// Fixture:
//   - phantomWS: active_tasks=3, NO recent activity_log     → must reset to 0
//   - recentWS:  active_tasks=2, activity_log 1 min ago      → must stay at 2
//   - staleWS:   active_tasks=1, activity_log 30 min ago     → must reset to 0
//   - removedWS: active_tasks=4, status='removed', no activity → must stay (status guard)
//   - idleWS:    active_tasks=0                               → untouched (not >0)
//
// Watch-fail: break the subquery (e.g. drop the status!='removed' guard, or
// invert the NOT IN), and the asserted end-state diverges on a real Postgres.
func TestIntegration_SweepPhantomBusy(t *testing.T) {
	conn := integrationDB(t)

	phantomWS := insertWorkspace(t, conn, "phantom-ws", 3)
	recentWS := insertWorkspace(t, conn, "recent-ws", 2)
	staleWS := insertWorkspace(t, conn, "stale-ws", 1)
	idleWS := insertWorkspace(t, conn, "idle-ws", 0)

	// removedWS: busy but status='removed' — the sweep must skip it.
	var removedWS string
	if err := conn.QueryRowContext(context.Background(), `
		INSERT INTO workspaces (name, status, active_tasks, max_concurrent_tasks)
		VALUES ('removed-ws', 'removed', 4, 1) RETURNING id
	`).Scan(&removedWS); err != nil {
		t.Fatalf("insert removedWS: %v", err)
	}

	// recentWS has a fresh activity_log (1 min ago → inside the 10-min window).
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO activity_logs (workspace_id, activity_type, status, created_at)
		VALUES ($1, 'a2a_receive', 'ok', now() - interval '1 minute')
	`, recentWS); err != nil {
		t.Fatalf("insert recent activity_log: %v", err)
	}
	// staleWS has only an OLD activity_log (30 min ago → outside the window).
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO activity_logs (workspace_id, activity_type, status, created_at)
		VALUES ($1, 'a2a_receive', 'ok', now() - interval '30 minutes')
	`, staleWS); err != nil {
		t.Fatalf("insert stale activity_log: %v", err)
	}

	s := New(nil, nil)
	s.sweepPhantomBusy(context.Background())

	active := func(id string) int {
		var n int
		if err := conn.QueryRowContext(context.Background(),
			`SELECT active_tasks FROM workspaces WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("read active_tasks(%s): %v", id, err)
		}
		return n
	}

	if got := active(phantomWS); got != 0 {
		t.Errorf("phantomWS active_tasks = %d, want 0 (busy + no recent activity → must be swept)", got)
	}
	if got := active(staleWS); got != 0 {
		t.Errorf("staleWS active_tasks = %d, want 0 (only stale activity → must be swept)", got)
	}
	if got := active(recentWS); got != 2 {
		t.Errorf("recentWS active_tasks = %d, want 2 (recent activity → must NOT be swept)", got)
	}
	if got := active(removedWS); got != 4 {
		t.Errorf("removedWS active_tasks = %d, want 4 (status='removed' → sweep must skip it)", got)
	}
	if got := active(idleWS); got != 0 {
		t.Errorf("idleWS active_tasks = %d, want 0 (was never busy)", got)
	}

	// The swept rows must also have current_task cleared.
	var ct string
	if err := conn.QueryRowContext(context.Background(),
		`SELECT COALESCE(current_task,'') FROM workspaces WHERE id = $1`, phantomWS).Scan(&ct); err != nil {
		t.Fatalf("read current_task: %v", err)
	}
	if ct != "" {
		t.Errorf("phantomWS current_task = %q, want empty after sweep", ct)
	}
}

// ── TestIntegration_NativeSchedulerSkipAdvancesNextRunAt (#2149) ──────────
//
// When a workspace's adapter owns scheduling natively, tick() must SKIP the
// fire but still advance next_run_at (so the row doesn't tight-loop on every
// poll) — observability (next_run_at) is preserved while the fire is dropped.
// Asserts the native-skip UPDATE landed on a real row and the proxy was NOT
// invoked. This is the native-skip UPDATE path #2149 calls out — sqlmock can
// only assert an UPDATE fired, not that next_run_at moved forward.
func TestIntegration_NativeSchedulerSkipAdvancesNextRunAt(t *testing.T) {
	conn := integrationDB(t)

	wsID := insertWorkspace(t, conn, "native-ws", 0)
	schedID := insertSchedule(t, conn, wsID, "native-job", "0 * * * *", "native run")

	// Capture the pre-tick next_run_at (it is in the past by construction).
	before := readScheduleState(t, conn, schedID)
	if !before.nextRunAt.Valid || before.nextRunAt.Time.After(time.Now()) {
		t.Fatalf("precondition: next_run_at should start in the past, got %v", before.nextRunAt)
	}

	proxy := &recordingProxy{status: 200, body: []byte(`{}`)}
	s := New(proxy, nil)
	// Every workspace reports native scheduling → fire must be skipped.
	s.SetNativeSchedulerCheck(func(string) bool { return true })
	s.tick(context.Background())

	if proxy.fires != 0 {
		t.Errorf("proxy fires = %d, want 0 (native-scheduler workspace must NOT fire)", proxy.fires)
	}

	after := readScheduleState(t, conn, schedID)
	if !after.nextRunAt.Valid || !after.nextRunAt.Time.After(time.Now()) {
		t.Errorf("next_run_at = %v, want advanced into the future (native-skip UPDATE must still run)", after.nextRunAt)
	}
	// Skip path does NOT bump run_count or write last_run_at (no fire happened).
	if after.runCount != 0 {
		t.Errorf("run_count = %d, want 0 (skip must not count as a run)", after.runCount)
	}
	if after.lastRunAt.Valid {
		t.Error("last_run_at set on native-skip, want NULL (no fire occurred)")
	}
}
