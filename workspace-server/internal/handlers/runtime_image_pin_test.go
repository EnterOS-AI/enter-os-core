package handlers

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// TestResolveRuntimeImage_NoPin: lookup returns sql.ErrNoRows (the steady-
// state when an operator hasn't pinned this runtime). resolveRuntimeImage
// returns "" so the caller falls back to RuntimeImages[runtime] (legacy
// :latest behavior). This is the expected hot path until digest pinning
// is opted into per runtime.
func TestResolveRuntimeImage_NoPin(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mockDB.Close()
	prev := db.DB
	db.DB = mockDB
	defer func() { db.DB = prev }()

	mock.ExpectQuery(`SELECT digest FROM runtime_image_pins WHERE template_name = \$1`).
		WithArgs("claude-code").
		WillReturnError(sql.ErrNoRows)

	got := resolveRuntimeImage(context.Background(), "claude-code")
	if got != "" {
		t.Errorf("expected empty (no pin = fallback), got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestResolveRuntimeImage_DBError: an unexpected DB failure (transient
// network blip, table missing post-rollback, etc.) must NOT block the
// provision — log + fall through to the legacy :latest path. This is
// the availability-over-pinning policy spelled out in resolveRuntimeImage's
// doc comment.
func TestResolveRuntimeImage_DBError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mockDB.Close()
	prev := db.DB
	db.DB = mockDB
	defer func() { db.DB = prev }()

	mock.ExpectQuery(`SELECT digest FROM runtime_image_pins WHERE template_name = \$1`).
		WithArgs("claude-code").
		WillReturnError(sqlmock.ErrCancelled)

	got := resolveRuntimeImage(context.Background(), "claude-code")
	if got != "" {
		t.Errorf("expected empty on DB error (fallback), got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestResolveRuntimeImage_WithPin returns image@sha256:<digest> when row exists.
func TestResolveRuntimeImage_WithPin(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mockDB.Close()
	prev := db.DB
	db.DB = mockDB
	defer func() { db.DB = prev }()

	digest := "sha256:3d6761a97ed07d7d33cfc19a8fbab81175d9d9179618d493dbc00c5f7ef076a3"
	mock.ExpectQuery(`SELECT digest FROM runtime_image_pins WHERE template_name = \$1`).
		WithArgs("claude-code").
		WillReturnRows(sqlmock.NewRows([]string{"digest"}).AddRow(digest))

	got := resolveRuntimeImage(context.Background(), "claude-code")
	if !strings.HasSuffix(got, "@"+digest) {
		t.Errorf("expected suffix @%s, got %q", digest, got)
	}
	if !strings.HasPrefix(got, "ghcr.io/molecule-ai/workspace-template-claude-code") {
		t.Errorf("expected GHCR prefix preserved, got %q", got)
	}
	if strings.Contains(got, ":latest") {
		t.Errorf("expected :latest stripped, got %q", got)
	}
}

// TestResolveRuntimeImage_EmptyRuntime short-circuits to "" without DB.
func TestResolveRuntimeImage_EmptyRuntime(t *testing.T) {
	got := resolveRuntimeImage(context.Background(), "")
	if got != "" {
		t.Errorf("expected empty for empty runtime, got %q", got)
	}
}

// TestResolveRuntimeImage_UnknownRuntime returns "" without DB lookup.
func TestResolveRuntimeImage_UnknownRuntime(t *testing.T) {
	got := resolveRuntimeImage(context.Background(), "no-such-runtime")
	if got != "" {
		t.Errorf("expected empty for unknown runtime, got %q", got)
	}
}

// TestResolveRuntimeImage_LocalOverride: when WORKSPACE_IMAGE_LOCAL_OVERRIDE
// is set, the pin lookup is short-circuited even with a row present —
// devs rebuild images locally and want the floating tag to resolve to
// their fresh build, not a remote-pinned digest.
func TestResolveRuntimeImage_LocalOverride(t *testing.T) {
	t.Setenv("WORKSPACE_IMAGE_LOCAL_OVERRIDE", "1")

	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mockDB.Close()
	prev := db.DB
	db.DB = mockDB
	defer func() { db.DB = prev }()

	// No expectation set — if resolveRuntimeImage queries the DB despite
	// the override, sqlmock fails the test via ExpectationsWereMet.
	got := resolveRuntimeImage(context.Background(), "claude-code")
	if got != "" {
		t.Errorf("expected empty under WORKSPACE_IMAGE_LOCAL_OVERRIDE=1, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB queried despite override: %v", err)
	}
}
