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

const moduleInternalPrefix = "github.com/EnterOS-AI/enter-os-core/workspace-server/internal/"

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

// SEO-patch grep-gate (RFC #2843 §4.4, PR #2844). The SEO
// patch (EnableSEOSkillPackage / SEOSkillPackageFiles /
// SEOSkillConfigBlock / seo_skill_package.go) was a
// per-template patch deleted by PR #2844. The generic asset
// channel (RFC #2843 #24) replaces it with template-agnostic
// template-repo fetches, so any reintroduction here is a
// layering violation (template content in core). This test
// makes the deletion STRUCTURAL — a future refactor that
// re-adds the patch (e.g. "let's just bring back
// EnableSEOSkillPackage for the new template") fails CI here
// before the SEO symbols leak into core, rather than after the
// next incident.
//
// Two layers of defense:
//   1. Symbol grep on every .go file in workspace-server/
//      (catches a re-add to any package, not just provisioner).
//   2. File-path existence check (catches the embedded
//      seo_skill_package/ directory reappearing).
//
// If this test fails: you re-introduced the per-template SEO
// patch that RFC #2843 §4.4 explicitly deletes. Use the generic
// template-repo asset channel (RFC #2843 #24) instead.

var seoPatchForbiddenSymbols = []string{
	"EnableSEOSkillPackage",
	"SEOSkillPackageFiles",
	"SEOSkillConfigBlock",
	"mergeSkillsBlockIntoConfigYAML",
}

var seoPatchForbiddenPathPrefixes = []string{
	"workspace-server/internal/provisioner/seo_skill_package",
	"workspace-server/internal/provisioner/seo_skill_package.go",
}

func TestNoSEOPatchSymbolsInCore(t *testing.T) {
	t.Parallel()

	// The test runs from internal/provisioner/ — the repo root
	// is 4 levels up (workspace-server/internal/provisioner → root).
	repoRoot, _ := filepath.Abs("../../..")
	root := filepath.Join(repoRoot, "workspace-server")

	// Layer 1: grep every .go file in workspace-server/ for the
	// forbidden SEO-patch symbols. Catches re-adds to ANY package
	// (provisioner, handlers, etc.) — the patch was per-template
	// content in core, no package is a valid home.
	var hits []string
	// Resolve the current test file's absolute path so the
	// self-exclude can match by exact path, not by basename
	// (a basename match would skip EVERY architecture_test.go
	// in the tree — workspace-server has them under
	// internal/{models,db,provisioner,wsauth}/ — and create
	// a blind spot where forbidden SEO symbols in a sibling
	// architecture test would pass silently). Researcher
	// RC #11684.
	selfPath, _ := filepath.Abs("architecture_test.go")
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip generated files and vendored code.
		if strings.Contains(path, "/vendor/") {
			return nil
		}
		// Skip THIS test file itself — it lists the forbidden
		// symbols as documentation. Excluding self from the grep
		// is necessary to avoid a self-match (the grep-gate would
		// fail on the very file that defines it). Match by EXACT
		// absolute path so sibling architecture_test.go files
		// (workspace-server/internal/{models,db,wsauth}/) are
		// still grep'd — they could legitimately contain the
		// forbidden SEO symbols and must be caught.
		if path == selfPath || strings.HasSuffix(path, "/"+filepath.Base(selfPath)) && path == selfPath {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, sym := range seoPatchForbiddenSymbols {
			if strings.Contains(string(b), sym) {
				hits = append(hits, path+":"+sym)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(hits) > 0 {
		t.Errorf(
			"RFC #2843 §4.4 grep-gate: the SEO-specific patch symbols "+
				"are forbidden in core (per-template content has no home "+
				"here). Use the generic template-repo asset channel "+
				"(RFC #2843 #24) instead. Hits: %v",
			hits,
		)
	}

	// Layer 2: forbidden file-path existence check. Catches
	// the seo_skill_package/ directory reappearing with the
	// embedded skill files (which the grep above would miss
	// since the .md/.yaml files don't reference the symbols).
	for _, forbidden := range seoPatchForbiddenPathPrefixes {
		abs := filepath.Join(repoRoot, forbidden)
		if _, statErr := os.Stat(abs); statErr == nil {
			t.Errorf(
				"RFC #2843 §4.4 grep-gate: forbidden file/dir re-appeared: %s "+
					"(this is the per-template SEO patch that the generic "+
					"asset channel replaces).",
				abs,
			)
		}
	}
}
