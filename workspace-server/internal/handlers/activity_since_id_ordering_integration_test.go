//go:build integration
// +build integration

// activity_since_id_ordering_integration_test.go — REAL Postgres proof that
// the poll-mode since_id activity feed (#2339) is DETERMINISTICALLY ordered
// even when multiple rows collide on the same created_at microsecond.
//
// This is the test that the original bug report mis-labeled a "flake".
// sqlmock cannot catch it: sqlmock returns rows in the order the test stuffs
// them, so it can never reveal a non-deterministic ORDER BY. Only a real
// planner over real same-created_at rows exposes it.
//
// Run with (same harness as activity_delegation_a2a_integration_test.go):
//
//	docker run --rm -d --name pg-integration \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	# apply migrations (incl. 20260604000000_activity_logs_seq.up.sql) then:
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_SinceID
//
// WATCH-IT-FAIL: against the pre-fix handler (ORDER BY created_at only, no
// seq tiebreaker, and `created_at > cursor` strict) this test is unstable —
// the equal-created_at rows come back in arbitrary planner order so the
// ordered-id assertion fails intermittently, and the same-microsecond
// boundary row is dropped so the count assertion fails. With the fix
// (ORDER BY created_at, seq + tuple cursor) it is green every run.

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// seedActivityRowAt inserts one activity_logs row with an explicit created_at
// (so the test can force microsecond-equal collisions) and a unique summary;
// returns the generated id. seq is left to the IDENTITY default — Postgres
// assigns it in INSERT order, which is the deterministic tiebreaker under test.
// db.DB has been hot-swapped to the integration connection by
// integrationDB_ActivityDelegationA2A(t) in the calling test.
func seedActivityRowAt(t *testing.T, wsID, summary string, createdAt time.Time) string {
	t.Helper()
	var id string
	err := db.DB.QueryRowContext(context.Background(), `
		INSERT INTO activity_logs (workspace_id, activity_type, summary, status, created_at)
		VALUES ($1, 'a2a_receive', $2, 'ok', $3)
		RETURNING id
	`, wsID, summary, createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("seedActivityRowAt(%q): %v", summary, err)
	}
	return id
}

// TestIntegration_SinceID_StableOrderingSameMicrosecond proves the feed is
// deterministic when rows share a created_at, AND that the same-microsecond
// boundary row immediately after the cursor is NOT dropped.
func TestIntegration_SinceID_StableOrderingSameMicrosecond(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	_ = conn
	wsID := seedWorkspace(t, conn, "test-2151-sinceid-ordering")

	// One earlier row to serve as the cursor (the "last processed" row).
	tCursor := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	cursorID := seedActivityRowAt(t, wsID, "cursor-row", tCursor)

	// Three rows that ALL collide on the exact same created_at microsecond,
	// inserted in a known order. Pre-fix, ORDER BY created_at alone returns
	// these in arbitrary planner order.
	tEqual := time.Date(2026, 6, 4, 12, 0, 1, 0, time.UTC)
	idA := seedActivityRowAt(t, wsID, "equal-A", tEqual)
	idB := seedActivityRowAt(t, wsID, "equal-B", tEqual)
	idCc := seedActivityRowAt(t, wsID, "equal-C", tEqual)
	wantOrder := []string{idA, idB, idCc}

	// Drive the handler exactly as a polling client would.
	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	q := c.Request.URL.Query()
	q.Set("since_id", cursorID)
	q.Set("type", "a2a_receive")
	q.Set("limit", "10")
	c.Request.URL.RawQuery = q.Encode()

	h.List(c)
	if w.Code != http.StatusOK {
		t.Fatalf("List returned %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// All three equal-created_at rows must be present (boundary not dropped)
	// and the cursor row itself must be excluded (strictly-after).
	if len(resp) != len(wantOrder) {
		t.Fatalf("expected %d rows after cursor (the 3 equal-created_at rows), got %d: %+v",
			len(wantOrder), len(resp), resp)
	}

	gotOrder := make([]string, len(resp))
	for i, row := range resp {
		idVal, _ := row["id"].(string)
		gotOrder[i] = idVal
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("non-deterministic ordering: got id order %v, want %v (seq tiebreaker not applied)",
				gotOrder, wantOrder)
		}
	}
}

// TestIntegration_SinceID_BoundaryRowSameMicrosecondNotSkipped isolates the
// cursor-boundary bug: a row written in the SAME microsecond as the cursor
// row (but with a higher seq) must still be returned. Pre-fix the strict
// `created_at > cursor` filter silently dropped it.
func TestIntegration_SinceID_BoundaryRowSameMicrosecondNotSkipped(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	_ = conn
	wsID := seedWorkspace(t, conn, "test-2151-sinceid-boundary")

	tSame := time.Date(2026, 6, 4, 13, 0, 0, 0, time.UTC)
	// Cursor row and the next row share the exact same created_at; the next
	// row is inserted afterwards so it gets a higher seq.
	cursorID := seedActivityRowAt(t, wsID, "boundary-cursor", tSame)
	nextID := seedActivityRowAt(t, wsID, "boundary-next-same-us", tSame)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	q := c.Request.URL.Query()
	q.Set("since_id", cursorID)
	q.Set("type", "a2a_receive")
	q.Set("limit", "10")
	c.Request.URL.RawQuery = q.Encode()

	h.List(c)
	if w.Code != http.StatusOK {
		t.Fatalf("List returned %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("same-microsecond boundary row dropped: expected exactly the 1 next row, got %d rows: %+v",
			len(resp), resp)
	}
	if got, _ := resp[0]["id"].(string); got != nextID {
		t.Fatalf("expected boundary row id %s, got %s", nextID, got)
	}
}
