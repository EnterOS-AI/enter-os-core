//go:build integration
// +build integration

// approval_gate_integration_test.go — REAL Postgres gate for requireApproval.
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_RequireApproval -v
//
// Why this is NOT a sqlmock test
// ------------------------------
// The whole gate is about row state across calls: a pending request is created
// once and reused (dedup), an approval is consumed exactly once (single-use via
// the conditional UPDATE ... RETURNING), and a different operation context hashes
// to a different request. sqlmock returns whatever the stub says; only a real
// Postgres proves the consume-once semantics and the partial-index lookup.

package handlers

import (
	"context"
	"database/sql"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/approvals"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func TestIntegration_RequireApproval_GateCycle(t *testing.T) {
	url := requireIntegrationDBURL(t)
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	// requireApproval + the broadcaster's structure_events write use the db.DB
	// global; point it at the integration DB and restore afterwards.
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev })
	setupTestRedis(t) // broadcaster publishes to db.RDB; miniredis backs it

	ctx := context.Background()
	b := newTestBroadcaster()

	wsID := uuid.New().String()
	t.Cleanup(func() {
		_, _ = conn.ExecContext(ctx, `DELETE FROM approval_requests WHERE workspace_id = $1`, wsID)
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, wsID)
	})
	// A root workspace (parent_id NULL) — like the platform agent, it has no
	// parent, so the gate's escalation target is the user/canvas. (This branch
	// is off main and has no kind column; the gate is kind-agnostic.)
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, status, runtime, parent_id)
		VALUES ($1, 'Org Concierge', 0, 'online', 'claude-code', NULL)`, wsID); err != nil {
		t.Fatalf("seed root workspace: %v", err)
	}

	action := approvals.ActionSecretWrite
	ctxA := map[string]interface{}{"target": "ws-A"}

	// 1. First call → no approval yet → pending created.
	ok, id1, err := requireApproval(ctx, b, wsID, action, "write secret ws-A", ctxA)
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if ok {
		t.Fatal("call 1: approved=true, want false (no approval exists yet)")
	}

	// 2. Same operation again → must REUSE the same pending row (dedup), not flood.
	ok, id2, err := requireApproval(ctx, b, wsID, action, "delete ws-A", ctxA)
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if ok || id2 != id1 {
		t.Fatalf("call 2: ok=%v id2=%s, want false and id2==id1(%s) (dedup)", ok, id2, id1)
	}
	var nPending int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM approval_requests WHERE workspace_id=$1 AND status='pending'`, wsID).Scan(&nPending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if nPending != 1 {
		t.Fatalf("pending rows = %d, want 1 (dedup must not flood)", nPending)
	}

	// 3. A human approves it (simulating the Decide handler).
	if _, err := conn.ExecContext(ctx,
		`UPDATE approval_requests SET status='approved', decided_by='human', decided_at=now() WHERE id=$1`, id1); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// 4. Now the gate consumes the approval and lets the op proceed.
	ok, consumedID, err := requireApproval(ctx, b, wsID, action, "delete ws-A", ctxA)
	if err != nil {
		t.Fatalf("call 4: %v", err)
	}
	if !ok || consumedID != id1 {
		t.Fatalf("call 4: ok=%v consumedID=%s, want true and id1(%s)", ok, consumedID, id1)
	}

	// 5. Single-use: the SAME approval cannot be replayed — the next call is
	//    pending again (a fresh request), not approved.
	ok, id5, err := requireApproval(ctx, b, wsID, action, "delete ws-A", ctxA)
	if err != nil {
		t.Fatalf("call 5: %v", err)
	}
	if ok {
		t.Fatal("call 5: approved=true — a consumed approval was replayed")
	}
	if id5 == id1 {
		t.Fatal("call 5: reused the consumed request id; want a new pending request")
	}

	// 6. Context isolation: an approval for ws-A must not authorize ws-B.
	//    Approve the ws-A request, then a ws-B op must still be pending.
	if _, err := conn.ExecContext(ctx,
		`UPDATE approval_requests SET status='approved', decided_at=now() WHERE id=$1`, id5); err != nil {
		t.Fatalf("approve id5: %v", err)
	}
	ok, _, err = requireApproval(ctx, b, wsID, action, "delete ws-B", map[string]interface{}{"target": "ws-B"})
	if err != nil {
		t.Fatalf("call 6: %v", err)
	}
	if ok {
		t.Fatal("call 6: ws-B proceeded on a ws-A approval — context isolation broken")
	}
}
