package providers

import (
	"sort"
	"strings"
	"testing"
)

// runtimeNativeProviders is the authoritative per-runtime native provider
// matrix from RFC #340 (CTO correction 2026-05-26): the manifest is
// constrained to what each runtime NATIVELY supports, not a 24-provider
// superset. Provider-level expectations; the model-id-level assertions
// live in TestModelsForRuntime_ModelIDs.
//
// Each runtime also natively supports the `platform` provider (Molecule
// platform-managed LLM: no tenant key, platform owns billing) for the subset
// of its native vendors the proxy can serve — kimi for hermes/openclaw,
// openai for codex, anthropic+kimi+minimax for claude-code.
//
// cp#529 adds NAME-ONLY BYOK arms (zero model ids) to claude-code/hermes/
// openclaw: they add NOTHING to the platform menu (ModelsForRuntime) but wire
// CONFIRMED-NON-PLATFORM providers into the runtime's NATIVE prefix-routing set
// so a matching BYOK id resolves via DeriveProvider. ProvidersForRuntime returns
// the full native arm set (menu + name-only), so the expected sets below include
// them. The platform-shared/denylist providers are NEVER wired into a BYOK arm.
//
//	claude-code -> anthropic (oauth+api), kimi (kimi-coding), minimax, platform
//	               + BYOK name-only: zai, deepseek, xiaomi-mimo
//	hermes      -> kimi (kimi-coding), platform
//	               + BYOK name-only: openrouter, huggingface, ai-gateway,
//	                 opencode-zen, opencode-go, kilocode, custom, nvidia, arcee,
//	                 ollama-cloud, minimax-cn, nousresearch, deepseek, zai,
//	                 xiaomi-mimo, alibaba
//	codex       -> openai (subscription + api), platform   (no BYOK name-only)
//	openclaw    -> kimi (kimi-coding), platform + BYOK name-only: openrouter, custom
var runtimeNativeProviders = map[string][]string{
	"claude-code": {"anthropic-api", "anthropic-oauth", "kimi-coding", "minimax", "platform", "zai", "deepseek", "xiaomi-mimo"},
	"hermes": {"kimi-coding", "platform",
		"openrouter", "huggingface", "ai-gateway", "opencode-zen", "opencode-go",
		"kilocode", "custom", "nvidia", "arcee", "ollama-cloud", "minimax-cn",
		"nousresearch", "deepseek", "zai", "xiaomi-mimo", "alibaba",
		// cp#529 dedicated BYOK-vendor name-only arms (shared-vendor namespaced ids).
		"byok-anthropic", "byok-gemini", "byok-openai", "byok-minimax"},
	// codex's OpenAI BYOK is split across the OAuth subscription arm
	// (openai-subscription) and the direct-key arm (openai-api), mirroring
	// claude-code's anthropic oauth+api split; platform openai via the proxy
	// Responses surface. cp#529 adds the byok-minimax name-only arm so the
	// template's BYOK MiniMax token-plan id (codex-minimax-m2.7) resolves.
	"codex":    {"openai-subscription", "openai-api", "platform", "byok-minimax"},
	"openclaw": {"kimi-coding", "platform", "openrouter", "custom",
		// cp#529 dedicated BYOK-vendor name-only arms (openai:/minimax:/groq:).
		"byok-openai", "byok-minimax", "groq"},
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestProvidersForRuntime_ExactNativeSet asserts ProvidersForRuntime
// returns EXACTLY the native provider set for each runtime — no more
// (over-offer drift), no fewer (under-route). Exact set equality, not
// substring/superset, per feedback_assert_exact_not_substring.
func TestProvidersForRuntime_ExactNativeSet(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}

	for rt, want := range runtimeNativeProviders {
		got, err := m.ProvidersForRuntime(rt)
		if err != nil {
			t.Fatalf("ProvidersForRuntime(%q) error = %v", rt, err)
		}
		var gotNames []string
		for _, p := range got {
			gotNames = append(gotNames, p.Name)
		}
		gotNames = sortedCopy(gotNames)
		wantSorted := sortedCopy(want)
		if len(gotNames) != len(wantSorted) {
			t.Fatalf("ProvidersForRuntime(%q) = %v, want exactly %v", rt, gotNames, wantSorted)
		}
		for i := range wantSorted {
			if gotNames[i] != wantSorted[i] {
				t.Fatalf("ProvidersForRuntime(%q) = %v, want exactly %v", rt, gotNames, wantSorted)
			}
		}
	}
}

// TestModelsForRuntime_ExactModelIDs is the brief's central assertion:
// ModelsForRuntime returns EXACTLY the native model-id set for each
// runtime. Encodes the model IDs extracted from each template config.yaml.
func TestModelsForRuntime_ExactModelIDs(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}

	cases := map[string][]string{
		// claude-code: anthropic (oauth aliases + versioned API ids +
		// platform-namespaced) + kimi (kimi-coding gateway + platform) +
		// minimax (BYOK + platform-namespaced). internal#718 P4 PR-1 added
		// the legacy colon-namespaced BYOK spelling (`vendor:model`) as
		// first-class registry entries — the live workspace-create corpus
		// uses both bare and colon forms across ~44 test files +
		// canvas/ConfigTab default + the openclaw template (precedent).
		"claude-code": {
			// anthropic OAuth aliases (bare + legacy colon-namespaced)
			"sonnet", "opus", "haiku",
			"anthropic:sonnet", "anthropic:opus", "anthropic:haiku",
			// anthropic API versioned (bare + legacy colon-namespaced BYOK)
			"claude-sonnet-4-6", "claude-opus-4-7", "claude-opus-4-8", "claude-haiku-4-5", "claude-sonnet-4-5",
			"anthropic:claude-sonnet-4-6", "anthropic:claude-opus-4-7", "anthropic:claude-opus-4-8",
			"anthropic:claude-haiku-4-5", "anthropic:claude-sonnet-4-5",
			// anthropic via platform proxy (namespaced)
			"anthropic/claude-opus-4-7", "anthropic/claude-opus-4-8", "anthropic/claude-sonnet-4-6",
			// kimi (kimi-coding gateway, bare form only — colon-forms removed
			// because claude-code's adapter cannot strip the moonshot: prefix;
			// openclaw retains them natively, cp#521).
			"kimi-for-coding", "kimi-k2.5", "kimi-k2",
			// kimi via platform proxy
			"moonshot/kimi-k2.6", "moonshot/kimi-k2.5",
			// minimax BYOK (bare form only — colon-forms removed because
			// claude-code's adapter cannot strip the minimax: prefix, cp#521).
			"MiniMax-M2", "MiniMax-M2.7", "MiniMax-M2.7-highspeed", "MiniMax-M3",
			// minimax via platform proxy
			"minimax/MiniMax-M2.7", "minimax/MiniMax-M2.7-highspeed", "minimax/MiniMax-M3",
		},
		// hermes: kimi (BYOK gateway) + platform-managed kimi.
		"hermes": {
			"kimi-coding/kimi-k2",
			"moonshot/kimi-k2.6", "moonshot/kimi-k2.5",
		},
		// codex: openai BYOK + platform-managed openai (served via the proxy
		// Responses surface; codex CLI 0.130+ is Responses-API-only).
		"codex": {
			"gpt-5.5", "gpt-5.4", "gpt-5.4-mini",
			"gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.2",
			"openai/gpt-5.4", "openai/gpt-5.4-mini",
		},
		// openclaw: kimi BYOK (moonshot: prefix -> KIMI_API_KEY ->
		// api.kimi.com/coding gateway) + platform-managed kimi (moonshot/).
		"openclaw": {
			"moonshot:kimi-k2.6", "moonshot:kimi-k2.5",
			"moonshot/kimi-k2.6", "moonshot/kimi-k2.5",
		},
	}

	for rt, want := range cases {
		got, err := m.ModelsForRuntime(rt)
		if err != nil {
			t.Fatalf("ModelsForRuntime(%q) error = %v", rt, err)
		}
		gotSorted := sortedCopy(got)
		wantSorted := sortedCopy(want)
		if len(gotSorted) != len(wantSorted) {
			t.Fatalf("ModelsForRuntime(%q) returned %d ids %v, want %d %v",
				rt, len(gotSorted), gotSorted, len(wantSorted), wantSorted)
		}
		for i := range wantSorted {
			if gotSorted[i] != wantSorted[i] {
				t.Fatalf("ModelsForRuntime(%q) = %v, want exactly %v", rt, gotSorted, wantSorted)
			}
		}
	}
}

// TestModelsForRuntime_UnknownRuntime: an unknown runtime returns an error
// (and an empty slice). Fail-direction proof — a runtime not in the matrix
// must not silently return the whole catalog.
func TestModelsForRuntime_UnknownRuntime(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	got, err := m.ModelsForRuntime("does-not-exist")
	if err == nil {
		t.Errorf("ModelsForRuntime(unknown) expected error, got nil (returned %v)", got)
	}
	if len(got) != 0 {
		t.Errorf("ModelsForRuntime(unknown) expected empty slice, got %v", got)
	}
}

// TestProvidersForRuntime_UnknownRuntime: same fail-closed contract for the
// provider-level accessor.
func TestProvidersForRuntime_UnknownRuntime(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	got, err := m.ProvidersForRuntime("does-not-exist")
	if err == nil {
		t.Errorf("ProvidersForRuntime(unknown) expected error, got nil (returned %v)", got)
	}
	if len(got) != 0 {
		t.Errorf("ProvidersForRuntime(unknown) expected empty slice, got %v", got)
	}
}

// TestNonNativeModelAbsentFromEveryRuntime is the drift-prune proof: a model
// that no runtime natively supports must NOT be returned by ModelsForRuntime
// for ANY runtime. These ids are template-declared drift the RFC prunes:
//   - gemini-2.5-pro (canvas/hermes-only, no native CTO matrix entry)
//   - GLM-4.6 (zai; claude-code template declares it but it's outside the
//     anthropic/kimi/minimax native set)
//   - deepseek-v4-pro (claude-code template declares it; outside native set)
//   - mimo-v2.5-pro (xiaomi; claude-code template declares it; outside set)
//   - openai:gpt-4o (openclaw template declares it; outside the kimi-only set)
func TestNonNativeModelAbsentFromEveryRuntime(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	driftModels := []string{
		"gemini-2.5-pro",
		"GLM-4.6",
		"deepseek-v4-pro",
		"mimo-v2.5-pro",
		"openai:gpt-4o",
		"qwen3-max",
		"nousresearch/hermes-4-70b",
	}
	for rt := range runtimeNativeProviders {
		got, err := m.ModelsForRuntime(rt)
		if err != nil {
			t.Fatalf("ModelsForRuntime(%q) error = %v", rt, err)
		}
		present := make(map[string]bool, len(got))
		for _, id := range got {
			present[id] = true
		}
		for _, drift := range driftModels {
			if present[drift] {
				t.Errorf("runtime %q must NOT offer non-native drift model %q, but it did", rt, drift)
			}
		}
	}
}

// minimalValidManifest is a tiny well-formed manifest used as the base for
// the fail-direction tests below. Each negative test mutates one field and
// asserts parseManifest rejects it — proving the load-time guards are
// load-bearing, not vacuously satisfied by the embedded baseline.
const minimalValidManifest = `
schema_version: 1
providers:
  - name: openai
    display_name: "OpenAI"
    protocol: openai
    auth_mode: anthropic_api
    auth_env: [OPENAI_API_KEY]
    model_prefix_match: "^gpt-"
runtimes:
  codex:
    providers:
      - name: openai
        models: [gpt-5.5]
`

// TestParseManifest_ValidBaseline proves the minimal manifest parses, so the
// negative tests below isolate exactly the field they each mutate.
func TestParseManifest_ValidBaseline(t *testing.T) {
	m, err := parseManifest([]byte(minimalValidManifest))
	if err != nil {
		t.Fatalf("parseManifest(valid) error = %v", err)
	}
	models, err := m.ModelsForRuntime("codex")
	if err != nil || len(models) != 1 || models[0] != "gpt-5.5" {
		t.Fatalf("ModelsForRuntime(codex) = %v, err = %v; want [gpt-5.5]", models, err)
	}
}

// TestParseManifest_NameOnlyArm proves a NAME-ONLY runtime arm (zero model
// ids) is PERMITTED (cp#529) and is additive: it contributes nothing to the
// runtime's platform menu (ModelsForRuntime) yet wires the provider into the
// runtime's NATIVE prefix-routing set so a matching BYOK id resolves via
// DeriveProvider. This is the loader half of the cp#529 routability change.
func TestParseManifest_NameOnlyArm(t *testing.T) {
	const y = `
schema_version: 1
providers:
  - name: openai
    display_name: "OpenAI"
    protocol: openai
    auth_mode: anthropic_api
    auth_env: [OPENAI_API_KEY]
    model_prefix_match: "^gpt-"
  - name: openrouter
    display_name: "OpenRouter"
    protocol: openai
    auth_mode: third_party_anthropic_compat
    auth_env: [OPENROUTER_API_KEY]
    model_prefix_match: "^openrouter[:/]"
runtimes:
  codex:
    providers:
      - name: openai
        models: [gpt-5.5]
      - name: openrouter
`
	m, err := parseManifest([]byte(y))
	if err != nil {
		t.Fatalf("parseManifest(name-only arm) error = %v; want nil (name-only arms are permitted)", err)
	}
	// The name-only arm adds NOTHING to the platform menu.
	models, err := m.ModelsForRuntime("codex")
	if err != nil {
		t.Fatalf("ModelsForRuntime(codex) error = %v", err)
	}
	if len(models) != 1 || models[0] != "gpt-5.5" {
		t.Fatalf("ModelsForRuntime(codex) = %v; want [gpt-5.5] (name-only arm must not add a menu id)", models)
	}
	// …yet a BYOK id matching the name-only arm's prefix now ROUTES.
	p, err := m.DeriveProvider("codex", "openrouter/anthropic/claude-3.5-sonnet", nil)
	if err != nil {
		t.Fatalf("DeriveProvider(codex, openrouter/…) error = %v; want it to resolve via the name-only arm", err)
	}
	if p.Name != "openrouter" {
		t.Fatalf("DeriveProvider resolved to %q; want openrouter", p.Name)
	}
}

// TestParseManifest_FailDirection is the load-bearing-guard proof: each case
// breaks the manifest in one way and asserts the matching error fires. If a
// future edit removes a guard, the corresponding case flips red.
func TestParseManifest_FailDirection(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "unknown provider ref",
			yaml: `
schema_version: 1
providers:
  - {name: openai, display_name: "OpenAI", protocol: openai, auth_mode: anthropic_api, auth_env: [OPENAI_API_KEY], model_prefix_match: "^gpt-"}
runtimes:
  codex:
    providers:
      - {name: typo-provider, models: [gpt-5.5]}
`,
			wantErr: "unknown provider",
		},
		{
			name: "empty native set",
			yaml: `
schema_version: 1
providers:
  - {name: openai, display_name: "OpenAI", protocol: openai, auth_mode: anthropic_api, auth_env: [OPENAI_API_KEY], model_prefix_match: "^gpt-"}
runtimes:
  codex:
    providers: []
`,
			wantErr: "empty native provider set",
		},
		{
			name: "duplicate provider ref",
			yaml: `
schema_version: 1
providers:
  - {name: openai, display_name: "OpenAI", protocol: openai, auth_mode: anthropic_api, auth_env: [OPENAI_API_KEY], model_prefix_match: "^gpt-"}
runtimes:
  codex:
    providers:
      - {name: openai, models: [gpt-5.5]}
      - {name: openai, models: [gpt-5.4]}
`,
			wantErr: "twice",
		},
		{
			name: "no runtimes block",
			yaml: `
schema_version: 1
providers:
  - {name: openai, display_name: "OpenAI", protocol: openai, auth_mode: anthropic_api, auth_env: [OPENAI_API_KEY], model_prefix_match: "^gpt-"}
`,
			wantErr: "no runtimes",
		},
		{
			name: "wrong schema version",
			yaml: `
schema_version: 99
providers:
  - {name: openai, display_name: "OpenAI", protocol: openai, auth_mode: anthropic_api, auth_env: [OPENAI_API_KEY], model_prefix_match: "^gpt-"}
runtimes:
  codex:
    providers:
      - {name: openai, models: [gpt-5.5]}
`,
			wantErr: "schema_version",
		},
		{
			name:    "malformed yaml",
			yaml:    "schema_version: 1\nproviders: [oops: not-a-list",
			wantErr: "parse manifest",
		},
		{
			name: "no providers",
			yaml: `
schema_version: 1
providers: []
runtimes:
  codex:
    providers:
      - {name: openai, models: [gpt-5.5]}
`,
			wantErr: "no providers",
		},
		{
			name: "duplicate provider name",
			yaml: `
schema_version: 1
providers:
  - {name: openai, display_name: "OpenAI", protocol: openai, auth_mode: anthropic_api, auth_env: [OPENAI_API_KEY], model_prefix_match: "^gpt-"}
  - {name: openai, display_name: "OpenAI dup", protocol: openai, auth_mode: anthropic_api, auth_env: [OPENAI_API_KEY], model_prefix_match: "^gpt-"}
runtimes:
  codex:
    providers:
      - {name: openai, models: [gpt-5.5]}
`,
			wantErr: "duplicate provider name",
		},
		{
			name: "uncompilable model_prefix_match",
			yaml: `
schema_version: 1
providers:
  - {name: openai, display_name: "OpenAI", protocol: openai, auth_mode: anthropic_api, auth_env: [OPENAI_API_KEY], model_prefix_match: "([unterminated"}
runtimes:
  codex:
    providers:
      - {name: openai, models: [gpt-5.5]}
`,
			wantErr: "model_prefix_match",
		},
		{
			name: "missing required field (protocol)",
			yaml: `
schema_version: 1
providers:
  - {name: openai, display_name: "OpenAI", auth_mode: anthropic_api, auth_env: [OPENAI_API_KEY], model_prefix_match: "^gpt-"}
runtimes:
  codex:
    providers:
      - {name: openai, models: [gpt-5.5]}
`,
			wantErr: "protocol must be",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseManifest([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("parseManifest(%s) expected error containing %q, got nil", tc.name, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("parseManifest(%s) error = %q, want substring %q", tc.name, err.Error(), tc.wantErr)
			}
		})
	}
}

// TestRuntimes_AllProviderRefsResolve guards manifest integrity: every
// provider name referenced in a runtime's native set must resolve to a real
// provider entry. A typo'd provider ref must fail Load, not silently drop a
// model. (Load-time validation; this asserts the loaded manifest is clean.)
func TestRuntimes_AllProviderRefsResolve(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	known := make(map[string]bool, len(m.Providers))
	for _, p := range m.Providers {
		known[p.Name] = true
	}
	if len(m.Runtimes) == 0 {
		t.Fatal("manifest declares no runtimes")
	}
	for rt, native := range m.Runtimes {
		for _, ref := range native.Providers {
			if !known[ref.Name] {
				t.Errorf("runtime %q references unknown provider %q", rt, ref.Name)
			}
		}
	}
}
