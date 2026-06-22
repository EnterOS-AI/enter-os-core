package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression pin for RFC internal#617 / task #335.
//
// The drop-runtime_image_pins migration MUST honor the care zone documented
// in the RFC: drop the `runtime_image_pins` table but PRESERVE the column
// `workspaces.runtime_image_digest` and its partial index
// `idx_workspaces_runtime_image_digest`.
//
// This is a static-file lint, not a DB-execution test. Running the actual
// migration is out of scope for unit tests (the migration test infra in
// postgres_schema_migrations_test.go already proves the apply mechanism
// works for any forward file). What we pin here is the *content shape* of
// the new migration:
//
//   - up.sql DROPs runtime_image_pins (the dead table)
//   - up.sql does NOT touch runtime_image_digest (the care-zone column)
//   - up.sql does NOT touch idx_workspaces_runtime_image_digest (care-zone index)
//   - down.sql recreates runtime_image_pins (idempotent rollback)
//
// If a future cleanup PR wants to also drop the column, it should be a
// separate migration with its own RFC — this test catches accidental
// scope creep at PR time, before it ships to tenant DBs.
func TestMigration20260520_DropsRuntimeImagePins_PreservesDigestColumn(t *testing.T) {
	// Locate the migrations dir relative to this test file's package dir.
	// /workspace-server/internal/db/ → ../../migrations/
	const migDir = "../../migrations"
	const upFile = "20260520120000_drop_runtime_image_pins.up.sql"
	const downFile = "20260520120000_drop_runtime_image_pins.down.sql"

	upPath := filepath.Join(migDir, upFile)
	downPath := filepath.Join(migDir, downFile)

	upBytes, err := os.ReadFile(upPath)
	if err != nil {
		t.Fatalf("read %s: %v", upPath, err)
	}
	downBytes, err := os.ReadFile(downPath)
	if err != nil {
		t.Fatalf("read %s: %v", downPath, err)
	}

	// Strip single-line SQL comments (`-- ...`) before assertion so the
	// rationale prose in the migration headers can mention the care-zone
	// column by name without tripping the DDL-touch guard. The guard is
	// specifically about DDL statements that act on the column.
	upDDL := stripSQLLineComments(strings.ToLower(string(upBytes)))
	downDDL := stripSQLLineComments(strings.ToLower(string(downBytes)))

	// up.sql MUST drop the dead table.
	if !strings.Contains(upDDL, "drop table") || !strings.Contains(upDDL, "runtime_image_pins") {
		t.Errorf("up.sql must DROP TABLE runtime_image_pins; got DDL:\n%s\n(full file:\n%s)", upDDL, upBytes)
	}

	// CARE ZONE: up.sql DDL MUST NOT touch the workspaces.runtime_image_digest
	// column or its index. A DDL statement that references either name is a
	// scope-creep defect — file a separate RFC.
	if strings.Contains(upDDL, "runtime_image_digest") {
		t.Errorf("up.sql DDL references runtime_image_digest — care-zone column must NOT be touched by this migration. See RFC internal#617 §3. DDL:\n%s\n(full file:\n%s)", upDDL, upBytes)
	}
	if strings.Contains(upDDL, "idx_workspaces_runtime_image_digest") {
		t.Errorf("up.sql DDL references idx_workspaces_runtime_image_digest — care-zone index must NOT be touched by this migration. See RFC internal#617 §3. DDL:\n%s\n(full file:\n%s)", upDDL, upBytes)
	}

	// down.sql MUST recreate the table (rollback path).
	if !strings.Contains(downDDL, "create table") || !strings.Contains(downDDL, "runtime_image_pins") {
		t.Errorf("down.sql must CREATE TABLE runtime_image_pins (rollback path); got DDL:\n%s\n(full file:\n%s)", downDDL, downBytes)
	}

	// down.sql DDL also must not touch the care-zone column (symmetry —
	// we never added the column in the up so we cannot drop or recreate it
	// in the down either).
	if strings.Contains(downDDL, "runtime_image_digest") {
		t.Errorf("down.sql DDL references runtime_image_digest — should be a no-op for the care-zone column. DDL:\n%s\n(full file:\n%s)", downDDL, downBytes)
	}
}

// stripSQLLineComments removes `-- ...` line comments from a SQL string,
// leaving only DDL statements + whitespace. Used by the migration-content
// guards so descriptive prose in the migration header doesn't false-flag.
//
// This is intentionally minimal — does NOT handle `/* */` block comments
// (the migration files don't use them) or string-literal embedded `--`
// (DDL doesn't use that shape). Good enough for static-content lint.
func stripSQLLineComments(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		// Trim everything after the first `--`. Conservative — if a future
		// migration genuinely needs `--` inside a string literal, that
		// would require parsing.
		if idx := strings.Index(ln, "--"); idx >= 0 {
			ln = ln[:idx]
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// TestMigration20260520_PairExists is a belt-and-braces guard that the
// up + down files both exist and aren't empty. RunMigrations only consumes
// the up but a missing down breaks the dev-side rollback workflow silently.
func TestMigration20260520_PairExists(t *testing.T) {
	const migDir = "../../migrations"
	for _, f := range []string{
		"20260520120000_drop_runtime_image_pins.up.sql",
		"20260520120000_drop_runtime_image_pins.down.sql",
	} {
		p := filepath.Join(migDir, f)
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("expected migration file %s to exist: %v", p, err)
			continue
		}
		if info.Size() < 50 {
			t.Errorf("migration file %s is suspiciously small (%d bytes) — header comment missing?", p, info.Size())
		}
	}
}

// TestMigration20260520_DeadReaderIsGone pins the deletion of the dead
// runtime_image_pin.go reader. If anyone reintroduces it (e.g., a cherry-
// pick from a stale branch), this catches it in unit tests before it hits
// review. The reader is provably dead under CP-as-SSOT — re-adding it
// reopens the divergence the RFC closed.
func TestMigration20260520_DeadReaderIsGone(t *testing.T) {
	const readerPath = "../handlers/runtime_image_pin.go"
	if _, err := os.Stat(readerPath); err == nil {
		t.Errorf("dead reader %s reappeared — RFC internal#617 retired it. If you really need a per-tenant pin path, file a follow-up RFC; do not just re-add the reader.", readerPath)
	}
	const testPath = "../handlers/runtime_image_pin_test.go"
	if _, err := os.Stat(testPath); err == nil {
		t.Errorf("dead reader test %s reappeared — should have been removed alongside the implementation.", testPath)
	}
}

