package handlers

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Tests for the idempotency helper added in #2859 (RFC #2857 Phase 3).
//
// Background: org_import.createWorkspaceTree was non-idempotent —
// every call INSERTed a fresh row for every workspace in the tree,
// regardless of whether matching workspaces already existed. Calling
// /org/import twice with the same template duplicated the entire tree;
// in a 4-day window tenant-hongming accumulated 72 stale child
// workspaces this way.
//
// The fix routes through lookupExistingChild before INSERT. These
// tests pin the helper's three observable behaviors plus an AST gate
// that catches future re-introductions of the un-checked INSERT.

func TestLookupExistingChild_NotFound_ReturnsFalseNoError(t *testing.T) {
	mock := setupTestDB(t)
	// 0-row result → driver returns sql.ErrNoRows on Scan.
	parent := "parent-1"
	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WithArgs("Alpha", &parent).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	h := &OrgHandler{}
	id, found, err := h.lookupExistingChild(context.Background(), "Alpha", &parent)

	if err != nil {
		t.Fatalf("expected nil error on no-rows, got: %v", err)
	}
	if found {
		t.Errorf("expected found=false on no-rows, got found=true")
	}
	if id != "" {
		t.Errorf("expected empty id on no-rows, got %q", id)
	}
}

func TestLookupExistingChild_Found_ReturnsIDAndTrue(t *testing.T) {
	mock := setupTestDB(t)
	parent := "parent-1"
	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WithArgs("Alpha", &parent).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-existing-uuid"))

	h := &OrgHandler{}
	id, found, err := h.lookupExistingChild(context.Background(), "Alpha", &parent)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Errorf("expected found=true when row exists")
	}
	if id != "ws-existing-uuid" {
		t.Errorf("expected id=ws-existing-uuid, got %q", id)
	}
}

func TestLookupExistingChild_NilParent_MatchesRoot(t *testing.T) {
	// `parent_id IS NOT DISTINCT FROM NULL` is the load-bearing trick —
	// a plain `=` would never match a NULL row. Pin that roots
	// (parent_id=NULL) are still found by the lookup.
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WithArgs("RootAgent", (*string)(nil)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-root-uuid"))

	h := &OrgHandler{}
	id, found, err := h.lookupExistingChild(context.Background(), "RootAgent", nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found || id != "ws-root-uuid" {
		t.Errorf("expected found=true id=ws-root-uuid, got found=%v id=%q", found, id)
	}
}

func TestLookupExistingChild_DBError_Propagates(t *testing.T) {
	// A real DB error must NOT be silently swallowed. If the SELECT
	// can't run, the caller fails fast — never falls back to creating
	// a duplicate. That fallback is the failure mode the helper exists
	// to prevent.
	mock := setupTestDB(t)
	parent := "parent-1"
	connFail := errors.New("simulated postgres unavailable")
	mock.ExpectQuery(`SELECT id FROM workspaces`).
		WithArgs("Alpha", &parent).
		WillReturnError(connFail)

	h := &OrgHandler{}
	id, found, err := h.lookupExistingChild(context.Background(), "Alpha", &parent)

	if err == nil {
		t.Fatalf("expected DB error to propagate, got nil")
	}
	// Helper returns the raw error, not a wrap — match by string for
	// portability across error wrapping conventions.
	if !strings.Contains(err.Error(), "simulated postgres unavailable") {
		t.Errorf("expected the original DB error to surface, got: %v", err)
	}
	if found {
		t.Errorf("expected found=false on DB error, got found=true")
	}
	if id != "" {
		t.Errorf("expected empty id on DB error, got %q", id)
	}
}

// workspacesInsertRE matches a SQL literal that begins (after optional
// leading whitespace) with `INSERT INTO workspaces` followed by `(` —
// requiring the open-paren rules out lookalikes like
// `INSERT INTO workspaces_audit`, `INSERT INTO workspace_secrets`,
// `INSERT INTO workspace_channels`, `INSERT INTO canvas_layouts`. The
// previous bytes.Index gate accepted `workspaces_audit` as a prefix
// match — see RFC #2872 Important-1 for the silent-false-pass shape.
var workspacesInsertRE = regexp.MustCompile(`(?s)^\s*INSERT\s+INTO\s+workspaces\s*\(`)

// findLookupAndWorkspacesInsertPos walks the AST of `src` and returns
// the source positions of (a) the first call to `lookupExistingChild`
// and (b) the first CallExpr whose argument list contains a STRING
// BasicLit matching workspacesInsertRE. Either may be token.NoPos if
// not found.
//
// Extracted as a helper so the gate logic can be exercised against
// synthetic source — TestGate_FailsWhenLookupAfterInsert below proves
// the gate actually catches the bug shape, not just the happy path.
func findLookupAndWorkspacesInsertPos(t *testing.T, fname string, src []byte) (lookupPos, insertPos token.Pos, fset *token.FileSet) {
	t.Helper()
	fset = token.NewFileSet()
	file, err := parser.ParseFile(fset, fname, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", fname, err)
	}
	lookupPos, insertPos = token.NoPos, token.NoPos
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if sel.Sel.Name == "lookupExistingChild" && lookupPos == token.NoPos {
				lookupPos = call.Pos()
			}
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			raw := lit.Value
			if unq, err := strconv.Unquote(raw); err == nil {
				raw = unq
			}
			if workspacesInsertRE.MatchString(raw) && insertPos == token.NoPos {
				insertPos = call.Pos()
			}
		}
		return true
	})
	return
}

// Source-level guard — pins that org_import.go calls
// h.lookupExistingChild BEFORE its INSERT INTO workspaces.
//
// Per memory feedback_behavior_based_ast_gates.md: pin the behavior
// (idempotency check before INSERT), not just function names. If a
// future refactor reintroduces the un-checked INSERT (the original
// bug shape that leaked 72 workspaces in 4 days), this test fails.
//
// AST-walk implementation closes the silent-false-pass mode that the
// previous bytes.Index gate had — see workspacesInsertRE comment for
// the failure mode (workspaces_audit / workspace_secrets / etc.
// shadowing the real target via prefix match).
func TestCreateWorkspaceTree_CallsLookupBeforeInsert(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(wd, "org_import.go"))
	if err != nil {
		t.Fatalf("read org_import.go: %v", err)
	}
	lookupPos, insertPos, fset := findLookupAndWorkspacesInsertPos(t, "org_import.go", src)

	if lookupPos == token.NoPos {
		t.Fatalf("AST: no call to lookupExistingChild in org_import.go — idempotency check removed?")
	}
	if insertPos == token.NoPos {
		t.Fatalf("AST: no SQL literal matching `^\\s*INSERT INTO workspaces\\s*\\(` in any CallExpr in org_import.go — schema change or rename?")
	}
	if lookupPos > insertPos {
		t.Errorf("lookupExistingChild call at %s must come BEFORE INSERT INTO workspaces at %s — non-idempotent ordering would re-leak under repeat /org/import calls",
			fset.Position(lookupPos), fset.Position(insertPos))
	}
}

// TestGate_FailsWhenLookupAfterInsert proves the gate actually catches
// the bug it's named after — running it against synthetic Go source
// where the lookup call is positioned AFTER the workspaces INSERT must
// produce lookupPos > insertPos, which the production gate flags as
// an ERROR. Without this test the gate could regress to "always pass"
// and we wouldn't notice until the bug shipped again.
//
// Per memory feedback_assert_exact_not_substring.md: verify a
// tightened test FAILS on old code before merging.
func TestGate_FailsWhenLookupAfterInsert(t *testing.T) {
	const buggySrc = `package handlers

import "context"

type fakeDB struct{}

func (fakeDB) ExecContext(ctx context.Context, sql string, args ...interface{}) {}

type fakeOrgHandler struct{}

func (h *fakeOrgHandler) lookupExistingChild(ctx context.Context, name string, parentID *string) (string, bool, error) {
	return "", false, nil
}

func buggyCreate(h *fakeOrgHandler, db fakeDB, ctx context.Context, name string, parentID *string) {
	// Bug shape: INSERT runs FIRST, lookup runs AFTER. This is the
	// non-idempotent ordering the gate exists to forbid.
	db.ExecContext(ctx, ` + "`INSERT INTO workspaces (id, name) VALUES ($1, $2)`" + `, "x", name)
	h.lookupExistingChild(ctx, name, parentID)
}
`
	lookupPos, insertPos, _ := findLookupAndWorkspacesInsertPos(t, "buggy.go", []byte(buggySrc))
	if lookupPos == token.NoPos || insertPos == token.NoPos {
		t.Fatalf("synthetic buggy source missing expected nodes (lookupPos=%v insertPos=%v) — helper logic regression", lookupPos, insertPos)
	}
	if lookupPos < insertPos {
		t.Fatalf("synthetic bug shape (lookup AFTER insert) returned lookupPos=%d < insertPos=%d — gate would NOT fire on actual bug, regression!", lookupPos, insertPos)
	}
	// Implicit: lookupPos > insertPos here, which the production gate
	// flags via t.Errorf. This proves the gate is live, not vestigial.
}

// TestGate_IgnoresAuditTableShadow proves the regex tightening
// actually ignores `INSERT INTO workspaces_audit` literals — the
// specific shape #2872 cited as the silent-false-pass failure mode
// for the previous bytes.Index gate.
func TestGate_IgnoresAuditTableShadow(t *testing.T) {
	// Synthetic source with audit-table INSERT at line 1 (would be
	// position 0 under prefix-match) and lookup + real INSERT at later
	// positions. With the tightened regex, the audit literal is
	// ignored: insertPos points at the REAL INSERT, lookup precedes it,
	// gate passes correctly.
	const src = `package handlers

import "context"

type fakeDB struct{}

func (fakeDB) ExecContext(ctx context.Context, sql string, args ...interface{}) {}

type fakeOrgHandler struct{}

func (h *fakeOrgHandler) lookupExistingChild(ctx context.Context, name string, parentID *string) (string, bool, error) {
	return "", false, nil
}

func okCreateWithAudit(h *fakeOrgHandler, db fakeDB, ctx context.Context, name string, parentID *string) {
	// Audit-table INSERT — should be IGNORED by the tightened regex.
	db.ExecContext(ctx, ` + "`INSERT INTO workspaces_audit (id, action) VALUES ($1, $2)`" + `, "x", "create_attempt")
	// Lookup BEFORE real INSERT — correct order.
	h.lookupExistingChild(ctx, name, parentID)
	// Real INSERT.
	db.ExecContext(ctx, ` + "`INSERT INTO workspaces (id, name) VALUES ($1, $2)`" + `, "x", name)
}
`
	lookupPos, insertPos, fset := findLookupAndWorkspacesInsertPos(t, "shadow.go", []byte(src))
	if lookupPos == token.NoPos || insertPos == token.NoPos {
		t.Fatalf("expected to find lookup + real INSERT, got lookupPos=%v insertPos=%v", lookupPos, insertPos)
	}
	// The audit-table INSERT is at line ~16 (column ~20-ish), the
	// lookup is at line 19, the real INSERT is at line 21. If the
	// regex regressed to prefix-match, insertPos would point at the
	// audit literal at line 16, and the gate would falsely fail
	// (lookup at 19 > "insert" at 16). With the tightened regex,
	// insertPos correctly points at line 21, and the gate passes.
	insertLine := fset.Position(insertPos).Line
	lookupLine := fset.Position(lookupPos).Line
	if insertLine < lookupLine {
		t.Errorf("regex regressed: audit shadow at line %d swallowed real INSERT (lookup at line %d). insertPos should point at the real INSERT (line ~21), not the audit literal.",
			insertLine, lookupLine)
	}
	if lookupPos > insertPos {
		t.Errorf("synthetic source has lookup at line %d before real INSERT at line %d, gate should pass (lookupPos < insertPos), got lookupPos=%d > insertPos=%d",
			lookupLine, insertLine, lookupPos, insertPos)
	}
}

// TestWorkspacesInsertRE_RejectsLookalikes pins the regex that
// discriminates the real workspaces INSERT from prefix-matching
// lookalikes. If this regex regresses to a substring match, the
// AST gate above silently false-passes when a future refactor
// shadows the real INSERT with a workspaces_audit / workspace_secrets
// / canvas_layouts literal placed earlier in source.
func TestWorkspacesInsertRE_RejectsLookalikes(t *testing.T) {
	cases := []struct {
		sql     string
		want    bool
		comment string
	}{
		{"INSERT INTO workspaces (id, name) VALUES ($1, $2)", true, "real target"},
		{"\n\t\tINSERT INTO workspaces (id, name)\n\t\tVALUES ($1, $2)", true, "real target with leading whitespace + newlines (raw string literal shape)"},
		{"INSERT INTO workspaces_audit (id) VALUES ($1)", false, "underscore-suffix lookalike (the #2872 specific failure mode)"},
		{"INSERT INTO workspace_secrets (key, value) VALUES ($1, $2)", false, "prefix without trailing 's' (workspace_*)"},
		{"INSERT INTO workspace_channels (id) VALUES ($1)", false, "another workspace_* prefix"},
		{"INSERT INTO canvas_layouts (workspace_id, x, y) VALUES ($1, $2, $3)", false, "unrelated table that contains 'workspace' in a column ref"},
		{"UPDATE workspaces SET status='running' WHERE id=$1", false, "UPDATE shouldn't match"},
		{"SELECT * FROM workspaces WHERE id=$1", false, "SELECT shouldn't match"},
		{"-- comment about INSERT INTO workspaces (\nSELECT 1", false, "comment shouldn't match"},
	}
	for _, c := range cases {
		got := workspacesInsertRE.MatchString(c.sql)
		if got != c.want {
			t.Errorf("workspacesInsertRE.MatchString(%q) = %v, want %v (%s)", c.sql, got, c.want, c.comment)
		}
	}
}

// Confirm the regex actually matches the literal currently in
// org_import.go. Pins the shape so `gofmt` reflows or trivial edits
// to the SQL string don't silently disable the gate above.
func TestWorkspacesInsertRE_MatchesActualSourceLiteral(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(wd, "org_import.go"))
	if err != nil {
		t.Fatalf("read org_import.go: %v", err)
	}
	// Strip backtick strings, find any whose content matches.
	// Walk the source via parser.ParseFile to avoid string-search
	// drift if the literal is reflowed.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(wd, "org_import.go"), src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse org_import.go: %v", err)
	}
	var matched bool
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		raw := lit.Value
		if unq, err := strconv.Unquote(raw); err == nil {
			raw = unq
		}
		if workspacesInsertRE.MatchString(raw) {
			matched = true
		}
		return true
	})
	if !matched {
		t.Fatalf("no SQL literal in org_import.go matches workspacesInsertRE — gate is dead. Either the INSERT was renamed (update the regex) or the file was restructured (review the gate logic).")
	}
	// strings.Contains keeps the test informative: if the regex
	// stopped matching but the literal source still contains the
	// magic phrase, that's a regex-side failure (test the fix above).
	if !strings.Contains(string(src), "INSERT INTO workspaces") {
		t.Fatalf("org_import.go has no `INSERT INTO workspaces` substring at all — schema change?")
	}
}
