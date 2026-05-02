package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
)

// fakeEmitter records every RecordAndBroadcast call so tests can assert
// payload shape + emission count. Safe for concurrent use since the sweeper
// itself is single-goroutine but keeping the lock lets the suite fan out.
type fakeEmitter struct {
	mu     sync.Mutex
	events []emittedEvent
	fail   bool
}

type emittedEvent struct {
	Type        string
	WorkspaceID string
	Payload     interface{}
}

func (f *fakeEmitter) RecordAndBroadcast(_ context.Context, eventType string, workspaceID string, payload interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, emittedEvent{eventType, workspaceID, payload})
	if f.fail {
		return errors.New("broadcast boom")
	}
	return nil
}

func (f *fakeEmitter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

// candidateRows builds the new-shape query result (id, runtime, age_sec).
// Use this in every sweep test to match the runtime-aware SELECT.
func candidateRows(rows ...[3]any) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{"id", "runtime", "age_sec"})
	for _, row := range rows {
		r = r.AddRow(row[0], row[1], row[2])
	}
	return r
}

// TestSweepStuckProvisioning_FlipsOverdue verifies the happy path: a stuck
// provisioning workspace gets flipped to failed AND an event is broadcast.
func TestSweepStuckProvisioning_FlipsOverdue(t *testing.T) {
	mock := setupTestDB(t)

	// claude-code workspace, 700s old > 600s default timeout → flipped.
	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), EXTRACT`).
		WillReturnRows(candidateRows([3]any{"ws-stuck", "claude-code", 700}))

	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-stuck", sqlmock.AnyArg(), sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))

	emit := &fakeEmitter{}
	sweepStuckProvisioning(context.Background(), emit, nil)

	if emit.count() != 1 {
		t.Fatalf("expected 1 event, got %d", emit.count())
	}
	if emit.events[0].Type != "WORKSPACE_PROVISION_FAILED" {
		t.Errorf("event type = %q, want WORKSPACE_PROVISION_FAILED", emit.events[0].Type)
	}
	if emit.events[0].WorkspaceID != "ws-stuck" {
		t.Errorf("workspace id = %q, want ws-stuck", emit.events[0].WorkspaceID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSweepStuckProvisioning_HermesGets30MinSlack — the regression that
// motivated the runtime-aware change. A hermes workspace 11 min into
// cold-boot must NOT be flipped to failed; the watcher's 25-min budget
// covers it. Without the fix, the 10-min sweep killed healthy hermes
// boots mid-install (issue #2061's E2E failure on 2026-04-26).
func TestSweepStuckProvisioning_HermesGets30MinSlack(t *testing.T) {
	mock := setupTestDB(t)

	// 11 min = 660 sec. < HermesProvisioningTimeout (1800s).
	// No UPDATE should fire — hermes still has time.
	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), EXTRACT`).
		WillReturnRows(candidateRows([3]any{"ws-hermes-booting", "hermes", 660}))

	emit := &fakeEmitter{}
	sweepStuckProvisioning(context.Background(), emit, nil)

	if emit.count() != 0 {
		t.Fatalf("hermes at 11min should NOT have been flipped, got %d events", emit.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSweepStuckProvisioning_HermesPastDeadline — a hermes workspace
// past 30 min DOES get flipped. Closes the loop on the runtime-aware
// fix: it's still bounded, just with a longer threshold than other
// runtimes.
func TestSweepStuckProvisioning_HermesPastDeadline(t *testing.T) {
	mock := setupTestDB(t)

	// 31 min = 1860 sec > HermesProvisioningTimeout (1800s).
	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), EXTRACT`).
		WillReturnRows(candidateRows([3]any{"ws-hermes-stuck", "hermes", 1860}))
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-hermes-stuck", sqlmock.AnyArg(), sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))

	emit := &fakeEmitter{}
	sweepStuckProvisioning(context.Background(), emit, nil)

	if emit.count() != 1 {
		t.Fatalf("hermes past 30min must be flipped, got %d events", emit.count())
	}
	// Payload should include runtime so ops can distinguish in logs.
	payload, ok := emit.events[0].Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("payload not a map: %T", emit.events[0].Payload)
	}
	if payload["runtime"] != "hermes" {
		t.Errorf("payload.runtime = %v, want hermes", payload["runtime"])
	}
}

// TestSweepStuckProvisioning_ManifestOverrideSparesRow pins the
// integration of the sweeper + RuntimeTimeoutLookup contract introduced
// in #2494. Closes the gap that the unit-test on provisioningTimeoutFor
// alone left open: a future refactor could drop the lookup arg from
// sweepStuckProvisioning's call to provisioningTimeoutFor and only the
// unit test would catch it. This test fails on that refactor too.
//
// Scenario: a claude-code workspace 11 min old (660s). Default budget
// is 10 min (600s) → without manifest override, this would be flipped
// to failed. Manifest override declares 1200s → it should be SPARED.
// No UPDATE, no event emitted.
func TestSweepStuckProvisioning_ManifestOverrideSparesRow(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), EXTRACT`).
		WillReturnRows(candidateRows([3]any{"ws-claude-templated", "claude-code", 660}))

	// No ExpectExec — if the sweeper still flips the row, sqlmock will
	// fail with an unexpected-query error.

	lookup := func(runtime string) int {
		if runtime == "claude-code" {
			return 1200 // manifest override: 20 min
		}
		return 0
	}

	emit := &fakeEmitter{}
	sweepStuckProvisioning(context.Background(), emit, lookup)

	if emit.count() != 0 {
		t.Errorf("manifest-overridden row should NOT have been flipped, got %d events", emit.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSweepStuckProvisioning_ManifestOverrideStillFlipsPastDeadline —
// the symmetric case. Manifest override gives a longer window but a
// row past THAT longer window must still be flipped. Otherwise a
// template that declares an absurd timeout could leave rows wedged
// forever.
func TestSweepStuckProvisioning_ManifestOverrideStillFlipsPastDeadline(t *testing.T) {
	mock := setupTestDB(t)

	// 21 min = 1260s > 1200s manifest override → flipped.
	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), EXTRACT`).
		WillReturnRows(candidateRows([3]any{"ws-claude-truly-stuck", "claude-code", 1260}))
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-claude-truly-stuck", sqlmock.AnyArg(), sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))

	lookup := func(runtime string) int {
		if runtime == "claude-code" {
			return 1200
		}
		return 0
	}

	emit := &fakeEmitter{}
	sweepStuckProvisioning(context.Background(), emit, lookup)

	if emit.count() != 1 {
		t.Fatalf("row past manifest deadline must still be flipped, got %d events", emit.count())
	}
	payload, ok := emit.events[0].Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("payload not a map: %T", emit.events[0].Payload)
	}
	if payload["timeout_secs"] != 1200 {
		t.Errorf("payload.timeout_secs = %v, want 1200 (manifest override applied to event payload)", payload["timeout_secs"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSweepStuckProvisioning_RaceSafe covers the case where UPDATE affects
// 0 rows because the workspace flipped to online (or got restarted) between
// the SELECT and the UPDATE. We should skip the event, not emit a false
// WORKSPACE_PROVISION_FAILED.
func TestSweepStuckProvisioning_RaceSafe(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), EXTRACT`).
		WillReturnRows(candidateRows([3]any{"ws-raced", "claude-code", 700}))

	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-raced", sqlmock.AnyArg(), sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 0)) // 0 rows — raced

	emit := &fakeEmitter{}
	sweepStuckProvisioning(context.Background(), emit, nil)

	if emit.count() != 0 {
		t.Errorf("expected 0 events on race, got %d", emit.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSweepStuckProvisioning_NoStuck verifies that an empty candidate list
// produces no events and no update queries.
func TestSweepStuckProvisioning_NoStuck(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), EXTRACT`).
		WillReturnRows(candidateRows())

	emit := &fakeEmitter{}
	sweepStuckProvisioning(context.Background(), emit, nil)

	if emit.count() != 0 {
		t.Errorf("expected 0 events when nothing stuck, got %d", emit.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSweepStuckProvisioning_MultipleStuck covers the realistic case where
// both agents (claude-code + hermes) are stuck — both should get flipped
// and both should get events. claude-code at 11 min (over its 10-min
// limit), hermes at 31 min (over its 30-min limit).
func TestSweepStuckProvisioning_MultipleStuck(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), EXTRACT`).
		WillReturnRows(candidateRows(
			[3]any{"ws-claude-code", "claude-code", 700},
			[3]any{"ws-hermes", "hermes", 1860},
		))

	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-claude-code", sqlmock.AnyArg(), sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-hermes", sqlmock.AnyArg(), sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))

	emit := &fakeEmitter{}
	sweepStuckProvisioning(context.Background(), emit, nil)

	if emit.count() != 2 {
		t.Fatalf("expected 2 events, got %d", emit.count())
	}
}

// TestSweepStuckProvisioning_BroadcastFailureDoesNotCrash ensures the
// sweeper tolerates a broadcast error (Redis hiccup) — the DB row is
// already flipped so the state stays coherent.
func TestSweepStuckProvisioning_BroadcastFailureDoesNotCrash(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectQuery(`SELECT id, COALESCE\(runtime, ''\), EXTRACT`).
		WillReturnRows(candidateRows([3]any{"ws-stuck", "claude-code", 700}))
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs("ws-stuck", sqlmock.AnyArg(), sqlmock.AnyArg(), models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))

	emit := &fakeEmitter{fail: true}
	// Must not panic.
	sweepStuckProvisioning(context.Background(), emit, nil)
}

// TestProvisioningTimeout_EnvOverride verifies PROVISION_TIMEOUT_SECONDS
// env var takes effect when set to a positive integer, and falls back to
// the per-runtime default otherwise.
func TestProvisioningTimeout_EnvOverride(t *testing.T) {
	t.Setenv("PROVISION_TIMEOUT_SECONDS", "60")
	// When env override is set it wins over runtime defaults.
	if got := provisioningTimeoutFor("", nil); got.Seconds() != 60 {
		t.Errorf("override (no runtime): got %v, want 60s", got)
	}
	if got := provisioningTimeoutFor("hermes", nil); got.Seconds() != 60 {
		t.Errorf("override (hermes): got %v, want 60s", got)
	}
	t.Setenv("PROVISION_TIMEOUT_SECONDS", "")
	if got := provisioningTimeoutFor("", nil); got != DefaultProvisioningTimeout {
		t.Errorf("default (no runtime): got %v, want %v", got, DefaultProvisioningTimeout)
	}
	t.Setenv("PROVISION_TIMEOUT_SECONDS", "not-a-number")
	if got := provisioningTimeoutFor("claude-code", nil); got != DefaultProvisioningTimeout {
		t.Errorf("bad override (claude-code): got %v, want default %v", got, DefaultProvisioningTimeout)
	}
}

// TestProvisioningTimeout_RuntimeAware verifies hermes gets the longer
// HermesProvisioningTimeout while other runtimes keep the default.
// Mirrors bootstrap_watcher.go's bootstrapTimeoutFn — these two
// timeouts must stay in sync (sweep > watcher) or healthy hermes
// boots get killed mid-install.
func TestProvisioningTimeout_RuntimeAware(t *testing.T) {
	cases := []struct {
		runtime string
		want    time.Duration
	}{
		{"hermes", HermesProvisioningTimeout},
		{"langgraph", DefaultProvisioningTimeout},
		{"claude-code", DefaultProvisioningTimeout},
		{"crewai", DefaultProvisioningTimeout},
		{"autogen", DefaultProvisioningTimeout},
		{"", DefaultProvisioningTimeout},
		{"unknown-runtime", DefaultProvisioningTimeout},
	}
	for _, c := range cases {
		if got := provisioningTimeoutFor(c.runtime, nil); got != c.want {
			t.Errorf("runtime=%q: got %v, want %v", c.runtime, got, c.want)
		}
	}
}

// TestProvisioningTimeout_ManifestOverride pins the resolution order
// when a template's config.yaml declared
// `runtime_config.provision_timeout_seconds`. Without this gate, the
// sweeper kept the hardcoded 10-min floor regardless of manifest —
// which is the original wiring gap that drove false-positive timeouts
// on cold-pull claude-code bursts.
//
// Order pinned:
//
//   1. PROVISION_TIMEOUT_SECONDS env beats everything (ops debug).
//   2. Manifest lookup beats hermes special-case + default.
//   3. Hermes default applies when lookup returns 0 for hermes.
//   4. DefaultProvisioningTimeout applies when lookup returns 0 for
//      anything else.
//   5. Lookup returning 0 for ANY runtime is "no override" — never
//      a 0-second timeout (which would kill every workspace instantly).
func TestProvisioningTimeout_ManifestOverride(t *testing.T) {
	manifest := map[string]int{
		"claude-code": 900, // 15 min — what an ops manifest bump would set
		"langgraph":   1200,
		"hermes":      2400, // 40 min — manifest can override hermes default too
	}
	lookup := func(runtime string) int { return manifest[runtime] }

	cases := []struct {
		name    string
		runtime string
		want    time.Duration
	}{
		{"manifest override beats default for claude-code", "claude-code", 900 * time.Second},
		{"manifest override applied for langgraph", "langgraph", 1200 * time.Second},
		{"manifest override beats hermes default", "hermes", 2400 * time.Second},
		{"unknown runtime + no manifest entry → default", "unknown-runtime", DefaultProvisioningTimeout},
		{"empty runtime + no manifest entry → default", "", DefaultProvisioningTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := provisioningTimeoutFor(c.runtime, lookup); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}

	// Env override beats manifest — ops debug must be the top priority.
	t.Setenv("PROVISION_TIMEOUT_SECONDS", "60")
	if got := provisioningTimeoutFor("claude-code", lookup); got.Seconds() != 60 {
		t.Errorf("env-override should beat manifest: got %v, want 60s", got)
	}
	t.Setenv("PROVISION_TIMEOUT_SECONDS", "")

	// Lookup returning 0 means "no entry" — must NOT result in a
	// 0-second timeout. Falls through to runtime defaults.
	zeroLookup := func(_ string) int { return 0 }
	if got := provisioningTimeoutFor("claude-code", zeroLookup); got != DefaultProvisioningTimeout {
		t.Errorf("zero-from-lookup should fall through to default, got %v", got)
	}
	if got := provisioningTimeoutFor("hermes", zeroLookup); got != HermesProvisioningTimeout {
		t.Errorf("zero-from-lookup should fall through to hermes default, got %v", got)
	}
}
