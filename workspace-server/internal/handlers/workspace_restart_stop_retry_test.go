package handlers

// workspace_restart_stop_retry_test.go — pins the contract of
// cpStopWithRetry, the helper introduced 2026-05-02 in
// fix/restart-stop-retry-then-flag.
//
// Why this helper exists, in brief: workspace_restart.go's two cpProv.Stop
// callers (the interactive Restart handler + the auto-restart cycle's
// stopForRestart) both used to log-and-continue on Stop failure. After
// PR #2500 made CPProvisioner.Stop surface CP non-2xx as an error, those
// log-and-continue paths became the actual leak generator: every transient
// CP/AWS hiccup = one orphan EC2 alongside the freshly provisioned one.
// 13 zombie workspace EC2s on demo-prep staging traced to this exact path.
//
// Helper contract:
//   - bounded retry (default 3 attempts, 1s/2s/4s backoff)
//   - early-exit on ctx cancel (don't stall the goroutine)
//   - on retry exhaustion: loud structured log `LEAK-SUSPECT cpProv.Stop ...`
//   - always returns (no error) — caller proceeds to reprovision regardless,
//     because Restart's contract is "make the workspace alive again" and
//     stranding the user with a dead workspace is worse than one leaked EC2
//     that the CP-side orphan reconciler will catch.
//
// Tests below cover every branch:
//   - no-op when cpProv is nil
//   - succeeds on first try (no retry log noise)
//   - succeeds after transient failures (retry log on success)
//   - exhausts retries, emits LEAK-SUSPECT
//   - ctx cancel mid-retry exits early without sleeping the backoff
//
// Plus an AST gate that pins the helper-only invariant: any future inline
// `h.cpProv.Stop(...)` in workspace_restart.go must go through cpStopWithRetry.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
)

// scriptedCPStop returns a fakeCPStop that returns errs[i] on call i, then
// nil for any further calls. Lets each test express its retry expectation
// declaratively without an ad-hoc counter inside the stub.
type scriptedCPStop struct {
	errs      []error
	calls     int
	stopDelay time.Duration // optional per-call sleep to prove ctx.Done wins
}

// satisfies provisioner.CPProvisionerAPI for the methods we touch in this test.
// The other methods are unused; we don't bother stubbing them with state.
func (s *scriptedCPStop) Stop(ctx context.Context, _ string) error {
	if s.stopDelay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.stopDelay):
		}
	}
	i := s.calls
	s.calls++
	if i < len(s.errs) {
		return s.errs[i]
	}
	return nil
}
func (s *scriptedCPStop) Start(_ context.Context, _ provisioner.WorkspaceConfig) (string, error) {
	return "", nil
}
func (s *scriptedCPStop) IsRunning(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (s *scriptedCPStop) GetConsoleOutput(_ context.Context, _ string) (string, error) {
	return "", nil
}

// captureLog is provided by workspace_provision_panic_test.go in this
// package — returns a buffer that accumulates log output for the test's
// lifetime. We don't redeclare it here.

// shrinkRetryBackoff swaps cpStopRetryBaseDelay to a tiny value so retry
// tests don't burn 7s of wall time. Restored on test cleanup.
func shrinkRetryBackoff(t *testing.T) {
	t.Helper()
	prev := cpStopRetryBaseDelay
	cpStopRetryBaseDelay = 1 * time.Millisecond
	t.Cleanup(func() { cpStopRetryBaseDelay = prev })
}

// --- behavior tests ---

func TestCPStopWithRetry_NoOpWhenCPProvNil(t *testing.T) {
	buf := captureLog(t)
	h := &WorkspaceHandler{} // cpProv left nil
	h.cpStopWithRetry(context.Background(), "ws-x", "Restart")
	if buf.Len() != 0 {
		t.Errorf("expected silent no-op when cpProv is nil; got log: %q", buf.String())
	}
}

func TestCPStopWithRetry_SucceedsOnFirstTry(t *testing.T) {
	buf := captureLog(t)
	stub := &scriptedCPStop{}
	h := &WorkspaceHandler{cpProv: stub}
	h.cpStopWithRetry(context.Background(), "ws-1", "Restart")
	if stub.calls != 1 {
		t.Errorf("expected exactly 1 Stop call on success; got %d", stub.calls)
	}
	out := buf.String()
	if strings.Contains(out, "succeeded on attempt") {
		t.Errorf("first-try success should not log a retry-success line; got %q", out)
	}
	if strings.Contains(out, "LEAK-SUSPECT") {
		t.Errorf("first-try success must not emit LEAK-SUSPECT; got %q", out)
	}
}

func TestCPStopWithRetry_RetriesTransientThenSucceeds(t *testing.T) {
	shrinkRetryBackoff(t)
	buf := captureLog(t)
	stub := &scriptedCPStop{errs: []error{
		errors.New("transient hiccup"),
		errors.New("still flaky"),
	}}
	h := &WorkspaceHandler{cpProv: stub}
	h.cpStopWithRetry(context.Background(), "ws-flaky", "Auto-restart")
	if stub.calls != 3 {
		t.Errorf("expected 3 Stop calls (2 fails + 1 success); got %d", stub.calls)
	}
	out := buf.String()
	if !strings.Contains(out, "Auto-restart: cpProv.Stop(ws-flaky) succeeded on attempt 3") {
		t.Errorf("expected eventual-success log; got %q", out)
	}
	if strings.Contains(out, "LEAK-SUSPECT") {
		t.Errorf("eventual success must not emit LEAK-SUSPECT; got %q", out)
	}
}

func TestCPStopWithRetry_AllRetriesExhaustEmitsLeakSuspect(t *testing.T) {
	shrinkRetryBackoff(t)
	buf := captureLog(t)
	stub := &scriptedCPStop{errs: []error{
		errors.New("cp 502 attempt 1"),
		errors.New("cp 502 attempt 2"),
		errors.New("cp 502 attempt 3 — final"),
	}}
	h := &WorkspaceHandler{cpProv: stub}
	h.cpStopWithRetry(context.Background(), "ws-doomed", "Auto-restart")
	if stub.calls != cpStopRetryAttempts {
		t.Errorf("expected %d Stop calls when all fail; got %d", cpStopRetryAttempts, stub.calls)
	}
	out := buf.String()
	// The LEAK-SUSPECT line is the bridge to the CP-side orphan reconciler.
	// Assert every key field is present so a future stringer change can't
	// silently break ops grep / parser.
	for _, want := range []string{
		"LEAK-SUSPECT cpProv.Stop",
		"workspace_id=ws-doomed",
		"source=Auto-restart",
		fmt.Sprintf("attempts=%d", cpStopRetryAttempts),
		"cp 502 attempt 3 — final", // the LAST error, not an earlier one
	} {
		if !strings.Contains(out, want) {
			t.Errorf("LEAK-SUSPECT log missing %q; got %q", want, out)
		}
	}
}

func TestCPStopWithRetry_RespectsContextCancellation(t *testing.T) {
	// Use the real (long) backoff so the test fails noisily if ctx-cancel
	// isn't honored: a non-cancelling implementation would block ~1 second
	// before the second attempt and the elapsed assertion below would fail.
	buf := captureLog(t)
	stub := &scriptedCPStop{errs: []error{
		errors.New("first fail"),
		errors.New("second fail"),
		errors.New("third fail"),
	}}
	h := &WorkspaceHandler{cpProv: stub}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel before the goroutine starts retrying so the very first
	// post-attempt-1 select hits the ctx.Done branch.
	cancel()

	start := time.Now()
	h.cpStopWithRetry(ctx, "ws-cancel", "Restart")
	elapsed := time.Since(start)

	// One attempt before bailing on cancel — never a second.
	if stub.calls != 1 {
		t.Errorf("expected 1 Stop call before ctx-cancel exit; got %d", stub.calls)
	}
	// Backoff is 1s minimum; if we slept it the test would take >=1s.
	if elapsed >= 500*time.Millisecond {
		t.Errorf("ctx-cancel should exit well under 500ms; took %v (likely slept the backoff)", elapsed)
	}
	out := buf.String()
	if !strings.Contains(out, "abandoned mid-retry: ctx cancelled") {
		t.Errorf("expected ctx-cancel log line; got %q", out)
	}
	if strings.Contains(out, "LEAK-SUSPECT") {
		// Ctx-cancel is operator-initiated (e.g. shutdown drain). It's
		// a different signal than "we tried hard and failed" — emitting
		// LEAK-SUSPECT here would noise up the orphan-reconciler queue
		// with workspaces we never had a chance to retry. Keep them
		// distinct in the log so triage doesn't conflate them.
		t.Errorf("ctx-cancel should NOT emit LEAK-SUSPECT (different signal than retry exhaustion); got %q", out)
	}
}

// --- AST gate ---
//
// Pins the invariant: in workspace_restart.go, the ONLY direct
// `h.cpProv.Stop(...)` call lives inside cpStopWithRetry. Any other call
// is a regression — re-introducing the pre-fix log-and-continue shape that
// silently leaks an EC2 on every transient CP failure.
//
// Same family as TestRestart_StopRunsInsideGoroutine in
// workspace_restart_async_test.go (per feedback memory: behavior-based AST
// gates beat name-list gates).

func TestRestart_CPStopOnlyInsideRetryHelper(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(".", "workspace_restart.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse workspace_restart.go: %v", err)
	}

	type violation struct {
		fn   string
		line int
	}
	var bad []violation

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Recv == nil {
			continue
		}
		// cpStopWithRetry is the ONE allowed home for h.cpProv.Stop.
		if fn.Name.Name == "cpStopWithRetry" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Stop" {
				return true
			}
			inner, ok := sel.X.(*ast.SelectorExpr)
			if !ok || inner.Sel.Name != "cpProv" {
				return true
			}
			bad = append(bad, violation{
				fn:   fn.Name.Name,
				line: fset.Position(call.Pos()).Line,
			})
			return true
		})
	}

	for _, v := range bad {
		t.Errorf(
			"workspace_restart.go:%d %s calls h.cpProv.Stop directly. "+
				"Use h.cpStopWithRetry(ctx, workspaceID, %q) instead — direct calls re-introduce "+
				"the silent-leak shape that produced the 2026-05-01 demo-prep zombie EC2 incident "+
				"(13 orphans on a 0-customer staging tenant). cpStopWithRetry adds bounded retry + "+
				"a LEAK-SUSPECT structured log on exhaustion so the orphan reconciler can correlate. "+
				"See fix/restart-stop-retry-then-flag (2026-05-02).",
			v.line, v.fn, v.fn,
		)
	}
}
