package db_test

// Static drift gate: every UPDATE that sets status to a "failed" value
// must also set last_sample_error in the same statement. Otherwise the
// row ends up with status='failed' + last_sample_error=NULL — operators
// see "workspace failed" with no reason, and the Canvas E2E reports the
// useless `Workspace failed: (no last_sample_error)` from #2632.
//
// Why a static gate: pre-2026-05-05 we had at least two writers
// (markProvisionFailed in workspace_provision_shared.go set the
// message; bundle/importer.go's markFailed didn't). The provision-
// timeout sweep also sets the message. Code review missed the
// importer drift for ~6 months until the Canvas E2E surfaced it.
//
// Rule:
//   - If a Go string literal in this repo contains both
//     `UPDATE workspaces` and a clause setting `status` to a value
//     resembling "failed" — either via a `$N` placeholder later bound
//     to StatusFailed, or via an inline `'failed'` literal — that same
//     literal MUST also contain `last_sample_error`.
//   - Allowed: an UPDATE that only sets status to a non-failed value
//     (online, hibernating, removed, etc.). Those don't need the
//     message column, and clearing it would lose forensic context.
//
// Caveats:
//   - The test reads source as text. Multi-line UPDATEs split across
//     concatenated string fragments will slip past — that's an
//     accepted limitation for now; the parameterized-write refactor
//     (#2799) will let us replace this textual gate with a typed-call
//     gate eventually.
//   - "last_sample_error" appearing anywhere in the same literal is
//     enough to satisfy the rule. We don't try to verify the column
//     receives a non-empty value at runtime — that's the
//     parameterized-write refactor's territory too.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWorkspaceStatusFailed_MustSetLastSampleError uses Go's AST to find
// every ExecContext call whose argument list includes the
// `models.StatusFailed` constant. For each such call, the SQL literal
// (the second argument) must also contain `last_sample_error`. This
// catches the bug class without false-positive matches on UPDATEs that
// set status to a non-failed value (online/hibernating/removed/etc.)
// because those don't pass StatusFailed as an arg.
func TestWorkspaceStatusFailed_MustSetLastSampleError(t *testing.T) {
	root := findRepoRoot(t)
	violations := []string{}

	walkErr := filepath.Walk(filepath.Join(root, "workspace-server", "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// Match db.DB.ExecContext / db.DB.QueryContext / db.DB.QueryRowContext
			// — the three SQL execution surfaces this codebase uses.
			methodName := sel.Sel.Name
			if methodName != "ExecContext" && methodName != "QueryContext" && methodName != "QueryRowContext" {
				return true
			}
			// Args: 0=ctx, 1=sql-literal, 2..=bind vars.
			if len(call.Args) < 3 {
				return true
			}
			passesStatusFailed := false
			for _, a := range call.Args[2:] {
				if isStatusFailedRef(a) {
					passesStatusFailed = true
					break
				}
			}
			if !passesStatusFailed {
				return true
			}
			// SQL literal — usually `*ast.BasicLit` for a single-line
			// string or a back-tick string. May also be a const ref.
			sqlText := extractStringLit(call.Args[1])
			if sqlText == "" {
				// SQL is a name reference, not a literal — can't check.
				return true
			}
			if strings.Contains(sqlText, "last_sample_error") {
				return true
			}
			// Skip non-UPDATE statements that happen to pass StatusFailed
			// (e.g. SELECT … WHERE status = $1). The drift target is
			// specifically writes that mark the row failed.
			if !regexp.MustCompile(`(?i)\bUPDATE\s+workspaces\b`).MatchString(sqlText) {
				return true
			}
			rel, _ := filepath.Rel(root, path)
			pos := fset.Position(call.Pos())
			snippet := strings.TrimSpace(sqlText)
			if len(snippet) > 120 {
				snippet = snippet[:120] + "..."
			}
			violations = append(violations,
				fmt.Sprintf("%s:%d: %s", rel, pos.Line, snippet))
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	if len(violations) > 0 {
		t.Errorf("UPDATE workspaces SET status = ... binds models.StatusFailed but the SQL literal does not write last_sample_error — every code path that marks a workspace failed must also write the reason, or operators see `Workspace failed: (no last_sample_error)` (incident: Canvas E2E #2632). Add `, last_sample_error = $N` to the SET clause.\n\nViolations:\n  - %s",
			strings.Join(violations, "\n  - "))
	}
}

// isStatusFailedRef returns true if expr resolves to models.StatusFailed
// (selector StatusFailed off the models package). Catches both
// `models.StatusFailed` directly and `models.StatusFailed.String()`
// style usages — anything that names the constant.
func isStatusFailedRef(expr ast.Expr) bool {
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		if sel.Sel.Name == "StatusFailed" {
			return true
		}
	}
	return false
}

// extractStringLit returns the unquoted contents of a string literal
// expression, or "" if expr is not a literal we can read statically
// (e.g. concatenation, function-call argument, named const reference).
func extractStringLit(expr ast.Expr) string {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	val := lit.Value
	if len(val) >= 2 {
		first, last := val[0], val[len(val)-1]
		if (first == '`' && last == '`') || (first == '"' && last == '"') {
			return val[1 : len(val)-1]
		}
	}
	return val
}


