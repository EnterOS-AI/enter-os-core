package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// request_nudge_sweeper_test.go — coverage for the RFC unified-requests-inbox
// Phase 4 idle-agent inbox-nudge sweeper. Validates:
//
//  1. A stale pending item for an IDLE online agent gets nudged + its
//     last_nudged_at stamped.
//  2. A busy (active_tasks>0) or offline agent is NOT returned by the sweep
//     query (gated in SQL) → no nudge, no stamp.
//  3. A recently-nudged item (<1h) is excluded by the query → no nudge.
//  4. A user-recipient item is never selected → no nudge.
//  5. Empty result set is a clean no-op.
//  6. Env-override interval parses + falls back; defaults when unset.
//
// The idle/busy/offline/user-recipient/recently-nudged gates all live in the
// single sweep SELECT (status='online', active_tasks=0, recipient_type='agent',
// last_nudged_at filter). So at the sqlmock level those cases are expressed as
// "the query returns the row" vs "the query returns no row" — the test pins
// that a returned row drives exactly one enqueue + one stamp, and that an
// empty result drives neither. The SQL predicates themselves are asserted by
// the integration/real-DB harness, not sqlmock (which can't evaluate WHERE).

// newTestNudgeSweeper builds a sweeper on the sqlmock db.DB with a fake
// enqueue that records calls, so tests assert the nudge without mocking
// EnqueueA2A's internal SQL.
type recordedEnqueue struct {
	workspaceID string
	idemKey     string
	method      string
	body        []byte
	calls       int
}

func newTestNudgeSweeper(t *testing.T) (*RequestNudgeSweeper, *recordedEnqueue) {
	t.Helper()
	sw := NewRequestNudgeSweeper(nil) // binds to the sqlmock db.DB set by setupTestDB
	rec := &recordedEnqueue{}
	sw.enqueue = func(ctx context.Context, workspaceID, callerID string, priority int,
		body []byte, method, idempotencyKey string, expiresAt *time.Time) (string, int, error) {
		rec.calls++
		rec.workspaceID = workspaceID
		rec.idemKey = idempotencyKey
		rec.method = method
		rec.body = body
		return "queue-id-1", 1, nil
	}
	return sw, rec
}

func TestNudgeSweeper_EmptyResultIsCleanNoOp(t *testing.T) {
	mock := setupTestDB(t)
	sw, rec := newTestNudgeSweeper(t)

	mock.ExpectQuery(`SELECT r.recipient_id`).
		WillReturnRows(sqlmock.NewRows([]string{"recipient_id", "ids"}))

	res := sw.Sweep(context.Background())
	if res.AgentsNudged != 0 || res.ItemsCovered != 0 || res.Errors != 0 {
		t.Errorf("empty set must produce zero changes; got %+v", res)
	}
	if rec.calls != 0 {
		t.Errorf("no enqueue expected on empty set; got %d", rec.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestNudgeSweeper_StaleIdleAgentIsNudgedAndStamped(t *testing.T) {
	mock := setupTestDB(t)
	sw, rec := newTestNudgeSweeper(t)

	const ws = "11111111-1111-1111-1111-111111111111"

	// One idle online agent with two stale pending items.
	mock.ExpectQuery(`SELECT r.recipient_id`).
		WillReturnRows(sqlmock.NewRows([]string{"recipient_id", "ids"}).
			AddRow(ws, "{req-a,req-b}"))

	// After the (faked) enqueue, the two covered items get last_nudged_at set.
	mock.ExpectExec(`UPDATE requests\s+SET last_nudged_at = now\(\)`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))

	res := sw.Sweep(context.Background())
	if res.AgentsNudged != 1 {
		t.Errorf("expected 1 agent nudged, got %d", res.AgentsNudged)
	}
	if res.ItemsCovered != 2 {
		t.Errorf("expected 2 items covered, got %d", res.ItemsCovered)
	}
	if res.Errors != 0 {
		t.Errorf("expected 0 errors, got %d", res.Errors)
	}
	if rec.calls != 1 {
		t.Fatalf("expected exactly 1 enqueue, got %d", rec.calls)
	}
	if rec.workspaceID != ws {
		t.Errorf("nudge enqueued to wrong workspace: got %q want %q", rec.workspaceID, ws)
	}
	if rec.method != "message/send" {
		t.Errorf("nudge method: got %q want message/send", rec.method)
	}
	if rec.idemKey == "" {
		t.Errorf("expected a non-empty hourly idempotency key")
	}
	// Body should mention the count (2 → plural "requests") and the tool names.
	bs := string(rec.body)
	if !strings.Contains(bs, "2 unhandled inbox requests") {
		t.Errorf("nudge body missing pluralized count: %s", bs)
	}
	if !strings.Contains(bs, "list_inbox") || !strings.Contains(bs, "respond_request") {
		t.Errorf("nudge body missing tool guidance: %s", bs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestNudgeSweeper_SingularBodyForOneItem pins the "1 request" (singular) copy.
func TestNudgeSweeper_SingularBodyForOneItem(t *testing.T) {
	mock := setupTestDB(t)
	sw, rec := newTestNudgeSweeper(t)
	const ws = "22222222-2222-2222-2222-222222222222"

	mock.ExpectQuery(`SELECT r.recipient_id`).
		WillReturnRows(sqlmock.NewRows([]string{"recipient_id", "ids"}).
			AddRow(ws, "{only-one}"))
	mock.ExpectExec(`UPDATE requests\s+SET last_nudged_at = now\(\)`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res := sw.Sweep(context.Background())
	if res.ItemsCovered != 1 {
		t.Fatalf("expected 1 item covered, got %d", res.ItemsCovered)
	}
	if bs := string(rec.body); !strings.Contains(bs, "1 unhandled inbox request ") {
		t.Errorf("expected singular copy '1 unhandled inbox request'; got %s", bs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestNudgeSweeper_BusyOfflineUserAndRecentlyNudgedAreGatedBySQL — the sweep
// query excludes busy agents (active_tasks>0), offline agents (status!=online),
// user-recipient items (recipient_type='agent' filter), and recently-nudged
// items (last_nudged_at filter). Whatever the reason a candidate is ineligible,
// it does not appear in the result set → no enqueue, no stamp. This pins the
// "no row ⇒ no side effects" contract that those gates rely on.
func TestNudgeSweeper_BusyOfflineUserAndRecentlyNudgedAreGatedBySQL(t *testing.T) {
	mock := setupTestDB(t)
	sw, rec := newTestNudgeSweeper(t)

	// The real WHERE clause filtered all of {busy, offline, user-recipient,
	// recently-nudged} out, so the query yields zero rows.
	mock.ExpectQuery(`SELECT r.recipient_id`).
		WillReturnRows(sqlmock.NewRows([]string{"recipient_id", "ids"}))

	res := sw.Sweep(context.Background())
	if res.AgentsNudged != 0 {
		t.Errorf("ineligible candidates must not be nudged; got %d", res.AgentsNudged)
	}
	if rec.calls != 0 {
		t.Errorf("no enqueue expected for ineligible candidates; got %d", rec.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (an UPDATE/stamp fired for an ineligible candidate?): %v", err)
	}
}

// TestNudgeSweeper_EnqueueFailureLeavesItemsUnstamped — if the enqueue fails,
// the last_nudged_at UPDATE must NOT fire, so the items stay eligible and the
// next sweep retries. Pins that the stamp is gated on a successful enqueue.
func TestNudgeSweeper_EnqueueFailureLeavesItemsUnstamped(t *testing.T) {
	mock := setupTestDB(t)
	sw, _ := newTestNudgeSweeper(t)
	const ws = "33333333-3333-3333-3333-333333333333"

	sw.enqueue = func(ctx context.Context, workspaceID, callerID string, priority int,
		body []byte, method, idempotencyKey string, expiresAt *time.Time) (string, int, error) {
		return "", 0, context.DeadlineExceeded
	}

	mock.ExpectQuery(`SELECT r.recipient_id`).
		WillReturnRows(sqlmock.NewRows([]string{"recipient_id", "ids"}).
			AddRow(ws, "{req-x}"))
	// No ExpectExec for the UPDATE — it must not fire after a failed enqueue.

	res := sw.Sweep(context.Background())
	if res.Errors != 1 {
		t.Errorf("expected 1 error from failed enqueue, got %d", res.Errors)
	}
	if res.AgentsNudged != 0 {
		t.Errorf("failed enqueue must not count as nudged; got %d", res.AgentsNudged)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (did the stamp UPDATE fire despite a failed enqueue?): %v", err)
	}
}

// ---------- env override parsing ----------

func TestNudgeSweeperConstructor_PicksUpEnvOverride(t *testing.T) {
	t.Setenv("REQUEST_NUDGE_SWEEPER_INTERVAL_S", "90")
	mock := setupTestDB(t)
	_ = mock
	sw := NewRequestNudgeSweeper(nil)
	if sw.Interval() != 90*time.Second {
		t.Errorf("interval override not picked up: got %v", sw.Interval())
	}
}

func TestNudgeSweeperConstructor_DefaultWhenEnvUnset(t *testing.T) {
	t.Setenv("REQUEST_NUDGE_SWEEPER_INTERVAL_S", "")
	mock := setupTestDB(t)
	_ = mock
	sw := NewRequestNudgeSweeper(nil)
	if sw.Interval() != defaultRequestNudgeInterval {
		t.Errorf("default interval not used: got %v", sw.Interval())
	}
}

// ---------- pg array adapter ----------

func TestStringArray_RoundTrip(t *testing.T) {
	// Scan a Postgres text[] literal, then Value() it back.
	var a stringArray
	if err := a.Scan("{aaa,bbb,ccc}"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(a) != 3 || a[0] != "aaa" || a[2] != "ccc" {
		t.Fatalf("parsed wrong: %#v", a)
	}
	v, err := a.Value()
	if err != nil {
		t.Fatalf("value: %v", err)
	}
	if v.(string) != `{"aaa","bbb","ccc"}` {
		t.Errorf("unexpected literal: %v", v)
	}
}

func TestStringArray_ScanNilAndEmpty(t *testing.T) {
	var a stringArray
	if err := a.Scan(nil); err != nil || a != nil {
		t.Errorf("nil scan should yield nil slice, no error; got %#v err=%v", a, err)
	}
	if err := a.Scan("{}"); err != nil || len(a) != 0 {
		t.Errorf("empty array literal should yield empty slice; got %#v err=%v", a, err)
	}
}

func TestStringArray_ScanQuotedElements(t *testing.T) {
	var a stringArray
	if err := a.Scan(`{"a b","c,d","e\"f"}`); err != nil {
		t.Fatalf("scan quoted: %v", err)
	}
	if len(a) != 3 || a[0] != "a b" || a[1] != "c,d" || a[2] != `e"f` {
		t.Fatalf("quoted parse wrong: %#v", a)
	}
}
