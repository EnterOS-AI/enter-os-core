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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/pendinguploads"
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

	// Ack flips acked_at; subsequent Gets return ErrNotFound (acked rows
	// are filtered out at the SELECT predicate).
	if err := store.Ack(ctx, fileID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if _, err := store.Get(ctx, fileID); err != pendinguploads.ErrNotFound {
		t.Errorf("Get after Ack: got %v, want ErrNotFound", err)
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
