//go:build integration
// +build integration

// workspace_create_name_integration_test.go — REAL Postgres
// integration test for the duplicate-name auto-suffix retry
// helper.
//
// Run with:
//
//   INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//     go test -tags=integration ./internal/handlers/ -run Integration_WorkspaceCreate_NameRetry -v
//
// CI: piggybacks on .github/workflows/handlers-postgres-integration.yml
// (path-filter includes workspace-server/internal/handlers/**, which
// covers this file).
//
// Why this is NOT a sqlmock test
// ------------------------------
// sqlmock CANNOT verify the actual partial-unique-index
// behaviour. The unit tests in workspace_create_name_test.go pin
// the helper's retry contract under a fake driver error, but only
// a real Postgres can confirm:
//
//   - The migration 20260506000000 actually created the index.
//   - lib/pq emits SQLSTATE 23505 with Constraint =
//     "workspaces_parent_name_uniq" (not a synonym, not the message
//     fallback).
//   - The COALESCE(parent_id, sentinel) target collapses NULL
//     parent_ids so two root-level workspaces with the same name
//     collide as the migration intends.
//   - The WHERE status != 'removed' partial filter exempts
//     tombstoned rows from blocking re-use.
//
// Per feedback_mandatory_local_e2e_before_ship: ship-mode requires
// the helper to be exercised against a real Postgres before the PR
// merges.

package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// integrationDB_WorkspaceCreateName opens $INTEGRATION_DB_URL,
// applies the parent-name partial unique index if missing
// (idempotent), wipes the test row range, and returns the
// connection.
//
// We intentionally do NOT wipe every row in `workspaces` because
// the integration DB may be shared with other tests in this
// package; we tag inserts with a per-test UUID prefix and clean up
// only those.
func integrationDB_WorkspaceCreateName(t *testing.T) *sql.DB {
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

	// Ensure the constraint we're testing exists. If the migration
	// already ran (the dev/CI default), this is a fast no-op via
	// IF NOT EXISTS. If the test DB was created from a snapshot
	// taken before 2026-05-06, we apply it here.
	if _, err := conn.ExecContext(context.Background(), `
		CREATE UNIQUE INDEX IF NOT EXISTS workspaces_parent_name_uniq
			ON workspaces (
				COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid),
				name
			)
			WHERE status != 'removed'
	`); err != nil {
		t.Fatalf("ensure constraint: %v", err)
	}
	return conn
}

// cleanupTestRows removes any rows inserted under the given name
// prefix. Called via t.Cleanup so a failing test still leaves the
// DB usable for the next run.
func cleanupTestRows(t *testing.T, conn *sql.DB, namePrefix string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM workspaces WHERE name LIKE $1`, namePrefix+"%"); err != nil {
		t.Logf("cleanup (non-fatal): %v", err)
	}
}

// TestIntegration_WorkspaceCreate_NameRetry_AutoSuffixesOnCollision
// exercises the helper end-to-end against a real Postgres:
//
//  1. INSERT a row with name "<prefix>-Repro" — succeeds.
//  2. Run insertWorkspaceWithNameRetry with the same name —
//     partial-unique violation fires, helper retries with
//     " (2)", that succeeds.
//  3. SELECT the row by id, confirm name = "<prefix>-Repro (2)".
//  4. Run helper AGAIN — second collision, helper retries with
//     " (3)".
//
// This is the live-test that proves the partial-index behaviour
// matches the migration's intent — sqlmock cannot reach this depth.
func TestIntegration_WorkspaceCreate_NameRetry_AutoSuffixesOnCollision(t *testing.T) {
	conn := integrationDB_WorkspaceCreateName(t)
	ctx := context.Background()

	// Per-test prefix so concurrent test runs don't collide on the
	// shared integration DB; also tags rows for cleanupTestRows.
	prefix := fmt.Sprintf("itest-namesuffix-%s", uuid.New().String()[:8])
	t.Cleanup(func() { cleanupTestRows(t, conn, prefix) })

	baseName := prefix + "-Repro"

	// Step 1 — seed an existing row to collide against. Uses a
	// minimal column set (the production INSERT has many more
	// columns; we only need the ones the partial-unique index
	// targets + the NOT NULL columns required by the schema).
	firstID := uuid.New().String()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status)
		VALUES ($1, $2, 2, 'claude-code', 'provisioning')
	`, firstID, baseName); err != nil {
		t.Fatalf("seed first row: %v", err)
	}

	// Step 2 — same name, helper must auto-suffix to " (2)".
	beginTx := func(ctx context.Context) (*sql.Tx, error) { return conn.BeginTx(ctx, nil) }

	tx, err := beginTx(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	secondID := uuid.New().String()
	query := `
		INSERT INTO workspaces (id, name, tier, runtime, status)
		VALUES ($1, $2, 2, 'claude-code', 'provisioning')
	`
	args := []any{secondID, baseName}
	persistedName, finalTx, err := insertWorkspaceWithNameRetry(
		ctx, tx, beginTx, baseName, 1, query, args,
	)
	if err != nil {
		t.Fatalf("retry helper on second insert: %v", err)
	}
	if persistedName != baseName+" (2)" {
		t.Fatalf("persistedName = %q, want exactly %q", persistedName, baseName+" (2)")
	}
	if err := finalTx.Commit(); err != nil {
		t.Fatalf("commit second: %v", err)
	}

	// Step 3 — verify DB state matches helper's return value.
	var actualName string
	if err := conn.QueryRowContext(ctx,
		`SELECT name FROM workspaces WHERE id = $1`, secondID).Scan(&actualName); err != nil {
		t.Fatalf("re-select second: %v", err)
	}
	if actualName != baseName+" (2)" {
		t.Fatalf("DB row name = %q, want exactly %q (helper return value lied to caller)",
			actualName, baseName+" (2)")
	}

	// Step 4 — third collision must produce " (3)".
	tx3, err := beginTx(ctx)
	if err != nil {
		t.Fatalf("begin tx3: %v", err)
	}
	thirdID := uuid.New().String()
	args3 := []any{thirdID, baseName}
	persistedName3, finalTx3, err := insertWorkspaceWithNameRetry(
		ctx, tx3, beginTx, baseName, 1, query, args3,
	)
	if err != nil {
		t.Fatalf("retry helper on third insert: %v", err)
	}
	if persistedName3 != baseName+" (3)" {
		t.Fatalf("third persistedName = %q, want exactly %q",
			persistedName3, baseName+" (3)")
	}
	if err := finalTx3.Commit(); err != nil {
		t.Fatalf("commit third: %v", err)
	}
}

// TestIntegration_WorkspaceCreate_NameRetry_TombstonedRowDoesNotCollide
// confirms the partial-index `WHERE status != 'removed'` predicate
// matches the helper's assumptions: a deleted (status='removed')
// workspace MUST NOT block re-creation under the same name.
//
// This is the post-2026-05-06 contract /org/import already relies
// on; the helper inherits it for the Canvas Create path. A
// regression in the migration's predicate would silently break
// both surfaces.
func TestIntegration_WorkspaceCreate_NameRetry_TombstonedRowDoesNotCollide(t *testing.T) {
	conn := integrationDB_WorkspaceCreateName(t)
	ctx := context.Background()

	prefix := fmt.Sprintf("itest-tombstone-%s", uuid.New().String()[:8])
	t.Cleanup(func() { cleanupTestRows(t, conn, prefix) })

	baseName := prefix + "-RevivedName"

	// Seed a row, then tombstone it.
	firstID := uuid.New().String()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status)
		VALUES ($1, $2, 2, 'claude-code', 'removed')
	`, firstID, baseName); err != nil {
		t.Fatalf("seed tombstoned row: %v", err)
	}

	// New INSERT with the same name MUST succeed without any
	// suffix — the partial index excludes the tombstoned row.
	beginTx := func(ctx context.Context) (*sql.Tx, error) { return conn.BeginTx(ctx, nil) }
	tx, err := beginTx(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	secondID := uuid.New().String()
	query := `
		INSERT INTO workspaces (id, name, tier, runtime, status)
		VALUES ($1, $2, 2, 'claude-code', 'provisioning')
	`
	args := []any{secondID, baseName}
	persistedName, finalTx, err := insertWorkspaceWithNameRetry(
		ctx, tx, beginTx, baseName, 1, query, args,
	)
	if err != nil {
		t.Fatalf("retry helper after tombstone: %v", err)
	}
	if persistedName != baseName {
		t.Fatalf("persistedName = %q, want %q (tombstoned row should NOT force a suffix)",
			persistedName, baseName)
	}
	if err := finalTx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
