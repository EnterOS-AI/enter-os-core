package handlers

// workspace.go — WorkspaceHandler struct, constructor, Create, List, Get,
// and the shared scanWorkspaceRow helper. State/Update/Delete and validators
// live in workspace_crud.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/crypto"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/memory/contract"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/registry"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/pkg/provisionhook"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type WorkspaceHandler struct {
	// broadcaster narrowed from `*events.Broadcaster` to the
	// events.EventEmitter interface (#1814) so tests can substitute a
	// capture-only stub without standing up the real Redis + WS-hub
	// topology. Production callers still pass *events.Broadcaster, which
	// satisfies the interface — see the compile-time assertion in
	// internal/events/broadcaster.go.
	broadcaster events.EventEmitter
	// provisioner narrowed from `*provisioner.Provisioner` to the
	// provisioner.LocalProvisionerAPI interface (#2369) so tests can
	// substitute a stub without standing up the real Docker daemon.
	// Production callers still pass *provisioner.Provisioner via
	// NewWorkspaceHandler, which satisfies the interface — see the
	// compile-time assertion in internal/provisioner/local_provisioner_api.go.
	// Mirrors cpProv's interface-typed field for symmetry across both
	// backends.
	provisioner provisioner.LocalProvisionerAPI
	// cpProv narrowed from `*provisioner.CPProvisioner` to the
	// provisioner.CPProvisionerAPI interface (#1814) so tests can
	// substitute a stub without standing up the real CP HTTP client +
	// auth chain. Production callers still pass *CPProvisioner via
	// SetCPProvisioner, which satisfies the interface — see the
	// compile-time assertion in internal/provisioner/cp_provisioner.go.
	cpProv      provisioner.CPProvisionerAPI
	platformURL string
	configsDir  string // path to workspace-configs-templates/ (for reading templates)
	cacheDir    string // optional runtime-refreshed template cache; overrides configsDir by template id
	// envMutators runs registered EnvMutator plugins right before
	// container Start, after built-in secret loads. Nil = no plugins
	// registered; Registry.Run handles a nil receiver as a no-op so the
	// hot path stays a single nil-pointer compare.
	envMutators *provisionhook.Registry
	// giteaTemplateFetcher is the per-deployment TemplateAssetFetcher
	// (RFC #2843 #24 PR-B) wired from main.go. nil = no fetcher
	// wired (self-host default; the SCAFFOLD gate in
	// collectCPConfigFiles treats nil fetcher as "skip the
	// fetcher", pre-scaffold behavior preserved). When non-nil,
	// the fetcher is called on EVERY provision AND every restart
	// (buildProvisionerConfig is the single source of truth for
	// WorkspaceConfig across both paths) — collectCPConfigFiles
	// reconciles every boot, not just first provision, per the
	// dispatch's "every boot" requirement.
	giteaTemplateFetcher provisioner.TemplateAssetFetcher
	// stopFnOverride is set exclusively in tests to intercept provisioner.Stop
	// calls made by HibernateWorkspace without requiring a running Docker daemon.
	// Always nil in production; the real provisioner path is used when nil.
	stopFnOverride func(ctx context.Context, workspaceID string)
	// enqueueA2A is the function handleA2ADispatchError calls to persist a busy
	// A2A request to a2a_queue. Always EnqueueA2A in production; tests swap it
	// to assert the passed context is detached from the inbound request (#2930).
	enqueueA2A func(ctx context.Context, workspaceID, callerID string, priority int, body []byte, method, idempotencyKey string, expiresAt *time.Time) (string, int, error)
	// provisionTimeouts caches per-runtime provision-timeout values from
	// template manifests (#2054 phase 2). Lazy-init on first scan; see
	// runtime_provision_timeouts.go for the loader contract.
	provisionTimeouts runtimeProvisionTimeoutsCache
	// namespaceCleanupFn is the I5 (RFC #2728) hook called best-effort
	// during purge to delete the workspace's plugin-side namespace.
	// nil = no-op (default for operators who haven't wired the v2
	// memory plugin). main.go sets this to plugin.DeleteNamespace
	// when MEMORY_PLUGIN_URL is configured.
	namespaceCleanupFn func(ctx context.Context, workspaceID string)
	// seedMemoryPlugin is the v2 memory plugin client used by
	// seedInitialMemories (issue #1755) to write workspace-create
	// `initial_memories` into the plugin instead of the legacy
	// `agent_memories` table. nil-safe: when nil, seeding logs a loud
	// warning and skips. After A1 (#1747) there is no SQL fallback —
	// seeded memories with no plugin wired are simply not persisted.
	// main.go attaches this alongside namespaceCleanupFn when
	// MEMORY_PLUGIN_URL is set (memBundle.Plugin).
	seedMemoryPlugin seedMemoryPluginAPI
	// deadProbeMu guards deadProbeAttempts, the per-workspace debounce
	// state used by maybeMarkContainerDead. A transient A2A forward error
	// or a single flaky IsRunning probe must not recycle a recently-alive
	// container (#2929). Protected because ProxyA2A is called concurrently.
	deadProbeMu       sync.Mutex
	deadProbeAttempts map[string]deadProbeRecord

	// asyncWG tracks goroutines launched by goAsync so tests can wait
	// for async DB users (restart, provision) before asserting results.
	// Matches the pattern from main commit 1c3b4ff3.
	asyncWG sync.WaitGroup
}

// deadProbeRecord tracks consecutive "container looks dead" observations
// for a workspace. first marks the initial observation in the current
// window; count is the number of observations since first.
type deadProbeRecord struct {
	count int
	first time.Time
	last  time.Time
}

// seedMemoryPluginAPI is the slice of the v2 memory plugin client that
// seedInitialMemories needs. Defining it as an interface here (parallel
// to memoryPluginAPI in mcp_tools_memory_v2.go) lets tests stub the
// plugin with a capture-only spy and keeps the handler decoupled from
// the concrete *client.Client.
type seedMemoryPluginAPI interface {
	CommitMemory(ctx context.Context, namespace string, body contract.MemoryWrite) (*contract.MemoryWriteResponse, error)
}

// newHandlerHook, when non-nil, is invoked for every WorkspaceHandler
// created via NewWorkspaceHandler. It is nil in production (zero cost);
// the test harness sets it so setupTestDB can drain every handler's
// in-flight async goroutines before swapping the global db.DB. Without
// this, a detached restart goroutine (maybeMarkContainerDead ->
// goAsync(RestartByID) -> runRestartCycle reads db.DB) races the
// db.DB restore in another test's t.Cleanup.
var newHandlerHook func(*WorkspaceHandler)

func (h *WorkspaceHandler) goAsync(fn func()) {
	h.asyncWG.Add(1)
	go func() {
		defer h.asyncWG.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC recovered in goAsync goroutine: %v\n%s", r, debug.Stack())
			}
		}()
		fn()
	}()
}

func (h *WorkspaceHandler) waitAsyncForTest() {
	h.asyncWG.Wait()
}

// globalAsync tracks goroutines launched by globalGoAsync — the
// equivalent of WorkspaceHandler.goAsync for sibling handlers that
// don't carry a *WorkspaceHandler reference (SecretsHandler /
// PluginsHandler / AdminPluginDriftHandler / ChannelHandler /
// MCPHandler / RegistryHandler), and for callers of package-level
// free functions (a2a_proxy_helpers extractAndUpsertTokenUsage).
//
// Forward-port of RFC internal#524 Layer 1 deliverable 2: the
// canonical db.DB race fix lives at workspace.go:goAsync / asyncWG,
// but ~25 sibling bare-`go` sites still write to db.DB outside any
// WorkspaceHandler's drain window. globalAsync gives them the same
// drain hook (waitGlobalAsyncForTest, drained from drainTestAsync)
// so a test's t.Cleanup db.DB restore cannot race a detached
// goroutine spawned by any sibling handler.
//
// Zero-cost in production (a single sync.WaitGroup Add/Done per
// fire-and-forget call, no test-only branching).
var globalAsync sync.WaitGroup

// globalGoAsync schedules fn on a detached goroutine tracked by
// globalAsync. Use this in package-internal callers that don't have
// a *WorkspaceHandler receiver to thread h.goAsync through.
//
// When a *WorkspaceHandler IS available, prefer h.goAsync — it lets
// per-handler tests (waitAsyncForTest) wait without disturbing
// unrelated handlers' inflight work.
func globalGoAsync(fn func()) {
	globalAsync.Add(1)
	go func() {
		defer globalAsync.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC recovered in globalGoAsync goroutine: %v\n%s", r, debug.Stack())
			}
		}()
		fn()
	}()
}

// waitGlobalAsyncForTest blocks until every globalGoAsync goroutine
// finishes. Called from drainTestAsync's cleanup chain in the test
// harness; production code never calls it.
func waitGlobalAsyncForTest() {
	globalAsync.Wait()
}

func NewWorkspaceHandler(b events.EventEmitter, p *provisioner.Provisioner, platformURL, configsDir string) *WorkspaceHandler {
	h := &WorkspaceHandler{
		broadcaster:       b,
		platformURL:       platformURL,
		configsDir:        configsDir,
		deadProbeAttempts: make(map[string]deadProbeRecord),
		enqueueA2A:        EnqueueA2A,
	}
	// Only assign p when the concrete pointer is non-nil. Without this
	// guard, a `NewWorkspaceHandler(..., nil, ...)` call (which all the
	// non-Docker test fixtures use) yields a typed-nil interface — the
	// `if h.provisioner != nil` checks scattered through the SaaS-vs-
	// Docker fork would incorrectly evaluate as non-nil and route into
	// the Docker path. Mirrors the pattern documented in the Go FAQ
	// "Why is my nil error value not equal to nil?".
	if p != nil {
		h.provisioner = p
	}
	if newHandlerHook != nil {
		newHandlerHook(h)
	}
	return h
}

func (h *WorkspaceHandler) WithTemplateCacheDir(cacheDir string) *WorkspaceHandler {
	h.cacheDir = cacheDir
	return h
}

// WithNamespaceCleanup wires the I5 hook (RFC #2728) so workspace
// purge can drop the plugin's `workspace:<id>` namespace. main.go
// passes a closure over plugin.DeleteNamespace; tests pass a stub.
// Nil-safe: omitting this leaves namespaceCleanupFn nil, which the
// purge path treats as a no-op.
func (h *WorkspaceHandler) WithNamespaceCleanup(fn func(ctx context.Context, workspaceID string)) *WorkspaceHandler {
	h.namespaceCleanupFn = fn
	return h
}

// WithSeedMemoryPlugin wires the v2 memory plugin so
// seedInitialMemories (issue #1755) routes workspace-create
// `initial_memories` through the plugin instead of the legacy
// `agent_memories` table. main.go passes memBundle.Plugin (a
// `*client.Client`); tests pass a stub matching the
// seedMemoryPluginAPI interface. Nil-safe: omitting this leaves the
// field nil and seedInitialMemories logs a warning + skips on each
// invocation.
func (h *WorkspaceHandler) WithSeedMemoryPlugin(p seedMemoryPluginAPI) *WorkspaceHandler {
	h.seedMemoryPlugin = p
	return h
}

// SetCPProvisioner wires the control plane provisioner for SaaS tenants.
// Auto-activated when MOLECULE_ORG_ID is set (no manual config needed).
//
// Parameter is the CPProvisionerAPI interface (#1814) — production passes
// the *CPProvisioner from NewCPProvisioner; tests pass a stub.
func (h *WorkspaceHandler) SetCPProvisioner(cp provisioner.CPProvisionerAPI) {
	h.cpProv = cp
}

// SetEnvMutators wires a provisionhook.Registry into the handler. Plugins
// living in separate repos register on the same Registry instance during
// boot (see cmd/server/main.go) and main.go calls this setter once before
// router.Setup. Re-callable for tests but not safe under concurrent
// provisions — only invoke during single-threaded init.
func (h *WorkspaceHandler) SetEnvMutators(r *provisionhook.Registry) {
	h.envMutators = r
}

// SetGiteaTemplateFetcher wires the per-deployment TemplateAssetFetcher
// (RFC #2843 #24 PR-B). Nil is fine (self-host default; the SCAFFOLD
// gate in collectCPConfigFiles treats nil as "skip the fetcher").
// Production wires a giteaTemplateAssetFetcher with baseURL +
// per-identity READ-ONLY Gitea PAT (threaded from Infisical SSOT).
// Tests pass a stub.
func (h *WorkspaceHandler) SetGiteaTemplateFetcher(f provisioner.TemplateAssetFetcher) {
	h.giteaTemplateFetcher = f
}

// TokenRegistry returns the provisionhook.Registry so the router can
// wire the GET /admin/github-installation-token handler without coupling
// to WorkspaceHandler's internals. Returns nil when no plugin has been
// registered (dev / self-hosted deployments without a GitHub App).
func (h *WorkspaceHandler) TokenRegistry() *provisionhook.Registry {
	return h.envMutators
}

// Create handles POST /workspaces
func (h *WorkspaceHandler) Create(c *gin.Context) {
	var payload models.CreateWorkspacePayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace payload"})
		return
	}

	// #685/#688: validate field lengths and reject injection characters before
	// any DB or provisioner interaction.
	if err := validateWorkspaceFields(payload.Name, payload.Role, payload.Model, payload.Runtime); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace fields"})
		return
	}

	id := uuid.New().String()
	if h.IsSaaS() {
		// SaaS hard gate: every hosted workspace gets its own sibling
		// EC2 instance, so T4 is the only meaningful runtime boundary.
		// Do not trust stale clients/templates that still send T1/T2/T3.
		payload.Tier = 4
	} else if payload.Tier == 0 {
		// Self-hosted default remains T3. Lower tiers (T1 sandboxed,
		// T2 standard) stay explicit opt-ins for low-trust local agents.
		payload.Tier = h.DefaultTier()
	}

	// Detect runtime + default model from template config.yaml when the
	// caller omitted them. Must happen before DB insert so persisted
	// fields match the template's intent.
	//
	// Model default pre-fills the hermes-trap gap (PR #1714 + TemplatePalette
	// patch): any create path (canvas dialog, TemplatePalette, direct API)
	// that names a template but forgets a model slug now inherits the
	// template's `runtime_config.model` — without it, hermes-agent falls
	// back to its compiled-in Anthropic default and 401s when the user's
	// key is for a different provider. Non-hermes runtimes are unaffected
	// (the server still passes model through, they just don't use it).
	// runtimeExplicitlyRequested is true when the caller expressed intent for
	// a SPECIFIC runtime — either by passing `runtime` directly, or by naming
	// a `template` (a template encodes a runtime). When true, we must NOT
	// silently fall back to the default runtime if that intent can't be honored: that
	// is the molecule-controlplane#188 / #184 contract violation (caller asks
	// for codex/hermes/openclaw, gets a default-runtime workspace, 201, no error — a
	// false success). #188 mandates fail-closed (error+notify) on mismatch,
	// not an advisory degrade. The legitimate "no template, no runtime →
	// default-runtime path (bare {"name":...}) is unaffected.
	runtimeExplicitlyRequested := payload.Runtime != "" || payload.Template != ""
	templateRuntimeResolved := payload.Runtime != ""
	if payload.Template != "" && (payload.Runtime == "" || payload.Model == "") {
		// #226: payload.Template is attacker-controllable. resolveInsideRoot
		// rejects absolute paths and any ".." that escapes configsDir so the
		// provisioner can't be pointed at host directories.
		candidatePath, resolveErr := resolveWorkspaceTemplatePath(h.configsDir, h.cacheDir, payload.Template)
		if resolveErr != nil {
			log.Printf("Create: invalid template path %q: %v", payload.Template, resolveErr)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid template"})
			return
		}
		cfgData, readErr := os.ReadFile(filepath.Join(candidatePath, "config.yaml"))
		if readErr != nil {
			log.Printf("Create: could not read config.yaml for template %q: %v", payload.Template, readErr)
		}
		// Two-pass line scanner: the old parser found top-level `runtime:`
		// by substring match on trimmed lines. We extend it to also find
		// the nested `runtime_config.model:` (new format) and top-level
		// `model:` (legacy format). A minimal state var tracks whether
		// we're inside the runtime_config block based on indentation.
		inRuntimeConfig := false
		for _, rawLine := range strings.Split(string(cfgData), "\n") {
			// Track indentation to detect block transitions.
			trimmed := strings.TrimLeft(rawLine, " \t")
			indented := len(rawLine) > len(trimmed)
			if !indented {
				// Left the runtime_config block (or never entered it).
				inRuntimeConfig = strings.HasPrefix(trimmed, "runtime_config:")
			}
			stripped := strings.TrimSpace(rawLine)
			switch {
			case payload.Runtime == "" && !indented && strings.HasPrefix(stripped, "runtime:") && !strings.HasPrefix(stripped, "runtime_config"):
				payload.Runtime = strings.TrimSpace(strings.TrimPrefix(stripped, "runtime:"))
				if payload.Runtime != "" {
					templateRuntimeResolved = true
				}
			case payload.Model == "" && !indented && strings.HasPrefix(stripped, "model:"):
				// Legacy top-level `model:` — pre-runtime_config templates.
				payload.Model = strings.Trim(strings.TrimSpace(strings.TrimPrefix(stripped, "model:")), `"'`)
			case payload.Model == "" && indented && inRuntimeConfig && strings.HasPrefix(stripped, "model:"):
				// Nested `runtime_config.model:` — current format (hermes etc.).
				payload.Model = strings.Trim(strings.TrimSpace(strings.TrimPrefix(stripped, "model:")), `"'`)
			}
			if payload.Runtime != "" && payload.Model != "" {
				break
			}
		}
	}
	// Fail-closed (molecule-controlplane#188 / #184): if the caller expressed
	// intent for a specific runtime (passed `runtime`, or named a `template`)
	// but we could NOT resolve a concrete runtime from it (template's
	// config.yaml unreadable, or it has no `runtime:` key), DO NOT silently
	// substitute the default runtime and return 201 — that is the silent contract
	// violation that produced 5/5 wrong workspaces and a false codex E2E pass.
	// Return 422 so the caller learns the requested runtime was not honored.
	// The platform-side CP fix (controlplane#188) is the sibling gate; this
	// closes the ws-server `Create` boundary the product UI actually hits.
	if payload.Runtime == "" && runtimeExplicitlyRequested && !templateRuntimeResolved {
		log.Printf("Create: FAIL-CLOSED (controlplane#188) — template=%q requested but runtime could not be resolved; refusing silent default-runtime fallback", payload.Template)
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":    "runtime could not be resolved from the requested template; refusing to silently provision the default runtime (controlplane#188). Pass an explicit \"runtime\", or use a template whose config.yaml declares one.",
			"template": payload.Template,
			"code":     "RUNTIME_UNRESOLVED",
		})
		return
	}
	if payload.Runtime == "" {
		if payload.External {
			payload.Runtime = "external"
		} else {
			// Legitimate default path: no template AND no runtime requested
			// (bare {"name":...}) — claude-code is the intended default here.
			payload.Runtime = "claude-code"
		}
	}

	if payload.External && !isExternalLikeRuntime(payload.Runtime) {
		log.Printf("Create: FAIL-CLOSED — external workspace requested with non-external runtime %q", payload.Runtime)
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			// Build the runtime list from the externalLikeRuntimes SSOT
			// (single source of truth) so adding a new BYO-compute
			// meta-runtime only requires updating the SSOT in
			// runtime_registry.go — see TestExternalLikeRuntimesConsistent.
			"error":   fmt.Sprintf("external workspaces must use runtime %s", joinExternalLikeRuntimesForMessage()),
			"runtime": payload.Runtime,
			"code":    "RUNTIME_UNSUPPORTED",
		})
		return
	}
	if payload.Runtime != "" && !isExternalLikeRuntime(payload.Runtime) {
		if _, ok := knownRuntimes[payload.Runtime]; !ok {
			log.Printf("Create: FAIL-CLOSED — unsupported runtime %q", payload.Runtime)
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":   "unsupported workspace runtime",
				"runtime": payload.Runtime,
				"code":    "RUNTIME_UNSUPPORTED",
			})
			return
		}
	}

	// SSOT (CTO 2026-05-22, feedback_workspace_model_required_no_platform_default_dynamic_credential_intake):
	// model is REQUIRED user input for SPAWNED-runtime workspaces. The
	// platform must not provide a default; the runtime must not fall back.
	// The decision belongs to the user (or to the agent acting on the
	// user's behalf), never to the platform.
	//
	// Empirical trigger: Code Reviewer 5ba15d7e was created with
	// `{"name":"Code Reviewer","role":"...","runtime":"codex",...}` (no
	// model). The legacy `DefaultModel(runtime)` fallback in
	// provisionWorkspace returned `"anthropic:claude-opus-4-7"`. Codex
	// adapter only supports openai-* providers — it wedged forever with
	// `codex adapter: workspace config picks provider='anthropic' but
	// it is not in the providers registry`. PATCH /workspaces/:id
	// explicitly disallows updating model (the comment literally reads
	// `model not patchable`), so the only recovery path was SQL UPDATE
	// or delete+recreate.
	//
	// External workspaces are EXEMPT — they intentionally do not spawn
	// a Docker container or run an adapter; they delegate to a registered
	// URL (see provision.go: "external is a first-class runtime that
	// intentionally does NOT spawn a Docker container"). The MODEL_REQUIRED
	// gate is meaningful for spawned-runtime workspaces where the model
	// id drives provider selection at adapter init. For external workspaces
	// the contract is the URL, not the model — requiring it would be
	// ceremony with no payoff, and would 422 every legitimate "register
	// my agent at https://..." flow. The SSOT directive concerns
	// platform-side defaults; an external workspace genuinely has no
	// "model decision" for the user to make.
	//
	// Fail-closed at the Create boundary so the caller learns the
	// contract immediately — same shape as the controlplane#188
	// runtime-unresolved gate above. Caller fixes the request, no
	// EC2 launched, no stuck workspace, no operator paging.
	isExternal := payload.External || isExternalLikeRuntime(payload.Runtime)
	if payload.Model == "" && !isExternal {
		log.Printf("Create: FAIL-CLOSED — model is required (runtime=%q template=%q); refusing the silent DefaultModel fallback per CTO 2026-05-22 SSOT directive", payload.Runtime, payload.Template)
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":    "model is required and has no platform-side default — pass an explicit \"model\" in the request body, or use a \"template\" whose config.yaml declares one. See feedback_workspace_model_required_no_platform_default_dynamic_credential_intake for the contract.",
			"runtime":  payload.Runtime,
			"template": payload.Template,
			"code":     "MODEL_REQUIRED",
		})
		return
	}

	// internal#718 P4 PR-2: ONLY-REGISTERED validation at the create boundary —
	// FLIPPED from WARN to HARD-REJECT (was the P2-B WARN-mode signal).
	//
	// For a runtime the provider registry knows (first-party:
	// claude-code/codex/hermes/openclaw) this checks the (runtime, model) pair
	// against the registry's native model set. Fails OPEN for runtimes the
	// registry doesn't know (langgraph/external/kimi/mock/federated) so
	// non-first-party / federated flows are UNCHANGED. Skipped for external
	// workspaces (the URL is the contract, not the model — see MODEL_REQUIRED
	// rationale above).
	//
	// THE FLIP (was WARN, now 422):
	//   * P2-B carried the gate in WARN mode (X-Molecule-Model-Unregistered
	//     response header + log line, create proceeds) because the legacy
	//     colon-namespaced BYOK vocabulary ('anthropic:claude-opus-4-7' etc.)
	//     was live across the create corpus but not yet in the registry's
	//     exact-id model sets — hard-rejecting would have 422'd legitimate
	//     existing flows.
	//   * P4 PR-1 reconciled that colon vocab into the registry as
	//     first-class native-set entries (each runtime native set now lists
	//     both bare/slash AND colon forms for the BYOK ids the live corpus
	//     uses; openclaw's pre-existing colon-form precedent extended to
	//     claude-code). DeriveProvider / Manifest.ModelsForRuntime now
	//     resolves every legitimate model in the corpus.
	//   * With the reconcile landed, an unregistered (runtime, model) pair
	//     is a real misconfiguration — the corpus has no legitimate model
	//     this validator now rejects. We flip to 422
	//     UNREGISTERED_MODEL_FOR_RUNTIME so the caller fails LOUDLY at the
	//     boundary instead of provisioning a workspace that will wedge at
	//     adapter init (the codex 'anthropic:claude-opus-4-7' wedge class
	//     the MODEL_REQUIRED gate also targets).
	//
	// The registry model set is code-generated from the canonical
	// providers.yaml (P2-A artifact); the check stays in sync with the SSOT
	// via the verify-providers-gen + sync-providers-yaml CI gates.
	if !isExternal {
		if ok, why := validateRegisteredModelForRuntime(payload.Runtime, payload.Model); !ok {
			log.Printf("Create: 422 UNREGISTERED_MODEL_FOR_RUNTIME (runtime=%q model=%q): %s [internal#718 P4 PR-2 hard-reject]", payload.Runtime, payload.Model, why)
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":   why,
				"runtime": payload.Runtime,
				"model":   payload.Model,
				"code":    "UNREGISTERED_MODEL_FOR_RUNTIME",
			})
			return
		}
		// issue #2172 (provider-side companion): once the (runtime, model)
		// pair is known to be registered, confirm the DERIVED provider
		// (the one the adapter will resolve at boot) is a known provider
		// in the providers.yaml catalog. Live trigger (adk-demo Assistant,
		// 2026-06-03): the model-side check passed for a registry-resident
		// model whose derived provider name was NOT in the providers list,
		// so the save was accepted and the agent wedged at boot with
		// "provider=X not in providers registry". This check is a
		// defense-in-depth registry-consistency guard: by construction a
		// model in a runtime's native set derives to a provider that IS in
		// the catalog, so the rejection path is primarily a registry-data
		// invariant — any future drift between `providers:` and `runtimes:`
		// fails the create with a clear pointer to the missing provider
		// rather than silently wedging the agent. Fails open for runtimes
		// the registry doesn't know (langgraph/external/kimi/mock/federated
		// — the federation contract the model-side check also honors).
		if ok, why := validateDerivedProviderInRegistry(payload.Runtime, payload.Model); !ok {
			log.Printf("Create: 422 DERIVED_PROVIDER_NOT_IN_REGISTRY (runtime=%q model=%q): %s [issue #2172 hard-reject]", payload.Runtime, payload.Model, why)
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":   why,
				"runtime": payload.Runtime,
				"model":   payload.Model,
				"code":    "DERIVED_PROVIDER_NOT_IN_REGISTRY",
			})
			return
		}

		// core#2608 (CTO 2026-06-11 "hard fail — why does it even pass and
		// create a not-working workspace"): a registered model that derives
		// BYOK with no satisfiable credential in scope fails HERE, before any
		// row exists, with the same actionable message provisioning would
		// have produced minutes later on a dead node.
		secretKeys := make([]string, 0, len(payload.Secrets))
		for k := range payload.Secrets {
			secretKeys = append(secretKeys, k)
		}
		if ok, why := validateBYOKCredentialSatisfiable(c.Request.Context(), payload.Runtime, payload.Model, secretKeys); !ok {
			log.Printf("Create: 422 MISSING_BYOK_CREDENTIAL (runtime=%q model=%q): %s [core#2608 create-boundary hard-reject]", payload.Runtime, payload.Model, why)
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":   why,
				"runtime": payload.Runtime,
				"model":   payload.Model,
				"code":    "MISSING_BYOK_CREDENTIAL",
			})
			return
		}
	}

	ctx := c.Request.Context()

	// Convert empty role to NULL
	var role interface{}
	if payload.Role != "" {
		role = payload.Role
	}

	// Validate and convert workspace_dir
	var workspaceDir interface{}
	if payload.WorkspaceDir != "" {
		if err := validateWorkspaceDir(payload.WorkspaceDir); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace directory"})
			return
		}
		workspaceDir = payload.WorkspaceDir
	}

	// #65: validate workspace_access, default to "none".
	workspaceAccess := payload.WorkspaceAccess
	if workspaceAccess == "" {
		workspaceAccess = provisioner.WorkspaceAccessNone
	}
	if err := provisioner.ValidateWorkspaceAccess(workspaceAccess, payload.WorkspaceDir); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace access"})
		return
	}
	if err := validateWorkspaceCompute(payload.Compute); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Begin a transaction so the workspace row and any initial secrets are
	// committed atomically.  A secret-encrypt or DB error rolls back the
	// workspace insert so we never leave a workspace row with missing secrets.

	// SSRF guard: validate workspace URL before starting any DB transaction.
	// registry.go:324 calls this same guard for agent self-registration;
	// the admin-create path must be covered too (core#212).
	// Must stay above BeginTx so the rejection path never touches the DB.
	if payload.URL != "" {
		if err := validateAgentURL(payload.URL); err != nil {
			log.Printf("Create: workspace URL rejected: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsafe workspace URL: " + err.Error()})
			return
		}
	}

	// core#2609: no explicit parent -> default under the org's platform root.
	// Live failure this prevents: the enter-os concierge provisioned its team
	// via the management surface, both workspaces landed parent_id NULL, and
	// every delegation died with "cannot communicate per hierarchy rules".
	// Best-effort: no platform root (or an ambiguous >1) leaves NULL intact,
	// preserving bootstrap/self-host multi-root behavior.
	//
	// core#2697: defaultCreateParentID also falls back to the SOLE plain root
	// workspace when the org has no platform-agent — so tenants provisioned
	// without a concierge (e.g. JRS's lone SEO Agent) nest new workspaces under
	// that root instead of scattering them as bare-root siblings.
	if payload.ParentID == nil {
		if rootID := defaultCreateParentID(ctx); rootID != "" {
			payload.ParentID = &rootID
		}
	}

	tx, txErr := db.DB.BeginTx(ctx, nil)
	if txErr != nil {
		log.Printf("Create workspace: begin tx error: %v", txErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	maxConcurrent := payload.MaxConcurrentTasks
	if maxConcurrent <= 0 {
		maxConcurrent = models.DefaultMaxConcurrentTasks
	}
	// delivery_mode: explicit payload value (validated below), else default
	// to push (the schema default + pre-#2339 behavior). Validated here, not
	// in workspace_provision.go, so a bad value fails the create cleanly
	// instead of mid-provision after side effects.
	deliveryMode := payload.DeliveryMode
	if deliveryMode == "" {
		deliveryMode = models.DeliveryModePush
	}
	if !models.IsValidDeliveryMode(deliveryMode) {
		tx.Rollback() //nolint:errcheck
		c.JSON(http.StatusBadRequest, gin.H{"error": "delivery_mode must be 'push' or 'poll'"})
		return
	}
	// Insert workspace with runtime + delivery_mode persisted in DB (inside transaction).
	//
	// Auto-suffix on (parent_id, name) collision via insertWorkspaceWithNameRetry:
	// the partial-unique index `workspaces_parent_name_uniq` (migration
	// 20260506000000) protects /org/import from TOCTOU duplicates, but the
	// pre-fix Canvas Create path bubbled the raw pq violation as a 500 on
	// double-click. Helper retries with " (2)", " (3)", … up to maxNameSuffix,
	// returns the actually-persisted name (which we MUST thread back into
	// payload + broadcast so the canvas displays what the DB has).
	const insertWorkspaceSQL = `
		INSERT INTO workspaces (id, name, role, tier, runtime, template, status, parent_id, workspace_dir, workspace_access, budget_limit, max_concurrent_tasks, delivery_mode)
		VALUES ($1, $2, $3, $4, $5, $6, 'provisioning', $7, $8, $9, $10, $11, $12)
	`
	insertArgs := []any{id, payload.Name, role, payload.Tier, payload.Runtime, payload.Template, payload.ParentID, workspaceDir, workspaceAccess, payload.BudgetLimit, maxConcurrent, deliveryMode}
	persistedName, currentTx, err := insertWorkspaceWithNameRetry(
		ctx,
		tx,
		// Closure captures ctx so the retry tx uses the same request context;
		// nil opts mirrors the original BeginTx call above.
		func(ctx context.Context) (*sql.Tx, error) { return db.DB.BeginTx(ctx, nil) },
		payload.Name,
		1, // args[1] is name
		insertWorkspaceSQL,
		insertArgs,
	)
	if err != nil {
		if currentTx != nil {
			currentTx.Rollback() //nolint:errcheck
		}
		if errors.Is(err, errWorkspaceNameExhausted) {
			log.Printf("Create workspace: name suffix exhausted for base %q under parent %v", payload.Name, payload.ParentID)
			c.JSON(http.StatusConflict, gin.H{"error": "workspace name already in use; please pick a different name"})
			return
		}
		log.Printf("Create workspace error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace"})
		return
	}
	// Helper may have rolled back the original tx and returned a fresh one;
	// rebind so the remaining secrets-INSERT + Commit run on the live tx.
	tx = currentTx
	if persistedName != payload.Name {
		log.Printf("Create workspace %s: name collision auto-suffix %q -> %q", id, payload.Name, persistedName)
		payload.Name = persistedName
	}

	if !workspaceComputeIsZero(payload.Compute) {
		computeJSON, encErr := workspaceComputeJSON(payload.Compute)
		if encErr != nil {
			tx.Rollback() //nolint:errcheck
			log.Printf("Create workspace %s: failed to encode compute config: %v", id, encErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode compute config"})
			return
		}
		if _, dbErr := tx.ExecContext(ctx,
			`UPDATE workspaces SET compute = $2::jsonb, updated_at = now() WHERE id = $1`,
			id, computeJSON); dbErr != nil {
			tx.Rollback() //nolint:errcheck
			log.Printf("Create workspace %s: failed to persist compute config: %v", id, dbErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save compute config"})
			return
		}
	}

	// RFC#2843 #33: persist the template name so the RESTART / re-provision path
	// can re-deliver the SAME template's config.yaml + prompts (and re-run the
	// declared-plugin reconcile). Without this, the auto-restart cycle rebuilds
	// the provision payload with template="" → the SaaS re-provision has no
	// TemplateIdentity → config degrades to a 218-byte stub and skills drop on
	// every restart. Non-fatal: a write failure must not abort an otherwise-good
	// create (the row is live; restart just falls back to the old stub behavior).
	if t := strings.TrimSpace(payload.Template); t != "" {
		if _, dbErr := tx.ExecContext(ctx,
			`UPDATE workspaces SET template = $2, updated_at = now() WHERE id = $1`,
			id, t); dbErr != nil {
			log.Printf("Create workspace %s: failed to persist template %q: %v (continuing — restart will fall back to stub config)", id, t, dbErr)
		}
	}

	// Persist initial secrets from the create payload (inside same transaction).
	// nil/empty map is a no-op.  Any failure rolls back the workspace insert
	// so we never have a workspace row without its intended secrets.
	//
	// Vendor-key guard, CREATE scope (core#2608 companion): the stored-state
	// resolver cannot see payload.Model yet (the MODEL secret persists after
	// commit), so a byok create would always misresolve platform_managed here
	// and reject the very credential the create-boundary gate REQUIRES in the
	// payload. Derive from the CREATE inputs instead: when (runtime, model,
	// payload keys) resolves a non-platform arm, vendor keys in this payload
	// are the atomic-byok-create shape and are allowed; platform-resolving
	// creates keep the full guard.
	allowVendorKeysForByokCreate := false
	if m, regErr := providerRegistry(); regErr == nil && m != nil {
		keys := make([]string, 0, len(payload.Secrets))
		for k := range payload.Secrets {
			keys = append(keys, k)
		}
		if prov, dErr := m.DeriveProvider(payload.Runtime, strings.TrimSpace(payload.Model), keys); dErr == nil && !prov.IsPlatform() {
			allowVendorKeysForByokCreate = true
		}
	}
	for k, v := range payload.Secrets {
		if !allowVendorKeysForByokCreate && rejectPlatformManagedDirectLLMBypassForWorkspace(c, id, k) {
			tx.Rollback() //nolint:errcheck
			return
		}
		encrypted, encErr := crypto.Encrypt([]byte(v))
		if encErr != nil {
			tx.Rollback() //nolint:errcheck
			log.Printf("Create workspace %s: failed to encrypt secret %q: %v", id, k, encErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt secret: " + k})
			return
		}
		version := crypto.CurrentEncryptionVersion()
		if _, dbErr := tx.ExecContext(ctx, `
			INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (workspace_id, key) DO UPDATE
				SET encrypted_value = $3, encryption_version = $4, updated_at = now()
		`, id, k, encrypted, version); dbErr != nil {
			tx.Rollback() //nolint:errcheck
			log.Printf("Create workspace %s: failed to persist secret %q: %v", id, k, dbErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save secret: " + k})
			return
		}
	}

	if commitErr := tx.Commit(); commitErr != nil {
		log.Printf("Create workspace %s: transaction commit failed: %v", id, commitErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace"})
		return
	}

	// Persist canvas-selected model as the MODEL workspace_secret so it
	// survives restart and is picked up by CP user-data when regenerating
	// /configs/config.yaml. Without this, the applyRuntimeModelEnv
	// fallback chain (workspace_provision.go) cannot recover the user's
	// choice on a Restart payload (which rebuilds from the workspaces
	// row, where there is no model column), and hermes silently boots
	// with the template-default model. See failed-workspace 95ed3ff2
	// (2026-05-02): canvas POSTed minimax/MiniMax-M2.7-highspeed,
	// MODEL_PROVIDER was never written, container fell through to
	// nousresearch/hermes-4-70b, derive-provider.sh produced the wrong
	// provider, hermes gateway 401'd, /health poll failed,
	// molecule-runtime never registered.
	//
	// internal#718 P4 closure: the prior `setProviderSecret` write
	// (LLM_PROVIDER row, derived from the canvas-supplied
	// payload.LLMProvider OR from deriveProviderFromModelSlug) has been
	// REMOVED. The provider is now DERIVED at every decision point from
	// (runtime, model) via the registry — billing (P2-B), CP user-data
	// (this PR's CP-side commit replaces resolveModelAndProvider's
	// env["LLM_PROVIDER"] read with a DeriveProvider call), and
	// validation (P3 PR-C provisioner). Storing it is pure write-ghost
	// with no remaining consumer. `payload.LLMProvider` is preserved on
	// the request struct for backward-compatibility with older canvases
	// that still send it; the value is intentionally ignored here.
	//
	// The setModelSecret write is non-fatal: a failure here logs and
	// continues so the workspace row stays consistent. The runtime can
	// still boot (with the template default) and a later
	// Save+Restart will re-persist via the SecretsHandler endpoints.
	if payload.Model != "" {
		if err := setModelSecret(ctx, id, payload.Model); err != nil {
			log.Printf("Create workspace %s: failed to persist MODEL_PROVIDER %q: %v (non-fatal)", id, payload.Model, err)
		}
	}

	// Insert canvas layout — non-fatal: workspace can be dragged into position later
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO canvas_layouts (workspace_id, x, y) VALUES ($1, $2, $3)
	`, id, payload.Canvas.X, payload.Canvas.Y); err != nil {
		log.Printf("Create: canvas layout insert failed for %s (workspace will appear at 0,0): %v", id, err)
	}

	// Seed initial memories from the create payload (issue #1050).
	// Non-fatal: failures are logged but don't block workspace creation.
	h.seedInitialMemories(ctx, id, payload.InitialMemories)

	// Broadcast provisioning event. Include `runtime` so the canvas can
	// populate the Runtime pill on the side panel immediately — without it
	// the node lives as "runtime: unknown" until something refetches the
	// workspace row (which nothing does during provisioning).
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), id, map[string]interface{}{
		"name":    payload.Name,
		"tier":    payload.Tier,
		"runtime": payload.Runtime,
	})

	// External workspaces: no container provisioning. Two shapes:
	//   (a) URL supplied up-front  — the operator already has their
	//       agent running somewhere reachable; we mark it online
	//       immediately. Legacy flow, preserved for callers that
	//       don't need the copy-this-snippet UX (org-import, etc.).
	//   (b) URL omitted             — the operator will install
	//       molecule-sdk-python or another A2A server later. We
	//       mint a workspace_auth_token now and return it alongside
	//       workspace_id + platform_url so the canvas UI can show
	//       one copy-paste connection snippet. Status is set to
	//       "awaiting_agent" — distinct from "provisioning" (which
	//       implies docker work in flight) so the canvas can render
	//       a "waiting for external agent to connect" state without
	//       tripping the provisioning-timeout UX.
	if payload.External || isExternalLikeRuntime(payload.Runtime) {
		var connectionToken string
		if payload.URL != "" {
			// URL already validated by validateAgentURL above (before BeginTx).
			// Now persist it: the external URL is set after the workspace row
			// commits so that a failed URL UPDATE doesn't roll back the row.
			// Preserve BYO-compute runtime label (kimi, kimi-cli, external) —
			// don't coerce to generic "external" so the canvas can show the
			// correct runtime name in the node card.
			if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET url = $1, status = $2, runtime = $3, updated_at = now() WHERE id = $4`, payload.URL, models.StatusOnline, normalizeExternalRuntime(payload.Runtime), id); err != nil {
				log.Printf("External workspace: failed to update URL/status for %s: %v", id, err)
			}
			if err := db.CacheURL(ctx, id, payload.URL); err != nil {
				log.Printf("External workspace: failed to cache URL for %s: %v", id, err)
			}
			h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOnline), id, map[string]interface{}{
				"name": payload.Name, "external": true,
			})
		} else {
			// Pre-register flow: mint a token and park the workspace
			// in awaiting_agent. First POST /registry/register call
			// from the external agent (with this token + its URL)
			// flips the row to online.
			// Preserve BYO-compute runtime label (kimi, kimi-cli, external).
			if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, runtime = $2, updated_at = now() WHERE id = $3`, models.StatusAwaitingAgent, normalizeExternalRuntime(payload.Runtime), id); err != nil {
				log.Printf("External workspace: failed to update status for %s: %v", id, err)
			}
			tok, tokErr := wsauth.IssueToken(ctx, db.DB, id)
			if tokErr != nil {
				log.Printf("External workspace %s: token issuance failed: %v", id, tokErr)
				// Non-fatal — the workspace row still exists; the
				// operator can call POST /workspaces/:id/external/rotate
				// later to recover. Return a 201 with a hint instead of
				// 500'ing a partial-success write.
			} else {
				connectionToken = tok
			}
			h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceAwaitingAgent), id, map[string]interface{}{
				"name": payload.Name, "external": true,
			})
		}
		log.Printf("Created external workspace %s (%s) url=%q awaiting=%v",
			payload.Name, id, payload.URL, payload.URL == "")
		resp := gin.H{
			"id":       id,
			"external": true,
		}
		if payload.URL != "" {
			resp["status"] = "online"
		} else {
			resp["status"] = "awaiting_agent"
			// Connection snippet payload. Returned ONCE on create —
			// the token is not recoverable from any later read.
			//
			// Payload assembly + per-snippet template stamping lives
			// in BuildExternalConnectionPayload (external_connection.go)
			// so the rotate + re-show endpoints emit byte-identical
			// shape. Adding a new snippet means adding it once there;
			// all three callers pick it up automatically.
			resp["connection"] = BuildExternalConnectionPayload(
				externalPlatformURL(c), id, payload.Name, connectionToken,
			)
		}
		c.JSON(http.StatusCreated, resp)
		return
	}

	// Resolve template config — needed for both Docker provisioning and
	// config-only persistence (tenant SaaS without Docker).
	var templatePath string
	var configFiles map[string][]byte
	if payload.Template != "" {
		candidatePath, resolveErr := resolveWorkspaceTemplatePath(h.configsDir, h.cacheDir, payload.Template)
		if resolveErr != nil {
			log.Printf("Create provision: rejecting template %q: %v", payload.Template, resolveErr)
			return
		}
		if _, err := os.Stat(candidatePath); err == nil {
			templatePath = candidatePath
		} else {
			log.Printf("Create: template %q not found, falling back for %s", payload.Template, payload.Name)
			safeRuntime := sanitizeRuntime(payload.Runtime)
			runtimeDefault := filepath.Join(h.configsDir, safeRuntime+"-default")
			if _, err := os.Stat(runtimeDefault); err == nil {
				templatePath = runtimeDefault
			} else {
				var cfgErr error
				configFiles, cfgErr = h.ensureDefaultConfig(id, payload)
				if cfgErr != nil {
					log.Printf("Create workspace %s: default config generation failed: %v", id, cfgErr)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate workspace configuration"})
					return
				}
			}
		}
	} else {
		var cfgErr error
		configFiles, cfgErr = h.ensureDefaultConfig(id, payload)
		if cfgErr != nil {
			log.Printf("Create workspace %s: default config generation failed: %v", id, cfgErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate workspace configuration"})
			return
		}
	}

	// Auto-provision — pick backend: control plane (SaaS) or Docker (self-hosted).
	// Routing AND the no-backend mark-failed path are both inside
	// provisionWorkspaceAuto (single source of truth). The Create-specific
	// extra is the workspace_config UPSERT below: when no backend is
	// wired, Auto marks the row failed but doesn't persist the bare
	// runtime/model/tier as JSON — the Config tab needs that to render
	// even on failed workspaces, so Create owns this Create-only side
	// effect rather than coupling Auto to a UI concern.
	provisionOK := h.provisionWorkspaceAuto(id, templatePath, configFiles, payload)
	if !provisionOK {
		cfgJSON := fmt.Sprintf(`{"name":%q,"runtime":%q,"tier":%d,"template":%q}`,
			payload.Name, payload.Runtime, payload.Tier, payload.Template)
		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO workspace_config (workspace_id, data) VALUES ($1, $2::jsonb)
			ON CONFLICT (workspace_id) DO UPDATE SET data = $2::jsonb
		`, id, cfgJSON); err != nil {
			log.Printf("Create: workspace_config persist failed for %s: %v", id, err)
		}
	}

	// Seed schedules declared in the workspace template's config.yaml
	// AFTER provisionWorkspaceAuto succeeds so the scheduler never
	// fires cron rows against a workspace whose backend never wired
	// (review feedback PR #1929#1). Async EC2 provisioning may still
	// fail downstream; scheduler.go is expected to handle non-online
	// status as a no-op tick. Idempotent across re-creates via
	// orgImportScheduleSQL's ON CONFLICT clause; runtime-added rows
	// are preserved (Issue #24 contract). Restart does not re-seed
	// (so user-deleted template rows stay deleted).
	//
	// Non-fatal: a broken schedules: block must never block workspace
	// provisioning — the workspace row is already live and the grid
	// is recoverable via POST /workspaces/{id}/schedules.
	if provisionOK && templatePath != "" {
		if templateScheds, parseErr := parseTemplateSchedules(templatePath); parseErr != nil {
			log.Printf("Create %s: parsing template schedules: %v (continuing)", id, parseErr)
		} else if len(templateScheds) > 0 {
			seeded, skipped := seedTemplateSchedules(ctx, id, templatePath, templateScheds)
			if skipped > 0 {
				log.Printf("Create %s: template schedule partial-seed: seeded=%d skipped=%d total=%d", id, seeded, skipped, len(templateScheds))
			} else {
				log.Printf("Create %s: seeded %d/%d template schedules", id, seeded, len(templateScheds))
			}
		}
	}

	// Record the template's declared plugins (RFC#2843 #32) so the post-online
	// reconcile (ReconcileWorkspacePlugins) installs them once the box is
	// reachable. agent-skills are PLUGINS now — they install dynamically
	// post-online, NOT via the provisioning channel — so a Create that omits
	// this step leaves workspace_declared_plugins empty, the reconcile no-ops,
	// and (e.g.) seo-all never installs. This mirrors the org_import.go loop;
	// recordDeclaredPlugin upserts ON CONFLICT so it is idempotent across
	// re-creates. Non-fatal: a broken plugins block must never block
	// provisioning (the workspace row is already live).
	if provisionOK && templatePath != "" {
		if declaredPlugins, parseErr := parseTemplatePlugins(templatePath); parseErr != nil {
			log.Printf("Create %s: parsing template plugins: %v (continuing)", id, parseErr)
		} else if len(declaredPlugins) > 0 {
			recorded, skipped := seedTemplatePlugins(ctx, id, declaredPlugins)
			if skipped > 0 {
				log.Printf("Create %s: template declared-plugin partial-record: recorded=%d skipped=%d total=%d", id, recorded, skipped, len(declaredPlugins))
			} else {
				log.Printf("Create %s: recorded %d/%d template declared plugins", id, recorded, len(declaredPlugins))
			}
		}
	}

	// RFC#2843 #32: on SaaS the DELIVERED template config (with plugins:) arrives
	// via the Gitea asset channel, NOT the local templatePath (which on a fresh
	// tenant is empty or falls back to <runtime>-default and misses the real
	// template's plugins:). Record declared plugins from the SAME fetched config
	// the asset channel delivers so the post-online reconcile installs them.
	// Idempotent (seedTemplatePlugins upserts ON CONFLICT); non-fatal.
	if provisionOK && h.giteaTemplateFetcher != nil && payload.Template != "" {
		if identity := templateIdentityForTemplateOrRuntime(payload.Template, payload.Runtime); identity != "" {
			if fetched, ferr := h.giteaTemplateFetcher.Load(ctx, identity); ferr != nil {
				log.Printf("Create %s: fetch template assets for declared-plugins: %v (continuing)", id, ferr)
			} else if declaredPlugins, perr := parseTemplatePluginsFromBytes(fetched["config.yaml"]); perr != nil {
				log.Printf("Create %s: parsing fetched template plugins: %v (continuing)", id, perr)
			} else if len(declaredPlugins) > 0 {
				recorded, skipped := seedTemplatePlugins(ctx, id, declaredPlugins)
				log.Printf("Create %s: recorded %d/%d fetched-template declared plugins (skipped=%d)", id, recorded, len(declaredPlugins), skipped)
			}
		}
	}

	// Mint the workspace's first bearer token and return it inline
	// (#1644). Pre-fix, callers had to make a separate POST to
	// /admin/workspaces/:id/tokens (production path, AdminAuth-gated,
	// but the path-prefix differs in CP-admin deploys so staging E2E
	// got HTML 404) OR fall back to GET /admin/workspaces/:id/test-token
	// (dev-only — deliberately 404s on MOLECULE_ENV=production per
	// admin_test_token.go::TestTokensEnabled, which violates
	// feedback_no_dev_only_routes_in_e2e). Inlining the first token here
	// makes the create response the SSOT — every caller (canvas Save,
	// org_import, E2E, third-party API) gets the bearer they need to
	// authenticate /activity, /a2a, /memory etc. without an extra
	// round trip to a separate mint endpoint.
	//
	// Failure is non-fatal: the workspace row already committed; the
	// operator can recover via POST /admin/workspaces/:id/tokens
	// (canonical admin mint) or POST /workspaces/:id/external/rotate
	// (already-used for the external pre-register path above). We log
	// the failure and return 201 without the field — callers that need
	// the token will get a clear-shaped fallback (auth_token absent
	// from response = use the admin mint path).
	resp := gin.H{
		"id":               id,
		"status":           "provisioning",
		"workspace_access": workspaceAccess,
	}
	if authToken, tokErr := wsauth.IssueAPIToken(ctx, db.DB, id); tokErr != nil {
		log.Printf("Create workspace %s: inline auth_token mint failed (non-fatal — caller can use POST /admin/workspaces/:id/tokens): %v", id, tokErr)
	} else {
		resp["auth_token"] = authToken
	}

	c.JSON(http.StatusCreated, resp)
}

// addProvisionTimeoutMs decorates a workspace response map with the
// per-runtime provision-timeout override (#2054 phase 2) when one is
// declared in the runtime's template manifest. No-op when the runtime
// has no declared timeout — the canvas-side resolver falls through to
// its runtime-profile default.
func (h *WorkspaceHandler) addProvisionTimeoutMs(ws map[string]interface{}, runtime string) {
	if secs := h.ProvisionTimeoutSecondsForRuntime(runtime); secs > 0 {
		ws["provision_timeout_ms"] = secs * 1000
	}
}

// ProvisionTimeoutSecondsForRuntime returns the per-runtime provision
// timeout in seconds when a template's config.yaml declared
// `runtime_config.provision_timeout_seconds`, else 0 ("no override —
// caller falls through to its own default").
//
// Exported so cmd/server/main.go can pass it to
// registry.StartProvisioningTimeoutSweep — same template-manifest value
// the canvas reads via addProvisionTimeoutMs. Without this, the
// sweeper killed claude-code at 10 min while the manifest declared a
// longer window, and a user saw the "Retry" UI before their image
// pull even finished. See registry.RuntimeTimeoutLookup for the
// resolution order.
func (h *WorkspaceHandler) ProvisionTimeoutSecondsForRuntime(runtime string) int {
	return h.provisionTimeouts.get(h.configsDir, runtime)
}

// scanWorkspaceRow is a helper to scan workspace+layout rows into a clean JSON map.
func scanWorkspaceRow(rows interface {
	Scan(dest ...interface{}) error
}) (map[string]interface{}, error) {
	var id, name, role, status, url, sampleError, currentTask, runtime, workspaceDir, kind string
	var computeRaw []byte
	var tier, activeTasks, maxConcurrentTasks, uptimeSeconds int
	var errorRate, x, y float64
	var collapsed, broadcastEnabled, talkToUserEnabled bool
	var parentID *string
	var agentCard []byte
	var budgetLimit sql.NullInt64
	var monthlySpend int64

	err := rows.Scan(&id, &name, &role, &tier, &status, &agentCard, &url,
		&parentID, &activeTasks, &maxConcurrentTasks, &errorRate, &sampleError, &uptimeSeconds,
		&currentTask, &runtime, &workspaceDir, &x, &y, &collapsed,
		&budgetLimit, &monthlySpend, &broadcastEnabled, &talkToUserEnabled, &computeRaw, &kind)
	if err != nil {
		return nil, err
	}

	ws := map[string]interface{}{
		"id":        id,
		"name":      name,
		"tier":      tier,
		"status":    status,
		"url":       url,
		"parent_id": parentID,
		// kind discriminates the org-level platform agent ('platform') from
		// ordinary workspaces ('workspace'). The canvas hides the platform
		// root from the node graph (it's the undeletable org anchor) and uses
		// it to resolve the concierge for the shell home/settings.
		"kind":                 kind,
		"active_tasks":         activeTasks,
		"max_concurrent_tasks": maxConcurrentTasks,
		"last_error_rate":      errorRate,
		"last_sample_error":    sampleError,
		"uptime_seconds":       uptimeSeconds,
		"current_task":         currentTask,
		"runtime":              runtime,
		"workspace_dir":        nilIfEmpty(workspaceDir),
		"monthly_spend":        monthlySpend,
		"x":                    x,
		"y":                    y,
		"collapsed":            collapsed,
		"broadcast_enabled":    broadcastEnabled,
		"talk_to_user_enabled": talkToUserEnabled,
	}
	if len(computeRaw) > 0 && string(computeRaw) != "null" {
		ws["compute"] = json.RawMessage(computeRaw)
	} else {
		ws["compute"] = json.RawMessage(`{}`)
	}

	// budget_limit: nil when no limit set, int64 otherwise
	if budgetLimit.Valid {
		ws["budget_limit"] = budgetLimit.Int64
	} else {
		ws["budget_limit"] = nil
	}

	// Only include non-empty values
	if role != "" {
		ws["role"] = role
	} else {
		ws["role"] = nil
	}

	// Parse agent_card as raw JSON
	if len(agentCard) > 0 && string(agentCard) != "null" {
		ws["agent_card"] = json.RawMessage(agentCard)
	} else {
		ws["agent_card"] = nil
	}

	return ws, nil
}

const workspaceListQuery = `
	SELECT w.id, w.name, COALESCE(w.role, ''), w.tier, w.status,
		   COALESCE(w.agent_card, 'null'::jsonb), COALESCE(w.url, ''),
		   w.parent_id, w.active_tasks, COALESCE(w.max_concurrent_tasks, 1),
		   w.last_error_rate,
		   COALESCE(w.last_sample_error, ''), w.uptime_seconds,
		   COALESCE(w.current_task, ''), COALESCE(w.runtime, 'claude-code'),
		   COALESCE(w.workspace_dir, ''),
		   COALESCE(cl.x, 0), COALESCE(cl.y, 0), COALESCE(cl.collapsed, false),
		   w.budget_limit, COALESCE(w.monthly_spend, 0),
		   w.broadcast_enabled, w.talk_to_user_enabled,
		   COALESCE(w.compute, '{}'::jsonb),
		   COALESCE(w.kind, 'workspace')
	FROM workspaces w
	LEFT JOIN canvas_layouts cl ON cl.workspace_id = w.id
	WHERE w.status != 'removed'
	ORDER BY w.created_at`

// List handles GET /workspaces
func (h *WorkspaceHandler) List(c *gin.Context) {
	rows, err := db.DB.QueryContext(c.Request.Context(), workspaceListQuery)
	if err != nil {
		log.Printf("List workspaces error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	workspaces := make([]map[string]interface{}, 0)
	for rows.Next() {
		ws, err := scanWorkspaceRow(rows)
		if err != nil {
			log.Printf("List scan error: %v", err)
			continue
		}
		// #2054 phase 2: surface per-runtime provision-timeout for
		// canvas's ProvisioningTimeout banner. Decorating per-row
		// (vs map-once-and-reuse) keeps the helper self-contained;
		// the cache hit is sub-microsecond.
		if rt, _ := ws["runtime"].(string); rt != "" {
			h.addProvisionTimeoutMs(ws, rt)
		}
		workspaces = append(workspaces, ws)
	}
	if err := rows.Err(); err != nil {
		log.Printf("List rows error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query iteration failed"})
		return
	}

	c.JSON(http.StatusOK, workspaces)
}

// Get handles GET /workspaces/:id
func (h *WorkspaceHandler) Get(c *gin.Context) {
	id := c.Param("id")

	// #687: reject non-UUID IDs before hitting the DB.
	if err := validateWorkspaceID(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}

	row := db.DB.QueryRowContext(c.Request.Context(), `
		SELECT w.id, w.name, COALESCE(w.role, ''), w.tier, w.status,
			   COALESCE(w.agent_card, 'null'::jsonb), COALESCE(w.url, ''),
			   w.parent_id, w.active_tasks, COALESCE(w.max_concurrent_tasks, 1),
			   w.last_error_rate,
			   COALESCE(w.last_sample_error, ''), w.uptime_seconds,
			   COALESCE(w.current_task, ''), COALESCE(w.runtime, 'claude-code'),
			   COALESCE(w.workspace_dir, ''),
			   COALESCE(cl.x, 0), COALESCE(cl.y, 0), COALESCE(cl.collapsed, false),
			   w.budget_limit, COALESCE(w.monthly_spend, 0),
			   w.broadcast_enabled, w.talk_to_user_enabled,
			   COALESCE(w.compute, '{}'::jsonb),
		   COALESCE(w.kind, 'workspace')
		FROM workspaces w
		LEFT JOIN canvas_layouts cl ON cl.workspace_id = w.id
		WHERE w.id = $1
	`, id)

	ws, err := scanWorkspaceRow(row)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		log.Printf("Get workspace error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	// #2429: workspaces with status='removed' return 410 Gone (not 200)
	// so callers fail loudly at startup instead of after 60s of revoked-
	// token heartbeats. The audit-trail consumers that need the body of
	// a removed workspace opt in via ?include_removed=true.
	//
	// Why a query param and not a header: cheap to set in curl/canvas
	// fetch alike, visible in access logs, and works without coupling
	// to content negotiation.
	if status, _ := ws["status"].(string); status == string(models.StatusRemoved) {
		if c.Query("include_removed") != "true" {
			// Best-effort fetch of the removal timestamp. If the row was
			// deleted (or some transient DB error fired) between the
			// scanWorkspaceRow above and this follow-up SELECT,
			// removedAt stays as Go's zero time. Emit `null` in that
			// case rather than the misleading `0001-01-01T00:00:00Z`
			// the client would otherwise see — the actionable signal
			// is the 410 + hint, not the timestamp.
			var removedAt time.Time
			if err := db.DB.QueryRowContext(c.Request.Context(),
				`SELECT updated_at FROM workspaces WHERE id = $1`, id,
			).Scan(&removedAt); err != nil {
				log.Printf("workspace GET: removed_at query failed for %s: %v", id, err)
			}
			body := gin.H{
				"error": "workspace removed",
				"id":    id,
				"hint":  "Regenerate workspace + token from the canvas → Tokens tab",
			}
			if removedAt.IsZero() {
				body["removed_at"] = nil
			} else {
				body["removed_at"] = removedAt
			}
			c.JSON(http.StatusGone, body)
			return
		}
	}

	// Strip sensitive fields — GET /workspaces/:id is on the open router.
	// Any caller with a valid UUID would otherwise read operational data.
	delete(ws, "budget_limit")
	delete(ws, "monthly_spend")
	delete(ws, "current_task")      // operational surveillance risk (#955)
	delete(ws, "last_sample_error") // internal error details
	delete(ws, "workspace_dir")     // host path disclosure

	// #817: expose last_outbound_at so orchestrators can detect silent
	// workspaces. Non-sensitive — just a timestamp of the most recent
	// outbound A2A. Null if the workspace has never sent anything.
	var lastOutbound sql.NullTime
	if err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT last_outbound_at FROM workspaces WHERE id = $1`, id,
	).Scan(&lastOutbound); err == nil && lastOutbound.Valid {
		ws["last_outbound_at"] = lastOutbound.Time
	} else {
		ws["last_outbound_at"] = nil
	}

	// 2026-06-19 a2a RCA (#3057): fetch last_heartbeat_at alongside
	// last_outbound_at. The GET SELECT and scanWorkspaceRow do NOT
	// include it (reviewers 12459 + 12460 caught the previous
	// assumption that the column was already scanned), so we read
	// it as a separate column-shaped follow-up query. The wedge
	// predicate needs BOTH timestamps — without the heartbeat fetch,
	// lastHeartbeat stayed invalid and any active workspace with a
	// stale outbound was flagged wedged, recreating the false-positive
	// safety concern this PR exists to fix.
	var lastHeartbeat sql.NullTime
	if err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT last_heartbeat_at FROM workspaces WHERE id = $1`, id,
	).Scan(&lastHeartbeat); err == nil && lastHeartbeat.Valid {
		ws["last_heartbeat_at"] = lastHeartbeat.Time
	} else {
		ws["last_heartbeat_at"] = nil
	}

	// 2026-06-19 a2a RCA (#3057): surface a `wedged` boolean so
	// operators and the canvas can detect the alive-but-wedged case
	// (active_tasks>0, no outbound, no heartbeat) without manually
	// inspecting the tuple. The predicate is the same one the
	// wedged-agent monitor uses internally (registry.IsWedgedAgent),
	// so the flag and the monitor can never disagree. Threshold comes
	// from the same env var override; default 5 minutes. active_tasks
	// is in the scanned row; last_outbound_at and last_heartbeat_at
	// are both fetched above as separate column queries.
	activeTasksVal, _ := ws["active_tasks"].(int)
	ws["wedged"] = registry.IsWedgedAgent(activeTasksVal, lastOutbound, lastHeartbeat, registry.WedgedThresholdForHTTP())

	// #2054 phase 2: per-runtime provision-timeout for canvas's
	// ProvisioningTimeout banner.
	if rt, _ := ws["runtime"].(string); rt != "" {
		h.addProvisionTimeoutMs(ws, rt)
	}

	c.JSON(http.StatusOK, ws)
}
