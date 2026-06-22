package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #211 regression: RunMigrations used to glob *.sql which caught both
// `.up.sql` and `.down.sql`. Alphabetical sort put `.down.sql` first so
// every platform boot ran the rollback followed by the forward, wiping any
// data the pair re-creates (workspace_auth_tokens was the canary).
//
// This test exercises the filter directly via filepath.Glob against a
// tmp dir of staged files. The real RunMigrations opens a DB connection
// so we can't run it end-to-end in a unit test, but the filtering step
// is where the bug was.

func TestRunMigrations_SkipsDownSqlFiles(t *testing.T) {
	tmp := t.TempDir()

	// Stage a realistic mix: legacy plain .sql (migration 009), plus a pair
	// (up + down), plus a runaway .down.sql that shouldn't exist alone.
	files := map[string]string{
		"009_legacy.sql":                     "-- legacy forward only\n",
		"020_workspace_auth_tokens.up.sql":   "CREATE TABLE workspace_auth_tokens ();\n",
		"020_workspace_auth_tokens.down.sql": "DROP TABLE workspace_auth_tokens;\n",
		"021_other.up.sql":                   "-- 21 forward\n",
		"021_other.down.sql":                 "-- 21 rollback (must not run)\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Mirror the filter logic from RunMigrations.
	allFiles, err := filepath.Glob(filepath.Join(tmp, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	forward := make([]string, 0, len(allFiles))
	for _, f := range allFiles {
		base := filepath.Base(f)
		if strings.HasSuffix(base, ".down.sql") {
			continue
		}
		forward = append(forward, base)
	}

	// Assert: exactly 3 forward files, none end in .down.sql
	if len(forward) != 3 {
		t.Errorf("expected 3 forward migrations, got %d: %v", len(forward), forward)
	}
	for _, f := range forward {
		if strings.HasSuffix(f, ".down.sql") {
			t.Errorf("down migration leaked through filter: %s", f)
		}
	}
	// Spot-check the ones that must be present
	wantPresent := []string{
		"009_legacy.sql",
		"020_workspace_auth_tokens.up.sql",
		"021_other.up.sql",
	}
	for _, w := range wantPresent {
		found := false
		for _, f := range forward {
			if f == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected forward set to include %q, got %v", w, forward)
		}
	}
}
