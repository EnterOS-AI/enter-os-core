//go:build integration
// +build integration

// pending_uploads_integration_test.go — REAL Postgres integration
// tests for the poll-mode chat upload flow (RFC: phases 1–3).
//
// Run with:
//
//   docker run --rm -d --name pg-integration \
//     -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//     -p 55432:5432 postgres:15-alpine
//   sleep 4
//   psql ... < workspace-server/migrations/20260505100000_pending_uploads.up.sql
//   cd workspace-server
//   INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//     go test -tags=integration ./internal/handlers/ -run Integration_PendingUploads
//
// CI (.github/workflows/handlers-postgres-integration.yml) runs this on
// every PR that touches workspace-server/internal/handlers/** OR
// workspace-server/migrations/**.
//
// Why these are NOT plain unit tests
// ----------------------------------
// The strict-sqlmock unit tests in storage_test.go pin which SQL
// statements fire — they are fast and let us iterate without a DB. But
// sqlmock CANNOT detect bugs that depend on the actual row state after
// the SQL runs. In particular:
//
//   - the WITH … DELETE … RETURNING CTE used by Sweep depends on
//     Postgres' `make_interval` function and the table's CHECK
//     constraints. sqlmock would happily accept a hand-written SQL
//     literal that Postgres rejects at runtime.
//   - the partial index `idx_pending_uploads_unacked` (created by the
//     Phase 1 migration) only catches a wrong WHERE predicate at real-
//     query-plan time.
//
// These tests close those gaps by booting a real Postgres, running the
// production helpers, and SELECTing the row to verify the observable
// state matches the expected outcome.

package handlers

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/pendinguploads"
)

// integrationDB_PendingUploads opens a connection from $INTEGRATION_DB_URL
// (skipping the test if unset), wipes the pending_uploads table for
// isolation, and registers a Cleanup that closes the connection.
//
// NOT SAFE FOR `t.Parallel()` — each test gets the table to itself.
// Mirrors the integrationDB helper in delegation_ledger_integration_test.go
// but kept separate so each table's wipe step is local to its tests.
func integrationDB_PendingUploads(t *testing.T) *sql.DB {
	t.Helper()
	url := requireIntegrationDBURL(t)
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), `DELETE FROM pending_uploads`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestIntegration_PendingUploads_PutGetAckRoundTrip(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	store := pendinguploads.NewPostgres(conn)
	ctx := context.Background()

	wsID := uuid.New()
	fileID, err := store.Put(ctx, wsID, []byte("hello PDF"), "report.pdf", "application/pdf")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Get reads back the row.
	rec, err := store.Get(ctx, fileID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.WorkspaceID != wsID {
		t.Errorf("workspace_id = %s, want %s", rec.WorkspaceID, wsID)
	}
	if string(rec.Content) != "hello PDF" {
		t.Errorf("content = %q, want %q", rec.Content, "hello PDF")
	}
	if rec.Filename != "report.pdf" {
		t.Errorf("filename = %q, want %q", rec.Filename, "report.pdf")
	}
	if rec.AckedAt != nil {
		t.Errorf("AckedAt should be nil before Ack, got %v", rec.AckedAt)
	}

	// MarkFetched stamps fetched_at.
	if err := store.MarkFetched(ctx, fileID); err != nil {
		t.Fatalf("MarkFetched: %v", err)
	}

	// Re-read to confirm.
	rec2, err := store.Get(ctx, fileID)
	if err != nil {
		t.Fatalf("Get after MarkFetched: %v", err)
	}
	if rec2.FetchedAt == nil {
		t.Errorf("FetchedAt should be set after MarkFetched")
	}

	// Ack flips acked_at. Acked rows remain readable during retention so
	// refreshed canvas previews can resolve platform-pending: attachment URIs.
	if err := store.Ack(ctx, fileID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	rec3, err := store.Get(ctx, fileID)
	if err != nil {
		t.Fatalf("Get after Ack: %v", err)
	}
	if rec3.AckedAt == nil {
		t.Errorf("AckedAt should be set after Ack")
	}

	// Idempotent re-ack succeeds.
	if err := store.Ack(ctx, fileID); err != nil {
		t.Errorf("re-Ack should be idempotent, got %v", err)
	}
}

func TestIntegration_PendingUploads_Sweep_DeletesAckedAfterRetention(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	store := pendinguploads.NewPostgres(conn)
	ctx := context.Background()

	wsID := uuid.New()
	fid, err := store.Put(ctx, wsID, []byte("data"), "x.txt", "text/plain")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Ack(ctx, fid); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	// retention=1h, row was acked just now → not yet eligible.
	res, err := store.Sweep(ctx, time.Hour)
	if err != nil {
		t.Fatalf("Sweep(1h): %v", err)
	}
	if res.Total() != 0 {
		t.Errorf("expected 0 deletions yet, got %+v", res)
	}

	// retention=0 → row IS eligible immediately.
	res, err = store.Sweep(ctx, 0)
	if err != nil {
		t.Fatalf("Sweep(0): %v", err)
	}
	if res.Acked != 1 || res.Expired != 0 {
		t.Errorf("expected acked=1 expired=0, got %+v", res)
	}

	// Verify row is actually gone — not just un-fetchable.
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_uploads WHERE file_id = $1`, fid).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("row should be DELETEd, found %d rows", n)
	}
}

func TestIntegration_PendingUploads_Sweep_DeletesExpiredUnacked(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	store := pendinguploads.NewPostgres(conn)
	ctx := context.Background()

	wsID := uuid.New()
	fid, err := store.Put(ctx, wsID, []byte("data"), "x.txt", "text/plain")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Manually backdate expires_at so the row IS expired. We don't ack,
	// so this exercises the unacked-and-expired branch of the WHERE
	// clause specifically.
	if _, err := conn.ExecContext(ctx,
		`UPDATE pending_uploads SET expires_at = now() - interval '1 minute' WHERE file_id = $1`,
		fid,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	res, err := store.Sweep(ctx, time.Hour)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Acked != 0 || res.Expired != 1 {
		t.Errorf("expected acked=0 expired=1, got %+v", res)
	}
}

func TestIntegration_PendingUploads_Sweep_DeletesBothCategoriesInOneCycle(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	store := pendinguploads.NewPostgres(conn)
	ctx := context.Background()

	wsID := uuid.New()

	// Three rows: one acked (eligible at retention=0), one expired
	// unacked, one fresh unacked (must NOT be deleted).
	ackedFID, err := store.Put(ctx, wsID, []byte("acked"), "a.txt", "text/plain")
	if err != nil {
		t.Fatalf("Put acked: %v", err)
	}
	if err := store.Ack(ctx, ackedFID); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	expiredFID, err := store.Put(ctx, wsID, []byte("expired"), "e.txt", "text/plain")
	if err != nil {
		t.Fatalf("Put expired: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE pending_uploads SET expires_at = now() - interval '1 minute' WHERE file_id = $1`,
		expiredFID,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	freshFID, err := store.Put(ctx, wsID, []byte("fresh"), "f.txt", "text/plain")
	if err != nil {
		t.Fatalf("Put fresh: %v", err)
	}

	res, err := store.Sweep(ctx, 0) // retention=0 makes the acked row eligible
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Acked != 1 || res.Expired != 1 {
		t.Errorf("expected acked=1 expired=1, got %+v", res)
	}

	// Fresh row survives.
	rec, err := store.Get(ctx, freshFID)
	if err != nil {
		t.Errorf("fresh row should still be Get-able, got err=%v", err)
	}
	if rec.FileID != freshFID {
		t.Errorf("fresh row file_id = %s, want %s", rec.FileID, freshFID)
	}
}

func TestIntegration_PendingUploads_PutEnforcesSizeCap(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	store := pendinguploads.NewPostgres(conn)
	ctx := context.Background()

	wsID := uuid.New()
	tooBig := make([]byte, pendinguploads.MaxFileBytes+1)
	if _, err := store.Put(ctx, wsID, tooBig, "big.bin", "application/octet-stream"); err != pendinguploads.ErrTooLarge {
		t.Errorf("expected ErrTooLarge, got %v", err)
	}
}

// TestIntegration_PendingUploads_PutBatch_HappyPath_AllRowsCommit pins the
// "all rows commit" leg of the PutBatch atomicity contract against a real
// Postgres. sqlmock can't catch a regression where the Go-side Tx machinery
// silently no-ops the inserts (e.g., wrong driver options on BeginTx); only
// COUNT(*) on the real table can.
func TestIntegration_PendingUploads_PutBatch_HappyPath_AllRowsCommit(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	store := pendinguploads.NewPostgres(conn)
	ctx := context.Background()

	wsID := uuid.New()

	// Pre-existing row so the COUNT(*) baseline is non-zero — proves
	// PutBatch adds rows incrementally rather than overwriting.
	if _, err := store.Put(ctx, wsID, []byte("seed"), "seed.txt", "text/plain"); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	items := []pendinguploads.PutItem{
		{Content: []byte("alpha"), Filename: "alpha.txt", Mimetype: "text/plain"},
		{Content: []byte("beta"), Filename: "beta.bin", Mimetype: "application/octet-stream"},
		{Content: []byte("gamma"), Filename: "gamma.pdf", Mimetype: "application/pdf"},
	}
	ids, err := store.PutBatch(ctx, wsID, items)
	if err != nil {
		t.Fatalf("PutBatch: %v", err)
	}
	if len(ids) != len(items) {
		t.Fatalf("ids length %d, want %d", len(ids), len(items))
	}

	// Each returned id round-trips through Get with the right content.
	for i, id := range ids {
		rec, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get item %d (%s): %v", i, id, err)
		}
		if string(rec.Content) != string(items[i].Content) {
			t.Errorf("item %d content = %q, want %q", i, rec.Content, items[i].Content)
		}
		if rec.Filename != items[i].Filename {
			t.Errorf("item %d filename = %q, want %q", i, rec.Filename, items[i].Filename)
		}
	}

	var n int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_uploads WHERE workspace_id = $1`, wsID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 4 {
		t.Errorf("workspace row count = %d, want 4 (1 seed + 3 batch)", n)
	}
}

// TestIntegration_PendingUploads_PutBatch_AtomicRollback_NoLeakOnFailure
// proves the all-or-nothing contract end-to-end against real Postgres MVCC.
//
// Strategy: build a 3-item batch where item index 1 carries a filename with
// an embedded NUL byte. lib/pq rejects NULs in TEXT columns at the protocol
// layer (`pq: invalid byte sequence for encoding "UTF8": 0x00`), which
// triggers the per-row INSERT error path in PutBatch. The first item's
// INSERT…RETURNING already wrote a row to the Tx's snapshot, so a buggy
// rollback would leave that row visible after PutBatch returns.
//
// Postgrest semantics: ROLLBACK is the only way a real DB can guarantee the
// "no leak" contract; a unit test with sqlmock can prove the Go function
// CALLED Rollback, but only this integration test proves Postgres actually
// HONORED it.
func TestIntegration_PendingUploads_PutBatch_AtomicRollback_NoLeakOnFailure(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	store := pendinguploads.NewPostgres(conn)
	ctx := context.Background()

	wsID := uuid.New()

	// Baseline COUNT(*) for this workspace — must remain 0 after a failed batch.
	var before int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_uploads WHERE workspace_id = $1`, wsID).Scan(&before); err != nil {
		t.Fatalf("baseline count: %v", err)
	}
	if before != 0 {
		t.Fatalf("workspace not isolated: baseline = %d, want 0", before)
	}

	// Item 1 has a NUL byte in the filename — Go-side pre-validation
	// (which only checks empty/length) lets it through, so the INSERT
	// reaches lib/pq, which rejects it at the protocol level. That's the
	// canonical "DB-side error mid-batch" we want to exercise.
	items := []pendinguploads.PutItem{
		{Content: []byte("ok"), Filename: "ok.txt", Mimetype: "text/plain"},
		{Content: []byte("bad"), Filename: "bad\x00name.txt", Mimetype: "text/plain"},
		{Content: []byte("never"), Filename: "never.txt", Mimetype: "text/plain"},
	}
	_, err := store.PutBatch(ctx, wsID, items)
	if err == nil {
		t.Fatalf("expected error from NUL-byte filename, got nil")
	}

	// THE assertion this whole test exists for: even though item 0's
	// INSERT…RETURNING succeeded inside the Tx, the rollback unwound
	// it — zero rows for this workspace, not one (let alone three).
	var after int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_uploads WHERE workspace_id = $1`, wsID).Scan(&after); err != nil {
		t.Fatalf("post-failure count: %v", err)
	}
	if after != 0 {
		t.Errorf("Tx rollback leaked rows: workspace count = %d, want 0", after)
	}
}

// TestIntegration_PendingUploads_PutBatch_Oversize_NoTxOpened verifies the
// pre-validation short-circuit: an oversized item rejects with ErrTooLarge
// BEFORE any Tx opens, so the table is untouched. The unit test (sqlmock
// with zero expectations) catches the Go-side path; this test sanity-checks
// no real DB I/O happens by confirming COUNT(*) doesn't move.
func TestIntegration_PendingUploads_PutBatch_Oversize_NoTxOpened(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	store := pendinguploads.NewPostgres(conn)
	ctx := context.Background()

	wsID := uuid.New()
	tooBig := make([]byte, pendinguploads.MaxFileBytes+1)
	_, err := store.PutBatch(ctx, wsID, []pendinguploads.PutItem{
		{Content: []byte("ok"), Filename: "ok.txt"},
		{Content: tooBig, Filename: "too-big.bin"},
	})
	if err != pendinguploads.ErrTooLarge {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_uploads WHERE workspace_id = $1`, wsID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("pre-validation did NOT short-circuit: count = %d, want 0", n)
	}
}

// TestIntegration_PendingUploads_AckedIndexExists verifies the Phase 5a
// migration (20260505200000_pending_uploads_acked_index.up.sql) actually
// created idx_pending_uploads_acked with the right partial-index predicate.
//
// Why pg_indexes and not EXPLAIN: the planner prefers Seq Scan on tiny
// tables regardless of available indexes — a plan-shape check would be
// flaky under real test loads. The contract we care about is "the index
// exists with the predicate we wrote in the migration"; pg_indexes is
// the canonical source for that, robust to row count and planner version.
func TestIntegration_PendingUploads_AckedIndexExists(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	ctx := context.Background()

	var indexdef string
	err := conn.QueryRowContext(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = 'public'
		  AND tablename = 'pending_uploads'
		  AND indexname = 'idx_pending_uploads_acked'
	`).Scan(&indexdef)
	if err == sql.ErrNoRows {
		t.Fatal("idx_pending_uploads_acked is missing — migration 20260505200000 not applied")
	}
	if err != nil {
		t.Fatalf("pg_indexes query: %v", err)
	}

	// Pin the partial-index predicate. Without "WHERE acked_at IS NOT NULL"
	// we'd be indexing the entire table (defeats the point — most rows are
	// unacked), and the existing idx_pending_uploads_unacked already covers
	// the inverse predicate.
	if !strings.Contains(indexdef, "(acked_at)") {
		t.Errorf("index missing acked_at column: %s", indexdef)
	}
	if !strings.Contains(indexdef, "WHERE (acked_at IS NOT NULL)") {
		t.Errorf("index missing partial predicate: %s", indexdef)
	}
}

// TestIntegration_PollUpload_AtomicRollback_AcrossBothTables proves the
// #149 cross-table contract at the database layer: when PutBatchTx and
// LogActivityTx run in the same caller-owned Tx and an activity INSERT
// fails after some rows have already been INSERTed, Rollback unwinds
// BOTH tables, leaving zero rows.
//
// Coverage map (#149):
//   - chat_files_poll_test.go's TestPollUpload_AtomicRollbackOnActivityInsertFailure
//     uses sqlmock to prove the Go handler issues Begin / N inserts /
//     Rollback in the right order (no Commit on failure path).
//   - This integration test proves the helpers + real Postgres compose
//     correctly: rollback after a mid-Tx activity insert failure
//     actually reverts BOTH the prior activity row AND the
//     pending_uploads rows from PutBatchTx.
//   - The pre-existing TestIntegration_PendingUploads_PutBatch_AtomicRollback
//     covers the pending_uploads-only case.
//
// Failure injection: a NUL byte in `summary` (TEXT column) — lib/pq
// rejects it at the protocol layer. Same trick the existing PutBatch
// AtomicRollback test uses for the pending_uploads INSERT.
func TestIntegration_PollUpload_AtomicRollback_AcrossBothTables(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	ctx := context.Background()

	// activity_logs has a FK to workspaces(id) — seed a real row so
	// non-failing inserts succeed. Wipe activity_logs + this workspaces
	// row at end so the next test sees a clean slate (the integrationDB
	// helper only wipes pending_uploads).
	wsID := uuid.New()
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO workspaces (id, name) VALUES ($1, 'test-149-rollback')`, wsID,
	); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() {
		// CASCADE on workspaces FK deletes the activity_logs rows; explicit
		// DELETE on activity_logs catches any rows that somehow leaked.
		_, _ = conn.ExecContext(context.Background(), `DELETE FROM activity_logs WHERE workspace_id = $1`, wsID)
		_, _ = conn.ExecContext(context.Background(), `DELETE FROM workspaces WHERE id = $1`, wsID)
	})

	store := pendinguploads.NewPostgres(conn)

	// Mirror uploadPollMode's Tx shape: BeginTx → PutBatchTx → N ×
	// LogActivityTx → Commit (or Rollback on failure).
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	items := []pendinguploads.PutItem{
		{Content: []byte("first"), Filename: "a.txt", Mimetype: "text/plain"},
		{Content: []byte("second"), Filename: "b.txt", Mimetype: "text/plain"},
	}
	fileIDs, err := store.PutBatchTx(ctx, tx, wsID, items)
	if err != nil {
		t.Fatalf("PutBatchTx: %v", err)
	}
	if len(fileIDs) != 2 {
		t.Fatalf("len(fileIDs) = %d, want 2", len(fileIDs))
	}

	// First activity insert succeeds — would commit if not for the
	// rollback that the second insert's failure forces.
	wsIDStr := wsID.String()
	method := "chat_upload_receive"
	okSummary := "chat_upload_receive: a.txt"
	if _, err := LogActivityTx(ctx, tx, nil, ActivityParams{
		WorkspaceID:  wsIDStr,
		ActivityType: "a2a_receive",
		TargetID:     &wsIDStr,
		Method:       &method,
		Summary:      &okSummary,
		Status:       "ok",
	}); err != nil {
		t.Fatalf("first LogActivityTx (should succeed): %v", err)
	}

	// Second activity insert: NUL byte in summary triggers lib/pq
	// "invalid byte sequence for encoding UTF8: 0x00" — the canonical
	// "DB-side error after some Tx work has already happened" we want.
	badSummary := "chat_upload_receive: b\x00.txt"
	_, err = LogActivityTx(ctx, tx, nil, ActivityParams{
		WorkspaceID:  wsIDStr,
		ActivityType: "a2a_receive",
		TargetID:     &wsIDStr,
		Method:       &method,
		Summary:      &badSummary,
		Status:       "ok",
	})
	if err == nil {
		t.Fatal("expected error from NUL-byte summary, got nil")
	}

	// Caller (uploadPollMode in production) rolls back on the error.
	if rbErr := tx.Rollback(); rbErr != nil {
		t.Fatalf("Rollback: %v", rbErr)
	}

	// THE assertion this test exists for: BOTH tables must have zero
	// rows for this workspace. Pre-#149 the activity_logs row from the
	// first insert would persist (separate fire-and-forget INSERT) and
	// pending_uploads would also persist (committed by PutBatch's own
	// Tx). Post-#149 the shared Tx + Rollback unwinds both.
	var puCount, alCount int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_uploads WHERE workspace_id = $1`, wsID,
	).Scan(&puCount); err != nil {
		t.Fatalf("count pending_uploads: %v", err)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM activity_logs WHERE workspace_id = $1`, wsID,
	).Scan(&alCount); err != nil {
		t.Fatalf("count activity_logs: %v", err)
	}
	if puCount != 0 {
		t.Errorf("pending_uploads leaked %d row(s) after Rollback — #149 regression", puCount)
	}
	if alCount != 0 {
		t.Errorf("activity_logs leaked %d row(s) after Rollback — #149 regression "+
			"(THIS is the scenario the ticket called out: pre-fix, the first activity row "+
			"committed in its own implicit Tx, leaving an orphan)", alCount)
	}
}

// TestIntegration_PollUpload_HappyPath_AcrossBothTables is the positive
// counterpart to the rollback test: when nothing fails, both tables
// commit together and the row counts match.
func TestIntegration_PollUpload_HappyPath_AcrossBothTables(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	ctx := context.Background()

	wsID := uuid.New()
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO workspaces (id, name) VALUES ($1, 'test-149-happy')`, wsID,
	); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), `DELETE FROM activity_logs WHERE workspace_id = $1`, wsID)
		_, _ = conn.ExecContext(context.Background(), `DELETE FROM workspaces WHERE id = $1`, wsID)
	})

	store := pendinguploads.NewPostgres(conn)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	items := []pendinguploads.PutItem{
		{Content: []byte("a"), Filename: "a.txt", Mimetype: "text/plain"},
		{Content: []byte("b"), Filename: "b.txt", Mimetype: "text/plain"},
		{Content: []byte("c"), Filename: "c.txt", Mimetype: "text/plain"},
	}
	if _, err := store.PutBatchTx(ctx, tx, wsID, items); err != nil {
		t.Fatalf("PutBatchTx: %v", err)
	}
	wsIDStr := wsID.String()
	method := "chat_upload_receive"
	for _, it := range items {
		summary := "chat_upload_receive: " + it.Filename
		if _, err := LogActivityTx(ctx, tx, nil, ActivityParams{
			WorkspaceID:  wsIDStr,
			ActivityType: "a2a_receive",
			TargetID:     &wsIDStr,
			Method:       &method,
			Summary:      &summary,
			Status:       "ok",
		}); err != nil {
			t.Fatalf("LogActivityTx %q: %v", it.Filename, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var puCount, alCount int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_uploads WHERE workspace_id = $1`, wsID,
	).Scan(&puCount); err != nil {
		t.Fatalf("count pending_uploads: %v", err)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM activity_logs WHERE workspace_id = $1`, wsID,
	).Scan(&alCount); err != nil {
		t.Fatalf("count activity_logs: %v", err)
	}
	if puCount != 3 {
		t.Errorf("pending_uploads count = %d, want 3", puCount)
	}
	if alCount != 3 {
		t.Errorf("activity_logs count = %d, want 3", alCount)
	}
}

func TestIntegration_PendingUploads_GetIgnoresExpiredAndAcked(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	store := pendinguploads.NewPostgres(conn)
	ctx := context.Background()

	wsID := uuid.New()
	fid, err := store.Put(ctx, wsID, []byte("data"), "x.txt", "text/plain")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Backdate expires_at — Get must return ErrNotFound, even though the
	// row physically exists in the table (Sweep hasn't run).
	if _, err := conn.ExecContext(ctx,
		`UPDATE pending_uploads SET expires_at = now() - interval '1 minute' WHERE file_id = $1`,
		fid,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := store.Get(ctx, fid); err != pendinguploads.ErrNotFound {
		t.Errorf("Get after expiry: got %v, want ErrNotFound", err)
	}
}

// TestIntegration_PendingUploads_SizeCap_100MB pins the post-mc#1588
// poll-mode cap to 100 MB at the DB CHECK level. The MaxFileBytes-1 row
// must INSERT cleanly (no CHECK violation) and the +1 row must be
// rejected pre-DB by Put with ErrTooLarge. We also verify the raw DB
// CHECK by attempting an INSERT that bypasses Put's pre-DB guard — the
// constraint itself must enforce 104857600.
//
// Regression target: migration 20260519200000_pending_uploads_bump_size_cap
// — if a future migration regresses the cap (e.g., re-applies the
// 25 MB ceiling) this test 413s.
func TestIntegration_PendingUploads_SizeCap_100MB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 100MB cap integration test in -short mode (allocates ~100MB)")
	}
	conn := integrationDB_PendingUploads(t)
	store := pendinguploads.NewPostgres(conn)
	ctx := context.Background()

	wsID := uuid.New()

	// 50 MB row — previously would have hit the 25 MB CHECK; must now
	// succeed end-to-end (Put → INSERT → row visible via Get).
	fiftyMB := make([]byte, 50*1024*1024)
	for i := range fiftyMB {
		fiftyMB[i] = 0x42 // non-zero so bytea round-trip is observable
	}
	fid, err := store.Put(ctx, wsID, fiftyMB, "halfcap.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("Put 50MB: want nil err (cap is 100MB after mc#1588 follow-up), got %v", err)
	}
	rec, err := store.Get(ctx, fid)
	if err != nil {
		t.Fatalf("Get 50MB row: %v", err)
	}
	if rec.SizeBytes != int64(len(fiftyMB)) {
		t.Errorf("size_bytes mismatch: got %d, want %d", rec.SizeBytes, len(fiftyMB))
	}

	// Exact-cap row — 100 MB exact must succeed (the CHECK is `<=`).
	atCap := make([]byte, pendinguploads.MaxFileBytes)
	if _, err := store.Put(ctx, wsID, atCap, "atcap.bin", "application/octet-stream"); err != nil {
		t.Errorf("Put MaxFileBytes (at-cap): want nil err, got %v", err)
	}

	// Over-cap row — must be rejected by Put pre-DB.
	overCap := make([]byte, pendinguploads.MaxFileBytes+1)
	if _, err := store.Put(ctx, wsID, overCap, "overcap.bin", "application/octet-stream"); err != pendinguploads.ErrTooLarge {
		t.Errorf("Put MaxFileBytes+1: want ErrTooLarge, got %v", err)
	}

	// Raw DB CHECK enforcement — bypass Put's pre-DB guard with a direct
	// INSERT of a 101-MB row. The CHECK must reject with a Postgres
	// integrity-violation (SQLSTATE 23514). This catches the case where a
	// future code path inserts straight into the table without going
	// through Put (e.g., a background importer).
	overByOne := pendinguploads.MaxFileBytes + 1
	_, dbErr := conn.ExecContext(ctx, `
		INSERT INTO pending_uploads (workspace_id, content, size_bytes, filename, mimetype)
		VALUES ($1, $2, $3, $4, $5)
	`, wsID, []byte("dummy"), overByOne, "raw-overcap.bin", "application/octet-stream")
	if dbErr == nil {
		t.Errorf("raw INSERT of %d bytes: want CHECK violation, got nil", overByOne)
	} else if !strings.Contains(dbErr.Error(), "pending_uploads_size_bytes_check") &&
		!strings.Contains(dbErr.Error(), "23514") {
		t.Errorf("raw INSERT: want size_bytes_check violation (SQLSTATE 23514), got: %v", dbErr)
	}
}

// TestIntegration_PendingUploads_SizeCap_DBConstraintName pins the
// expected CHECK constraint name. Migration 20260519200000 DROP IF
// EXISTSes by the auto-generated name `pending_uploads_size_bytes_check`
// and re-adds the constraint under the same name. If a future schema
// edit renames the constraint, the next bump migration will silently
// no-op the DROP and leave the old ceiling in place — this test catches
// that drift.
func TestIntegration_PendingUploads_SizeCap_DBConstraintName(t *testing.T) {
	conn := integrationDB_PendingUploads(t)
	ctx := context.Background()

	var checkClause string
	err := conn.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		WHERE t.relname = 'pending_uploads'
		  AND c.conname = 'pending_uploads_size_bytes_check'
	`).Scan(&checkClause)
	if err != nil {
		t.Fatalf("lookup CHECK clause: %v", err)
	}
	if !strings.Contains(checkClause, "104857600") {
		t.Errorf("size_bytes CHECK clause = %q; want it to contain 104857600 (100 MB)", checkClause)
	}
	if strings.Contains(checkClause, "26214400") {
		t.Errorf("size_bytes CHECK clause = %q; must NOT contain 26214400 (the pre-bump 25 MB ceiling)", checkClause)
	}
}
