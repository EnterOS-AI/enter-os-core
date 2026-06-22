package models_test

// Architecture test (#2344): models is a leaf — it carries pure type
// definitions and must not import any other internal/* package. Almost
// every package in workspace-server depends on models; if models grew a
// reverse dep, the import graph would cycle.
//
// If this test fails: you put behavior inside models. Move the behavior
// to whichever package actually owns it (handlers, provisioner, db, …)
// and have *that* package import models, not the reverse.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const moduleInternalPrefix = "github.com/EnterOS-AI/enter-os-core/workspace-server/internal/"

func TestModelsHasNoInternalDependencies(t *testing.T) {
	t.Parallel()
	for path, file := range listImports(t, ".") {
		if strings.HasPrefix(path, moduleInternalPrefix) {
			t.Errorf(
				"models must not import other internal packages "+
					"(found %q in %s) — models is the pure-types leaf and any "+
					"reverse dep creates an import cycle since most packages "+
					"depend on models. See workspace-server/internal/models/architecture_test.go.",
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
