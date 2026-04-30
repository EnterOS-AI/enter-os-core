package models

// Workspace status — typed constants that mirror the `workspace_status`
// Postgres enum (migrations 043 + 046). Every UPDATE/INSERT against
// `workspaces.status` MUST use one of these constants; raw string
// literals are forbidden (see internal/db/workspace_status_enum_drift_test.go,
// which fails the build if a literal sneaks in).
//
// Why typed: pre-2026-04-30 the enum migrated without `awaiting_agent`
// and `hibernating` even though application code wrote those values.
// Every UPDATE silently failed with `invalid input value for enum
// workspace_status: ...` for five days because:
//
//   - Status values were ad-hoc string literals scattered across
//     ~15 raw SQL strings in 8 files. Typos (e.g. 'hibernating' vs
//     'hibernated') and missing-from-enum cases were invisible to
//     the compiler.
//   - sqlmock (the unit-test layer) matched SQL by regex, not against
//     the live enum constraint.
//   - Errors were dropped or log-and-continued at every call site.
//
// Typed constants close the first leg by making the bug uncompilable:
// adding a new status forces both this file AND the migration to
// change in the same PR; typos at call sites become "undefined: ..."
// at build time, not silent runtime failures.

// WorkspaceStatus is the type-safe alias for values written into
// `workspaces.status`. Its underlying type is string so it flows
// through database/sql args and JSON marshalling unchanged.
type WorkspaceStatus string

// Recognised values. The set MUST be a subset of the workspace_status
// Postgres enum (migrations 043 + 046). The drift gate in
// internal/db/workspace_status_enum_drift_test.go enforces this.
const (
	StatusProvisioning   WorkspaceStatus = "provisioning"
	StatusOnline         WorkspaceStatus = "online"
	StatusOffline        WorkspaceStatus = "offline"
	StatusDegraded       WorkspaceStatus = "degraded"
	StatusFailed         WorkspaceStatus = "failed"
	StatusRemoved        WorkspaceStatus = "removed"
	StatusPaused         WorkspaceStatus = "paused"
	StatusHibernated     WorkspaceStatus = "hibernated"
	StatusHibernating    WorkspaceStatus = "hibernating"
	StatusAwaitingAgent  WorkspaceStatus = "awaiting_agent"
)

// AllWorkspaceStatuses is the source-of-truth list the drift gate
// parses. Keep in sync with the const block above. Deliberately a
// var (not derivable from the const block at compile time without
// reflection) — the gate parses the const block AST directly, which
// is more robust than reflection.
var AllWorkspaceStatuses = []WorkspaceStatus{
	StatusProvisioning,
	StatusOnline,
	StatusOffline,
	StatusDegraded,
	StatusFailed,
	StatusRemoved,
	StatusPaused,
	StatusHibernated,
	StatusHibernating,
	StatusAwaitingAgent,
}

// String allows fmt.Sprintf("%s", status) without callers having to
// cast. Returns the underlying enum string.
func (s WorkspaceStatus) String() string { return string(s) }
