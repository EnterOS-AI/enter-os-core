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
	stopped  []string
	startErr error
	stopErr  error
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
func (r *trackingCPProv) Stop(_ context.Context, workspaceID string) error {
	r.mu.Lock()
	r.stopped = append(r.stopped, workspaceID)
	r.mu.Unlock()
	return r.stopErr
}
func (r *trackingCPProv) GetConsoleOutput(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (r *trackingCPProv) IsRunning(_ context.Context, _ string) (bool, error) { return true, nil }

func (r *trackingCPProv) stoppedSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.stopped))
	copy(out, r.stopped)
	return out
}

func (r *trackingCPProv) startedSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.started))
	copy(out, r.started)
	return out
}

// TestProvisionWorkspaceAuto_NoBackendMarksFailed — when neither cpProv
// nor provisioner is wired, the dispatcher must:
//  1. Return false (so the caller can do its own extra cleanup if
//     needed — Create persists workspace_config for the Config tab).
//  2. Mark the workspace failed via markProvisionFailed (defense in
//     depth: if a future caller bypasses the bool return, the workspace
//     still doesn't sit stuck in 'provisioning' for 10 min until the
//     sweeper fires).
//
// Pre-2026-05-05 the false return was silent and TeamHandler /
// OrgHandler.createWorkspaceTree dropped workspaces on the floor when
// they ignored it. This test pins the new contract that Auto owns the
// failed-mark on no-backend.
func TestProvisionWorkspaceAuto_NoBackendMarksFailed(t *testing.T) {
	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	// markProvisionFailed does a single UPDATE workspaces ... SET status='failed'.
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	// Do NOT call SetCPProvisioner — both backends nil.

	ok := h.provisionWorkspaceAuto("ws-noback", "", nil, models.CreateWorkspacePayload{
		Name: "noback", Tier: 1, Runtime: "claude-code",
	})
	if ok {
		t.Fatalf("expected provisionWorkspaceAuto to return false with no backend wired")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected markProvisionFailed UPDATE to fire on no-backend path: %v", err)
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

// TestNoCallSiteCallsDirectProvisionerExceptAuto — generic source-level
// gate covering ANY future caller, not just team.go and org_import.go.
//
// The architectural intent is: provisionWorkspaceAuto is the single
// source of truth for "how to start a workspace"; the per-backend
// helpers (provisionWorkspace = Docker, provisionWorkspaceCP = CP) are
// implementation details Auto routes between based on which backend is
// wired. Pre-2026-05-04 we had this abstraction but enforced only by
// convention — TeamHandler.Expand violated it (silent SaaS bug), then
// org_import.go violated it the same way. The fixes were identical:
// route through Auto. This gate prevents the *next* call site from
// repeating the pattern.
//
// Walks every .go file under handlers/ (except the dispatcher itself
// in workspace.go, and tests). Fails if any non-test handler calls
// h.*.provisionWorkspace( or h.*.provisionWorkspaceCP( directly —
// they should ALL go through provisionWorkspaceAuto.
func TestNoCallSiteCallsDirectProvisionerExceptAuto(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	directRe := []string{
		// Receiver could be anything, so match on the suffix.
		".provisionWorkspace(",
		".provisionWorkspaceCP(",
	}
	allowedFiles := map[string]bool{
		// workspace.go DEFINES the methods + the Auto dispatcher; it's
		// allowed to reference them directly.
		"workspace.go": true,
		// workspace_provision.go DEFINES the bodies of the direct
		// methods (and the Auto-internal call from CP-mode itself).
		"workspace_provision.go": true,
		// workspace_restart.go pre-dates the Auto dispatcher and has
		// its own if-cpProv-else manual dispatch (line 219-228, 571-575,
		// 704-708). Functionally equivalent to Auto, so it's not the
		// bug class this gate targets — but it IS architectural
		// duplication, tracked as a follow-up for proper de-dup.
		// See <follow-up issue> filed alongside this PR.
		"workspace_restart.go": true,
	}
	for _, entry := range entries {
		name := entry.Name()
		if !filepath.IsAbs(name) && entry.IsDir() {
			continue
		}
		if filepath.Ext(name) != ".go" {
			continue
		}
		// Skip tests — tests legitimately stub or call the helpers
		// to exercise their behavior.
		if filepath.Base(name) != name {
			continue
		}
		if filepath.Ext(name) == ".go" && len(name) > len("_test.go") &&
			name[len(name)-len("_test.go"):] == "_test.go" {
			continue
		}
		if allowedFiles[name] {
			continue
		}
		src, err := os.ReadFile(filepath.Join(wd, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, needle := range directRe {
			if bytes.Contains(src, []byte(needle)) {
				t.Errorf("%s calls h.X%s directly — must use h.X.provisionWorkspaceAuto so backend routing stays centralized. "+
					"Pre-2026-05-04 the same pattern caused the silent-drop bug in TeamHandler.Expand, then again in org_import.go (#2486). "+
					"Fix: replace the call with h.X.provisionWorkspaceAuto(...) — Auto picks Docker vs CP based on which backend is wired.",
					name, needle)
			}
		}
	}
}

// TestOrgImport_UsesAutoNotDirectDockerPath — source-level guard for
// the org_import.go call site. Same bug pattern as team.go above:
// pre-2026-05-04 #2 (this PR), org_import called h.workspace.provisionWorkspace
// directly, sending every imported workspace down the Docker path on
// SaaS. User reproduced 2026-05-04 ~22:30Z importing a 7-workspace
// "Director Pattern" template on the hongming prod tenant — every
// workspace sat in "provisioning" until the 600s sweeper marked it
// failed with "container started but never called /registry/register",
// because no container ever existed (the Docker provisioner was nil
// in SaaS, the goroutine returned silently, no log emitted from
// provisionWorkspaceCP because that function was never invoked).
//
// The repro pattern was identical to issue #2486. The fix is identical
// to the team.go fix above: route through provisionWorkspaceAuto.
//
// This test pins the call site so a future refactor can't re-introduce
// the bug. Substring match on the source — same rationale as the team.go
// gate above.
func TestOrgImport_UsesAutoNotDirectDockerPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(wd, "org_import.go"))
	if err != nil {
		t.Fatalf("read org_import.go: %v", err)
	}
	if bytes.Contains(src, []byte("h.workspace.provisionWorkspace(")) {
		t.Errorf("org_import.go calls h.workspace.provisionWorkspace directly — must use h.workspace.provisionWorkspaceAuto so SaaS tenants route to CP. " +
			"Pre-fix repro: 7-workspace org-import on hongming prod tenant 2026-05-04 ~22:30Z, every workspace timed out at 600s with the misleading 'container started but never called /registry/register' message — see #2486.")
	}
	if !bytes.Contains(src, []byte("h.workspace.provisionWorkspaceAuto(")) {
		t.Errorf("org_import.go must call h.workspace.provisionWorkspaceAuto for child provisioning — current code does not")
	}
}

// TestHasProvisioner_TrueOnCPOnly — SaaS tenants run with cpProv set and
// the local Docker provisioner nil. HasProvisioner must report true so
// gate-y callers (org-import prep block) don't skip provisioning.
//
// Pre-2026-05-05 the org-import gate checked `h.provisioner != nil`
// directly — false on SaaS — and the entire provisioning prep block was
// skipped. The Auto call inside the block was unreachable; PR #2798's
// "route through Auto" fix didn't help because the gate fired earlier.
// Symptom: 7-workspace org-import on hongming sat in 'provisioning' for
// the full 10-minute sweep window.
func TestHasProvisioner_TrueOnCPOnly(t *testing.T) {
	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(&trackingCPProv{})
	if !h.HasProvisioner() {
		t.Errorf("HasProvisioner() == false with cpProv wired (Docker nil) — every gate that uses this would skip provisioning on SaaS, reproducing the hongming 7-workspace stuck-in-provisioning incident from 2026-05-05")
	}
}

// TestHasProvisioner_TrueOnDockerOnly — self-hosted operators run with
// the local Docker provisioner wired and cpProv nil. HasProvisioner must
// report true.
func TestHasProvisioner_TrueOnDockerOnly(t *testing.T) {
	bcast := &concurrentSafeBroadcaster{}
	// NewWorkspaceHandler guards the typed-nil-interface trap (workspace.go
	// docstring) — pass a real *Provisioner stub via the test fixture
	// rather than a nil pointer cast to the interface.
	h := NewWorkspaceHandler(bcast, &provisioner.Provisioner{}, "http://localhost:8080", t.TempDir())
	if !h.HasProvisioner() {
		t.Errorf("HasProvisioner() == false with Docker wired (cpProv nil) — would break self-hosted operators")
	}
}

// TestHasProvisioner_FalseWhenNeitherWired — misconfigured deployment
// with neither backend reachable. HasProvisioner must report false so
// the org-import prep block is skipped (no point doing template/secret
// prep work when nothing can run the resulting container).
func TestHasProvisioner_FalseWhenNeitherWired(t *testing.T) {
	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	if h.HasProvisioner() {
		t.Errorf("HasProvisioner() == true with no backend wired — gate should short-circuit and not waste prep cycles")
	}
}

// TestNoBareBothNilCheck — source-level pin: any code that wants to ask
// "is no backend wired?" must use !HasProvisioner(), not the verbose
// `h.provisioner == nil && h.cpProv == nil` shape. Two reasons:
//
//  1. Single source of truth — when a third backend lands (k8s,
//     containerd, whatever), HasProvisioner gets the new field added in
//     one place. Bare both-nil checks each need to be hunted down.
//  2. Symmetry — easier to read `!h.HasProvisioner()` and know the
//     intent than to mentally evaluate `nil && nil`.
//
// Allowed exception: workspace.go's HasProvisioner() definition itself.
// Test files are also exempt — assertions on internal field state are
// fine.
func TestNoBareBothNilCheck(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	bareShapes := []string{
		"h.provisioner == nil && h.cpProv == nil",
		"h.cpProv == nil && h.provisioner == nil",
	}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".go" {
			continue
		}
		// Allow tests (legitimate field-state assertions).
		if len(name) > len("_test.go") &&
			name[len(name)-len("_test.go"):] == "_test.go" {
			continue
		}
		// workspace.go houses HasProvisioner's definition + can reference
		// the fields directly — but with the !HasProvisioner() refactor
		// it shouldn't contain the bare both-nil shape any more.
		src, err := os.ReadFile(filepath.Join(wd, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, needle := range bareShapes {
			if bytes.Contains(src, []byte(needle)) {
				t.Errorf("%s contains bare `%s` — must use `!h.HasProvisioner()` for SSOT.", name, needle)
			}
		}
	}
}

// TestOrgImportGate_UsesHasProvisionerNotBareField — source-level pin
// for the org-import gate. Pre-fix the gate read `h.provisioner != nil`,
// which checked only the Docker pointer and silently dropped every
// SaaS workspace. The fix routes through HasProvisioner so both
// backends count.
//
// Substring match because the failure shape is "wrong field" — a plain
// text gate suffices, same rationale as TestTeamExpand_UsesAutoNotDirectDockerPath
// above.
func TestOrgImportGate_UsesHasProvisionerNotBareField(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(wd, "org_import.go"))
	if err != nil {
		t.Fatalf("read org_import.go: %v", err)
	}
	// The provisioning gate is the `else if ...` clause that follows the
	// `if ws.External {` external-workspace branch. If org_import.go
	// reintroduces a bare `h.provisioner` check there, every SaaS tenant
	// silently drops org-imported workspaces again. Auto's nil check is
	// the right routing layer; the gate just decides whether to do prep
	// work at all, and HasProvisioner is the symmetric question.
	if bytes.Contains(src, []byte("} else if h.provisioner != nil {")) {
		t.Errorf("org_import.go gates the provisioning prep block on `h.provisioner != nil` (bare Docker check) — must use `h.workspace.HasProvisioner()` so SaaS tenants (cpProv set, provisioner nil) reach the Auto call. " +
			"Repro: 2026-05-05 hongming org-import incident — 7 claude-code workspaces stuck in 'provisioning' for 10 min because the gate skipped the entire block on SaaS, hiding the Auto call PR #2798 introduced.")
	}
	if !bytes.Contains(src, []byte("h.workspace.HasProvisioner()")) {
		t.Errorf("org_import.go must call h.workspace.HasProvisioner() in the provisioning gate — current code does not")
	}
}

// TestStopWorkspaceAuto_RoutesToCPWhenSet — symmetric with the
// provision dispatcher test above. SaaS tenants run with cpProv set
// and the local Docker provisioner nil; Auto must route Stop to CP
// (= terminate the EC2). Pre-2026-05-05 the absence of this dispatcher
// meant team-collapse + workspace-delete called h.provisioner.Stop
// directly, no-oping on every SaaS tenant — issue #2813 (collapse) and
// #2814 (delete) both leak EC2s for ~6 months.
func TestStopWorkspaceAuto_RoutesToCPWhenSet(t *testing.T) {
	rec := &trackingCPProv{}
	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(rec)

	wsID := "ws-stop-routes-cp"
	if err := h.StopWorkspaceAuto(context.Background(), wsID); err != nil {
		t.Fatalf("StopWorkspaceAuto returned err with CP wired: %v", err)
	}
	got := rec.stoppedSnapshot()
	if len(got) != 1 || got[0] != wsID {
		t.Errorf("expected cpProv.Stop invoked once with %q, got %v", wsID, got)
	}
}

// TestStopWorkspaceAuto_RoutesToDockerWhenOnlyDocker — self-hosted
// operators run with the local Docker provisioner wired and cpProv nil.
// Auto must route to Docker.
//
// Stub-injects a LocalProvisionerAPI via a private constructor pattern
// so we don't need a real Docker daemon. NewWorkspaceHandler's
// constructor takes *provisioner.Provisioner (concrete) so we set the
// interface field directly.
func TestStopWorkspaceAuto_RoutesToDockerWhenOnlyDocker(t *testing.T) {
	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	stub := &stoppingLocalProv{}
	h.provisioner = stub

	wsID := "ws-stop-routes-docker"
	if err := h.StopWorkspaceAuto(context.Background(), wsID); err != nil {
		t.Fatalf("StopWorkspaceAuto returned err with Docker wired: %v", err)
	}
	if len(stub.stopped) != 1 || stub.stopped[0] != wsID {
		t.Errorf("expected Docker provisioner.Stop invoked once with %q, got %v", wsID, stub.stopped)
	}
}

// TestStopWorkspaceAuto_NoBackendIsNoOp — when neither backend is wired
// (misconfigured deployment, or test fixture), StopWorkspaceAuto returns
// nil silently. Distinct from provisionWorkspaceAuto's mark-failed
// behavior: there's no row state to mark "failed to stop" against, and
// the absence of a backend means nothing was running to stop.
func TestStopWorkspaceAuto_NoBackendIsNoOp(t *testing.T) {
	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	// Neither SetCPProvisioner nor a Docker provisioner — both nil.

	if err := h.StopWorkspaceAuto(context.Background(), "ws-noback"); err != nil {
		t.Errorf("expected nil error on no-backend stop, got %v", err)
	}
}

// stoppingLocalProv is a minimal LocalProvisionerAPI stub that records
// Stop invocations. Other methods panic — guards against accidental
// use by tests that should be using a different stub.
type stoppingLocalProv struct {
	stopped []string
}

func (s *stoppingLocalProv) Stop(_ context.Context, workspaceID string) error {
	s.stopped = append(s.stopped, workspaceID)
	return nil
}
func (s *stoppingLocalProv) Start(_ context.Context, _ provisioner.WorkspaceConfig) (string, error) {
	panic("stoppingLocalProv: Start not implemented for this test")
}
func (s *stoppingLocalProv) IsRunning(_ context.Context, _ string) (bool, error) {
	panic("stoppingLocalProv: IsRunning not implemented for this test")
}
func (s *stoppingLocalProv) ExecRead(_ context.Context, _, _ string) ([]byte, error) {
	panic("stoppingLocalProv: ExecRead not implemented for this test")
}
func (s *stoppingLocalProv) RemoveVolume(_ context.Context, _ string) error {
	panic("stoppingLocalProv: RemoveVolume not implemented for this test")
}
func (s *stoppingLocalProv) VolumeHasFile(_ context.Context, _, _ string) (bool, error) {
	panic("stoppingLocalProv: VolumeHasFile not implemented for this test")
}
func (s *stoppingLocalProv) WriteAuthTokenToVolume(_ context.Context, _, _ string) error {
	panic("stoppingLocalProv: WriteAuthTokenToVolume not implemented for this test")
}

// TestNoCallSiteCallsBareStop — source-level pin against the bug
// pattern that motivated this PR. Any non-test handler that wants to
// "stop the workload" must go through h.X.StopWorkspaceAuto, not bare
// h.X.provisioner.Stop / h.X.cpProv.Stop / h.X.Stop. Pre-2026-05-05
// team.go and workspace_crud.go both called h.provisioner.Stop directly
// inside `if h.provisioner != nil { ... }` gates — silent no-op on
// SaaS, EC2 leak (#2813, #2814).
//
// Allowed exceptions:
//   - workspace.go: defines StopWorkspaceAuto (the dispatcher itself).
//   - workspace_provision.go: defines per-backend Start/Stop bodies.
//   - workspace_restart.go: pre-dates the dispatchers and uses manual
//     if-cpProv-else dispatch with retry semantics tuned for the
//     restart hot path. Functionally equivalent + wraps cpStopWithRetry,
//     so it's not the bug class this gate targets — but it IS
//     architectural duplication, tracked under #2799.
//   - container_files.go: drives Docker daemon directly for file-copy
//     short-lived containers; no workspace-level Stop semantics.
func TestNoCallSiteCallsBareStop(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	bareShapes := []string{
		".provisioner.Stop(",
		".cpProv.Stop(",
	}
	allowedFiles := map[string]bool{
		"workspace.go":           true,
		"workspace_provision.go": true,
		"workspace_restart.go":   true,
		"container_files.go":     true,
	}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".go" {
			continue
		}
		if len(name) > len("_test.go") &&
			name[len(name)-len("_test.go"):] == "_test.go" {
			continue
		}
		if allowedFiles[name] {
			continue
		}
		src, err := os.ReadFile(filepath.Join(wd, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// Strip line + block comments before substring check — the gate
		// targets call expressions in real code, not historical
		// references in documentation/comments. Without this, comments
		// describing the old buggy shape (kept on purpose for
		// archaeology) trip the test.
		stripped := stripGoComments(src)
		for _, needle := range bareShapes {
			if bytes.Contains(stripped, []byte(needle)) {
				t.Errorf("%s contains bare `%s` — must go through h.X.StopWorkspaceAuto so SaaS tenants route to CP. "+
					"Pre-2026-05-05 team.go and workspace_crud.go did this and silently leaked EC2s on every SaaS collapse / delete (#2813, #2814).", name, needle)
			}
		}
	}
}

// TestRestartWorkspaceAuto_RoutesToCPWhenSet — third dispatcher, same
// drift-class shape as the other two. SaaS path goes through CP with
// retry semantics. The cpStopWithRetry retry loop fires before
// provision spawns; this test asserts cpProv.Stop was invoked at
// least once with the workspace ID (we can't assert exact retry
// count without mocking out the retry helper itself, which would
// invert the test contract — the retry IS the dispatcher's job here).
func TestRestartWorkspaceAuto_RoutesToCPWhenSet(t *testing.T) {
	rec := &trackingCPProv{}
	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(rec)

	// Mock DB so cpStopWithRetry can run without a real Postgres.
	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	// provisionWorkspaceCP runs in the goroutine and will hit secrets
	// SELECTs + UPDATE workspace as failed (we make CP Start return
	// an error to short-circuit the post-Start path).
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec.startErr = errors.New("simulated CP rejection")

	wsID := "ws-restart-routes-cp-0123456789ab"
	ok := h.RestartWorkspaceAuto(context.Background(), wsID, "", nil, models.CreateWorkspacePayload{
		Name: "restart-test", Tier: 1, Runtime: "claude-code",
	})
	if !ok {
		t.Fatalf("expected RestartWorkspaceAuto to return true with CP wired")
	}

	// Wait for the goroutine to land. cpStopWithRetry runs synchronously
	// before the provision goroutine fires; both call sites record into
	// the tracking stub, so we expect at least one Stop and (eventually)
	// at least one Start.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(rec.stoppedSnapshot()) > 0 && len(rec.startedSnapshot()) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for cpProv.Stop + cpProv.Start; stopped=%v started=%v",
				rec.stoppedSnapshot(), rec.startedSnapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}

	stopped := rec.stoppedSnapshot()
	if len(stopped) == 0 || stopped[0] != wsID {
		t.Errorf("expected cpProv.Stop invoked with %q, got %v", wsID, stopped)
	}
	started := rec.startedSnapshot()
	if len(started) == 0 || started[0] != wsID {
		t.Errorf("expected cpProv.Start invoked with %q, got %v", wsID, started)
	}
}

// TestRestartWorkspaceAuto_RoutesToDockerWhenOnlyDocker — self-hosted
// path. Docker provisioner.Stop has no retry; this test only asserts
// the dispatch order (Stop → spawn provision goroutine) without
// stubbing the entire Docker provision pipeline.
//
// The spawned provision goroutine WILL panic in provisionWorkspaceOpts
// (no real Docker daemon), be recovered by logProvisionPanic, and
// attempt a markProvisionFailed UPDATE on the test DB. We pre-register
// that expectation so the panic-recovery doesn't fail the test as a
// "was not expected" call. We also wait for the goroutine to land
// before the test body exits, so its db.DB writes don't leak into the
// next test's sqlmock when tests run sequentially in the same package.
func TestRestartWorkspaceAuto_RoutesToDockerWhenOnlyDocker(t *testing.T) {
	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	// Allow up to 5 markProvisionFailed UPDATEs from the panic-recovered
	// goroutine (it'll panic in provisionWorkspaceOpts since
	// stoppingLocalProv.Start panics, then logProvisionPanic calls
	// markProvisionFailed). Generous count so a slower CI runner
	// doesn't trip on duplicate writes; we don't assert
	// ExpectationsWereMet since the count is a runtime detail.
	for i := 0; i < 5; i++ {
		mock.ExpectExec(`UPDATE workspaces SET status =`).
			WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	stub := &stoppingLocalProv{}
	h.provisioner = stub

	wsID := "ws-restart-routes-docker"
	ok := h.RestartWorkspaceAuto(context.Background(), wsID, "", nil, models.CreateWorkspacePayload{
		Name: "restart-test", Tier: 1, Runtime: "claude-code",
	})
	if !ok {
		t.Fatalf("expected RestartWorkspaceAuto to return true with Docker wired")
	}

	// Wait for the spawned goroutine to settle — it'll panic in
	// provisionWorkspaceOpts (stoppingLocalProv.Start panics) and be
	// recovered by logProvisionPanic. Without this wait, the goroutine
	// outlives the test and writes to a sqlmock that the NEXT test
	// owns, causing a `was not expected` race.
	time.Sleep(200 * time.Millisecond)

	// Stop call is synchronous on the Docker leg.
	if len(stub.stopped) == 0 || stub.stopped[0] != wsID {
		t.Errorf("expected provisioner.Stop invoked with %q, got %v", wsID, stub.stopped)
	}
}

// TestRestartWorkspaceAuto_NoBackendMarksFailed — when neither backend
// is wired, the dispatcher returns false AND marks the workspace
// failed (defense in depth, mirroring provisionWorkspaceAuto). Distinct
// from StopWorkspaceAuto's no-op-on-no-backend contract: Restart's
// promise is "the workspace will be alive again" — failing silently
// would strand the user with a stuck workspace and no error path.
func TestRestartWorkspaceAuto_NoBackendMarksFailed(t *testing.T) {
	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	bcast := &concurrentSafeBroadcaster{}
	h := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	// Neither SetCPProvisioner nor a Docker provisioner — both nil.

	ok := h.RestartWorkspaceAuto(context.Background(), "ws-restart-noback", "", nil, models.CreateWorkspacePayload{
		Name: "restart-test", Tier: 1, Runtime: "claude-code",
	})
	if ok {
		t.Fatalf("expected RestartWorkspaceAuto to return false with no backend wired")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected markProvisionFailed UPDATE to fire on no-backend path: %v", err)
	}
}

// TestRestartHandler_UsesRestartWorkspaceAuto — source-level pin that
// the Restart HTTP handler routes through the dispatcher. Phase 2 PR-A
// of #2799 migrates Site 1+2 (the Restart goroutine) to call
// RestartWorkspaceAutoOpts. This test pins the migration so the next
// refactor doesn't accidentally regress to the inline if-cpProv-else
// dispatch — that pre-fix shape had Docker-FIRST ordering, a different
// drift class from the silent-drop bugs PRs #2811/#2824 closed.
//
// Allowed in workspace_restart.go: stopForRestart (Site 4), Pause
// (Site 5). Both are tracked under #2799 Phase 2 PR-B / Phase 3.
func TestRestartHandler_UsesRestartWorkspaceAuto(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(wd, "workspace_restart.go"))
	if err != nil {
		t.Fatalf("read workspace_restart.go: %v", err)
	}
	stripped := stripGoComments(src)
	// The Restart handler must dispatch through the SoT helper. Either
	// signature variant satisfies the gate.
	if !bytes.Contains(stripped, []byte("h.RestartWorkspaceAutoOpts(")) &&
		!bytes.Contains(stripped, []byte("h.RestartWorkspaceAuto(")) {
		t.Errorf("workspace_restart.go must call RestartWorkspaceAuto[Opts] from the Restart handler — current code does not. " +
			"Phase 2 of #2799 migrated this site; do not regress to the inline if-cpProv-else dispatch.")
	}
}

// TestResumeHandler_UsesProvisionWorkspaceAuto — source-level pin that
// the Resume HTTP handler routes through the dispatcher. Phase 2 PR-A
// of #2799 migrates Site 3 (Resume goroutine) to call
// provisionWorkspaceAuto (Resume is provision-only — the workspace is
// already paused/stopped, no Stop step needed).
func TestResumeHandler_UsesProvisionWorkspaceAuto(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(wd, "workspace_restart.go"))
	if err != nil {
		t.Fatalf("read workspace_restart.go: %v", err)
	}
	stripped := stripGoComments(src)
	// The Resume handler's loop must dispatch through provisionWorkspaceAuto.
	// Doesn't need a uniqueness check — the file already calls it from at
	// least the Resume site (a regression that removes only the Resume call
	// would still match this needle from another call in the file, but the
	// stripGoComments output of workspace_restart.go is small enough that
	// inspecting the diff catches that.)
	if !bytes.Contains(stripped, []byte("h.provisionWorkspaceAuto(ws.id")) {
		t.Errorf("workspace_restart.go must call provisionWorkspaceAuto from the Resume handler with `ws.id` — current code does not. " +
			"Phase 2 of #2799 migrated this site; do not regress to the inline if-cpProv-else dispatch.")
	}
}

// stripGoComments removes // line comments and /* */ block comments
// from Go source. Imperfect (doesn't handle comments-inside-strings)
// but adequate for the source-level pin tests in this file — none of
// our gated needles legitimately appear inside string literals in the
// handlers package.
func stripGoComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); i++ {
		// Block comment
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++ // skip closing /
			continue
		}
		// Line comment — preserve the newline so line counts stay sane
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '/' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				out = append(out, '\n')
			}
			continue
		}
		out = append(out, src[i])
	}
	return out
}
