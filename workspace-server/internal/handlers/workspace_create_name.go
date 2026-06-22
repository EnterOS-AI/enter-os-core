package handlers

// workspace_create_name.go — disambiguate workspace names on the
// Canvas POST /workspaces path so a double-clicked template card
// does not surface raw Postgres errors.
//
// Background (#2872 + post-2026-05-06 follow-up):
//   - Migration 20260506000000_workspaces_unique_parent_name added a
//     partial UNIQUE index on (COALESCE(parent_id, sentinel), name)
//     WHERE status != 'removed'. It exists to close the TOCTOU race in
//     /org/import that previously let two concurrent POSTs both INSERT
//     the same (parent_id, name) row.
//   - /org/import handles the constraint via `ON CONFLICT DO NOTHING`
//     + idempotent re-select (handlers/org_import.go).
//   - The Canvas Create handler (handlers/workspace.go) did NOT — a
//     duplicate POST returned an opaque HTTP 500 with the raw pq error
//     in the server log. Repro path: user clicks a template card twice
//     in canvas before the first response paints.
//
// Resolution: auto-suffix the user-typed name on collision. The
// uniqueness constraint required for #2872 stays in place; only the
// Canvas Create path's reaction to it changes. Names become a
// free-form display label that the platform disambiguates; row
// identity is carried by the workspace id (UUID).
//
// Suffix shape: " (2)", " (3)", … up to N=maxNameSuffix. Chosen over
// numeric "-2" / "_2" because the parenthesised form is the standard
// disambiguation pattern users already expect from Finder / Explorer
// / Google Docs / file managers. Stays under the 255-char name cap
// (#688 — validated by validateWorkspaceFields) for any reasonable
// base name; parens are not in yamlSpecialChars so the existing YAML-
// safety guard is unaffected.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// maxNameSuffix bounds the suffix-retry loop. 20 is well above any
// plausible accidental-double-click rate (typical: 2-3 races) and
// keeps the worst-case handler latency to ~20 round-trips. If a
// caller actually wants 21+ workspaces with the same base name, they
// can pre-disambiguate client-side; the platform refuses to spin
// indefinitely.
const maxNameSuffix = 20

// workspacesUniqueIndexName is the partial-unique index this handler
// is reacting to. Pinned to the migration's index name so we
// distinguish "the base name collision we know how to handle" from
// every other unique violation (which we surface as 409 without
// retry — silently auto-suffixing a name on the wrong constraint
// would mask real bugs).
const workspacesUniqueIndexName = "workspaces_parent_name_uniq"

// errWorkspaceNameExhausted is returned when maxNameSuffix retries
// all fail because every candidate name in the (base, " (2)", …,
// " (N)") sequence is taken. The caller maps this to HTTP 409
// Conflict — the user must rename and re-try.
var errWorkspaceNameExhausted = errors.New("workspace name exhausted: too many duplicates of base name under same parent")

// insertWorkspaceWithNameRetry runs the workspace INSERT and, if it
// hits the parent-name unique-violation, retries with a suffixed
// name. Returns the name actually persisted (which the caller MUST
// use in the response and in broadcast payloads — without it the
// canvas would show the user-typed name while the DB has the
// suffixed one, and the next poll would surprise the user with the
// "real" name).
//
// The query string is intentionally a parameter (not hardcoded) so
// the helper composes with future schema additions without growing
// a new arity each time. Only the FIRST arg of args must be the
// name placeholder ($1) — the helper rewrites args[0] on retry; all
// other args pass through verbatim. (This matches the workspace.go
// INSERT below where $1 is the id and $2 is name, so the caller
// passes nameArgIndex=1.)
//
// On the unique-violation, the original tx is rolled back and a
// fresh one is begun before retry — Postgres marks the tx aborted
// on any error, so re-using it would silently no-op every
// subsequent statement.
//
// `beginTx` is a closure (not a *sql.DB) so the caller controls the
// transaction-options + the context. Returning the fresh tx each
// retry means the caller can commit it once the helper succeeds.
//
// `query` MUST be parameterized — the name placeholder is rewritten
// via args[nameArgIndex], not via string substitution. Passing a
// fmt.Sprintf'd query string would silently disable the safety.
func insertWorkspaceWithNameRetry(
	ctx context.Context,
	tx *sql.Tx,
	beginTx func(ctx context.Context) (*sql.Tx, error),
	baseName string,
	nameArgIndex int,
	query string,
	args []any,
) (finalName string, finalTx *sql.Tx, err error) {
	if nameArgIndex < 0 || nameArgIndex >= len(args) {
		return "", tx, fmt.Errorf("insertWorkspaceWithNameRetry: nameArgIndex %d out of range for %d args", nameArgIndex, len(args))
	}

	current := tx
	for attempt := 0; attempt <= maxNameSuffix; attempt++ {
		candidate := baseName
		if attempt > 0 {
			candidate = fmt.Sprintf("%s (%d)", baseName, attempt+1)
		}
		args[nameArgIndex] = candidate
		_, execErr := current.ExecContext(ctx, query, args...)
		if execErr == nil {
			return candidate, current, nil
		}
		if !isParentNameUniqueViolation(execErr) {
			// Any other error (encoding, connection, FK violation,
			// other unique index) — return as-is. Caller decides
			// status code.
			return "", current, execErr
		}
		// Hit the partial-unique index. Postgres has aborted this
		// tx — roll it back and start fresh before retrying with a
		// new candidate name.
		_ = current.Rollback()
		if attempt == maxNameSuffix {
			break
		}
		next, txErr := beginTx(ctx)
		if txErr != nil {
			return "", nil, fmt.Errorf("begin retry tx after name collision: %w", txErr)
		}
		current = next
	}
	// Exhausted: the helper rolled back the last tx already. Return
	// nil tx so the caller does not try to commit/rollback again.
	return "", nil, errWorkspaceNameExhausted
}

// isParentNameUniqueViolation reports whether err is the specific
// partial-unique-index violation we know how to auto-suffix. We pin
// on BOTH the SQLSTATE 23505 (unique_violation) AND the constraint
// name so we don't silently rename around an unrelated unique index
// (e.g. a future workspaces.slug unique).
//
// errors.As is used (not a `.(*pq.Error)` type assertion) because
// lib/pq wraps the error through fmt.Errorf in some paths.
//
// Defensive fallback: if Constraint is empty (older pq builds, or
// the error came through a wrapper that dropped the field), match
// on the error message as well. The message form is brittle
// (postgres locale-dependent) but every English-locale Postgres
// emits the index name verbatim.
func isParentNameUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		if pqErr.Code != "23505" {
			return false
		}
		if pqErr.Constraint == workspacesUniqueIndexName {
			return true
		}
		// Fallback for builds that drop Constraint metadata.
		return strings.Contains(pqErr.Message, workspacesUniqueIndexName)
	}
	// Last-resort string match — the pq.Error type was lost
	// through wrapping. Same English-locale caveat as above; keeps
	// the helper robust in test seams that synthesize errors via
	// fmt.Errorf("pq: …").
	return strings.Contains(err.Error(), workspacesUniqueIndexName)
}
