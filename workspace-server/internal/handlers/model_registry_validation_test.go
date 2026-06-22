package handlers

// model_registry_validation_test.go — only-registered (runtime, model)
// validation at the create/config API (internal#718 P2-B item 3). Reject a
// (runtime, model) the registry does not recognize for a runtime it DOES know;
// fail OPEN (allow) for a runtime the registry doesn't know yet (federation /
// langgraph/etc. not in the first-party registry) so the existing knownRuntimes
// gate stays authoritative there.
//
// TestValidateDerivedProviderInRegistry (issue #2172) is the provider-side
// companion: once the model-side check passes, confirm the DERIVED provider
// (the one the adapter will resolve at boot) is a known provider in
// providers.yaml. Catches the adk-demo "provider=X not in providers registry"
// class at config-SAVE time instead of letting it wedge the agent at boot.

import (
	"strings"
	"testing"
)

func TestValidateRegisteredModelForRuntime(t *testing.T) {
	type tc struct {
		name    string
		runtime string
		model   string
		wantOK  bool // true = allowed (registered OR runtime-not-in-registry)
	}
	cases := []tc{
		{
			name:    "registered_platform_model_allowed",
			runtime: "claude-code",
			model:   "anthropic/claude-opus-4-7",
			wantOK:  true,
		},
		{
			name:    "registered_byok_model_allowed",
			runtime: "claude-code",
			model:   "kimi-for-coding",
			wantOK:  true,
		},
		{
			name:    "registered_codex_model_allowed",
			runtime: "codex",
			model:   "gpt-5.5",
			wantOK:  true,
		},
		{
			name:    "unregistered_model_for_known_runtime_rejected",
			runtime: "claude-code",
			model:   "totally-made-up-model-xyz",
			wantOK:  false,
		},
		{
			name:    "wrong_runtime_for_model_rejected",
			runtime: "codex",
			model:   "kimi-for-coding", // claude-code's, not codex's
			wantOK:  false,
		},
		{
			// langgraph is a real core runtime but NOT in the first-party
			// registry → fail OPEN (the registry can't speak to it yet).
			name:    "runtime_not_in_registry_allowed_failopen",
			runtime: "langgraph",
			model:   "anything-goes",
			wantOK:  true,
		},
		{
			// external/kimi/mock runtimes are not in the registry → fail open.
			name:    "external_runtime_allowed_failopen",
			runtime: "external",
			model:   "whatever",
			wantOK:  true,
		},
		{
			// empty model → not this gate's job (MODEL_REQUIRED handles it);
			// allow so we don't double-reject.
			name:    "empty_model_allowed_other_gate_owns_it",
			runtime: "claude-code",
			model:   "",
			wantOK:  true,
		},
		// ---- cp#529 routability-aware allow path -------------------------------
		{
			// BYOK passthrough id: NOT on hermes's platform menu, but the
			// openrouter name-only native arm prefix-owns it → DeriveProvider
			// resolves → ALLOWED (no platform billing — openrouter is BYOK).
			name:    "byok_passthrough_routable_now_allowed",
			runtime: "hermes",
			model:   "openrouter/anthropic/claude-3.5-sonnet",
			wantOK:  true,
		},
		{
			// BYOK namespaced vendor id: deepseek's widened ^deepseek[-:/]
			// matches the vendor/ form on a name-only hermes arm → allowed.
			name:    "byok_namespaced_vendor_routable_now_allowed",
			runtime: "hermes",
			model:   "deepseek/deepseek-chat",
			wantOK:  true,
		},
		{
			// claude-code bare GLM- BYOK id: zai name-only arm + (?i)^(glm-|…)
			// matches → DeriveProvider resolves → allowed.
			name:    "claude_code_bare_glm_byok_routable_now_allowed",
			runtime: "claude-code",
			model:   "GLM-4.6",
			wantOK:  true,
		},
		{
			// Genuinely UNROUTABLE id: no native hermes arm prefix-owns bare
			// gpt-4o (the platform-shared openai vendor is NOT wired into hermes
			// — billing guardrail), so DeriveProvider errors → still 422.
			name:    "genuinely_unroutable_still_rejected",
			runtime: "hermes",
			model:   "gpt-4o",
			wantOK:  false,
		},
		{
			// A namespaced vendor id NOW routable on hermes via the dedicated
			// byok-openai provider (cp#529 BYOK-vendor arms): routes with the
			// tenant's OPENAI_API_KEY → BYOK billing, never the platform key.
			name:    "byok_openai_namespaced_routable_now_allowed",
			runtime: "hermes",
			model:   "openai/gpt-4o",
			wantOK:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, _ := validateRegisteredModelForRuntime(c.runtime, c.model)
			if ok != c.wantOK {
				t.Errorf("validateRegisteredModelForRuntime(%q,%q) ok=%v want %v", c.runtime, c.model, ok, c.wantOK)
			}
		})
	}
}

func TestValidateDerivedProviderInRegistry(t *testing.T) {
	type tc struct {
		name    string
		runtime string
		model   string
		wantOK  bool
		// wantReasonContains: a substring the rejection reason must include
		// (skipped for OK cases). Pins the actionable list / derivation pointer
		// so the caller knows which provider was missing and what the valid
		// set looks like — this is the fix that distinguishes the new gate
		// from the boot-time "provider=X not in providers registry" string
		// it replaces.
		wantReasonContains string
	}
	cases := []tc{
		// PASS — every native (runtime, model) in the catalog derives to a
		// provider that IS in the providers list. These are the live corpus
		// entries; the test pins the registry-consistency invariant.
		{
			name:    "claude_code_anthropic_api_native",
			runtime: "claude-code",
			model:   "claude-sonnet-4-6",
			wantOK:  true,
		},
		{
			name:    "claude_code_kimi_coding_native",
			runtime: "claude-code",
			model:   "kimi-for-coding",
			wantOK:  true,
		},
		{
			name:    "claude_code_minimax_native",
			runtime: "claude-code",
			model:   "MiniMax-M2.7",
			wantOK:  true,
		},
		{
			name:    "claude_code_platform_namespaced",
			runtime: "claude-code",
			model:   "moonshot/kimi-k2.6",
			wantOK:  true,
		},
		{
			name:    "codex_openai_subscription_default_arm",
			runtime: "codex",
			model:   "gpt-5.5",
			wantOK:  true,
		},
		{
			name:    "codex_platform_namespaced",
			runtime: "codex",
			model:   "openai/gpt-5.4-mini",
			wantOK:  true,
		},
		{
			name:    "hermes_kimi_coding",
			runtime: "hermes",
			model:   "kimi-coding/kimi-k2",
			wantOK:  true,
		},
		{
			name:    "hermes_platform_namespaced",
			runtime: "hermes",
			model:   "moonshot/kimi-k2.6",
			wantOK:  true,
		},
		{
			name:    "openclaw_kimi_coding",
			runtime: "openclaw",
			model:   "moonshot:kimi-k2.6",
			wantOK:  true,
		},
		// FAIL — model-side validator catches this, but the provider-side
		// gate is called AFTER it in Create and inherits the fail-open
		// contract for "model is not native to runtime" (DeriveProvider
		// errors → allow, letting the model-side response own the message).
		// This is the deliberate "don't double-reject" decision.
		{
			name:    "unregistered_model_pass_through_to_model_side",
			runtime: "claude-code",
			model:   "totally-made-up-model-xyz",
			wantOK:  true, // pass-through: model-side validator owns the rejection
		},
		// Federation contract — mirror of the model-side test above.
		{
			name:    "langgraph_runtime_failopen",
			runtime: "langgraph",
			model:   "anything-goes",
			wantOK:  true,
		},
		{
			name:    "external_runtime_failopen",
			runtime: "external",
			model:   "whatever",
			wantOK:  true,
		},
		// Empty model — MODEL_REQUIRED owns it; allow.
		{
			name:    "empty_model_allowed_other_gate_owns_it",
			runtime: "claude-code",
			model:   "",
			wantOK:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, why := validateDerivedProviderInRegistry(c.runtime, c.model)
			if ok != c.wantOK {
				t.Errorf("validateDerivedProviderInRegistry(%q,%q) ok=%v want %v (reason=%q)",
					c.runtime, c.model, ok, c.wantOK, why)
			}
			if !c.wantOK && c.wantReasonContains != "" && !strings.Contains(why, c.wantReasonContains) {
				t.Errorf("rejection reason missing %q: got %q", c.wantReasonContains, why)
			}
		})
	}
}

// TestRegistryConsistency_AllNativeModelsDeriveToKnownProvider walks every
// (runtime, model) pair in the registry's native model sets and asserts each
// one derives to a provider that IS in the providers list. This is the
// static regression gate the issue calls for ("a CI test fails if any shipped
// demo/template config references an unregistered provider") — generalized
// to the catalog as a whole: if anyone edits providers.yaml such that a
// `runtimes:` block names a provider absent from `providers:`, this test
// fires before the bad config can reach a customer workspace.
//
// By construction this invariant should always hold (DeriveProvider only
// returns a Provider that was looked up by name from `providers:`), so the
// test primarily guards against future federation merges that introduce a
// runtime ref pointing at a contributed provider absent from the core
// catalog — exactly the failure shape the adk-demo Assistant wedge
// belongs to.
func TestRegistryConsistency_AllNativeModelsDeriveToKnownProvider(t *testing.T) {
	m, err := providerRegistry()
	if err != nil || m == nil {
		t.Skipf("providerRegistry unavailable in test env (err=%v); skipping consistency walk", err)
	}
	providerNames := make(map[string]struct{}, len(m.Providers))
	for _, p := range m.Providers {
		providerNames[p.Name] = struct{}{}
	}
	for runtimeName, runtime := range m.Runtimes {
		for _, ref := range runtime.Providers {
			for _, modelID := range ref.Models {
				p, err := m.DeriveProvider(runtimeName, modelID, nil)
				if err != nil {
					t.Errorf("catalog invariant broken: runtime=%q model=%q failed DeriveProvider: %v",
						runtimeName, modelID, err)
					continue
				}
				if _, ok := providerNames[p.Name]; !ok {
					t.Errorf("catalog invariant broken: runtime=%q model=%q derives to provider %q which is not in the providers list (refs=%q)",
						runtimeName, modelID, p.Name, ref.Name)
				}
			}
		}
	}
}
