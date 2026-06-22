package registry

import (
	"context"
	"sync"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
)

// rescueHookRecorder captures the args of every BootFailureRescueHook
// invocation so tests can assert the rescue capture fires exactly on the
// boot-failure verdict — and never on a healthy/raced row.
type rescueHookRecorder struct {
	mu    sync.Mutex
	calls [][3]string // {workspaceID, instanceID, reason}
}

func (r *rescueHookRecorder) hook() func(workspaceID, instanceID, reason string) {
	return func(workspaceID, instanceID, reason string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, [3]string{workspaceID, instanceID, reason})
	}
}

func (r *rescueHookRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// withRescueHook installs a recorder as the package-level
// BootFailureRescueHook for the test's duration.
func withRescueHook(t *testing.T) *rescueHookRecorder {
	t.Helper()
	rec := &rescueHookRecorder{}
	prev := BootFailureRescueHook
	BootFailureRescueHook = rec.hook()
	t.Cleanup(func() { BootFailureRescueHook = prev })
	return rec
}

// TestSweep_RescueFiresOnBootFailureVerdict — the core RFC internal#742
// assertion: when the sweep flips a stuck workspace to `failed`, the
// rescue hook fires once with the workspace + instance id and the
// provision_timeout_sweep reason, BEFORE teardown.
func TestSweep_RescueFiresOnBootFailureVerdict(t *testing.T) {
	mock := setupTestDB(t)
	rec := withRescueHook(t)

	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), COALESCE\(instance_id, ''\), EXTRACT`).
		WillReturnRows(candidateRows([4]any{"ws-stuck", "codex", "i-0badf00d", 800}))
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-stuck", sqlmock.AnyArg(), sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))

	sweepStuckProvisioning(context.Background(), &fakeEmitter{}, nil)

	if rec.count() != 1 {
		t.Fatalf("rescue hook should fire once on a boot-failure flip, got %d", rec.count())
	}
	got := rec.calls[0]
	if got[0] != "ws-stuck" || got[1] != "i-0badf00d" || got[2] != "provision_timeout_sweep" {
		t.Errorf("rescue hook args = %v, want {ws-stuck i-0badf00d provision_timeout_sweep}", got)
	}
}

// TestSweep_RescueDoesNotFireOnRace — affected==0 means the row raced to
// online/restart between SELECT and UPDATE. That is NOT a boot-failure
// verdict, so the rescue capture must NOT fire (we'd be snapshotting a
// healthy box that's about to come online).
func TestSweep_RescueDoesNotFireOnRace(t *testing.T) {
	mock := setupTestDB(t)
	rec := withRescueHook(t)

	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), COALESCE\(instance_id, ''\), EXTRACT`).
		WillReturnRows(candidateRows([4]any{"ws-raced", "codex", "i-raced", 800}))
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-raced", sqlmock.AnyArg(), sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 0)) // raced — 0 rows

	sweepStuckProvisioning(context.Background(), &fakeEmitter{}, nil)

	if rec.count() != 0 {
		t.Errorf("rescue hook must NOT fire on a raced flip (affected==0), got %d calls", rec.count())
	}
}

// TestSweep_RescueDoesNotFireOnHealthyRow — a not-yet-overdue row is
// never flipped, so the rescue capture must not fire. Guards against the
// hook being attached above the age gate.
func TestSweep_RescueDoesNotFireOnHealthyRow(t *testing.T) {
	mock := setupTestDB(t)
	rec := withRescueHook(t)

	// hermes at 11 min (660s) < 30 min hermes budget → not overdue, no flip.
	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), COALESCE\(instance_id, ''\), EXTRACT`).
		WillReturnRows(candidateRows([4]any{"ws-healthy", "hermes", "i-healthy", 660}))

	sweepStuckProvisioning(context.Background(), &fakeEmitter{}, nil)

	if rec.count() != 0 {
		t.Errorf("rescue hook must NOT fire on a non-overdue (healthy) row, got %d calls", rec.count())
	}
}

// TestSweep_RescueNilHookIsSafe — on a deploy where the hook is unwired
// (self-hosted / no rescue shipping), the sweep must still flip + emit
// without panicking on the nil hook.
func TestSweep_RescueNilHookIsSafe(t *testing.T) {
	mock := setupTestDB(t)
	prev := BootFailureRescueHook
	BootFailureRescueHook = nil
	t.Cleanup(func() { BootFailureRescueHook = prev })

	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), COALESCE\(instance_id, ''\), EXTRACT`).
		WillReturnRows(candidateRows([4]any{"ws-stuck", "codex", "i-x", 800}))
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-stuck", sqlmock.AnyArg(), sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))

	emit := &fakeEmitter{}
	sweepStuckProvisioning(context.Background(), emit, nil) // must not panic

	if emit.count() != 1 {
		t.Errorf("flip+emit must still happen with a nil rescue hook, got %d events", emit.count())
	}
}
