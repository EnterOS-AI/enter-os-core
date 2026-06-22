package handlers

// workspace_provision_shared.go — mode-agnostic provision-time
// orchestration that BOTH provisionWorkspaceOpts (Docker) and
// provisionWorkspaceCP (SaaS) call. Extracted because the original
// per-mode functions had drifted: the SaaS path forgot to call
// issueAndInjectInboundSecret, which produced silent-503 chat upload
// errors for every prod workspace (RFC #2312 — discovered 2026-04-30).
//
// The drift class this module closes: when a new provision-time setup
// step is added, it goes in ONE place and both modes pick it up. New
// steps that legitimately differ per mode stay in the per-mode
// functions and are explicitly documented there.
//
// What's shared (this file):
//   - Loading secrets (global + workspace) from Postgres
//   - Applying git identity, runtime model env, role propagation
//   - Running env mutators
//   - Preflight: missing required env
//   - Building provisioner.WorkspaceConfig
//   - Minting auth_token + platform_inbound_secret (#2312)
//
// What's mode-specific (kept in the per-mode functions):
//   - Docker: empty-config-volume preflight + auto-recover via
//     runtime template (#1858), then provisioner.Start with local
//     Docker daemon, then WriteFilesToContainer for config volume.
//   - SaaS: cpProv.Start which delegates EC2 launch to control plane,
//     then persist returned instance_id, then defer .auth_token /
//     .platform_inbound_secret delivery to the workspace's first
//     /registry/register response (registry.go:344-362).
//
// Architectural test (workspace_provision_shared_test.go) asserts
// every code path that takes a workspaceID and starts a provision
// MUST call mintWorkspaceSecrets — same shape as the
// audit-coverage AST gate from #335.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
)

// readOrLazyHealInboundSecret reads the workspace's
// platform_inbound_secret. On ErrNoInboundSecret it mints inline
// (lazy-heal — RFC #2312 backfill for workspaces provisioned before
// the shared-mint refactor closed the gap).
//
// Returns:
//   - (secret, false, nil) — secret was already present
//   - (secret, true,  nil) — secret was missing; just minted
//   - ("",     false, err) — read failed (non-NoInboundSecret) OR mint failed
//
// opLabel prefixes log lines so operators can tell which feature
// triggered the heal (e.g. "Registry", "chat_files Upload"). Centralized
// here so the next "what to do when the secret is missing" decision —
// rotation, audit, alerting — goes in ONE place. Same drift-prevention
// rationale as resolveWorkspaceForwardCreds and mintWorkspaceSecrets.
func readOrLazyHealInboundSecret(ctx context.Context, workspaceID, opLabel string) (secret string, healed bool, err error) {
	s, readErr := wsauth.ReadPlatformInboundSecret(ctx, db.DB, workspaceID)
	if readErr == nil {
		return s, false, nil
	}
	if !errors.Is(readErr, wsauth.ErrNoInboundSecret) {
		log.Printf("%s: read platform_inbound_secret failed for %s: %v", opLabel, workspaceID, readErr)
		return "", false, readErr
	}
	minted, mintErr := wsauth.IssuePlatformInboundSecret(ctx, db.DB, workspaceID)
	if mintErr != nil {
		log.Printf("%s: lazy-heal mint of platform_inbound_secret failed for %s: %v", opLabel, workspaceID, mintErr)
		return "", false, mintErr
	}
	log.Printf("%s: lazy-healed platform_inbound_secret for %s (#2312 backfill)", opLabel, workspaceID)
	return minted, true, nil
}

// preparedProvisionContext carries the computed env + cfg through
// the per-mode provisioner-Start step. Returned from
// prepareProvisionContext when the caller proceeds; nil + non-empty
// abort message when the caller must mark the workspace failed.
type preparedProvisionContext struct {
	EnvVars     map[string]string
	PluginsPath string
	Config      provisioner.WorkspaceConfig
}

// provisionAbort describes why prepareProvisionContext refused to
// produce a context. Msg is the caller-safe summary that gets
// persisted to workspaces.last_sample_error AND broadcast. Extra
// is the structured payload to surface alongside (e.g. the missing
// env list for the missing-env failure class). Both fields are
// zero-valued on the success return.
type provisionAbort struct {
	Msg   string
	Extra map[string]interface{}
}

// prepareProvisionContext does the mode-agnostic setup work that
// both Docker and SaaS provisioners need before their respective
// Start() call. Returns the prepared cfg or an abort message that
// the caller MUST broadcast as WORKSPACE_PROVISION_FAILED and
// persist as workspaces.last_sample_error.
//
// The function does NOT broadcast / DB-write the failure itself —
// failure surface is mode-aware (Docker logs differ from SaaS logs
// in observed format, and the failure-class taxonomy is shared
// between modes but the broadcast key may evolve per mode in the
// future).
func (h *WorkspaceHandler) prepareProvisionContext(
	ctx context.Context,
	workspaceID, templatePath string,
	configFiles map[string][]byte,
	payload models.CreateWorkspacePayload,
	resetClaudeSession bool,
) (*preparedProvisionContext, *provisionAbort) {
	envVars, globalSecretKeys, workspaceSecretKeys, decryptErr := loadWorkspaceSecrets(ctx, workspaceID)
	if decryptErr != "" {
		return nil, &provisionAbort{Msg: decryptErr}
	}

	// RFC#523 Layer 1 (issue molecule-ai/internal#523): refuse to start a
	// tenant workspace when any forbidden operator-scope env var is
	// present in the operator-controlled store (global_secrets).
	//
	// PROVENANCE-AWARE — fix for the over-fire reported by CTO empirical
	// 2026-05-20: the original implementation ran this check on the
	// merged env-set, which conflated two very different sources:
	//
	//   1. global_secrets — operator-side store. ANY operator-scope token
	//      here is an upstream bleed (e.g. tenant_secrets_seed.go pre-
	//      4f45d37 propagating CP-env GITHUB_TOKEN into every fresh
	//      tenant's row). RFC#523's literal threat model.
	//
	//   2. workspace_secrets — user-set via the canvas Secrets tab,
	//      authenticated as the workspace owner. If the user pastes
	//      their own scoped GitHub PAT under GITHUB_TOKEN so the agent
	//      can push to their personal repos, that is the system working
	//      as designed — not the leak RFC#523 was written to catch.
	//
	// The provenance side-channel from loadWorkspaceSecrets tells us
	// which keys came from global_secrets (workspace_secrets writes
	// override and clear the flag, since the user explicitly re-set
	// the value). We restrict the abort to that set.
	//
	// Defense-in-depth NOT removed: provisioner.buildContainerEnv still
	// runs the forensic #145 silent-strip (lower-confidence late layer),
	// and workspace/entrypoint.sh has Layer 2 inside the container. If a
	// real operator-scope token slips into workspace_secrets some other
	// way, the later layers (and the per-workspace SG, and the per-tenant
	// VPC isolation) are still in force.
	//
	// Key names (not values) are echoed in the user-facing error so
	// the operator can locate and remove the offending row. Per memory
	// `feedback_passwords_in_chat_are_burned`, key names are not
	// secret; values would be.
	if forbidden := findForbiddenTenantEnvKeysFromGlobals(envVars, globalSecretKeys); len(forbidden) > 0 {
		msg := formatForbiddenTenantEnvError(forbidden)
		log.Printf("Provisioner: ABORT workspace=%s — forbidden operator-scope env keys present in global_secrets: %v (RFC#523)", workspaceID, forbidden)
		return nil, &provisionAbort{
			Msg:   msg,
			Extra: map[string]interface{}{"error": msg, "forbidden_env_keys": forbidden, "rfc": "523", "source": "global_secrets"},
		}
	}

	pluginsPath, _ := filepath.Abs(filepath.Join(h.configsDir, "..", "plugins"))

	// Per-agent git identity (#1957) — must run after secret loads so
	// a workspace_secret named GIT_AUTHOR_NAME can override.
	applyAgentGitIdentity(envVars, payload.Name)
	// Per-agent git HTTP credential injection — bridges the gap that
	// PR template-claude-code#30 + mc#1525 left open: the askpass binary
	// + GIT_ASKPASS env are wired in-image, but until now no code path
	// in workspace-server actually read the persona's git token from
	// the operator-host bootstrap dir and exported it as
	// GIT_HTTP_USERNAME / GIT_HTTP_PASSWORD. Without this, the askpass
	// helper invokes with an empty password env and git fails the
	// auth challenge in ~500ms (live-verified for Dev-A/Dev-B
	// 2026-05-18 ~23:55Z).
	//
	// Runs AFTER applyAgentGitIdentity so workspace_secrets named
	// GIT_HTTP_USERNAME / GIT_HTTP_PASSWORD (operator-supplied,
	// loaded earlier by loadWorkspaceSecrets) win over the
	// persona-file default. Uses payload.Role as the persona key —
	// this matches the slug-form convention agent-dev-a /
	// agent-dev-b / agent-pm. Descriptive multi-word roles
	// ("Frontend Engineer") take the silent-no-op branch and
	// continue to rely on workspace_secrets / org-import persona-env
	// merge for their git auth.
	applyAgentGitHTTPCreds(envVars, payload.Role)
	// molecule-core#1994: per-workspace LLM billing-mode resolution + env wiring.
	// On platform_managed it forces the CP proxy usage token; on byok/disabled
	// it keeps the tenant's own provider-MATCHING creds (global OR workspace
	// scope) and reports whether a usable LLM credential is present.
	//
	// internal#728 Bug 1: globalSecretKeys (loadWorkspaceSecrets provenance)
	// lets the byok branch strip ONLY operator-store-origin LLM creds that do
	// NOT match the resolved provider's auth_env — so a non-anthropic-oauth
	// claude-code workspace no longer inherits the stray tenant-global
	// CLAUDE_CODE_OAUTH_TOKEN the runtime would greedily prefer. User-authored
	// workspace_secrets (provenance flag cleared) are exempt.
	llmRes := applyPlatformManagedLLMEnv(ctx, envVars, workspaceID, payload.Runtime, payload.Model, globalSecretKeys)
	// Fail closed for a BYOK workspace with no usable LLM credential at ANY
	// scope: do NOT start it credential-less. Mirror the "model+provider+
	// credential REQUIRED at create" spirit with an actionable error surfaced
	// at provision time.
	//
	// Scoped to byok specifically (NOT disabled): "byok" means "the user
	// intends to run an LLM on their own credential" — a missing one is a
	// misconfiguration worth surfacing loudly. "disabled" means "this
	// workspace runs no platform-billed LLM at all" (terminal / file work, or
	// a runtime that talks to a non-bypass-key endpoint), so aborting would
	// regress a legitimate no-LLM workspace.
	//
	// The bypass-key check is intentionally broad — any present bypass key
	// (the tenant's own, at global or workspace scope) clears it.
	if llmRes.ResolvedMode == LLMBillingModeBYOK && !llmRes.HasUsableLLMCred {
		msg := formatMissingBYOKCredentialError(llmRes.ResolvedMode)
		log.Printf("Provisioner: ABORT workspace=%s — byok billing mode has no usable LLM credential (MISSING_BYOK_CREDENTIAL, molecule-core#1994)", workspaceID)
		return nil, &provisionAbort{
			Msg:   msg,
			Extra: map[string]interface{}{"error": msg, "code": "MISSING_BYOK_CREDENTIAL", "billing_mode": llmRes.ResolvedMode, "issue": "1994"},
		}
	}
	// Fail closed for a platform-managed workspace whose CP proxy env is
	// absent: do NOT start it credential-less (adk-demo dark-wedge class,
	// #2162). The platform_managed path requires the proxy injection to
	// produce a usable credential.
	if llmRes.ResolvedMode == LLMBillingModePlatformManaged && !llmRes.HasUsableLLMCred {
		msg := formatMissingPlatformProxyError()
		log.Printf("Provisioner: ABORT workspace=%s — platform-managed billing mode but CP proxy env absent (MISSING_PLATFORM_PROXY, molecule-core#2162)", workspaceID)
		return nil, &provisionAbort{
			Msg:   msg,
			Extra: map[string]interface{}{"error": msg, "code": "MISSING_PLATFORM_PROXY", "billing_mode": llmRes.ResolvedMode, "issue": "2162"},
		}
	}
	applyRuntimeModelEnv(envVars, payload.Runtime, payload.Model)
	if payload.Role != "" {
		envVars["MOLECULE_AGENT_ROLE"] = payload.Role
	}
	// PARENT_ID is consumed by workspace/coordinator.py to track the
	// parent-child relationship at runtime. Sourced from payload so
	// every provision path that knows about a parent (currently:
	// TeamHandler.Expand) injects it without having to thread env
	// through provisioner.WorkspaceConfig manually.
	if payload.ParentID != nil && *payload.ParentID != "" {
		envVars["PARENT_ID"] = *payload.ParentID
	}

	// Plugin extension point: env mutators run AFTER built-in identity
	// injection so plugins can override or augment identity vars.
	if err := h.envMutators.Run(ctx, workspaceID, envVars); err != nil {
		return nil, &provisionAbort{Msg: "plugin env mutator chain failed"}
	}

	// Concierge identity (RFC docs/design/rfc-platform-agent.md): when this
	// workspace is the org platform agent (kind='platform'), overlay the
	// Org-Concierge system prompt + the platform-MCP declaration and inject the
	// org-admin MCP env. No-op for ordinary workspaces. Runs BEFORE the
	// required-env preflight so a concierge config.yaml that the overlay just
	// wrote is the one preflight inspects. Rebinds configFiles because it is nil
	// on the auto-restart path (where the overlay is what introduces the files).
	configFiles = h.applyConciergeProvisionConfig(ctx, workspaceID, templatePath, configFiles, envVars, payload.Name)

	// core#2594: universal MODEL fail-closed gate. Runs AFTER every model-setting
	// path (create payload → stored MODEL secret via applyRuntimeModelEnv → the
	// concierge's declared model via applyConciergeProvisionConfig) and is the
	// single place that refuses to launch a workspace with no resolved model.
	//
	// This replaces the deleted MOLECULE_LLM_DEFAULT_MODEL env fail-open: rather
	// than silently substituting a server-env model (or letting the runtime fall
	// back to its hardcoded anthropic:claude-opus-4-7), a workspace that reaches
	// here with neither MOLECULE_MODEL nor MODEL set aborts with an actionable
	// MISSING_MODEL error. Create already fails closed at the boundary; this
	// catches every other provision path (restart/resume/auto-recover/import) so
	// the guarantee holds "for everything".
	//
	// Scoped to NON-disabled billing: a "disabled" workspace runs no platform-
	// billed LLM at all (terminal / file work), so requiring a model there would
	// regress a legitimate no-LLM workspace — same scoping rationale as the
	// MISSING_BYOK / MISSING_PLATFORM_PROXY credential gates above.
	if llmRes.ResolvedMode != LLMBillingModeDisabled &&
		strings.TrimSpace(envVars["MOLECULE_MODEL"]) == "" &&
		strings.TrimSpace(envVars["MODEL"]) == "" {
		msg := formatMissingModelError(llmRes.ResolvedMode)
		log.Printf("Provisioner: ABORT workspace=%s — no resolved model (MISSING_MODEL, core#2594); refusing the runtime's opaque default", workspaceID)
		return nil, &provisionAbort{
			Msg:   msg,
			Extra: map[string]interface{}{"error": msg, "code": "MISSING_MODEL", "billing_mode": llmRes.ResolvedMode, "issue": "2594"},
		}
	}

	// Preflight #5: refuse to launch when config.yaml declares required
	// env vars that are not set. Skipped in SaaS mode when configFiles
	// is nil (CP-mode's cfg is built without local config bytes — the
	// CP-side launches the workspace with envVars but doesn't inspect
	// the config.yaml the same way Docker does). Future: lift the
	// preflight to a pure-data check that doesn't need configFiles
	// bytes, so SaaS mode can run it too.
	if configFiles != nil {
		if missing := missingRequiredEnv(configFiles, envVars); len(missing) > 0 {
			msg := formatMissingEnvError(missing)
			return nil, &provisionAbort{
				Msg:   msg,
				Extra: map[string]interface{}{"error": msg, "missing": missing},
			}
		}
	}

	// Preflight: runtime-seed match (issue #2027). Fail LOUD when a workspace
	// NAMED a runtime but the config.yaml we're about to seed declares a
	// different top-level runtime — the symmetric counterpart to selectImage's
	// ErrUnresolvableRuntime guard, on the config/template side. Pre-fix, when a
	// runtime's workspace template wasn't in the tenant cache at provision time
	// (or sanitizeRuntime coerced an unknown runtime), seeding silently fell
	// back to the claude-code-default template: the image+env said e.g.
	// google-adk but the seeded config said claude-code, so the agent booted
	// mislabeled and personaless yet looked 'online' and returned canned
	// non-answers. Refusing loudly turns that silent wrong-agent into a visible
	// WORKSPACE_PROVISION_FAILED the operator can act on.
	if abort := runtimeSeedMismatchAbort(payload.Runtime, templatePath, configFiles); abort != nil {
		log.Printf("Provisioner: ABORT workspace=%s — %s", workspaceID, abort.Msg)
		return nil, abort
	}

	cfg := h.buildProvisionerConfig(ctx, workspaceID, templatePath, configFiles, payload, envVars, workspaceSecretKeys, pluginsPath)
	cfg.ResetClaudeSession = resetClaudeSession

	return &preparedProvisionContext{
		EnvVars:     envVars,
		PluginsPath: pluginsPath,
		Config:      cfg,
	}, nil
}

// runtimeSeedMismatchAbort returns a non-nil abort when a workspace NAMED a
// runtime but the config.yaml about to be seeded declares a *different*
// top-level runtime — the fail-loud counterpart to selectImage's
// ErrUnresolvableRuntime (issue #2027). It catches the silent
// claude-code-default substitution that occurs when a runtime's workspace
// template isn't cached at provision time (or sanitizeRuntime coerced an
// unknown runtime to claude-code): both surface as a seeded config whose
// runtime contradicts the requested one.
//
// Pure (modulo reading the template dir's config.yaml). An empty
// requestedRuntime (unspecified / org-template default path) or an
// indeterminate seeded runtime (e.g. CP mode with no local config bytes) is
// allowed — we only fail on a concrete, contradictory signal, never on
// absence of one.
func runtimeSeedMismatchAbort(requestedRuntime, templatePath string, configFiles map[string][]byte) *provisionAbort {
	if requestedRuntime == "" {
		return nil
	}
	seeded := seededConfigRuntime(templatePath, configFiles)
	if seeded == "" || seeded == requestedRuntime {
		return nil
	}
	msg := fmt.Sprintf(
		"runtime seed mismatch: workspace requested runtime %q but the seeded config.yaml declares %q — the %q workspace template was not available at provision time (silent %q fallback). Refusing to launch a mislabeled agent; refresh the template cache (POST /admin/templates/refresh) and re-provision.",
		requestedRuntime, seeded, requestedRuntime, seeded,
	)
	return &provisionAbort{
		Msg: msg,
		Extra: map[string]interface{}{
			"error":             msg,
			"requested_runtime": requestedRuntime,
			"seeded_runtime":    seeded,
			"issue":             "2027",
		},
	}
}

// seededConfigRuntime extracts the top-level `runtime:` from the config.yaml
// that will be seeded into the workspace — preferring the in-memory
// configFiles, falling back to the template directory on disk. Returns ""
// when no config.yaml is available or it declares no top-level runtime.
func seededConfigRuntime(templatePath string, configFiles map[string][]byte) string {
	if data, ok := configFiles["config.yaml"]; ok {
		return parseTopLevelRuntime(data)
	}
	if templatePath != "" {
		if data, err := os.ReadFile(filepath.Join(templatePath, "config.yaml")); err == nil {
			return parseTopLevelRuntime(data)
		}
	}
	return ""
}

// parseTopLevelRuntime returns the value of the top-level `runtime:` key in a
// config.yaml, ignoring the nested `runtime_config:` block. A small dedicated
// line scanner (mirrors the one the Create handler uses to read a template's
// runtime) so the provision-time guard needs no YAML dependency.
func parseTopLevelRuntime(data []byte) string {
	for _, raw := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimLeft(raw, " \t")
		if len(raw) > len(trimmed) {
			continue // indented — inside a nested block (e.g. runtime_config:)
		}
		if strings.HasPrefix(trimmed, "runtime:") && !strings.HasPrefix(trimmed, "runtime_config") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "runtime:")), `"'`)
		}
	}
	return ""
}

// mintWorkspaceSecrets issues + persists the workspace auth token
// AND the platform→workspace inbound secret (#2312). Both modes MUST
// call this — Docker mints + writes to local config volume; SaaS
// mints and persists only to the DB column (the workspace fetches
// via /registry/register response).
//
// Pre-2026-04-30: the SaaS path (provisionWorkspaceCP) was missing
// the inbound-secret mint, leaving every prod workspace with a NULL
// platform_inbound_secret column and 503-ing every chat upload. This
// extracted helper is the structural fix — both paths share one
// function so adding a new mint can't be silently forgotten on one
// side.
//
// Failure model: best-effort. Each underlying issue function logs +
// returns silently on its own failure (token mint failure → workspace
// 401s on first /internal call; secret mint failure → first chat
// upload 503s with the same message we used to surface for old-
// workspaces). The shared helper does NOT abort the provision because
// the workspace can still come up and serve non-internal traffic; the
// 401/503 surfaces the missing secret loudly when the user actually
// tries to use the affected feature.
func (h *WorkspaceHandler) mintWorkspaceSecrets(ctx context.Context, workspaceID string, cfg *provisioner.WorkspaceConfig) {
	h.issueAndInjectToken(ctx, workspaceID, cfg)
	h.issueAndInjectInboundSecret(ctx, workspaceID, cfg)
}

// markProvisionFailed is the standard "abort with message" path used
// by both provision modes. Wraps the broadcast + DB update in one
// call so the failure shape stays consistent across modes.
func (h *WorkspaceHandler) markProvisionFailed(ctx context.Context, workspaceID, msg string, extra map[string]interface{}) {
	if extra == nil {
		extra = map[string]interface{}{"error": msg}
	} else if _, hasErr := extra["error"]; !hasErr {
		extra["error"] = msg
	}
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisionFailed), workspaceID, extra)
	if _, dbErr := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $3, last_sample_error = $2, updated_at = now() WHERE id = $1`,
		workspaceID, msg, models.StatusFailed); dbErr != nil {
		// Non-fatal: the broadcast already fired, the operator sees the
		// failure event in the canvas. The DB row stays at whatever
		// status it had — provisioning event log is the source of truth.
		log.Printf("markProvisionFailed: db update failed for %s: %v", workspaceID, dbErr)
	}
}
