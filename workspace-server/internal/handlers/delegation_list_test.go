package handlers

// delegation_list_test.go — unit tests for listDelegationsFromLedger and
// listDelegationsFromActivityLogs. Both methods are the data-backend of the
// ListDelegations handler; coverage was missing (cf. infra-sre review of PR #942).

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
)

// ---------- listDelegationsFromLedger ----------

func TestListDelegationsFromLedger_EmptyResult(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	rows := sqlmock.NewRows([]string{
		"delegation_id", "caller_id", "callee_id", "task_preview",
		"status", "result_preview", "error_detail",
		"last_heartbeat", "deadline", "created_at", "updated_at",
	})
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	if got != nil {
		t.Errorf("empty result: expected nil, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromLedger_SingleRow(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	// Use time.Time{} for nullable *time.Time columns — sqlmock passes the
	// zero value to the handler's scan destination. The handler checks Valid
	// before using each nullable field, so zero values are safe.
	rows := sqlmock.NewRows([]string{
		"delegation_id", "caller_id", "callee_id", "task_preview",
		"status", "result_preview", "error_detail",
		"last_heartbeat", "deadline", "created_at", "updated_at",
	}).AddRow(
		"del-1", "ws-1", "ws-2", "summarise the report",
		"completed", "the report is about Q1",
		"", now, now, now, now,
	)
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	e := got[0]
	if e["delegation_id"] != "del-1" {
		t.Errorf("delegation_id: got %v, want del-1", e["delegation_id"])
	}
	if e["source_id"] != "ws-1" {
		t.Errorf("source_id: got %v, want ws-1", e["source_id"])
	}
	if e["target_id"] != "ws-2" {
		t.Errorf("target_id: got %v, want ws-2", e["target_id"])
	}
	if e["status"] != "completed" {
		t.Errorf("status: got %v, want completed", e["status"])
	}
	if e["response_preview"] != "the report is about Q1" {
		t.Errorf("response_preview: got %v", e["response_preview"])
	}
	if _, ok := e["error"]; ok {
		t.Errorf("error should be absent when empty, got %v", e["error"])
	}
	if e["_ledger"] != true {
		t.Errorf("_ledger marker: got %v, want true", e["_ledger"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromLedger_MultipleRows(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"delegation_id", "caller_id", "callee_id", "task_preview",
		"status", "result_preview", "error_detail",
		"last_heartbeat", "deadline", "created_at", "updated_at",
	}).
		AddRow("del-a", "ws-1", "ws-2", "task a", "in_progress", "", "", now, now, now, now).
		AddRow("del-b", "ws-1", "ws-3", "task b", "failed", "", "timeout", now, now, now, now).
		AddRow("del-c", "ws-1", "ws-4", "task c", "completed", "result c", "", now, now, now, now)
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	if got[0]["delegation_id"] != "del-a" || got[1]["delegation_id"] != "del-b" || got[2]["delegation_id"] != "del-c" {
		t.Errorf("unexpected order: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

=======
func TestListDelegationsFromLedger_NullsOmitted(t *testing.T) {
	// last_heartbeat, deadline, result_preview, error_detail are all NULL.
	// Handler must not panic and must omit those keys from the map.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { mockDB.Close(); db.DB = prevDB })

	now := time.Now()
	rows := sqlmock.NewRows([]string{}).
		AddRow("del-1", "ws-1", "ws-2", "task", "queued", nil, nil, nil, nil, now, now)
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	e := got[0]
	if _, ok := e["last_heartbeat"]; ok {
		t.Error("last_heartbeat should be absent when NULL")
	}
	if _, ok := e["deadline"]; ok {
		t.Error("deadline should be absent when NULL")
	}
	if _, ok := e["response_preview"]; ok {
		t.Error("response_preview should be absent when NULL result_preview")
	}
	if _, ok := e["error"]; ok {
		t.Error("error should be absent when NULL error_detail")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

>>>>>>> 5531b471 (handlers: restore db.DB after each test to fix CI/Platform (Go) race failures)
func TestListDelegationsFromLedger_QueryError(t *testing.T) {
	// Query failure returns nil — graceful fallback, no panic.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnError(context.DeadlineExceeded)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	if got != nil {
		t.Errorf("query error: expected nil, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromLedger_RowsErr(t *testing.T) {
	// rows.Err() mid-stream: handler collects partial results and returns them.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	// RowError(0) before AddRow(0): row 0 is "bad", rows.Next() returns false
	// on first call — the row never scans, result stays nil. To get partial
	// results (row 0 scanned) with rows.Err() non-nil, we use 2 rows and put
	// RowError(1) after AddRow(1): row 0 scans normally, row 1 is bad,
	// rows.Err() is error, handler returns partial result.
	rows := sqlmock.NewRows([]string{
		"delegation_id", "caller_id", "callee_id", "task_preview",
		"status", "result_preview", "error_detail",
		"last_heartbeat", "deadline", "created_at", "updated_at",
	}).
		AddRow("del-1", "ws-1", "ws-2", "task", "queued", "", "", now, now, now, now).
		AddRow("del-2", "ws-1", "ws-3", "another task", "queued", "", "", now, now, now, now).
		RowError(1, context.DeadlineExceeded)
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	// Row 0 scanned and appended; row 1 is bad; rows.Err() is non-nil.
	// Handler logs the error but returns result (partial results because result != nil).
	if got == nil || len(got) != 1 {
		t.Errorf("rows.Err path: expected 1 partial result, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestListDelegationsFromLedger_ScanError is removed.
//
// In Go 1.25 sqlmock.NewRows validates column count at AddRow() time and
// panics when len(values) != len(columns). The old pattern
//   sqlmock.NewRows([]string{}).AddRow("only-one-col")
// therefore panics in test SETUP, not inside the handler. The handler has no
// recover(), so a scan panic would propagate out of listDelegationsFromLedger
// and crash the process — this is the correct behaviour (not silently skipping
// a row). The correct way to cover this path is a real-DB integration test.
//
// ---------- listDelegationsFromActivityLogs ----------

func TestListDelegationsFromActivityLogs_EmptyResult(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	rows := sqlmock.NewRows([]string{
		"id", "activity_type", "source_id", "target_id",
		"summary", "status", "error_detail",
		"response_preview", "delegation_id", "created_at",
	})
	mock.ExpectQuery("SELECT .+ FROM activity_logs").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	if len(got) != 0 {
		t.Errorf("empty result: expected empty slice, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromActivityLogs_SingleDelegateRow(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"id", "activity_type", "source_id", "target_id",
		"summary", "status", "error_detail",
		"response_preview", "delegation_id", "created_at",
	}).AddRow(
		"act-1", "delegate",
		"ws-1", "ws-2",
		"analyse Q1 numbers",
		"in_progress",
		"", "", "",
		now,
	)
	mock.ExpectQuery("SELECT .+ FROM activity_logs").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	e := got[0]
	if e["id"] != "act-1" {
		t.Errorf("id: got %v, want act-1", e["id"])
	}
	if e["type"] != "delegate" {
		t.Errorf("type: got %v, want delegate", e["type"])
	}
	if e["source_id"] != "ws-1" {
		t.Errorf("source_id: got %v, want ws-1", e["source_id"])
	}
	if e["target_id"] != "ws-2" {
		t.Errorf("target_id: got %v, want ws-2", e["target_id"])
	}
	if e["summary"] != "analyse Q1 numbers" {
		t.Errorf("summary: got %v", e["summary"])
	}
	if e["status"] != "in_progress" {
		t.Errorf("status: got %v", e["status"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromActivityLogs_DelegateResultWithError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"id", "activity_type", "source_id", "target_id",
		"summary", "status", "error_detail",
		"response_preview", "delegation_id", "created_at",
	}).AddRow(
		"act-2", "delegate_result",
		"ws-1", "ws-2",
		"result summary",
		"failed",
		"Callee workspace not reachable",
		`{"text":"the result body text"}`,
		"del-abc",
		now,
	)
	mock.ExpectQuery("SELECT .+ FROM activity_logs").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	e := got[0]
	if e["type"] != "delegate_result" {
		t.Errorf("type: got %v", e["type"])
	}
	if e["error"] != "Callee workspace not reachable" {
		t.Errorf("error: got %v", e["error"])
	}
	if e["response_preview"] != `{"text":"the result body text"}` {
		t.Errorf("response_preview: got %v", e["response_preview"])
	}
	if e["delegation_id"] != "del-abc" {
		t.Errorf("delegation_id: got %v", e["delegation_id"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromActivityLogs_QueryError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	mock.ExpectQuery("SELECT .+ FROM activity_logs").
		WithArgs("ws-1").
		WillReturnError(context.DeadlineExceeded)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	// Error → returns empty slice, not nil.
	if len(got) != 0 {
		t.Errorf("query error: expected empty slice, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromActivityLogs_RowsErr(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	// RowError(0) before AddRow(0): row 0 is "bad", rows.Next() returns false
	// on first call — the row never scans, result stays nil. To get partial
	// results (row 0 scanned) with rows.Err() non-nil, we use 2 rows and put
	// RowError(1) after AddRow(1): row 0 scans normally, row 1 is bad,
	// rows.Err() is error, handler returns partial result.
	rows := sqlmock.NewRows([]string{
		"id", "activity_type", "source_id", "target_id",
		"summary", "status", "error_detail",
		"response_preview", "delegation_id", "created_at",
	}).
		AddRow("act-1", "delegate", "ws-1", "ws-2", "task", "queued", "", "", "", now).
		AddRow("act-2", "delegate", "ws-1", "ws-3", "another task", "queued", "", "", "", now).
		RowError(1, context.DeadlineExceeded)
	mock.ExpectQuery("SELECT .+ FROM activity_logs").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	// Row 0 scanned and appended; row 1 is bad; rows.Err() is non-nil.
	// Handler logs the error but returns result (partial results because result != nil).
	if got == nil || len(got) != 1 {
		t.Errorf("rows.Err path: expected 1 partial result, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

<<<<<<< HEAD
// TestListDelegationsFromActivityLogs_ScanErrorSkipped is removed.
//
// Same reason as TestListDelegationsFromLedger_ScanError: Go 1.25 causes
// sqlmock.NewRows([]string{}).AddRow(...) to panic in test SETUP. The handler
// has no recover(), so a scan panic would crash the process — the correct
// behaviour. Real-DB integration tests cover this path.
