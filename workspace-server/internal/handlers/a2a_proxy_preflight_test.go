package handlers

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
)

// preflightLocalProv is a controllable LocalProvisionerAPI stub for the
// preflight tests (#36). Other API methods panic to guard against tests
// that should be using a different stub.
type preflightLocalProv struct {
	running    bool
	err        error
	calls      int
	calledWith []string
}

func (p *preflightLocalProv) IsRunning(_ context.Context, workspaceID string) (bool, error) {
	p.calls++
	p.calledWith = append(p.calledWith, workspaceID)
	return p.running, p.err
}
func (p *preflightLocalProv) Start(_ context.Context, _ provisioner.WorkspaceConfig) (string, error) {
	panic("preflightLocalProv: Start not implemented")
}
func (p *preflightLocalProv) Stop(_ context.Context, _ string) error {
	panic("preflightLocalProv: Stop not implemented")
}
func (p *preflightLocalProv) ExecRead(_ context.Context, _, _ string) ([]byte, error) {
	panic("preflightLocalProv: ExecRead not implemented")
}
func (p *preflightLocalProv) RemoveVolume(_ context.Context, _ string) error {
	panic("preflightLocalProv: RemoveVolume not implemented")
}
func (p *preflightLocalProv) VolumeHasFile(_ context.Context, _, _ string) (bool, error) {
	panic("preflightLocalProv: VolumeHasFile not implemented")
}
func (p *preflightLocalProv) WriteAuthTokenToVolume(_ context.Context, _, _ string) error {
	panic("preflightLocalProv: WriteAuthTokenToVolume not implemented")
}

// TestPreflight_ContainerRunning_ReturnsNil — IsRunning(true,nil): forward
// proceeds. preflight returns nil → caller continues to dispatchA2A.
func TestPreflight_ContainerRunning_ReturnsNil(t *testing.T) {
	_ = setupTestDB(t)
	stub := &preflightLocalProv{running: true, err: nil}
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.provisioner = stub

	if err := h.preflightContainerHealth(context.Background(), "ws-running-123"); err != nil {
		t.Fatalf("preflight should return nil when container running, got %+v", err)
	}
	if stub.calls != 1 {
		t.Errorf("IsRunning should be called exactly once, got %d", stub.calls)
	}
	if len(stub.calledWith) != 1 || stub.calledWith[0] != "ws-running-123" {
		t.Errorf("IsRunning should be called with workspace id, got %v", stub.calledWith)
	}
}

// TestPreflight_ContainerNotRunning_StructuredFastFail — IsRunning(false,nil):
// preflight returns structured 503 with restarting=true + preflight=true, AND
// triggers the offline-flip + WORKSPACE_OFFLINE broadcast + async restart.
// This is the load-bearing case — saves the caller 2-30s of network timeout.
func TestPreflight_ContainerNotRunning_StructuredFastFail(t *testing.T) {
	mock := setupTestDB(t)
	_ = setupTestRedis(t)
	stub := &preflightLocalProv{running: false, err: nil}
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.provisioner = stub

	// Expect the offline-flip UPDATE.
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusOffline, "ws-dead-456").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Broadcaster's INSERT INTO structure_events fires too — best-effort
	// log entry for the WORKSPACE_OFFLINE event. Match permissively.
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	proxyErr := h.preflightContainerHealth(context.Background(), "ws-dead-456")
	if proxyErr == nil {
		t.Fatal("preflight should return *proxyA2AError when container not running")
	}
	if proxyErr.Status != 503 {
		t.Errorf("expected 503, got %d", proxyErr.Status)
	}
	if got := proxyErr.Response["restarting"]; got != true {
		t.Errorf("response should mark restarting=true, got %v", got)
	}
	if got := proxyErr.Response["preflight"]; got != true {
		t.Errorf("response should mark preflight=true so callers can distinguish from reactive containerDead, got %v", got)
	}
	if got := proxyErr.Response["error"]; got != "workspace container not running — restart triggered" {
		t.Errorf("error message mismatch, got %q", got)
	}

	// Note: broadcaster firing is exercised by the production path's
	// h.broadcaster.RecordAndBroadcast call but not asserted here — the
	// real *events.Broadcaster doesn't expose received events for inspection.
	// The DB UPDATE expectation is sufficient to pin the offline-flip path.
}

// TestPreflight_TransientError_FailsSoftAsAlive — IsRunning(true,err): the
// (true, err) "fail-soft" contract — preflight returns nil so the optimistic
// forward runs; reactive maybeMarkContainerDead handles a real failure later.
// This pin is critical: a flaky daemon must NOT trigger a restart cascade.
func TestPreflight_TransientError_FailsSoftAsAlive(t *testing.T) {
	_ = setupTestDB(t)
	stub := &preflightLocalProv{running: true, err: errors.New("docker daemon EOF")}
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.provisioner = stub

	if err := h.preflightContainerHealth(context.Background(), "ws-flaky-789"); err != nil {
		t.Fatalf("preflight should return nil on transient error (fail-soft), got %+v", err)
	}
	// No DB UPDATE expected — sqlmock would complain about unexpected calls
	// at test cleanup if the offline-flip path fired.
}

// TestProxyA2A_Preflight_RoutesThroughProvisionerSSOT — AST gate (#36 mirror
// of #12's gate). Pins the invariant that preflightContainerHealth uses the
// SSOT Provisioner.IsRunning helper, NOT a parallel docker.ContainerInspect
// of its own.
//
// Mutation invariant: if a future PR replaces h.provisioner.IsRunning with
// a direct cli.ContainerInspect call, this test fails. That's the signal to
// either (a) extend Provisioner.IsRunning's contract OR (b) document why
// this call site needs to differ. Either way, the drift gets a reviewer's
// attention instead of shipping silently.
func TestProxyA2A_Preflight_RoutesThroughProvisionerSSOT(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "a2a_proxy_helpers.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse a2a_proxy_helpers.go: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		f, ok := n.(*ast.FuncDecl)
		if !ok || f.Name.Name != "preflightContainerHealth" {
			return true
		}
		fn = f
		return false
	})
	if fn == nil {
		t.Fatal("preflightContainerHealth not found — was it renamed? update this gate or the SSOT routing assumption")
	}

	var (
		callsIsRunning             bool
		callsContainerInspectRaw   bool
		callsRunningContainerNameDirect bool
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
		switch sel.Sel.Name {
		case "IsRunning":
			callsIsRunning = true
		case "ContainerInspect":
			callsContainerInspectRaw = true
		case "RunningContainerName":
			// Direct RunningContainerName is also acceptable SSOT — but
			// preferring IsRunning keeps the (bool, error) contract that
			// already exists in the helper API surface.
			callsRunningContainerNameDirect = true
		}
		return true
	})

	if !callsIsRunning && !callsRunningContainerNameDirect {
		t.Errorf("preflightContainerHealth must call provisioner.IsRunning OR provisioner.RunningContainerName for the SSOT health check — see molecule-core#36. Found neither.")
	}
	if callsContainerInspectRaw {
		t.Errorf("preflightContainerHealth carries a direct ContainerInspect call. This is the parallel-impl drift molecule-core#36 fixed. " +
			"Either route through provisioner.IsRunning OR — if a new use case truly needs a different inspect — extend the helper's contract first and update this gate to allow the specific delta.")
	}
}
