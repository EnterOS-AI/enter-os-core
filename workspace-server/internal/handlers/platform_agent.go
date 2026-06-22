package handlers

// platform_agent.go — installs the org-level platform agent as the org root.
// (RFC docs/design/rfc-platform-agent.md)
//
// The platform agent IS the org root: an org is the subtree under the single
// parent_id IS NULL row (org_scope.go), so making the concierge the user's
// universal A2A peer means making it that root. Installing it therefore:
//
//	1. upserts the platform-agent row (kind='platform', parent_id NULL);
//	2. re-parents the org's existing root(s) under it;
//	3. moves the org-anchor references — org_api_tokens.org_id and
//	   org_plugin_allowlist.org_id, both of which key off the root workspace id
//	   (see migrations 035/036 + 026) — from each old root to the platform agent.
//
// All of that happens in ONE transaction so a tenant's auth tokens and plugin
// allowlist never point at a stale anchor. The operation is idempotent: a second
// call finds the platform agent already the sole root and does nothing.
//
// Routing (CanCommunicate/sameOrg in registry/access.go + org_scope.go) needs NO
// change — once the platform agent is the root, the existing ancestor/descendant
// rules already give it universal in-org reach and keep tenant isolation intact.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/crypto"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The concierge system-prompt template, the concierge MCP servers block, the
// concierge MCP fragment file (mcp_servers.yaml), the concierge runtime, the
// concierge declared model, and the concierge identity files function were
// REMOVED as part of RFC #2843 §10a (the concierge de-hardcode migration). The
// concierge's identity — system prompt, model, runtime, MCP wiring — is now
// delivered via the molecule-ai-workspace-template-platform-agent template
// (manifest.json workspace_templates entry) and applied like any other runtime
// template. ZERO concierge literals remain in core.
//
// A minimal {{CONCIERGE_NAME}} substitution is performed by
// applyConciergeProvisionConfig (see PR description for the substitution
// recommendation: option (a) — substitute, with the per-instance name; the
// template's prompts/concierge.md already has the placeholder where the name
// goes). The MCP env (conciergePlatformMCPEnv) is still concierge-specific
// because the env wiring is per-MCP-binary, not per-template.

// SelfHostedPlatformAgentID is the deterministic platform-agent id used when no
// control plane is present to derive a per-org id (self-hosted / local). There
// is one platform agent per self-hosted tenant, so a fixed namespaced uuidv5 is
// sufficient and stable across restarts.
var SelfHostedPlatformAgentID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("molecule:self-hosted:platform-agent")).String()

// defaultCreateParentID resolves the parent a NEW workspace should nest under
// when the caller didn't pass one. Order:
//  1. The org's platform-agent root (kind='platform'), if exactly one — the
//     intended home (core#2609).
//  2. FALLBACK (core#2697): when the org has NO platform-agent (e.g. a tenant
//     provisioned with only a plain root workspace — JRS had just its SEO Agent
//     at parent_id NULL), nest under the SOLE non-removed root workspace if there
//     is exactly one. Without this, new workspaces scatter at bare root as
//     siblings of that root agent, and approval/discovery treat each NULL-parent
//     row as its own org root, breaking hierarchy + delegation routing.
//
// The root fallback fires ONLY for the ZERO-platform case. When MULTIPLE
// platform agents exist (ambiguous), we preserve the original fail-soft
// behavior and return "" WITHOUT falling back to a root — picking a root there
// would silently change the intended ambiguous-platform semantics (CR2 #2783).
// Returns "" when: >1 platform; or 0 platform with 0/>1 roots — preserving
// bootstrap/self-host multi-root behavior. The DURABLE fix is guaranteeing every
// org has a platform-agent at provision; this is the safe runtime fallback.
func defaultCreateParentID(ctx context.Context) string {
	// Count platform-agent roots directly (LIMIT 2 distinguishes 0 / 1 / >1).
	plats := queryUpToTwoIDs(ctx,
		`SELECT id FROM workspaces WHERE COALESCE(kind, 'workspace') = 'platform' AND status != 'removed' LIMIT 2`)
	if len(plats) == 1 {
		return plats[0] // the platform-agent root (core#2609)
	}
	if len(plats) >= 2 {
		return "" // ambiguous platform — fail soft, do NOT fall back to a root
	}
	// Exactly ZERO platform agents → nest under the sole plain root if unambiguous.
	roots := queryUpToTwoIDs(ctx,
		`SELECT id FROM workspaces WHERE parent_id IS NULL AND status != 'removed' LIMIT 2`)
	if len(roots) == 1 {
		return roots[0]
	}
	return ""
}

// queryUpToTwoIDs runs an id-selecting query (with its own LIMIT 2) and returns
// up to two ids; on any error it returns an empty slice (fail-soft — defaulting
// the parent is best-effort and must never fail the create).
func queryUpToTwoIDs(ctx context.Context, query string) []string {
	rows, err := db.DB.QueryContext(ctx, query)
	if err != nil {
		return nil
	}
	defer rows.Close()
	ids := make([]string, 0, 2)
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// defaultPlatformAgentName returns the display name for the org's platform
// agent (the concierge). When the tenant server is told its org's name via the
// MOLECULE_ORG_NAME env (the self-hosted docker-compose sets it; SaaS passes an
// explicit name in the CP install payload instead), the concierge is named
// "<org name> Agent" — e.g. org "Molecule AI" → "Molecule AI Agent". With no
// org name configured it falls back to the legacy "Org Concierge".
func defaultPlatformAgentName() string {
	if orgName := os.Getenv("MOLECULE_ORG_NAME"); orgName != "" {
		return fmt.Sprintf("%s Agent", orgName)
	}
	return "Org Concierge"
}

// (the concierge identity files function removed in RFC #2843 §10a — the concierge's
// identity is now delivered via the platform-agent template's
// prompts/concierge.md + config.yaml + mcp_servers.yaml, applied like
// any other runtime template. See substituteConciergeName below for
// the only remaining per-instance identity step.)

// conciergePlatformMCPEnv injects the env the platform MCP child reads at spawn
// (RFC §5.5/§5.6). The org-admin token is ADMIN_TOKEN on self-host; the platform
// URL is the in-cluster PLATFORM_URL (e.g. http://platform:8080). Existing
// values in env win, so an operator/CP override is never clobbered. No-op for a
// non-platform workspace. Best-effort: when ADMIN_TOKEN is unset (pure-local dev
// with AdminAuth fail-open) the key is simply absent and the MCP — which only
// runs on the platform-agent image anyway — is unauthenticated locally.
func conciergePlatformMCPEnv(env map[string]string) {
	setIfAbsent := func(k, v string) {
		if v == "" {
			return
		}
		if _, ok := env[k]; !ok {
			env[k] = v
		}
	}
	setIfAbsent("MOLECULE_API_KEY", os.Getenv("ADMIN_TOKEN"))
	// The management-mode tool registry (mcp-server >=1.5.0,
	// src/tools/management/client.ts) authenticates with
	// MOLECULE_ORG_API_KEY — a distinct env from the connectivity-preflight
	// MOLECULE_API_KEY. The tenant ADMIN_TOKEN is a valid bearer for the
	// tenant-admin surface those tools call (same header shape as the
	// install/restart curls), so wire it under both names. Verified live on
	// the agents-team pilot: with only MOLECULE_API_KEY set, every
	// management tool returns AUTH_ERROR.
	setIfAbsent("MOLECULE_ORG_API_KEY", os.Getenv("ADMIN_TOKEN"))
	// MOLECULE_API_URL: prefer an explicit env, else the in-cluster platform URL.
	apiURL := os.Getenv("MOLECULE_API_URL")
	if apiURL == "" {
		apiURL = os.Getenv("PLATFORM_URL")
	}
	setIfAbsent("MOLECULE_API_URL", apiURL)
	setIfAbsent("MOLECULE_ORG_ID", os.Getenv("MOLECULE_ORG_ID"))

	// Authenticate the on-box plugin boot-install's gitea fetch (core#3065).
	// The concierge declares its management MCP as a PRIVATE gitea:// plugin
	// (molecule-ai-plugin-molecule-platform-mcp). The runtime's boot-install
	// fetches it via curl + ~/.netrc, which setup-gitea-netrc.sh builds from
	// GIT_HTTP_USERNAME / GIT_HTTP_PASSWORD. The concierge has no per-persona git
	// token (applyAgentGitHTTPCreds no-ops for it), so without these the fetch is
	// UNAUTHENTICATED → 404 on the private repo → plugin never installs → no
	// create_workspace (verified live post-#872). Use the read-only
	// MOLECULE_TEMPLATE_REPO_TOKEN the box already holds for fetching template/
	// plugin repos, in the SAME basic-auth shape the Go gitea resolver uses (the
	// PAT as the username + literal "x-oauth-basic" password — see
	// plugins/gitea.go resolveURL). setIfAbsent so an operator-supplied GIT_HTTP_*
	// or a real persona token still wins.
	if tok := strings.TrimSpace(os.Getenv("MOLECULE_TEMPLATE_REPO_TOKEN")); tok != "" {
		setIfAbsent("GIT_HTTP_USERNAME", tok)
		setIfAbsent("GIT_HTTP_PASSWORD", "x-oauth-basic")
	}
}

// applyConciergeProvisionConfig is the provision-time hook for the platform
// agent. Called from prepareProvisionContext for EVERY provision of a
// kind='platform' workspace (create, restart, auto-recover). It is a no-op
// for ordinary workspaces.
//
// Post RFC #2843 §10a: the concierge's PROMPT + MCP-wiring identity (system
// prompt, agent-skills) is delivered via the molecule-ai-workspace-template-
// platform-agent template and applied like any other runtime template. This
// hook's responsibilities are (0) SEED the concierge's declared model so the
// provision passes the universal MISSING_MODEL gate (core#2594), (1) inject the
// platform-MCP env (org-admin token + platform URL + org id) and (2) the
// per-instance {{CONCIERGE_NAME}} substitution in the system-prompt.md.
//
// IMPORTANT (incident 2026-06-15, regression from #2919): the model is NOT
// delivered via the template. The MISSING_MODEL gate reads the stored MODEL
// workspace_secret at provision time — BEFORE any template config.yaml is
// fetched — so a model-less platform agent fails closed ("reached provisioning
// with no model set"). #2919 removed the model-seed on the theory the
// platform-agent template would supply it, but that template entry was never
// added to the manifest. The concierge is the platform-agent PRODUCT itself, so
// it carries an explicit declared model in code (core#2594) exactly as a
// template declares one — this is the SSOT-correct source, not a stopgap. The
// CI test TestApplyConciergeProvisionConfig_SeedsModel gates against re-removal.
//
// Returns the (possibly newly-allocated) configFiles map so the caller can
// rebind it — configFiles is nil on the auto-restart path, where this is the
// thing that introduces the substitution.
func (h *WorkspaceHandler) applyConciergeProvisionConfig(
	ctx context.Context,
	workspaceID, templatePath string,
	configFiles map[string][]byte,
	envVars map[string]string,
	name string,
) map[string][]byte {
	var kind string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(kind, 'workspace') FROM workspaces WHERE id = $1`, workspaceID).Scan(&kind); err != nil {
		// Non-fatal: a missing row / probe error just means "treat as ordinary".
		return configFiles
	}
	if kind != models.KindPlatform {
		return configFiles
	}

	// 0. Concierge model (core#2594). The platform agent carries an explicit,
	//    SSOT-declared model so it never relies on a silent default. The
	//    universal MISSING_MODEL gate (prepareProvisionContext) reads the stored
	//    MODEL secret at provision time — before any template config.yaml is
	//    fetched — so this MUST run here, not be deferred to the template.
	//    Seed-only: it respects a model the customer later picked.
	h.ensureConciergeModel(ctx, workspaceID, envVars)

	// 0b. Concierge LLM provider pin (companion to the model seed). The
	//    molecule-runtime wheel DERIVES a provider slug from the model id
	//    ("moonshot/kimi-k2.6" -> "moonshot" via _derive_provider_from_model),
	//    which is a model-PREFIX on the `platform` provider, NOT a provider
	//    NAME — so the claude-code adapter's _resolve_provider fail-closes
	//    ("provider='moonshot' but it is not in the providers registry") and
	//    the concierge boots configuration_status=not_configured: online but
	//    unable to run a single turn (last_outbound_at stays null).
	//
	//    The platform-agent template config.yaml's `provider: platform` field
	//    does NOT fix this on SaaS: the on-box /configs/config.yaml is the
	//    BAKED base-image config (the box reports the base claude-code image's
	//    8-entry provider registry, not the platform-agent template's 3), so
	//    the template `provider:` scalar never reaches the adapter. Seeding
	//    LLM_PROVIDER (env, highest precedence in the wheel's
	//    LLM_PROVIDER > YAML provider: > derive chain; injected via the
	//    workspace secret) is the robust pin — it survives restart and the
	//    regenerated config.yaml. Verified on prod: setting LLM_PROVIDER=platform
	//    flipped a stuck concierge from not_configured to ready + responding.
	//    Seed-only + gated to the platform-managed model namespace so it never
	//    overrides a BYOK/self-host concierge (see ensureConciergeProvider).
	h.ensureConciergeProvider(ctx, workspaceID, envVars)

	// 0c. Declare the concierge's management MCP as a PLUGIN (RFC:
	//    rfc-platform-mcp-as-plugin). The asset-channel mcp_servers.yaml does NOT
	//    reach the on-box /configs (the box runs the baked base-image config), so
	//    the concierge boots with no management MCP — generic Claude Code, no
	//    create_workspace. Routing it through the plugin channel (the path that
	//    reliably delivers skills) fixes that: declare it here so the post-online
	//    reconcile + boot-install wire molecule-platform-mcp via MCPServerAdaptor.
	//    This declaration runs ONLY on the kind=platform concierge (this function
	//    is kind-gated) → it is the primary entitlement gate for the privileged
	//    org-admin MCP; recordDeclaredPlugin fail-closes the same name for any
	//    non-platform workspace as defense-in-depth. Idempotent (upsert).
	if rec, skip := seedTemplatePlugins(ctx, workspaceID, []string{conciergePlatformMCPSource}); skip > 0 {
		log.Printf("Provisioner: concierge %s could not declare %q plugin (recorded=%d skipped=%d) — management MCP may be absent until next provision", workspaceID, conciergePlatformMCPSource, rec, skip)
	}

	// 1. Platform-MCP env (org-admin token + platform URL + org id).
	conciergePlatformMCPEnv(envVars)

	// 2. {{CONCIERGE_NAME}} substitution in the template-delivered
	//    system-prompt.md. The runtime's build_system_prompt does NOT
	//    template prompt files, so we do the minimal per-instance
	//    substitution here at provision time. The template's
	//    prompts/concierge.md carries the {{CONCIERGE_NAME}} placeholder
	//    where the per-instance name goes. Idempotent: a re-provision
	//    re-runs the substitution; the result is stable.
	if configFiles == nil {
		configFiles = map[string][]byte{}
	}
	if prompt, ok := configFiles["system-prompt.md"]; ok {
		configFiles["system-prompt.md"] = substituteConciergeName(prompt, name)
	}
	log.Printf("Provisioner: applied platform-agent env + {{CONCIERGE_NAME}} substitution for %s (name=%q, %d config file(s))",
		workspaceID, name, len(configFiles))
	return configFiles
}

// conciergeNamePlaceholder is the {{CONCIERGE_NAME}} marker the template's
// prompts/concierge.md carries where the per-instance name goes. The runtime's
// build_system_prompt does NOT template prompt files, so applyConciergeProvisionConfig
// performs the substitution at provision time (RFC #2843 §10a).
const conciergeNamePlaceholder = "{{CONCIERGE_NAME}}"

// substituteConciergeName replaces every occurrence of the {{CONCIERGE_NAME}}
// placeholder in a system-prompt byte slice with the per-instance concierge
// name. Stable: if the placeholder is absent, the input is returned
// unchanged. No other templating is performed — keep this minimal.
func substituteConciergeName(prompt []byte, name string) []byte {
	if len(prompt) == 0 {
		return prompt
	}
	// Use a single allocation to avoid multiple string-to-byte conversions
	// on the hot path. strings.Replace is in the standard library and
	// handles the empty-name case safely (the placeholder is replaced
	// with "" — leaving the prompt with a blank first line. We
	// intentionally do NOT guard against empty name here: the caller
	// (defaultPlatformAgentName) guarantees a non-empty name; if it
	// somehow becomes empty, an empty first line is the louder failure
	// mode (visible in the agent's startup log) than a silent skip.
	// CR2 RC 11903 QF1004: strings.ReplaceAll (replaces all) replaces
	// the legacy strings.Replace(s, old, new, -1) "replace all" idiom
	// with the dedicated stdlib helper.
	return []byte(strings.ReplaceAll(string(prompt), conciergeNamePlaceholder, name))
}

// conciergeRuntime is the runtime the platform agent (concierge) always runs as
// (the platform-agent image variant of 'claude-code'). conciergeDeclaredModel is
// validated against the registry for this runtime before being seeded.
const conciergeRuntime = "claude-code"

// conciergeDeclaredModel is the platform agent's OWN declared model — a
// first-class product decision, NOT a generic platform default. It mirrors a
// template's runtime_config.model SSOT. The SSOT directive
// (feedback_workspace_model_required_no_platform_default...) forbids the platform
// from defaulting a USER workspace's model, but the concierge IS the
// platform-agent product, so it declares its own model exactly as a template
// declares one (core#2594).
const conciergeDeclaredModel = "moonshot/kimi-k2.6"

// ensureConciergeModel makes the platform agent's model explicit (core#2594).
// It (1) seeds the container model env for the current provision and (2)
// persists the MODEL workspace_secret so the read endpoint / canvas Config tab
// surface the resolved model. The model is the concierge's declared SSOT model,
// validated against the registry for its runtime. If validation fails (registry
// drift — a build bug caught by the model-registry CI test), it sets NOTHING:
// the downstream universal MISSING_MODEL gate then fails the provision CLOSED
// rather than letting the runtime pick an opaque default.
func (h *WorkspaceHandler) ensureConciergeModel(ctx context.Context, workspaceID string, envVars map[string]string) {
	// SEED-ONLY (CTO 2026-06-12: customer setting > platform default; the
	// concierge's model is changeable like any workspace, "anytime"). If a MODEL
	// secret already exists — whether the original seed or a model the customer
	// later picked in the canvas — RESPECT it: loadWorkspaceSecrets +
	// applyRuntimeModelEnv have already put it in envVars, so do nothing. Only
	// SEED the declared default when the concierge has no model at all (first
	// boot). Re-asserting on EVERY provision would silently revert the customer's
	// pick — exactly the platform-overriding-customer violation the SSOT
	// directive forbids.
	if existing := readStoredModelSecret(ctx, workspaceID); existing != "" {
		return // explicit model already set; never overwrite the customer's choice
	}

	// First boot — no model yet. Seed the concierge's declared default, but only
	// if it is genuinely routable for the runtime; otherwise leave it unset and
	// let the MISSING_MODEL gate fail closed (loud) rather than seed a model the
	// platform cannot actually route to (silent late failure).
	model := conciergeDeclaredModel
	if ok, why := validateRegisteredModelForRuntime(conciergeRuntime, model); !ok {
		log.Printf("Provisioner: concierge %s declared model %q is NOT registered for runtime %q (%s) — leaving model unset; provision will fail closed", workspaceID, model, conciergeRuntime, why)
		return
	}
	if ok, why := validateDerivedProviderInRegistry(conciergeRuntime, model); !ok {
		log.Printf("Provisioner: concierge %s declared model %q has no derivable registry provider for runtime %q (%s) — leaving model unset; provision will fail closed", workspaceID, model, conciergeRuntime, why)
		return
	}

	// Seed the container env (precedence MOLECULE_MODEL > MODEL in the runtime).
	// applyRuntimeModelEnv already ran with an empty payload model for the
	// concierge (no stored MODEL on first boot), so set both canonical names
	// here so this provision actually runs the seeded default.
	envVars["MOLECULE_MODEL"] = model
	envVars["MODEL"] = model

	// Persist so GET /workspaces/:id/model returns it (Config tab visibility) and
	// the next provision's readStoredModelSecret takes the respect-existing path.
	if setErr := setModelSecret(ctx, workspaceID, model); setErr != nil {
		log.Printf("Provisioner: concierge %s persist MODEL secret failed: %v (env still seeded for this provision)", workspaceID, setErr)
	}
}

// readStoredModelSecret returns the decrypted MODEL workspace_secret, or "" when
// none is stored (or on any read/decrypt error — treated as "unset" so a
// transient miss re-seeds rather than wedges). Used by ensureConciergeModel to
// decide seed-vs-respect.
func readStoredModelSecret(ctx context.Context, workspaceID string) string {
	var stored []byte
	var version int
	if err := db.DB.QueryRowContext(ctx,
		`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = 'MODEL'`,
		workspaceID).Scan(&stored, &version); err != nil {
		return ""
	}
	dec, err := crypto.DecryptVersioned(stored, version)
	if err != nil {
		return ""
	}
	return string(dec)
}

// conciergeProvider is the provider-registry NAME the concierge's declared
// platform-managed model resolves to. The platform agent is always
// platform-managed (billing/audit flow through the platform LLM proxy), so the
// provider is unconditionally "platform" for the platform-managed model family.
const conciergeProvider = "platform"

// platformManagedModelPrefix is the model-id namespace served by the platform
// LLM proxy that ALSO collides with the wheel's provider derivation (the slug
// before '/' is "moonshot", not a registry name). A concierge whose effective
// model carries this prefix MUST have its provider pinned to `platform`
// explicitly; without the pin the claude-code adapter fail-closes. Gating on
// this prefix keeps the seed from touching a BYOK/self-host concierge whose
// model resolves cleanly on its own (e.g. `sonnet` -> anthropic-oauth).
const platformManagedModelPrefix = "moonshot/"

// The management-MCP plugin the concierge declares. It wires the `molecule-mcp`
// server (MOLECULE_MCP_MODE=management — create_workspace, list_workspaces, …)
// into the Claude Code runtime via the plugin channel's MCPServerAdaptor,
// replacing the baked-image + asset-channel mcp_servers.yaml path that does NOT
// reach the on-box config (RFC: rfc-platform-mcp-as-plugin). Declaring it from
// the kind=platform-only applyConciergeProvisionConfig IS the primary
// entitlement gate (no user workspace runs this path); recordDeclaredPlugin adds
// a defense-in-depth refusal for this PRIVILEGED name on any non-platform
// workspace. The post-online reconcile + boot-install then install it.
//
// conciergePlatformMCPSource MUST be a gitea:// source, not a bare name: the
// box's boot-install (runtime-image entrypoint) ONLY fetches gitea:// sources
// and SKIPS anything else ("skip unsupported source"). A bare name parses to the
// `local` scheme, which only resolves plugins baked into the image — and this is
// a brand-new Gitea-only plugin repo, so a bare name would never be fetched.
//
// It MUST also carry a pinned #ref: the gitea resolver rejects an unpinned spec
// in production (PLUGIN_ALLOW_UNPINNED is unset by default — see plugins/gitea.go),
// so an unpinned source would record the declaration but then FAIL to fetch at
// boot-install time → no management MCP, no create_workspace. #main matches the
// established seo-all convention (gitea.go example). The #ref does NOT affect
// PluginNameFromSource, so conciergePlatformMCPName below is unchanged.
const conciergePlatformMCPSource = "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main"

// conciergePlatformMCPName is the install NAME plugins.PluginNameFromSource
// derives from the gitea:// source above (the repo segment, no subpath). It is
// what gets written to workspace_declared_plugins.plugin_name and what the
// recordDeclaredPlugin entitlement gate matches on — so it MUST equal the
// derivation, not the human label "molecule-platform-mcp".
const conciergePlatformMCPName = "molecule-ai-plugin-molecule-platform-mcp"

// conciergePlatformMCPCreateWorkspaceTool is the literal MCP tool identifier
// the platform concierge must surface for the post-online fail-loud gate
// (core#3082) to consider the management MCP actually loaded. The Claude
// Code dispatcher formats every MCP tool as `mcp__<server>__<tool>`; the
// platform MCP server's install name derives to "molecule-platform" via
// PluginNameFromSource(conciergePlatformMCPSource), so the create_workspace
// tool's namespaced identifier is `mcp__molecule-platform__create_workspace`.
// This is the SAME literal the staging concierge E2E (tests/e2e/test_staging_
// concierge_creates_workspace_e2e.sh:4.5/6) probes for; pinning it as a
// constant keeps the runtime gate and the E2E in lock-step so a drift in
// one breaks both with the same signal.
//
// Why we don't match the server/plugin NAME here: the heartbeat's
// loaded_mcp_tools list carries TOOL identifiers (mcp__<server>__<tool>),
// not plugin names. The previous check (loadedSet[conciergePlatformMCPName])
// was a no-op false-green — it matched the plugin NAME against a list of
// TOOL strings, which would always be empty for the management MCP (the
// plugin is named "molecule-ai-plugin-molecule-platform-mcp" while its
// tools are namespaced "mcp__molecule-platform__*"). The literal-tool
// match below is the actual contract the runtime must satisfy.
const conciergePlatformMCPCreateWorkspaceTool = "mcp__molecule-platform__create_workspace"

// ensureConciergeProvider pins the concierge's LLM provider to `platform` (core
// companion to ensureConciergeModel). It guarantees the env-level provider pin
// that the runtime needs, independent of the template config.yaml (which is NOT
// delivered to the on-box /configs — the box uses the baked base-image config).
//
// SEED-ONLY, keyed on the LLM_PROVIDER secret (NOT MODEL) so an EXISTING
// concierge that already has a MODEL secret still receives the provider pin on
// its next provision, while a provider the customer later pinned in the canvas
// (which writes LLM_PROVIDER) is respected. GATED on the effective model's
// platform-managed namespace so it never forces `platform` onto a BYOK or
// self-hosted concierge running a non-proxy model.
func (h *WorkspaceHandler) ensureConciergeProvider(ctx context.Context, workspaceID string, envVars map[string]string) {
	// Respect an explicit provider already set (customer canvas pick or a prior
	// seed): loadWorkspaceSecrets already injected it into envVars. Do nothing.
	if existing := readStoredProviderSecret(ctx, workspaceID); existing != "" {
		return
	}

	// Effective model for this provision. In production envVars["MODEL"] is
	// ALWAYS populated before this runs — either by loadWorkspaceSecrets +
	// applyRuntimeModelEnv (an existing/customer model) or by ensureConciergeModel
	// just above (the fresh-boot seed) — so reading it here is sufficient and
	// avoids a redundant secret decrypt.
	model := strings.TrimSpace(envVars["MODEL"])
	// Only pin when the model is in the platform-managed namespace that needs it.
	// A non-platform model (e.g. `sonnet`, a BYOK `claude-…`) resolves on its
	// own; forcing `platform` there would mis-route auth and break the agent.
	if !strings.HasPrefix(strings.ToLower(model), platformManagedModelPrefix) {
		return
	}

	envVars["LLM_PROVIDER"] = conciergeProvider
	if setErr := setProviderSecret(ctx, workspaceID, conciergeProvider); setErr != nil {
		log.Printf("Provisioner: concierge %s persist LLM_PROVIDER secret failed: %v (env still seeded for this provision)", workspaceID, setErr)
	} else {
		log.Printf("Provisioner: concierge %s pinned LLM_PROVIDER=%s for platform-managed model %q", workspaceID, conciergeProvider, model)
	}
}

// readStoredProviderSecret returns the decrypted LLM_PROVIDER workspace_secret,
// or "" when none is stored (or on any read/decrypt error — treated as "unset"
// so a transient miss re-seeds rather than wedges). Mirrors readStoredModelSecret.
func readStoredProviderSecret(ctx context.Context, workspaceID string) string {
	var stored []byte
	var version int
	if err := db.DB.QueryRowContext(ctx,
		`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = 'LLM_PROVIDER'`,
		workspaceID).Scan(&stored, &version); err != nil {
		return ""
	}
	dec, err := crypto.DecryptVersioned(stored, version)
	if err != nil {
		return ""
	}
	return string(dec)
}

// setProviderSecret persists (or clears, when provider == "") the LLM_PROVIDER
// workspace_secret. Mirrors setModelSecret (secrets.go). LLM_PROVIDER is the
// provider-slug pin the molecule-runtime wheel reads at highest precedence; it
// is injected into the container as an env var by loadWorkspaceSecrets, so it
// survives restarts and the regenerated on-box config.yaml.
func setProviderSecret(ctx context.Context, workspaceID, provider string) error {
	if provider == "" {
		_, err := db.DB.ExecContext(ctx,
			`DELETE FROM workspace_secrets WHERE workspace_id = $1 AND key = 'LLM_PROVIDER'`,
			workspaceID)
		return err
	}
	encrypted, err := crypto.Encrypt([]byte(provider))
	if err != nil {
		return err
	}
	version := crypto.CurrentEncryptionVersion()
	_, err = db.DB.ExecContext(ctx, `
		INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
		VALUES ($1, 'LLM_PROVIDER', $2, $3)
		ON CONFLICT (workspace_id, key) DO UPDATE
			SET encrypted_value = $2, encryption_version = $3, updated_at = now()
	`, workspaceID, encrypted, version)
	return err
}

// EnsureSelfHostedPlatformAgent installs the org's platform agent (the concierge,
// the org root) on a tenant that has no control plane to do it — i.e. self-hosted
// or local. In SaaS the CP calls InstallPlatformAgent at org-provision time; this
// is the no-CP equivalent. Idempotent: returns early if a kind='platform' root
// already exists (a prior boot, or a CP install in a hybrid setup). The CALLER
// gates this on the MOLECULE_SEED_PLATFORM_AGENT flag (set by the self-hosted
// docker-compose) so CI harnesses and SaaS tenants are unaffected.
func EnsureSelfHostedPlatformAgent(ctx context.Context, database *sql.DB) error {
	var existing string
	err := database.QueryRowContext(ctx,
		`SELECT id FROM workspaces WHERE kind = 'platform' AND parent_id IS NULL LIMIT 1`).Scan(&existing)
	if err == nil {
		return nil // platform agent already present — nothing to do
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("check existing platform agent: %w", err)
	}
	log.Printf("boot: no platform agent present — self-seeding %s (self-hosted)", SelfHostedPlatformAgentID)
	return installPlatformAgent(ctx, database, SelfHostedPlatformAgentID, defaultPlatformAgentName())
}

// OrgIdentityResponse is the body of GET /org/identity.
type OrgIdentityResponse struct {
	// Name is the org's display name (MOLECULE_ORG_NAME, "" when unset).
	Name string `json:"name"`
	// Slug is the org's URL slug (MOLECULE_ORG_SLUG, "" when unset). Empty on
	// a self-hosted stack where no control plane assigns a slug.
	Slug string `json:"slug"`
	// OrgID is the org's UUID (MOLECULE_ORG_ID, "" when unset). Empty on a
	// self-hosted stack where no control plane assigns an org id.
	OrgID string `json:"org_id"`
	// PlatformManagedAvailable reports whether a Molecule LLM proxy is wired
	// into this workspace-server process — i.e. whether platform_managed billing
	// can actually work. True on SaaS (the CP provisioner exports the proxy base
	// URL + usage token), false on a self-hosted stack (no hosted proxy / no
	// credit ledger). The canvas reads this pre-login to decide whether to offer
	// the "Platform (proxy)" billing option or hide it and default to BYOK.
	PlatformManagedAvailable bool `json:"platform_managed_available"`
}

// OrgIdentity handles GET /org/identity (open / CORS-friendly, no auth).
//
// Returns the org's display name from the MOLECULE_ORG_NAME env (empty string
// when unset), its slug (MOLECULE_ORG_SLUG) and id (MOLECULE_ORG_ID) — both ""
// on self-host where no control plane assigns them — plus a
// platform_managed_available capability flag. The canvas topbar reads `name` to
// render "<org name>" without an admin token; the Settings → Organization tab
// reads name+slug+org_id to render the org-identity card on self-host (where the
// control-plane /cp/orgs endpoint does not exist); and the Settings billing card
// reads `platform_managed_available` to decide whether to offer platform-managed
// (proxy) billing — exactly like /health and /buildinfo, it exposes only
// non-sensitive identity/capability signals.
//
// platform_managed_available is true iff a Molecule LLM proxy is configured in
// this process env (PlatformManagedProxyConfigured — the same base-URL + usage-
// token precondition the strip gate enforces). On self-host both are unset, so
// it is false and the canvas hides the "Platform (proxy)" option + defaults BYOK.
//
//	@Summary	Get the org's display name + billing capability
//	@Tags		org
//	@Produce	json
//	@Success	200	{object}	OrgIdentityResponse
//	@Router		/org/identity [get]
func OrgIdentity(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"name":                       os.Getenv("MOLECULE_ORG_NAME"),
		"slug":                       os.Getenv("MOLECULE_ORG_SLUG"),
		"org_id":                     os.Getenv("MOLECULE_ORG_ID"),
		"platform_managed_available": PlatformManagedProxyConfigured(),
	})
}

// MaybeProvisionPlatformAgentOnBoot best-effort provisions a container for the
// self-hosted org's platform agent (the concierge) at boot. The boot-seed
// (EnsureSelfHostedPlatformAgent) only creates the DB row; on a fresh self-host
// that leaves the concierge with no container. This brings it online
// automatically once creds exist.
//
// STRICTLY self-host + best-effort:
//   - The CALLER gates this on MOLECULE_SEED_PLATFORM_AGENT set AND the local
//     Docker provisioner being active (prov != nil, i.e. MOLECULE_ORG_ID unset).
//     SaaS (cpProv) never reaches here.
//   - It looks up the kind='platform' root; if absent (seed disabled / failed)
//     it no-ops. If the container is already running (prov.IsRunning) it no-ops.
//   - Otherwise it kicks off ONE provision via the same path the restart
//     endpoint uses (WorkspaceHandler.RestartByID), which reads the row's
//     runtime ('claude-code' as seeded) + config and provisions accordingly.
//
// On a fresh self-host with no LLM credentials the provision will fail (missing
// key) and the agent stays 'failed' until the user configures BYOK via
// Settings — that's expected. This never fatals and never loops: RestartByID is
// itself debounced/coalesced, and this runs exactly once at boot. Run it in a
// goroutine so a slow Docker pull doesn't delay the HTTP server coming up.
func MaybeProvisionPlatformAgentOnBoot(ctx context.Context, database *sql.DB, prov localProvisionerIsRunning, restartByID func(string)) {
	if prov == nil || restartByID == nil {
		return
	}
	var id, status string
	err := database.QueryRowContext(ctx,
		`SELECT id, status FROM workspaces WHERE kind = 'platform' AND parent_id IS NULL LIMIT 1`).Scan(&id, &status)
	if err == sql.ErrNoRows {
		log.Printf("boot: platform-agent provision skipped — no platform agent row present")
		return
	}
	if err != nil {
		log.Printf("boot: platform-agent provision lookup failed (non-fatal): %v", err)
		return
	}
	// Already online AND a live container? Then it's running — but it may be a
	// concierge that pre-dates the identity overlay (booted as a vanilla
	// claude-code agent with no system-prompt.md). Probe for the concierge
	// identity; if it's missing, re-declare the management MCP plugin in the DB
	// BEFORE restarting so the post-restart reconcile + boot-install see it, then
	// restart ONCE so the provision path re-seeds the overlay. This is what makes
	// the seed idempotent + self-applying on the EXISTING concierge (the
	// deterministic self-hosted id), not just new installs. IsRunning is the
	// authoritative liveness check; status is the cheap one.
	running, _ := prov.IsRunning(ctx, id)
	if running {
		if conciergeIdentityPresent(ctx, prov, id) {
			log.Printf("boot: platform-agent %s already running with concierge identity — skipping", id)
			return
		}
		log.Printf("boot: platform-agent %s running but MISSING concierge identity — re-declaring management MCP and restarting once to apply the system prompt + platform MCP", id)
		if rec, skip := seedTemplatePlugins(ctx, id, []string{conciergePlatformMCPSource}); skip > 0 {
			log.Printf("boot: concierge %s could not re-declare %q plugin (recorded=%d skipped=%d) — management MCP may be absent until next provision", id, conciergePlatformMCPSource, rec, skip)
		}
		go restartByID(id)
		return
	}
	log.Printf("boot: platform-agent %s not running (status=%s) — kicking off best-effort provision", id, status)
	go restartByID(id)
}

// conciergeIdentityPresent reports whether the running concierge container
// already carries the seeded identity (a non-empty /configs/system-prompt.md).
// Used to decide whether a running-but-vanilla concierge needs a one-shot
// restart to pick up the overlay. Best-effort: on a probe error or an empty
// file it returns false (so the safe action — re-seed via restart — is taken).
func conciergeIdentityPresent(ctx context.Context, prov localProvisionerIsRunning, id string) bool {
	reader, ok := prov.(interface {
		ExecRead(ctx context.Context, containerName, filePath string) ([]byte, error)
	})
	if !ok {
		// Can't probe — assume present to avoid a restart loop on a backend
		// that doesn't expose ExecRead.
		return true
	}
	body, err := reader.ExecRead(ctx, provisioner.ContainerName(id), "/configs/system-prompt.md")
	if err != nil {
		return false
	}
	return strings.Contains(string(body), "Org Concierge")
}

// localProvisionerIsRunning is the minimal slice of the local Docker
// provisioner that MaybeProvisionPlatformAgentOnBoot needs — the
// "is this workspace's container live?" probe. The boot helper additionally
// type-asserts for an optional ExecRead (conciergeIdentityPresent) to detect a
// running-but-vanilla concierge; keeping ExecRead off this interface keeps the
// unit-test fake minimal while still letting the real *Provisioner satisfy it.
type localProvisionerIsRunning interface {
	IsRunning(ctx context.Context, workspaceID string) (bool, error)
}

type installPlatformAgentPayload struct {
	// ID is the platform agent's workspace id (a deterministic uuidv5 the
	// control plane derives per org). Required.
	ID string `json:"id" binding:"required"`
	// Name is the display name; defaults to "Org Concierge" when omitted.
	Name string `json:"name"`
}

// InstallPlatformAgent handles POST /admin/org/platform-agent (AdminAuth).
//
// Idempotently installs the platform agent as the org root for THIS tenant. The
// control plane calls it at org-provision time (new orgs) and during the
// existing-org backfill rollout. Safe to call repeatedly.
func InstallPlatformAgent(c *gin.Context) {
	var p installPlatformAgentPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	name := p.Name
	if name == "" {
		name = defaultPlatformAgentName()
	}
	if err := installPlatformAgent(c.Request.Context(), db.DB, p.ID, name); err != nil {
		log.Printf("InstallPlatformAgent: %v (id=%s)", err, p.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "install failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":            "installed",
		"platform_agent_id": p.ID,
		"kind":              models.KindPlatform,
	})
}

// installPlatformAgent performs the idempotent, transactional install described
// in the file header. Separated from the gin handler so integration tests can
// exercise it directly against a real Postgres (the org-anchor migration cannot
// be proven with sqlmock).
func installPlatformAgent(ctx context.Context, database *sql.DB, platformID, name string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// 0. If a different platform root already exists, downgrade it to 'workspace'
	//    so it no longer blocks the partial unique index uniq_workspaces_one_platform_root.
	//    It will then be picked up as an old root and re-parented under the new
	//    platform agent (defense-in-depth: covers pathological cases where a
	//    platform root was created with a non-canonical id, e.g. test fixtures
	//    or a prior failed migration).
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspaces SET kind = 'workspace', updated_at = now()
		WHERE kind = 'platform' AND parent_id IS NULL AND id <> $1
	`, platformID); err != nil {
		return fmt.Errorf("downgrade existing platform root: %w", err)
	}

	// 1. Ensure the platform-agent row exists as a kind='platform' root.
	//    ON CONFLICT keeps it a platform root if it was pre-seeded; the row is
	//    tier 0 and never billed/provisioned as an ordinary workspace EC2.
	//    Status starts as 'offline' — there is no container yet; 'online' is a
	//    green-dot lie until the first heartbeat (core#2508).
	//
	//    parent_name_uniq collision: the platform-agent name is deterministic
	//    and unique per org by construction (e.g. "Org Concierge"). A pre-
	//    existing root with the same name is a data inconsistency; we fail loud
	//    rather than silently rename/reparent, which could orphan billing or
	//    provisioning state. The integration tests use unique names per fixture
	//    to avoid cross-test collision (CR-A RC 10610).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, kind, tier, status, runtime, parent_id, template)
		VALUES ($1, $2, 'platform', 0, 'offline', 'claude-code', NULL, 'platform-agent')
		ON CONFLICT (id) DO UPDATE SET kind = 'platform', runtime = 'claude-code', parent_id = NULL, template = 'platform-agent'
	`, platformID, name); err != nil {
		return fmt.Errorf("upsert platform agent: %w", err)
	}

	// 2. Capture the org's other current roots (everything at parent_id IS NULL
	//    except the platform agent itself). In a one-org tenant DB this is the
	//    single team root; the query tolerates 0 (already installed) or N.
	//    FOR UPDATE prevents a root created mid-install from escaping re-parent
	//    (core#2508).
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM workspaces WHERE parent_id IS NULL AND id <> $1 FOR UPDATE`, platformID)
	if err != nil {
		return fmt.Errorf("select old roots: %w", err)
	}
	var oldRoots []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan old root: %w", err)
		}
		oldRoots = append(oldRoots, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate old roots: %w", err)
	}

	// 3 + 4. Re-parent each old root under the platform agent and move its
	//        org-anchor references in the same transaction. A non-root old root
	//        is kind='workspace', so it does not trip workspaces_platform_root_check.
	for _, root := range oldRoots {
		if _, err := tx.ExecContext(ctx,
			`UPDATE workspaces SET parent_id = $1, updated_at = now() WHERE id = $2`,
			platformID, root); err != nil {
			return fmt.Errorf("re-parent %s: %w", root, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE org_api_tokens SET org_id = $1 WHERE org_id = $2`, platformID, root); err != nil {
			return fmt.Errorf("migrate org_api_tokens for %s: %w", root, err)
		}
		// org_plugin_allowlist has UNIQUE(org_id, plugin_name). A plain UPDATE
		// collides 23505 when N>1 old roots allowlisted the SAME plugin.
		// INSERT…SELECT…ON CONFLICT DO NOTHING deduplicates; DELETE leftovers
		// cleans the old-root rows (core#2508).
		//
		// Column-list matches the actual schema (026_org_plugin_allowlist.up.sql):
		//   id, org_id, plugin_name, enabled_by, enabled_at
		// id is auto-populated by gen_random_uuid(); enabled_by is NOT NULL but
		// preserved from the old root row (same admin who enabled it);
		// enabled_at is preserved verbatim (no audit-time rewrite on
		// re-parenting — the original "when this was enabled" stays stable).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO org_plugin_allowlist (org_id, plugin_name, enabled_by, enabled_at)
			SELECT $1, plugin_name, enabled_by, enabled_at
			FROM org_plugin_allowlist
			WHERE org_id = $2
			ON CONFLICT (org_id, plugin_name) DO NOTHING
		`, platformID, root); err != nil {
			return fmt.Errorf("migrate org_plugin_allowlist for %s: %w", root, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM org_plugin_allowlist WHERE org_id = $1`, root); err != nil {
			return fmt.Errorf("delete old org_plugin_allowlist for %s: %w", root, err)
		}
	}

	return tx.Commit()
}
