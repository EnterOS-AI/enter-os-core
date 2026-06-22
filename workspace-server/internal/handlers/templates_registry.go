package handlers

// templates_registry.go — internal#718 P3: serve the GET /templates selectable
// provider/model list FROM the provider registry (workspace-server/internal/
// providers) instead of from each template's hand-authored config.yaml
// `providers:` / `runtime_config.models` block.
//
// The registry (P2-A synced copy of the canonical CP providers.yaml) is the
// SSOT for "which providers + models does runtime R natively support" and
// "which derived provider owns model M" (DeriveProvider) and "is that provider
// the closed platform set" (IsPlatform). This file projects that into the
// templates payload's registry_backed / registry_providers / registry_models
// fields so the canvas can drop its hardcoded VENDOR_LABELS /
// billingModeForProvider vocabularies (retire-list #4/#5) and physically can't
// render an option the registry didn't serve.
//
// Federation-ready, fail-OPEN: a runtime ABSENT from the registry's runtimes:
// block (external / mock / kimi / a future third-party runtime) yields
// RegistryBacked=false and an empty registry block — the template's own fields
// stay authoritative. No behavior change for non-registry runtimes.
//
// NOTE: this reuses the package-level providerRegistry() accessor +
// LLMBillingModePlatformManaged / LLMBillingModeBYOK constants from
// llm_billing_mode.go (added by P2-B, internal#718 #1972, now on main) — both
// the billing-derivation and this templates projection wrap the same
// providers.LoadManifest() SSOT and the same platform_managed/byok wire
// strings, so there is one accessor + one constant set for the package.

import (
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/providers"
)

// billingModeForRegistryProvider maps a registry Provider to the billing mode
// it implies: platform_managed for the closed core-only platform provider,
// byok for everything else. Keyed off the registry IsPlatform predicate —
// the same one billing/credential emission (llm_billing_mode.go) keys off the
// DERIVED provider — so the canvas shows the true billing source of the
// resolved provider. Returns the same LLMBillingMode* wire strings the Config
// tab's billing-mode switch sends.
func billingModeForRegistryProvider(p providers.Provider) string {
	if p.IsPlatform() {
		return LLMBillingModePlatformManaged
	}
	return LLMBillingModeBYOK
}

// requiredEnvForRegistryProvider derives the env vars the USER must supply for
// a model owned by the resolved provider — the proper-SSOT single fact behind
// the canvas "Missing API Keys" preflight (task #65). The closed platform
// provider injects credentials server-side (the metered proxy) -> nothing
// required; BYOK providers require their auth_env. Derived from IsPlatform +
// AuthEnv so a template can no longer hand-author a required_env that drifts
// from the registry's serving classification.
func requiredEnvForRegistryProvider(p providers.Provider) []string {
	if p.IsPlatform() {
		return nil
	}
	return p.AuthEnv
}

// enrichFromRegistry populates the registry-served fields on a templateSummary
// when its runtime is known to the provider registry. It is a no-op (leaves
// RegistryBacked=false and the registry slices nil) for a runtime the registry
// does not know — the federation/fail-open path.
//
// runtime is the template's already-normalised runtime string (the caller
// strips the "-default" suffix before calling, matching List's existing
// knownRuntimes check).
func enrichFromRegistry(summary *templateSummary, runtime string) {
	m, err := providerRegistry()
	if err != nil || m == nil {
		return // fail open — registry load defect; keep template-served fields.
	}

	provs, err := m.ProvidersForRuntime(runtime)
	if err != nil {
		// Runtime not in the registry runtimes: block (external / mock / kimi
		// / future third-party). Fail open: the template's own fields stay
		// authoritative; no registry annotation.
		return
	}

	// SSOT filter (the BLOCKER): when no Molecule LLM proxy is wired into this
	// process the platform_managed billing path cannot inject a credential, so
	// the closed `platform` provider (and every model that derives to it) is
	// not actually selectable. Drop it AT THE SOURCE so every consumer of the
	// /templates payload (ConfigTab, CreateWorkspaceDialog, MissingKeysModal)
	// respects it — instead of a frontend leaf-filter that each consumer must
	// remember to apply. On SaaS the proxy is configured -> proxyOn=true and
	// this is a no-op, leaving the payload byte-identical to before.
	proxyOn := PlatformManagedProxyConfigured()

	// registry_providers — the runtime's native provider set, in registry
	// declared order, projected to the canvas-facing view.
	views := make([]registryProviderView, 0, len(provs))
	for _, p := range provs {
		if !proxyOn && p.IsPlatform() {
			// Self-host: no proxy -> platform-managed billing is impossible.
			// Hide the platform provider so it can't be offered anywhere.
			continue
		}
		views = append(views, registryProviderView{
			Name:        p.Name,
			DisplayName: p.DisplayName,
			AuthEnv:     p.AuthEnv,
			BillingMode: billingModeForRegistryProvider(p),
			Deprecated:  p.Deprecated,
		})
	}

	// registry_models — the runtime's native model ids, each annotated with
	// the DERIVED owning provider + the billing mode it implies. DeriveProvider
	// is the SSOT for model→provider; we pass nil availableAuthEnv because a
	// template manifest has no per-workspace auth env, and the registry's
	// exact-id mapping resolves every native model id unambiguously (the
	// claude-code kimi split is by exact id, not a shared prefix).
	models, err := m.ModelsForRuntime(runtime)
	if err != nil {
		// ProvidersForRuntime succeeded but ModelsForRuntime did not — should
		// be impossible (both gate on the same Runtimes entry), but fail open
		// rather than serve a half-populated block.
		return
	}
	regModels := make([]modelSpec, 0, len(models))
	for _, id := range models {
		ms := modelSpec{ID: id}
		if derived, derr := m.DeriveProvider(runtime, id, nil); derr == nil {
			if !proxyOn && derived.IsPlatform() {
				// Self-host: this model derives to the platform-managed
				// provider, which is unusable without a proxy. Drop it at the
				// source so it can't leak as a selectable id in any consumer.
				continue
			}
			ms.Provider = derived.Name
			ms.BillingMode = billingModeForRegistryProvider(derived)
			ms.RequiredEnv = requiredEnvForRegistryProvider(derived)
		}
		// If DeriveProvider errors (ambiguous/overlap — a manifest defect the
		// loader's tests pin against), still serve the id without a provider
		// annotation rather than dropping it; the canvas treats an
		// un-annotated registry model as selectable-but-unlabelled.
		regModels = append(regModels, ms)
	}

	summary.RegistryBacked = true
	summary.RegistryProviders = views
	summary.RegistryModels = regModels
}
