package handlers

// workspace_restart_async_test.go — behavior-based AST gate that pins
// the invariant introduced 2026-04-30 in fix/restart-async-stop:
//
//   The /workspaces/:id/restart handler MUST NOT call provisioner.Stop
//   or cpProv.Stop synchronously before returning the HTTP response.
//
// Why: CPProvisioner.Stop is `DELETE /cp/workspaces/:id?instance_id=...`
// → CP → AWS EC2 terminate. Right after a platform-wide redeploy (every
// tenant queues a CP request at once) this can exceed the canvas's 15s
// HTTP timeout, surfacing as a misleading "signal timed out" red banner
// even though the async re-provision goroutine continues and the restart
// actually succeeds. Caught on hongmingwang hermes workspace
// 32993ee7-…cb9d75d112a5 right after the heartbeat-fix redeploy.
//
// The fix moves Stop into the same goroutine as provisionWorkspaceCP /
// provisionWorkspaceOpts so the handler returns ~immediately (only the
// metadata lookup + DB UPDATE remain on the response path). This gate
// catches anyone re-introducing the pre-fix shape — easy to do because
// "stop, then provision" reads as natural ordering at the handler top
// level.
//
// Behavior-based: matches `<recv>.{provisioner,cpProv}.Stop(...)` calls
// regardless of variable names; only the AST shape matters. A future
// rename of either field is still pinned, same family as the
// callsProvisionStart gate in workspace_provision_shared_test.go.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// TestRestart_StopRunsInsideGoroutine asserts the /restart handler does
// not call provisioner.Stop / cpProv.Stop at the top level of its body.
// Any such call must be nested inside a *ast.FuncLit (the `go func() {
// ... }()` block) so the HTTP response can return before Stop completes.
func TestRestart_StopRunsInsideGoroutine(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(".", "workspace_restart.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse workspace_restart.go: %v", err)
	}

	var restartFn *ast.FuncDecl
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if fn.Name.Name == "Restart" && fn.Recv != nil {
			restartFn = fn
			break
		}
	}
	if restartFn == nil {
		t.Fatal("Restart method not found in workspace_restart.go — did the handler get renamed?")
	}

	// Walk the function body. For every call to <recv>.<provField>.Stop,
	// record whether it appears at the top level (any ancestor frame is
	// the Restart body) or inside a FuncLit (i.e. inside `go func() { ... }`).
	type violation struct {
		line  int
		field string
	}
	var topLevelStops []violation

	var inspect func(node ast.Node, insideFuncLit bool)
	inspect = func(node ast.Node, insideFuncLit bool) {
		if node == nil {
			return
		}
		switch n := node.(type) {
		case *ast.FuncLit:
			// Descend into the func literal body, but mark all
			// children as inside-FuncLit. This is the goroutine
			// boundary we want.
			ast.Inspect(n, func(child ast.Node) bool {
				if child == n {
					return true
				}
				inspect(child, true)
				return false
			})
			return
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Stop" {
				if inner, ok := sel.X.(*ast.SelectorExpr); ok {
					switch inner.Sel.Name {
					case "provisioner", "cpProv":
						if !insideFuncLit {
							topLevelStops = append(topLevelStops, violation{
								line:  fset.Position(n.Pos()).Line,
								field: inner.Sel.Name,
							})
						}
					}
				}
			}
		}
		// Continue walking children of non-FuncLit nodes.
		ast.Inspect(node, func(child ast.Node) bool {
			if child == node {
				return true
			}
			inspect(child, insideFuncLit)
			return false
		})
	}
	inspect(restartFn.Body, false)

	for _, v := range topLevelStops {
		t.Errorf(
			"workspace_restart.go:%d Restart calls h.%s.Stop synchronously at the top level. "+
				"Stop must run inside the `go func() { ... }()` goroutine that wraps "+
				"provisionWorkspaceCP/provisionWorkspaceOpts — otherwise the canvas's 15s "+
				"HTTP timeout fires before the response, surfacing 'signal timed out' even "+
				"when the restart actually succeeds. See fix/restart-async-stop (2026-04-30).",
			v.line, v.field,
		)
	}
}
