package handlers

// Pins the backend-dispatcher invariant added 2026-05-04.
//
// Before the fix, TeamHandler.Expand hardcoded the Docker provisioner
// (provisionWorkspace), so on a SaaS tenant where the workspace-server
// has no docker socket, child workspaces were created as DB rows but
// never got an EC2 instance. The 600s sweeper then logged the misleading
// "container started but never called /registry/register".
//
// The fix centralizes backend selection in
// WorkspaceHandler.provisionWorkspaceAuto and routes both Create and
// TeamHandler.Expand through it. These tests pin:
//
//  1. Auto returns false when neither backend is wired (caller must
//     persist + mark-failed itself).
//  2. Auto picks CP when cpProv is set.
//  3. team.go uses provisionWorkspaceAuto, not provisionWorkspace
//     directly (source-level guard against the original drift).

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
)

// trackingCPProv records every Start() call in a thread-safe slice.
// Defined locally to avoid coupling this test to the recordingCPProv
// in workspace_provision_concurrent_repro_test.go (whose Stop/etc.
// methods panic — fine there, would be noise here).
type trackingCPProv struct {
	mu       sync.Mutex
	started  []string
	startErr error
}

func (r *trackingCPProv) Start(_ context.Context, cfg provisioner.WorkspaceConfig) (string, error) {
	r.mu.Lock()
	r.started = append(r.started, cfg.WorkspaceID)
	r.mu.Unlock()
	if r.startErr != nil {
		return "", r.startErr
	}
	return "i-stub-" + cfg.WorkspaceID, nil
}
func (r *trackingCPProv) Stop(_ context.Context, _ string) error { return nil }
func (r *trackingCPProv) GetConsoleOutput(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (r *trackingCPProv) IsRunning(_ context.Context, _ string) (bool, error) { return true, nil }

func (r *trackingCPProv) startedSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.started))
	copy(out, r.started)
	return out
}

// TestProvisionWorkspaceAuto_NoBackendReturnsFalse — when neither
// cpProv nor provisioner is wired, the dispatcher returns false so the
// caller knows it must own the persist + mark-failed path. Pre-fix,
// TeamHandler had no equivalent fallback at all and silently dropped
// children on the floor.
func TestProvisionWorkspaceAuto_NoBackendReturnsFalse(t *testing.T) {
	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	// Do NOT call SetCPProvisioner — both backends nil.

	ok := h.provisionWorkspaceAuto("ws-noback", "", nil, models.CreateWorkspacePayload{
		Name: "noback", Tier: 1, Runtime: "claude-code",
	})
	if ok {
		t.Fatalf("expected provisionWorkspaceAuto to return false with no backend wired")
	}
}

// TestProvisionWorkspaceAuto_RoutesToCPWhenSet — when cpProv is set
// (SaaS tenant), Auto MUST route there. CP wins because per-workspace
// EC2 is the SaaS path; Docker would silently fail "no docker socket"
// on the tenant EC2.
//
// This is the regression-prevention test for the Design Director bug
// where 7-of-7 sub-agents went down the Docker path on SaaS.
func TestProvisionWorkspaceAuto_RoutesToCPWhenSet(t *testing.T) {
	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)

	// provisionWorkspaceCP runs in the goroutine and will hit:
	// secrets SELECTs + UPDATE workspace as failed (because we make
	// CP Start return an error to short-circuit the rest of the path).
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := &trackingCPProv{startErr: errors.New("simulated CP rejection")}
	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(rec)

	wsID := "ws-routes-to-cp-0123456789abcdef"
	ok := h.provisionWorkspaceAuto(wsID, "", nil, models.CreateWorkspacePayload{
		Name: "test", Tier: 1, Runtime: "claude-code",
	})
	if !ok {
		t.Fatalf("expected provisionWorkspaceAuto to return true with CP wired")
	}

	// Wait for the goroutine to land in cpProv.Start (or give up).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(rec.startedSnapshot()) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for cpProv.Start; recorded=%v", rec.startedSnapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}

	got := rec.startedSnapshot()
	if len(got) != 1 || got[0] != wsID {
		t.Errorf("expected cpProv.Start invoked once with %q, got %v", wsID, got)
	}
}

// TestTeamExpand_UsesAutoNotDirectDockerPath — source-level guard: if
// a future refactor reintroduces a hardcoded `h.wh.provisionWorkspace`
// call in team.go, this fails. Pre-fix the hardcoded call was the bug.
//
// Substring match on the source rather than AST because the failure
// shape is "wrong function name" — a plain text gate suffices.
// Per `feedback_behavior_based_ast_gates.md` we'd usually pin the
// behavior, but the behavior here ("calls dispatcher, not dispatcher's
// docker leg") is awkward to assert without standing up the entire
// Expand stack — the auto test above covers the dispatcher behavior;
// this test is the cheap source-level seatbelt for the call site.
func TestTeamExpand_UsesAutoNotDirectDockerPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(wd, "team.go"))
	if err != nil {
		t.Fatalf("read team.go: %v", err)
	}
	if bytes.Contains(src, []byte("h.wh.provisionWorkspace(")) {
		t.Errorf("team.go calls h.wh.provisionWorkspace directly — must use h.wh.provisionWorkspaceAuto so SaaS tenants route to CP. " +
			"Pre-2026-05-04 the direct call sent every team child down the Docker path on SaaS, " +
			"creating workspace rows with no EC2 instance.")
	}
	if !bytes.Contains(src, []byte("h.wh.provisionWorkspaceAuto(")) {
		t.Errorf("team.go must call h.wh.provisionWorkspaceAuto for child provisioning — current code does not")
	}
}
