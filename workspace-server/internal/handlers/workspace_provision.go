package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/crypto"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/memory/contract"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/providers"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"gopkg.in/yaml.v3"
)

// instanceIDPersistRetryAttempts caps total instance_id UPDATE attempts
// (initial + retries). 3 catches transient DB blips without stalling the
// provision goroutine past the context timeout.
var instanceIDPersistRetryAttempts = 3

// instanceIDPersistRetryBaseDelay is the first-retry backoff. Doubles each
// attempt: 100ms → 200ms → 400ms. Total stall ≤ 700ms.
var instanceIDPersistRetryBaseDelay = 100 * time.Millisecond

// logProvisionPanic is the deferred recover at the top of every provision
// goroutine. Without it, a panic inside provisionWorkspaceOpts /
// provisionWorkspaceCP propagates up the goroutine stack and crashes the
// whole workspace-server process — taking every other tenant workspace
// down with it. With it, the panic is logged with a stack trace, the
// workspace is marked failed via markProvisionFailed (so the canvas
// surfaces a failure card immediately instead of leaving the spinner
// stuck on "provisioning" until the 10-min sweeper fires), and the rest
// of the process keeps serving.
//
// Issue #2486 added this after the symmetric class — silent goroutine
// exit, no log, no failure mark — was observed in prod. Even if the
// root cause turns out not to be a panic, surfacing the panic class
// closes one branch of "what could have happened" cleanly.
//
// Method on *WorkspaceHandler (not free function) so the panic path can
// reuse markProvisionFailed and emit the WORKSPACE_PROVISION_FAILED
// broadcast — without the broadcast the canvas only learns of the
// failure when the next poll/refresh hits the DB.
func (h *WorkspaceHandler) logProvisionPanic(workspaceID, mode string) {
	r := recover()
	if r == nil {
		return
	}
	log.Printf("Provisioner: PANIC during provision goroutine for %s (mode=%s): %v\nstack:\n%s",
		workspaceID, mode, r, debug.Stack())
	// Fresh context: the provision goroutine's ctx may have been the one
	// panicking (timeout, cancelled). 10s is enough for the broadcast +
	// single UPDATE inside markProvisionFailed.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.markProvisionFailed(ctx, workspaceID, fmt.Sprintf("provision panic: %v", r), nil)
}

// provisionWorkspace handles async container deployment with timeout.
func (h *WorkspaceHandler) provisionWorkspace(workspaceID, templatePath string, configFiles map[string][]byte, payload models.CreateWorkspacePayload) {
	h.provisionWorkspaceOpts(workspaceID, templatePath, configFiles, payload, false)
}

// provisionWorkspaceOpts is the workhorse variant of provisionWorkspace that
// accepts extra per-invocation knobs (e.g. resetClaudeSession for issue #12)
// that should NOT be persisted on CreateWorkspacePayload because they're
// request-scoped flags.
func (h *WorkspaceHandler) provisionWorkspaceOpts(workspaceID, templatePath string, configFiles map[string][]byte, payload models.CreateWorkspacePayload, resetClaudeSession bool) {
	// Entry log — distinguishes "goroutine never started" from "started but
	// exited via an unlogged path" when debugging stuck-in-provisioning
	// rows. Issue #2486: 7 claude-code workspaces stuck in provisioning had
	// neither a prepare-failed nor start-failed nor success log line, so an
	// operator couldn't tell whether the goroutine ran at all.
	log.Printf("Provisioner: goroutine entered for %s (runtime=%s, mode=docker)", workspaceID, payload.Runtime)
	defer h.logProvisionPanic(workspaceID, "docker")

	ctx, cancel := context.WithTimeout(context.Background(), provisioner.ProvisionTimeout)
	defer cancel()

	prepared, abort := h.prepareProvisionContext(ctx, workspaceID, templatePath, configFiles, payload, resetClaudeSession)
	if prepared == nil {
		log.Printf("Provisioner: prepare failed for %s: %s", workspaceID, abort.Msg)
		h.markProvisionFailed(ctx, workspaceID, abort.Msg, abort.Extra)
		return
	}
	cfg := prepared.Config

	// Preflight #17: detect + auto-recover the "empty config volume" crashloop.
	// Docker-specific — SaaS mode delegates volume management to the
	// control-plane EC2 launcher and never has a local Docker volume to
	// probe. Runs AFTER prepareProvisionContext because the recovered
	// template still needs the same env-and-cfg surface the prepare built.
	//
	// When the caller supplies neither a template dir nor in-memory configFiles
	// (the auto-restart path), probe the existing Docker named volume. If the
	// volume is empty / missing config.yaml, we can't just hand the container
	// to Docker's unless-stopped restart policy — molecule-runtime will crash
	// on FileNotFoundError and loop forever.
	//
	// Before #1858: bail out and mark the workspace 'failed'. Required operator
	// intervention (manual `docker run --rm -v <vol>:/configs -v <tmpl>:/src
	// alpine cp -r /src/. /configs/`).
	//
	// After #1858: attempt recovery by resolving the workspace's runtime-default
	// template from h.configsDir (same path the Restart handler uses for
	// apply_template=true) and wiring it in. The volume will be rewritten from
	// the template on container start, same as first-provision. Only if the
	// recovery template itself is missing do we bail.
	if srcErr := provisioner.ValidateConfigSource(templatePath, configFiles); srcErr != nil {
		hasConfig, probeErr := h.provisioner.VolumeHasFile(ctx, workspaceID, "config.yaml")
		if probeErr != nil {
			log.Printf("Provisioner: config.yaml preflight probe failed for %s: %v (proceeding)", workspaceID, probeErr)
		} else if !hasConfig {
			// Try to recover by applying the runtime-default template. payload.Runtime
			// is populated by the caller (Restart handler / Create handler) from the
			// DB row — same source of truth the apply_template=true path uses.
			// Try `<runtime>-default` first (historical naming), then plain
			// `<runtime>` (current naming in workspace-configs-templates/).
			// Only claude-code has the `-default` suffix; every other
			// runtime directory uses the bare name. Without the bare-name
			// fallback, recovery only worked for claude-code and blank
			// workspaces on every other runtime bricked on first start.
			recovered := false
			if payload.Runtime != "" {
				candidates := []string{
					filepath.Join(h.configsDir, payload.Runtime+"-default"),
					filepath.Join(h.configsDir, payload.Runtime),
				}
				for _, runtimeTemplate := range candidates {
					if _, statErr := os.Stat(runtimeTemplate); statErr == nil {
						log.Printf("Provisioner: auto-recover for %s — config volume empty, applying %s template (#1858)",
							workspaceID, filepath.Base(runtimeTemplate))
						templatePath = runtimeTemplate
						// Rebuild cfg with the recovered template path so Start() sees it.
						cfg = h.buildProvisionerConfig(ctx, workspaceID, templatePath, configFiles, payload, prepared.EnvVars, prepared.Config.WorkspaceSecretKeys, prepared.PluginsPath)
						cfg.ResetClaudeSession = resetClaudeSession
						recovered = true
						break
					}
				}
				if !recovered {
					log.Printf("Provisioner: auto-recover for %s — no template found under %s for runtime=%s",
						workspaceID, h.configsDir, payload.Runtime)
				}
			}

			if !recovered {
				msg := fmt.Sprintf("cannot start workspace %s: no config.yaml source and config volume is empty — delete the workspace or provide a template", workspaceID)
				log.Printf("Provisioner: %s", msg)
				h.markProvisionFailed(ctx, workspaceID, msg, nil)
				return
			}
		}
	}

	// Issue/rotate the workspace auth token + platform→workspace inbound secret
	// and inject both into the config volume. See mintWorkspaceSecrets doc for
	// the shared invariant — every provision path MUST mint here. Plaintext
	// is written into /configs/.auth_token + /configs/.platform_inbound_secret
	// via WriteFilesToContainer, which runs immediately after ContainerStart
	// and wins the race against the Python adapter's startup time (~1-2 s).
	h.mintWorkspaceSecrets(ctx, workspaceID, &cfg)

	url, err := h.provisioner.Start(ctx, cfg)
	if err != nil {
		// F1086 / #1206: persist a generic message so the canvas and
		// GET /workspaces/:id expose something actionable without leaking
		// docker/error internals (image pull messages, volume paths, etc.).
		log.Printf("Provisioner: workspace start failed for %s: %v", workspaceID, err)
		h.markProvisionFailed(ctx, workspaceID, "workspace start failed", nil)
	} else if url != "" {
		// Pre-store the host-accessible URL (http://127.0.0.1:<port>) so the A2A proxy can reach the container.
		// The registry's ON CONFLICT preserves URLs starting with http://127.0.0.1 when the agent self-registers.
		if _, dbErr := db.DB.ExecContext(ctx, `UPDATE workspaces SET url = $1 WHERE id = $2`, url, workspaceID); dbErr != nil {
			log.Printf("Provisioner: failed to store URL for %s: %v", workspaceID, dbErr)
		}
		if cacheErr := db.CacheURL(ctx, workspaceID, url); cacheErr != nil {
			log.Printf("Provisioner: failed to cache URL for %s: %v", workspaceID, cacheErr)
		}
		// Also cache the Docker-internal URL for workspace-to-workspace discovery.
		// Containers on molecule-core-net can reach each other by container name.
		internalURL := provisioner.InternalURL(workspaceID)
		if cacheErr := db.CacheInternalURL(ctx, workspaceID, internalURL); cacheErr != nil {
			log.Printf("Provisioner: failed to cache internal URL for %s: %v", workspaceID, cacheErr)
		}
	}
	// On success, the workspace will register via POST /registry/register
	// which transitions status to 'online' and broadcasts WORKSPACE_ONLINE
}

// seedInitialMemories writes a list of MemorySeed entries through the v2
// memory plugin for the given workspace. Called during workspace creation
// and org import to pre-populate memories from config/template (issue
// #1050). Non-fatal: each plugin call is attempted independently and
// failures are logged.
//
// Issue #1755: post-A1 (#1747) v2 is the only memory backend; writing
// into `agent_memories` would be invisible to `recall_memory` (which
// reads exclusively from the plugin). When the plugin isn't wired
// (WithSeedMemoryPlugin not called, i.e. `MEMORY_PLUGIN_URL` unset on
// platform-tenant), seedInitialMemories logs a loud warning and skips
// — seeded memories simply don't materialise on that operator's
// deployment. There is no SQL fallback.
//
// Scope handling: the v2 plugin's data model has no `scope` column. All
// seeded memories land in `workspace:<id>` (the workspace's private
// namespace). Pre-A1 callers wrote TEAM/GLOBAL-scoped memories into
// `agent_memories` with `namespace=workspace:<id>` anyway, so this is
// behaviour-preserving for LOCAL and a no-op-shrink for TEAM/GLOBAL
// (those scopes can be promoted later via an explicit
// `commit_memory_v2` call by the agent).
//
// maxMemoryContentLength is the maximum allowed size for a single
// memory content field. Content exceeding this limit is truncated to
// prevent storage exhaustion (CWE-400) and OOM on read paths. The
// limit is intentionally generous — it fits a ~64k context window
// worth of text — but small enough to prevent abuse.
const maxMemoryContentLength = 100_000 // ~100 KiB of text

func (h *WorkspaceHandler) seedInitialMemories(ctx context.Context, workspaceID string, memories []models.MemorySeed) {
	if len(memories) == 0 {
		return
	}
	if h.seedMemoryPlugin == nil {
		log.Printf("seedInitialMemories: ⚠️  skipping %d memories for workspace %s — v2 memory plugin not wired (set MEMORY_PLUGIN_URL on platform-tenant). Seeded memories from this template are not persisted.", len(memories), workspaceID)
		return
	}
	namespace := workspaceMemoryNamespace(workspaceID)
	seeded := 0
	for _, mem := range memories {
		scope := strings.ToUpper(mem.Scope)
		if scope == "" {
			scope = "LOCAL"
		}
		if scope != "LOCAL" && scope != "TEAM" && scope != "GLOBAL" {
			log.Printf("seedInitialMemories: skipping memory for %s — invalid scope %q", workspaceID, scope)
			continue
		}
		if mem.Content == "" {
			continue
		}
		// #1066: enforce content length limit to prevent storage exhaustion
		// (CWE-400). Truncate oversized content rather than rejecting the
		// whole insert so that template authors get a predictable fallback
		// rather than a silent skip.
		content := mem.Content
		if len(content) > maxMemoryContentLength {
			content = content[:maxMemoryContentLength]
			log.Printf("seedInitialMemories: truncated memory content for %s (scope=%s) from %d to %d bytes",
				workspaceID, scope, len(mem.Content), maxMemoryContentLength)
		}
		redactedContent, _ := redactSecrets(workspaceID, content)
		if _, err := h.seedMemoryPlugin.CommitMemory(ctx, namespace, contract.MemoryWrite{
			Content: redactedContent,
			// Kind = fact: seeded memories are factual baseline knowledge,
			// not session summaries or runtime checkpoints.
			Kind: contract.MemoryKindFact,
			// Source = runtime: the platform (not the agent) is writing
			// these on the agent's behalf at provision time. Distinct from
			// `agent` (commit_memory MCP call) and `user` (canvas write).
			Source: contract.MemorySourceRuntime,
		}); err != nil {
			log.Printf("seedInitialMemories: plugin commit failed for %s (scope=%s): %v", workspaceID, scope, err)
			continue
		}
		seeded++
	}
	if seeded > 0 {
		log.Printf("seedInitialMemories: seeded %d/%d memories for workspace %s via v2 plugin (namespace=%s)",
			seeded, len(memories), workspaceID, namespace)
	}
}

// workspaceMemoryNamespace returns the canonical v2 memory namespace
// string for a workspace. Matches the form produced by
// internal/memory/namespace/resolver.go for self-reads (issue #1735).
func workspaceMemoryNamespace(workspaceID string) string {
	return fmt.Sprintf("workspace:%s", workspaceID)
}

// conciergeTemplateOrDefault forces the platform-agent template for a
// kind='platform' concierge when no explicit template is set. RFC §5.7: the
// concierge identity (config.yaml + prompts/concierge.md + mcp_servers.yaml) is
// delivered "like any other runtime template" via the platform-agent template
// entry. But the platform-agent workspace row was upserted with no `template`
// (platform_agent.go installPlatformAgent), so payload.Template was empty and
// the identity resolved to the GENERIC claude-code-default config — the
// concierge booted online but with no persona ("doesn't know it's the platform
// agent", #30/#2970). Forcing "platform-agent" here makes the asset fetcher pull
// the concierge identity for every concierge provision/restart, new or existing,
// without depending on the row's template column being backfilled. An explicit
// template (set by a future caller) still wins.
func conciergeTemplateOrDefault(kind, template string) string {
	if kind == models.KindPlatform && strings.TrimSpace(template) == "" {
		return "platform-agent"
	}
	return template
}

func (h *WorkspaceHandler) buildProvisionerConfig(
	ctx context.Context,
	workspaceID, templatePath string,
	configFiles map[string][]byte,
	payload models.CreateWorkspacePayload,
	envVars map[string]string,
	workspaceSecretKeys map[string]struct{},
	pluginsPath string,
) provisioner.WorkspaceConfig {
	// Per-workspace workspace_dir takes priority over global WORKSPACE_DIR env var.
	// If neither is set, the provisioner creates an isolated Docker volume.
	//
	// #65: also read workspace_access (DB column) so restart paths preserve
	// the mode set at create/import time. Payload's WorkspaceAccess (if
	// present) wins, matching the existing WorkspaceDir precedence.
	workspacePath := payload.WorkspaceDir
	workspaceAccess := payload.WorkspaceAccess
	// kind drives the platform-agent image selection in the provisioner (a
	// kind='platform' concierge runs on the platform-agent image variant, which
	// bakes /opt/molecule-mcp-server so the org-admin MCP can load). Sourced from
	// the DB row (CreateWorkspacePayload carries no kind — the row is the SSOT,
	// written by InstallPlatformAgent / EnsureSelfHostedPlatformAgent).
	var kind string
	if db.DB != nil {
		var dbDir, dbAccess, dbKind string
		// QueryRowContext (not QueryRow) so the provision-timeout ctx
		// propagates here too. Previously ctx flowed in only to be passed
		// to resolveRuntimeImage; that dead reader was removed by
		// RFC internal#617 / task #335. Wiring ctx into the surviving DB
		// query keeps the parameter load-bearing and is a small correctness
		// nudge (a 10s ProvisionTimeout now actually bounds this lookup).
		if err := db.DB.QueryRowContext(
			ctx,
			`SELECT COALESCE(workspace_dir, ''), COALESCE(workspace_access, 'none'), COALESCE(kind, 'workspace') FROM workspaces WHERE id = $1`,
			workspaceID,
		).Scan(&dbDir, &dbAccess, &dbKind); err == nil {
			if workspacePath == "" && dbDir != "" {
				workspacePath = dbDir
			}
			if workspaceAccess == "" {
				workspaceAccess = dbAccess
			}
			kind = dbKind
		} else if err != sql.ErrNoRows {
			log.Printf("ERROR: workspace kind lookup failed for %s: %v", workspaceID, err)
		}
	}
	if workspacePath == "" {
		workspacePath = os.Getenv("WORKSPACE_DIR")
	}
	if workspaceAccess == "" {
		workspaceAccess = provisioner.WorkspaceAccessNone
	}

	// RFC#2843 #32/#42: stamp the workspace's DESIRED plugin sources (the DB
	// desired-set) into the box env so the boot-install step (CP
	// userdata_containerized.go) re-establishes /configs/plugins on EVERY
	// (re)provision — surviving the ephemeral-instance restart that a
	// post-online EIC push could not (root-caused 2026-06-17: restart = fresh
	// instance, plugin lived only on the destroyed instance's disk). The box
	// fetches each source itself (gitea:// via its read-only PAT); only the
	// small source LIST rides the env, never the skill content. Non-fatal.
	//
	// #42: the desired-set is the UNION of declared (template intent) AND
	// installed (workspace_plugins — the live runtime set). DECLARED-only wiped
	// any plugin the user installed at runtime via install_plugin on the next
	// restart; desiredPluginSources() unions them so user additions survive.
	if srcs, derr := desiredPluginSources(ctx, workspaceID); derr != nil {
		log.Printf("workspace provision: desired plugin sources for %s: %v (continuing without MOLECULE_DECLARED_PLUGINS)", workspaceID, derr)
	} else if len(srcs) > 0 {
		if envVars == nil {
			envVars = make(map[string]string)
		}
		envVars["MOLECULE_DECLARED_PLUGINS"] = strings.Join(srcs, ",")
	}

	return provisioner.WorkspaceConfig{
		WorkspaceID:     workspaceID,
		TemplatePath:    templatePath,
		Template:        payload.Template,
		ConfigFiles:     configFiles,
		PluginsPath:     pluginsPath,
		WorkspacePath:   workspacePath,
		WorkspaceAccess: workspaceAccess,
		Kind:            kind,
		Tier:            payload.Tier,
		Runtime:         payload.Runtime,
		InstanceType:    payload.Compute.InstanceType,
		DiskGB:          int32(payload.Compute.Volume.RootGB),
		DataPersistence: payload.Compute.DataPersistence,
		Provider:        payload.Compute.Provider,
		Display: provisioner.WorkspaceDisplayConfig{
			Mode:     payload.Compute.Display.Mode,
			Width:    payload.Compute.Display.Width,
			Height:   payload.Compute.Display.Height,
			Protocol: payload.Compute.Display.Protocol,
		},
		EnvVars: envVars,
		// Forensic #145: positive provenance set so the SCM-write-token guard
		// (cp_provisioner.Start) exempts a workspace-authored GITEA_TOKEN from
		// the operator-bleed strip while still stripping global/persona-merged
		// SCM tokens. Carried by both Docker- and CP-mode configs.
		WorkspaceSecretKeys: workspaceSecretKeys,
		PlatformURL:         h.platformURL,
		// Image left empty — molecule-core's runtime_image_pins table (mig
		// 047, dead reader removed by RFC internal#617 / task #335) was an
		// aspirational SSOT that never received a writer. CP's
		// runtime_image_pins (CP migration 027) is the single SSOT; the
		// pin is applied at CP's provisioner layer before this code path
		// runs. Empty here means selectImage() falls back to the legacy
		// RuntimeImages[Runtime] :latest lookup, which is what the dead
		// reader's sql.ErrNoRows path was producing already.
		Image: "",

		// RFC #2843 #24 PR-B + Phase 1 template decoupling: derive the
		// template identity from the installed template first, falling back
		// to the runtime's default template. For kind='platform' concierges
		// with no explicit template, force "platform-agent" so the concierge
		// persona/config is delivered (RFC §5.7; #30/#2970). The empty
		// identity tells the SCAFFOLD gate in collectCPConfigFiles to skip
		// the fetcher (external runtimes).
		TemplateIdentity:     templateIdentityOrEmpty(resolveTemplateIdentity(conciergeTemplateOrDefault(kind, payload.Template), payload.Runtime)),
		TemplateAssetFetcher: h.giteaTemplateFetcher,
	}
}

// templateIdentityOrEmpty is a tiny wrapper around resolveTemplateIdentity
// that returns "" on miss (rather than the (string, bool) tuple). Used at the
// call site so the assignment can be a single expression.
func templateIdentityOrEmpty(id string, ok bool) string {
	if !ok {
		return ""
	}
	return id
}

// templateIdentityForTemplateOrRuntime resolves the template-asset fetch
// identity, preferring the explicit TEMPLATE over the runtime. The manifest's
// workspace_templates (templateRepoByName) are keyed by TEMPLATE NAME
// (claude-code-default, seo-agent, platform-agent, …), NOT by runtime. A
// template VARIANT like seo-agent has runtime="claude-code" but
// template="seo-agent"; keying the fetch on runtime looked up
// templateRepoByName["claude-code"] (no such key) → empty identity → the
// fetcher delivered NOTHING, so the seo-agent box got a stub config.yaml.
// With this fix the seo-agent identity resolves and config.yaml + prompts
// arrive via the asset channel. (agent-skills/seo-all is no longer carried
// here at all — RFC#2843 #32: skills are plugins, installed post-online.)
// Falls back to runtime for the common case where runtime==template name
// (hermes/codex/openclaw/google-adk), and to "" when neither resolves (external
// runtimes — collectCPConfigFiles treats empty identity as "skip the fetcher").
func templateIdentityForTemplateOrRuntime(template, runtime string) string {
	if t := strings.TrimSpace(template); t != "" {
		if id, ok := templateIdentityForRuntime(t); ok {
			return id
		}
	}
	return templateIdentityOrEmpty(templateIdentityForRuntime(runtime))
}

// issueAndInjectToken rotates the workspace auth token and injects the
// plaintext into cfg.ConfigFiles[".auth_token"] so it is written into the
// /configs volume by WriteFilesToContainer immediately after the container
// starts (issue #418: container rebuild wipes /configs/.auth_token).
//
// Rotation strategy: since the DB only stores sha256(plaintext) we can never
// recover an existing token. We revoke all live tokens first and issue a
// fresh one. On any error the injection is skipped and a warning is logged;
// provisioning continues — the workspace will get 401 on its first heartbeat
// and can recover on the next restart.
func (h *WorkspaceHandler) issueAndInjectToken(ctx context.Context, workspaceID string, cfg *provisioner.WorkspaceConfig) {
	// Revoke existing live INSTANCE tokens FIRST — this must run in both
	// modes. API-kind tokens (the Create 201 inline bearer, admin/Token
	// mints) are deliberately NOT revoked: provisioning invalidating the
	// caller bearer it just returned was the core#1644 contract break.
	// In SaaS mode the revoke is load-bearing on re-provision: without it,
	// the previous workspace instance's live token sits in the DB, and
	// RegistryHandler.requireWorkspaceToken on the fresh instance's first
	// /registry/register would reject it (live token exists → no
	// bootstrap allowance, but the new workspace has no plaintext because
	// the CP provisioner doesn't carry cfg.ConfigFiles across user-data).
	// Revoking clears the gate so the register handler's bootstrap path
	// can mint a fresh token and return the plaintext in the response.
	if err := wsauth.RevokeInstanceTokensForWorkspace(ctx, db.DB, workspaceID); err != nil {
		log.Printf("Provisioner: failed to revoke existing tokens for %s: %v — skipping auth-token injection", workspaceID, err)
		return
	}

	// SaaS mode skips the IssueToken + ConfigFiles write because both
	// only make sense on the Docker provisioner's volume-mount delivery
	// path. The register handler mints a fresh token on first successful
	// register and returns the plaintext in the response body for the
	// runtime to persist locally.
	if saasMode() {
		return
	}

	token, err := wsauth.IssueToken(ctx, db.DB, workspaceID)
	if err != nil {
		log.Printf("Provisioner: failed to issue auth token for %s: %v — skipping auth-token injection", workspaceID, err)
		return
	}

	ensureConfigFiles(cfg)[".auth_token"] = []byte(token)
	// Option B (issue #1877): write token to volume BEFORE ContainerStart.
	// Pre-write eliminates the race window where a restarted container could
	// read a stale /configs/.auth_token before WriteFilesToContainer runs.
	// This call is best-effort — if it fails (or provisioner is nil in tests)
	// we still log and fall through; the runtime's heartbeat.py will retry
	// on 401 if needed.
	if h.provisioner != nil {
		if writeErr := h.provisioner.WriteAuthTokenToVolume(ctx, workspaceID, token); writeErr != nil {
			log.Printf("Provisioner: warning — pre-write token to volume failed for %s: %v (token still injected via WriteFilesToContainer after start)", workspaceID, writeErr)
		}
	}
	log.Printf("Provisioner: injected fresh auth token for workspace %s into config volume", workspaceID)
}

// issueAndInjectInboundSecret mints the platform→workspace shared secret
// (RFC #2312, migration 044) and persists the plaintext into the
// workspaces.platform_inbound_secret column so platform-side handlers can
// read it back on every forward call.
//
// Docker mode also writes the plaintext into cfg.ConfigFiles
// [".platform_inbound_secret"] so WriteFilesToContainer drops it on the
// /configs volume alongside .auth_token.
//
// SaaS mode persists to the DB but does NOT write a local file from
// here — there is no workspace-server-managed volume in SaaS. The
// workspace receives the secret out-of-band via the /registry/register
// response (mirrors the existing .auth_token bootstrap path).
//
// Best-effort: failure logs and continues. The workspace-side
// /internal/* handlers fail-closed when the file is missing, so a
// failed mint surfaces as 401 on the platform's first forward call —
// loud, debuggable, no silent fail-open.
func (h *WorkspaceHandler) issueAndInjectInboundSecret(ctx context.Context, workspaceID string, cfg *provisioner.WorkspaceConfig) {
	secret, err := wsauth.IssuePlatformInboundSecret(ctx, db.DB, workspaceID)
	if err != nil {
		log.Printf("Provisioner: failed to issue platform_inbound_secret for %s: %v — chat upload + other /internal endpoints will 401", workspaceID, err)
		return
	}

	if saasMode() {
		// Plaintext lives in the DB column; the workspace will fetch it
		// via /registry/register response (handled in a follow-up PR).
		log.Printf("Provisioner: minted platform_inbound_secret for %s (SaaS mode — workspace will receive via register response)", workspaceID)
		return
	}

	ensureConfigFiles(cfg)[".platform_inbound_secret"] = []byte(secret)
	log.Printf("Provisioner: injected platform_inbound_secret for workspace %s into config volume", workspaceID)
}

// ensureConfigFiles returns cfg.ConfigFiles as a non-nil
// map[string][]byte, allocating it if necessary. Extracted
// from the two inject sites (issueAndInjectToken +
// issueAndInjectInboundSecret) that previously each had a
// `if cfg.ConfigFiles == nil { cfg.ConfigFiles = make(...) }`
// ceremony — same pattern, repeated.
//
// Behavior-preserving: the helper does EXACTLY what the inline
// check did. The allocation is lazy (on first write only), the
// returned map is the SAME map cfg.ConfigFiles points to
// (so subsequent writes are visible to the caller), and the
// no-op case (already-allocated map) is a pointer return with
// no allocation.
//
// Note: this helper is the "extend" direction — it allocates
// ONLY when nil. The contract is "I want to write a key; give
// me a writable map." Readers should use cfg.ConfigFiles
// directly (with a nil-check if they need to distinguish
// "not yet populated" from "populated with zero keys").
func ensureConfigFiles(cfg *provisioner.WorkspaceConfig) map[string][]byte {
	if cfg.ConfigFiles == nil {
		cfg.ConfigFiles = make(map[string][]byte)
	}
	return cfg.ConfigFiles
}

// findTemplateByName looks for a workspace-configs-templates directory matching a name.
func findTemplateByName(configsDir, name string) string {
	entries, err := os.ReadDir(configsDir)
	if err != nil {
		return ""
	}
	// Normalize name: "SEO Agent" → look for "seo-agent"
	normalized := strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	for _, e := range entries {
		if e.IsDir() && e.Name() == normalized {
			return e.Name()
		}
	}
	// Also search by config.yaml name field (for templates like org-pm where dir name != workspace name)
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "ws-") {
			continue
		}
		cfgPath := filepath.Join(configsDir, e.Name(), "config.yaml")
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			continue
		}
		// Quick YAML name extraction (avoids importing yaml parser)
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "name:") {
				cfgName := strings.TrimSpace(strings.TrimPrefix(line, "name:"))
				if strings.EqualFold(cfgName, name) {
					return e.Name()
				}
				break
			}
		}
	}
	return ""
}

func resolveWorkspaceTemplatePath(configsDir, cacheDir, template string) (string, error) {
	if cacheDir != "" {
		if p, err := resolveInsideRoot(cacheDir, template); err != nil {
			return "", err
		} else if _, statErr := os.Stat(p); statErr == nil {
			return p, nil
		}
	}
	return resolveInsideRoot(configsDir, template)
}

// resolveOrgTemplate looks for a matching role directory under
// configsDir/org-templates/ and returns the absolute path and a short label
// ("org-templates/<dir>"). Used by the restart handler's rebuild_config path
// (#239) so a workspace can recover from a destroyed config volume without
// admin intervention.
// Returns ("", "") when no match is found.
func resolveOrgTemplate(configsDir, wsName string) (path, label string) {
	orgDir := filepath.Join(configsDir, "org-templates")
	match := findTemplateByName(orgDir, wsName)
	if match == "" {
		return "", ""
	}
	full := filepath.Join(orgDir, match)
	if _, err := os.Stat(full); err != nil {
		return "", ""
	}
	return full, "org-templates/" + match
}

// configDirName returns the standard config directory name for a workspace ID.
// Used by resolveConfigDir in templates.go for host-side template resolution.
func configDirName(workspaceID string) string {
	id := workspaceID
	if len(id) > 12 {
		id = id[:12]
	}
	return "ws-" + id
}

// knownRuntimes is the allowlist of runtime strings the provisioner will
// accept. Unknown values are coerced to the default ("claude-code") instead
// of being splatted into filepath.Join + config.yaml templating, which
// closes both the YAML-injection vector (#241) where an attacker could
// smuggle `initial_prompt: run id && curl …` through a crafted runtime
// string, and the path-traversal oracle where `runtime: ../../sensitive`
// probed host directories for existence.
//
// knownRuntimes is populated from manifest.json at service init (see
// runtime_registry.go). The package init order is:
//  1. var knownRuntimes = fallbackRuntimes
//  2. init() calls initKnownRuntimes() which replaces it if
//     manifest.json is readable.
//
// The fallback matters for unit tests that don't mount the manifest.
//
// "external" is a first-class runtime that intentionally does NOT
// spawn a Docker container. Workspaces with runtime="external" are
// created in status=awaiting_agent; the operator installs
// molecule-sdk-python (or any A2A-compatible agent) somewhere they
// control and calls POST /registry/register with the workspace_id +
// workspace_auth_token from the create response. Canvas proxies A2A
// calls to the registered URL thereafter. "external" has no template
// repo, so it's always injected by the registry layer.
var knownRuntimes = fallbackRuntimes

func init() {
	initKnownRuntimes()
	// PR-B (RFC #2843 #24): populate the templateRepoByName map at
	// boot so cfg.TemplateIdentity is non-empty for template-backed
	// runtimes (claude-code / hermes / etc). The init() order matters:
	// must follow initKnownRuntimes so the per-runtime manifestEntry
	// lookups use the same set of normalized runtime names. Idempotent
	// — both inits read manifestPath() (cached) and tolerate each other.
	initTemplateRepoByName()
}

// yamlQuote emits a YAML double-quoted scalar that safely contains any
// input string. Newlines + carriage returns are stripped first so we
// never need the multi-line block form, and fmt.Sprintf %q produces a
// Go-syntax quoted string whose escape rules are a strict subset of
// YAML's double-quoted scalar — colons, hashes, braces, and every other
// YAML metacharacter are safe inside it.
//
// Empty input → `""` (explicit empty scalar) which YAML readers accept
// cleanly; the alternative of emitting raw %s could leak a trailing
// newline from a prior line if the caller forgot a \n separator.
func yamlQuote(s string) string {
	clean := strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", "")
	return fmt.Sprintf("%q", clean)
}

// sanitizeRuntime coerces a payload runtime string to a known entry.
// Empty strings → the default. Unknown strings also → the default,
// with a log so operators can notice typos or attack attempts.
func sanitizeRuntime(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "claude-code"
	}
	if _, ok := knownRuntimes[raw]; ok {
		return raw
	}
	log.Printf("provisioner: rejected unknown runtime %q, falling back to claude-code", raw)
	return "claude-code"
}

// ensureDefaultConfig generates minimal config files in memory for workspaces without a template.
// Returns a map of filename → content to be written into the container's /configs volume.
//
// #2248 follow-up (provider-correctness): if the provider registry is
// available and the runtime/model IS known, but DeriveProvider errors,
// the error is propagated so provisioning is blocked rather than
// generating a providerless config that re-derives to the wrong provider
// at runtime. Unknown/federated runtimes and derive-misses still return
// a providerless config (preserving today's pass-through behavior).
func (h *WorkspaceHandler) ensureDefaultConfig(workspaceID string, payload models.CreateWorkspacePayload) (map[string][]byte, error) {
	files := make(map[string][]byte)

	// Determine runtime — pass through the allowlist so an attacker
	// can't smuggle `initial_prompt: …` or a path-traversal oracle
	// via a crafted runtime string (#241).
	runtime := sanitizeRuntime(payload.Runtime)

	// Generate a minimal config.yaml.
	//
	// SSOT (CTO 2026-05-22): model is REQUIRED user input. The platform
	// must not provide a default; the runtime must not fall back. The
	// Create handler is responsible for rejecting empty model BEFORE
	// reaching provisionWorkspace; this is a defence-in-depth assertion.
	// If we hit here with an empty model the YAML below would still
	// render a `model: ""` line — which renders all downstream provider
	// derivation undefined. Log loudly and let the workspace boot into
	// not_configured rather than masking the contract violation with a
	// silently-broken default (the prior `anthropic:claude-opus-4-7`
	// fallback was the canonical example — every codex workspace
	// created without an explicit model wedged).
	model := payload.Model
	if model == "" {
		log.Printf("ensureDefaultConfig: workspace %s reached provisioning with empty model — Create handler should have rejected this; rendering empty model: \"\" in config.yaml (workspace will boot not_configured)", workspaceID)
	}

	// Derive the provider from the providers manifest and stamp it into the
	// generated config BEFORE claude-code model normalization strips the
	// slash-prefix. DeriveProvider needs the FULL, un-normalized model id
	// (e.g. "moonshot/kimi-k2.6") for the exact-id match that resolves the
	// canvas claude-code case to provider=platform — normalizing to
	// "kimi-k2.6" first would lose that match.
	//
	// Why this exists (RFC#340 Fix A): a canvas-created claude-code workspace
	// with model "moonshot/kimi-k2.6" booted NOT_CONFIGURED — the adapter
	// derived provider="moonshot" (slash-split of the model id) which is not
	// in the providers registry. CP bakes `provider: platform` via heredoc,
	// but the cp#329 config-bundle fetch overwrites /configs/config.yaml with
	// THIS (previously providerless) bundle version, so molecule-runtime
	// config.py re-derived the wrong provider. Stamping the manifest-derived
	// provider here (mirroring CP's buildModelProviderYAML shape) makes the
	// config the adapter reads carry the canonical provider.
	//
	// Reuses the SAME manifest path the config-SAVE validators use
	// (providerRegistry() + Manifest.DeriveProvider; see
	// model_registry_validation.go). On a derive MISS (unknown/unregistered
	// model for a known runtime) provider is left empty and the field is
	// omitted below — preserving today's behavior. On a registry load error
	// or an exceptional DeriveProvider failure for a KNOWN runtime/model,
	// the error is propagated and provisioning is blocked.
	derivedProvider, err := deriveDefaultConfigProvider(runtime, model)
	if err != nil {
		return nil, fmt.Errorf("ensureDefaultConfig: provider derivation failed for workspace %s (runtime=%s model=%s): %w", workspaceID, runtime, model, err)
	}

	if runtime == "claude-code" {
		model = normalizeClaudeCodeModel(model)
	}

	// Sanitize name/role/model for YAML safety — always double-quote so
	// a crafted value with a newline or colon can't terminate the scalar
	// and inject an arbitrary key into the generated config. runtime is
	// already allowlisted above so it does not need quoting.
	//
	// Pattern: strip newlines (unrepresentable in a double-quoted YAML
	// scalar without escaping), then emit via %q which produces a Go-
	// syntax quoted string — valid YAML double-quoted scalar because
	// the character sets overlap for this field-value shape.
	quoteName := yamlQuote(payload.Name)
	quoteRole := yamlQuote(payload.Role)
	quoteModel := yamlQuote(model)
	configYAML := fmt.Sprintf("name: %s\ndescription: %s\nversion: 1.0.0\ntier: %d\nruntime: %s\n",
		quoteName, quoteRole, payload.Tier, runtime)
	if runtime == "claude-code" {
		if providersYAML := h.defaultTemplateProvidersYAML(runtime); providersYAML != "" {
			configYAML += providersYAML + "\n"
		}
	}

	// Model always at top level — config.py reads raw["model"] for all runtimes.
	configYAML += fmt.Sprintf("model: %s\n", quoteModel)

	// Stamp the manifest-derived provider at top level (mirroring CP's
	// buildModelProviderYAML). Omitted entirely on a derive miss so the prior
	// behavior — no `provider:` key, runtime re-derives — is preserved for
	// unregistered models (requirement #3).
	if derivedProvider != "" {
		configYAML += fmt.Sprintf("provider: '%s'\n", yamlEscapeSingleQuotedProvider(derivedProvider))
	}

	// Add runtime_config. required_env is intentionally omitted — the
	// platform injects secrets at container-start time via the secrets API,
	// and preflight already validates that the env vars are present before
	// the agent loop starts.  Hardcoding token names here caused #1028
	// (expired CLAUDE_CODE_OAUTH_TOKEN baked into config.yaml).
	configYAML += "runtime_config:\n"
	if runtime == "claude-code" {
		configYAML += fmt.Sprintf("  model: %s\n", quoteModel)
	}
	// Mirror the top-level provider under runtime_config (CP writes both).
	if derivedProvider != "" {
		configYAML += fmt.Sprintf("  provider: '%s'\n", yamlEscapeSingleQuotedProvider(derivedProvider))
	}
	configYAML += "  timeout: 0\n"

	files["config.yaml"] = []byte(configYAML)

	log.Printf("Provisioner: generated %d config files for workspace %s (runtime: %s)", len(files), workspaceID, runtime)
	return files, nil
}

// deriveDefaultConfigProvider resolves the provider name the adapter should
// see for (runtime, model) using the SAME providers manifest the config-SAVE
// validators use (providerRegistry() + Manifest.DeriveProvider; see
// model_registry_validation.go).
//
// Failure modes:
//   - Empty model → ("", nil) — pass-through, no provider stamp.
//   - Registry unavailable/load-error → ("", error) — fail-closed; provisioning
//     must not proceed on a degraded registry.
//   - Unknown/federated runtime → ("", nil) — pass-through; no first-party
//     provider exists, the runtime re-derives at boot.
//   - Known runtime + known model, but DeriveProvider errors (ambiguous match,
//     overlap, etc.) → ("", error) — fail-closed; a known model should never
//     fail derivation, so silently omitting the provider would generate a
//     providerless config that re-derives to the WRONG provider at runtime
//     (the moonshot→platform NOT_CONFIGURED class, #2248 follow-up).
//   - Known runtime + unregistered model (derive miss) → ("", nil) —
//     pass-through; preserves today's behavior for unregistered models.
//
// `model` must be the FULL, un-normalized id (e.g. "moonshot/kimi-k2.6") so
// DeriveProvider's exact-id match resolves the canvas claude-code case to
// provider=platform. The availableAuthEnv arg is nil here — config-generation
// has no per-workspace auth context yet (secrets are injected at container
// start), matching the validators' nil call.
func deriveDefaultConfigProvider(runtime, model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		return "", nil
	}
	m, err := providerRegistry()
	if err != nil || m == nil {
		// Registry unavailable (a build-time defect the gen/sync gates catch).
		// Fail closed — don't provision on a degraded registry.
		return "", fmt.Errorf("provider registry unavailable: %w", err)
	}
	return deriveDefaultConfigProviderFromManifest(m, runtime, model)
}

// deriveDefaultConfigProviderFromManifest contains the core logic so it can be
// unit-tested with mock manifests without touching the package-level singleton.
func deriveDefaultConfigProviderFromManifest(manifest *providers.Manifest, runtime, model string) (string, error) {
	// Unknown/federated runtime — no first-party provider exists.
	// Pass-through explicitly so federation is not broken.
	native, ok := manifest.Runtimes[runtime]
	if !ok {
		return "", nil
	}

	p, err := manifest.DeriveProvider(runtime, model, nil)
	if err != nil {
		// Derive miss for a known runtime (unregistered model) → pass-through.
		// We detect "known" vs "unknown" by checking whether the model is
		// recognized by ANY native provider of this runtime — either via an
		// exact-id match or a prefix match. If the runtime knows the model
		// (exact or prefix) but DeriveProvider still errors, the error is
		// exceptional (ambiguous prefix, overlap, etc.) and must fail-closed.
		// If the runtime does NOT recognize the model at all, it's a genuine
		// derive miss and the providerless config is the correct fallback.
		byName := make(map[string]providers.Provider, len(manifest.Providers))
		for _, prov := range manifest.Providers {
			byName[prov.Name] = prov
		}
		knownModel := false
		for _, ref := range native.Providers {
			// Exact-id match
			for _, mid := range ref.Models {
				if mid == model {
					knownModel = true
					break
				}
			}
			if knownModel {
				break
			}
			// Prefix match
			if prov, ok := byName[ref.Name]; ok && prov.MatchesModel(model) {
				knownModel = true
				break
			}
		}
		if knownModel {
			return "", fmt.Errorf("derive provider for known runtime/model %s/%s: %w", runtime, model, err)
		}
		return "", nil
	}
	return p.Name, nil
}

// yamlEscapeSingleQuotedProvider escapes a value for a YAML single-quoted
// scalar, mirroring CP's buildModelProviderYAML (a literal single quote is
// doubled). Provider names are registry-controlled identifiers, so this is a
// defense-in-depth measure rather than a hot path.
func yamlEscapeSingleQuotedProvider(v string) string {
	return strings.ReplaceAll(v, "'", "''")
}

func normalizeClaudeCodeModel(model string) string {
	model = strings.TrimSpace(model)
	if before, after, ok := strings.Cut(model, "/"); ok && before != "" && after != "" {
		return after
	}
	return model
}

func (h *WorkspaceHandler) defaultTemplateProvidersYAML(runtime string) string {
	if h.configsDir == "" {
		return ""
	}
	templateName := runtime + "-default"
	templatePath, err := resolveWorkspaceTemplatePath(h.configsDir, h.cacheDir, templateName)
	if err != nil {
		log.Printf("Provisioner: default template providers skipped for runtime %s: %v", runtime, err)
		return ""
	}
	data, err := os.ReadFile(filepath.Join(templatePath, "config.yaml"))
	if err != nil {
		return ""
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		log.Printf("Provisioner: default template providers skipped for runtime %s: invalid YAML: %v", runtime, err)
		return ""
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return ""
	}

	mapping := root.Content[0]
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "providers" {
			continue
		}
		out := yaml.Node{
			Kind: yaml.MappingNode,
			Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "providers"},
				mapping.Content[i+1],
			},
		}
		encoded, err := yaml.Marshal(&out)
		if err != nil {
			log.Printf("Provisioner: default template providers skipped for runtime %s: marshal failed: %v", runtime, err)
			return ""
		}
		return strings.TrimRight(string(encoded), "\n")
	}
	return ""
}

// internal#718 P4 closure — `deriveProviderFromModelSlug` (retire-list #3)
// has been removed together with its only caller (WorkspaceHandler.Create's
// setProviderSecret write) and the LLM_PROVIDER workspace_secret it
// populated.
//
// The hand-rolled prefix switch was a Go mirror of
// workspace-configs-templates/hermes/scripts/derive-provider.sh kept in
// sync via a drift test. The replacement is providers.Manifest.DeriveProvider
// (synced in P2-A), which derives the provider from (runtime, model)
// against the registry SSOT at every decision point — billing (P2-B),
// CP user-data emission (this PR's CP-side commit), validation
// (P3 PR-C). The shell script in the hermes template continues to be the
// runtime fallback for unregistered models; codegen of the template's
// providers block from the registry is the P4 follow-up gated on
// registry data growth.

// applyRuntimeModelEnv exposes the workspace's selected model via an
// env var the target runtime's install.sh / start.sh knows to read.
// Each runtime owns its own env-var contract — the tenant just plumbs
// the value through so CP can bake it into user-data.
//
// Why per-runtime rather than a generic MOLECULE_MODEL: each runtime
// installer has its own config schema and naming (hermes writes to
// ~/.hermes/config.yaml with `model.default`; codex reads from
// /configs/config.yaml directly; future IoT/robotics targets may have
// firmware manifests). Keeping the contract owned by the runtime
// template means adding a new runtime doesn't require edits on the
// tenant side for each one.
//
// For runtimes with no env-based model override (codex etc. read
// model from /configs/config.yaml which CP user-data generates from
// payload.Model at boot), this is a no-op — no harm in the switch
// being empty for those cases.
func applyRuntimeModelEnv(envVars map[string]string, runtime, model string) {
	// Resolution order (priority high → low):
	//   1. payload.Model (caller passed the canvas-picked model id verbatim)
	//   2. envVars["MOLECULE_MODEL"]  (the canonical, unambiguous name)
	//   3. envVars["MODEL"]  (workspace_secret — written by SetModel /
	//      WorkspaceHandler.Create / persona env file; the only correct
	//      home for a picked model id).
	//
	// Pre-fix bug (2026-05-08): this function used to consult
	// envVars["MODEL_PROVIDER"] as a fourth fallback AND unconditionally
	// overwrite envVars["MODEL"] with that slug when payload.Model was
	// empty. The MODEL_PROVIDER key was misleadingly named — it carried
	// a model id, never a provider — and the persona-env convention
	// sometimes (mis)set it to a provider slug ("minimax") or a runtime
	// name ("claude-code"), neither a valid model id. Symptom: a
	// workspace whose persona env said MODEL=MiniMax-M2.7-highspeed
	// booted fine on first /org/import, then on the next Restart the
	// workspace_secrets-derived MODEL got clobbered by
	// MODEL_PROVIDER="minimax" — the literal slug, not a valid model
	// id — and the workspace template's adapter routed to providers[0]
	// (anthropic-oauth) and wedged at SDK initialize.
	//
	// The 2026-05-19 follow-up fix (this commit) renamed the
	// workspace_secrets row MODEL_PROVIDER → MODEL (root cause: the
	// misleading column name; see secrets.go + the
	// 20260519000000_workspace_secrets_model_provider_rename migration)
	// and drops the MODEL_PROVIDER fallback here so the fallback chain
	// can no longer confuse a provider slug for a model id. CP-side
	// slot-separation (cp#213 + cp#220) merged the analogous fix on
	// the CP side; this is the workspace-server companion.
	model = effectiveModelForBilling(model, envVars)
	if model == "" {
		return
	}

	// Canonical model env vars — molecule-runtime's workspace/config.py
	// resolves the picked model as MOLECULE_MODEL > MODEL > (legacy)
	// MODEL_PROVIDER (#280; the legacy env-var fallback in the Python
	// runtime is independent of the workspace_secrets row rename — it
	// still reads the env var for back-compat with already-running
	// images, but workspace-server no longer emits it). Export both new
	// names so adapters can read either; MODEL stays for backwards
	// compat with everything that already reads os.environ["MODEL"]
	// (the claude-code adapter does, since #194). Without this, the
	// user's canvas selection is silently dropped on every templated
	// provision — confirmed via crash-loop diagnosis on 2026-05-02
	// where MiniMax picks booted with model=sonnet (template default)
	// and demanded CLAUDE_CODE_OAUTH_TOKEN. Set these FIRST so the
	// per-runtime branches below can layer on additional vendor-
	// specific names without fighting over the canonical one.
	envVars["MOLECULE_MODEL"] = model
	envVars["MODEL"] = model

	switch runtime {
	case "hermes":
		// template-hermes install.sh reads this into ~/.hermes/config.yaml's
		// model.default field; derives HERMES_INFERENCE_PROVIDER from the
		// slug prefix (minimax/…, anthropic/…, openai/…, etc.) when the
		// provider isn't explicitly set.
		envVars["HERMES_DEFAULT_MODEL"] = model
	}
}

// effectiveModelForBilling resolves the picked model id from an explicit
// argument with the SAME fallback chain applyRuntimeModelEnv uses to set the
// container MODEL env: explicit arg → envVars["MOLECULE_MODEL"] →
// envVars["MODEL"] (the workspace_secret). It is the single source of truth
// for "what model is this workspace going to run", shared by both
// applyRuntimeModelEnv (which exports it to the container) and
// applyPlatformManagedLLMEnv (which derives the billing mode from it).
//
// molecule-core#1994: the billing resolver MUST consult the same effective
// model the container will actually run. Pre-fix it used the raw payload.Model
// only, which is "" on a re-provision (the payload is rebuilt from the DB with
// no Model), so it derived from an empty model → defaulted closed to
// platform_managed and diverged from the read endpoint (which reads the stored
// MODEL secret). Returns "" only when no model is resolvable anywhere — the
// legitimate "unset → platform default" case the resolver fails closed on.
func effectiveModelForBilling(model string, envVars map[string]string) string {
	if model == "" {
		model = envVars["MOLECULE_MODEL"]
	}
	if model == "" {
		model = envVars["MODEL"]
	}
	return model
}

// applyPlatformManagedLLMEnv wires the control-plane LLM proxy into a
// workspace only when the RESOLVED billing mode for this workspace is
// platform_managed. "Resolved" is PER-WORKSPACE only: the workspace-level
// override (if any) wins, else the mode is DERIVED from the workspace's
// selected model/provider (platform-namespaced → platform_managed; a specific
// vendor → byok). There is NO org-level billing mode (retired 2026-06-12); the
// SSOT resolver (ResolveLLMBillingModeDerived) never reads any org input.
//
// Default-closed: any resolver error / garbled enum / underivable model
// collapses to the deployment default (platform_managed when a proxy is wired,
// byok on self-host — see llm_billing_mode.go).
//
// The resolved mode is exported into the workspace container as
// MOLECULE_LLM_BILLING_MODE_RESOLVED so an in-container debug check can
// answer "what mode is this workspace running under" without DB queries
// (RFC Observability hot-spot).
//
// molecule-core#1994 (credential-handling follow-on, CTO-confirmed model).
// `global_secrets` is the TENANT's own secret store, shared across all of
// that tenant's workspaces — it is NOT the platform's. The platform's own
// LLM credential is the CP proxy usage token (MOLECULE_LLM_USAGE_TOKEN),
// injected SEPARATELY on the platform_managed path below; it is never stored
// in any tenant's global_secrets.
//
// Consequently the byok/disabled branch does NOT strip the tenant's
// global-origin LLM creds. Under the corrected model the tenant's own
// credential — whether at global scope (a global_secrets row, e.g. the key
// they configured via the org-import required-env preflight / the settings
// Secrets tab) or at workspace scope (a workspace_secrets row) — is exactly
// what byok must run on, direct. The earlier internal#711 strip rested on the
// inverted premise that a global-scope LLM cred was "the platform's own"; it
// was wrong and it killed legitimate byok workspaces (MISSING_BYOK_CREDENTIAL
// for tenants whose oauth lived at global scope — Reno Stars Marketing agent,
// confirmed live 2026-05-28). Removing the strip is only safe because the
// platform's own credential is never co-mingled into a tenant's global_secrets
// (guarded at the write boundary: SetGlobal rejects bypass-list keys for a
// platform-managed tenant; the platform proxy token is read from server env
// only, never persisted to a tenant store).
//
// The boolean return still reports whether the workspace has at least one
// usable LLM credential. The caller (prepareProvisionContext) uses it to FAIL
// CLOSED — a byok workspace with no usable LLM credential at ANY scope is
// aborted with a clear MISSING_BYOK_CREDENTIAL error at provision time rather
// than started credential-less.
// platformLLMEnvResult is the structured outcome of applyPlatformManagedLLMEnv.
// ResolvedMode is the per-workspace billing/provider mode the resolver
// landed on. HasUsableLLMCred reports whether the workspace has at least one
// platform-managed-shaped LLM credential key in its env — the tenant's own,
// at global or workspace scope. Only the non-platform (byok) path consults
// HasUsableLLMCred for the fail-closed decision; the platform_managed path
// always returns true (it forces the CP proxy usage token, which IS the
// usable credential).
type platformLLMEnvResult struct {
	ResolvedMode     string
	HasUsableLLMCred bool
	// Source records which layer decided the mode (internal#718 P2-B + core#2608):
	// workspace_override, org_default, derived_provider (registry derivation),
	// derived_default (derive failed → platform default), or constant_fallback
	// (DB error). Surfaced for observability + asserted by the behavior-delta
	// tests so a regression of "derived, not stored" flips red.
	Source BillingModeSource
}

// globalKeys is the provenance side-channel from loadWorkspaceSecrets: the set
// of env keys that originated from the operator-controlled global_secrets table
// (a workspace_secrets row of the same name overrides and clears the flag). It
// is consumed ONLY on the byok/disabled branch's provider-matched strip
// (internal#728 Bug 1): a global-origin LLM bypass cred that does NOT match the
// resolved provider's auth_env is stripped so a greedy runtime (claude-code
// prefers CLAUDE_CODE_OAUTH_TOKEN) cannot route a non-anthropic model to the
// wrong upstream. May be nil (no global-origin keys / unknown provenance) — a
// nil set strips nothing, preserving the pre-#728 behavior for callers that do
// not thread provenance.
func applyPlatformManagedLLMEnv(ctx context.Context, envVars map[string]string, workspaceID, runtime, model string, globalKeys map[string]struct{}) platformLLMEnvResult {
	// internal#718 P2-B: the platform-vs-byok decision now DERIVES the provider
	// from (runtime, model) via the registry and keys off IsPlatform(derived) —
	// NOT a stored LLM_PROVIDER and NOT the org rung. This path already carries
	// runtime + model + the workspace env, so it calls the DERIVED resolver
	// directly (no DB round-trip for runtime/model). availableAuthEnv is the set
	// of recognized provider auth-env-var NAMES present in envVars (the same
	// disambiguation input the registry uses to split oauth-vs-api). There is NO
	// org-level billing mode input: the org-env MOLECULE_LLM_BILLING_MODE was
	// fully retired (CTO 2026-06-12 — billing is per-workspace only).
	availableAuthEnv := availableAuthEnvNames(envVars)
	// molecule-core#1994: derive billing mode from the EFFECTIVE model, not the
	// raw payload.Model. On a re-provision (restart/resume/auto-restart) the
	// payload is rebuilt from the DB with Name+Tier+Runtime only — payload.Model
	// is "" (workspace_restart.go via withStoredCompute, which backfills Compute
	// but NOT Model). With an empty model DeriveProvider errors → the resolver
	// defaults closed to platform_managed and bakes the CP proxy, DIVERGING from
	// the read endpoint (which reads the stored MODEL workspace_secret and derives
	// byok). The stored model already lives in the merged envVars (loaded by
	// loadWorkspaceSecrets); resolve it with the SAME fallback chain
	// applyRuntimeModelEnv uses so the provision-path derive inputs match the
	// read-path's — keeping the two resolvers in parity (the #1994 regression
	// guard test asserts this).
	effectiveModel := effectiveModelForBilling(model, envVars)
	res, resolveErr := ResolveLLMBillingModeDerived(ctx, workspaceID, runtime, effectiveModel, availableAuthEnv)
	if resolveErr != nil {
		// resolveErr != nil ⇒ resolver hit a DB error AND already defaulted
		// res.ResolvedMode to platform_managed. Log + proceed; the safe default
		// is already in place, no early return needed.
		log.Printf("workspace_provision: resolve billing mode workspace=%s err=%v (defaulting to platform_managed)", workspaceID, resolveErr)
	}
	log.Printf("workspace_provision: billing mode workspace=%s resolved=%s source=%s derived_provider=%s", workspaceID, res.ResolvedMode, res.Source, derefOrEmpty(res.ProviderSelection))
	// internal#703: MOLECULE_LLM_BILLING_MODE in the container must reflect the
	// RESOLVED per-workspace mode, not a hardcoded literal. Pre-fix this var was
	// only emitted (hardcoded "platform_managed") on the strip path below, so a
	// byok/disabled container never carried a truthful billing-mode value — only
	// MOLECULE_LLM_BILLING_MODE_RESOLVED. Emit both here, resolver-driven, for
	// every mode so the value is correct on the byok/disabled early-return path
	// too (and downstream consumers / debug shells see byok, not platform_managed).
	envVars["MOLECULE_LLM_BILLING_MODE"] = res.ResolvedMode
	// Observability: surface the resolved mode in the container env so the
	// agent / debug shell can answer "why is my key being stripped" without
	// pulling logs or hitting the admin route.
	envVars["MOLECULE_LLM_BILLING_MODE_RESOLVED"] = res.ResolvedMode
	if res.ResolvedMode != LLMBillingModePlatformManaged {
		// byok or disabled — DO NOT force-route to CP, DO NOT override the
		// workspace's own ANTHROPIC_BASE_URL, and DO NOT strip the tenant's own
		// (provider-matching) LLM credentials.
		//
		// molecule-core#1994 (corrected model): `global_secrets` is the
		// TENANT's store, not the platform's. The tenant's own credential —
		// at global OR workspace scope — is exactly what byok runs on, direct.
		// The platform's own credential is never in a tenant's global_secrets
		// (guarded at the SetGlobal write boundary + the proxy token is
		// server-env-only), so leaving the tenant's globals in place cannot
		// re-open the platform-credit drain.
		//
		// internal#728 Bug 1 (provider-matched credential injection): #1994
		// removed the BLANKET strip, which was correct for the platform-key
		// co-mingling it targeted but left EVERY claude-code workspace
		// inheriting the tenant-global CLAUDE_CODE_OAUTH_TOKEN. A claude-code
		// runtime greedily prefers that oauth (`llm-auth: detected oauth` →
		// api.anthropic.com), so a workspace whose RESOLVED provider is NOT
		// anthropic-oauth (minimax, kimi-byok, …) routes its non-Anthropic
		// model to Anthropic and errors (`Claude Code returned an error
		// result`; DevB MiniMax-M2.7 live-confirmed 2026-05-28).
		//
		// The precise, provider-AWARE replacement for the over-removed strip:
		// keep ONLY the global-origin bypass creds whose env-var name is in the
		// RESOLVED provider's auth_env; strip the rest. This is NOT a return to
		// the blanket strip — it is keyed off the derived provider:
		//   - minimax (auth_env: MINIMAX_API_KEY, ANTHROPIC_AUTH_TOKEN,
		//     ANTHROPIC_API_KEY) → global-origin CLAUDE_CODE_OAUTH_TOKEN is
		//     NOT a match → stripped (fixes DevB).
		//   - anthropic-oauth (auth_env: CLAUDE_CODE_OAUTH_TOKEN) → the
		//     global-origin oauth IS a match → kept (PM/reno opus byok NOT
		//     regressed — the #1994 ByokGlobalScopeOAuthSurvives guard holds).
		// WORKSPACE-origin creds (the user explicitly set them via the canvas
		// Secrets tab → NOT in globalKeys) are NEVER stripped here, even when
		// they don't match: the user authored them deliberately (JRS kimi
		// workspace-key, reno's own oauth). Only the inherited operator-store
		// channel is provider-gated.
		stripNonMatchingGlobalOriginLLMCreds(envVars, globalKeys, runtime, effectiveModel, availableAuthEnv)

		// core#2709: claude-code's Anthropic SDK adapter reads ANTHROPIC_AUTH_TOKEN
		// and ANTHROPIC_BASE_URL. A BYOK MiniMax workspace arrives here with
		// MINIMAX_API_KEY but no Anthropic-shaped creds, so the adapter 401s after
		// restart. Project the provider's preferred auth token env and Anthropic
		// base URL from the workspace's available provider credential.
		if res.ResolvedMode == LLMBillingModeBYOK && runtimeUsesAnthropicNativeProxy(runtime) {
			providerName := derefOrEmpty(res.ProviderSelection)
			// core#2712: a per-workspace billing-mode override (source=workspace_override)
			// short-circuits ResolveLLMBillingModeDerived before it sets ProviderSelection,
			// but the Anthropic-adapter projection still needs to know WHICH BYOK
			// provider the workspace is running so it can map MINIMAX_API_KEY (etc.)
			// to ANTHROPIC_AUTH_TOKEN. Derive it from the effective model when missing.
			if providerName == "" && effectiveModel != "" {
				if manifest, mErr := providerRegistry(); mErr == nil && manifest != nil {
					if p, dErr := manifest.DeriveProvider(runtime, effectiveModel, availableAuthEnv); dErr == nil {
						providerName = p.Name
					}
				}
			}
			if provider, ok := providerFromRegistry(providerName); ok && provider.AuthTokenEnv != "" {
				if _, hasToken := envVars[provider.AuthTokenEnv]; !hasToken {
					for _, authEnv := range provider.AuthEnv {
						if v := strings.TrimSpace(envVars[authEnv]); v != "" {
							envVars[provider.AuthTokenEnv] = v
							break
						}
					}
				}
				if _, hasBase := envVars["ANTHROPIC_BASE_URL"]; !hasBase && provider.BaseURLAnthropic != "" {
					// core#2748: the registry base_url_anthropic is PROXY-shaped and
					// carries a trailing /v1 (providers.yaml follows the routing layer;
					// see the minimax "PR-5" reconciliation comment). But THIS is the
					// direct-BYOK adapter path: the claude-code Anthropic SDK appends
					// /v1/messages to ANTHROPIC_BASE_URL itself. Projecting the proxy
					// value verbatim yields a double /v1 (.../anthropic/v1/v1/messages)
					// -> upstream 404, surfaced as "selected model may not exist or no
					// access" (the #2748 engine outage; #2735 introduced this projection).
					// Strip a single trailing /v1 (and any trailing slash) so the SDK
					// re-append produces exactly one /v1. Correct for every anthropic-proto
					// provider: minimax .../anthropic/v1->.../anthropic, kimi-coding
					// .../coding/v1->.../coding, anthropic .../v1->root, moonshot likewise.
					adapterBase := strings.TrimSuffix(strings.TrimRight(provider.BaseURLAnthropic, "/"), "/v1")
					envVars["ANTHROPIC_BASE_URL"] = adapterBase
				}
			}
		}

		return platformLLMEnvResult{
			ResolvedMode:     res.ResolvedMode,
			HasUsableLLMCred: hasAnyPlatformManagedLLMKey(envVars),
			Source:           res.Source,
		}
	}
	baseURL := firstNonEmptyEnv("MOLECULE_LLM_BASE_URL", "OPENAI_BASE_URL")
	anthropicBaseURL := firstNonEmptyEnv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "ANTHROPIC_BASE_URL")
	token := firstNonEmptyEnv("MOLECULE_LLM_USAGE_TOKEN", "OPENAI_API_KEY")
	if baseURL == "" || token == "" {
		// Proxy not configured (boot race / misconfig). The platform_managed
		// path REQUIRES the CP proxy env to inject a usable credential.
		// Reporting HasUsableLLMCred=true here would start the workspace
		// credential-less — the adk-demo dark-wedge class (#2162).
		// Return false so the caller's fail-closed branch aborts with
		// MISSING_PLATFORM_PROXY.
		return platformLLMEnvResult{ResolvedMode: res.ResolvedMode, HasUsableLLMCred: false, Source: res.Source}
	}
	stripPlatformManagedLLMBypassEnv(envVars)

	// MOLECULE_LLM_BILLING_MODE is already set to res.ResolvedMode (==
	// platform_managed on this path) above (internal#703); no hardcode here.
	envVars["MOLECULE_LLM_BASE_URL"] = baseURL
	envVars["MOLECULE_LLM_USAGE_TOKEN"] = token
	if anthropicBaseURL != "" {
		envVars["MOLECULE_LLM_ANTHROPIC_BASE_URL"] = anthropicBaseURL
	}
	if usageURL := strings.TrimSpace(os.Getenv("MOLECULE_LLM_USAGE_URL")); usageURL != "" {
		envVars["MOLECULE_LLM_USAGE_URL"] = usageURL
	}

	if !runtimeUsesAnthropicNativeProxy(runtime) {
		envVars["OPENAI_API_KEY"] = token
		envVars["OPENAI_BASE_URL"] = baseURL
	}
	if runtimeUsesAnthropicNativeProxy(runtime) && anthropicBaseURL != "" {
		// core#2709: use the resolved provider's auth_token_env instead of
		// hardcoding ANTHROPIC_API_KEY. MiniMax's Anthropic-compatible endpoint
		// expects ANTHROPIC_AUTH_TOKEN, while the platform proxy surface expects
		// ANTHROPIC_API_KEY.
		anthropicTokenEnv := "ANTHROPIC_API_KEY"
		if provider, ok := providerFromRegistry(derefOrEmpty(res.ProviderSelection)); ok && provider.AuthTokenEnv != "" {
			anthropicTokenEnv = provider.AuthTokenEnv
		}
		envVars[anthropicTokenEnv] = token
		envVars["ANTHROPIC_BASE_URL"] = anthropicBaseURL
		// CP#752 WS1b: claude-code uses the Anthropic CLI/SDK's
		// ANTHROPIC_CUSTOM_HEADERS env var to attach per-workspace
		// attribution headers on every proxied LLM call. The CP proxy
		// (internal/handlers/llm_proxy.go resolveLLMProxyPrincipal)
		// verifies the workspace id against the org; mismatch → 401.
		envVars["ANTHROPIC_CUSTOM_HEADERS"] = fmt.Sprintf("X-Molecule-Workspace-Id: %s", workspaceID)
	}

	// core#2594: the MOLECULE_LLM_DEFAULT_MODEL env fail-open was REMOVED here.
	// It silently injected MOLECULE_MODEL when a workspace reached provision with
	// no resolvable model — an opaque, server-env-sourced substitution that hid
	// the missing model (the concierge ran on it; see the platform-agent
	// template's `model:` declaration in
	// molecule-ai-workspace-template-platform-agent/config.yaml).
	// Per the CTO SSOT directive (2026-05-22, models/runtime_defaults.go) the
	// platform must not default a workspace's model. Resolution is now stored-only
	// (create requires it; the concierge declares its own); a workspace that still
	// has no MOLECULE_MODEL/MODEL after all model-setting fails CLOSED at the
	// universal MISSING_MODEL gate in prepareProvisionContext rather than letting
	// the runtime pick its hardcoded anthropic:claude-opus-4-7 fallback.
	//
	// platform_managed: the CP proxy usage token (injected as ANTHROPIC_API_KEY
	// / OPENAI_API_KEY above) IS the usable credential, so the workspace is
	// never fail-closed on the CREDENTIAL axis on this path; the model axis is
	// gated separately below.
	return platformLLMEnvResult{ResolvedMode: res.ResolvedMode, HasUsableLLMCred: true, Source: res.Source}
}

func stripPlatformManagedLLMBypassEnv(envVars map[string]string) {
	for key := range platformManagedDirectLLMBypassKeys {
		delete(envVars, key)
	}
}

// hasAnyPlatformManagedLLMKey reports whether envVars carries at least one
// non-empty platform-managed-shaped LLM credential key (the tenant's own, at
// global or workspace scope). Used by the byok fail-closed branch: a byok
// workspace with no LLM credential at ANY scope must be aborted with
// MISSING_BYOK_CREDENTIAL rather than started credential-less.
func hasAnyPlatformManagedLLMKey(envVars map[string]string) bool {
	for key := range platformManagedDirectLLMBypassKeys {
		if strings.TrimSpace(envVars[key]) != "" {
			return true
		}
	}
	return false
}

// stripNonMatchingGlobalOriginLLMCreds is the byok-branch provider-matched
// credential injection (internal#728 Bug 1). It removes from envVars every
// platform-managed LLM bypass key that:
//
//  1. originated from the operator-controlled global_secrets store
//     (present in globalKeys — a workspace_secrets row of the same name
//     overrides + clears the flag, so user-authored creds are exempt), AND
//  2. is NOT in the RESOLVED provider's auth_env set.
//
// The motivating regression: #1994 dropped the blanket strip, so a claude-code
// workspace resolving to `minimax` still inherited the tenant-global
// CLAUDE_CODE_OAUTH_TOKEN; the runtime prefers that oauth and routes the
// MiniMax model to api.anthropic.com → error. Keeping only the resolved
// provider's own auth_env keys (minimax: MINIMAX_API_KEY/ANTHROPIC_AUTH_TOKEN/
// ANTHROPIC_API_KEY — not the oauth) removes the stray oauth while preserving
// anthropic-oauth's CLAUDE_CODE_OAUTH_TOKEN for an opus byok workspace.
//
// Fail-OPEN by design: if the provider cannot be derived (empty model /
// unknown runtime / ambiguous) or the registry is unavailable, we strip
// NOTHING — we never strip a credential we cannot prove is non-matching, so a
// derive miss can never fail-close a legitimate byok workspace (mirrors the
// resolver's own default-closed-to-platform contract: the worst case is we
// keep a stray cred, never that we remove the only usable one). The earlier
// internal#711 blanket strip's fail-direction (remove first) was the bug;
// this strip's fail-direction is keep-first.
func stripNonMatchingGlobalOriginLLMCreds(envVars map[string]string, globalKeys map[string]struct{}, runtime, model string, availableAuthEnv []string) {
	if len(globalKeys) == 0 {
		return // no operator-store-origin keys to consider — nothing to strip.
	}
	manifest, err := providerRegistry()
	if err != nil || manifest == nil {
		return // registry unavailable — fail open, strip nothing.
	}
	provider, dErr := manifest.DeriveProvider(runtime, model, availableAuthEnv)
	if dErr != nil {
		return // underivable provider — fail open, strip nothing.
	}
	// The resolved provider's accepted auth-env-var NAMES (case-insensitive
	// for parity with isPlatformManagedDirectLLMBypassKey, which upper-cases).
	keep := make(map[string]struct{}, len(provider.AuthEnv))
	for _, e := range provider.AuthEnv {
		keep[strings.ToUpper(strings.TrimSpace(e))] = struct{}{}
	}
	for key := range globalKeys {
		upper := strings.ToUpper(strings.TrimSpace(key))
		if _, isBypass := platformManagedDirectLLMBypassKeys[upper]; !isBypass {
			continue // not an LLM bypass cred (e.g. a non-LLM operator secret) — leave it.
		}
		if _, matches := keep[upper]; matches {
			continue // matches the resolved provider's auth_env — this is what byok runs on.
		}
		// Global-origin LLM bypass cred that does NOT match the resolved
		// provider — the stray that a greedy runtime would mis-prefer. Strip.
		if _, present := envVars[key]; present {
			log.Printf("workspace_provision: byok provider-matched strip — removing global-origin LLM cred %s (resolved provider=%s does not accept it)", key, provider.Name)
			delete(envVars, key)
		}
	}
}

func runtimeUsesAnthropicNativeProxy(runtime string) bool {
	return strings.EqualFold(strings.TrimSpace(runtime), "claude-code")
}

// providerFromRegistry looks up a provider by name in the cached embedded
// providers manifest. It returns the provider and true if found. Used by
// applyPlatformManagedLLMEnv to project the adapter-specific auth env / base
// URL (e.g. ANTHROPIC_AUTH_TOKEN for MiniMax on claude-code).
func providerFromRegistry(name string) (providers.Provider, bool) {
	if name == "" {
		return providers.Provider{}, false
	}
	manifest, err := providerRegistry()
	if err != nil || manifest == nil {
		return providers.Provider{}, false
	}
	for _, p := range manifest.Providers {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return providers.Provider{}, false
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// PlatformManagedProxyConfigured reports whether a Molecule LLM proxy is wired
// into THIS workspace-server process — i.e. whether the platform_managed billing
// path can actually inject a usable credential. It is the SAME precondition the
// strip gate enforces in applyPlatformManagedLLMEnv on the platform_managed
// branch: a proxy base URL (MOLECULE_LLM_BASE_URL / OPENAI_BASE_URL) AND a proxy
// usage token (MOLECULE_LLM_USAGE_TOKEN / OPENAI_API_KEY) must BOTH be present.
//
// On a SELF-HOSTED stack neither is set (there is no hosted Molecule proxy and
// no org credit ledger), so this returns false and platform_managed cannot work.
// The open GET /org/identity handler surfaces this as platform_managed_available
// so the canvas can hide the "Platform (proxy)" option and default to BYOK.
// On SaaS the CP provisioner exports both, so it returns true and the canvas
// behaves exactly as before.
func PlatformManagedProxyConfigured() bool {
	baseURL := firstNonEmptyEnv("MOLECULE_LLM_BASE_URL", "OPENAI_BASE_URL")
	token := firstNonEmptyEnv("MOLECULE_LLM_USAGE_TOKEN", "OPENAI_API_KEY")
	return baseURL != "" && token != ""
}

// loadWorkspaceSecrets loads global + workspace-specific secrets into a map.
// Returns nil map + error string on decrypt failure. Shared by both Docker
// and control plane provisioning paths to avoid duplication.
//
// The second return value (globalKeys) records which keys originated from
// the operator-controlled `global_secrets` table — used by RFC#523 Layer 1
// to constrain its forbidden-key check to the operator-bleed channel,
// instead of blanket-blocking by name across BOTH provenance channels (the
// over-fire that breaks the legitimate user flow of pasting their own
// GitHub PAT into the canvas Secrets tab → workspace_secrets row). See
// `feedback_upstream_docs_first_before_hypothesizing`: RFC#523's threat
// model (issue molecule-ai/internal#523 §"Threat model") names operator-
// scope tokens being injected via provision-time env / operator-side
// stores — NOT the user's own scoped PAT they explicitly authorized via
// the per-workspace Secrets tab.
//
// The third return value (workspaceKeys) is the POSITIVE counterpart: the
// set of keys authored via the per-workspace `workspace_secrets` table
// (user / org-admin set, authenticated as the workspace owner). It is the
// provenance signal the forensic #145 SCM-write-token guard consults to
// EXEMPT a workspace-scoped GITEA_TOKEN (the intended, legitimate delivery
// channel for a reviewer agent) from the operator-bleed strip. A key set
// in BOTH stores lands here (workspace overrides global) and is removed
// from globalKeys, matching the precedence semantic below.
//
// The merged map preserves the existing precedence semantic (workspace
// rows overwrite global rows on key collision); only the provenance side-
// channels are new. Existing callers can ignore globalKeys / workspaceKeys.
//
// F1086 / #1206: the returned error string is the SAFE-CANNED message that
// gets persisted to workspaces.last_sample_error AND broadcast as the
// WORKSPACE_PROVISION_FAILED payload. Internal detail (the secret key name,
// the encryption version, the decrypt-error text) is logged here, never
// returned to the caller, so it can't leak via the canvas event stream
// (cf. TestProvisionWorkspace_NoInternalErrorsInBroadcast).
func loadWorkspaceSecrets(ctx context.Context, workspaceID string) (map[string]string, map[string]struct{}, map[string]struct{}, string) {
	envVars := map[string]string{}
	globalKeys := map[string]struct{}{}
	workspaceKeys := map[string]struct{}{}
	globalRows, globalErr := db.DB.QueryContext(ctx,
		`SELECT key, encrypted_value, encryption_version FROM global_secrets`)
	if globalErr == nil {
		defer globalRows.Close()
		for globalRows.Next() {
			var k string
			var v []byte
			var ver int
			if globalRows.Scan(&k, &v, &ver) == nil {
				// internal#718 P4 closure: LLM_PROVIDER is retired even
				// at the global rung. The same provider-from-(runtime,model)
				// derivation runs per-workspace, so a global default
				// would be pure ghost. Symmetric with the workspace_secrets
				// drop below.
				if k == "LLM_PROVIDER" {
					continue
				}
				decrypted, decErr := crypto.DecryptVersioned(v, ver)
				if decErr != nil {
					log.Printf("Provisioner: FATAL — failed to decrypt global secret %s (version=%d): %v — aborting provision of workspace %s", k, ver, decErr, workspaceID)
					return nil, nil, nil, "failed to decrypt global secret"
				}
				envVars[k] = string(decrypted)
				globalKeys[k] = struct{}{}
			}
		}
		if err := globalRows.Err(); err != nil {
			log.Printf("Provisioner: global_secrets rows.Err workspace=%s: %v", workspaceID, err)
		}
	}
	wsRows, err := db.DB.QueryContext(ctx,
		`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1`, workspaceID)
	if err == nil {
		defer wsRows.Close()
		for wsRows.Next() {
			var k string
			var v []byte
			var ver int
			if wsRows.Scan(&k, &v, &ver) == nil {
				// internal#718 P4 closure: LLM_PROVIDER is a retired
				// secret key. Migration 20260528000000 deletes any
				// straggler rows; this drop is defence-in-depth so a
				// rolling deploy (new code, old DB) never re-emits the
				// retired key into the provisioner env (which would
				// reach the CP-side resolveModelAndProvider — now
				// itself retired, but the env contract belongs to
				// core). Idempotent: a fresh tenant has zero
				// LLM_PROVIDER rows and this branch is unreached.
				if k == "LLM_PROVIDER" {
					continue
				}
				decrypted, decErr := crypto.DecryptVersioned(v, ver)
				if decErr != nil {
					log.Printf("Provisioner: FATAL — failed to decrypt workspace secret %s (version=%d) for %s: %v — aborting provision", k, ver, workspaceID, decErr)
					return nil, nil, nil, "failed to decrypt workspace secret"
				}
				envVars[k] = string(decrypted)
				// User-authored workspace_secrets value supersedes any
				// global_secrets row of the same key — including dropping
				// the operator-bleed provenance flag. The user explicitly
				// re-set the value via the canvas Secrets tab, so it is
				// no longer "the operator-store version."
				delete(globalKeys, k)
				// Positive provenance: record that this key was authored
				// via workspace_secrets. The forensic #145 SCM-write-token
				// guard exempts only keys in this set — a workspace-scoped
				// GITEA_TOKEN is the intended delivery channel for that
				// workspace's agent.
				workspaceKeys[k] = struct{}{}
			}
		}
		if err := wsRows.Err(); err != nil {
			log.Printf("Provisioner: workspace_secrets rows.Err workspace=%s: %v", workspaceID, err)
		}
	}
	return envVars, globalKeys, workspaceKeys, ""
}

// provisionWorkspaceCP provisions a workspace via the control plane API.
//
// Mode-specific work this function owns: cpProv.Start (delegates EC2
// launch to control plane) + persist instance_id in DB. The shared
// setup (secrets, env mutators, mint of auth_token + inbound_secret)
// lives in prepareProvisionContext + mintWorkspaceSecrets and is
// called by both this function and the Docker-mode counterpart.
//
// Pre-#2026-04-30: this function did NOT call mintWorkspaceSecrets.
// That left every prod workspace with a NULL platform_inbound_secret
// column → 503 on every chat upload (RFC #2312). The bug shipped
// because the Docker and SaaS provision paths had drifted: Docker
// got the mint when #2312 landed; SaaS was missed. Refactored to
// share so the next mint added can't be silently forgotten on one
// side.
func (h *WorkspaceHandler) provisionWorkspaceCP(workspaceID, templatePath string, configFiles map[string][]byte, payload models.CreateWorkspacePayload) {
	// Entry log + panic recovery — see provisionWorkspaceOpts for rationale.
	// Issue #2486: 7 claude-code workspaces stuck in provisioning produced
	// none of the four documented exit-path log lines, leaving operators
	// unable to distinguish "goroutine never started" from "started but
	// returned via an unlogged path."
	log.Printf("CPProvisioner: goroutine entered for %s (runtime=%s, mode=cp)", workspaceID, payload.Runtime)
	defer h.logProvisionPanic(workspaceID, "cp")

	ctx, cancel := context.WithTimeout(context.Background(), provisioner.ProvisionTimeout)
	defer cancel()

	prepared, abort := h.prepareProvisionContext(ctx, workspaceID, templatePath, configFiles, payload, false)
	if prepared == nil {
		log.Printf("CPProvisioner: prepare failed for %s: %s", workspaceID, abort.Msg)
		h.markProvisionFailed(ctx, workspaceID, abort.Msg, abort.Extra)
		return
	}

	// Mint the workspace's auth_token + platform_inbound_secret now,
	// before cpProv.Start. Both modes write to the DB column; the
	// workspace receives the plaintext via /registry/register response
	// (registry.go:344-362) on its first heartbeat after boot.
	h.mintWorkspaceSecrets(ctx, workspaceID, &prepared.Config)

	machineID, err := h.cpProv.Start(ctx, prepared.Config)
	if err != nil {
		// F1086 / #1206: CP errors can include machine type, AMI IDs, VPC
		// paths — use generic message for broadcast and last_sample_error.
		log.Printf("CPProvisioner: workspace start failed for %s: %v", workspaceID, err)
		h.markProvisionFailed(ctx, workspaceID, "provisioning failed", nil)
		return
	}

	// Persist the backing instance id so later operations (terminal via
	// EIC+SSH, live logs, debug introspection) can resolve workspace → EC2
	// without re-asking CP on every request.
	//
	// Bounded retry with exponential backoff: a transient DB blip must not
	// orphan a healthy running instance. If all retries fail, mark the
	// workspace failed and record the instance_id in the broadcast event +
	// last_sample_error so an operator/reaper can reconcile later. The live
	// EC2 is NOT terminated — it may contain valuable state. (#1)
	var persistErr error
	delay := instanceIDPersistRetryBaseDelay
	for attempt := 1; attempt <= instanceIDPersistRetryAttempts; attempt++ {
		_, persistErr = db.DB.ExecContext(ctx,
			`UPDATE workspaces SET instance_id = $2, updated_at = now() WHERE id = $1`,
			workspaceID, machineID)
		if persistErr == nil {
			if attempt > 1 {
				log.Printf("CPProvisioner: instance_id persist for %s succeeded on attempt %d", workspaceID, attempt)
			}
			break
		}
		if attempt < instanceIDPersistRetryAttempts {
			time.Sleep(delay)
			delay *= 2
		}
	}
	if persistErr != nil {
		log.Printf("CPProvisioner: CRITICAL persist instance_id failed for %s after %d attempts: %v — EC2 instance %s is RUNNING but UNTRACKED. Operator must manually reconcile or remove the workspace to trigger orphan cleanup.", workspaceID, instanceIDPersistRetryAttempts, persistErr, machineID)
		// Server-only log already captures the raw error above; broadcast gets
		// safe fields only (no client-visible DB error). Security: RC 9378.
		h.markProvisionFailed(ctx, workspaceID, "instance_id persist failed after retry — EC2 untracked", map[string]interface{}{
			"instance_id": machineID,
			"attempts":    instanceIDPersistRetryAttempts,
		})
		return
	}

	log.Printf("CPProvisioner: workspace %s started as machine %s via control plane", workspaceID, machineID)
}
