package handlers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestFindRunningContainer_RoutesThroughProvisionerSSOT is a behavior-based
// AST gate: it pins the invariant that PluginsHandler.findRunningContainer
// MUST go through provisioner.RunningContainerName for its is-running check,
// instead of carrying its own copy of cli.ContainerInspect logic.
//
// Background — molecule-core#10: a parallel impl of "is the workspace's
// container running" used to live in plugins.go. It drifted from the
// canonical impl in healthsweep (which goes through Provisioner.IsRunning
// → RunningContainerName) on edge cases like "transient daemon error" —
// the duplicate would 503 with a misleading message while healthsweep
// correctly stayed defensive. Consolidating onto RunningContainerName as
// the SSOT prevents any future copy from re-introducing that drift.
//
// Mutation invariant: if a future PR replaces the provisioner call with
// `h.docker.ContainerInspect(...)` directly, this test fails. That's the
// signal to either (a) extend RunningContainerName's contract OR (b)
// document why this call site needs to differ. Either way: the drift
// gets a reviewer's attention instead of shipping silently.
func TestFindRunningContainer_RoutesThroughProvisionerSSOT(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "plugins.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse plugins.go: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		f, ok := n.(*ast.FuncDecl)
		if !ok || f.Name.Name != "findRunningContainer" {
			return true
		}
		// Confirm receiver is *PluginsHandler so we don't pick up an unrelated
		// helper of the same name. ast.Recv is a FieldList — receivers carry
		// at most one field.
		if f.Recv == nil || len(f.Recv.List) == 0 {
			return true
		}
		fn = f
		return false
	})

	if fn == nil {
		t.Fatal("findRunningContainer not found in plugins.go — was it renamed? update this test or the SSOT routing assumption")
	}

	var (
		callsRunningContainerName bool
		callsContainerInspectRaw  bool
	)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// Pkg.Func form: provisioner.RunningContainerName(...)
		if pkgIdent, ok := sel.X.(*ast.Ident); ok {
			if pkgIdent.Name == "provisioner" && sel.Sel.Name == "RunningContainerName" {
				callsRunningContainerName = true
			}
		}
		// Receiver-then-method form: h.docker.ContainerInspect(...) /
		// p.cli.ContainerInspect(...) — anything ending in
		// .ContainerInspect that's NOT routed through provisioner.
		if sel.Sel.Name == "ContainerInspect" {
			callsContainerInspectRaw = true
		}
		return true
	})

	if !callsRunningContainerName {
		t.Errorf(
			"findRunningContainer must call provisioner.RunningContainerName for the SSOT inspect — see molecule-core#10. Found no such call.",
		)
	}
	if callsContainerInspectRaw {
		t.Errorf(
			"findRunningContainer carries a direct ContainerInspect call. This is the parallel-impl drift molecule-core#10 fixed. " +
				"Either route through provisioner.RunningContainerName OR — if a new use case truly needs a different inspect — extend RunningContainerName's contract first and update this gate to allow the specific delta.",
		)
	}
}

// TestProvisionerIsRunning_RoutesThroughRunningContainerName mirrors the
// gate above but for the OTHER consumer of the SSOT — Provisioner.IsRunning
// (called by healthsweep). If a future refactor makes IsRunning carry its
// own ContainerInspect again, the two consumers' edge-case behaviors will
// silently drift. Keep them yoked.
func TestProvisionerIsRunning_RoutesThroughRunningContainerName(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../provisioner/provisioner.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse provisioner.go: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		f, ok := n.(*ast.FuncDecl)
		if !ok || f.Name.Name != "IsRunning" || f.Recv == nil {
			return true
		}
		// The receiver type must be *Provisioner specifically. CPProvisioner
		// has its own IsRunning that talks HTTP to the controlplane and is
		// out of scope for this gate.
		if !receiverIs(f, "Provisioner") {
			return true
		}
		fn = f
		return false
	})
	if fn == nil {
		t.Fatal("Provisioner.IsRunning not found — was it renamed? update this test")
	}

	var (
		callsRunningContainerName bool
		callsContainerInspectRaw  bool
	)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// Same-package call: bare identifier (e.g. RunningContainerName(...)).
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "RunningContainerName" {
			callsRunningContainerName = true
			return true
		}
		// Selector call: pkg.Func (e.g. provisioner.RunningContainerName)
		// OR recv.Method (e.g. p.cli.ContainerInspect).
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "RunningContainerName":
			callsRunningContainerName = true
		case "ContainerInspect":
			callsContainerInspectRaw = true
		}
		return true
	})

	if !callsRunningContainerName {
		t.Errorf("Provisioner.IsRunning must call RunningContainerName for the SSOT inspect — see molecule-core#10")
	}
	if callsContainerInspectRaw {
		t.Errorf("Provisioner.IsRunning carries a direct ContainerInspect call; route through RunningContainerName instead")
	}
}

// receiverIs reports whether fn's receiver is `*<typeName>` or `<typeName>`.
func receiverIs(fn *ast.FuncDecl, typeName string) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return false
	}
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	id, ok := expr.(*ast.Ident)
	return ok && strings.EqualFold(id.Name, typeName)
}
