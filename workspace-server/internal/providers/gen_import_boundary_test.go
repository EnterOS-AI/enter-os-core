package providers

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gen_import_boundary_test.go — arch-lint-equivalent boundary gate
// (internal#718 P2-A, CTO 2026-05-27 "arch-lint so prod doesn't import the raw
// gen package incorrectly").
//
// molecule-controlplane enforces this with go-arch-lint: the
// internal/providers/gen component is absent from every other component's
// mayDependOn list, so a production package importing the raw generated
// projection fails CI. molecule-core has no go-arch-lint regime, so we pin the
// SAME invariant with a behavior-based AST gate (the established core pattern —
// see derive_provider_drift_test.go / class1_ast_gate_test.go).
//
// Invariant: NO production (non-test) Go file in workspace-server may import
// internal/providers/gen, EXCEPT inside internal/providers itself (the loader's
// own parity test wiring) — and even there only test files. The generated
// projection is checked-in + drift-gated DATA; production code derives through
// the loader (internal/providers DeriveProvider / IsPlatform), never the raw
// gen literals. P2-B wires the billing decision onto the loader, not gen.

const genImportPath = "github.com/EnterOS-AI/enter-os-core/workspace-server/internal/providers/gen"

func TestNoProductionImportOfGenPackage(t *testing.T) {
	// Walk up to the workspace-server module root (this test runs with cwd =
	// internal/providers).
	root := moduleRoot(t)

	var offenders []string
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := info.Name()
			// Skip vendored / non-source trees.
			if base == "vendor" || base == "node_modules" || base == ".git" || base == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Test files are exempt — the loader's own gen parity test
		// (gen/registry_gen_test.go) legitimately imports the loader, and any
		// test may cross boundaries to assert on the projection.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The gen package's own files import nothing internal; skip the dir
		// itself so we never flag generated code referencing its own path in a
		// comment-derived parse (build.ImportDir reads real imports only, but be
		// explicit).
		dir := filepath.Dir(path)
		if filepath.Base(dir) == "gen" && strings.HasSuffix(filepath.Dir(dir), filepath.Join("internal", "providers")) {
			return nil
		}

		pkg, perr := build.ImportDir(dir, build.ImportComment)
		if perr != nil {
			// A dir with build-tagged-out files or no buildable package for the
			// default tags is not an offender; skip quietly.
			return nil //nolint:nilerr // unbuildable dir is not a boundary violation
		}
		for _, imp := range pkg.Imports {
			if imp == genImportPath {
				rel, _ := filepath.Rel(root, dir)
				offenders = append(offenders, rel)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk module tree: %v", walkErr)
	}

	if len(offenders) > 0 {
		t.Errorf("production packages import the raw generated projection %q: %v\n"+
			"Production code must derive through the loader (internal/providers "+
			"DeriveProvider / IsPlatform), never the raw gen literals. The gen "+
			"package is checked-in + drift-gated DATA only (internal#718).",
			genImportPath, dedupe(offenders))
	}
}

// moduleRoot returns the workspace-server module root by walking up from the
// test's cwd (internal/providers) until it finds go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod above %s", dir)
		}
		dir = parent
	}
}

func dedupe(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
