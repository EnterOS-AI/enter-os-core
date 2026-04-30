package provisioner_test

// Architecture test (#2344): provisioner is below handlers/router in
// the layer hierarchy. handlers wires provisioner into HTTP routes;
// the reverse direction (provisioner reaching back into handlers or
// the router) creates a cycle and tangles infra-orchestration with
// transport.
//
// Note: provisioner CURRENTLY imports db (for the runtime-image
// lookup). That's a known coupling — see PR #2276 review thread on
// where image resolution should live. The narrower rule we enforce
// here is "no upward import to handlers/router," which is the harder
// rule to keep clean.
//
// If this test fails: you reached "up" the stack. Pass whatever you
// need from handlers down through a constructor parameter or a
// function-typed callback instead of importing the package directly.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const moduleInternalPrefix = "github.com/Molecule-AI/molecule-monorepo/platform/internal/"

var provisionerForbiddenImports = []string{
	moduleInternalPrefix + "handlers",
	moduleInternalPrefix + "router",
}

func TestProvisionerDoesNotImportUpstreamLayers(t *testing.T) {
	t.Parallel()
	imports := listImports(t, ".")
	for path, file := range imports {
		for _, forbidden := range provisionerForbiddenImports {
			if path == forbidden || strings.HasPrefix(path, forbidden+"/") {
				t.Errorf(
					"provisioner must not import %q (found in %s) — "+
						"provisioner sits below handlers/router in the layer "+
						"hierarchy and a reverse dep creates a cycle. Pass "+
						"what you need down via constructor params or "+
						"function-typed callbacks. See workspace-server/internal/"+
						"provisioner/architecture_test.go.",
					path, file,
				)
			}
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
