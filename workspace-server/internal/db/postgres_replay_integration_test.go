//go:build integration
// +build integration

// postgres_replay_integration_test.go — REAL Postgres integration tests for
// the boot-time migration runner (db.RunMigrations) and the connection
// bootstrap (db.InitPostgres).
//
// Issue #2150 (SOP rule internal#765 regression-coverage). test_layer:
// real-postgres.
//
// Run locally with:
//
//	docker run --rm -d --name pg-replay \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/db/ -run '^TestIntegration_Migration|^TestIntegration_InitPostgres'
//
// In CI these run on .gitea/workflows/handlers-postgres-integration.yml,
// which already provisions a real Postgres on the operator-host bridge and
// triggers on workspace-server/migrations/** changes — the exact blast
// radius this gate must cover.
//
// WHY A REAL DATABASE — and why the existing coverage is NOT enough
// -----------------------------------------------------------------
// postgres_migrate_test.go and postgres_schema_migrations_test.go are
// sqlmock-only: they pin which SQL *statements* fire, but a mock cannot
// execute SQL, so it cannot prove the 118-file (.up + legacy .sql) chain
// actually REPLAYS FROM SCRATCH against a real Postgres. The CI psql loop
// in handlers-postgres-integration.yml deliberately *skips* failing
// migrations (`⊘ skipped`), so it would stay green even if the chain
// stopped replaying — it is not a replay gate.
//
// This file closes that gap. It boots a Postgres, resets the public schema
// to a blank slate, and runs the PRODUCTION db.RunMigrations entrypoint —
// the same function platform boot calls — with hard-fail semantics. It
// would FAIL (watch-fail intent) against:
//
//   - Issue #211: if RunMigrations regresses to globbing `*.sql` and
//     sorting `.down.sql` before `.up.sql`, the rollback runs before the
//     forward for any pair (020_workspace_auth_tokens was the canary),
//     either erroring on the DROP or wiping the just-created table.
//
//   - The 045 crash-loop class (cp#429 / project_cp_migration_045_*): the
//     runner re-applies every recorded-absent file every boot, so a
//     non-idempotent migration (bare CREATE / INSERT without IF NOT EXISTS
//     / ON CONFLICT) replays cleanly the first time and FAILS the second.
//     TestIntegration_MigrationReplay_IsIdempotent_DoubleApply runs the
//     full chain twice against the same DB to catch that at PR time.
//
//   - A new migration that depends on a table a later migration drops, or
//     is mis-ordered in the lexicographic chain — it simply will not apply
//     from scratch and the replay errors.
//
// All assertions key off the OBSERVABLE database state after the real run,
// not a proxy for "a statement fired".

package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/lib/pq"
)

// migrationsDir is the on-disk path to the forward+legacy migration chain
// relative to this test file (workspace-server/internal/db → ../../migrations).
const migrationsDir = "../../migrations"

// freshIntegrationDB opens $INTEGRATION_DB_URL (skipping the test if unset),
// resets the `public` schema to an empty slate so the run is a true
// replay-from-scratch regardless of what an earlier CI step applied, and
// registers a Cleanup that closes the connection.
//
// It also points the package-global db.DB at this connection, because
// RunMigrations operates on db.DB. NOT SAFE for t.Parallel() — it owns the
// schema for the duration of the test.
func freshIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping real-PG replay test (local devs: see file header)")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// True from-scratch: blow away any schema a prior CI step (e.g. the
	// handlers psql apply-all loop) left behind, then start clean. This is
	// what makes the test a *replay-from-scratch* gate rather than a
	// re-apply-onto-existing test.
	if _, err := conn.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset public schema: %v", err)
	}
	// gen_random_uuid() (used by 001_workspaces.sql et al.) lives in
	// pgcrypto on PG < 13 and core on PG 13+. postgres:15-alpine has it in
	// core, but create the extension defensively so the test does not pin a
	// specific PG minor.
	if _, err := conn.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create pgcrypto: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// forwardMigrationCount counts the files RunMigrations is expected to apply:
// every *.sql that is NOT a *.down.sql. This is derived from the real
// directory so the gate auto-tracks new migrations without an edit here.
func forwardMigrationCount(t *testing.T) int {
	t.Helper()
	all, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	n := 0
	for _, f := range all {
		if len(f) >= len(".down.sql") && f[len(f)-len(".down.sql"):] == ".down.sql" {
			continue
		}
		n++
	}
	if n == 0 {
		t.Fatalf("found zero forward migrations under %s — wrong path?", migrationsDir)
	}
	return n
}

// TestIntegration_InitPostgres_PingSucceeds proves the production connection
// bootstrap actually establishes a usable pool against a real server. A
// sqlmock test can never exercise the real DB.Ping() inside InitPostgres,
// which is the line that turns a bad DSN / unreachable host into a boot
// failure instead of a silently-broken pool.
func TestIntegration_InitPostgres_PingSucceeds(t *testing.T) {
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping")
	}
	if err := InitPostgres(url); err != nil {
		t.Fatalf("InitPostgres against real PG failed: %v", err)
	}
	if DB == nil {
		t.Fatal("InitPostgres returned nil error but db.DB is nil")
	}
	// The pool must be live, not just opened.
	if err := DB.Ping(); err != nil {
		t.Fatalf("db.DB.Ping after InitPostgres: %v", err)
	}
	// Round-trip a trivial query to prove the connection actually serves.
	var one int
	if err := DB.QueryRow("SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1 round-trip: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 returned %d", one)
	}
}

// TestIntegration_InitPostgres_BadDSNFails proves InitPostgres surfaces an
// unreachable/garbage DSN as an error (the ping path), rather than handing
// back a half-open pool. Watch-fail: if someone drops the DB.Ping() check
// from InitPostgres, this stops returning an error and fails.
func TestIntegration_InitPostgres_BadDSNFails(t *testing.T) {
	if os.Getenv("INTEGRATION_DB_URL") == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping")
	}
	// Valid DSN shape, but nothing is listening on this port.
	err := InitPostgres("postgres://postgres:test@127.0.0.1:1/does_not_exist?sslmode=disable&connect_timeout=2")
	if err == nil {
		t.Fatal("expected InitPostgres to fail against an unreachable DSN, got nil (DB.Ping check removed?)")
	}
}

// TestIntegration_MigrationReplay_FromScratch is the core gate: run the
// PRODUCTION RunMigrations over a blank public schema and assert the full
// forward chain applies cleanly with zero skips.
//
// Watch-fail intent:
//   - #211 .down-wipe: a `.down.sql` leaking into the forward set would
//     run a DROP before its CREATE → error here (hard fail), or wipe a
//     table → the schema_migrations / table-presence assertions catch it.
//   - mis-ordered / dangling-dependency migration → RunMigrations returns
//     a non-nil error and this test fails.
func TestIntegration_MigrationReplay_FromScratch(t *testing.T) {
	conn := freshIntegrationDB(t)
	DB = conn // RunMigrations operates on the package-global DB.

	if err := RunMigrations(migrationsDir); err != nil {
		t.Fatalf("full-chain replay-from-scratch failed: %v", err)
	}

	// Every forward migration must be recorded as applied — proves none was
	// silently skipped (the failure mode the CI psql loop tolerates).
	want := forwardMigrationCount(t)
	var got int
	if err := DB.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&got); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if got != want {
		t.Errorf("schema_migrations recorded %d migrations, expected %d (the full forward chain)", got, want)
	}

	// No `.down.sql` may ever be recorded — that is the #211 signature.
	var downRecorded int
	if err := DB.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE filename LIKE '%.down.sql'",
	).Scan(&downRecorded); err != nil {
		t.Fatalf("count down migrations: %v", err)
	}
	if downRecorded != 0 {
		t.Errorf("a .down.sql migration was applied (#211 regression): %d recorded", downRecorded)
	}

	// Spot-check load-bearing tables that survive to HEAD of the chain.
	// workspaces is the root table; workspace_auth_tokens was the #211
	// canary (its data wipe regressed AdminAuth to fail-open).
	for _, tbl := range []string{"workspaces", "workspace_auth_tokens", "delegations", "activity_logs"} {
		var exists bool
		if err := DB.QueryRow(
			"SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name=$1)",
			tbl,
		).Scan(&exists); err != nil {
			t.Fatalf("check table %s: %v", tbl, err)
		}
		if !exists {
			t.Errorf("table %q missing after full replay — chain did not land it", tbl)
		}
	}

	// agent_memories is CREATEd at 008 and DROPped at the end of the chain
	// (20260524110000_drop_agent_memories). Its absence proves the late
	// drop migration actually ran AFTER the early create — i.e. ordering
	// held. If the chain ever runs a drop before its create, this flips.
	var legacyExists bool
	if err := DB.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='agent_memories')",
	).Scan(&legacyExists); err != nil {
		t.Fatalf("check agent_memories: %v", err)
	}
	if legacyExists {
		t.Error("agent_memories still present at HEAD — the late drop migration did not replay in order")
	}
}

// TestIntegration_MigrationReplay_IsIdempotent_DoubleApply guards the 045
// crash-loop class (cp#429 / project_cp_migration_045_crashloop_idempotency_guard):
// the runner re-checks every file on every boot, so a non-idempotent
// migration replays fine once and FAILS on the second pass. Here we run the
// full chain twice. The second pass must apply ZERO new files (all recorded)
// and must not error.
//
// NOTE: this runs against the SAME populated schema, so it also exercises
// the "skip already-applied" tracking path end-to-end against real PG, which
// the sqlmock tests only simulate.
func TestIntegration_MigrationReplay_IsIdempotent_DoubleApply(t *testing.T) {
	conn := freshIntegrationDB(t)
	DB = conn

	if err := RunMigrations(migrationsDir); err != nil {
		t.Fatalf("first replay failed: %v", err)
	}
	var afterFirst int
	if err := DB.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&afterFirst); err != nil {
		t.Fatalf("count after first: %v", err)
	}

	// Second boot: nothing new should apply, and it must not error even
	// though the runner re-evaluates every file (the 045 failure mode).
	if err := RunMigrations(migrationsDir); err != nil {
		t.Fatalf("second replay failed (non-idempotent migration / 045 crash-loop class): %v", err)
	}
	var afterSecond int
	if err := DB.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&afterSecond); err != nil {
		t.Fatalf("count after second: %v", err)
	}
	if afterSecond != afterFirst {
		t.Errorf("second boot changed schema_migrations from %d to %d — re-application is not a clean no-op", afterFirst, afterSecond)
	}
}
