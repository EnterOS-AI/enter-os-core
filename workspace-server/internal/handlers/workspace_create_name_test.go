package handlers

// workspace_create_name_test.go — unit + table tests for the
// duplicate-name auto-suffix retry helper.
//
// Phase 3 of the dev-SOP: write the test first, watch it fail in
// the way you predicted, then watch the fix make it pass. The fix
// landed in workspace_create_name.go; these tests pin its contract
// so a refactor that drops the retry (or auto-suffixes on the
// WRONG constraint) blows up loud.
//
// sqlmock CANNOT verify the real partial-index behaviour — that
// lives in the companion integration test
// workspace_create_name_integration_test.go (real Postgres).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/lib/pq"
)

// fakePqUniqueViolation reproduces the SQLSTATE/Constraint shape
// the real lib/pq driver emits when an INSERT hits
// workspaces_parent_name_uniq. Used by the unit test to drive the
// retry path without standing up a real Postgres.
func fakePqUniqueViolation(constraint string) error {
	return &pq.Error{
		Code:       "23505",
		Constraint: constraint,
		Message:    fmt.Sprintf("duplicate key value violates unique constraint %q", constraint),
	}
}

// TestIsParentNameUniqueViolation_PinsTheConstraint exhaustively
// pins which error shapes the helper considers "auto-suffix
// eligible." A regression that broadens this predicate (e.g.
// matching ANY 23505) would mask real bugs; a regression that
// narrows it (e.g. dropping the message fallback) would let the
// 500-on-double-click bug recur on driver builds that strip
// Constraint metadata.
func TestIsParentNameUniqueViolation_PinsTheConstraint(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"plain string error", errors.New("network down"), false},
		{
			name: "23505 on parent_name_uniq via pq.Error",
			err:  fakePqUniqueViolation("workspaces_parent_name_uniq"),
			want: true,
		},
		{
			name: "23505 on a DIFFERENT unique index — must NOT be auto-suffixed",
			err:  fakePqUniqueViolation("workspaces_slug_uniq"),
			want: false,
		},
		{
			name: "23505 with empty Constraint — fall back to message match",
			err: &pq.Error{
				Code:    "23505",
				Message: `duplicate key value violates unique constraint "workspaces_parent_name_uniq"`,
			},
			want: true,
		},
		{
			name: "non-23505 (e.g. FK violation) on the same index name in message — must NOT match",
			err: &pq.Error{
				Code:    "23503",
				Message: `foreign key references workspaces_parent_name_uniq region`,
			},
			want: false,
		},
		{
			name: "wrapped via fmt.Errorf (errors.As must unwrap)",
			err:  fmt.Errorf("create workspace: %w", fakePqUniqueViolation("workspaces_parent_name_uniq")),
			want: true,
		},
		{
			name: "raw string from a non-pq error mentioning the index — last-resort fallback",
			err:  errors.New(`pq: duplicate key value violates unique constraint "workspaces_parent_name_uniq"`),
			want: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := isParentNameUniqueViolation(tc.err)
			if got != tc.want {
				t.Fatalf("isParentNameUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestInsertWorkspaceWithNameRetry_FirstAttemptSucceeds confirms
// the helper does NOT modify the name when the first INSERT
// succeeds — a naive implementation that always wraps in a retry
// loop could accidentally add a " (1)" suffix even on the happy
// path.
func TestInsertWorkspaceWithNameRetry_FirstAttemptSucceeds(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("id-1", "MyWorkspace").
		WillReturnResult(sqlmock.NewResult(0, 1))

	tx, err := getDBHandle(t).BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	name, finalTx, err := insertWorkspaceWithNameRetry(
		context.Background(),
		tx,
		func(ctx context.Context) (*sql.Tx, error) {
			return getDBHandle(t).BeginTx(ctx, nil)
		},
		"MyWorkspace",
		1,
		"INSERT INTO workspaces (id, name) VALUES ($1, $2)",
		[]any{"id-1", "MyWorkspace"},
	)
	if err != nil {
		t.Fatalf("retry helper: %v", err)
	}
	if name != "MyWorkspace" {
		t.Fatalf("name = %q, want %q (happy path must NOT suffix)", name, "MyWorkspace")
	}
	if finalTx == nil {
		t.Fatalf("finalTx == nil; caller needs a live tx to commit")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestInsertWorkspaceWithNameRetry_SecondAttemptSuffixed confirms
// that on a single collision the helper retries with " (2)" and
// returns that as the persisted name. The dispatched-name suffix
// shape is part of the user-visible contract — if a future
// refactor switches to "-2" / "_2" / "MyWorkspace2", the canvas
// renders the wrong label until the next poll.
func TestInsertWorkspaceWithNameRetry_SecondAttemptSuffixed(t *testing.T) {
	mock := setupTestDB(t)

	// First begin (caller-owned), then first INSERT fails with the
	// partial-unique violation, helper rolls back the tx, opens a
	// fresh tx, and the second INSERT (with " (2)") succeeds.
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("id-1", "MyWorkspace").
		WillReturnError(fakePqUniqueViolation("workspaces_parent_name_uniq"))
	mock.ExpectRollback()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("id-1", "MyWorkspace (2)").
		WillReturnResult(sqlmock.NewResult(0, 1))

	tx, err := getDBHandle(t).BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	name, finalTx, err := insertWorkspaceWithNameRetry(
		context.Background(),
		tx,
		func(ctx context.Context) (*sql.Tx, error) {
			return getDBHandle(t).BeginTx(ctx, nil)
		},
		"MyWorkspace",
		1,
		"INSERT INTO workspaces (id, name) VALUES ($1, $2)",
		[]any{"id-1", "MyWorkspace"},
	)
	if err != nil {
		t.Fatalf("retry helper: %v", err)
	}
	// Exact-equality assertion (per feedback_assert_exact_not_substring):
	// substring-match on "MyWorkspace" would also pass for the bug case
	// where the helper accidentally returns "MyWorkspace (1)" or
	// "MyWorkspace2".
	if name != "MyWorkspace (2)" {
		t.Fatalf("name = %q, want exactly %q", name, "MyWorkspace (2)")
	}
	if finalTx == nil {
		t.Fatalf("finalTx == nil after successful retry")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestInsertWorkspaceWithNameRetry_NonRetryableErrorPassesThrough
// pins that we do NOT retry on errors we don't recognize. A
// connection drop, an FK violation, a check-constraint failure
// must propagate verbatim — the helper is NOT a generic
// SQL-retry wrapper.
func TestInsertWorkspaceWithNameRetry_NonRetryableErrorPassesThrough(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectBegin()
	connErr := errors.New("connection reset by peer")
	mock.ExpectExec("INSERT INTO workspaces").
		WithArgs("id-1", "MyWorkspace").
		WillReturnError(connErr)

	tx, err := getDBHandle(t).BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	name, _, err := insertWorkspaceWithNameRetry(
		context.Background(),
		tx,
		func(ctx context.Context) (*sql.Tx, error) {
			return getDBHandle(t).BeginTx(ctx, nil)
		},
		"MyWorkspace",
		1,
		"INSERT INTO workspaces (id, name) VALUES ($1, $2)",
		[]any{"id-1", "MyWorkspace"},
	)
	if err == nil {
		t.Fatalf("expected error, got nil (name=%q)", name)
	}
	if !errors.Is(err, connErr) && !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("expected connection-reset to propagate, got %v", err)
	}
	if name != "" {
		t.Fatalf("name = %q, want empty on failure", name)
	}
}

// TestInsertWorkspaceWithNameRetry_ExhaustsAfterMaxSuffix pins the
// upper bound: after maxNameSuffix retries the helper returns
// errWorkspaceNameExhausted so the caller maps it to 409 Conflict
// rather than spinning indefinitely.
func TestInsertWorkspaceWithNameRetry_ExhaustsAfterMaxSuffix(t *testing.T) {
	mock := setupTestDB(t)

	// Every attempt collides. Expect maxNameSuffix+1 INSERTs (the
	// initial + maxNameSuffix retries), each followed by a Rollback,
	// and a Begin between rollbacks except the final terminal one.
	mock.ExpectBegin()
	for i := 0; i <= maxNameSuffix; i++ {
		mock.ExpectExec("INSERT INTO workspaces").
			WillReturnError(fakePqUniqueViolation("workspaces_parent_name_uniq"))
		mock.ExpectRollback()
		if i < maxNameSuffix {
			mock.ExpectBegin()
		}
	}

	tx, err := getDBHandle(t).BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	_, finalTx, err := insertWorkspaceWithNameRetry(
		context.Background(),
		tx,
		func(ctx context.Context) (*sql.Tx, error) {
			return getDBHandle(t).BeginTx(ctx, nil)
		},
		"MyWorkspace",
		1,
		"INSERT INTO workspaces (id, name) VALUES ($1, $2)",
		[]any{"id-1", "MyWorkspace"},
	)
	if !errors.Is(err, errWorkspaceNameExhausted) {
		t.Fatalf("err = %v, want errWorkspaceNameExhausted", err)
	}
	if finalTx != nil {
		t.Fatalf("finalTx must be nil on exhaustion (helper already rolled back); got %v", finalTx)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// getDBHandle exposes the package-level db.DB the test infrastructure
// stashes after setupTestDB. Kept as a helper so the test reads as
// the production code does ("BeginTx on the platform's DB") without
// the cross-package import noise.
func getDBHandle(t *testing.T) *sql.DB {
	t.Helper()
	// db.DB is the package-level handle; setupTestDB assigns it to
	// the sqlmock-backed *sql.DB. Use this helper everywhere instead
	// of dereferencing db.DB directly so a future move to a per-test
	// container fixture has one rename surface.
	return db.DB
}
