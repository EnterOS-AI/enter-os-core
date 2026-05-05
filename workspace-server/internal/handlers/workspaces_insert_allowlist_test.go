package handlers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestINSERTworkspacesAllowlist enumerates every function in this
// package that emits an `INSERT INTO workspaces (` SQL literal, and
// pins the result against an explicit allowlist. New entries fail the
// build until a reviewer adds them — forcing the question "what
// makes this INSERT idempotent?" at PR-review time, not after the
// next bulk-create leak.
//
// Pairs with TestCreateWorkspaceTree_CallsLookupBeforeInsert (the
// behavior pin for the one bulk path). Together they close the
// regression class: this test catches "did a new function start
// inserting workspaces?", that test catches "did the existing bulk
// path drop its idempotency check?". Either fires immediately when
// drift happens.
//
// Why allowlist rather than pure behavior gate (per memory
// feedback_behavior_based_ast_gates.md): the bulk-create leak class
// is small + stable (1 path today), and a behavior gate would have
// to disambiguate "iterating a YAML array of workspaces" from the
// many other `for ... range` patterns in a Create handler (config
// lines, secrets map, channels). Type-info-aware AST analysis would
// catch the YAML-iteration shape but is heavy. Allowlisting is the
// minimum-viable pin: any PR that adds a new INSERT site is forced
// to pause, add an entry here, and document the safety mechanism in
// the comment alongside.
//
// RFC #2867 class 1.
func TestINSERTworkspacesAllowlist(t *testing.T) {
	// expected[key] = safety mechanism. Keep the comment pinned to
	// what makes that function safe — if the safety changes, the
	// allowlist must be re-reviewed.
	expected := map[string]string{
		// org_import.createWorkspaceTree: lookupExistingChild
		// before INSERT (#2868 phase 3). Also pinned by
		// TestCreateWorkspaceTree_CallsLookupBeforeInsert.
		"org_import.go:createWorkspaceTree": "lookup-then-insert via lookupExistingChild",
		// registry.Register: external workspace registers itself with
		// its known UUID; INSERT is idempotent via ON CONFLICT (id)
		// DO UPDATE — re-registration upserts, never duplicates.
		"registry.go:Register": "ON CONFLICT (id) DO UPDATE",
		// workspace.Create: single-workspace POST /workspaces from a
		// human or automation. No iteration; payload describes one
		// workspace; UUID is server-generated. Caller intent IS to
		// create, so no idempotency check is needed.
		"workspace.go:Create": "single-workspace POST, server-generated UUID",
	}

	actual := map[string]string{}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatalf("readdir %s: %v", wd, err)
	}
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() {
			continue
		}
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(wd, name)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		// For each top-level FuncDecl, walk its body and check for an
		// `INSERT INTO workspaces (` SQL literal in any CallExpr arg.
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var foundInsert bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				raw := lit.Value
				if unq, err := strconv.Unquote(raw); err == nil {
					raw = unq
				}
				if workspacesInsertRE.MatchString(raw) {
					foundInsert = true
					return false
				}
				return true
			})
			if foundInsert {
				key := name + ":" + fn.Name.Name
				actual[key] = "(observed via AST walk)"
			}
		}
	}

	// Compute set diffs so failures point at the specific drift.
	missing := []string{}
	unexpected := []string{}
	for k := range expected {
		if _, ok := actual[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range actual {
		if _, ok := expected[k]; !ok {
			unexpected = append(unexpected, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(unexpected)

	if len(unexpected) > 0 {
		t.Errorf(`new function(s) emit `+"`INSERT INTO workspaces (`"+` and aren't in the allowlist:
  %s

If this is a legitimate addition, add an entry to expected[] in this test
with the safety mechanism pinned in the comment alongside (lookup-then-
insert / ON CONFLICT / single-workspace path / etc.). The bulk-create
regression class needs explicit per-handler review, not silent drift.

Reference: RFC #2867 class 1, sibling test
TestCreateWorkspaceTree_CallsLookupBeforeInsert.`,
			strings.Join(unexpected, "\n  "))
	}
	if len(missing) > 0 {
		t.Errorf(`expected function(s) no longer emit `+"`INSERT INTO workspaces (`"+`:
  %s

Either the function was renamed/deleted (update the allowlist) or the
INSERT was moved out (verify the new home is also covered). Don't just
delete the entry — confirm the safety mechanism is still in place
elsewhere or that the workspace-create path was intentionally
restructured.`,
			strings.Join(missing, "\n  "))
	}
}
