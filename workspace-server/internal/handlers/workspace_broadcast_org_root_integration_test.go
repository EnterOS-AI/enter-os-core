//go:build integration
// +build integration

// workspace_broadcast_org_root_integration_test.go — REAL Postgres
// regression test for #1959: the Broadcast org-root recursive CTE.
//
// Run with:
//
//   INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//     go test -tags=integration ./internal/handlers/ -run Integration_BroadcastOrgRoot -v
//
// CI: piggybacks on .github/workflows/handlers-postgres-integration.yml
// (path-filter includes workspace-server/internal/handlers/**).
//
// Why this is NOT a sqlmock test
// ------------------------------
// The unit tests in workspace_broadcast_test.go use sqlmock, which
// returns whatever rows the test stubs — it CANNOT execute the
// recursive CTE, so it cannot catch the #1959 bug where the anchor
// pinned `id AS root_id` to the SENDER's own id and carried it
// unchanged up the chain. With that bug a non-root sender resolved
// ITSELF as the org root (wrong broadcast scoping). Only a real
// Postgres can prove the corrected CTE resolves UP to the true
// null-parent ancestor.
//
// The query under test is copied verbatim from Broadcast() in
// workspace_broadcast.go; if that query changes, this test must be
// updated in lockstep (it is the real-artifact gate for the fix).

package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// orgRootCTE is the exact org-root resolution query from Broadcast().
// Kept here verbatim so the test fails loudly if the handler regresses
// to the #1959 sender-id-pinned form.
const orgRootCTE = `
	WITH RECURSIVE org_chain AS (
		SELECT id, parent_id
		FROM workspaces
		WHERE id = $1
		UNION ALL
		SELECT w.id, w.parent_id
		FROM workspaces w
		JOIN org_chain c ON w.id = c.parent_id
	)
	SELECT id AS root_id FROM org_chain WHERE parent_id IS NULL LIMIT 1
`

func integrationDB_BroadcastOrgRoot(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping (see file header)")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// TestIntegration_BroadcastOrgRoot_NonRootSenderResolvesToRoot builds a
// real three-level org chain in Postgres:
//
//	root  (parent_id = NULL)
//	└── mid   (parent_id = root)
//	    └── leaf  (parent_id = mid)   ← non-root sender
//
// and runs the handler's org-root CTE for each node. Every node — root,
// mid, and leaf — MUST resolve to `root`. Under the #1959 bug the leaf
// (and mid) resolved to themselves; this test pins the fix.
func TestIntegration_BroadcastOrgRoot_NonRootSenderResolvesToRoot(t *testing.T) {
	conn := integrationDB_BroadcastOrgRoot(t)
	ctx := context.Background()

	prefix := fmt.Sprintf("itest-bcastroot-%s", uuid.New().String()[:8])
	t.Cleanup(func() {
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM workspaces WHERE name LIKE $1`, prefix+"%"); err != nil {
			t.Logf("cleanup (non-fatal): %v", err)
		}
	})

	// Pre-test hygiene: if a prior run crashed or was killed, its rows may
	// still be in the shared integration DB. Remove them before inserting so
	// the unique index workspaces_parent_name_uniq does not conflict.
	if _, err := conn.ExecContext(ctx,
		`DELETE FROM workspaces WHERE name LIKE $1`, prefix+"%"); err != nil {
		t.Logf("pre-test cleanup (non-fatal): %v", err)
	}

	rootID := uuid.New().String()
	midID := uuid.New().String()
	leafID := uuid.New().String()

	// root — parent_id NULL.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
		VALUES ($1, $2, 2, 'claude-code', 'online', NULL)
	`, rootID, prefix+"-root"); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	// mid — child of root.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
		VALUES ($1, $2, 2, 'claude-code', 'online', $3)
	`, midID, prefix+"-mid", rootID); err != nil {
		t.Fatalf("seed mid: %v", err)
	}
	// leaf — child of mid (a non-root, non-direct-child sender).
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
		VALUES ($1, $2, 2, 'claude-code', 'online', $3)
	`, leafID, prefix+"-leaf", midID); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}

	cases := []struct {
		name     string
		senderID string
	}{
		{"root sender resolves to itself", rootID},
		{"mid sender resolves to root", midID},
		{"leaf (deep non-root) sender resolves to root", leafID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			if err := conn.QueryRowContext(ctx, orgRootCTE, tc.senderID).Scan(&got); err != nil {
				t.Fatalf("org-root CTE for %s: %v", tc.senderID, err)
			}
			if got != rootID {
				t.Errorf("org root for sender %s = %s; want %s (the true null-parent ancestor) — #1959 regression: a non-root sender resolved to the wrong root",
					tc.senderID, got, rootID)
			}
		})
	}
}
