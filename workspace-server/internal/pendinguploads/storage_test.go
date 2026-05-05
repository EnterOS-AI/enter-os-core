package pendinguploads_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/pendinguploads"
)

// Tests pin the SQL the handler relies on. Drift detection: if the
// migration changes column order / predicate shape, sqlmock's
// QueryMatcherEqual + ExpectQuery / ExpectExec on the literal text
// fails the test before the handler can ship a silently-broken read.
//
// Why sqlmock and not testcontainers / real Postgres:
//
//	The Storage contract is "this Go method runs THIS SQL."  Real-DB
//	tests would catch SQL-syntax errors but not the contract drift
//	we care about (e.g. handler accidentally reordering columns,
//	dropping the acked_at predicate, etc.).  Postgres-syntax coverage
//	lives in the migration round-trip test (Phase 4 E2E).

func newMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

// Single source of truth for the SQL strings — drift here = test fails;
// matches the Go literals in storage.go exactly.
const (
	insertSQL = `
		INSERT INTO pending_uploads (workspace_id, content, size_bytes, filename, mimetype)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING file_id
	`
	selectSQL = `
		SELECT file_id, workspace_id, content, filename, mimetype,
		       size_bytes, created_at, fetched_at, acked_at, expires_at
		FROM pending_uploads
		WHERE file_id = $1
		  AND acked_at IS NULL
		  AND expires_at > now()
	`
	markFetchedSQL = `
		UPDATE pending_uploads
		SET fetched_at = now()
		WHERE file_id = $1
		  AND acked_at IS NULL
		  AND expires_at > now()
	`
	ackSQL = `
		UPDATE pending_uploads
		SET acked_at = now()
		WHERE file_id = $1
		  AND acked_at IS NULL
		  AND expires_at > now()
	`
	ackDisambiguateSQL = `
		SELECT acked_at FROM pending_uploads
		WHERE file_id = $1 AND expires_at > now()
	`
	sweepSQL = `
		WITH deleted AS (
			DELETE FROM pending_uploads
			WHERE (acked_at IS NOT NULL AND acked_at < now() - make_interval(secs => $1))
			   OR (acked_at IS NULL     AND expires_at < now())
			RETURNING (acked_at IS NOT NULL) AS was_acked
		)
		SELECT
			COALESCE(SUM(CASE WHEN was_acked     THEN 1 ELSE 0 END), 0)::int AS acked,
			COALESCE(SUM(CASE WHEN NOT was_acked THEN 1 ELSE 0 END), 0)::int AS expired
		FROM deleted
	`
)

// ----- Put ------------------------------------------------------------------

func TestPut_HappyPath_ReturnsAssignedFileID(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	wsID := uuid.New()
	expectedID := uuid.New()
	mock.ExpectQuery(insertSQL).
		WithArgs(wsID, []byte("hello"), int64(5), "report.pdf", "application/pdf").
		WillReturnRows(sqlmock.NewRows([]string{"file_id"}).AddRow(expectedID))

	got, err := store.Put(context.Background(), wsID, []byte("hello"), "report.pdf", "application/pdf")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got != expectedID {
		t.Errorf("file_id mismatch: got %s want %s", got, expectedID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestPut_RejectsEmptyContentBeforeDB(t *testing.T) {
	db, _ := newMockDB(t) // no expectations — must NOT round-trip
	store := pendinguploads.NewPostgres(db)

	_, err := store.Put(context.Background(), uuid.New(), nil, "x.txt", "")
	if err == nil || !strings.Contains(err.Error(), "empty content") {
		t.Fatalf("expected empty-content error, got %v", err)
	}
}

func TestPut_RejectsOversizeBeforeDB(t *testing.T) {
	db, _ := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	too := make([]byte, pendinguploads.MaxFileBytes+1)
	_, err := store.Put(context.Background(), uuid.New(), too, "x.txt", "")
	if !errors.Is(err, pendinguploads.ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
}

func TestPut_RejectsEmptyFilenameBeforeDB(t *testing.T) {
	db, _ := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	_, err := store.Put(context.Background(), uuid.New(), []byte("hi"), "", "")
	if err == nil || !strings.Contains(err.Error(), "empty filename") {
		t.Fatalf("expected empty-filename error, got %v", err)
	}
}

func TestPut_RejectsLongFilenameBeforeDB(t *testing.T) {
	db, _ := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	long := strings.Repeat("a", 101)
	_, err := store.Put(context.Background(), uuid.New(), []byte("hi"), long, "")
	if err == nil || !strings.Contains(err.Error(), "exceeds 100 chars") {
		t.Fatalf("expected too-long-filename error, got %v", err)
	}
}

func TestPut_PropagatesDBError(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	mock.ExpectQuery(insertSQL).
		WithArgs(uuid.Nil, sqlmock.AnyArg(), int64(2), "x", "").
		WillReturnError(errors.New("connection refused"))

	wsID := uuid.Nil
	_, err := store.Put(context.Background(), wsID, []byte("hi"), "x", "")
	if err == nil || !strings.Contains(err.Error(), "insert") {
		t.Fatalf("expected wrapped insert error, got %v", err)
	}
}

// ----- Get ------------------------------------------------------------------

func TestGet_HappyPath_ReturnsFullRow(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	fid := uuid.New()
	wsID := uuid.New()
	now := time.Now().UTC()
	mock.ExpectQuery(selectSQL).
		WithArgs(fid).
		WillReturnRows(sqlmock.NewRows([]string{
			"file_id", "workspace_id", "content", "filename", "mimetype",
			"size_bytes", "created_at", "fetched_at", "acked_at", "expires_at",
		}).AddRow(
			fid, wsID, []byte("data"), "x.bin", "application/octet-stream",
			int64(4), now, nil, nil, now.Add(24*time.Hour),
		))

	r, err := store.Get(context.Background(), fid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.FileID != fid || r.WorkspaceID != wsID {
		t.Errorf("ids mismatch: %+v", r)
	}
	if string(r.Content) != "data" || r.SizeBytes != 4 {
		t.Errorf("content mismatch: %+v", r)
	}
	if r.FetchedAt != nil || r.AckedAt != nil {
		t.Errorf("expected nil timestamps for unfetched row, got fetched=%v acked=%v", r.FetchedAt, r.AckedAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestGet_AbsentRow_ReturnsErrNotFound(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	fid := uuid.New()
	mock.ExpectQuery(selectSQL).
		WithArgs(fid).
		WillReturnError(sql.ErrNoRows)

	_, err := store.Get(context.Background(), fid)
	if !errors.Is(err, pendinguploads.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGet_DBError_WrappedAndPropagated(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	mock.ExpectQuery(selectSQL).
		WillReturnError(errors.New("connection lost"))

	_, err := store.Get(context.Background(), uuid.New())
	if err == nil || errors.Is(err, pendinguploads.ErrNotFound) || !strings.Contains(err.Error(), "select") {
		t.Fatalf("expected wrapped select error, got %v", err)
	}
}

// ----- MarkFetched ----------------------------------------------------------

func TestMarkFetched_HappyPath(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	fid := uuid.New()
	mock.ExpectExec(markFetchedSQL).
		WithArgs(fid).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.MarkFetched(context.Background(), fid); err != nil {
		t.Fatalf("MarkFetched: %v", err)
	}
}

func TestMarkFetched_AbsentOrAckedOrExpired_ReturnsErrNotFound(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	fid := uuid.New()
	mock.ExpectExec(markFetchedSQL).
		WithArgs(fid).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := store.MarkFetched(context.Background(), fid)
	if !errors.Is(err, pendinguploads.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMarkFetched_DBError_Wrapped(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	mock.ExpectExec(markFetchedSQL).
		WillReturnError(errors.New("pg flake"))

	err := store.MarkFetched(context.Background(), uuid.New())
	if err == nil || errors.Is(err, pendinguploads.ErrNotFound) || !strings.Contains(err.Error(), "mark_fetched") {
		t.Fatalf("expected wrapped mark_fetched error, got %v", err)
	}
}

// ----- Ack ------------------------------------------------------------------

func TestAck_FirstAck_StampsAckedAt(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	fid := uuid.New()
	mock.ExpectExec(ackSQL).
		WithArgs(fid).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Ack(context.Background(), fid); err != nil {
		t.Fatalf("Ack: %v", err)
	}
}

func TestAck_AlreadyAcked_IdempotentSuccess(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	fid := uuid.New()
	// First UPDATE matches zero rows (already acked).
	mock.ExpectExec(ackSQL).
		WithArgs(fid).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Disambiguation SELECT finds the row with acked_at non-null.
	mock.ExpectQuery(ackDisambiguateSQL).
		WithArgs(fid).
		WillReturnRows(sqlmock.NewRows([]string{"acked_at"}).AddRow(time.Now().UTC()))

	if err := store.Ack(context.Background(), fid); err != nil {
		t.Fatalf("expected idempotent success on already-acked, got %v", err)
	}
}

func TestAck_AbsentOrExpired_ReturnsErrNotFound(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	fid := uuid.New()
	mock.ExpectExec(ackSQL).
		WithArgs(fid).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(ackDisambiguateSQL).
		WithArgs(fid).
		WillReturnError(sql.ErrNoRows)

	err := store.Ack(context.Background(), fid)
	if !errors.Is(err, pendinguploads.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestAck_RaceWithSweep_ReturnsSuccess(t *testing.T) {
	// UPDATE saw 0 rows AND the disambiguate SELECT saw a row with
	// acked_at IS NULL — only possible if the GC sweep raced between
	// the two queries. The contract says we honor the workspace's ACK
	// intent and return success.
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	fid := uuid.New()
	mock.ExpectExec(ackSQL).
		WithArgs(fid).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(ackDisambiguateSQL).
		WithArgs(fid).
		WillReturnRows(sqlmock.NewRows([]string{"acked_at"}).AddRow(nil))

	if err := store.Ack(context.Background(), fid); err != nil {
		t.Fatalf("expected race success, got %v", err)
	}
}

func TestAck_DBErrorOnUpdate_Wrapped(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	mock.ExpectExec(ackSQL).
		WillReturnError(errors.New("conn refused"))

	err := store.Ack(context.Background(), uuid.New())
	if err == nil || !strings.Contains(err.Error(), "ack:") {
		t.Fatalf("expected wrapped ack error, got %v", err)
	}
}

func TestMarkFetched_RowsAffectedError_Wrapped(t *testing.T) {
	// Some drivers (or Result wrappers) return an error from
	// RowsAffected() even when ExecContext succeeded — the contract
	// says we surface that as a wrapped error rather than silently
	// treating it as 0 rows (= ErrNotFound, which would mislead the
	// workspace into giving up on a possibly-fetched row).
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	mock.ExpectExec(markFetchedSQL).
		WillReturnResult(sqlmock.NewErrorResult(errors.New("driver doesn't support RowsAffected")))

	err := store.MarkFetched(context.Background(), uuid.New())
	if err == nil || !strings.Contains(err.Error(), "mark_fetched rows") {
		t.Fatalf("expected wrapped rows-affected error, got %v", err)
	}
}

func TestAck_RowsAffectedError_Wrapped(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	mock.ExpectExec(ackSQL).
		WillReturnResult(sqlmock.NewErrorResult(errors.New("driver doesn't support RowsAffected")))

	err := store.Ack(context.Background(), uuid.New())
	if err == nil || !strings.Contains(err.Error(), "ack rows") {
		t.Fatalf("expected wrapped rows-affected error, got %v", err)
	}
}

func TestAck_DBErrorOnDisambiguate_Wrapped(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	fid := uuid.New()
	mock.ExpectExec(ackSQL).
		WithArgs(fid).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(ackDisambiguateSQL).
		WithArgs(fid).
		WillReturnError(errors.New("connection refused"))

	err := store.Ack(context.Background(), fid)
	if err == nil || !strings.Contains(err.Error(), "disambiguate") {
		t.Fatalf("expected wrapped disambiguate error, got %v", err)
	}
}

// ----- Sweep ----------------------------------------------------------------

func TestSweep_DeletesAckedAndExpired_ReturnsCounts(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	mock.ExpectQuery(sweepSQL).
		WithArgs(int64(3600)). // 1h retention
		WillReturnRows(sqlmock.NewRows([]string{"acked", "expired"}).AddRow(7, 2))

	res, err := store.Sweep(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Acked != 7 || res.Expired != 2 || res.Total() != 9 {
		t.Errorf("got %+v want acked=7 expired=2 total=9", res)
	}
}

func TestSweep_NothingToDelete_ReturnsZero(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	mock.ExpectQuery(sweepSQL).
		WithArgs(int64(3600)).
		WillReturnRows(sqlmock.NewRows([]string{"acked", "expired"}).AddRow(0, 0))

	res, err := store.Sweep(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Total() != 0 {
		t.Errorf("got %+v, want zero result", res)
	}
}

func TestSweep_NegativeRetentionClampedToZero(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	// Negative retention must clamp to 0; the SQL gets `secs => 0` so an
	// acked-just-now row is eligible for deletion immediately. Pinned
	// here because passing the raw negative through `make_interval` would
	// silently shift acked_at → future and effectively retain rows
	// forever — exactly the wrong behavior for a "delete more aggressively"
	// caller.
	mock.ExpectQuery(sweepSQL).
		WithArgs(int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"acked", "expired"}).AddRow(3, 0))

	res, err := store.Sweep(context.Background(), -1*time.Second)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Acked != 3 {
		t.Errorf("got %+v want acked=3", res)
	}
}

func TestSweep_ZeroRetentionImmediatelyDeletesAcked(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	mock.ExpectQuery(sweepSQL).
		WithArgs(int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"acked", "expired"}).AddRow(5, 1))

	res, err := store.Sweep(context.Background(), 0)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Acked != 5 || res.Expired != 1 {
		t.Errorf("got %+v want acked=5 expired=1", res)
	}
}

func TestSweep_DBError_Wrapped(t *testing.T) {
	db, mock := newMockDB(t)
	store := pendinguploads.NewPostgres(db)

	mock.ExpectQuery(sweepSQL).
		WithArgs(int64(60)).
		WillReturnError(errors.New("connection lost"))

	_, err := store.Sweep(context.Background(), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "sweep") {
		t.Fatalf("expected wrapped sweep error, got %v", err)
	}
}

func TestSweepResult_TotalSumsCounts(t *testing.T) {
	r := pendinguploads.SweepResult{Acked: 4, Expired: 3}
	if r.Total() != 7 {
		t.Errorf("Total = %d, want 7", r.Total())
	}
	z := pendinguploads.SweepResult{}
	if z.Total() != 0 {
		t.Errorf("zero Total = %d, want 0", z.Total())
	}
}
