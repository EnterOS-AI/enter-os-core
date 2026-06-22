package handlers

// llm_billing_mode_derived_test.go — tests for the DERIVED billing-mode
// resolver (internal#718 P2-B + core#2608). The platform-vs-byok decision
// consults (1) explicit workspace override, (2) org default, and only then
// (3) derives the provider from (runtime, model). The org rung is restored
// above derivation so a SaaS org pinned to platform_managed is not flipped to
// byok for models whose provider is not the closed `platform` provider.
//
// This file pins the explicit BEHAVIOR DELTA the RFC's P2 calls out:
//   - workspace override                                     → wins over everything
//   - org default (platform_managed/byok/disabled)           → wins over derive
//   - platform-derived (or unset → platform default)         → platform_managed
//   - non-platform-derived                                   → byok (when org default is absent)
//   - derive error / unregistered                            → platform_managed (default-closed)

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectOverrideQuery sets up the workspaces.llm_billing_mode override read
// (first precedence). value=="" means NULL (no override).
func expectOverrideQuery(m sqlmock.Sqlmock, wsID, value string) {
	rows := sqlmock.NewRows([]string{"llm_billing_mode"})
	if value == "" {
		rows.AddRow(nil)
	} else {
		rows.AddRow(value)
	}
	m.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(rows)
}

// withProxyConfigured sets the Molecule LLM proxy env (base URL + usage token)
// for the duration of a test so PlatformManagedProxyConfigured() is true — i.e.
// the SaaS context, where the default-closed billing mode is platform_managed.
// Self-host (no proxy env) is covered separately by the *_SelfHost tests.
func withProxyConfigured(t *testing.T) {
	t.Helper()
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://proxy.example/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tok-test")
}

func TestResolveLLMBillingModeDerived_BehaviorDelta(t *testing.T) {
	withProxyConfigured(t) // SaaS context: default-closed → platform_managed.
	ctx := context.Background()
	const wsID = "33333333-3333-3333-3333-333333333333"

	type tc struct {
		name       string
		runtime    string
		model      string
		authEnv    []string
		override   string // "" = NULL override (no explicit operator override)
		wantMode   string
		wantSource BillingModeSource
		wantErr    bool
	}

	cases := []tc{
		{
			// PLATFORM-DERIVED → platform_managed (UNCHANGED). claude-code +
			// a platform-namespaced model id derives to the closed `platform`
			// provider → IsPlatform → platform_managed.
			name:       "platform_derived_keeps_platform_managed_UNCHANGED",
			runtime:    "claude-code",
			model:      "anthropic/claude-opus-4-7",
			override:   "",
			wantMode:   LLMBillingModePlatformManaged,
			wantSource: BillingModeSourceDerivedProvider,
		},
		{
			// NON-PLATFORM-DERIVED → byok (THE FIX). The workspace SELECTED a vendor
			// model (kimi-for-coding → kimi-coding provider, IsPlatform=false) ⇒
			// byok. The PROVIDER CHOICE is the signal — NOT key presence. Reno
			// billing-leak class: pre-P2 this resolved platform_managed.
			name:       "non_platform_derived_resolves_byok_THE_FIX",
			runtime:    "claude-code",
			model:      "kimi-for-coding",
			override:   "",
			wantMode:   LLMBillingModeBYOK,
			wantSource: BillingModeSourceDerivedProvider,
		},
		{
			// NON-PLATFORM vendor on codex: gpt-5.5 → openai (vendor) → byok.
			name:       "non_platform_openai_codex_byok",
			runtime:    "codex",
			model:      "gpt-5.5",
			override:   "",
			wantMode:   LLMBillingModeBYOK,
			wantSource: BillingModeSourceDerivedProvider,
		},
		{
			// PLATFORM-DERIVED on codex: openai/gpt-5.4 is platform-namespaced.
			name:       "platform_derived_codex_platform_managed",
			runtime:    "codex",
			model:      "openai/gpt-5.4",
			override:   "",
			wantMode:   LLMBillingModePlatformManaged,
			wantSource: BillingModeSourceDerivedProvider,
		},
		{
			// UNSET model → platform default (CTO-confirmed "unset → platform
			// default"). No model means nothing to derive; default-closed.
			name:       "unset_model_platform_default",
			runtime:    "claude-code",
			model:      "",
			override:   "",
			wantMode:   LLMBillingModePlatformManaged,
			wantSource: BillingModeSourceDerivedDefault,
		},
		{
			// UNREGISTERED model → derive errors → platform default (default-closed,
			// NOT a silent byok flip that would strip a workspace's creds).
			name:       "unregistered_model_derive_error_platform_default",
			runtime:    "claude-code",
			model:      "totally-made-up-model-xyz",
			override:   "",
			wantMode:   LLMBillingModePlatformManaged,
			wantSource: BillingModeSourceDerivedDefault,
		},
		{
			// UNKNOWN runtime → derive errors → platform default (default-closed).
			name:       "unknown_runtime_platform_default",
			runtime:    "no-such-runtime",
			model:      "claude-opus-4-7",
			override:   "",
			wantMode:   LLMBillingModePlatformManaged,
			wantSource: BillingModeSourceDerivedDefault,
		},
		{
			// EXPLICIT OVERRIDE wins over derive: a non-platform-deriving model
			// kept on platform_managed by an operator override (escape hatch).
			name:       "explicit_override_platform_managed_wins_over_byok_derive",
			runtime:    "claude-code",
			model:      "kimi-for-coding", // would derive byok
			override:   LLMBillingModePlatformManaged,
			wantMode:   LLMBillingModePlatformManaged,
			wantSource: BillingModeSourceWorkspaceOverride,
		},
		{
			// EXPLICIT OVERRIDE byok wins over a platform-deriving model.
			name:       "explicit_override_byok_wins_over_platform_derive",
			runtime:    "claude-code",
			model:      "anthropic/claude-opus-4-7", // would derive platform_managed
			override:   LLMBillingModeBYOK,
			wantMode:   LLMBillingModeBYOK,
			wantSource: BillingModeSourceWorkspaceOverride,
		},
		{
			// EXPLICIT OVERRIDE disabled wins (no-LLM workspace).
			name:       "explicit_override_disabled_wins",
			runtime:    "claude-code",
			model:      "anthropic/claude-opus-4-7",
			override:   LLMBillingModeDisabled,
			wantMode:   LLMBillingModeDisabled,
			wantSource: BillingModeSourceWorkspaceOverride,
		},
		{
			// AUTH-ENV disambiguation: claude-code's anthropic-oauth (alias
			// model "opus") vs anthropic-api both could match a bare alias; with
			// CLAUDE_CODE_OAUTH_TOKEN present it derives anthropic-oauth → byok.
			name:       "auth_env_disambiguates_oauth_byok",
			runtime:    "claude-code",
			model:      "opus",
			authEnv:    []string{"CLAUDE_CODE_OAUTH_TOKEN"},
			override:   "",
			wantMode:   LLMBillingModeBYOK,
			wantSource: BillingModeSourceDerivedProvider,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mock := setupTestDB(t)
			expectOverrideQuery(mock, wsID, c.override)

			res, err := ResolveLLMBillingModeDerived(ctx, wsID, c.runtime, c.model, c.authEnv)
			if (err != nil) != c.wantErr {
				t.Fatalf("err: got %v wantErr=%v", err, c.wantErr)
			}
			if res.ResolvedMode != c.wantMode {
				t.Errorf("mode: got %q want %q", res.ResolvedMode, c.wantMode)
			}
			if res.Source != c.wantSource {
				t.Errorf("source: got %q want %q", res.Source, c.wantSource)
			}
			if !isKnownBillingMode(res.ResolvedMode) {
				t.Errorf("post-condition: resolved mode %q not a known enum", res.ResolvedMode)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sqlmock expectations: %v", err)
			}
		})
	}
}

// TestResolveLLMBillingModeDerived_OverrideDBError_DefaultClosed asserts a DB
// error reading the override column defaults closed to platform_managed and
// propagates the error — never silently flips a workspace off platform creds.
func TestResolveLLMBillingModeDerived_OverrideDBError_DefaultClosed(t *testing.T) {
	// A transient DB error MUST default to platform_managed regardless of proxy
	// config (it propagates an error; it is not the no-proxy decision path).
	withProxyConfigured(t)
	ctx := context.Background()
	const wsID = "44444444-4444-4444-4444-444444444444"

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnError(errors.New("connection refused"))

	res, err := ResolveLLMBillingModeDerived(ctx, wsID, "claude-code", "kimi-for-coding", nil)
	if err == nil {
		t.Fatalf("expected propagated DB error, got nil")
	}
	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Errorf("default-closed: DB error must resolve platform_managed, got %q", res.ResolvedMode)
	}
	if res.Source != BillingModeSourceConstantFallback {
		t.Errorf("source: got %q want %q", res.Source, BillingModeSourceConstantFallback)
	}
}

// TestResolveLLMBillingModeDerived_EmptyWorkspaceID_PlatformDefault asserts the
// pre-provision context (no workspace id, no override read) with NO model to
// derive defaults to platform_managed without a DB query. (With a vendor model,
// the pre-provision path derives byok like any other — that is the SELECTED
// provider speaking; this test pins the no-model deployment default.)
func TestResolveLLMBillingModeDerived_EmptyWorkspaceID_PlatformDefault(t *testing.T) {
	withProxyConfigured(t) // SaaS context.
	ctx := context.Background()
	mock := setupTestDB(t) // no query expected
	res, err := ResolveLLMBillingModeDerived(ctx, "", "claude-code", "", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Errorf("empty workspace id, no model must default platform_managed, got %q", res.ResolvedMode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestResolveLLMBillingModeDerived_SelfHost_DefaultsBYOK asserts the
// environment-aware default: on a SELF-HOSTED stack (no Molecule LLM proxy env
// configured) the default-closed branches resolve to byok instead of
// platform_managed (which is unreachable there). It covers all three derive-
// failure fallbacks: unset model, unregistered model, and the empty-workspace
// pre-provision path. A successfully-DERIVED provider and an explicit override
// are NOT affected by the no-proxy default (decided before the fallback).
func TestResolveLLMBillingModeDerived_SelfHost_DefaultsBYOK(t *testing.T) {
	// Ensure no proxy env leaks in from the host.
	t.Setenv("MOLECULE_LLM_BASE_URL", "")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	ctx := context.Background()
	const wsID = "55555555-5555-5555-5555-555555555555"

	t.Run("unset_model_defaults_byok_on_selfhost", func(t *testing.T) {
		mock := setupTestDB(t)
		expectOverrideQuery(mock, wsID, "") // NULL override
		res, err := ResolveLLMBillingModeDerived(ctx, wsID, "claude-code", "", nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.ResolvedMode != LLMBillingModeBYOK {
			t.Errorf("self-host unset model: got %q want byok", res.ResolvedMode)
		}
		if res.Source != BillingModeSourceDerivedDefault {
			t.Errorf("source: got %q want %q", res.Source, BillingModeSourceDerivedDefault)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock expectations: %v", err)
		}
	})

	t.Run("unregistered_model_defaults_byok_on_selfhost", func(t *testing.T) {
		mock := setupTestDB(t)
		expectOverrideQuery(mock, wsID, "")
		res, err := ResolveLLMBillingModeDerived(ctx, wsID, "claude-code", "totally-made-up-model-xyz", nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.ResolvedMode != LLMBillingModeBYOK {
			t.Errorf("self-host unregistered model: got %q want byok", res.ResolvedMode)
		}
		if res.Source != BillingModeSourceDerivedDefault {
			t.Errorf("source: got %q want %q", res.Source, BillingModeSourceDerivedDefault)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock expectations: %v", err)
		}
	})

	t.Run("empty_workspace_id_defaults_byok_on_selfhost", func(t *testing.T) {
		mock := setupTestDB(t) // no query expected (pre-provision path)
		res, err := ResolveLLMBillingModeDerived(ctx, "", "claude-code", "kimi-for-coding", nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.ResolvedMode != LLMBillingModeBYOK {
			t.Errorf("self-host empty workspace id: got %q want byok", res.ResolvedMode)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock expectations: %v", err)
		}
	})

	t.Run("explicit_platform_override_still_wins_on_selfhost", func(t *testing.T) {
		// An operator override is honored even on self-host (escape hatch); the
		// no-proxy default only governs the derive-failure fallback.
		mock := setupTestDB(t)
		expectOverrideQuery(mock, wsID, LLMBillingModePlatformManaged)
		res, err := ResolveLLMBillingModeDerived(ctx, wsID, "claude-code", "", nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.ResolvedMode != LLMBillingModePlatformManaged {
			t.Errorf("explicit override must win: got %q want platform_managed", res.ResolvedMode)
		}
		if res.Source != BillingModeSourceWorkspaceOverride {
			t.Errorf("source: got %q want %q", res.Source, BillingModeSourceWorkspaceOverride)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock expectations: %v", err)
		}
	})
}

// TestResolveLLMBillingModeDerived_PerWorkspaceNoOrgDefault is the SSOT
// regression after the org-level billing mode was fully removed (CTO
// 2026-06-12). Billing is decided per-workspace ONLY: explicit override →
// derive from model → deployment default-closed. There is NO org default
// anywhere. On SaaS (proxy wired) a keyless vendor-model workspace "defaults to
// platform" via the DEPLOYMENT fact PlatformManagedProxyConfigured(), not via
// an org setting.
func TestResolveLLMBillingModeDerived_PerWorkspaceNoOrgDefault(t *testing.T) {
	withProxyConfigured(t) // SaaS context: PlatformManagedProxyConfigured() == true.
	ctx := context.Background()
	const wsID = "66666666-6666-6666-6666-666666666666"

	t.Run("vendor_model_is_byok_regardless_of_key", func(t *testing.T) {
		// kimi-for-coding → kimi-coding vendor (IsPlatform=false) ⇒ byok. The
		// SELECTED provider is the signal — NO key-presence check (a byok ws with
		// no usable cred fails closed later at provision, which is correct).
		mock := setupTestDB(t)
		expectOverrideQuery(mock, wsID, "") // NULL override
		res, err := ResolveLLMBillingModeDerived(ctx, wsID, "claude-code", "kimi-for-coding", nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.ResolvedMode != LLMBillingModeBYOK {
			t.Errorf("vendor model: got %q want byok", res.ResolvedMode)
		}
		if res.Source != BillingModeSourceDerivedProvider {
			t.Errorf("source: got %q want %q", res.Source, BillingModeSourceDerivedProvider)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock expectations: %v", err)
		}
	})

	t.Run("platform_model_is_platform_managed", func(t *testing.T) {
		mock := setupTestDB(t)
		expectOverrideQuery(mock, wsID, "") // NULL override
		res, err := ResolveLLMBillingModeDerived(ctx, wsID, "claude-code", "anthropic/claude-opus-4-7", nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.ResolvedMode != LLMBillingModePlatformManaged {
			t.Errorf("platform model: got %q want platform_managed", res.ResolvedMode)
		}
		if res.Source != BillingModeSourceDerivedProvider {
			t.Errorf("source: got %q want %q", res.Source, BillingModeSourceDerivedProvider)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock expectations: %v", err)
		}
	})

	t.Run("explicit_workspace_override_wins", func(t *testing.T) {
		mock := setupTestDB(t)
		expectOverrideQuery(mock, wsID, LLMBillingModeBYOK)
		res, err := ResolveLLMBillingModeDerived(ctx, wsID, "claude-code", "anthropic/claude-opus-4-7", nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.ResolvedMode != LLMBillingModeBYOK {
			t.Errorf("workspace override must win: got %q want byok", res.ResolvedMode)
		}
		if res.Source != BillingModeSourceWorkspaceOverride {
			t.Errorf("source: got %q want %q", res.Source, BillingModeSourceWorkspaceOverride)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock expectations: %v", err)
		}
	})
}

