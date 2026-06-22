package handlers

// workspace_provision_platform_boot_test.go — the deterministic, SSOT-driven
// regression suite for the class of bug behind the moonshot/kimi
// "canvas-created claude-code workspace boots NOT_CONFIGURED" production
// incident (RFC#340 Fix A #2187, canvas Fix C #2188).
//
// THE BUG (what shipped to prod):
//   A claude-code workspace created via the canvas with provider=Platform +
//   model="moonshot/kimi-k2.6" booted NOT_CONFIGURED. Unit tests passed; the
//   REAL boot path was broken. ensureDefaultConfig generated a config.yaml that
//   carried NO derived `provider:` key, so the cp#329 config-bundle the adapter
//   actually reads left molecule-runtime config.py to slash-split the model id
//   "moonshot/kimi-k2.6" -> provider="moonshot", which is NOT in the providers
//   registry -> NOT_CONFIGURED.
//
// THE FIX A INVARIANT (this file pins it, and pins it for the WHOLE class):
//   ensureDefaultConfig MUST stamp the manifest-derived provider into the
//   generated config.yaml — at BOTH the top level and under runtime_config —
//   for every (runtime, model) the providers SSOT maps to a platform provider.
//   The single-combo pin (TestEnsureDefaultConfig_StampsDerivedProvider in
//   workspace_provision_test.go) proves the headline case. THIS file closes the
//   gap that single pin leaves: it is PARAMETRIZED OVER THE SSOT, so when a NEW
//   platform model is added to providers.yaml for claude-code (or any runtime
//   with a platform arm), the new id is automatically covered — a future
//   platform model that fails to derive `provider: platform` fails THIS test at
//   build time, before it can ship a NOT_CONFIGURED boot.
//
// WHY SSOT-DRIVEN AND NOT A HAND-MAINTAINED LIST:
//   The original bug was a divergence between "what the canvas offers"
//   (providers.yaml platform arm) and "what the config generator stamps". A
//   hardcoded test model list would itself drift from the SSOT and re-open the
//   same divergence gap. By enumerating the platform model set directly from the
//   loaded providers.Manifest (the SAME manifest ensureDefaultConfig's
//   deriveDefaultConfigProvider resolves against), this test cannot fall behind
//   the offered set: add a platform model, get a test case for free; the test
//   only passes if the generator actually stamps it.
//
// SCOPE: deterministic, no live infra. The REAL-boot complement (provision a
// staging workspace and assert status=online + a completion returns 200 for the
// SAME combo) is the bash staging harness — see
// tests/e2e/test_staging_full_saas.sh (E2E_LLM_PATH=platform) and the
// e2e-staging-platform-boot job in .gitea/workflows/e2e-staging-saas.yml. That
// asserts the REAL artifact (booted status / completion); THIS asserts the
// deterministic config-generation invariant the real boot depends on.

import (
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/providers"
	"gopkg.in/yaml.v3"
)

// platformModelsForRuntime returns the exact model ids the providers SSOT lists
// under runtime rt's `platform` native provider arm — the set the canvas offers
// as provider=Platform and the set ensureDefaultConfig MUST stamp
// `provider: platform` for. Reads the SAME embedded manifest the config
// generator derives against (providers.LoadManifest), so it can never drift from
// the offered set. Returns nil when the runtime has no platform arm.
func platformModelsForRuntime(t *testing.T, rt string) []string {
	t.Helper()
	m, err := providers.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	native, ok := m.Runtimes[rt]
	if !ok {
		t.Fatalf("providers SSOT has no runtimes entry for %q", rt)
	}
	for _, ref := range native.Providers {
		if ref.Name == "platform" {
			return ref.Models
		}
	}
	return nil
}

// TestEnsureDefaultConfig_StampsProviderForEverySSOTPlatformModel is the
// class-level regression for the moonshot/kimi NOT_CONFIGURED incident. For
// EVERY model the providers SSOT offers under claude-code's platform arm, it
// asserts the generated config.yaml carries the manifest-derived provider at
// both the top level and under runtime_config. This is the Fix A invariant,
// parametrized over the SSOT so a newly-offered platform model cannot ship
// without the stamp (the exact divergence — offered-but-not-stamped — that
// booted "moonshot/kimi-k2.6" into NOT_CONFIGURED).
func TestEnsureDefaultConfig_StampsProviderForEverySSOTPlatformModel(t *testing.T) {
	const runtime = "claude-code"
	platformModels := platformModelsForRuntime(t, runtime)
	if len(platformModels) == 0 {
		t.Fatalf("providers SSOT lists no platform models for runtime %q — the regression matrix would be empty; the SSOT shape changed (this test is the canary)", runtime)
	}
	// Headline sentinel: the exact id that booted NOT_CONFIGURED in prod MUST be
	// in the enumerated set. If a refactor drops it from the platform arm, this
	// test must still cover it explicitly — fail loud rather than silently
	// shrinking the matrix.
	if !containsString(platformModels, "moonshot/kimi-k2.6") {
		t.Fatalf("the headline incident model \"moonshot/kimi-k2.6\" is no longer in the claude-code platform SSOT set (%v) — regression coverage for the original bug would be lost", platformModels)
	}

	for _, model := range platformModels {
		model := model
		t.Run(model, func(t *testing.T) {
			broadcaster := newTestBroadcaster()
			handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

			files, err := handler.ensureDefaultConfig("ws-platform-boot", models.CreateWorkspacePayload{
				Name:    "Platform Boot Agent",
				Tier:    2,
				Runtime: runtime,
				Model:   model,
			})
			if err != nil {
				t.Fatalf("ensureDefaultConfig failed for model %q: %v", model, err)
			}

			raw, ok := files["config.yaml"]
			if !ok {
				t.Fatalf("expected config.yaml in generated files for model %q", model)
			}

			var parsed struct {
				Model         string `yaml:"model"`
				Provider      string `yaml:"provider"`
				RuntimeConfig struct {
					Model    string `yaml:"model"`
					Provider string `yaml:"provider"`
				} `yaml:"runtime_config"`
			}
			if err := yaml.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("generated YAML invalid for model %q: %v\n%s", model, err, raw)
			}

			// The load-bearing invariant: BOTH the top-level and the
			// runtime_config provider must be exactly "platform". An empty or
			// vendor-namespace ("moonshot") value here is the prod NOT_CONFIGURED
			// boot — the adapter would slash-split the model id and look up an
			// unregistered provider.
			if parsed.Provider != "platform" {
				t.Errorf("model %q: top-level provider = %q, want \"platform\" (Fix A invariant — empty/vendor value is the NOT_CONFIGURED boot)\n%s", model, parsed.Provider, raw)
			}
			if parsed.RuntimeConfig.Provider != "platform" {
				t.Errorf("model %q: runtime_config.provider = %q, want \"platform\"\n%s", model, parsed.RuntimeConfig.Provider, raw)
			}
			// Sanity: the config must still render a non-empty model (a config
			// with provider but no model is equally undeployable).
			if parsed.Model == "" {
				t.Errorf("model %q: generated config has empty top-level model\n%s", model, raw)
			}
		})
	}
}

// TestPlatformModelDeriveProvider_SSOTConsistency is the upstream half of the
// same invariant, one layer below ensureDefaultConfig: it asserts the providers
// manifest's DeriveProvider — the resolver deriveDefaultConfigProvider calls —
// maps every SSOT-offered claude-code platform model to a provider whose Name is
// "platform". If DeriveProvider itself regressed (e.g. a model_prefix_match
// change made "moonshot/kimi-k2.6" resolve to the bare "moonshot" entry again),
// this fails closer to the root cause than the config-shape test above, making
// the diagnosis unambiguous: SSOT/derive regression vs config-emission
// regression.
func TestPlatformModelDeriveProvider_SSOTConsistency(t *testing.T) {
	const runtime = "claude-code"
	m, err := providers.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	platformModels := platformModelsForRuntime(t, runtime)
	if len(platformModels) == 0 {
		t.Fatalf("no platform models for %q in SSOT", runtime)
	}
	for _, model := range platformModels {
		model := model
		t.Run(model, func(t *testing.T) {
			// nil availableAuthEnv mirrors deriveDefaultConfigProvider's call at
			// config-generation time (no per-workspace auth context yet).
			p, err := m.DeriveProvider(runtime, model, nil)
			if err != nil {
				t.Fatalf("DeriveProvider(%q, %q): unexpected error %v — an SSOT-offered platform model MUST derive", runtime, model, err)
			}
			if p.Name != "platform" {
				t.Errorf("DeriveProvider(%q, %q).Name = %q, want \"platform\" (this is the exact slash-split-to-vendor regression that booted NOT_CONFIGURED)", runtime, model, p.Name)
			}
		})
	}
}

// containsString is a tiny local membership helper. Kept here (not a shared
// test util) so this regression file is self-contained and can be read top to
// bottom without chasing helpers across the package.
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
