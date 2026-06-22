package providers

import (
	"testing"
)

// TestLoadParses asserts the embedded manifest parses and is non-empty.
func TestLoadParses(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(ps) == 0 {
		t.Fatal("Load() returned an empty provider slice")
	}
}

// TestRequiredFieldsPopulated asserts every entry has the fields the
// validate invariant requires (name, protocol, auth_mode, auth_env,
// display_name, model_prefix_match), and that protocol is one of the
// two legal wire formats.
func TestRequiredFieldsPopulated(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, p := range ps {
		if p.Name == "" {
			t.Errorf("provider with display_name %q has empty name", p.DisplayName)
		}
		if p.DisplayName == "" {
			t.Errorf("provider %q has empty display_name", p.Name)
		}
		if p.AuthMode == "" {
			t.Errorf("provider %q has empty auth_mode", p.Name)
		}
		if len(p.AuthEnv) == 0 {
			t.Errorf("provider %q has empty auth_env", p.Name)
		}
		if p.ModelPrefixMatch == "" {
			t.Errorf("provider %q has empty model_prefix_match", p.Name)
		}
		switch p.Protocol {
		case ProtocolOpenAI, ProtocolAnthropic:
		default:
			t.Errorf("provider %q has invalid protocol %q", p.Name, p.Protocol)
		}
	}
}

// TestUniqueNames asserts provider names are unique (Load enforces this;
// this test guards the manifest data itself).
func TestUniqueNames(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	seen := make(map[string]bool, len(ps))
	for _, p := range ps {
		if seen[p.Name] {
			t.Errorf("duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true
	}
}

// providerByName is a test helper.
func providerByName(t *testing.T, ps []Provider, name string) Provider {
	t.Helper()
	for _, p := range ps {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("provider %q not found in manifest", name)
	return Provider{}
}

// TestMatchesModel maps representative slugs from each source (proxy
// prefixes, canvas BARE_VENDOR_PATTERNS, adapter model_prefixes, DB
// catalog ids) to the provider that should own them.
func TestMatchesModel(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	cases := []struct {
		slug   string
		expect string // provider name that must match
	}{
		// Moonshot vs Kimi-coding — corrected serving split (internal#718
		// P0, CTO 2026-05-27, empirically verified): the BYOK api.kimi.com/
		// coding gateway owns the BARE kimi-* ids; the moonshot endpoint owns
		// the moonshot-namespaced/prefixed ids. Bare kimi-k2.6 / kimi-k2.5 /
		// kimi-for-coding therefore belong to kimi-coding; only the explicit
		// moonshot/ (proxy/platform) and moonshot- (bare moonshot model)
		// prefixes belong to moonshot.
		{"kimi-k2.6", "kimi-coding"},
		{"kimi-k2.5", "kimi-coding"},
		{"kimi-latest", "kimi-coding"},
		{"moonshot/kimi-k2.6", "moonshot"},
		{"moonshot-v1-128k", "moonshot"},
		// Anthropic — proxy "claude"->anthropic + DB claude-* + canvas /^claude-/.
		{"claude-sonnet-4-6", "anthropic-api"},
		{"claude-opus-4-7", "anthropic-api"},
		{"claude-haiku-4-5-20251001", "anthropic-api"},
		// Anthropic OAuth aliases.
		{"sonnet", "anthropic-oauth"},
		{"opus", "anthropic-oauth"},
		{"haiku", "anthropic-oauth"},
		// MiniMax — DB MiniMax-M2.7 (mixed case) + canvas /^MiniMax-/.
		{"MiniMax-M2.7", "minimax"},
		{"MiniMax-M2", "minimax"},
		{"minimax-m2.5", "minimax"},
		// OpenAI — the bare gpt-* family is owned by the codex DEFAULT arm
		// openai-subscription (the OAuth subscription); openai-api uses a
		// disjoint sentinel prefix so the catalog overlap guard stays green
		// (mirror of anthropic-oauth's alias-only regex vs anthropic-api's
		// ^claude). canvas /^gpt-/.
		{"gpt-5.5", "openai-subscription"},
		{"gpt-5.4-mini", "openai-subscription"},
		// Xiaomi MiMo — adapter mimo- + canvas /^mimo-/.
		{"mimo-v2.5-pro", "xiaomi-mimo"},
		// Z.ai GLM — adapter glm- + canvas /^GLM-/ (mixed case).
		{"GLM-4.6", "zai"},
		{"glm-4.5", "zai"},
		// DeepSeek.
		{"deepseek-v4-pro", "deepseek"},
		// Kimi coding-tuned gateway (distinct from moonshot).
		{"kimi-for-coding", "kimi-coding"},
		// Canvas-only slash-prefixed vendors.
		{"openrouter/anthropic/claude-3.5", "openrouter"},
		{"huggingface/mistralai/Mistral-7B", "huggingface"},
		{"custom/my-local-model", "custom"},
		{"gemini-2.5-pro", "google"},
		{"qwen-3-max", "alibaba"},
		{"nousresearch/hermes-4-70b", "nousresearch"},
	}

	for _, tc := range cases {
		p := providerByName(t, ps, tc.expect)
		if !p.MatchesModel(tc.slug) {
			t.Errorf("slug %q: expected provider %q to match, but it did not (regex %q)", tc.slug, tc.expect, p.ModelPrefixMatch)
		}
	}
}

// TestNoAmbiguousModelMatch is the RFC §8.5 overlap guard: no two
// providers may claim the same representative slug. A bad regex that
// over-broadly matches another vendor's ids breaks routing across three
// runtimes, so we catch overlap at PR-1 load time.
func TestNoAmbiguousModelMatch(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Representative slug corpus spanning every source. Each slug must be
	// claimed by exactly one provider.
	corpus := []string{
		"kimi-k2.6", "kimi-k2.5", "moonshot-v1-128k", "moonshot/kimi-k2.6",
		"claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5-20251001",
		"sonnet", "opus", "haiku",
		"MiniMax-M2.7", "MiniMax-M2", "minimax-m2.5", "MiniMax-M2.7-highspeed",
		"gpt-5.5", "gpt-5.4", "gpt-5.4-mini",
		"mimo-v2.5-pro", "mimo-v2-flash",
		"GLM-4.6", "glm-4.5",
		"deepseek-v4-pro", "deepseek-v4-flash",
		"kimi-for-coding",
		"openrouter/x", "huggingface/y", "custom/z",
		"gemini-2.5-pro", "qwen-3-max", "nousresearch/hermes-4-70b",
		"ai-gateway/m", "opencode-zen/m", "opencode-go/m", "kilocode/m",
		"minimax-cn/m2", "ollama-cloud/m", "ollama/llama4", "nvidia/m", "arcee/m",
		"platform/anything",
	}

	for _, slug := range corpus {
		var matched []string
		for _, p := range ps {
			if p.MatchesModel(slug) {
				matched = append(matched, p.Name)
			}
		}
		if len(matched) > 1 {
			t.Errorf("slug %q ambiguously matched %d providers: %v", slug, len(matched), matched)
		}
	}
}

// TestMatchesModelZeroValue exercises the lazy on-demand compile path of
// a Provider not produced by Load.
func TestMatchesModelZeroValue(t *testing.T) {
	p := Provider{ModelPrefixMatch: "^claude-"}
	if !p.MatchesModel("claude-opus-4-7") {
		t.Error("zero-value Provider should match claude-opus-4-7")
	}
	if p.MatchesModel("gpt-5.5") {
		t.Error("zero-value Provider should not match gpt-5.5")
	}

	bad := Provider{ModelPrefixMatch: "([unterminated"}
	if bad.MatchesModel("anything") {
		t.Error("Provider with an invalid regex must never match")
	}

	empty := Provider{}
	if empty.MatchesModel("anything") {
		t.Error("Provider with an empty regex must never match")
	}
}

// TestGoogleADKRuntimeRegistered locks the providers.yaml SSOT entry for the
// google-adk runtime (Gemini via Vertex AI, keyless ADC). The runtime picker
// + GET /templates enrichment read this matrix as SSOT; a missing entry
// silently degrades the ADK runtime's model/provider surface. See
// project_canvas_runtime_dropdown_ssot_fix.
func TestGoogleADKRuntimeRegistered(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	models, err := m.ModelsForRuntime("google-adk")
	if err != nil {
		t.Fatalf("ModelsForRuntime(google-adk) error = %v", err)
	}
	hasModel := false
	for _, id := range models {
		if id == "gemini-2.5-pro" {
			hasModel = true
		}
	}
	if !hasModel {
		t.Errorf("google-adk models missing gemini-2.5-pro; got %v", models)
	}
	provs, err := m.ProvidersForRuntime("google-adk")
	if err != nil {
		t.Fatalf("ProvidersForRuntime(google-adk) error = %v", err)
	}
	hasProv := false
	for _, p := range provs {
		if p.Name == "google" {
			hasProv = true
		}
	}
	if !hasProv {
		t.Errorf("google-adk providers missing google vendor; got %d providers", len(provs))
	}
}

// TestVertexProviderRegistered locks the keyless Vertex provider variant in the
// providers.yaml SSOT. google-adk serves platform-managed Gemini via the LLM
// proxy -> Vertex AI with server-side WIF (no on-box key); the registry must
// still model the keyless "vertex" provider (auth_env GOOGLE_APPLICATION_CREDENTIALS,
// ^vertex: namespace) as a first-class entry distinct from the API-key "google"
// vendor, so the proxy can still route/bill any Vertex-upstream request that
// carries a `vertex:` id. The TRANSITIONAL `vertex:` arm on the google-adk
// RUNTIME (the selectable model set) was removed in cp#514 now that templates
// default to `platform:`; the runtime offers only the `platform` + API-key
// `google` arms. A saved `vertex:gemini-*` model still RESOLVES harmlessly via
// this standalone provider (it is just no longer offered as a new selection).
// See project_canvas_runtime_dropdown_ssot_fix + cp#514.
func TestVertexProviderRegistered(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	var vertex *Provider
	for i := range ps {
		if ps[i].Name == "vertex" {
			vertex = &ps[i]
		}
	}
	if vertex == nil {
		t.Fatal("vertex provider not registered in providers.yaml")
	}
	// Keyless: ADC env, not an API key.
	hasADC := false
	for _, e := range vertex.AuthEnv {
		if e == "GOOGLE_APPLICATION_CREDENTIALS" {
			hasADC = true
		}
	}
	if !hasADC {
		t.Errorf("vertex auth_env should be keyless GOOGLE_APPLICATION_CREDENTIALS; got %v", vertex.AuthEnv)
	}
	// Owns the vertex: namespace, NOT ^gemini- (which the API-key google vendor owns).
	if !vertex.MatchesModel("vertex:gemini-2.5-pro") {
		t.Errorf("vertex provider should match vertex:gemini-2.5-pro")
	}
	if vertex.MatchesModel("gemini-2.5-pro") {
		t.Errorf("vertex provider must NOT claim the bare gemini- namespace (owned by google vendor)")
	}

	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	provs, err := m.ProvidersForRuntime("google-adk")
	if err != nil {
		t.Fatalf("ProvidersForRuntime(google-adk) error = %v", err)
	}
	names := map[string]bool{}
	for _, p := range provs {
		names[p.Name] = true
	}
	// cp#514: the transitional `vertex` arm was dropped from the google-adk
	// runtime. The runtime keeps the platform-managed default + the API-key
	// google arm; the standalone `vertex` PROVIDER (asserted above) survives
	// for ^vertex: resolution but is no longer a selectable runtime arm.
	if names["vertex"] {
		t.Errorf("google-adk runtime should NOT offer the transitional vertex arm (removed cp#514); got %v", names)
	}
	if !names["platform"] {
		t.Errorf("google-adk runtime should keep the platform-managed arm; got %v", names)
	}
	if !names["google"] {
		t.Errorf("google-adk runtime should keep the API-key google arm; got %v", names)
	}
	models, _ := m.ModelsForRuntime("google-adk")
	for _, id := range models {
		if id == "vertex:gemini-2.5-pro" {
			t.Errorf("google-adk models should NOT include vertex:gemini-2.5-pro (removed cp#514); got %v", models)
		}
	}
}

// TestPlatformProvider_AuthEnvIsUsageTokenOnly is the SSOT-side regression
// gate for the platform-managed auth_env drift class (issue #2250 — the
// codex template's `platform` provider shipped
// auth_env: [MOLECULE_LLM_USAGE_TOKEN, ANTHROPIC_API_KEY], wrongly
// advertising a vendor key under a platform-managed provider).
//
// The `platform` provider is the closed Molecule proxy arm: the platform
// owns billing and injects MOLECULE_LLM_USAGE_TOKEN, so a tenant supplies
// NO vendor credential. Listing ANTHROPIC_API_KEY (or any other vendor key)
// in its auth_env makes the canvas demand a credential the platform path
// neither needs nor uses, and lets a stray vendor key satisfy the
// "auth present" check on a path that ignores it — exactly the wrong-bill /
// silent-no-op failure mode the BYOK-vs-platform split exists to prevent.
//
// EXACT-equality (not membership): the prior template-side test only
// asserted `"MOLECULE_LLM_USAGE_TOKEN" in auth_env`, which PASSED against
// the buggy two-element list. Pin the WHOLE set so an extra vendor key
// trips the gate. This is the core providers.yaml SSOT; the template
// derives from / must byte-match this set (drift-gated by molecule-ci).
// On core this currently PASSES (auth_env is already clean; the vendor
// key lives in the separate auth_token_env field) — the gate locks that
// in so a future drift onto this SSOT trips CI.
func TestPlatformProvider_AuthEnvIsUsageTokenOnly(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	var platform *Provider
	for i := range ps {
		if ps[i].Name == "platform" {
			platform = &ps[i]
			break
		}
	}
	if platform == nil {
		t.Fatal("platform provider missing from providers.yaml — the closed proxy arm must exist")
	}
	want := []string{"MOLECULE_LLM_USAGE_TOKEN"}
	if len(platform.AuthEnv) != len(want) || platform.AuthEnv[0] != want[0] {
		t.Errorf("platform provider auth_env = %v, want exactly %v — a vendor key under a platform-managed provider is the #2250 drift; auth_token_env (the proxy's internal projection target) is a SEPARATE field and must not leak into auth_env", platform.AuthEnv, want)
	}
}
