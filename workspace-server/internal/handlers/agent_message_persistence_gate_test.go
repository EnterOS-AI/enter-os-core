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

// TestAgentMessageBroadcastsArePersisted is a forward-looking AST
// gate: every function in this package that broadcasts an
// `AGENT_MESSAGE` WebSocket event MUST also call
// `INSERT INTO activity_logs` somewhere in its body.
//
// The reno-stars production data-loss bug (CEO Ryan PC's long-form
// onboarding-friction message visible live but missing on reload)
// happened because mcp_tools.go:toolSendMessageToUser broadcast WS
// without a paired INSERT — while the HTTP /notify sibling DID
// persist. The fix added the INSERT; this gate prevents the regression
// class from re-emerging in any future chat-bearing tool.
//
// Why an AST gate vs a code-review checklist (per memory
// feedback_behavior_based_ast_gates.md): "pin invariants by what a
// function calls, not what it's named". The shape that loses data is:
//
//	BroadcastOnly(_, "AGENT_MESSAGE", _) without an INSERT companion
//
// Any new tool that emits AGENT_MESSAGE must persist or the next
// canvas refresh drops the message — same shape as reno-stars. A
// reviewer can miss this; the AST walk can't.
//
// Allowlist: empty by intent. If a future use case genuinely needs
// fire-and-forget broadcast (e.g., transient typing indicators that
// should NOT survive reload), add an entry here AND document why.
// "Doesn't need to persist" is rarely the right answer for chat —
// the canvas history is the source of truth.
func TestAgentMessageBroadcastsArePersisted(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatalf("readdir %s: %v", wd, err)
	}

	type violation struct {
		file string
		fn   string
	}
	var violations []violation

	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(wd, name)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if !funcEmitsAgentMessageBroadcast(fn) {
				continue
			}
			if !funcInsertsIntoActivityLogs(fn) {
				violations = append(violations, violation{file: name, fn: fn.Name.Name})
			}
		}
	}

	if len(violations) > 0 {
		sort.Slice(violations, func(i, j int) bool {
			if violations[i].file != violations[j].file {
				return violations[i].file < violations[j].file
			}
			return violations[i].fn < violations[j].fn
		})
		var buf strings.Builder
		for _, v := range violations {
			buf.WriteString("  - ")
			buf.WriteString(v.file)
			buf.WriteString(":")
			buf.WriteString(v.fn)
			buf.WriteString("\n")
		}
		t.Errorf(`function(s) broadcast `+"`AGENT_MESSAGE`"+` without persisting to activity_logs:

%s
This is the reno-stars data-loss regression class: live message
visible to the user, but missing on reload because activity_log was
never written. Every chat-bearing broadcast MUST be paired with:

  INSERT INTO activity_logs (workspace_id, activity_type, method,
    summary, response_body, status)
  VALUES ($1, 'a2a_receive', 'notify', $2, $3::jsonb, 'ok')

See activity.go:Notify and mcp_tools.go:toolSendMessageToUser for
the canonical shapes. Don't add an allowlist entry without a
documented reason — the canvas chat history is the source of truth
and silently dropping messages is a P0 user trust break.`,
			buf.String())
	}
}

// funcEmitsAgentMessageBroadcast walks fn.Body for any CallExpr that
// looks like `*.BroadcastOnly(_, "AGENT_MESSAGE", _)`.
func funcEmitsAgentMessageBroadcast(fn *ast.FuncDecl) bool {
	var found bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "BroadcastOnly" {
			return true
		}
		// BroadcastOnly(workspaceID, eventType, payload) — the second
		// arg is the event name. Match by string-literal value.
		if len(call.Args) < 2 {
			return true
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		raw := lit.Value
		if unq, err := strconv.Unquote(raw); err == nil {
			raw = unq
		}
		if raw == "AGENT_MESSAGE" {
			found = true
			return false
		}
		return true
	})
	return found
}

// funcInsertsIntoActivityLogs walks fn.Body for any STRING BasicLit
// whose body contains `INSERT INTO activity_logs` (the SQL literal
// passed to ExecContext). Matches the substring rather than a strict
// regex because we don't care about the exact INSERT shape here —
// only that the function persists. Specific shape pinning lives in
// the per-handler test (see TestMCPHandler_SendMessageToUser_*).
func funcInsertsIntoActivityLogs(fn *ast.FuncDecl) bool {
	var found bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		raw := lit.Value
		if unq, err := strconv.Unquote(raw); err == nil {
			raw = unq
		}
		if strings.Contains(raw, "INSERT INTO activity_logs") {
			found = true
			return false
		}
		return true
	})
	return found
}
