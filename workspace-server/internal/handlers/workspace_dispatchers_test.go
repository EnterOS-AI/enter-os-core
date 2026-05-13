package handlers

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
)

// ==================== resolveDeliveryMode ====================
// Covers workspace_dispatchers.go / registry.go:resolveDeliveryMode

func TestResolveDeliveryMode_PayloadModeWins(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	h := NewRegistryHandler(broadcaster)

	ctx := context.Background()
	for _, mode := range []string{models.DeliveryModePush, models.DeliveryModePoll} {
		got, err := h.resolveDeliveryMode(ctx, "ws-any-id", mode)
		if err != nil {
			t.Errorf("resolveDeliveryMode(payloadMode=%q) unexpected error: %v", mode, err)
		}
		if got != mode {
			t.Errorf("resolveDeliveryMode(payloadMode=%q) = %q, want %q", mode, got, mode)
		}
	}

	// DB must NOT have been queried when payloadMode is set.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations not met: %v", err)
	}
}

func TestResolveDeliveryMode_ExistingDeliveryMode(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	h := NewRegistryHandler(broadcaster)

	// Workspace row has existing delivery_mode = "poll"
	mock.ExpectQuery("SELECT delivery_mode, runtime FROM workspaces").
		WithArgs("ws-poll").
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode", "runtime"}).
			AddRow("poll", "langgraph"))

	ctx := context.Background()
	got, err := h.resolveDeliveryMode(ctx, "ws-poll", "")
	if err != nil {
		t.Errorf("resolveDeliveryMode() unexpected error: %v", err)
	}
	if got != models.DeliveryModePoll {
		t.Errorf("resolveDeliveryMode() = %q, want %q", got, models.DeliveryModePoll)
	}
}

func TestResolveDeliveryMode_ExternalRuntime_DefaultsToPoll(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	h := NewRegistryHandler(broadcaster)

	// Row exists but delivery_mode is NULL; runtime = "external"
	mock.ExpectQuery("SELECT delivery_mode, runtime FROM workspaces").
		WithArgs("ws-external").
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode", "runtime"}).
			AddRow(nil, "external"))

	ctx := context.Background()
	got, err := h.resolveDeliveryMode(ctx, "ws-external", "")
	if err != nil {
		t.Errorf("resolveDeliveryMode() unexpected error: %v", err)
	}
	if got != models.DeliveryModePoll {
		t.Errorf("resolveDeliveryMode() = %q, want %q (external runtime)", got, models.DeliveryModePoll)
	}
}

func TestResolveDeliveryMode_SelfHosted_DefaultsToPush(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	h := NewRegistryHandler(broadcaster)

	// Row exists; delivery_mode is NULL; runtime = "langgraph"
	mock.ExpectQuery("SELECT delivery_mode, runtime FROM workspaces").
		WithArgs("ws-self-hosted").
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode", "runtime"}).
			AddRow(nil, "langgraph"))

	ctx := context.Background()
	got, err := h.resolveDeliveryMode(ctx, "ws-self-hosted", "")
	if err != nil {
		t.Errorf("resolveDeliveryMode() unexpected error: %v", err)
	}
	if got != models.DeliveryModePush {
		t.Errorf("resolveDeliveryMode() = %q, want %q (self-hosted default)", got, models.DeliveryModePush)
	}
}

func TestResolveDeliveryMode_NotFound_DefaultsToPush(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	h := NewRegistryHandler(broadcaster)

	// Row not found → sql.ErrNoRows → default push
	mock.ExpectQuery("SELECT delivery_mode, runtime FROM workspaces").
		WithArgs("ws-nonexistent").
		WillReturnError(sql.ErrNoRows)

	ctx := context.Background()
	got, err := h.resolveDeliveryMode(ctx, "ws-nonexistent", "")
	if err != nil {
		t.Errorf("resolveDeliveryMode() unexpected error on no-rows: %v", err)
	}
	if got != models.DeliveryModePush {
		t.Errorf("resolveDeliveryMode() = %q, want %q (not-found default)", got, models.DeliveryModePush)
	}
}

func TestResolveDeliveryMode_DBError_Propagated(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	h := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT delivery_mode, runtime FROM workspaces").
		WithArgs("ws-error").
		WillReturnError(context.DeadlineExceeded)

	ctx := context.Background()
	_, err := h.resolveDeliveryMode(ctx, "ws-error", "")
	if err == nil {
		t.Errorf("resolveDeliveryMode() expected error, got nil")
	}
}

func TestResolveDeliveryMode_ExistingDeliveryModeEmptyString(t *testing.T) {
	// When the DB returns an empty (non-NULL) string for delivery_mode,
	// it falls through to the runtime check (not the existing.Valid path).
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	h := NewRegistryHandler(broadcaster)

	// delivery_mode is explicitly empty string (not NULL), runtime = "langgraph"
	// → falls through to runtime check → "push" for non-external
	mock.ExpectQuery("SELECT delivery_mode, runtime FROM workspaces").
		WithArgs("ws-empty-mode").
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode", "runtime"}).
			AddRow("", "langgraph"))

	ctx := context.Background()
	got, err := h.resolveDeliveryMode(ctx, "ws-empty-mode", "")
	if err != nil {
		t.Errorf("resolveDeliveryMode() unexpected error: %v", err)
	}
	if got != models.DeliveryModePush {
		t.Errorf("resolveDeliveryMode() = %q, want %q", got, models.DeliveryModePush)
	}
}
