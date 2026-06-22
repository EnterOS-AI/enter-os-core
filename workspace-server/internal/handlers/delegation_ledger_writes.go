package handlers

import (
	"context"
	"os"
)

// delegation_ledger_writes.go — RFC #2829 follow-up (#318): wire
// DelegationLedger Insert + SetStatus calls into the existing
// activity_logs-driven flow without touching the legacy code path.
//
// Why a flag (not always-on)
// --------------------------
// The legacy flow writes everything to activity_logs and a tight
// strict-sqlmock test surface (~30 tests) pins exactly which SQL
// statements fire per handler invocation. Adding ledger writes
// always-on would force updating each of those tests in this PR.
// Gating behind DELEGATION_LEDGER_WRITE=1 lets ledger-driven
// behavior land independently of the test refactor — operators
// can flip it on in staging to populate the `delegations` table
// (and thus give the PR-3 sweeper + PR-4 dashboard data to work
// with) without coupling the rollout to a churn-y test diff.
//
// Default off → byte-identical to pre-#318 behavior. Flip after
// staging burn-in once the agent-side cutover (PR-5) has proven
// the round-trip end-to-end.

func ledgerWritesEnabled() bool {
	return os.Getenv("DELEGATION_LEDGER_WRITE") == "1"
}

// recordLedgerInsert is the gated wrapper around DelegationLedger.Insert.
// All callers in delegation.go go through here so flipping the flag
// requires no further code changes — the gate is one function.
//
// taskPreview is truncated by the ledger to `previewCap` bytes; pass
// the full task text without pre-truncating.
func recordLedgerInsert(ctx context.Context, callerID, calleeID, delegationID, taskPreview, idemKey string) {
	if !ledgerWritesEnabled() {
		return
	}
	NewDelegationLedger(nil).Insert(ctx, InsertOpts{
		DelegationID:   delegationID,
		CallerID:       callerID,
		CalleeID:       calleeID,
		TaskPreview:    taskPreview,
		IdempotencyKey: idemKey,
	})
}

// recordLedgerStatus is the gated wrapper around DelegationLedger.SetStatus.
// status MUST be one of the lifecycle values the ledger accepts
// (queued|dispatched|in_progress|completed|failed|stuck). errorDetail is
// non-empty for failed/stuck; resultPreview is non-empty for completed.
//
// Errors are logged inside the ledger and not propagated — the legacy
// activity_logs path remains authoritative; ledger is best-effort
// (matches the tenant_resources audit posture, memory ref:
// `reference_tenant_resources_audit`).
func recordLedgerStatus(ctx context.Context, delegationID, status, errorDetail, resultPreview string) {
	if !ledgerWritesEnabled() {
		return
	}
	// SetStatus returns an error (e.g. ErrInvalidTransition for forward-
	// only protection on terminal states) but we don't propagate it —
	// the legacy path's status writes are still authoritative for the
	// dashboard, and a ledger replay error is not a delegation failure.
	_ = NewDelegationLedger(nil).SetStatus(ctx, delegationID, status, errorDetail, resultPreview)
}
