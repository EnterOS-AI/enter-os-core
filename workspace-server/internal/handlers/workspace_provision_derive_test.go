package handlers

import (
	"errors"
	"strings"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/providers"
)

// ==================== deriveDefaultConfigProviderFromManifest (#2248 follow-up) ====================

// TestDeriveProvider_UnknownRuntimePassThrough pins requirement #2: unknown /
// federated runtimes that have no first-party provider entry must still
// succeed providerless (derive returns ("", nil)).
func TestDeriveProvider_UnknownRuntimePassThrough(t *testing.T) {
	manifest := &providers.Manifest{
		Runtimes: map[string]providers.RuntimeNativeSet{
			"claude-code": {
				Providers: []providers.RuntimeProviderRef{
					{Name: "anthropic", Models: []string{"sonnet"}},
				},
			},
		},
		Providers: []providers.Provider{
			{Name: "anthropic", ModelPrefixMatch: "^sonnet$"},
		},
	}

	provider, err := deriveDefaultConfigProviderFromManifest(manifest, "federated-custom", "some-model")
	if err != nil {
		t.Fatalf("unknown runtime must pass-through, not error: %v", err)
	}
	if provider != "" {
		t.Errorf("unknown runtime must return empty provider, got %q", provider)
	}
}

// TestDeriveProvider_DeriveMissPassThrough pins today's behavior: a model the
// runtime does NOT natively own is a derive miss and must return ("", nil)
// so the caller omits the provider field.
func TestDeriveProvider_DeriveMissPassThrough(t *testing.T) {
	manifest := &providers.Manifest{
		Runtimes: map[string]providers.RuntimeNativeSet{
			"claude-code": {
				Providers: []providers.RuntimeProviderRef{
					{Name: "anthropic", Models: []string{"sonnet"}},
				},
			},
		},
		Providers: []providers.Provider{
			{Name: "anthropic", ModelPrefixMatch: "^sonnet$"},
		},
	}

	provider, err := deriveDefaultConfigProviderFromManifest(manifest, "claude-code", "gpt-4o")
	if err != nil {
		t.Fatalf("derive miss must pass-through, not error: %v", err)
	}
	if provider != "" {
		t.Errorf("derive miss must return empty provider, got %q", provider)
	}
}

// TestDeriveProvider_KnownModelErrorFailClosed pins requirement #1: when the
// runtime AND model are both registry-known but DeriveProvider still errors
// (ambiguous prefix, overlap, etc.), the error must be propagated so
// provisioning is blocked — silently omitting the provider would generate a
// providerless config that re-derives to the WRONG provider at runtime.
func TestDeriveProvider_KnownModelErrorFailClosed(t *testing.T) {
	// Construct a manifest where TWO providers match the SAME model prefix,
	// causing DeriveProvider to return an ambiguous-match error. The model is
	// NOT in any exact list, but it matches both prefixes — so the runtime
	// DOES "know" the model (it matches native providers) and the error is
	// exceptional → must fail-closed.
	manifest := &providers.Manifest{
		Runtimes: map[string]providers.RuntimeNativeSet{
			"claude-code": {
				Providers: []providers.RuntimeProviderRef{
					{Name: "anthropic-api", Models: []string{"sonnet"}},
					{Name: "openai-sub", Models: []string{"gpt-4"}},
				},
			},
		},
		Providers: []providers.Provider{
			{Name: "anthropic-api", ModelPrefixMatch: "^gpt-"},
			{Name: "openai-sub", ModelPrefixMatch: "^gpt-"},
		},
	}

	provider, err := deriveDefaultConfigProviderFromManifest(manifest, "claude-code", "gpt-4o")
	if err == nil {
		t.Fatal("ambiguous match for known model must fail-closed, got nil error")
	}
	if provider != "" {
		t.Errorf("fail-closed must return empty provider, got %q", provider)
	}
	if !strings.Contains(err.Error(), "derive provider for known runtime/model") {
		t.Errorf("error should signal known-model fail-closed, got: %v", err)
	}
}

// TestDeriveProvider_RegistryLoadErrorFailClosed pins requirement #3:
// when the provider registry itself fails to load (build-time defect, degraded
// disk, corrupted manifest), provisioning must be blocked — do not silently
// generate a providerless config on a degraded registry.
func TestDeriveProvider_RegistryLoadErrorFailClosed(t *testing.T) {
	oldProviderRegistry := providerRegistry
	providerRegistry = func() (*providers.Manifest, error) {
		return nil, errors.New("test registry load failure")
	}
	defer func() { providerRegistry = oldProviderRegistry }()

	provider, err := deriveDefaultConfigProvider("claude-code", "sonnet")
	if err == nil {
		t.Fatal("registry load error must fail-closed, got nil error")
	}
	if provider != "" {
		t.Errorf("fail-closed must return empty provider, got %q", provider)
	}
	if !strings.Contains(err.Error(), "provider registry unavailable") {
		t.Errorf("error should signal registry-unavailable fail-closed, got: %v", err)
	}
}

// TestDeriveProvider_KnownModelSuccess confirms the happy path: a known
// runtime/model that DeriveProvider resolves cleanly returns the provider name.
func TestDeriveProvider_KnownModelSuccess(t *testing.T) {
	manifest := &providers.Manifest{
		Runtimes: map[string]providers.RuntimeNativeSet{
			"claude-code": {
				Providers: []providers.RuntimeProviderRef{
					{Name: "platform", Models: []string{"moonshot/kimi-k2.6"}},
				},
			},
		},
		Providers: []providers.Provider{
			{Name: "platform", ModelPrefixMatch: "^moonshot/"},
		},
	}

	provider, err := deriveDefaultConfigProviderFromManifest(manifest, "claude-code", "moonshot/kimi-k2.6")
	if err != nil {
		t.Fatalf("known model success should not error: %v", err)
	}
	if provider != "platform" {
		t.Errorf("provider = %q, want platform", provider)
	}
}
