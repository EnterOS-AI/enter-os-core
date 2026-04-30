package db_test

// Static drift gate: every value declared in models.AllWorkspaceStatuses
// must exist in the workspace_status enum after every migration applies.
//
// Why this exists: the workspace_status enum (migration 043) initially
// shipped without 'awaiting_agent' and 'hibernating' even though
// application code already wrote both. Every UPDATE silently failed in
// production for five days because:
//
//   - Status values were ad-hoc string literals scattered across raw
//     SQL strings in 8+ files, with no compile-time check.
//   - sqlmock matched SQL by regex, not against the live enum.
//   - Errors were dropped or log-and-continued at every call site.
//
// The fix is layered. This gate is the static layer:
//
//   - models.AllWorkspaceStatuses is the source of truth for the
//     codebase side. Every status write goes through one of those
//     typed constants (the parameterized-write refactor enforces this).
//   - The migrations are the source of truth for the DB side.
//   - This test parses both and asserts the codebase set ⊆ migration set.
//
// If you add a new status:
//
//   1. Add a `Status…` constant in models/workspace_status.go AND
//      append it to AllWorkspaceStatuses.
//   2. Open a migration `ALTER TYPE workspace_status ADD VALUE 'X'`.
//   3. This test confirms both happened in the same PR.
//
// If you intend to retire a status: keep it in the enum as long as any
// row could legitimately still hold it, then drop it from
// AllWorkspaceStatuses (the gate runs the inclusion in one direction
// only — extras in the enum are fine).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestWorkspaceStatusEnum_NoLiteralDrift(t *testing.T) {
	t.Parallel()

	repoRoot := findRepoRoot(t)
	migrationsDir := filepath.Join(repoRoot, "workspace-server", "migrations")
	statusFile := filepath.Join(repoRoot, "workspace-server", "internal", "models", "workspace_status.go")
	srcRoot := filepath.Join(repoRoot, "workspace-server")

	enum := loadWorkspaceStatusEnum(t, migrationsDir)
	if len(enum) == 0 {
		t.Fatalf("could not parse workspace_status enum from %s — gate is non-functional", migrationsDir)
	}

	codebase := loadAllWorkspaceStatuses(t, statusFile)
	if len(codebase) == 0 {
		t.Fatalf("could not parse models.AllWorkspaceStatuses from %s — gate is non-functional", statusFile)
	}

	var rogue []string
	for lit := range codebase {
		if _, ok := enum[lit]; !ok {
			rogue = append(rogue, lit)
		}
	}
	if len(rogue) > 0 {
		sort.Strings(rogue)
		t.Errorf(
			"workspace status constants %v are declared in models.AllWorkspaceStatuses but not in the workspace_status enum.\n"+
				"Add a migration `ALTER TYPE workspace_status ADD VALUE 'X';` (see migration 046 for shape).\n"+
				"Enum currently: %v\nCodebase declares: %v",
			rogue, sortedKeys(enum), sortedKeys(codebase),
		)
	}

	// Second axis: scan production .go files for hard-coded
	// `UPDATE workspaces SET status = '<literal>'`. Every status write must
	// flow through models.Status* constants — the typed-constants refactor
	// (PR #2396) made this enforceable. Without this scan, a future
	// site-update can silently re-introduce a literal that bypasses
	// AllWorkspaceStatuses + the migration gate above. The hard-coded site
	// in workspace_bootstrap.go:62 was missed in the initial sweep and
	// only caught by manual grep — this gate makes that automatic.
	if hits := findHardCodedStatusWrites(t, srcRoot); len(hits) > 0 {
		t.Errorf(
			"hard-coded `SET status = '<literal>'` found in production code — replace with a parameterized $N + models.Status* constant:\n  %s",
			strings.Join(hits, "\n  "),
		)
	}
}

// loadWorkspaceStatusEnum scans every *.up.sql file for either:
//
//	CREATE TYPE workspace_status AS ENUM ('a', 'b', ...)
//	ALTER TYPE workspace_status ADD VALUE [IF NOT EXISTS] 'X' [BEFORE|AFTER 'Y']
//
// and returns the union of every value the enum will hold after all
// migrations apply.
func loadWorkspaceStatusEnum(t *testing.T, migrationsDir string) map[string]struct{} {
	t.Helper()

	out := make(map[string]struct{})

	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	sort.Strings(files)

	createRE := regexp.MustCompile(`(?is)CREATE\s+TYPE\s+workspace_status\s+AS\s+ENUM\s*\(([^)]+)\)`)
	addValueRE := regexp.MustCompile(`(?i)ALTER\s+TYPE\s+workspace_status\s+ADD\s+VALUE(?:\s+IF\s+NOT\s+EXISTS)?\s+'([^']+)'`)
	literalRE := regexp.MustCompile(`'([^']+)'`)

	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range createRE.FindAllStringSubmatch(string(body), -1) {
			for _, lit := range literalRE.FindAllStringSubmatch(m[1], -1) {
				out[lit[1]] = struct{}{}
			}
		}
		for _, m := range addValueRE.FindAllStringSubmatch(string(body), -1) {
			out[m[1]] = struct{}{}
		}
	}
	return out
}

// loadAllWorkspaceStatuses parses workspace_status.go and extracts:
//
//   - Every `Status… WorkspaceStatus = "..."` declaration in the const block.
//   - Every entry in the AllWorkspaceStatuses slice literal.
//
// The gate asserts the slice's set equals (or is a subset of) the const
// block's set, so a new status added to the const block but forgotten
// in AllWorkspaceStatuses surfaces here. AllWorkspaceStatuses is the
// canonical "what the codebase expects the DB to accept" list — any
// const not in the slice is unenforced by the gate.
func loadAllWorkspaceStatuses(t *testing.T, statusFile string) map[string]struct{} {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, statusFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", statusFile, err)
	}

	consts := make(map[string]string)        // const name → string value
	var sliceEntries []string                 // identifiers used in AllWorkspaceStatuses
	allWorkspaceStatusesFound := false

	ast.Inspect(f, func(n ast.Node) bool {
		switch decl := n.(type) {
		case *ast.GenDecl:
			if decl.Tok == token.CONST {
				for _, spec := range decl.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if !strings.HasPrefix(name.Name, "Status") {
							continue
						}
						if i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						unquoted := strings.Trim(lit.Value, `"`)
						consts[name.Name] = unquoted
					}
				}
			}
			if decl.Tok == token.VAR {
				for _, spec := range decl.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if name.Name != "AllWorkspaceStatuses" {
							continue
						}
						allWorkspaceStatusesFound = true
						if i >= len(vs.Values) {
							continue
						}
						composite, ok := vs.Values[i].(*ast.CompositeLit)
						if !ok {
							continue
						}
						for _, elt := range composite.Elts {
							ident, ok := elt.(*ast.Ident)
							if !ok {
								continue
							}
							sliceEntries = append(sliceEntries, ident.Name)
						}
					}
				}
			}
		}
		return true
	})

	if !allWorkspaceStatusesFound {
		t.Fatalf("AllWorkspaceStatuses not found in %s", statusFile)
	}

	// Cross-check: every slice entry must resolve to a known const.
	out := make(map[string]struct{})
	for _, entry := range sliceEntries {
		v, ok := consts[entry]
		if !ok {
			t.Errorf("AllWorkspaceStatuses references undefined identifier %q in %s", entry, statusFile)
			continue
		}
		out[v] = struct{}{}
	}

	// Cross-check: every const must be in the slice (otherwise the
	// gate runs against an outdated source-of-truth list).
	sliceSet := make(map[string]struct{}, len(sliceEntries))
	for _, e := range sliceEntries {
		sliceSet[e] = struct{}{}
	}
	for name := range consts {
		if _, ok := sliceSet[name]; !ok {
			t.Errorf(
				"const %q is declared but missing from AllWorkspaceStatuses in %s — "+
					"add it to the slice or the drift gate cannot enforce migration coverage for it",
				name, statusFile,
			)
		}
	}

	return out
}

// findHardCodedStatusWrites walks workspace-server/ production .go files
// (excluding *_test.go) and returns any string literal that contains a
// `SET status = '<literal>'` write against the workspaces table. Uses Go
// AST so quoted snippets in comments don't false-positive.
func findHardCodedStatusWrites(t *testing.T, srcRoot string) []string {
	t.Helper()

	// Match `SET status = '<lit>'` only in strings that also reference
	// the workspaces table — narrows out a2a_queue / agents / approvals
	// which have their own status enums.
	literalRE := regexp.MustCompile(`(?is)UPDATE\s+workspaces\b[^']*?SET\s+status\s*=\s*'([^']+)'`)

	var hits []string
	walkErr := filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip vendor + .git + migrations (literals there are intentional).
			base := filepath.Base(path)
			if base == "vendor" || base == ".git" || base == "migrations" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			return nil
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s := lit.Value
			if !strings.Contains(s, "UPDATE workspaces") && !strings.Contains(s, "UPDATE\nworkspaces") && !strings.Contains(s, "UPDATE\n\t\t\tworkspaces") {
				return true
			}
			for _, m := range literalRE.FindAllStringSubmatch(s, -1) {
				pos := fset.Position(lit.Pos())
				rel, _ := filepath.Rel(srcRoot, path)
				hits = append(hits, rel+":"+itoa(pos.Line)+" → SET status = '"+m[1]+"'")
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", srcRoot, walkErr)
	}
	sort.Strings(hits)
	return hits
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "workspace-server", "migrations")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate repo root with workspace-server/migrations from %s", dir)
	return ""
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
