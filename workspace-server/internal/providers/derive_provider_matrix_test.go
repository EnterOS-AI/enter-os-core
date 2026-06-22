package providers

// derive_provider_matrix_test.go — the SSOT-DRIVEN provider-routing matrix
// (internal#718 / coverage-audit hole closure).
//
// GOAL (CTO "e2e covers every supported runtime and provider, no regressions"):
// a KEYLESS, REQUIRED-lane-gateable test that asserts EVERY offered
// (runtime × model/provider arm) in the providers SSOT resolves to the EXACT
// correct provider via DeriveProvider — closing the provider-routing-correctness
// hole for ALL providers + every BYOK arm without needing any LLM key.
//
// WHY THIS IS THE HIGH-LEVERAGE TEST. DeriveProvider(runtime, modelId) +
// ModelPrefixMatch resolve a model id to a provider with NO upstream call — it
// is a pure function of the merged registry. So the ENTIRE offered routing table
// (every (runtime → provider) pair, including hermes's 17 name-only BYOK arms,
// claude-code's zai/deepseek/xiaomi-mimo, openclaw's byok-openai/byok-minimax/
// groq/openrouter/custom, codex's byok-minimax, etc.) is gateable in the
// REQUIRED `CI / all-required` lane with zero secrets. A regression in the
// routing table (wrong provider, dropped arm, bad regex) now reds CI instead of
// shipping silently and wedging a tenant agent at boot.
//
// SELF-MAINTAINING / SSOT-DRIVEN (NOT hardcoded). The matrix is DRIVEN FROM the
// loaded manifest (LoadManifest().Runtimes — the same SSOT production reads),
// not a hand-listed table:
//
//   - EXACT-LISTED arms (a runtime ref with non-empty Models): every model id is
//     iterated straight off the SSOT and its EXPECTED provider is COMPUTED from
//     the SSOT (the native arm(s) that exact-list the id; first-declared wins the
//     "one id, two auth arms" codex/anthropic shape) — so a newly-added model is
//     AUTO-COVERED and a misrouted one fails RED naming the mismatch.
//   - NAME-ONLY arms (a runtime ref with zero Models — pure prefix-routing BYOK):
//     these have no exact id to iterate, so each is probed with a REPRESENTATIVE
//     BYOK id its regex must own (representativeBYOKModel). The matrix REQUIRES a
//     representative for EVERY name-only arm it encounters in the SSOT — so
//     "added a name-only provider arm but forgot to wire routing / supply a
//     sample" fails RED here, keeping the probe set honest as the SSOT grows.
//
// Every asserted (runtime, model) is ALSO checked for registration-validity
// (the validateRegisteredModelForRuntime predicate: on the platform menu OR
// DeriveProvider resolves) so the matrix proves no offered id silently falls
// through to "unregistered/unselectable".

import (
	"sort"
	"testing"
)

// representativeBYOKModel maps a provider NAME to a representative BYOK model id
// that the provider's model_prefix_match MUST own. It is the routing probe for
// NAME-ONLY native arms (refs with zero exact Models — the cp#529 pure-prefix
// BYOK arms). The matrix asserts every name-only arm in the SSOT has an entry
// here AND that DeriveProvider routes the sample id to exactly that provider.
//
// Adding a name-only provider arm to providers.yaml WITHOUT adding a
// representative here fails TestDeriveProviderMatrix_SSOTDriven loudly — that is
// the self-maintaining contract that keeps "new BYOK arm but no routing proof"
// from shipping. The id must be one the provider's regex matches and NO sibling
// native arm of the same runtime also matches (else it is a registry overlap the
// load guard or the auth-env tie-break — not this map — must resolve).
var representativeBYOKModel = map[string]string{
	// hermes passthrough + bare-vendor BYOK arms (all ^name[:/]) ----------
	"openrouter":   "openrouter/anthropic/claude-3.5-sonnet",
	"huggingface":  "huggingface/meta-llama/Llama-3.3-70B",
	"ai-gateway":   "ai-gateway/openai/gpt-4o",
	"opencode-zen": "opencode-zen/some-model",
	"opencode-go":  "opencode-go/some-model",
	"kilocode":     "kilocode/some-model",
	"custom":       "custom/my-endpoint-model",
	"nvidia":       "nvidia/nemotron-4",
	"arcee":        "arcee/coder",
	"ollama-cloud": "ollama-cloud/qwen2.5",
	"minimax-cn":   "minimax-cn/abab6.5",
	"nousresearch": "nousresearch/Hermes-3",
	// claude-code + hermes name-only arms (case-insensitive vendor-prefixed)
	"deepseek":    "deepseek-chat",
	"zai":         "zai:glm-4.6",
	"xiaomi-mimo": "mimo-7b",
	"alibaba":     "qwen-max",
	// dedicated BYOK-vendor arms (hermes/openclaw/codex) ------------------
	"byok-anthropic": "anthropic/claude-opus-4-7",
	"byok-gemini":    "gemini/gemini-2.5-pro",
	"byok-openai":    "openai:gpt-4o",
	"byok-minimax":   "minimax:MiniMax-M2.7",
	"groq":           "groq:llama-3.3-70b",
}

// sortedRuntimeNames returns the manifest's runtime names sorted, for a
// deterministic iteration order (the loaded Runtimes is a map).
func sortedRuntimeNames(m *Manifest) []string {
	out := make([]string, 0, len(m.Runtimes))
	for rt := range m.Runtimes {
		out = append(out, rt)
	}
	sort.Strings(out)
	return out
}

// expectedExactProvider computes, FROM THE SSOT, the provider DeriveProvider
// must resolve a given exact-listed model id to for a runtime — without calling
// DeriveProvider. It mirrors DeriveProvider's exact-id rule (steps 3): the
// native arm(s), in declaration order, that exact-list the id. When exactly one
// arm lists it, that arm is the answer. When MORE THAN ONE lists it (the
// legitimate "one model id, two auth arms" codex gpt-* / claude-code anthropic
// shape), the FIRST-declared arm is the deterministic no-auth default. Returns
// "" if no native arm exact-lists the id (caller treats that as "not an
// exact-listed case").
func expectedExactProvider(native RuntimeNativeSet, model string) string {
	for _, ref := range native.Providers {
		for _, mid := range ref.Models {
			if mid == model {
				// First-declared arm that lists it = DeriveProvider's no-auth
				// answer (single-arm → that arm; multi-arm → first declared).
				return ref.Name
			}
		}
	}
	return ""
}

// isRegisteredForRuntime mirrors handlers.validateRegisteredModelForRuntime's
// allow predicate against the SAME registry (kept here so the matrix lives in
// the providers package, which owns the SSOT and cannot import handlers): a
// (runtime, model) is registration-valid iff it is on the runtime's platform
// menu (ModelsForRuntime) OR DeriveProvider resolves a native provider for it.
// A name-only BYOK id is NOT on the platform menu but IS routable — exactly the
// cp#529 routability-aware OR the handler enforces and the controlplane drift
// checker mirrors.
func isRegisteredForRuntime(m *Manifest, runtime, model string) bool {
	models, err := m.ModelsForRuntime(runtime)
	if err != nil {
		return false // unknown runtime — not registration-valid here.
	}
	for _, mid := range models {
		if mid == model {
			return true
		}
	}
	_, derr := m.DeriveProvider(runtime, model, nil)
	return derr == nil
}

// TestDeriveProviderMatrix_SSOTDriven is the headline coverage test: for EVERY
// runtime in the registry and EVERY model/provider arm offered to it in the
// SSOT, it asserts (a) DeriveProvider resolves to the EXACT expected provider,
// (b) the (runtime, model) is registration-valid, and (c) NO offered id silently
// resolves to the wrong arm or falls through to a default/error. The table is
// DRIVEN FROM LoadManifest().Runtimes (the production SSOT) — not hardcoded — so
// a newly-added provider/model is auto-covered and an unrouteable one fails RED.
func TestDeriveProviderMatrix_SSOTDriven(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}

	var (
		exactPairs   int // (runtime, exact-listed model) assertions
		nameOnlyArms int // (runtime, name-only arm) routing probes
		coveredArms  int // distinct (runtime, provider) arms touched
		coveredProvs = map[string]struct{}{}
		coveredRTs   = map[string]struct{}{}
	)

	for _, rt := range sortedRuntimeNames(m) {
		native := m.Runtimes[rt]
		coveredRTs[rt] = struct{}{}

		for _, ref := range native.Providers {
			coveredArms++
			coveredProvs[ref.Name] = struct{}{}

			if len(ref.Models) == 0 {
				// ---- NAME-ONLY ARM: pure prefix-routing BYOK -----------------
				// No exact id to iterate; probe the provider's regex with a
				// representative BYOK id and assert it routes to THIS arm. The
				// representative MUST exist (self-maintaining contract).
				nameOnlyArms++
				sample, ok := representativeBYOKModel[ref.Name]
				if !ok {
					t.Errorf("name-only arm %q on runtime %q has NO representativeBYOKModel entry — add a sample BYOK id its model_prefix_match owns so its routing is proven (SSOT grew, probe set did not)", ref.Name, rt)
					continue
				}
				t.Run(rt+"/name-only/"+ref.Name, func(t *testing.T) {
					got, derr := m.DeriveProvider(rt, sample, nil)
					if derr != nil {
						t.Fatalf("DeriveProvider(%q, %q [name-only arm %q sample]) errored: %v — the arm is offered but its sample id does not route (bad/missing regex, or a sibling arm shadows it)", rt, sample, ref.Name, derr)
					}
					if got.Name != ref.Name {
						t.Errorf("DeriveProvider(%q, %q) = %q, want %q (the name-only arm the sample id was chosen to probe) — routing table misroutes this BYOK arm", rt, sample, got.Name, ref.Name)
					}
					if !isRegisteredForRuntime(m, rt, sample) {
						t.Errorf("name-only arm probe (%q, %q) is not registration-valid — a routable BYOK id must pass validateRegisteredModelForRuntime", rt, sample)
					}
				})
				continue
			}

			// ---- EXACT-LISTED ARM: iterate every model id off the SSOT -------
			for _, model := range ref.Models {
				model := model
				want := expectedExactProvider(native, model)
				// want is computed from the SSOT and equals THIS ref.Name unless
				// an earlier-declared arm also exact-lists the same id (the
				// "one id, two auth arms" codex/anthropic shape), in which case
				// the first-declared arm is the no-auth default. Either way the
				// computed `want` is what DeriveProvider(no-auth) must return.
				t.Run(rt+"/"+ref.Name+"/"+model, func(t *testing.T) {
					exactPairs++
					got, derr := m.DeriveProvider(rt, model, nil)
					if derr != nil {
						t.Fatalf("DeriveProvider(%q, %q) errored: %v — an OFFERED exact-listed id must resolve, never fall through", rt, model, derr)
					}
					if got.Name != want {
						t.Errorf("DeriveProvider(%q, %q) = %q, want %q — offered id resolves to the WRONG arm (routing regression)", rt, model, got.Name, want)
					}
					if !isRegisteredForRuntime(m, rt, model) {
						t.Errorf("offered exact-listed id (%q, %q) is not registration-valid (validateRegisteredModelForRuntime would reject it) — SSOT lists it but it is unrouteable/unregistered", rt, model)
					}
				})
			}
		}
	}

	t.Logf("MATRIX COVERAGE: %d runtimes, %d (runtime×provider) arms (%d distinct providers), %d exact-listed (runtime×model) assertions, %d name-only BYOK routing probes",
		len(coveredRTs), coveredArms, len(coveredProvs), exactPairs, nameOnlyArms)

	// Floor guards: if a refactor accidentally empties the SSOT or skips arms,
	// these fail rather than letting an empty matrix pass green (a coverage
	// regression that would otherwise be invisible).
	if exactPairs == 0 {
		t.Error("matrix asserted ZERO exact-listed pairs — the SSOT-driven iteration is broken")
	}
	if nameOnlyArms == 0 {
		t.Error("matrix asserted ZERO name-only arm probes — the name-only BYOK routing is no longer being exercised")
	}
}

// TestDeriveProviderMatrix_RepresentativesAreUsed proves the
// representativeBYOKModel map carries no DEAD entries: every key must correspond
// to a provider that actually appears as a name-only arm in the SSOT. A stale
// entry (a provider renamed/removed but its sample left behind) would silently
// rot; this fails it RED so the probe set stays a faithful mirror of the SSOT's
// name-only arm set. (The complementary direction — every name-only arm HAS a
// representative — is enforced inside TestDeriveProviderMatrix_SSOTDriven.)
func TestDeriveProviderMatrix_RepresentativesAreUsed(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	nameOnlyProviders := map[string]struct{}{}
	for _, native := range m.Runtimes {
		for _, ref := range native.Providers {
			if len(ref.Models) == 0 {
				nameOnlyProviders[ref.Name] = struct{}{}
			}
		}
	}
	for name := range representativeBYOKModel {
		if _, ok := nameOnlyProviders[name]; !ok {
			t.Errorf("representativeBYOKModel has a DEAD entry %q — no name-only arm in the SSOT uses it; remove it or fix the name", name)
		}
	}
}

// TestDeriveProviderMatrix_KnownTrickyForms pins, as EXPLICIT assertions, the
// historically-bug-prone routing forms the SSOT-driven loop covers implicitly —
// so a regression in any of them names the exact class it broke (these are the
// cases #2263/#2274/#2265 shipped/nearly-shipped before the SSOT tightening).
// Explicit here = a failing CI line that says "the colon-vs-slash minimax split
// broke" rather than only a generic matrix-cell failure.
func TestDeriveProviderMatrix_KnownTrickyForms(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	cases := []struct {
		name    string
		runtime string
		model   string
		authEnv []string
		want    string // provider name; "" => expect an unregistered/unrouteable error
		wantErr bool
	}{
		// --- the #2263/#2274 colon-vs-slash-vs-bare MiniMax triple on claude-code:
		// THREE spellings, THREE distinct outcomes. A routing-table edit that
		// collapses any two of these reds here.
		{"minimax bare -> BYOK minimax provider", "claude-code", "MiniMax-M2.7", []string{"MINIMAX_API_KEY"}, "minimax", false},
		{"minimax slash -> platform (proxy upstream)", "claude-code", "minimax/MiniMax-M2.7", nil, "platform", false},
		{"minimax colon -> UNREGISTERED on claude-code (adapter can't strip minimax:)", "claude-code", "minimax:MiniMax-M2.7", nil, "", true},
		// --- openai namespaced is REJECTED on platform-shared runtimes that do
		// not natively wire an openai arm (#2265 class): claude-code offers NO
		// openai/openai-* native arm, so a bare gpt-* and an `openai:`/`openai/`
		// id are both unregistered for it (the platform-shared openai vendor is
		// never wired into a BYOK runtime → cannot bill the platform's key).
		{"claude-code bare gpt -> unregistered (#2265)", "claude-code", "gpt-5.5", nil, "", true},
		{"claude-code openai-namespaced -> unregistered (#2265)", "claude-code", "openai:gpt-4o", nil, "", true},
		// --- groq routes to groq (openclaw's dedicated BYOK groq arm) ----------
		{"openclaw groq: -> groq", "openclaw", "groq:llama-3.3-70b", nil, "groq", false},
		// --- openclaw colon BYOK minimax (the runtime's DEFAULT model) ---------
		{"openclaw minimax: -> byok-minimax", "openclaw", "minimax:MiniMax-M2.7", nil, "byok-minimax", false},
		// --- hermes namespaced shared-vendor ids route to the BYOK-vendor arm,
		// NOT platform (the cp#529 billing-safety property: a tenant's
		// anthropic/gemini/openai/minimax id bills the TENANT key, not platform).
		{"hermes anthropic/ -> byok-anthropic NOT platform", "hermes", "anthropic/claude-opus-4-7", nil, "byok-anthropic", false},
		{"hermes gemini/ -> byok-gemini", "hermes", "gemini/gemini-2.5-pro", nil, "byok-gemini", false},
		{"hermes openai: -> byok-openai", "hermes", "openai:gpt-4o", nil, "byok-openai", false},
		{"hermes minimax: -> byok-minimax", "hermes", "minimax:MiniMax-M2", nil, "byok-minimax", false},
		// --- codex BYOK minimax token-plan id routes via the narrow codex- leg --
		{"codex codex-minimax- -> byok-minimax", "codex", "codex-minimax-m2.7", nil, "byok-minimax", false},
		// --- codex gpt-* default (no auth) -> openai-subscription (first arm) ---
		{"codex gpt default -> openai-subscription", "codex", "gpt-5.5", nil, "openai-subscription", false},
		{"codex gpt with OPENAI_API_KEY -> openai-api", "codex", "gpt-5.5", []string{"OPENAI_API_KEY"}, "openai-api", false},
		// --- google-adk platform vs BYOK google split -------------------------
		{"google-adk platform: -> platform", "google-adk", "platform:gemini-2.5-pro", nil, "platform", false},
		{"google-adk bare gemini -> google (BYOK)", "google-adk", "gemini-2.5-pro", nil, "google", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, derr := m.DeriveProvider(tc.runtime, tc.model, tc.authEnv)
			if tc.wantErr {
				if derr == nil {
					t.Fatalf("DeriveProvider(%q, %q, %v) = %q, want an unregistered/unrouteable ERROR", tc.runtime, tc.model, tc.authEnv, got.Name)
				}
				if got.Name != "" {
					t.Errorf("DeriveProvider(%q, %q) on error must return a zero Provider, got %q", tc.runtime, tc.model, got.Name)
				}
				return
			}
			if derr != nil {
				t.Fatalf("DeriveProvider(%q, %q, %v) errored: %v, want %q", tc.runtime, tc.model, tc.authEnv, derr, tc.want)
			}
			if got.Name != tc.want {
				t.Errorf("DeriveProvider(%q, %q, %v) = %q, want %q", tc.runtime, tc.model, tc.authEnv, got.Name, tc.want)
			}
		})
	}
}
