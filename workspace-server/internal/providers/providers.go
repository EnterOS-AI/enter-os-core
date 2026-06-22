// Package providers is the SSOT baseline for the LLM provider registry.
//
// RFC: molecule-ai/molecule-controlplane#340 "Canonical Providers
// Manifest". This package is PR-1: it embeds and parses providers.yaml
// (the git-tracked baseline that transcribes the union of the proxy
// switch, the canvas VENDOR_LABELS, the adapter config.yaml `providers:`
// block, and the DB llm_price_catalog). NOTHING imports it yet — the
// consumers (internal/handlers/llm_proxy.go, the canvas dropdown, and
// the workspace-template adapters) are migrated in later PRs. Reverting
// PR-1 = delete this package; zero runtime behavior change.
//
// Distribution model mirrors internal/envs (RFC internal#213 §6.5.4
// Option C): go:embed the YAML into the binary so a boot-time Load never
// touches the network. A future DB override layer (RFC §3 (c)) can merge
// on top of the embedded baseline without breaking this package's API.
package providers

import (
	_ "embed"
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

// schemaVersion is the providers.yaml schema this package knows how to
// parse. It is the MAJOR component of the semver'd extension contract
// (internal#718: the manifest is a first-class versioned public artifact;
// breaking the field set is a governed API break). Bumped only on a breaking
// field-set change; Load fails closed on a mismatch so an older binary cannot
// silently consume a newer manifest (mirrors internal/envs). See
// internal/providers/README.md for the contract + compatibility policy.
const schemaVersion = 1

// SchemaVersion exposes the schema/contract MAJOR version the loader knows
// how to parse. It is the version the codegen artifact (cmd/gen-providers)
// and any future conformance suite pin against. Public so the generator and
// external conformance tooling read the same constant the loader enforces.
func SchemaVersion() int { return schemaVersion }

//go:embed providers.yaml
var embeddedYAML []byte

// Protocol is the wire format the proxy speaks to a provider's upstream.
type Protocol string

const (
	// ProtocolOpenAI is the OpenAI chat-completions wire format.
	ProtocolOpenAI Protocol = "openai"
	// ProtocolAnthropic is the Anthropic messages wire format.
	ProtocolAnthropic Protocol = "anthropic"
)

// Provider is one entry in the canonical manifest. It is the superset
// schema from RFC §2 — each consumer reads the subset it needs (the
// proxy reads protocol/base_url/auth_env, the canvas reads
// display_name/vendor_logo/model_prefix_match, the adapter reads
// auth_mode/auth_token_env/base_url). Field names mirror the YAML keys.
type Provider struct {
	// Name is the stable key (intended to align with
	// llm_price_catalog.provider; see the DRIFT NOTE in providers.yaml).
	Name string `yaml:"name"`
	// DisplayName is the canvas dropdown label.
	DisplayName string `yaml:"display_name"`
	// VendorLogo is the canvas asset key.
	VendorLogo string `yaml:"vendor_logo"`
	// Protocol is the proxy wire format: "openai" or "anthropic".
	Protocol Protocol `yaml:"protocol"`
	// AuthMode is one of "anthropic_api", "oauth",
	// "third_party_anthropic_compat".
	AuthMode string `yaml:"auth_mode"`
	// BaseURLTemplate is the openai-protocol base URL (empty = SDK/CLI
	// default).
	BaseURLTemplate string `yaml:"base_url_template"`
	// BaseURLAnthropic is the anthropic-protocol base URL where the
	// provider exposes one (empty otherwise).
	BaseURLAnthropic string `yaml:"base_url_anthropic"`
	// AuthEnv is the list of env var NAMES accepted (never secret
	// values); any one being set satisfies auth.
	AuthEnv []string `yaml:"auth_env"`
	// AuthTokenEnv is the env var the adapter projects the vendor key
	// into (defaults to ANTHROPIC_AUTH_TOKEN when empty).
	AuthTokenEnv string `yaml:"auth_token_env"`
	// ModelPrefixMatch is the RE2 regex that unifies the proxy's
	// inferLLMProvider prefixes, the canvas BARE_VENDOR_PATTERNS, and
	// the adapter model_prefixes.
	ModelPrefixMatch string `yaml:"model_prefix_match"`
	// ModelAliases are canvas shortcut ids (e.g. sonnet/opus/haiku).
	ModelAliases []string `yaml:"model_aliases"`
	// Deprecated greys the provider out in the canvas (RFC §8.2)
	// without breaking saved workspace configs. Optional; default false.
	Deprecated bool `yaml:"deprecated"`
	// UpstreamVendor is the proxy's upstream-vendor key for this entry — the
	// 4-name vocabulary {openai, moonshot, anthropic, minimax} the proxy's
	// resolveLLMProviderTarget switch dispatches on to pick the upstream base
	// URL + key (internal#718 P1, CONVERGED). It is set ONLY on the entries the
	// proxy routes to an upstream vendor; empty for every other catalog entry.
	//
	// It is a single PROPERTY of the entry, not a parallel routing block: the
	// upstream-vendor IDENTITY of a provider (e.g. "anthropic-api"'s upstream is
	// the "anthropic" vendor) is a fact about that one entry. ResolveUpstream
	// reads it to map a model id's NAMESPACE token to the backing provider,
	// whose base_url_* / auth_env (already on this same entry) are the SINGLE
	// source for the upstream target. The token may differ from Name (the entry
	// "anthropic-api" has UpstreamVendor "anthropic"); for moonshot/openai/
	// minimax the entry name and the upstream vendor coincide.
	UpstreamVendor string `yaml:"upstream_vendor"`

	// re is the compiled ModelPrefixMatch. Compiled at Load (so a bad
	// regex fails the whole manifest, per RFC §8.5) and reused by
	// MatchesModel. Nil only for a zero-value Provider not produced by
	// Load, in which case MatchesModel compiles on demand.
	re *regexp.Regexp
}

// RuntimeProviderRef is one provider a runtime natively supports, plus the
// exact model ids that runtime exposes for it. RFC #340 (CTO correction
// 2026-05-26): the manifest is constrained to each runtime's NATIVE support
// matrix, NOT the 24-provider superset. A provider absent from every
// runtime's native set is over-offer drift the canvas must not surface and
// the proxy must not route (matches cp#334 "use native endpoint, don't
// translate").
type RuntimeProviderRef struct {
	// Name references a Provider.Name. Load fails closed if it does not
	// resolve, so a typo can never silently drop a model from a runtime.
	Name string `yaml:"name"`
	// Models is the exact set of model ids this runtime exposes for the
	// referenced provider (extracted verbatim from the runtime template's
	// config.yaml runtime_config.models block). Empty is a manifest error:
	// a native provider with zero models offers nothing.
	Models []string `yaml:"models"`
}

// RuntimeNativeSet is the native provider+model matrix for a single runtime.
type RuntimeNativeSet struct {
	// Providers is the runtime's native provider set (each with its exact
	// model ids). Exactly the set the canvas may offer and the proxy may
	// route for this runtime — no more, no fewer.
	Providers []RuntimeProviderRef `yaml:"providers"`
}

// Manifest is the parsed providers.yaml: the provider catalog plus the
// per-runtime native constraint layer. Returned by LoadManifest; Load
// remains for callers that only need the flat provider slice.
type Manifest struct {
	// Providers is the full provider catalog (protocol, base_url, auth).
	Providers []Provider
	// Runtimes maps a runtime name (claude-code, hermes, codex, openclaw)
	// to its native provider+model set. The SSOT for "which providers and
	// models does runtime R natively support".
	Runtimes map[string]RuntimeNativeSet
}

type manifest struct {
	SchemaVersion int                         `yaml:"schema_version"`
	Providers     []Provider                  `yaml:"providers"`
	Runtimes      map[string]RuntimeNativeSet `yaml:"runtimes"`
}

// Load parses the embedded providers.yaml and returns the manifest's
// provider slice. It validates the schema version, that every entry has
// the required fields populated, and that every model_prefix_match is a
// compilable RE2 regex. Errors are returned (never panic) so callers
// decide their own fallback (the proxy keeps a legacy switch; see RFC
// §6). Load does not touch the network.
//
// Load is the flat-slice accessor retained for PR-1 callers that only need
// the provider catalog. Callers needing the per-runtime native constraint
// layer use LoadManifest.
func Load() ([]Provider, error) {
	m, err := LoadManifest()
	if err != nil {
		return nil, err
	}
	return m.Providers, nil
}

// LoadManifest parses the embedded providers.yaml into a Manifest: the
// provider catalog plus the per-runtime native support matrix (RFC #340).
// It performs all of Load's validation AND validates the runtimes block:
// every provider name a runtime references must resolve to a real provider
// entry, and every referenced provider must carry at least one model id.
// Fails closed (never panic, never network) so a typo'd provider ref or an
// empty native set is a load error, not a silent over/under-offer.
func LoadManifest() (*Manifest, error) {
	return parseManifest(embeddedYAML)
}

// parseManifest is the byte-level seam LoadManifest delegates to. Split out
// so the validation branches (bad schema version, unknown provider ref,
// empty native set, duplicate ref, model-less ref) are unit-testable
// against crafted YAML without mutating the embedded baseline.
func parseManifest(raw []byte) (*Manifest, error) {
	var m manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("providers: parse manifest: %w", err)
	}
	if m.SchemaVersion != schemaVersion {
		return nil, fmt.Errorf("providers: manifest schema_version %d, loader expects %d", m.SchemaVersion, schemaVersion)
	}
	if len(m.Providers) == 0 {
		return nil, fmt.Errorf("providers: manifest has no providers")
	}

	seen := make(map[string]struct{}, len(m.Providers))
	out := make([]Provider, 0, len(m.Providers))
	for i := range m.Providers {
		p := m.Providers[i]
		if err := p.validate(); err != nil {
			return nil, fmt.Errorf("providers: entry %d (%q): %w", i, p.Name, err)
		}
		if _, dup := seen[p.Name]; dup {
			return nil, fmt.Errorf("providers: duplicate provider name %q", p.Name)
		}
		seen[p.Name] = struct{}{}

		re, err := regexp.Compile(p.ModelPrefixMatch)
		if err != nil {
			return nil, fmt.Errorf("providers: entry %q model_prefix_match %q: %w", p.Name, p.ModelPrefixMatch, err)
		}
		p.re = re
		out = append(out, p)
	}

	// upstream_vendor validation (internal#718 P1, CONVERGED). It is optional
	// (set only on the entries the proxy routes to an upstream), but it must be
	// UNIQUE across the catalog: ResolveUpstream maps a model id's namespace
	// token to the ONE entry whose UpstreamVendor equals it, so two entries
	// claiming the same vendor would make the namespace token ambiguous (a
	// non-deterministic upstream). Fail closed so a typo can never produce two
	// entries owning the same upstream vendor.
	vendorOwner := make(map[string]string, len(out))
	for i := range out {
		v := out[i].UpstreamVendor
		if v == "" {
			continue
		}
		if prev, dup := vendorOwner[v]; dup {
			return nil, fmt.Errorf("providers: entries %q and %q both declare upstream_vendor %q — it must be unique (the namespace token resolves to exactly one entry)", prev, out[i].Name, v)
		}
		vendorOwner[v] = out[i].Name
	}

	if len(m.Runtimes) == 0 {
		return nil, fmt.Errorf("providers: manifest declares no runtimes")
	}
	for rt, native := range m.Runtimes {
		if len(native.Providers) == 0 {
			return nil, fmt.Errorf("providers: runtime %q has an empty native provider set", rt)
		}
		refSeen := make(map[string]struct{}, len(native.Providers))
		for _, ref := range native.Providers {
			if _, ok := seen[ref.Name]; !ok {
				return nil, fmt.Errorf("providers: runtime %q references unknown provider %q", rt, ref.Name)
			}
			if _, dup := refSeen[ref.Name]; dup {
				return nil, fmt.Errorf("providers: runtime %q references provider %q twice", rt, ref.Name)
			}
			refSeen[ref.Name] = struct{}{}
			// A NAME-ONLY arm (zero model ids) is permitted (cp#529): it adds
			// NOTHING to the runtime's platform menu (ModelsForRuntime only
			// iterates ref.Models, so an empty Models contributes no selectable
			// id — additive, zero platform-menu change) yet wires the provider
			// into the runtime's NATIVE prefix-routing set, so a BYOK id the
			// provider's model_prefix_match matches becomes routable via
			// DeriveProvider step-4. This is the mechanism the cp#529
			// routability-aware enforcer keys off: a name-only BYOK arm makes a
			// passthrough id (openrouter/…, deepseek-…, etc.) resolve to a
			// concrete provider without ever appearing on the platform menu.
			// BILLING GUARDRAIL: only CONFIRMED-NON-PLATFORM (BYOK) providers
			// are wired as name-only arms — never `platform`/anthropic-*/
			// openai-*/moonshot/minimax/google/vertex — so a name-only arm can
			// never route a customer model through the platform's key.
		}
	}

	return &Manifest{Providers: out, Runtimes: m.Runtimes}, nil
}

// ProvidersForRuntime returns the providers runtime rt natively supports,
// in the manifest's declared order. An unknown runtime returns a non-nil
// error and a nil slice — it never falls through to "all providers", so a
// caller that fat-fingers a runtime name fails loud rather than offering
// the whole catalog.
func (m *Manifest) ProvidersForRuntime(rt string) ([]Provider, error) {
	native, ok := m.Runtimes[rt]
	if !ok {
		return nil, fmt.Errorf("providers: unknown runtime %q", rt)
	}
	byName := make(map[string]Provider, len(m.Providers))
	for _, p := range m.Providers {
		byName[p.Name] = p
	}
	out := make([]Provider, 0, len(native.Providers))
	for _, ref := range native.Providers {
		// Resolution is guaranteed by LoadManifest's validation, but guard
		// anyway so a hand-built Manifest can't panic here.
		if p, ok := byName[ref.Name]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// ModelsForRuntime returns the exact model ids runtime rt natively exposes,
// flattened across all its native providers, in manifest-declared order.
// An unknown runtime returns a non-nil error and a nil slice (never the
// whole catalog). This is the SSOT the canvas dropdown (PR-4) and the proxy
// router (PR-3) both consume so they can never offer/route a model the
// runtime can't natively run.
func (m *Manifest) ModelsForRuntime(rt string) ([]string, error) {
	native, ok := m.Runtimes[rt]
	if !ok {
		return nil, fmt.Errorf("providers: unknown runtime %q", rt)
	}
	// De-duplicate while preserving first-seen order. A single model id may be
	// exact-listed under MORE THAN ONE native arm — the legitimate "one model
	// id, two auth arms" shape (codex's gpt-* family is offered on both the
	// openai-subscription OAuth arm and the openai-api direct-key arm, mirroring
	// claude-code's anthropic oauth+api split). The canvas surfaces each id
	// once (the auth path is chosen at runtime by which key is present), so the
	// flattened native model set must not repeat it. A no-op for every runtime
	// whose arms list disjoint ids.
	var out []string
	seen := make(map[string]struct{})
	for _, ref := range native.Providers {
		for _, mid := range ref.Models {
			if _, dup := seen[mid]; dup {
				continue
			}
			seen[mid] = struct{}{}
			out = append(out, mid)
		}
	}
	return out, nil
}

// validate checks the required-field invariants for a single entry.
func (p *Provider) validate() error {
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	switch p.Protocol {
	case ProtocolOpenAI, ProtocolAnthropic:
	default:
		return fmt.Errorf("protocol must be %q or %q, got %q", ProtocolOpenAI, ProtocolAnthropic, p.Protocol)
	}
	if p.AuthMode == "" {
		return fmt.Errorf("auth_mode is required")
	}
	if len(p.AuthEnv) == 0 {
		return fmt.Errorf("auth_env must be non-empty")
	}
	if p.DisplayName == "" {
		return fmt.Errorf("display_name is required")
	}
	if p.ModelPrefixMatch == "" {
		return fmt.Errorf("model_prefix_match is required")
	}
	return nil
}

// MatchesModel reports whether the given model slug is owned by this
// provider per its ModelPrefixMatch regex. A Provider produced by Load
// uses its precompiled regex. A zero-value Provider (one constructed
// directly, not via Load) compiles on demand; if the pattern is invalid
// or empty it never matches.
func (p Provider) MatchesModel(slug string) bool {
	re := p.re
	if re == nil {
		if p.ModelPrefixMatch == "" {
			return false
		}
		compiled, err := regexp.Compile(p.ModelPrefixMatch)
		if err != nil {
			return false
		}
		re = compiled
	}
	return re.MatchString(slug)
}
