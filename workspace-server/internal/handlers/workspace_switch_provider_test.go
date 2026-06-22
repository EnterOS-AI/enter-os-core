package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestSwitchProvider_StopBeforeProviderWrite is the load-bearing ordering pin.
// The stop helper MUST appear before the UPDATE that writes the new provider —
// otherwise the teardown uses the wrong backend metadata and the old box leaks.
// A source-level position check guards against a refactor reordering the two.
func TestSwitchProvider_StopBeforeProviderWrite(t *testing.T) {
	wd, _ := os.Getwd()
	src, err := os.ReadFile(filepath.Join(wd, "workspace_switch_provider.go"))
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	stripped := stripGoComments(src)
	stopIdx := bytes.Index(stripped, []byte("cpStopWithRetryErr(ctx, id, \"SwitchProvider\""))
	if stopIdx < 0 {
		t.Fatal("SwitchProvider must stop the old box via cpStopWithRetryErr before reprovisioning")
	}
	// the provider write is the jsonb_set on compute -> {provider}
	writeIdx := bytes.Index(stripped, []byte("'{provider}'"))
	if writeIdx < 0 {
		t.Fatal("SwitchProvider must write the new provider via jsonb_set on compute->{provider}")
	}
	if stopIdx >= writeIdx {
		t.Fatalf("ORDERING HAZARD: stop helper (idx %d) must come BEFORE the provider write (idx %d) — else the old box is torn down with wrong backend metadata and leaks", stopIdx, writeIdx)
	}
	// and the instance_id must be cleared in the same UPDATE (retry-safety)
	if !bytes.Contains(stripped, []byte("instance_id = NULL")) {
		t.Fatal("SwitchProvider must clear instance_id when writing the new provider (retry-safety)")
	}
}

// TestSwitchProvider_PreClaimGatesStop pins the CR2 #11473 blocking-finding
// fix: the per-workspace stop helper MUST appear AFTER the pre-claim's
// RowsAffected check, so a losing pre-claim returns 409 without ever
// touching the stop. Pre-fix, the stop ran unconditionally before the
// CAS — a request against a workspace that was already provisioning
// would stop the in-flight box it didn't own (review finding: "the
// loser should not be able to stop a box owned by an in-flight
// provision/switch"). A source-level position check guards against
// a refactor re-introducing the order.
func TestSwitchProvider_PreClaimGatesStop(t *testing.T) {
	wd, _ := os.Getwd()
	src, err := os.ReadFile(filepath.Join(wd, "workspace_switch_provider.go"))
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	stripped := stripGoComments(src)
	preClaimIdx := bytes.Index(stripped, []byte("preClaim, err := db.DB.ExecContext(ctx, `"))
	if preClaimIdx < 0 {
		t.Fatal("SwitchProvider must have a pre-claim UPDATE (the fix for CR2 #11473) — pre-claim gates the stop on a successful CAS")
	}
	preClaimLoseIdx := bytes.Index(stripped, []byte("ALREADY_SWITCHING"))
	if preClaimLoseIdx < 0 {
		t.Fatal("SwitchProvider must return ALREADY_SWITCHING on a lost pre-claim — this is the 409 path the pre-claim gates")
	}
	stopIdx := bytes.Index(stripped, []byte("cpStopWithRetryErr(ctx, id, \"SwitchProvider\""))
	if stopIdx < 0 {
		t.Fatal("SwitchProvider must call cpStopWithRetryErr for the OLD box")
	}
	// The 409-on-lost-pre-claim path must appear BEFORE the stop — the
	// stop is gated on a successful pre-claim.
	if preClaimLoseIdx >= stopIdx {
		t.Fatalf("ORDERING HAZARD: the ALREADY_SWITCHING 409 path (idx %d) must come BEFORE the stop helper (idx %d) — a losing pre-claim must return 409 without ever touching the stop side effect (CR2 #11473)", preClaimLoseIdx, stopIdx)
	}
	// And the pre-claim itself must come before the stop too.
	if preClaimIdx >= stopIdx {
		t.Fatalf("ORDERING HAZARD: the pre-claim (idx %d) must come BEFORE the stop (idx %d)", preClaimIdx, stopIdx)
	}
}

// TestSwitchProvider_ConcurrencyGuardAndAudit pins the two hardening items from
// the correctness review: (a) a switch is guarded by an atomic CAS (pre-claim
// + provider write) so two concurrent switches can't both launch a provision
// or both stop the same box, and (b) stop-exhaustion emits a durable audit
// row carrying the old instance_id+provider so the old box remains
// discoverable after instance_id is nulled.
//
// CR2 #11473 update: the original code did the stop BEFORE the CAS, so a
// losing request still executed the stop side effect. The fix splits the
// guard into a PRE-CLAIM (status='provisioning' only, provider unchanged)
// and the provider write — the stop now runs ONLY after the pre-claim
// succeeds, so a losing pre-claim returns 409 without stopping the box.
func TestSwitchProvider_ConcurrencyGuardAndAudit(t *testing.T) {
	wd, _ := os.Getwd()
	src, err := os.ReadFile(filepath.Join(wd, "workspace_switch_provider.go"))
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := stripGoComments(src)
	// Pre-claim: status='provisioning' with status<>provisioning CAS so
	// the stop is gated on a successful claim. The pre-claim's WHERE
	// clause must include BOTH `status <> $` AND a provider-unchanged
	// check, so a losing race (workspace already provisioning, OR
	// provider changed) returns 0 rows and 409s without stopping
	// the box.
	if !bytes.Contains(s, []byte("status <> $2")) || !bytes.Contains(s, []byte("IS NOT DISTINCT FROM $3")) {
		t.Error("the PRE-CLAIM must be a CAS (status not already provisioning AND provider unchanged) — this is the guard that prevents a losing request from executing the stop side effect (CR2 #11473)")
	}
	if !bytes.Contains(s, []byte("RowsAffected")) || !bytes.Contains(s, []byte("ALREADY_SWITCHING")) {
		t.Error("SwitchProvider must 409 ALREADY_SWITCHING when the pre-claim affects 0 rows (lost the race before the stop runs)")
	}
	if !bytes.Contains(s, []byte("cpStopWithRetryErr")) {
		t.Error("SwitchProvider must use cpStopWithRetryErr to detect stop exhaustion")
	}
	if !bytes.Contains(s, []byte("emitSwitchProviderStopExhausted")) {
		t.Error("SwitchProvider must emit an audit row with old instance/provider metadata on stop exhaustion")
	}
	// Routing invariant: the NEW-box provision must go through the
	// central Auto dispatcher, not the direct per-backend body (this
	// is the core#2422 RCA-tick fix that closes the Platform-Go red
	// on TestNoCallSiteCallsDirectProvisionerExceptAuto).
	if bytes.Contains(s, []byte("h.goAsync(func() { h.provisionWorkspaceCP(")) {
		t.Error("SwitchProvider must route the NEW-box provision through provisionWorkspaceAuto, NOT through h.provisionWorkspaceCP directly (TestNoCallSiteCallsDirectProvisionerExceptAuto pin)")
	}
}

// TestSwitchProvider_RejectsBadProvider: the allowlist check fires before any DB
// access, so a bad/missing provider is a clean 400 without touching the backend.
func TestSwitchProvider_RejectsBadProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &WorkspaceHandler{}
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"provider":"azure"}`, http.StatusBadRequest},
		{`{"provider":""}`, http.StatusBadRequest},
		{`{"provider":"AWS-typo"}`, http.StatusBadRequest},
		{`{}`, http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
		c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/switch-provider", strings.NewReader(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.SwitchProvider(c)
		if w.Code != tc.want {
			t.Errorf("body %s: got %d want %d (%s)", tc.body, w.Code, tc.want, w.Body.String())
		}
	}
}

// TestSwitchProvider_PreClaimRollbackOnError (CR2 #11486 + #11493) is the
// source-level pin for the commit-or-rollback pattern added after the
// pre-claim landed. The pre-claim sets status='provisioning'; without
// the rollback defer, any error / ctx-cancellation between pre-claim
// and step 5 (committed) would strand the workspace in 'provisioning'
// forever. The defer must:
//   1. Be armed ONLY AFTER this request's pre-claim succeeds
//      (CR2 #11493 ownership-ordering fix — a losing pre-claim must
//      NOT arm the defer, otherwise the rollback UPDATE (gated on
//      `status = 'provisioning'`) would clobber ANOTHER request's
//      in-flight pre-claim to OUR priorStatus, stranding them).
//   2. Revert status to priorStatus on any error path (commit-or-rollback).
//   3. Use a fresh context (not the request ctx) so client
//      disconnect mid-switch still cleans up.
//   4. Set `committed = true` ONLY at the very end (after step 5).
func TestSwitchProvider_PreClaimRollbackOnError(t *testing.T) {
	wd, _ := os.Getwd()
	src, err := os.ReadFile(filepath.Join(wd, "workspace_switch_provider.go"))
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := stripGoComments(src)
	// Commit-or-rollback flag
	if !bytes.Contains(s, []byte("committed := false")) {
		t.Error("SwitchProvider must declare a `committed := false` flag for the commit-or-rollback pattern (CR2 #11486)")
	}
	if !bytes.Contains(s, []byte("committed = true")) {
		t.Error("SwitchProvider must set `committed = true` only after the provision is dispatched — the rollback defer reads this flag to decide whether to revert status")
	}
	// defer that checks the flag and reverts
	if !bytes.Contains(s, []byte("defer func() {")) || !bytes.Contains(s, []byte("if committed {")) {
		t.Error("SwitchProvider must have a defer that checks the `committed` flag and reverts status to priorStatus on any uncommitted path (CR2 #11486)")
	}
	// Rollback uses a fresh context (not the request ctx) so client
	// disconnect mid-switch still cleans up.
	if !bytes.Contains(s, []byte("rollbackCtx, cancel := context.WithTimeout(context.Background()")) {
		t.Error("SwitchProvider's rollback defer must use a FRESH context (context.Background), not the request ctx — a cancelled request ctx would skip the cleanup and strand the workspace")
	}
	// priorStatus captured at the top of the function
	if !bytes.Contains(s, []byte("priorStatus := status")) {
		t.Error("SwitchProvider must capture `priorStatus := status` (the value from the lookup query) before the pre-claim so the rollback can restore it")
	}
	// The rollback UPDATE is gated on status='provisioning' so we don't
	// clobber a newer status set by a concurrent switch/provision.
	if !bytes.Contains(s, []byte("AND status = $3")) {
		t.Error("SwitchProvider's rollback UPDATE must be gated on `status = 'provisioning'` so a concurrent switch/provision that has already advanced the status is not clobbered")
	}
}

// TestSwitchProvider_PreClaimLoserDoesNotArmRollback (CR2 #11493) is
// the regression test for the ownership-ordering fix: the commit-or-
// rollback defer MUST be armed AFTER the pre-claim's 0-rows return,
// not before. A losing pre-claim must NOT arm the defer — otherwise
// the rollback UPDATE (gated on `status = 'provisioning'`) could
// clobber ANOTHER request's in-flight pre-claim to OUR priorStatus,
// stranding them.
//
// Source-level position check: the `defer func() {` opening must
// appear AFTER the `ALREADY_SWITCHING` 409 return (the losing
// pre-claim path).
func TestSwitchProvider_PreClaimLoserDoesNotArmRollback(t *testing.T) {
	wd, _ := os.Getwd()
	src, err := os.ReadFile(filepath.Join(wd, "workspace_switch_provider.go"))
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	stripped := stripGoComments(src)
	// Find the ALREADY_SWITCHING 409 return — the losing-pre-claim path.
	loseIdx := bytes.Index(stripped, []byte("ALREADY_SWITCHING"))
	if loseIdx < 0 {
		t.Fatal("SwitchProvider must have an ALREADY_SWITCHING 409 return on a lost pre-claim")
	}
	// Find the defer opening — must come AFTER the 409 return.
	deferIdx := bytes.Index(stripped, []byte("defer func() {"))
	if deferIdx < 0 {
		t.Fatal("SwitchProvider must have a `defer func() { ... }` block for the commit-or-rollback pattern")
	}
	if deferIdx <= loseIdx {
		t.Fatalf("OWNERSHIP-ORDERING BUG (CR2 #11493): the commit-or-rollback defer (idx %d) must be armed AFTER the losing-pre-claim 409 return (idx %d) — a losing pre-claim must NOT arm the defer, otherwise the rollback UPDATE (gated on `status = 'provisioning'`) would clobber another request's in-flight pre-claim to OUR priorStatus, stranding them", deferIdx, loseIdx)
	}
}

// TestSwitchProvider_RouteRegistered pins the route wiring.
func TestSwitchProvider_RouteRegistered(t *testing.T) {
	wd, _ := os.Getwd()
	src, err := os.ReadFile(filepath.Join(wd, "..", "router", "router.go"))
	if err != nil {
		t.Fatalf("read router: %v", err)
	}
	if !bytes.Contains(src, []byte(`POST("/switch-provider", wh.SwitchProvider)`)) {
		t.Fatal("router must register POST /switch-provider → wh.SwitchProvider")
	}
}
