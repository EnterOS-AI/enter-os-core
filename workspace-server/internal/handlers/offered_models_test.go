package handlers

// Tests for workspace-server/internal/handlers/offered_models.go
// (ListOfferedModels, GET /admin/llm/offered-models?runtime=<rt>).
//
// The endpoint is the SSOT model-discovery surface (core#2608, CTO
// 2026-06-11): agents call it BEFORE provisioning instead of guessing
// a model id. The create-boundary MISSING_BYOK_CREDENTIAL hard-reject
// is the enforcement twin.
//
// Coverage gap closed: the existing TestListOfferedModels_ClaudeCode
// in model_registry_validation_2608_test.go covers the happy path on
// ?runtime=claude-code, but the file offered_models.go has its own
// branches that are not pinned:
//
//   1. Empty / missing ?runtime query defaults to "claude-code"
//   2. Unknown runtime returns 404 with structured "unknown runtime" body
//   3. providerRegistry load error returns 503
//   4. Model list is emitted in alphabetic order regardless of
//      manifest-declared order
//   5. Models that DeriveProvider cannot resolve (ambiguous without
//      auth context) are silently dropped from the response
//   6. Non-platform (BYOK) providers surface their auth_env in the
//      payload
//   7. Response top-level "runtime" field is the resolved (defaulted)
//      runtime, not the raw query string
//
// These tests use a hand-built providers.Manifest fixture (same shape
// as workspace_provision_derive_test.go) so they are deterministic
// and do not depend on the embedded providers.yaml evolving.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/providers"
)

// offeredModelsTestManifest returns a deterministic, two-runtime
// manifest. The claude-code runtime has 3 models whose native-arm
// ordering is NOT alphabetic (so the sort-order test is meaningful):
// "zulu", "alpha", "mike". The hermes runtime has 1 model. The
// gpt-* model on claude-code has TWO native arms (a / b) so the
// auth-disambiguation shape (RFC #340) is exercised end-to-end.
func offeredModelsTestManifest() *providers.Manifest {
	return &providers.Manifest{
		Providers: []providers.Provider{
			// Platform: no auth_env (keyless).
			{Name: "platform", ModelPrefixMatch: "^moonshot/"},
			// BYOK: requires OPENAI_API_KEY.
			{Name: "openai-api", ModelPrefixMatch: "^gpt-", AuthEnv: []string{"OPENAI_API_KEY"}},
			// BYOK: requires ANTHROPIC_API_KEY.
			{Name: "anthropic-api", ModelPrefixMatch: "^claude-", AuthEnv: []string{"ANTHROPIC_API_KEY"}},
		},
		Runtimes: map[string]providers.RuntimeNativeSet{
			"claude-code": {
				Providers: []providers.RuntimeProviderRef{
					{Name: "platform", Models: []string{"zulu", "alpha", "mike"}},
					{Name: "anthropic-api", Models: []string{"claude-sonnet-4-6"}},
				},
			},
			"hermes": {
				Providers: []providers.RuntimeProviderRef{
					{Name: "anthropic-api", Models: []string{"claude-haiku-4-5"}},
				},
			},
		},
	}
}

// withSwappedProviderRegistry runs fn with a stub providerRegistry
// that returns the supplied manifest (or error). The previous
// providerRegistry is restored when fn returns.
func withSwappedProviderRegistry(t *testing.T, m *providers.Manifest, err error, fn func()) {
	t.Helper()
	old := providerRegistry
	providerRegistry = func() (*providers.Manifest, error) {
		return m, err
	}
	t.Cleanup(func() { providerRegistry = old })
	fn()
}

// callListOfferedModels issues an HTTP GET against the handler with
// the given raw query string and returns the recorded response.
func callListOfferedModels(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	url := "/admin/llm/offered-models"
	if query != "" {
		url += "?" + query
	}
	c.Request = httptest.NewRequest("GET", url, nil)
	ListOfferedModels(c)
	return w
}

// TestListOfferedModels_DefaultRuntime: an empty / missing ?runtime
// query must default to "claude-code" (the production default for
// the enterprise agent fleet). Agents that hit the endpoint with no
// query get the claude-code menu.
func TestListOfferedModels_DefaultRuntime(t *testing.T) {
	withSwappedProviderRegistry(t, offeredModelsTestManifest(), nil, func() {
		w := callListOfferedModels(t, "")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Runtime string         `json:"runtime"`
			Models  []OfferedModel `json:"models"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if resp.Runtime != "claude-code" {
			t.Errorf("default runtime must be claude-code, got %q", resp.Runtime)
		}
		// We have 4 distinct model ids for claude-code: zulu, alpha, mike, claude-sonnet-4-6.
		if len(resp.Models) != 4 {
			t.Errorf("expected 4 models for claude-code, got %d: %+v", len(resp.Models), resp.Models)
		}
	})
}

// TestListOfferedModels_UnknownRuntime: an unknown runtime must
// return 404 with a structured body so the canvas (or a confused
// agent) can pattern-match on "unknown runtime" rather than getting
// a generic 500.
func TestListOfferedModels_UnknownRuntime(t *testing.T) {
	withSwappedProviderRegistry(t, offeredModelsTestManifest(), nil, func() {
		w := callListOfferedModels(t, "runtime=does-not-exist")
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if resp["error"] != "unknown runtime" {
			t.Errorf("error = %v, want \"unknown runtime\"", resp["error"])
		}
		if resp["runtime"] != "does-not-exist" {
			t.Errorf("runtime echo = %v, want \"does-not-exist\"", resp["runtime"])
		}
	})
}

// TestListOfferedModels_RegistryLoadError: when the provider
// registry itself fails to load (build-time defect, degraded
// disk, corrupted manifest), the endpoint must return 503 — the
// caller cannot derive a model menu without the registry, and
// a 200 with an empty list would let the agent proceed with a
// bogus model id (caught only at create time, too late).
func TestListOfferedModels_RegistryLoadError(t *testing.T) {
	withSwappedProviderRegistry(t, nil, errRegistryUnavailable, func() {
		w := callListOfferedModels(t, "runtime=claude-code")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got, ok := resp["error"].(string); !ok || got != "provider registry unavailable" {
			t.Errorf("error = %v, want \"provider registry unavailable\"", resp["error"])
		}
	})
}

// errRegistryUnavailable is the sentinel used by the
// providerRegistry load-error path. Defined as a local error so
// the test does not depend on a particular error-string shape from
// the loader.
var errRegistryUnavailable = &registryUnavailableError{}

type registryUnavailableError struct{}

func (e *registryUnavailableError) Error() string { return "test: provider registry unavailable" }

// TestListOfferedModels_SortOrder: the response is consumed by
// the canvas dropdown and the agent's discovery loop, both of
// which assume alphabetic order. The manifest declares zulu,
// alpha, mike — the endpoint MUST sort them. (Without the sort,
// the first-declared native arm order would surface, which is
// unstable across runtime-template edits and trips agent UIs that
// dedupe by model id.)
func TestListOfferedModels_SortOrder(t *testing.T) {
	withSwappedProviderRegistry(t, offeredModelsTestManifest(), nil, func() {
		w := callListOfferedModels(t, "runtime=claude-code")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Runtime string         `json:"runtime"`
			Models  []OfferedModel `json:"models"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("parse: %v", err)
		}
		// Pull the bare model ids in response order.
		got := make([]string, 0, len(resp.Models))
		for _, m := range resp.Models {
			got = append(got, m.Model)
		}
		// Expected sorted set: alpha, claude-sonnet-4-6, mike, zulu.
		want := []string{"alpha", "claude-sonnet-4-6", "mike", "zulu"}
		if len(got) != len(want) {
			t.Fatalf("model count = %d, want %d (got=%v)", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("position %d: got %q, want %q (full=%v)", i, got[i], want[i], got)
			}
		}
	})
}

// TestListOfferedModels_BYOKAuthEnv: a non-platform (BYOK)
// provider must surface its auth_env so the agent can prompt the
// user for the right key. The platform provider must NOT surface
// auth_env (it's keyless, so the agent would chase a key that
// doesn't exist). The omitempty JSON tag means auth_env is absent
// from the platform entries, not just empty — verify both shapes.
func TestListOfferedModels_BYOKAuthEnv(t *testing.T) {
	withSwappedProviderRegistry(t, offeredModelsTestManifest(), nil, func() {
		w := callListOfferedModels(t, "runtime=claude-code")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Runtime string         `json:"runtime"`
			Models  []OfferedModel `json:"models"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("parse: %v", err)
		}
		byID := map[string]OfferedModel{}
		for _, m := range resp.Models {
			byID[m.Model] = m
		}

		// Platform entry: keyless, no auth_env, PlatformBilled=true.
		// (The fixture has 3 platform entries — pick any; they all
		// share the same shape.)
		alpha, ok := byID["alpha"]
		if !ok {
			t.Fatalf("expected alpha in menu, got %+v", byID)
		}
		if !alpha.PlatformBilled {
			t.Errorf("alpha: PlatformBilled = false, want true (provider=platform is keyless)")
		}
		if alpha.Provider != "platform" {
			t.Errorf("alpha: Provider = %q, want \"platform\"", alpha.Provider)
		}
		if len(alpha.AuthEnv) != 0 {
			t.Errorf("alpha: AuthEnv = %v, want empty (keyless platform entry)", alpha.AuthEnv)
		}

		// BYOK entry: PlatformBilled=false, AuthEnv populated.
		sonnet, ok := byID["claude-sonnet-4-6"]
		if !ok {
			t.Fatalf("expected claude-sonnet-4-6 in menu, got %+v", byID)
		}
		if sonnet.PlatformBilled {
			t.Errorf("sonnet: PlatformBilled = true, want false (anthropic-api is BYOK)")
		}
		if sonnet.Provider != "anthropic-api" {
			t.Errorf("sonnet: Provider = %q, want \"anthropic-api\"", sonnet.Provider)
		}
		// AuthEnv should contain the BYOK env name. (Exact membership
		// may include additional fallback names; the load-bearing
		// assertion is that ANTHROPIC_API_KEY is among them.)
		found := false
		for _, e := range sonnet.AuthEnv {
			if e == "ANTHROPIC_API_KEY" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("sonnet: AuthEnv = %v, want ANTHROPIC_API_KEY among them", sonnet.AuthEnv)
		}

		// Auth-env omitempty: the platform entries must NOT emit an
		// "auth_env" key in the raw JSON. (The struct field has
		// `json:"auth_env,omitempty"`, so an empty slice is dropped.)
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatalf("raw parse: %v", err)
		}
		rawModels := []map[string]json.RawMessage{}
		if err := json.Unmarshal(raw["models"], &rawModels); err != nil {
			t.Fatalf("raw models parse: %v", err)
		}
		for _, entry := range rawModels {
			var provider string
			_ = json.Unmarshal(entry["provider"], &provider)
			if provider != "platform" {
				continue
			}
			if _, has := entry["auth_env"]; has {
				t.Errorf("platform entry must omit auth_env key (omitempty); got %s", string(entry["auth_env"]))
			}
		}
	})
}

// TestListOfferedModels_AmbiguousModelSkipped: pins the `continue` path
// in the handler's per-model loop — when DeriveProvider returns an
// error for a model id, the handler silently drops that model from
// the response. The create gate would reject such a model at
// provision time anyway, so the agent must not see a menu entry it
// cannot actually use.
//
// CRITICAL: the test must exercise the branch with a model id that
// is ACTUALLY RETURNED BY `m.ModelsForRuntime(runtime)`. The handler
// only iterates that set; a model id not in it never enters the
// loop, so the `dErr != nil { continue }` branch is never reached
// and the test becomes tautological (would pass even if the branch
// were deleted).
//
// Fixture recipe (per CR2 #11570): a runtime ref whose `Name`
// references a provider that is NOT in the provider catalog. The
// fixture is built directly as a `*providers.Manifest` (bypassing
// `parseManifest` validation, which would reject the dangling ref
// at load time — that's a load-time check, not a runtime invariant;
// the runtime code only consults the catalog when DeriveProvider
// looks the ref up by name). Effect:
//
//  1. `ModelsForRuntime("split")` iterates `ref.Models` and
//     returns BOTH "alpha-survives" and "ghost-drops" (de-duped,
//     native-declaration order).
//  2. Handler iterates and calls `DeriveProvider("split", id, nil)`.
//  3. For "alpha-survives" — listed under `real-co`, which IS in
//     the catalog. DeriveProvider step 3 finds it in `byName`,
//     adds to `exact`, `len(exact)==1`, returns the real-co
//     provider. No error.
//  4. For "ghost-drops" — listed under `ghost-co`, which is NOT
//     in the catalog. DeriveProvider step 3 tries `byName["ghost-co"]`
//     and gets `ok=false`, so it does NOT add to `exact`. Step 4
//     iterates native providers, but only `real-co` is in
//     `byName`; "ghost-drops" does not match `^alpha-` so
//     `matched` ends up empty. Step 6 errors with
//     "unregistered/unselectable" — exactly the dErr the handler
//     is supposed to swallow.
//
// The sibling "alpha-survives" must still appear in the response;
// only "ghost-drops" is dropped. This is the load-bearing
// distinction: if the handler accidentally treated the error as a
// 500 or returned an empty list, the sibling would disappear and
// the test would fail loudly.
func TestListOfferedModels_AmbiguousModelSkipped(t *testing.T) {
	manifest := &providers.Manifest{
		Providers: []providers.Provider{
			// The ONLY provider in the catalog. Its prefix matches
			// "alpha-*" but NOT "ghost-*", so the dangling-ref model
			// is invisible to step 4.
			{Name: "real-co", ModelPrefixMatch: "^alpha-", AuthEnv: []string{"REAL_KEY"}},
		},
		Runtimes: map[string]providers.RuntimeNativeSet{
			"split": {
				Providers: []providers.RuntimeProviderRef{
					// Real ref + listed sibling. This arm resolves
					// cleanly through step 3.
					{Name: "real-co", Models: []string{"alpha-survives"}},
					// Ghost ref + listed model. The ref name
					// "ghost-co" is NOT in the provider catalog, so
					// step 3 cannot resolve "ghost-drops" to any
					// provider and step 4 has no matching regex →
					// DeriveProvider errors. ModelsForRuntime still
					// returns "ghost-drops" (it doesn't validate
					// ref.Name against the catalog at runtime).
					{Name: "ghost-co", Models: []string{"ghost-drops"}},
				},
			},
		},
	}
	withSwappedProviderRegistry(t, manifest, nil, func() {
		w := callListOfferedModels(t, "runtime=split")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Runtime string         `json:"runtime"`
			Models  []OfferedModel `json:"models"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("parse: %v", err)
		}
		got := map[string]bool{}
		for _, m := range resp.Models {
			got[m.Model] = true
		}
		// ghost-drops MUST be dropped (DeriveProvider errored on it —
		// the dangling-ref path that the `continue` branch swallows).
		if got["ghost-drops"] {
			t.Errorf("ghost-listed model with no catalog provider must be dropped, but ghost-drops is in response: %+v", resp.Models)
		}
		// alpha-survives MUST survive (clean step-3 resolution). If
		// the handler swallowed the loop entirely on any error,
		// this assertion would fail.
		if !got["alpha-survives"] {
			t.Errorf("sibling must survive alongside the dropped entry, but alpha-survives is missing: %+v", resp.Models)
		}
	})
}
