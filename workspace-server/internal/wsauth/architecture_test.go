package wsauth_test

// Architecture test (#2344): wsauth is a leaf package — it must not import
// any other internal/* package. The auth layer is below business logic;
// importing handlers, db, or any cousin package would force every wsauth
// test to spin up that subsystem, defeating the unit-test boundary that
// makes the auth code reviewable.
//
// If this test fails: you added an import that crosses a layer. Either
// move the dependency the other direction (consumer wires wsauth into
// itself), accept the boundary by inlining what you need, or — if the
// new coupling is genuinely correct — explicitly update this test with
// the new allowed import + a comment explaining why.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const moduleInternalPrefix = "github.com/EnterOS-AI/enter-os-core/workspace-server/internal/"

func TestWsauthHasNoInternalDependencies(t *testing.T) {
	t.Parallel()
	for path, file := range listImports(t, ".") {
		if strings.HasPrefix(path, moduleInternalPrefix) {
			t.Errorf(
				"wsauth must not import other internal packages "+
					"(found %q in %s) — wsauth is the auth leaf and must stay "+
					"unit-testable without spinning up other subsystems. "+
					"See workspace-server/internal/wsauth/architecture_test.go for context.",
				path, file,
			)
		}
	}
}

// listImports returns import-path → first-file-where-seen for non-test
// .go files in dir. Used by every architecture_test.go in this tree.
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
