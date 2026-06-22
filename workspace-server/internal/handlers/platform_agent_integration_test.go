//go:build integration
// +build integration

// platform_agent_integration_test.go — REAL Postgres gate for installPlatformAgent.
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_PlatformAgentInstall -v
//
// CI: handlers-postgres-integration workflow (handlers + migrations path filter).
//
// Why this is NOT a sqlmock test
// ------------------------------
// The install re-parents the org's existing root under the platform agent AND
// moves the org-anchor references (org_api_tokens.org_id, org_plugin_allowlist.
// org_id) from old root to platform agent, atomically. The whole point is the
// post-transaction row state: orgRootID() must resolve every node to the platform
// agent, sameOrg() must still hold, and the auth/allowlist anchors must point at
// the new root. Only a real Postgres can prove that; sqlmock cannot.

package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func integrationDB_PlatformAgentInstall(t *testing.T) *sql.DB {
	t.Helper()
	url := requireIntegrationDBURL(t)
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

// TestIntegration_PlatformAgentInstall_ReparentsRootAndMovesAnchors builds a
// real org in Postgres:
//
//	root (parent_id NULL, kind=workspace)
//	└── child
//	+ an org_api_token  anchored to root
//	+ an org_plugin_allowlist entry anchored to root
//
// then installs the platform agent and asserts:
//   - the platform agent is the new sole root (kind=platform, parent_id NULL);
//   - the old root is re-parented under it; the child is untouched;
//   - both org-anchor references now point at the platform agent;
//   - a second install is a no-op (idempotent).
func TestIntegration_PlatformAgentInstall_ReparentsRootAndMovesAnchors(t *testing.T) {
	conn := integrationDB_PlatformAgentInstall(t)
	ctx := context.Background()

	tag := uuid.New().String()[:8]
	prefix := fmt.Sprintf("itest-pinstall-%s", tag)
	rootID := uuid.New().String()
	childID := uuid.New().String()
	platformID := uuid.New().String()

	paName := "Org Concierge " + tag
	cleanup := func() {
		_, _ = conn.ExecContext(ctx, `DELETE FROM org_plugin_allowlist WHERE plugin_name = $1`, prefix+"-plugin")
		_, _ = conn.ExecContext(ctx, `DELETE FROM org_api_tokens WHERE prefix = $1`, tag)
		// child + old root (prefixed names) first, then the platform agent by id
		// (root.parent_id references it, so it must go last).
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name LIKE $1`, prefix+"%")
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name = $1`, paName)
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, platformID)
	}
	t.Cleanup(cleanup)
	cleanup()

	// Seed org tree.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
		VALUES ($1, $2, 2, 'claude-code', 'online', NULL)`, rootID, prefix+"-root"); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
		VALUES ($1, $2, 2, 'claude-code', 'online', $3)`, childID, prefix+"-child", rootID); err != nil {
		t.Fatalf("seed child: %v", err)
	}
	// Org-anchor rows keyed to the OLD root.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO org_api_tokens (token_hash, prefix, name, org_id)
		VALUES ($1, $2, $3, $4)`,
		[]byte("hash-"+tag), tag, prefix+"-tok", rootID); err != nil {
		t.Fatalf("seed org_api_token: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO org_plugin_allowlist (org_id, plugin_name, enabled_by)
		VALUES ($1, $2, $3)`, rootID, prefix+"-plugin", childID); err != nil {
		t.Fatalf("seed allowlist: %v", err)
	}

	// Install.
	if err := installPlatformAgent(ctx, conn, platformID, paName); err != nil {
		t.Fatalf("install: %v", err)
	}

	assertState := func(stage string) {
		// platform agent is a kind=platform root.
		var kind string
		var parent sql.NullString
		if err := conn.QueryRowContext(ctx,
			`SELECT kind, parent_id FROM workspaces WHERE id = $1`, platformID).Scan(&kind, &parent); err != nil {
			t.Fatalf("[%s] read platform agent: %v", stage, err)
		}
		if kind != "platform" || parent.Valid {
			t.Fatalf("[%s] platform agent kind=%q parent=%v, want platform/NULL", stage, kind, parent)
		}
		// old root re-parented under the platform agent.
		var rootParent sql.NullString
		if err := conn.QueryRowContext(ctx,
			`SELECT parent_id FROM workspaces WHERE id = $1`, rootID).Scan(&rootParent); err != nil {
			t.Fatalf("[%s] read old root: %v", stage, err)
		}
		if !rootParent.Valid || rootParent.String != platformID {
			t.Fatalf("[%s] old root parent=%v, want %s", stage, rootParent, platformID)
		}
		// child untouched.
		var childParent sql.NullString
		if err := conn.QueryRowContext(ctx,
			`SELECT parent_id FROM workspaces WHERE id = $1`, childID).Scan(&childParent); err != nil {
			t.Fatalf("[%s] read child: %v", stage, err)
		}
		if !childParent.Valid || childParent.String != rootID {
			t.Fatalf("[%s] child parent=%v, want %s (unchanged)", stage, childParent, rootID)
		}
		// org-anchor references moved to the platform agent.
		var tokOrg, alOrg string
		if err := conn.QueryRowContext(ctx,
			`SELECT org_id FROM org_api_tokens WHERE prefix = $1`, tag).Scan(&tokOrg); err != nil {
			t.Fatalf("[%s] read token org_id: %v", stage, err)
		}
		if tokOrg != platformID {
			t.Fatalf("[%s] org_api_tokens.org_id=%s, want %s", stage, tokOrg, platformID)
		}
		if err := conn.QueryRowContext(ctx,
			`SELECT org_id FROM org_plugin_allowlist WHERE plugin_name = $1`, prefix+"-plugin").Scan(&alOrg); err != nil {
			t.Fatalf("[%s] read allowlist org_id: %v", stage, err)
		}
		if alOrg != platformID {
			t.Fatalf("[%s] org_plugin_allowlist.org_id=%s, want %s", stage, alOrg, platformID)
		}
		// orgRootID + sameOrg now resolve everything to the platform agent.
		got, err := orgRootID(ctx, conn, childID)
		if err != nil {
			t.Fatalf("[%s] orgRootID(child): %v", stage, err)
		}
		if got != platformID {
			t.Fatalf("[%s] orgRootID(child)=%s, want %s", stage, got, platformID)
		}
		same, err := sameOrg(ctx, conn, childID, platformID)
		if err != nil || !same {
			t.Fatalf("[%s] sameOrg(child, platform)=%v err=%v, want true", stage, same, err)
		}
	}

	assertState("first install")

	// Idempotent: second install must not error or change state.
	if err := installPlatformAgent(ctx, conn, platformID, paName); err != nil {
		t.Fatalf("second install (idempotent): %v", err)
	}
	assertState("second install")

	// Neither seeded team node is a root any more — the platform agent is.
	var nRoots int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM workspaces WHERE parent_id IS NULL AND id IN ($1, $2)`,
		rootID, childID).Scan(&nRoots); err != nil {
		t.Fatalf("count roots: %v", err)
	}
	if nRoots != 0 {
		t.Fatalf("team roots after install = %d, want 0 (old root re-parented under platform agent)", nRoots)
	}
}

// TestIntegration_PlatformAgentInstall_MultiRootAllowlistDedup guards the
// UNIQUE(org_id, plugin_name) collision on org_plugin_allowlist. When N>1
// old roots have allowlisted the SAME plugin, a plain UPDATE…SET org_id
// collides 23505. The INSERT…SELECT…ON CONFLICT DO NOTHING + DELETE leftovers
// path must survive this deterministically (core#2508).
func TestIntegration_PlatformAgentInstall_MultiRootAllowlistDedup(t *testing.T) {
	conn := integrationDB_PlatformAgentInstall(t)
	ctx := context.Background()

	tag := uuid.New().String()[:8]
	prefix := fmt.Sprintf("itest-pallow-%s", tag)
	rootA := uuid.New().String()
	rootB := uuid.New().String()
	platformID := uuid.New().String()

	paName := "Org Concierge " + tag
	cleanup := func() {
		_, _ = conn.ExecContext(ctx, `DELETE FROM org_plugin_allowlist WHERE plugin_name = $1`, prefix+"-plugin")
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name LIKE $1`, prefix+"%")
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name = $1`, paName)
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, platformID)
	}
	t.Cleanup(cleanup)
	cleanup()

	// Seed TWO old roots with the SAME plugin allowlisted.
	for _, rid := range []string{rootA, rootB} {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
			VALUES ($1, $2, 2, 'claude-code', 'online', NULL)`, rid, prefix+"-"+rid[:8]); err != nil {
			t.Fatalf("seed root %s: %v", rid, err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO org_plugin_allowlist (org_id, plugin_name, enabled_by)
			VALUES ($1, $2, $3)`, rid, prefix+"-plugin", rid); err != nil {
			t.Fatalf("seed allowlist for %s: %v", rid, err)
		}
	}

	// Install must NOT fail with 23505 unique violation.
	if err := installPlatformAgent(ctx, conn, platformID, paName); err != nil {
		t.Fatalf("install with duplicate allowlist: %v", err)
	}

	// The platform agent must end up with exactly ONE row for the plugin.
	var count int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM org_plugin_allowlist WHERE org_id = $1`, platformID).Scan(&count); err != nil {
		t.Fatalf("count platform-agent allowlist: %v", err)
	}
	if count != 1 {
		t.Fatalf("platform-agent allowlist count = %d, want 1 (deduped from 2 old roots)", count)
	}

	// Old roots must have ZERO allowlist rows left.
	for _, rid := range []string{rootA, rootB} {
		var left int
		if err := conn.QueryRowContext(ctx,
			`SELECT count(*) FROM org_plugin_allowlist WHERE org_id = $1`, rid).Scan(&left); err != nil {
			t.Fatalf("count old-root allowlist %s: %v", rid, err)
		}
		if left != 0 {
			t.Fatalf("old root %s still has %d allowlist rows, want 0 (leftovers deleted)", rid, left)
		}
	}
}

// TestIntegration_PlatformAgentInstall_StatusNotOnline prevents the green-dot
// lie: a freshly inserted platform-agent row must NOT claim status='online'
// when there is no container yet (core#2508).
func TestIntegration_PlatformAgentInstall_StatusNotOnline(t *testing.T) {
	conn := integrationDB_PlatformAgentInstall(t)
	ctx := context.Background()

	platformID := uuid.New().String()
	paName := "Org Concierge " + platformID[:8]

	cleanup := func() {
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name = $1`, paName)
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, platformID)
	}
	t.Cleanup(cleanup)
	cleanup()

	if err := installPlatformAgent(ctx, conn, platformID, paName); err != nil {
		t.Fatalf("install: %v", err)
	}

	var status string
	if err := conn.QueryRowContext(ctx,
		`SELECT status FROM workspaces WHERE id = $1`, platformID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status == "online" {
		t.Fatalf("fresh platform-agent status=%q, must not be 'online' (no container yet)", status)
	}
	if status != "offline" {
		// Allow future changes, but catch the specific lie.
		t.Logf("fresh platform-agent status=%q (expected 'offline', but accepting non-online)", status)
	}
}

// TestIntegration_PlatformAgentInstall_OnConflictUpdatesRuntime locks in
// the core#2496 root-fix. Pre-#2496, the ON CONFLICT clause on the
// platform-agent upsert only set kind and parent_id; the runtime column
// was left at its INSERT-time value (the schema default). So if a
// platform-agent row had been self-registered first (creating a row with
// the schema default runtime) and the install endpoint was then called,
// the runtime would be left STALE — not updated to 'claude-code'.
//
// #2496 added `runtime = 'claude-code'` to the ON CONFLICT DO UPDATE
// SET clause, so the runtime is now corrected on every install, not just
// on the initial insert.
//
// This test seeds a platform-agent row with a NON-canonical runtime
// ('codex' — anything other than 'claude-code'), then calls
// installPlatformAgent. The post-install assertion is: the runtime
// MUST be 'claude-code'. Pre-#2496, this assertion would fail; post-#2496,
// the ON CONFLICT clause resets it.
//
// We also test the "fresh insert" path (no prior row) to confirm the
// regression is specifically about the conflict-path; if a future refactor
// accidentally drops the runtime from the INSERT VALUES too, that case
// catches it.
func TestIntegration_PlatformAgentInstall_OnConflictUpdatesRuntime(t *testing.T) {
	conn := integrationDB_PlatformAgentInstall(t)
	ctx := context.Background()

	tag := uuid.New().String()[:8]
	platformID := uuid.New().String()
	paName := "Org Concierge " + tag
	staleRuntime := "codex" // deliberately non-canonical

	// The schema has `uniq_workspaces_one_platform_root` (UNIQUE
	// partial index allowing at most one row with kind='platform' AND
	// parent_id IS NULL per database). A prior sibling test in this
	// package (e.g. TestIntegration_PlatformAgentInstall_ReparentsRoot
	// AndMovesAnchors) leaves a platform-agent row behind; its cleanup
	// deletes by its OWN uuid+name, so by the time this test runs,
	// the slot may be occupied by a prior test's row (depending on
	// test scheduling).
	//
	// To make this test robust to test ordering, the pre-seed
	// cleanup DOWNGRADES any existing platform root to 'workspace'
	// (matching what installPlatformAgent itself does for prior
	// roots — see its step 0). We use UPDATE not DELETE because
	// child workspaces may have `parent_id` pointing at the platform
	// root, and a DELETE would trip `workspaces_parent_id_fkey`.
	// Downgrading (UPDATE kind='workspace') is FK-safe and clears
	// the uniq-platform-root slot.
	cleanup := func() {
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name = $1`, paName)
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, platformID)
		// Belt-and-suspenders: also downgrade any other platform-agent
		// row left by a sibling test, so the next test's seed slot
		// is free.
		_, _ = conn.ExecContext(ctx, `UPDATE workspaces SET kind = 'workspace' WHERE kind = 'platform' AND parent_id IS NULL AND id <> $1`, platformID)
	}
	t.Cleanup(cleanup)
	// Pre-seed: clear the uniq-platform-root slot by downgrading
	// any existing platform root to 'workspace'. This is FK-safe
	// and matches installPlatformAgent's own step 0 (so we're
	// exercising the same shape the production code uses).
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET kind = 'workspace' WHERE kind = 'platform' AND parent_id IS NULL`); err != nil {
		t.Fatalf("pre-seed: downgrade existing platform-agent rows: %v", err)
	}
	cleanup()

	// Case 1: ON CONFLICT path. Seed the platform-agent row with a STALE
	// runtime (simulates a self-registered row with the schema default).
	// The seed is INSERT (not UPSERT) to deliberately NOT use the install
	// endpoint's path — we're pre-arranging the conflict scenario.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, kind, tier, status, runtime, parent_id)
		VALUES ($1, $2, 'platform', 0, 'offline', $3, NULL)`,
		platformID, paName, staleRuntime); err != nil {
		t.Fatalf("seed stale platform agent: %v", err)
	}

	// Sanity: the seeded row has the stale runtime (not the canonical
	// 'claude-code'). If this assertion fails, the seed is wrong; fix
	// the seed, not the test assertion.
	var seededRuntime string
	if err := conn.QueryRowContext(ctx,
		`SELECT runtime FROM workspaces WHERE id = $1`, platformID).Scan(&seededRuntime); err != nil {
		t.Fatalf("read seeded runtime: %v", err)
	}
	if seededRuntime != staleRuntime {
		t.Fatalf("seed precondition: runtime=%q, want %q (the test would be meaningless if the seed already had 'claude-code')",
			seededRuntime, staleRuntime)
	}

	// Now call installPlatformAgent. Per the pre-#2496 bug, the
	// ON CONFLICT DO UPDATE SET clause would have left runtime stuck
	// at 'codex'. Per the #2496 fix, it must now be 'claude-code'.
	if err := installPlatformAgent(ctx, conn, platformID, paName); err != nil {
		t.Fatalf("install: %v", err)
	}

	// PRIMARY ASSERTION: the runtime is 'claude-code' after install.
	// Pre-#2496, the runtime would be 'codex' (stale). Post-#2496, the
	// ON CONFLICT clause sets it to 'claude-code'.
	var postInstallRuntime string
	if err := conn.QueryRowContext(ctx,
		`SELECT runtime FROM workspaces WHERE id = $1`, platformID).Scan(&postInstallRuntime); err != nil {
		t.Fatalf("read post-install runtime: %v", err)
	}
	if postInstallRuntime != "claude-code" {
		t.Fatalf("core#2496 regression: post-install runtime=%q, want 'claude-code'. "+
			"This means the ON CONFLICT DO UPDATE SET clause is NOT setting runtime on the conflict path. "+
			"Pre-#2496, the clause was: `ON CONFLICT (id) DO UPDATE SET kind = 'platform', parent_id = NULL` "+
			"(no runtime update). Post-#2496, it MUST be: `... SET kind = 'platform', runtime = 'claude-code', parent_id = NULL`.",
			postInstallRuntime)
	}

	// Sanity: a second install (still the conflict path) keeps runtime
	// at 'claude-code' (idempotency assertion).
	if err := installPlatformAgent(ctx, conn, platformID, paName); err != nil {
		t.Fatalf("second install: %v", err)
	}
	var post2Runtime string
	if err := conn.QueryRowContext(ctx,
		`SELECT runtime FROM workspaces WHERE id = $1`, platformID).Scan(&post2Runtime); err != nil {
		t.Fatalf("read post-second-install runtime: %v", err)
	}
	if post2Runtime != "claude-code" {
		t.Fatalf("idempotent: post-second-install runtime=%q, want 'claude-code'", post2Runtime)
	}

	// Case 2: Fresh insert path. Verify the runtime='claude-code' is
	// also set on the INSERT VALUES, not just on conflict. This guards
	// against a future refactor that moves the runtime to the conflict
	// clause only and removes it from the INSERT — a regression that
	// would let a self-registering row skip the runtime entirely.
	freshID := uuid.New().String()
	freshName := "Org Concierge fresh " + tag
	if err := installPlatformAgent(ctx, conn, freshID, freshName); err != nil {
		t.Fatalf("fresh install: %v", err)
	}
	var freshRuntime string
	if err := conn.QueryRowContext(ctx,
		`SELECT runtime FROM workspaces WHERE id = $1`, freshID).Scan(&freshRuntime); err != nil {
		t.Fatalf("read fresh runtime: %v", err)
	}
	if freshRuntime != "claude-code" {
		t.Fatalf("fresh insert path: runtime=%q, want 'claude-code'", freshRuntime)
	}
	_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, freshID)
}
