package handlers

// llm_billing_mode_test.go — tests for the LEGACY-signature resolver
// ResolveLLMBillingMode after internal#718 P2-B + core#2608. The legacy shim
// reads the explicit override first, then the org default, then DERIVES the
// provider from the workspace's stored (runtime, model). The dedicated
// derived-resolver cases live in llm_billing_mode_derived_test.go; this file
// pins the legacy shim's DB-read sequence + that it routes through the derived
// semantics when no org default is present.

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectLegacyShimQueries sets up the DB reads the legacy ResolveLLMBillingMode
// shim makes on a NO-explicit-override path (internal#718 P2-B), in order:
//  1. override read (NULL) — the shim's own precedence-1 check,
//  2. workspaces.runtime read,
//  3. workspace_secrets scan (MODEL + auth-env names),
//  4. override read AGAIN (NULL) — the derived resolver re-checks it so it is a
//     complete, independently-callable SSOT.
//
// model=="" means no MODEL secret row.
// expectShimQueries mocks the query sequence ResolveLLMBillingMode issues: it
// loads the derive inputs (runtime, secrets) then delegates to
// ResolveLLMBillingModeDerived, which reads the override ONCE. Order: runtime →
// secrets → override (single override read — the prior double-read was removed
// when the org-level path was retired 2026-06-12). override="" mocks a NULL
// (absent) override.
func expectShimQueries(m sqlmock.Sqlmock, wsID, runtime, model, override string) {
	m.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow(runtime))
	secretRows := sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"})
	if model != "" {
		secretRows.AddRow("MODEL", []byte(model), 0) // version 0 = plaintext
	}
	m.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1`).
		WithArgs(wsID).
		WillReturnRows(secretRows)
	overrideRow := sqlmock.NewRows([]string{"llm_billing_mode"})
	if override == "" {
		overrideRow.AddRow(nil)
	} else {
		overrideRow.AddRow(override)
	}
	m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(overrideRow)
}

func TestResolveLLMBillingMode_LegacyShimDerives(t *testing.T) {
	withProxyConfigured(t) // SaaS context: default-closed → platform_managed.
	ctx := context.Background()
	const wsID = "11111111-1111-1111-1111-111111111111"

	type want struct {
		mode        string
		source      BillingModeSource
		hasOverride bool
	}
	type tc struct {
		name      string
		setupMock func(m sqlmock.Sqlmock)
		want      want
		wantErr   bool
	}

	cases := []tc{
		{
			// Explicit per-workspace override wins (first precedence). Inputs are
			// loaded first (runtime, secrets) then the override read decides.
			name: "explicit_override_byok_wins",
			setupMock: func(m sqlmock.Sqlmock) {
				expectShimQueries(m, wsID, "claude-code", "anthropic/claude-opus-4-7", LLMBillingModeBYOK)
			},
			want: want{mode: LLMBillingModeBYOK, source: BillingModeSourceWorkspaceOverride, hasOverride: true},
		},
		{
			// No override + vendor model → byok via derive (the SELECTED provider
			// is the signal; no key-presence check).
			name: "vendor_model_derives_byok",
			setupMock: func(m sqlmock.Sqlmock) {
				expectShimQueries(m, wsID, "claude-code", "kimi-for-coding", "")
			},
			want: want{mode: LLMBillingModeBYOK, source: BillingModeSourceDerivedProvider, hasOverride: false},
		},
		{
			// No override + platform-namespaced model → platform_managed via derive.
			name: "platform_model_derives_platform",
			setupMock: func(m sqlmock.Sqlmock) {
				expectShimQueries(m, wsID, "claude-code", "anthropic/claude-opus-4-7", "")
			},
			want: want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceDerivedProvider, hasOverride: false},
		},
		{
			// No override + no model → derived_default → platform_managed (proxy wired).
			name: "no_model_deploy_default_platform",
			setupMock: func(m sqlmock.Sqlmock) {
				expectShimQueries(m, wsID, "claude-code", "", "")
			},
			want: want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceDerivedDefault, hasOverride: false},
		},
		{
			// Garbled override is NOT honored — falls through to derivation. With no
			// model it lands on the deployment default (platform_managed).
			name: "garbled_override_falls_through_to_derive",
			setupMock: func(m sqlmock.Sqlmock) {
				expectShimQueries(m, wsID, "claude-code", "", "byokk")
			},
			want: want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceDerivedDefault, hasOverride: false},
		},
		{
			// DB error on the override read → default-closed + propagated error.
			// (Inputs load first, then the override read errors.)
			name: "override_db_error_default_closed_with_error",
			setupMock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))
				m.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1`).
					WithArgs(wsID).
					WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
				m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
					WithArgs(wsID).
					WillReturnError(errors.New("connection refused"))
			},
			want:    want{mode: LLMBillingModePlatformManaged, source: BillingModeSourceConstantFallback, hasOverride: false},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mock := setupTestDB(t)
			c.setupMock(mock)

			res, err := ResolveLLMBillingMode(ctx, wsID)
			if (err != nil) != c.wantErr {
				t.Fatalf("err: got %v wantErr=%v", err, c.wantErr)
			}
			if res.ResolvedMode != c.want.mode {
				t.Errorf("mode: got %q want %q", res.ResolvedMode, c.want.mode)
			}
			if res.Source != c.want.source {
				t.Errorf("source: got %q want %q", res.Source, c.want.source)
			}
			if (res.WorkspaceOverride != nil) != c.want.hasOverride {
				t.Errorf("hasOverride: got %v want %v", res.WorkspaceOverride != nil, c.want.hasOverride)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sqlmock expectations: %v", err)
			}
		})
	}
}

// TestResolveLLMBillingMode_EmptyWorkspaceID_PlatformDefault: pre-provision
// (no workspace id) defaults closed with no DB read. An org default is still
// honored in this path (it is purely a string decision, no DB needed).
func TestResolveLLMBillingMode_EmptyWorkspaceID_PlatformDefault(t *testing.T) {
	withProxyConfigured(t) // SaaS context.
	ctx := context.Background()
	mock := setupTestDB(t) // no DB read expected
	res, err := ResolveLLMBillingMode(ctx, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Errorf("empty ws id must default platform_managed, got %q", res.ResolvedMode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestResolveLLMBillingMode_ResolvedModeIsAlwaysValid asserts the resolver's
// post-condition: the returned mode is ALWAYS one of the three known enum
// values. The strip gate downstream relies on this so it can switch on
// res.ResolvedMode without a separate is-valid check on every call site.
func TestResolveLLMBillingMode_ResolvedModeIsAlwaysValid(t *testing.T) {
	withProxyConfigured(t) // SaaS context: default-closed → platform_managed.
	ctx := context.Background()
	const wsID = "22222222-2222-2222-2222-222222222222"

	// Garbled override + no derivable model: must still resolve a known enum
	// (platform_managed, default-closed). Query order: runtime, secrets, override
	// (garbled → not honored → derive → no model → deployment default).
	mock := setupTestDB(t)
	expectShimQueries(mock, wsID, "claude-code", "", "totally-bogus")

	res, err := ResolveLLMBillingMode(ctx, wsID)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !isKnownBillingMode(res.ResolvedMode) {
		t.Errorf("post-condition violated: resolved mode %q is not a known enum value", res.ResolvedMode)
	}
	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Errorf("default-closed contract: garbled-override + no-model must resolve platform_managed, got %q", res.ResolvedMode)
	}
}

// TestSetWorkspaceLLMBillingMode_Validation guards the SET path. The CHECK
// constraint at the DB layer is the second line of defense; the route
// handler relies on this function rejecting unknown modes with a clean
// error (so it can map to 400) instead of letting them hit Postgres and
// surfacing as a sql-driver error string.
func TestSetWorkspaceLLMBillingMode_Validation(t *testing.T) {
	ctx := context.Background()
	const wsID = "33333333-3333-3333-3333-333333333333"

	t.Run("rejects_unknown_mode_without_db_call", func(t *testing.T) {
		setupTestDB(t) // mock expects nothing — the function must short-circuit
		if err := SetWorkspaceLLMBillingMode(ctx, wsID, "totally-bogus"); err == nil {
			t.Fatal("expected error for unknown mode, got nil")
		}
	})

	t.Run("rejects_empty_workspace_id", func(t *testing.T) {
		setupTestDB(t)
		if err := SetWorkspaceLLMBillingMode(ctx, "", LLMBillingModeBYOK); err == nil {
			t.Fatal("expected error for empty workspace id, got nil")
		}
	})

	t.Run("clear_uses_NULL_update", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectExec(`UPDATE workspaces SET llm_billing_mode = NULL WHERE id = \$1`).
			WithArgs(wsID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := SetWorkspaceLLMBillingMode(ctx, wsID, ""); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("set_byok_uses_value_update", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectExec(`UPDATE workspaces SET llm_billing_mode = \$1 WHERE id = \$2`).
			WithArgs(LLMBillingModeBYOK, wsID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := SetWorkspaceLLMBillingMode(ctx, wsID, LLMBillingModeBYOK); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}
