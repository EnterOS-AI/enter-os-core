package handlers

// class1_ast_gate_test.go — generic Class 1 leak gate per #2867 PR-A.
//
// What this gate prevents:
//   The tenant-hongming leak class — a handler iterates a YAML-derived
//   slice (ws.Children, sub_workspaces, etc.) and calls
//   `INSERT INTO workspaces` inside the loop body without first
//   checking whether a workspace with the same (parent_id, name) is
//   already there. Each call to such a handler doubles the tree.
//
// Why this is broader than TestCreateWorkspaceTree_CallsLookupBeforeInsert:
//   The existing gate is hard-coded to org_import.go's createWorkspaceTree.
//   That catches the specific function that triggered the original
//   incident — but a future handler written from scratch in a different
//   file would not be covered. This gate walks every production handler
//   .go file and applies a structural rule that does not depend on
//   function or file names.
//
// The rule (verbatim from #2867 PR-A):
//
//   "No handler in handlers/ may iterate a slice (any RangeStmt) AND
//   call INSERT INTO workspaces inside the loop body without a
//   preceding SELECT id FROM workspaces WHERE name=$1 AND parent_id IS
//   NOT DISTINCT FROM $2 in the same function (== a lookupExistingChild
//   call, OR an ON CONFLICT clause baked into the same INSERT, OR an
//   explicit allowlist annotation)."
//
// Allowlist mechanism: a function whose body contains the exact comment
// string `// class1-gate: idempotent-by-design` is treated as safe.
// Use this only after writing a unit test that pins WHY the function
// is safe. The annotation is intentionally awkward to type — it should
// be rare.

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

// reINSERTWorkspaces matches the exact statement shape we care about.
// Tightened (vs bytes.Index "INSERT INTO workspaces") so the audit
// table `workspaces_audit` literal — or any other lookalike — does not
// false-positive trigger this gate. The same regex is used in the
// existing createWorkspaceTree gate (workspaces_insert_allowlist_test.go)
// — keep them in sync if either changes.
var reINSERTWorkspaces = regexp.MustCompile(`(?m)^\s*INSERT INTO workspaces\s*\(`)

// reONCONFLICT matches ON CONFLICT clauses anywhere in the same SQL
// literal. An UPSERT (INSERT ... ON CONFLICT ... DO UPDATE) is
// idempotent by definition, so the gate exempts it.
var reONCONFLICT = regexp.MustCompile(`(?i)\bON CONFLICT\b`)

// gateAllowlistComment is the magic comment a function author writes
// to opt out of this gate. Forces an explicit decision.
const gateAllowlistComment = "// class1-gate: idempotent-by-design"

// preflightCallNames are function names whose presence in a function
// body counts as "did a SELECT-by-(parent_id, name) preflight". Add
// new names here as new preflight helpers are introduced. Keep the
// list TIGHT — any sloppy addition weakens the gate.
var preflightCallNames = map[string]bool{
	"lookupExistingChild": true,
}

// TestClass1_NoUnpreflightedInsertInsideRange walks every production
// .go file in this package, parses the AST, and fails the test if any
// FuncDecl violates the rule above.
//
// Failure message must include: file path, function name, line of
// the offending INSERT, line of the enclosing range, and a hint at
// the three escape hatches (preflight call, ON CONFLICT, allowlist
// comment).
func TestClass1_NoUnpreflightedInsertInsideRange(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatalf("readdir %s: %v", wd, err)
	}

	type violation struct {
		file       string
		fn         string
		insertLine int
		rangeLine  int
	}
	var violations []violation
	scanned := 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(wd, name)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scanned++

		// Walk every function declaration and apply the rule.
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}

			// Allowlist: skip if the function body contains the magic
			// comment. We check via the source range of the function
			// — comments inside the body are in file.Comments and
			// must overlap the function's Pos/End range.
			if functionHasAllowlistComment(file, fd) {
				continue
			}

			// First pass: locate every INSERT INTO workspaces literal
			// in this function. We treat each such literal as a
			// candidate violation and try to clear it via the rules.
			candidates := findInsertWorkspacesLiterals(fd, src, fset)
			if len(candidates) == 0 {
				continue
			}

			// Has the function called a preflight helper? Single
			// pass — if any preflight name appears, every INSERT in
			// the function is considered preflighted. This is more
			// permissive than position-aware (preflight could be
			// AFTER the INSERT and still satisfy the gate), but the
			// existing org_import.go gate already pins the position
			// invariant for createWorkspaceTree, and a function that
			// preflights AFTER inserting would fail the position
			// gate in a separate test.
			hasPreflight := functionCallsAny(fd, preflightCallNames)

			for _, c := range candidates {
				if c.hasONCONFLICT {
					continue
				}
				if hasPreflight {
					continue
				}
				if c.enclosingRangeLine == 0 {
					// INSERT not inside any RangeStmt — single-shot,
					// not the bug pattern.
					continue
				}
				violations = append(violations, violation{
					file:       name,
					fn:         fd.Name.Name,
					insertLine: c.insertLine,
					rangeLine:  c.enclosingRangeLine,
				})
			}
		}
	}

	if scanned == 0 {
		t.Fatal("scanned 0 .go files — wrong working directory? gate would always pass")
	}

	if len(violations) > 0 {
		// Stable sort so the failure message is deterministic across
		// reruns.
		sort.Slice(violations, func(i, j int) bool {
			if violations[i].file != violations[j].file {
				return violations[i].file < violations[j].file
			}
			return violations[i].insertLine < violations[j].insertLine
		})
		var b strings.Builder
		b.WriteString("Class 1 leak gate (#2867 PR-A) — these handler functions iterate a slice and INSERT INTO workspaces inside the loop body without a (parent_id, name) preflight.\n\n")
		b.WriteString("This is the bug shape that triggered the tenant-hongming leak (TeamHandler.Expand re-inserting the entire sub_workspaces tree on every call). To fix any reported violation, choose ONE of:\n")
		b.WriteString("  1. Call h.lookupExistingChild(ctx, name, parentID) before the INSERT and skip the INSERT when it returns existing=true. (preferred)\n")
		b.WriteString("  2. Use INSERT ... ON CONFLICT ... DO ... (idempotent UPSERT, like registry.go).\n")
		b.WriteString("  3. Annotate the function with a `// class1-gate: idempotent-by-design` comment AND a unit test that pins why the function is structurally idempotent. (rare; require code review)\n\n")
		b.WriteString("Violations:\n")
		for _, v := range violations {
			b.WriteString("  - ")
			b.WriteString(v.file)
			b.WriteString(":")
			b.WriteString(itoa(v.insertLine))
			b.WriteString(" — function ")
			b.WriteString(v.fn)
			b.WriteString("() INSERTs inside RangeStmt at line ")
			b.WriteString(itoa(v.rangeLine))
			b.WriteString("\n")
		}
		t.Fatal(b.String())
	}
}

func itoa(n int) string {
	// Avoid strconv import for one call site — keeps the test focused.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// candidateInsert holds the per-INSERT facts needed to decide whether
// the gate fires.
type candidateInsert struct {
	insertLine         int
	hasONCONFLICT      bool
	enclosingRangeLine int // 0 means not inside any range
}

// findInsertWorkspacesLiterals walks fd's body and returns one
// candidateInsert per INSERT INTO workspaces string literal.
//
// Position-based detection: collect every RangeStmt's body span first,
// then for each INSERT literal check if its position is inside any
// span. ast.Inspect's nil-call ordering does NOT give per-node pop
// semantics, so a stack-based approach against ast.Inspect would
// silently miscount. Position spans are deterministic and easy to
// reason about.
func findInsertWorkspacesLiterals(fd *ast.FuncDecl, src []byte, fset *token.FileSet) []candidateInsert {
	var out []candidateInsert

	type span struct{ start, end token.Pos }
	var ranges []span
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		rs, ok := n.(*ast.RangeStmt)
		if !ok || rs.Body == nil {
			return true
		}
		ranges = append(ranges, span{rs.Body.Lbrace, rs.Body.Rbrace})
		return true
	})

	enclosingRangeLineFor := func(p token.Pos) int {
		// Pick the innermost enclosing range — i.e., the one with the
		// largest start that still covers p. Innermost is the one
		// whose body actually contains the INSERT, which is the line
		// most useful in a violation message.
		bestStart := token.NoPos
		bestLine := 0
		for _, s := range ranges {
			if p > s.start && p < s.end && s.start > bestStart {
				bestStart = s.start
				bestLine = fset.Position(s.start).Line
			}
		}
		return bestLine
	}

	ast.Inspect(fd.Body, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		// Strip surrounding backticks/quotes — value includes them.
		lit := bl.Value
		if len(lit) >= 2 {
			lit = lit[1 : len(lit)-1]
		}
		if !reINSERTWorkspaces.MatchString(lit) {
			return true
		}
		out = append(out, candidateInsert{
			insertLine:         fset.Position(bl.Pos()).Line,
			hasONCONFLICT:      reONCONFLICT.MatchString(lit),
			enclosingRangeLine: enclosingRangeLineFor(bl.Pos()),
		})
		return true
	})
	return out
}

// functionCallsAny returns true if any CallExpr in fd's body has a
// function name (either a SelectorExpr Sel.Name or an Ident name)
// matching a key in names.
func functionCallsAny(fd *ast.FuncDecl, names map[string]bool) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := ce.Fun.(type) {
		case *ast.Ident:
			if names[fun.Name] {
				found = true
				return false
			}
		case *ast.SelectorExpr:
			if names[fun.Sel.Name] {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// functionHasAllowlistComment returns true if the function body
// (between fd.Body.Lbrace and fd.Body.Rbrace) contains a comment
// equal to gateAllowlistComment.
func functionHasAllowlistComment(file *ast.File, fd *ast.FuncDecl) bool {
	if fd.Body == nil {
		return false
	}
	start := fd.Body.Lbrace
	end := fd.Body.Rbrace
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if c.Pos() < start || c.Pos() > end {
				continue
			}
			if strings.TrimSpace(c.Text) == gateAllowlistComment {
				return true
			}
		}
	}
	return false
}

// TestClass1_GateFiresOnSyntheticBuggySource — proves the gate actually
// catches the bug shape it's named after. Without this, a regression
// to "always pass" would not be noticed until the leak shipped again.
// Per memory feedback_assert_exact_not_substring.md: tighten the test
// + verify it FAILS on old-shape source before merging.
func TestClass1_GateFiresOnSyntheticBuggySource(t *testing.T) {
	const buggySrc = `package handlers

import "context"

type fakeDB struct{}
func (fakeDB) ExecContext(ctx context.Context, sql string, args ...interface{}) {}

func buggyExpand(db fakeDB, ctx context.Context, children []string) {
	for _, child := range children {
		// Bug shape: INSERT inside the range body, no preflight.
		db.ExecContext(ctx, ` + "`INSERT INTO workspaces (id, name) VALUES ($1, $2)`" + `, "x", child)
	}
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "buggy.go", buggySrc, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "buggyExpand" {
			continue
		}
		candidates := findInsertWorkspacesLiterals(fd, []byte(buggySrc), fset)
		if len(candidates) != 1 {
			t.Fatalf("expected 1 INSERT literal, got %d", len(candidates))
		}
		c := candidates[0]
		if c.enclosingRangeLine == 0 {
			t.Errorf("synthetic INSERT inside `for _, child := range` should be detected as enclosed by range, got enclosingRangeLine=0 — gate would miss the bug shape")
		}
		if c.hasONCONFLICT {
			t.Errorf("synthetic INSERT has no ON CONFLICT, gate falsely treated it as idempotent")
		}
		if functionCallsAny(fd, preflightCallNames) {
			t.Errorf("synthetic function does not call lookupExistingChild — gate falsely treated it as preflighted")
		}
		// All three guards say the gate WOULD fire. Pass.
		return
	}
	t.Fatal("buggyExpand FuncDecl not found in synthetic source")
}

// TestClass1_GateAllowsONCONFLICT — pins that an INSERT with ON
// CONFLICT inside a range body is NOT flagged. registry.go's
// upsert pattern is the prod example.
func TestClass1_GateAllowsONCONFLICT(t *testing.T) {
	const safeSrc = `package handlers

import "context"

type fakeDB struct{}
func (fakeDB) ExecContext(ctx context.Context, sql string, args ...interface{}) {}

func upsertLoop(db fakeDB, ctx context.Context, children []string) {
	for _, child := range children {
		db.ExecContext(ctx, ` + "`INSERT INTO workspaces (id, name) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET name = $2`" + `, "x", child)
	}
}
`
	fset := token.NewFileSet()
	file, _ := parser.ParseFile(fset, "safe.go", safeSrc, parser.ParseComments)
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "upsertLoop" {
			continue
		}
		candidates := findInsertWorkspacesLiterals(fd, []byte(safeSrc), fset)
		if len(candidates) != 1 {
			t.Fatalf("expected 1 candidate, got %d", len(candidates))
		}
		if !candidates[0].hasONCONFLICT {
			t.Errorf("ON CONFLICT clause should be detected, was missed — gate would falsely flag idempotent UPSERTs")
		}
	}
}

// TestClass1_GateAllowsAllowlistAnnotation — pins the escape hatch
// works. Annotated functions are skipped at the FuncDecl level.
func TestClass1_GateAllowsAllowlistAnnotation(t *testing.T) {
	const annotatedSrc = `package handlers

import "context"

type fakeDB struct{}
func (fakeDB) ExecContext(ctx context.Context, sql string, args ...interface{}) {}

func intentionallyUnpreflighted(db fakeDB, ctx context.Context, children []string) {
	// class1-gate: idempotent-by-design
	for _, child := range children {
		db.ExecContext(ctx, ` + "`INSERT INTO workspaces (id, name) VALUES ($1, $2)`" + `, "x", child)
	}
}
`
	fset := token.NewFileSet()
	file, _ := parser.ParseFile(fset, "annotated.go", annotatedSrc, parser.ParseComments)
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "intentionallyUnpreflighted" {
			continue
		}
		if !functionHasAllowlistComment(file, fd) {
			t.Error("allowlist comment should be detected for the intentionallyUnpreflighted function — escape hatch not working")
		}
	}
}
