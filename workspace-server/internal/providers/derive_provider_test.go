package providers

import (
	"strings"
	"testing"
)

// TestDeriveProvider_RealManifest exercises DeriveProvider against the
// embedded baseline manifest — the cases the brief (internal#718 P0)
// enumerates. DeriveProvider resolves the SINGLE owning provider for a
// (runtime, model) pair using the runtime's NATIVE set, restricted by:
//  1. exact model-id match (the runtime native ref's Models list is the
//     authoritative disambiguator — CTO 2026-05-27 "disambiguate by exact
//     model id"), then
//  2. model_prefix_match among native providers, then
//  3. auth-env disambiguation when >1 native provider still matches.
//
// It ERRORS on overlap (>=2 unresolved) and on none — never silently picks.
func TestDeriveProvider_RealManifest(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}

	cases := []struct {
		name    string
		runtime string
		model   string
		authEnv []string
		expect  string // provider name DeriveProvider must return
	}{
		// --- kimi serving split (the central P0 data fix) ---------------
		// Platform/proxy path: the moonshot-namespaced id routes to the
		// `platform` provider (proxy -> moonshot upstream) for claude-code.
		// This is the "kimi-k2.6 -> moonshot (proxy)" CTO decision expressed
		// via the platform namespace.
		{"claude-code platform moonshot/kimi-k2.6", "claude-code", "moonshot/kimi-k2.6", []string{"ANTHROPIC_API_KEY"}, "platform"},
		// BYOK gateway path: bare kimi ids route to the kimi-coding gateway
		// (api.kimi.com/coding) for claude-code — "kimi-for-coding ->
		// kimi-coding" CTO decision.
		{"claude-code byok kimi-for-coding", "claude-code", "kimi-for-coding", []string{"KIMI_API_KEY"}, "kimi-coding"},
		{"claude-code byok kimi-k2.5", "claude-code", "kimi-k2.5", []string{"KIMI_API_KEY"}, "kimi-coding"},
		{"claude-code byok kimi-k2", "claude-code", "kimi-k2", []string{"KIMI_API_KEY"}, "kimi-coding"},

		// --- platform-model -> platform (closed set) --------------------
		{"claude-code platform anthropic ns", "claude-code", "anthropic/claude-opus-4-7", []string{"ANTHROPIC_API_KEY"}, "platform"},
		{"codex platform openai ns", "codex", "openai/gpt-5.4", []string{"MOLECULE_LLM_USAGE_TOKEN"}, "platform"},
		{"hermes platform moonshot ns", "hermes", "moonshot/kimi-k2.6", []string{"ANTHROPIC_API_KEY"}, "platform"},

		// --- anthropic alias + authEnv disambiguation (oauth vs api) -----
		// Bare aliases are OAuth-only when the OAuth token is the available
		// auth env (matches canvas env-gating). Versioned ids are the API
		// provider.
		{"claude-code oauth opus", "claude-code", "opus", []string{"CLAUDE_CODE_OAUTH_TOKEN"}, "anthropic-oauth"},
		{"claude-code oauth sonnet", "claude-code", "sonnet", []string{"CLAUDE_CODE_OAUTH_TOKEN"}, "anthropic-oauth"},
		{"claude-code oauth haiku", "claude-code", "haiku", []string{"CLAUDE_CODE_OAUTH_TOKEN"}, "anthropic-oauth"},
		{"claude-code api opus versioned", "claude-code", "claude-opus-4-7", []string{"ANTHROPIC_API_KEY"}, "anthropic-api"},
		{"claude-code api sonnet versioned", "claude-code", "claude-sonnet-4-6", []string{"ANTHROPIC_API_KEY"}, "anthropic-api"},

		// --- other runtimes' native sets --------------------------------
		// codex OpenAI is split across openai-subscription (OAuth — the
		// DEFAULT) + openai-api (direct key), mirroring the anthropic
		// oauth+api split. The codex template/adapter registry uses these
		// SPLIT names, never bare `openai` (the prod "picks provider='openai'
		// but it is not in the providers registry" wedge this fixes). The
		// shared gpt-* ids are exact-listed under BOTH arms and disambiguated
		// by available auth env, defaulting to the first-declared arm
		// (openai-subscription) when no auth context resolves it.
		{"codex byok gpt-5.5 with OPENAI_API_KEY -> api", "codex", "gpt-5.5", []string{"OPENAI_API_KEY"}, "openai-api"},
		{"codex byok gpt-5.5 with CODEX_AUTH_JSON -> subscription", "codex", "gpt-5.5", []string{"CODEX_AUTH_JSON"}, "openai-subscription"},
		{"codex byok gpt-5.5 no auth -> subscription (default)", "codex", "gpt-5.5", nil, "openai-subscription"},
		{"codex byok gpt-5.4-mini no auth -> subscription (default)", "codex", "gpt-5.4-mini", nil, "openai-subscription"},
		{"claude-code minimax", "claude-code", "MiniMax-M2.7", []string{"MINIMAX_API_KEY"}, "minimax"},
		{"openclaw byok colon", "openclaw", "moonshot:kimi-k2.6", []string{"KIMI_API_KEY"}, "kimi-coding"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.DeriveProvider(tc.runtime, tc.model, tc.authEnv)
			if err != nil {
				t.Fatalf("DeriveProvider(%q, %q, %v) error = %v", tc.runtime, tc.model, tc.authEnv, err)
			}
			if got.Name != tc.expect {
				t.Errorf("DeriveProvider(%q, %q, %v) = %q, want %q", tc.runtime, tc.model, tc.authEnv, got.Name, tc.expect)
			}
		})
	}
}

// TestDeriveProvider_UnregisteredErrors: a model no native provider owns
// for the runtime must ERROR (never silently default). This is the
// "only-registered-selectable" invariant — fail-closed.
func TestDeriveProvider_UnregisteredErrors(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	cases := []struct {
		runtime string
		model   string
	}{
		// gpt-* is OpenAI — not in claude-code's native set (no openai arm;
		// the platform-shared openai vendor is never wired into a BYOK runtime).
		{"claude-code", "gpt-5.5"},
		// qwen-* is alibaba — a catalog provider NOT wired into claude-code
		// (cp#529 wires alibaba only into hermes; claude-code's name-only BYOK
		// arms are zai/deepseek/xiaomi-mimo). So it stays unregistered here.
		// (NB: deepseek-* IS now routable on claude-code via the deepseek
		// name-only arm — see the routability tests — so it is no longer a valid
		// "unregistered" example; qwen replaces it.)
		{"claude-code", "qwen-max"},
		// codex is OpenAI-only — a kimi id is unregistered for it.
		{"codex", "kimi-for-coding"},
		// a slug no provider in the manifest matches at all.
		{"claude-code", "totally-made-up-model-xyz"},
	}
	for _, tc := range cases {
		p, err := m.DeriveProvider(tc.runtime, tc.model, nil)
		if err == nil {
			t.Errorf("DeriveProvider(%q, %q) expected unregistered error, got provider %q", tc.runtime, tc.model, p.Name)
		}
		if p.Name != "" {
			t.Errorf("DeriveProvider(%q, %q) on error must return a zero Provider, got %q", tc.runtime, tc.model, p.Name)
		}
	}
}

// TestDeriveProvider_UnknownRuntimeErrors: fail-closed on an unknown
// runtime (never falls through to "all providers").
func TestDeriveProvider_UnknownRuntimeErrors(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	p, err := m.DeriveProvider("does-not-exist", "claude-opus-4-7", nil)
	if err == nil {
		t.Errorf("DeriveProvider(unknown runtime) expected error, got provider %q", p.Name)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "runtime") {
		t.Errorf("DeriveProvider(unknown runtime) error = %q, want it to name the runtime problem", err.Error())
	}
}

// TestDeriveProvider_PlatformIsClosed proves a third-party-style provider
// can never be derived as `platform`. `platform` is a CLOSED core-only set:
// only models a native runtime's `platform` ref lists (vendor-namespaced)
// derive to platform. A BYOK id, even one a runtime natively supports,
// derives to its BYOK provider, never to platform.
func TestDeriveProvider_PlatformIsClosed(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	// kimi-for-coding is a BYOK id natively supported by claude-code; it
	// must derive to kimi-coding (BYOK), NOT platform — even though
	// `platform` is in claude-code's native set.
	got, err := m.DeriveProvider("claude-code", "kimi-for-coding", []string{"KIMI_API_KEY"})
	if err != nil {
		t.Fatalf("DeriveProvider(claude-code, kimi-for-coding) error = %v", err)
	}
	if got.Name == "platform" {
		t.Fatal("BYOK kimi-for-coding must not derive to the closed platform provider")
	}
	if got.Name != "kimi-coding" {
		t.Errorf("DeriveProvider(claude-code, kimi-for-coding) = %q, want kimi-coding", got.Name)
	}
}

// craftedManifest is a tiny well-formed manifest with a DELIBERATE prefix
// overlap between two native providers, used to exercise DeriveProvider's
// overlap-error path and the auth-env disambiguation path without depending
// on the real manifest staying overlap-free (it is, by the load guard).
const craftedOverlapManifest = `
schema_version: 1
providers:
  - name: prov-a
    display_name: "Provider A"
    protocol: openai
    auth_mode: anthropic_api
    auth_env: [A_API_KEY]
    model_prefix_match: "^shared-"
  - name: prov-b
    display_name: "Provider B"
    protocol: openai
    auth_mode: anthropic_api
    auth_env: [B_API_KEY]
    model_prefix_match: "^shared-"
runtimes:
  testrt:
    providers:
      - name: prov-a
        models: [a-only-model]
      - name: prov-b
        models: [b-only-model]
`

// TestDeriveProvider_OverlapErrors proves DeriveProvider ERRORS when >=2
// native providers match the same slug and auth-env cannot disambiguate —
// it never silently picks one. This is the load-time-overlap guard's
// runtime counterpart at derivation time.
func TestDeriveProvider_OverlapErrors(t *testing.T) {
	m, err := parseManifest([]byte(craftedOverlapManifest))
	if err != nil {
		t.Fatalf("parseManifest(crafted) error = %v", err)
	}
	// "shared-x" matches BOTH prov-a and prov-b via prefix; no exact-id
	// resolves it; no auth env is supplied -> unresolved overlap -> error.
	p, err := m.DeriveProvider("testrt", "shared-x", nil)
	if err == nil {
		t.Fatalf("DeriveProvider expected overlap error, got provider %q", p.Name)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "overlap") &&
		!strings.Contains(strings.ToLower(err.Error()), "ambiguous") {
		t.Errorf("overlap error = %q, want it to name overlap/ambiguity", err.Error())
	}
	if p.Name != "" {
		t.Errorf("on overlap error DeriveProvider must return zero Provider, got %q", p.Name)
	}
}

// TestDeriveProvider_AuthEnvDisambiguates proves auth-env breaks an
// otherwise-ambiguous prefix overlap: when two native providers match the
// same slug but exactly one's auth_env intersects the available env set,
// DeriveProvider resolves to that one.
func TestDeriveProvider_AuthEnvDisambiguates(t *testing.T) {
	m, err := parseManifest([]byte(craftedOverlapManifest))
	if err != nil {
		t.Fatalf("parseManifest(crafted) error = %v", err)
	}
	// Only B_API_KEY is available -> the shared prefix resolves to prov-b.
	got, err := m.DeriveProvider("testrt", "shared-x", []string{"B_API_KEY"})
	if err != nil {
		t.Fatalf("DeriveProvider(authEnv=B_API_KEY) error = %v", err)
	}
	if got.Name != "prov-b" {
		t.Errorf("DeriveProvider(authEnv=B_API_KEY) = %q, want prov-b", got.Name)
	}
	// Only A_API_KEY -> prov-a.
	got, err = m.DeriveProvider("testrt", "shared-x", []string{"A_API_KEY"})
	if err != nil {
		t.Fatalf("DeriveProvider(authEnv=A_API_KEY) error = %v", err)
	}
	if got.Name != "prov-a" {
		t.Errorf("DeriveProvider(authEnv=A_API_KEY) = %q, want prov-a", got.Name)
	}
	// Both keys available -> still ambiguous -> error (auth env doesn't
	// narrow to one).
	p, err := m.DeriveProvider("testrt", "shared-x", []string{"A_API_KEY", "B_API_KEY"})
	if err == nil {
		t.Errorf("DeriveProvider(both keys) expected overlap error, got %q", p.Name)
	}
}

// TestDeriveProvider_KimiPrefixFallback proves the kimi serving split holds
// on the PREFIX-FALLBACK path too — not only for exact-listed ids. A bare
// kimi id that is NOT in any runtime's exact Models list (e.g. a new
// kimi-latest the gateway serves but the template hasn't enumerated) must
// still resolve to the kimi-coding gateway for claude-code, NOT error
// "unregistered". This catches the false-overlap data bug: before the YAML
// tightening, kimi-coding's regex was too narrow (coding-suffixed ids only)
// and moonshot's was too broad (claimed bare kimi-k2*), so a bare kimi id
// resolved to NEITHER native provider for claude-code.
func TestDeriveProvider_KimiPrefixFallback(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	for _, model := range []string{"kimi-latest", "kimi-thinking-preview"} {
		got, err := m.DeriveProvider("claude-code", model, []string{"KIMI_API_KEY"})
		if err != nil {
			t.Errorf("DeriveProvider(claude-code, %q) prefix-fallback error = %v; want kimi-coding", model, err)
			continue
		}
		if got.Name != "kimi-coding" {
			t.Errorf("DeriveProvider(claude-code, %q) = %q, want kimi-coding (gateway serves any kimi id)", model, got.Name)
		}
	}
}

// TestDeriveProvider_ExactIdBeatsPrefix proves the exact model-id match in
// the runtime native set is authoritative over a prefix match — the CTO
// "disambiguate by exact model id" rule. A model id listed under provider P
// for runtime R derives to P even if another native provider's prefix would
// also match it.
func TestDeriveProvider_ExactIdBeatsPrefix(t *testing.T) {
	const yaml = `
schema_version: 1
providers:
  - name: gateway
    display_name: "Gateway"
    protocol: anthropic
    auth_mode: third_party_anthropic_compat
    auth_env: [GW_KEY]
    model_prefix_match: "^never-matches-anything$"
  - name: broad
    display_name: "Broad"
    protocol: openai
    auth_mode: anthropic_api
    auth_env: [BROAD_KEY]
    model_prefix_match: "^kimi-"
runtimes:
  rt:
    providers:
      - name: gateway
        models: [kimi-k2.5]
      - name: broad
        models: [kimi-other]
`
	m, err := parseManifest([]byte(yaml))
	if err != nil {
		t.Fatalf("parseManifest error = %v", err)
	}
	// kimi-k2.5 is EXACT-listed under `gateway` for rt, but `broad`'s
	// ^kimi- prefix also matches it. Exact id wins -> gateway.
	got, err := m.DeriveProvider("rt", "kimi-k2.5", nil)
	if err != nil {
		t.Fatalf("DeriveProvider error = %v", err)
	}
	if got.Name != "gateway" {
		t.Errorf("exact-id should beat prefix: got %q, want gateway", got.Name)
	}
}

// TestResolveUpstream_RealManifest exercises the SINGLE runtime-AGNOSTIC
// proxy-upstream resolution (internal#718 P1, CONVERGED) against the embedded
// baseline. ResolveUpstream is the ONE resolution over the EXISTING vendor
// provider entries (no proxy_routing block): it maps a model id's NAMESPACE
// token to the entry whose upstream_vendor equals it, answering "which UPSTREAM
// vendor owns this wire model id" in the proxy's 4-name vocabulary {openai,
// moonshot, anthropic, minimax}, with NO runtime context. The byte-identical
// equivalence guard lives in the handlers package (against the live
// inferLLMProvider oracle); this test pins the resolution's own semantics:
// namespace split, separator order, suffix-stripping, and the
// bare-id-is-vestigial (errors) contract.
func TestResolveUpstream_RealManifest(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	cases := []struct {
		name         string
		model        string
		wantVendor   string
		wantResolved string
		wantProvider string // catalog entry the namespace resolved to
		wantErr      bool
	}{
		// --- namespace split — the LIVE traffic shape (vendor/model + vendor:model)
		// demo SEO's LIVE platform model + sibling — MUST stay on moonshot.
		{"platform moonshot slash", "moonshot/kimi-k2.6", "moonshot", "kimi-k2.6", "moonshot", false},
		{"platform moonshot colon (openclaw)", "moonshot:kimi-k2.6", "moonshot", "kimi-k2.6", "moonshot", false},
		// anthropic namespace resolves to the anthropic-api ENTRY (name != vendor).
		{"platform anthropic ns", "anthropic/claude-opus-4-7", "anthropic", "claude-opus-4-7", "anthropic-api", false},
		// openai namespace resolves to the openai-api ENTRY (name != vendor),
		// mirroring anthropic/ -> anthropic-api: the OAuth subscription arm
		// carries NO upstream_vendor (OAuth never traverses the proxy), so the
		// `openai/` namespace + Responses surface route through openai-api.
		{"platform openai ns", "openai/gpt-5.4", "openai", "gpt-5.4", "openai-api", false},
		{"platform minimax ns", "minimax/MiniMax-M2.7", "minimax", "MiniMax-M2.7", "minimax", false},
		{"openai ns gpt-4o", "openai/gpt-4o", "openai", "gpt-4o", "openai-api", false},
		// --- bare ids are VESTIGIAL at the proxy: ResolveUpstream errors (the
		//     proxy falls back to its legacy switch for these). No live bare traffic.
		{"bare kimi -> err (vestigial, legacy fallback)", "kimi-k2.6", "", "", "", true},
		{"bare claude -> err (vestigial)", "claude-3-5-sonnet", "", "", "", true},
		{"bare minimax -> err (vestigial)", "minimax-m1", "", "", "", true},
		{"bare gpt -> err (vestigial)", "gpt-5.5", "", "", "", true},
		{"alias sonnet -> err (vestigial)", "sonnet", "", "", "", true},
		{"unknown bare id -> err (vestigial)", "totally-made-up-xyz", "", "", "", true},
		// non-allowlisted namespace token ("kimi-coding" is no entry's
		// upstream_vendor) does NOT resolve; the whole id is then bare -> err.
		// (The proxy's legacy fallback routes "kimi-coding/kimi-k2" to moonshot,
		// preserving the prior behavior — proven by the handlers equivalence test.)
		{"kimi-coding/ ns not a vendor -> err (legacy fallback)", "kimi-coding/kimi-k2", "", "", "", true},
		// --- empty -------------------------------------------------------
		{"empty -> err", "", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, err := m.ResolveUpstream(tc.model)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveUpstream(%q) = %+v, want error", tc.model, up)
				}
				if up.Vendor != "" || up.Model != "" || up.Provider.Name != "" {
					t.Errorf("ResolveUpstream(%q) on error must return zero Upstream, got %+v", tc.model, up)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveUpstream(%q) error = %v", tc.model, err)
			}
			if up.Vendor != tc.wantVendor {
				t.Errorf("ResolveUpstream(%q) vendor = %q, want %q", tc.model, up.Vendor, tc.wantVendor)
			}
			if up.Model != tc.wantResolved {
				t.Errorf("ResolveUpstream(%q) model = %q, want %q", tc.model, up.Model, tc.wantResolved)
			}
			if up.Provider.Name != tc.wantProvider {
				t.Errorf("ResolveUpstream(%q) provider = %q, want %q", tc.model, up.Provider.Name, tc.wantProvider)
			}
		})
	}
}

// TestResolveUpstream_SeparatorOrder pins the proxy's "/" then ":" separator
// order: an id containing BOTH must split on "/" first (the proxy's loop
// order), so the "/"-prefix vendor wins.
func TestResolveUpstream_SeparatorOrder(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	// "moonshot/foo:bar" cuts on "/" first -> before="moonshot", after="foo:bar".
	up, err := m.ResolveUpstream("moonshot/foo:bar")
	if err != nil || up.Vendor != "moonshot" || up.Model != "foo:bar" {
		t.Fatalf("separator order: got (%+v, err=%v), want vendor=moonshot model=foo:bar", up, err)
	}
}

// TestResolveUpstream_ResolvesToProviderEntry proves the SINGLE-SOURCE
// invariant of the convergence: ResolveUpstream returns the EXISTING vendor
// provider entry, and that entry carries the upstream base URLs + auth — there
// is no parallel routing data block. The proxy dials the entry's base_url_*;
// the test pins them so a future entry edit that breaks the live upstream is
// caught here, not in production.
func TestResolveUpstream_ResolvesToProviderEntry(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	cases := []struct {
		model              string
		wantProvider       string
		wantBaseURL        string // base_url_template on the resolved entry
		wantBaseURLAnthro  string // base_url_anthropic on the resolved entry
		wantAuthEnvContain string // an auth_env name the entry must carry
	}{
		{"moonshot/kimi-k2.6", "moonshot", "https://api.moonshot.ai/v1", "https://api.moonshot.ai/anthropic/v1", "MOONSHOT_API_KEY"},
		{"anthropic/claude-opus-4-7", "anthropic-api", "https://api.anthropic.com/v1", "https://api.anthropic.com/v1", "ANTHROPIC_API_KEY"},
		{"minimax/MiniMax-M2.7", "minimax", "https://api.minimax.io/v1", "https://api.minimax.io/anthropic/v1", "MINIMAX_API_KEY"},
		{"openai/gpt-5.4", "openai-api", "https://api.openai.com/v1", "", "OPENAI_API_KEY"},
	}
	for _, tc := range cases {
		up, err := m.ResolveUpstream(tc.model)
		if err != nil {
			t.Fatalf("ResolveUpstream(%q) error = %v", tc.model, err)
		}
		if up.Provider.Name != tc.wantProvider {
			t.Errorf("%q: provider = %q, want %q", tc.model, up.Provider.Name, tc.wantProvider)
		}
		if up.Provider.BaseURLTemplate != tc.wantBaseURL {
			t.Errorf("%q: base_url_template = %q, want %q", tc.model, up.Provider.BaseURLTemplate, tc.wantBaseURL)
		}
		if up.Provider.BaseURLAnthropic != tc.wantBaseURLAnthro {
			t.Errorf("%q: base_url_anthropic = %q, want %q", tc.model, up.Provider.BaseURLAnthropic, tc.wantBaseURLAnthro)
		}
		found := false
		for _, e := range up.Provider.AuthEnv {
			if e == tc.wantAuthEnvContain {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q: auth_env %v missing %q", tc.model, up.Provider.AuthEnv, tc.wantAuthEnvContain)
		}
	}
}

// TestParseManifest_RejectsDuplicateUpstreamVendor proves the convergence's
// load-time invariant: two entries cannot claim the same upstream_vendor (the
// namespace token must resolve to exactly one entry). Replaces the prior
// closed-catch-all / vendorless-proxy_routing guards.
func TestParseManifest_RejectsDuplicateUpstreamVendor(t *testing.T) {
	const dupVendor = `
schema_version: 1
providers:
  - name: prov-a
    display_name: "Provider A"
    protocol: openai
    auth_mode: anthropic_api
    auth_env: [A_API_KEY]
    model_prefix_match: "^a-"
    upstream_vendor: shared-vendor
  - name: prov-b
    display_name: "Provider B"
    protocol: openai
    auth_mode: anthropic_api
    auth_env: [B_API_KEY]
    model_prefix_match: "^b-"
    upstream_vendor: shared-vendor
runtimes:
  testrt:
    providers:
      - name: prov-a
        models: [a-only]
`
	_, err := parseManifest([]byte(dupVendor))
	if err == nil {
		t.Fatal("manifest with two entries claiming the same upstream_vendor must fail to load")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "upstream_vendor") &&
		!strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("duplicate-vendor error = %q, want it to name the upstream_vendor uniqueness problem", err.Error())
	}
}

// TestResolveUpstream_OnlyRoutingEntriesCarryVendor documents the data shape:
// in the real manifest, EXACTLY the four upstream entries carry upstream_vendor,
// they are {anthropic, openai, moonshot, minimax}, and each is unique. This is
// the converged single-source-of-truth assertion (was TestProxyRoutingClosedCatchAll).
func TestResolveUpstream_OnlyRoutingEntriesCarryVendor(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	got := map[string]string{} // vendor -> entry name
	for _, p := range m.Providers {
		if p.UpstreamVendor == "" {
			continue
		}
		if prev, dup := got[p.UpstreamVendor]; dup {
			t.Fatalf("upstream_vendor %q claimed by both %q and %q", p.UpstreamVendor, prev, p.Name)
		}
		got[p.UpstreamVendor] = p.Name
	}
	want := map[string]string{
		"anthropic": "anthropic-api",
		// openai's upstream_vendor lives on the openai-api entry (the proxy
		// arm); the openai-subscription OAuth arm carries none — OAuth never
		// traverses the proxy (mirror of anthropic-oauth).
		"openai":   "openai-api",
		"moonshot": "moonshot",
		"minimax":  "minimax",
	}
	if len(got) != len(want) {
		t.Fatalf("upstream_vendor entries = %v, want exactly %v", got, want)
	}
	for v, name := range want {
		if got[v] != name {
			t.Errorf("upstream_vendor %q -> entry %q, want %q", v, got[v], name)
		}
	}
}

// codexTemplateProviderRegistry is the set of provider names the DEPLOYED codex
// workspace template/adapter accepts in its `providers:` registry
// (git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-codex
// config.yaml — the source of truth for the codex adapter's vocabulary). The
// adapter REJECTS any provider name not in this set with the prod error:
//
//	ValueError: codex adapter: workspace config picks provider='openai' but it
//	is not in the providers registry. Known providers: openai-subscription,
//	openai-api
//
// `minimax-token-plan` + `platform` are also in the template but are NOT in the
// codex NATIVE matrix (token-plan is pruned — the vendor's /v1/responses leg
// 404s; platform is core-only billing), so the BYOK arms the SSOT derives must
// be a SUBSET of {openai-subscription, openai-api}. `platform` is the legitimate
// platform-managed exception (it is in both the template and the native set).
var codexTemplateProviderRegistry = map[string]struct{}{
	"openai-subscription": {},
	"openai-api":          {},
	"minimax-token-plan":  {},
	"platform":            {},
}

// TestCodexDerivesOnlyTemplateRegistryProviders is the DRIFT GATE that the
// pre-fix gate MISSED: it caught the claude-code/kimi SSOT↔template divergence
// but NOT codex's (the SSOT derived bare `openai`, which the codex adapter
// rejects). It asserts that for EVERY model the codex runtime natively exposes,
// DeriveProvider resolves to a provider name the deployed codex template's
// `providers:` registry actually accepts — so a future SSOT edit that derives a
// codex-adapter-unknown provider (a bare `openai` regression, a typo'd arm)
// fails RED here instead of wedging codex agents as NOT CONFIGURED in prod.
func TestCodexDerivesOnlyTemplateRegistryProviders(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	models, err := m.ModelsForRuntime("codex")
	if err != nil {
		t.Fatalf("ModelsForRuntime(codex) error = %v", err)
	}
	if len(models) == 0 {
		t.Fatal("codex native model set is empty — nothing to gate")
	}
	// Exercise both auth contexts the codex adapter resolves: the OAuth
	// subscription (CODEX_AUTH_JSON) and the direct key (OPENAI_API_KEY), plus
	// the no-auth default. Every resulting provider name MUST be one the codex
	// template registry accepts (never bare `openai`).
	authContexts := [][]string{
		nil,                          // no auth -> default (subscription)
		{"CODEX_AUTH_JSON"},          // ChatGPT/Codex subscription
		{"OPENAI_API_KEY"},           // direct OpenAI key
		{"MOLECULE_LLM_USAGE_TOKEN"}, // platform-managed
	}
	for _, model := range models {
		for _, authEnv := range authContexts {
			p, derr := m.DeriveProvider("codex", model, authEnv)
			if derr != nil {
				// A platform/-namespaced id requires the platform auth env to
				// disambiguate; an unrelated auth context legitimately can't
				// resolve it. Only a CLEAN derivation must be in-registry.
				continue
			}
			if _, ok := codexTemplateProviderRegistry[p.Name]; !ok {
				t.Errorf("codex model %q (authEnv=%v) derived provider %q, which the codex template registry REJECTS (known: openai-subscription, openai-api, minimax-token-plan, platform) — SSOT↔template drift, the exact prod wedge",
					model, authEnv, p.Name)
			}
		}
	}
	// And pin the load-bearing default explicitly: the bare gpt-* family with
	// no auth context defaults to the OAuth subscription (the codex adapter's
	// resolve-provider precedence #1), never bare `openai`.
	for _, model := range []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.2"} {
		p, derr := m.DeriveProvider("codex", model, nil)
		if derr != nil {
			t.Errorf("codex default derivation for %q errored: %v", model, derr)
			continue
		}
		if p.Name != "openai-subscription" {
			t.Errorf("codex default for %q = %q, want openai-subscription (the OAuth subscription default)", model, p.Name)
		}
	}
}
