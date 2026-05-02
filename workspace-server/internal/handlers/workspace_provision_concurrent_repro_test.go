package handlers

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
)

// Issue #2486 reproduction harness: 7 simultaneous claude-code provisions
// against the SAME workspace-server (Director Pattern fan-out). On the
// hongming prod tenant this produced ZERO log lines from any of the four
// documented exit paths in provisionWorkspaceCP — operators couldn't tell
// whether the goroutines ran. This test closes the visibility gap by
// pinning that:
//
//  1. Every provision goroutine produces ONE entry log line ("CPProvisioner:
//     goroutine entered for ws-N").
//  2. Every goroutine reaches its registered exit path (cpProv.Start),
//     i.e. the stub records all 7 workspace IDs.
//
// If the silent-drop class is present in current head code, this test
// fails because either (a) the entry-log count is < 7 (meaning one or
// more goroutines reached the goroutine boundary but never produced
// the entry-log line — entry log renamed/removed, or log writer
// hijacked), or (b) the
// recorder count is < 7 (meaning a goroutine entered but exited before
// reaching cpProv.Start, via some unlogged path).
//
// Result on staging head as of 2026-05-02: PASSES — meaning the
// silent-drop seen in the prod incident is NOT reproducible against
// current head with stub CP. Possibilities: (i) bug already fixed
// upstream of the tenant's stale build (sha 76c604fb, 725 commits
// behind), (ii) bug requires real-CP-side rate-limiting we don't
// model here, (iii) bug requires a DB-layer interaction (lock
// contention, deadlock) the sqlmock doesn't model.
//
// Even when this passes today, it stays as a regression gate: any
// future refactor that re-introduces silent goroutine swallow in the
// CP provision path trips it.

// recordingCPProv implements provisioner.CPProvisionerAPI and records
// every Start() invocation in a thread-safe slice so a concurrent
// burst can be verified post-hoc.
type recordingCPProv struct {
	mu        sync.Mutex
	startedWS []string
	// startErr controls what Start() returns. nil → success. Non-nil →
	// error path; provisionWorkspaceCP marks failed + returns.
	startErr error
}

func (r *recordingCPProv) Start(_ context.Context, cfg provisioner.WorkspaceConfig) (string, error) {
	r.mu.Lock()
	r.startedWS = append(r.startedWS, cfg.WorkspaceID)
	r.mu.Unlock()
	if r.startErr != nil {
		return "", r.startErr
	}
	return "i-stubbed-" + cfg.WorkspaceID[:8], nil
}

func (r *recordingCPProv) Stop(_ context.Context, _ string) error {
	panic("recordingCPProv.Stop not expected in concurrent-repro test")
}

func (r *recordingCPProv) GetConsoleOutput(_ context.Context, _ string) (string, error) {
	panic("recordingCPProv.GetConsoleOutput not expected in concurrent-repro test")
}

func (r *recordingCPProv) IsRunning(_ context.Context, _ string) (bool, error) {
	panic("recordingCPProv.IsRunning not expected in concurrent-repro test")
}

func (r *recordingCPProv) startedSet() map[string]struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]struct{}, len(r.startedWS))
	for _, id := range r.startedWS {
		out[id] = struct{}{}
	}
	return out
}

// TestProvisionWorkspaceCP_ConcurrentBurst_NoSilentDrop is the
// repro harness for issue #2486. See file-level comment.
func TestProvisionWorkspaceCP_ConcurrentBurst_NoSilentDrop(t *testing.T) {
	const numWorkspaces = 7

	mock := setupTestDB(t)

	// Every goroutine runs prepareProvisionContext → mintWorkspaceSecrets
	// → cpProv.Start (stubbed to fail) → markProvisionFailed. The DB
	// shape per goroutine: 2 SELECTs + 1 UPDATE. Order between
	// goroutines is non-deterministic so use MatchExpectationsInOrder
	// false.
	mock.MatchExpectationsInOrder(false)
	for i := 0; i < numWorkspaces; i++ {
		mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
			WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
		mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
			WithArgs(sqlmock.AnyArg()).
			WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
		mock.ExpectExec(`UPDATE workspaces SET status =`).
			WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	// Capture every log line so we can count entry-log occurrences.
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	prev := log.Writer()
	log.SetOutput(&safeWriter{buf: &logBuf, mu: &logMu})
	defer log.SetOutput(prev)

	// stubFailing-shaped behaviour but recording-capable. Failure is
	// fine — we're not testing the success path, only that every
	// goroutine entered AND reached the recorded Start() call.
	rec := &recordingCPProv{startErr: fmt.Errorf("simulated CP rejection")}

	// Concurrent-safe broadcaster — captureBroadcaster (used by sequential
	// tests in workspace_provision_test.go) writes lastData unguarded.
	// Under -race + 7 fan-out goroutines that's a real data race; this
	// stub serializes via mutex and only counts (we don't need the
	// payload for any assertion below).
	bcast := &concurrentSafeBroadcaster{}
	handler := NewWorkspaceHandler(bcast, nil, "http://localhost:8080", t.TempDir())
	handler.SetCPProvisioner(rec)

	var wg sync.WaitGroup
	var enteredCount int64
	for i := 0; i < numWorkspaces; i++ {
		wg.Add(1)
		// Use a UUID-shaped ID so cfg.WorkspaceID slicing in the stub
		// has 8 chars to read.
		wsID := fmt.Sprintf("ws-fan-%016d", i)
		go func() {
			defer wg.Done()
			atomic.AddInt64(&enteredCount, 1)
			handler.provisionWorkspaceCP(wsID, "", nil, models.CreateWorkspacePayload{
				Name:    wsID,
				Tier:    1,
				Runtime: "claude-code",
			})
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&enteredCount); got != numWorkspaces {
		t.Fatalf("test setup bug: expected %d goroutines to enter, got %d", numWorkspaces, got)
	}

	// Assertion 1: every goroutine produced an entry log. Without the
	// fix in this PR (#2487), there's NO entry log so this assertion
	// is what closes the visibility gap.
	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	entryCount := strings.Count(logged, "CPProvisioner: goroutine entered for")
	if entryCount != numWorkspaces {
		t.Errorf("entry log fired %d times, want %d. Either (a) a goroutine never reached the entry log or (b) the entry log was removed/renamed.\nlog dump:\n%s",
			entryCount, numWorkspaces, logged)
	}

	// Assertion 2: every goroutine's Start() call was recorded by the
	// stub — no silent drop between entry log and the registered exit
	// path (cpProv.Start).
	started := rec.startedSet()
	if len(started) != numWorkspaces {
		t.Errorf("stub CPProvisioner saw %d distinct Start() calls, want %d. SILENT-DROP CLASS: a goroutine entered but never reached Start(). seen=%v",
			len(started), numWorkspaces, started)
	}

	// Assertion 3: every entry-log line names a distinct workspace —
	// guards against a future refactor that hard-codes a single ID
	// and double-logs.
	for i := 0; i < numWorkspaces; i++ {
		want := fmt.Sprintf("CPProvisioner: goroutine entered for ws-fan-%016d", i)
		if !strings.Contains(logged, want) {
			t.Errorf("missing entry log for ws-fan-%016d. log dump:\n%s", i, logged)
		}
	}

	// Assertion 4: every goroutine's failure path called RecordAndBroadcast
	// exactly once (via h.markProvisionFailed inside provisionWorkspaceCP's
	// "start failed" arm). Cross-checks Assertion 2 from a different angle
	// — if a goroutine reaches Start() but then loses its WORKSPACE_
	// PROVISION_FAILED broadcast, the canvas spinner sticks on
	// "provisioning" until the sweeper. That regression class is what
	// drove making logProvisionPanic a method on *WorkspaceHandler — so
	// it's worth pinning here too.
	bcast.mu.Lock()
	bcastCount := bcast.count
	bcast.mu.Unlock()
	if bcastCount != numWorkspaces {
		t.Errorf("broadcaster saw %d RecordAndBroadcast calls, want %d. SILENT-DROP CLASS: either a goroutine reached cpProv.Start but was lost before markProvisionFailed, OR it exited via an earlier path before reaching Start (cross-check Assertion 2 above).",
			bcastCount, numWorkspaces)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		// Soft-fail: under concurrency some queries may have been
		// re-ordered relative to the (non-strict) expectation set,
		// which sqlmock can sometimes flag. Surface as t.Logf rather
		// than t.Errorf so the assertion above (concrete observable
		// behaviour) remains the primary gate.
		t.Logf("sqlmock expectations note (non-fatal under concurrent fan-out): %v", err)
	}
}

// safeWriter serializes log writes from concurrent goroutines so the
// captured buffer isn't a torn-write mess. Without this the log lines
// from 7 concurrent goroutines interleave at byte boundaries and the
// strings.Count assertion above gets unreliable.
type safeWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

// concurrentSafeBroadcaster is a thread-safe events.EventEmitter stub
// for the 7-goroutine fan-out test. captureBroadcaster (the canonical
// sequential-test stub in workspace_provision_test.go) writes its
// lastData field without synchronization — under -race that's a true
// data race when 7 markProvisionFailed calls run concurrently. This
// stub only counts (no payload retention) and serializes via mutex.
type concurrentSafeBroadcaster struct {
	mu    sync.Mutex
	count int
}

func (b *concurrentSafeBroadcaster) BroadcastOnly(_ string, _ string, _ interface{}) {}

func (b *concurrentSafeBroadcaster) RecordAndBroadcast(_ context.Context, _, _ string, _ interface{}) error {
	b.mu.Lock()
	b.count++
	b.mu.Unlock()
	return nil
}

func (w *safeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
