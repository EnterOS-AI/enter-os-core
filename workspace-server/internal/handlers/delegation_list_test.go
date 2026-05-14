package handlers

// delegation_list_test.go — unit tests for listDelegationsFromLedger and
// listDelegationsFromActivityLogs. Both methods are the data-backend of the
// ListDelegations handler; coverage was missing (cf. infra-sre review of PR #942).

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
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

	rows := sqlmock.NewRows([]string{})
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
	rows := sqlmock.NewRows([]string{}).AddRow(
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
	rows := sqlmock.NewRows([]string{}).
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
	rows := sqlmock.NewRows([]string{}).
		AddRow("del-1", "ws-1", "ws-2", "task", "queued", "", "", now, now, now, now).
		RowError(0, context.DeadlineExceeded)
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	if got == nil {
		t.Error("rows.Err path should still return partial results")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromLedger_ScanError(t *testing.T) {
	// Scan error on a row: handler skips that row and continues.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	// Wrong column count → scan error on first row
	badRows := sqlmock.NewRows([]string{}).AddRow("only-one-col")
	goodRows := sqlmock.NewRows([]string{}).
		AddRow("del-1", "ws-1", "ws-2", "task", "queued", "", "", now, now, now, now)
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(badRows, goodRows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	// Bad row is skipped; good row is returned.
	if len(got) != 1 {
		t.Fatalf("expected 1 entry after scan skip, got %d", len(got))
	}
	if got[0]["delegation_id"] != "del-1" {
		t.Errorf("unexpected entry: %v", got[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// ---------- listDelegationsFromActivityLogs ----------

func TestListDelegationsFromActivityLogs_EmptyResult(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	rows := sqlmock.NewRows([]string{})
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
	rows := sqlmock.NewRows([]string{}).AddRow(
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
	rows := sqlmock.NewRows([]string{}).AddRow(
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
	rows := sqlmock.NewRows([]string{}).
		AddRow("act-1", "delegate", "ws-1", "ws-2", "task", "queued", "", "", "", now).
		RowError(0, context.DeadlineExceeded)
	mock.ExpectQuery("SELECT .+ FROM activity_logs").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	if got == nil {
		t.Error("rows.Err path should not return nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromActivityLogs_ScanErrorSkipped(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	// Wrong column count → scan error on first row
	badRows := sqlmock.NewRows([]string{}).AddRow("only-one")
	goodRows := sqlmock.NewRows([]string{}).
		AddRow("act-1", "delegate", "ws-1", "ws-2", "task", "queued", "", "", "", now)
	mock.ExpectQuery("SELECT .+ FROM activity_logs").
		WithArgs("ws-1").
		WillReturnRows(badRows, goodRows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry after scan skip, got %d", len(got))
	}
	if got[0]["id"] != "act-1" {
		t.Errorf("unexpected entry: %v", got[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}
