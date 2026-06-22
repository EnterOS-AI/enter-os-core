package db_test

// Architecture test (#2344): db is a leaf — DB pool + migrations + raw
// SQL helpers, no business-logic dependencies. The DB layer must be
// testable with sqlmock in isolation. If db starts importing handlers
// or provisioner, every db unit test would need to bring up that
// subsystem, and the layering becomes circular.
//
// If this test fails: you put business logic in the db package. Move
// it to a higher-tier package that imports db, not the reverse.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const moduleInternalPrefix = "github.com/EnterOS-AI/enter-os-core/workspace-server/internal/"

func TestDBHasNoInternalDependencies(t *testing.T) {
	t.Parallel()
	for path, file := range listImports(t, ".") {
		if strings.HasPrefix(path, moduleInternalPrefix) {
			t.Errorf(
				"db must not import other internal packages "+
					"(found %q in %s) — db is the foundation layer and a "+
					"reverse dep creates a cycle (everything imports db). "+
					"See workspace-server/internal/db/architecture_test.go.",
				path, file,
			)
		}
	}
}

func listImports(t *testing.T, dir string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := make(map[string]string)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, "\"")
			if _, seen := out[path]; !seen {
				out[path] = name
			}
		}
	}
	return out
}
