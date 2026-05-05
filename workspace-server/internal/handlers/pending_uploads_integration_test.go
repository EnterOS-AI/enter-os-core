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
