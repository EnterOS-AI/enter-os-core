package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectStaleTokenSweepNoOp registers the third-pass query
// (sweepStaleTokensWithoutContainer) returning zero rows. The third
// pass runs unconditionally on every sweepOnce, so every test that
// doesn't specifically exercise stale-token revocation must register
// this expectation or sqlmock will fail "unexpected query".
//
// Centralising the regex here keeps the existing test suite readable —
// individual tests don't have to spell out a query they're not actually
// asserting against.
//
// The regex is anchored at the start of the query AND requires both the
// status-filter (R3 from the review) and the runtime-filter (2026-05-03
// fix for external workspaces being incorrectly swept), to keep us from
// accidentally matching a future query that opens with the same column
// name OR a regression that drops one of the load-bearing predicates.
func expectStaleTokenSweepNoOp(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`(?s)^\s*SELECT DISTINCT t\.workspace_id::text\s+FROM workspace_auth_tokens.*status NOT IN \('removed', 'provisioning'\).*runtime != 'external'`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id"}))
}

// fakeReaper is a hand-rolled OrphanReaper for the sweeper tests.
// Records every Stop / RemoveVolume call so tests can assert which
// workspace IDs got reconciled.
type fakeReaper struct {
	mu                  sync.Mutex
	listResponse        []string
	listErr             error
	managedListResponse []string
	managedListErr      error
	stopErr             map[string]error
	removeVolErr        map[string]error
	stopCalls           []string
	removeVolCalls      []string
}

func (f *fakeReaper) ListWorkspaceContainerIDPrefixes(_ context.Context) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResponse, nil
}

func (f *fakeReaper) ListManagedContainerIDPrefixes(_ context.Context) ([]string, error) {
	if f.managedListErr != nil {
		return nil, f.managedListErr
	}
	return f.managedListResponse, nil
}

func (f *fakeReaper) Stop(_ context.Context, wsID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls = append(f.stopCalls, wsID)
	return f.stopErr[wsID]
}

func (f *fakeReaper) RemoveVolume(_ context.Context, wsID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeVolCalls = append(f.removeVolCalls, wsID)
	return f.removeVolErr[wsID]
}

// TestSweepOnce_ReconcilesRunningRemovedRows — the core reconcile
// behavior: a container running for a workspace whose DB row is
// 'removed' gets stopped + volume removed.
func TestSweepOnce_ReconcilesRunningRemovedRows(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Docker reports two ws-* containers; one's row is 'removed'
	// (the leak), the other's is 'online' (the DB rightly excludes
	// it from the WHERE clause and we should NOT reap it).
	reaper := &fakeReaper{
		listResponse: []string{"abc123def456", "xyz789ghi012"},
	}

	// The query asks for status='removed' rows whose id matches the
	// LIKE patterns built from the running container prefixes. Mock
	// returns only the leaked one as a UUID-shaped full id.
	mock.ExpectQuery(`SELECT id::text\s+FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("abc123def456-0000-0000-0000-000000000000"))
	expectStaleTokenSweepNoOp(mock)

	sweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 1 || reaper.stopCalls[0] != "abc123def456-0000-0000-0000-000000000000" {
		t.Errorf("Stop calls = %v, want exactly the leaked id", reaper.stopCalls)
	}
	if len(reaper.removeVolCalls) != 1 || reaper.removeVolCalls[0] != "abc123def456-0000-0000-0000-000000000000" {
		t.Errorf("RemoveVolume calls = %v, want exactly the leaked id", reaper.removeVolCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepOnce_NoRunningContainers — Docker returns nothing, sweeper
// short-circuits without a DB query (no leak possible if no
// containers exist).
func TestSweepOnce_NoRunningContainers(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	reaper := &fakeReaper{listResponse: nil}

	// First two passes short-circuit on empty container lists. The
	// third pass (stale-token sweep) DOES query — that's its whole
	// reason for existing in the no-containers case (operator nuked
	// everything). Mock it returning no stale tokens.
	expectStaleTokenSweepNoOp(mock)
	sweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 0 {
		t.Errorf("Stop should not fire when no containers exist; got %v", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepOnce_DockerListErrorSkipsCycle — a Docker daemon hiccup
// must not cascade into a DB query (otherwise we'd reap based on
// stale information). Skip the cycle, retry next tick.
func TestSweepOnce_DockerListErrorSkipsCycle(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	reaper := &fakeReaper{listErr: errors.New("daemon unreachable")}
	sweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 0 {
		t.Errorf("Stop must not fire when Docker list failed; got %v", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepOnce_StopFailureLeavesVolume — if Stop fails, RemoveVolume
// MUST NOT fire. This is the same trap that motivated the sweeper:
// removing a volume held by a still-running container always errors
// with "volume in use", and we'd accumulate noise in the log without
// actually fixing anything. Leave the volume for the next sweep
// (which will retry Stop).
func TestSweepOnce_StopFailureLeavesVolume(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	reaper := &fakeReaper{
		listResponse: []string{"abc123def456"},
		stopErr: map[string]error{
			"abc123def456-0000-0000-0000-000000000000": errors.New("docker daemon timeout"),
		},
	}
	mock.ExpectQuery(`SELECT id::text\s+FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("abc123def456-0000-0000-0000-000000000000"))
	expectStaleTokenSweepNoOp(mock)

	sweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 1 {
		t.Errorf("Stop should have been attempted exactly once, got %v", reaper.stopCalls)
	}
	if len(reaper.removeVolCalls) != 0 {
		t.Errorf("RemoveVolume must not fire when Stop failed; got %v", reaper.removeVolCalls)
	}
}

// TestSweepOnce_VolumeRemoveErrorIsNonFatal — RemoveVolume failures
// are logged but don't prevent processing other orphans in the same
// cycle. Belt + braces against a transient daemon issue mid-loop.
func TestSweepOnce_VolumeRemoveErrorIsNonFatal(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	reaper := &fakeReaper{
		listResponse: []string{"aaa111bbb222", "ccc333ddd444"},
		removeVolErr: map[string]error{
			"aaa111bbb222-0000-0000-0000-000000000000": errors.New("volume not found"),
		},
	}
	mock.ExpectQuery(`SELECT id::text\s+FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("aaa111bbb222-0000-0000-0000-000000000000").
			AddRow("ccc333ddd444-0000-0000-0000-000000000000"))
	expectStaleTokenSweepNoOp(mock)

	sweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 2 {
		t.Errorf("both orphans should have been Stopped; got %v", reaper.stopCalls)
	}
	if len(reaper.removeVolCalls) != 2 {
		t.Errorf("both orphans should have had RemoveVolume attempted; got %v", reaper.removeVolCalls)
	}
}

// TestSweepOnce_FiltersNonWorkspacePrefixes — the Docker name filter
// is a SUBSTRING match so containers like "my-ws-thing" can slip
// through. The HasPrefix check in the provisioner trims those, but
// the in-sweeper isLikelyWorkspaceID guard is the second line of
// defence: anything outside the UUID alphabet (hex + dashes) is
// rejected before being turned into a SQL LIKE pattern. Locks in
// that no DB query fires when every prefix is filtered out.
func TestSweepOnce_FiltersNonWorkspacePrefixes(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	reaper := &fakeReaper{
		listResponse: []string{
			"not_a_uuid_at_all",            // underscore not in UUID alphabet
			"contains%wildcard",            // SQL LIKE wildcard — must not reach the query
			"contains_wildcard",            // SQL LIKE single-char wildcard
			"",                             // empty
			"valid-but-non-workspace-name", // dash + lowercase letters that aren't hex
		},
	}

	// First-pass query is skipped — every prefix is rejected before
	// the query builds. Third-pass query still runs (filtered prefixes
	// + non-empty input list still produces an empty likes array,
	// which the third-pass treats the same as "no containers running"
	// → stale-token candidates with no LIKE filter). Mock it empty.
	expectStaleTokenSweepNoOp(mock)
	sweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 0 {
		t.Errorf("Stop must not fire when all prefixes filtered; got %v", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestIsLikelyWorkspaceID — pin the alphabet directly. This is the
// guard that prevents SQL LIKE wildcards (`%`, `_`) from reaching
// the sweeper's query.
func TestIsLikelyWorkspaceID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"abc123def456", true},
		{"abcdef-1234-5678-90ab-cdef00112233", true},
		{"ABC123DEF456", true}, // uppercase hex still allowed
		{"", false},
		{"abc_123", false},      // underscore (SQL LIKE single-char wildcard)
		{"abc%123", false},      // percent (SQL LIKE multi-char wildcard)
		{"hello world", false},  // space, non-hex letters
		{"valid-but-not", false}, // 'l', 't', 'n' aren't hex
		{"abc 123", false},
		{".../escape", false},
	}
	for _, tc := range cases {
		got := isLikelyWorkspaceID(tc.in)
		if got != tc.want {
			t.Errorf("isLikelyWorkspaceID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestStartOrphanSweeper_NilReaperIsNoOp — tolerance for the
// nil-provisioner path used by some test harnesses.
func TestStartOrphanSweeper_NilReaperIsNoOp(t *testing.T) {
	// Should return immediately without panicking. Wrap in a goroutine
	// + done-channel so we can assert it didn't block.
	done := make(chan struct{})
	go func() {
		StartOrphanSweeper(context.Background(), nil)
		close(done)
	}()
	select {
	case <-done:
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Fatal("StartOrphanSweeper(nil) blocked instead of returning immediately")
	}
}

// TestSweepOnce_WipedDBReapsLabeledOrphans — the new branch.
// Scenario: a previous platform process labeled and provisioned two
// containers; the operator then `docker compose down -v`'d the DB.
// The new platform boots, sweeper runs:
//   - ListWorkspaceContainerIDPrefixes returns nothing (no name-filter
//     matches because we cleared running ws-* in this scenario via the
//     test setup — irrelevant to second pass)
//   - ListManagedContainerIDPrefixes returns the two labeled prefixes
//     (in real Docker these still exist; their label survives daemon
//     restart)
//   - The reverse-lookup query returns zero matches (DB is empty)
//   - Sweeper reaps both
func TestSweepOnce_WipedDBReapsLabeledOrphans(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	reaper := &fakeReaper{
		listResponse:        nil, // no name-filter matches in this scenario
		managedListResponse: []string{"abc123def456", "ee0011223344"},
	}

	// First-pass query is skipped (listResponse is nil → early return
	// path doesn't even reach a DB call). Second-pass reverse lookup
	// returns no rows — both prefixes are unknown.
	mock.ExpectQuery(`SELECT lk\s+FROM unnest`).
		WillReturnRows(sqlmock.NewRows([]string{"lk"}))
	expectStaleTokenSweepNoOp(mock)

	sweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 2 {
		t.Fatalf("expected 2 Stop calls (both prefixes reaped), got %v", reaper.stopCalls)
	}
	wantStops := map[string]struct{}{"abc123def456": {}, "ee0011223344": {}}
	for _, c := range reaper.stopCalls {
		if _, ok := wantStops[c]; !ok {
			t.Errorf("unexpected Stop call: %q", c)
		}
	}
	if len(reaper.removeVolCalls) != 2 {
		t.Errorf("expected 2 RemoveVolume calls, got %v", reaper.removeVolCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepOnce_WipedDBSkipsLabeledContainersWithRows — the safety
// guarantee: if a labeled container DOES have a workspace row (e.g.
// status='online' or 'paused'), the sweeper must not reap it. Only
// the no-row case justifies reaping.
func TestSweepOnce_WipedDBSkipsLabeledContainersWithRows(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	reaper := &fakeReaper{
		listResponse:        nil,
		managedListResponse: []string{"abc123def456", "ee0011223344"},
	}

	// The reverse-lookup returns both prefixes — both have rows in DB.
	// Sweeper should not reap either.
	mock.ExpectQuery(`SELECT lk\s+FROM unnest`).
		WillReturnRows(sqlmock.NewRows([]string{"lk"}).
			AddRow("abc123def456%").
			AddRow("ee0011223344%"))
	expectStaleTokenSweepNoOp(mock)

	sweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 0 {
		t.Errorf("Stop must not fire when all labeled containers have DB rows; got %v", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepOnce_WipedDBReapsOnlyTheUnknownOnes — mixed case: one
// labeled container has a row (keep), one doesn't (reap).
func TestSweepOnce_WipedDBReapsOnlyTheUnknownOnes(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const keep = "abcdef012345"
	const reap = "fedcba543210"
	reaper := &fakeReaper{
		listResponse:        nil,
		managedListResponse: []string{keep, reap},
	}

	mock.ExpectQuery(`SELECT lk\s+FROM unnest`).
		WillReturnRows(sqlmock.NewRows([]string{"lk"}).
			AddRow(keep + "%"))
	expectStaleTokenSweepNoOp(mock)

	sweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 1 || reaper.stopCalls[0] != reap {
		t.Errorf("expected 1 Stop call for %s, got %v", reap, reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepOnce_WipedDBSkippedOnDockerError — if Docker errors when
// listing managed containers, the second pass aborts cleanly without
// bleeding the error into the first pass. (In this test there's no
// first-pass work either, so nothing should fire.)
func TestSweepOnce_WipedDBSkippedOnDockerError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	reaper := &fakeReaper{
		listResponse:   nil,
		managedListErr: errors.New("docker daemon offline"),
	}

	// No DB query expected for the second pass since we error out
	// before reaching SQL. The third pass (stale-token sweep) uses
	// ListWorkspaceContainerIDPrefixes (which succeeded with empty
	// here, not the same call that errored), so it DOES query.
	expectStaleTokenSweepNoOp(mock)
	sweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 0 {
		t.Errorf("Docker error must not result in Stop calls; got %v", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepOnce_WipedDBSkipsNonUUIDPrefixes — defence-in-depth: if a
// non-UUID-shaped name slipped past the label filter (shouldn't happen
// because the provisioner only labels ws-* containers, but the sweeper
// shouldn't trust upstream invariants), the prefix is dropped before
// hitting the SQL query.
func TestSweepOnce_WipedDBSkipsNonUUIDPrefixes(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const valid = "abc123def456"
	reaper := &fakeReaper{
		listResponse:        nil,
		managedListResponse: []string{"hello world", "abc%inject", valid},
	}

	// Only `valid` survives isLikelyWorkspaceID — it's the only prefix
	// that should appear in the unnest array.
	mock.ExpectQuery(`SELECT lk\s+FROM unnest`).
		WillReturnRows(sqlmock.NewRows([]string{"lk"}))
	expectStaleTokenSweepNoOp(mock)

	sweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 1 || reaper.stopCalls[0] != valid {
		t.Errorf("expected exactly 1 reap (the UUID-shaped one); got %v", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// =============================================================================
// Third pass: sweepStaleTokensWithoutContainer
//
// Heals the user-reported "auth token conflict after volume wipe" failure mode.
// Scenario: operator runs `docker compose down -v` (or any out-of-band volume
// removal); DB still has tokens for workspaces whose recreated containers boot
// with empty /configs and 401 forever on /registry/register.
// =============================================================================

// TestSweepOnce_StaleTokenRevokeFiresWhenNoContainer — the headline
// case. A workspace has live tokens in the DB but no live container
// matches it (volume-wipe scenario). The third pass revokes.
func TestSweepOnce_StaleTokenRevokeFiresWhenNoContainer(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Two name-shaped containers running, both have status='removed'
	// rows so first pass reaps them. Second pass finds nothing
	// (managed list empty in this scenario). Third pass: even though
	// the running containers cover those two prefixes, an unrelated
	// workspace ID (no live container, no name prefix match) has
	// stale tokens — revoke it.
	const orphanedID = "deadbeef-0000-0000-0000-000000000000"
	reaper := &fakeReaper{listResponse: []string{"abc123def456"}}

	mock.ExpectQuery(`SELECT id::text\s+FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("abc123def456-0000-0000-0000-000000000000"))

	// Third-pass query returns the orphaned workspace.
	// Tight regex pins the safety guards: status-filter excludes
	// 'removed' and 'provisioning' (R2 + the C1 fix), runtime filter
	// excludes 'external' (2026-05-03 fix — the sweep was incorrectly
	// targeting external workspaces which have no container by design),
	// and the staleness predicate appears in the SELECT.
	mock.ExpectQuery(`(?s)^\s*SELECT DISTINCT t\.workspace_id::text\s+FROM workspace_auth_tokens.*status NOT IN \('removed', 'provisioning'\).*runtime != 'external'.*COALESCE\(t\.last_used_at, t\.created_at\) < now\(\) - make_interval`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id"}).
			AddRow(orphanedID))

	// Revoke executes one UPDATE — and the UPDATE itself MUST also
	// carry the staleness predicate (closes the C1 TOCTOU race
	// against issueAndInjectToken inserting a fresh token between
	// our SELECT and our UPDATE).
	mock.ExpectExec(`(?s)UPDATE workspace_auth_tokens\s+SET revoked_at = now\(\)\s+WHERE workspace_id = \$1\s+AND revoked_at IS NULL\s+AND COALESCE\(last_used_at, created_at\) < now\(\) - make_interval`).
		WithArgs(orphanedID, 300).
		WillReturnResult(sqlmock.NewResult(0, 1))

	sweepOnce(context.Background(), reaper)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepOnce_StaleTokenSkippedWhenContainerExists — pin the safety
// guarantee: a workspace with both live tokens AND a live container
// must NOT be revoked. The query's NOT LIKE clause is the gate; this
// test exercises that gate by having the third-pass query return zero
// rows (the live-container workspace is filtered out).
func TestSweepOnce_StaleTokenSkippedWhenContainerExists(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// One running container; first pass returns no removed-row matches.
	reaper := &fakeReaper{listResponse: []string{"abc123def456"}}
	mock.ExpectQuery(`SELECT id::text\s+FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	// Third-pass query: the live workspace has live tokens but its
	// prefix matches the running container, so the NOT LIKE excludes
	// it. Result: zero stale tokens.
	expectStaleTokenSweepNoOp(mock)

	sweepOnce(context.Background(), reaper)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepOnce_StaleTokenRevokeFailureBailsLoop — a transient DB
// error during revoke must not spam the log on every iteration.
// Bail out of the loop; next 60s cycle retries.
func TestSweepOnce_StaleTokenRevokeFailureBailsLoop(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	reaper := &fakeReaper{listResponse: nil}

	// Third-pass returns two stale-token workspaces; the first revoke
	// errors. Loop must bail without attempting the second.
	mock.ExpectQuery(`(?s)^\s*SELECT DISTINCT t\.workspace_id::text\s+FROM workspace_auth_tokens.*status NOT IN \('removed', 'provisioning'\).*runtime != 'external'`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id"}).
			AddRow("aaaa1111-0000-0000-0000-000000000000").
			AddRow("bbbb2222-0000-0000-0000-000000000000"))
	mock.ExpectExec(`(?s)UPDATE workspace_auth_tokens\s+SET revoked_at = now\(\)\s+WHERE workspace_id = \$1\s+AND revoked_at IS NULL\s+AND COALESCE\(last_used_at, created_at\) < now\(\) - make_interval`).
		WithArgs("aaaa1111-0000-0000-0000-000000000000", 300).
		WillReturnError(errors.New("connection reset"))
	// No second ExpectExec: if the loop tries it, sqlmock fails
	// "unexpected call".

	sweepOnce(context.Background(), reaper)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepOnce_StaleTokenQueryErrorIsNonFatal — a transient DB error
// on the SELECT must not prevent the rest of sweepOnce from making
// progress. (In this test there's no other progress to make either,
// just verifying no panic + the cycle completes.)
func TestSweepOnce_StaleTokenQueryErrorIsNonFatal(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	reaper := &fakeReaper{listResponse: nil}

	mock.ExpectQuery(`(?s)^\s*SELECT DISTINCT t\.workspace_id::text\s+FROM workspace_auth_tokens.*status NOT IN \('removed', 'provisioning'\)`).
		WillReturnError(errors.New("connection reset"))

	sweepOnce(context.Background(), reaper)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepOnce_StaleTokenRevokeUsesStalenessPredicate — pin the C1
// race fix: the per-workspace UPDATE must carry the staleness
// predicate so a token inserted by issueAndInjectToken between our
// SELECT and our UPDATE is automatically excluded (its created_at is
// fresh and won't satisfy `< now() - grace`).
//
// This test asserts the SHAPE of the UPDATE (predicate present, grace
// argument bound). A real-Postgres integration test would prove the
// race resolution end-to-end; this catches the regression where
// someone "simplifies" the UPDATE back to a predicate-only revoke.
func TestSweepOnce_StaleTokenRevokeUsesStalenessPredicate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const orphanedID = "deadbeef-0000-0000-0000-000000000000"
	reaper := &fakeReaper{listResponse: nil}

	mock.ExpectQuery(`(?s)^\s*SELECT DISTINCT t\.workspace_id::text\s+FROM workspace_auth_tokens.*status NOT IN \('removed', 'provisioning'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id"}).
			AddRow(orphanedID))

	// The UPDATE regex requires every guard: workspace_id binding,
	// revoked_at IS NULL, AND the staleness predicate using the SAME
	// COALESCE expression as the SELECT. Loosening any of these
	// would re-open the C1 race, and this regex would no longer match.
	mock.ExpectExec(`(?s)UPDATE workspace_auth_tokens\s+SET revoked_at = now\(\)\s+WHERE workspace_id = \$1\s+AND revoked_at IS NULL\s+AND COALESCE\(last_used_at, created_at\) < now\(\) - make_interval\(secs => \$2\)`).
		WithArgs(orphanedID, 300).
		WillReturnResult(sqlmock.NewResult(0, 1))

	sweepOnce(context.Background(), reaper)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepStaleTokens_NilReaperEarlyExit — defence-in-depth (F2):
// even though StartOrphanSweeper short-circuits on nil reaper, the
// individual pass also early-exits. Protects against future refactors
// that wire the pass without the outer guard.
func TestSweepStaleTokens_NilReaperEarlyExit(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// No DB queries expected. If the early-return is removed, sqlmock
	// fails on the unexpected SELECT.
	sweepStaleTokensWithoutContainer(context.Background(), nil)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSweepOnce_StaleTokenSkippedWhenDockerListFails — if the third
// pass can't enumerate containers (Docker hiccup), it must skip the
// query entirely. Otherwise it would query with empty likes and
// revoke every stale-token workspace based on stale information.
func TestSweepOnce_StaleTokenSkippedWhenDockerListFails(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	reaper := &fakeReaper{listErr: errors.New("daemon unreachable")}

	// No DB queries expected: first pass bails on listErr, second
	// pass uses managedList (also fails because we never set it),
	// third pass also bails on listErr. Verify by NOT registering
	// ExpectStaleTokenSweepNoOp — sqlmock fails on any unexpected
	// query.
	sweepOnce(context.Background(), reaper)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
