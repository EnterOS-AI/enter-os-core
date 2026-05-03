package handlers

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/crypto"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
)

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
						cfg = h.buildProvisionerConfig(ctx, workspaceID, templatePath, configFiles, payload, prepared.EnvVars, prepared.PluginsPath, prepared.AwarenessNamespace)
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
		// Containers on molecule-monorepo-net can reach each other by container name.
		internalURL := provisioner.InternalURL(workspaceID)
		if cacheErr := db.CacheInternalURL(ctx, workspaceID, internalURL); cacheErr != nil {
			log.Printf("Provisioner: failed to cache internal URL for %s: %v", workspaceID, cacheErr)
		}
	}
	// On success, the workspace will register via POST /registry/register
	// which transitions status to 'online' and broadcasts WORKSPACE_ONLINE
}

// seedInitialMemories inserts a list of MemorySeed entries into agent_memories
// for the given workspace. Called during workspace creation and org import to
// pre-populate memories from config/template. Non-fatal: each insert is
// attempted independently and failures are logged. Issue #1050.
// maxMemoryContentLength is the maximum allowed size for a single memory content
// field. Content exceeding this limit is truncated to prevent storage exhaustion
// (CWE-400) and OOM on read paths. The limit is intentionally generous — it fits
// a ~64k context window worth of text — but small enough to prevent abuse.
const maxMemoryContentLength = 100_000 // ~100 KiB of text

func seedInitialMemories(ctx context.Context, workspaceID string, memories []models.MemorySeed, awarenessNamespace string) {
	if len(memories) == 0 {
		return
	}
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
		// #1066: enforce content length limit to prevent storage exhaustion (CWE-400).
		// Truncate oversized content rather than rejecting the whole insert so that
		// template authors get a predictable fallback rather than a silent skip.
		content := mem.Content
		if len(content) > maxMemoryContentLength {
			content = content[:maxMemoryContentLength]
			log.Printf("seedInitialMemories: truncated memory content for %s (scope=%s) from %d to %d bytes",
				workspaceID, scope, len(mem.Content), maxMemoryContentLength)
		}
		redactedContent, _ := redactSecrets(workspaceID, content)
		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO agent_memories (workspace_id, content, scope, namespace)
			VALUES ($1, $2, $3, $4)
		`, workspaceID, redactedContent, scope, awarenessNamespace); err != nil {
			log.Printf("seedInitialMemories: failed to insert memory for %s (scope=%s): %v", workspaceID, scope, err)
		}
	}
	log.Printf("seedInitialMemories: seeded %d memories for workspace %s", len(memories), workspaceID)
}

func workspaceAwarenessNamespace(workspaceID string) string {
	return fmt.Sprintf("workspace:%s", workspaceID)
}

func (h *WorkspaceHandler) loadAwarenessNamespace(ctx context.Context, workspaceID string) string {
	var awarenessNamespace string
	err := db.DB.QueryRowContext(ctx, `SELECT COALESCE(awareness_namespace, '') FROM workspaces WHERE id = $1`, workspaceID).Scan(&awarenessNamespace)
	if err != nil || awarenessNamespace == "" {
		return workspaceAwarenessNamespace(workspaceID)
	}
	return awarenessNamespace
}

func (h *WorkspaceHandler) buildProvisionerConfig(
	ctx context.Context,
	workspaceID, templatePath string,
	configFiles map[string][]byte,
	payload models.CreateWorkspacePayload,
	envVars map[string]string,
	pluginsPath, awarenessNamespace string,
) provisioner.WorkspaceConfig {
	// Per-workspace workspace_dir takes priority over global WORKSPACE_DIR env var.
	// If neither is set, the provisioner creates an isolated Docker volume.
	//
	// #65: also read workspace_access (DB column) so restart paths preserve
	// the mode set at create/import time. Payload's WorkspaceAccess (if
	// present) wins, matching the existing WorkspaceDir precedence.
	workspacePath := payload.WorkspaceDir
	workspaceAccess := payload.WorkspaceAccess
	if workspacePath == "" || workspaceAccess == "" {
		var dbDir, dbAccess string
		if err := db.DB.QueryRow(
			`SELECT COALESCE(workspace_dir, ''), COALESCE(workspace_access, 'none') FROM workspaces WHERE id = $1`,
			workspaceID,
		).Scan(&dbDir, &dbAccess); err == nil {
			if workspacePath == "" && dbDir != "" {
				workspacePath = dbDir
			}
			if workspaceAccess == "" {
				workspaceAccess = dbAccess
			}
		}
	}
	if workspacePath == "" {
		workspacePath = os.Getenv("WORKSPACE_DIR")
	}
	if workspaceAccess == "" {
		workspaceAccess = provisioner.WorkspaceAccessNone
	}

	return provisioner.WorkspaceConfig{
		WorkspaceID:        workspaceID,
		TemplatePath:       templatePath,
		ConfigFiles:        configFiles,
		PluginsPath:        pluginsPath,
		WorkspacePath:      workspacePath,
		WorkspaceAccess:    workspaceAccess,
		Tier:               payload.Tier,
		Runtime:            payload.Runtime,
		EnvVars:            envVars,
		PlatformURL:        h.platformURL,
		AwarenessURL:       os.Getenv("AWARENESS_URL"),
		AwarenessNamespace: awarenessNamespace,
		Image:              resolveRuntimeImage(ctx, payload.Runtime),
	}
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
	// Revoke any existing live tokens FIRST — this must run in both modes.
	// In SaaS mode the revoke is load-bearing on re-provision: without it,
	// the previous workspace instance's live token sits in the DB, and
	// RegistryHandler.requireWorkspaceToken on the fresh instance's first
	// /registry/register would reject it (live token exists → no
	// bootstrap allowance, but the new workspace has no plaintext because
	// the CP provisioner doesn't carry cfg.ConfigFiles across user-data).
	// Revoking clears the gate so the register handler's bootstrap path
	// can mint a fresh token and return the plaintext in the response.
	if err := wsauth.RevokeAllForWorkspace(ctx, db.DB, workspaceID); err != nil {
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

	if cfg.ConfigFiles == nil {
		cfg.ConfigFiles = make(map[string][]byte)
	}
	cfg.ConfigFiles[".auth_token"] = []byte(token)
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

	if cfg.ConfigFiles == nil {
		cfg.ConfigFiles = make(map[string][]byte)
	}
	cfg.ConfigFiles[".platform_inbound_secret"] = []byte(secret)
	log.Printf("Provisioner: injected platform_inbound_secret for workspace %s into config volume", workspaceID)
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
// accept. Unknown values are coerced to the default ("langgraph") instead
// of being splatted into filepath.Join + config.yaml templating, which
// closes both the YAML-injection vector (#241) where an attacker could
// smuggle `initial_prompt: run id && curl …` through a crafted runtime
// string, and the path-traversal oracle where `runtime: ../../sensitive`
// probed host directories for existence.
//
// Keep in sync with workspace/build-all.sh — adding a new
// runtime means bumping both this list and the Docker image tags.
// knownRuntimes is populated from manifest.json at service init (see
// runtime_registry.go). The package init order is:
//   1. var knownRuntimes = fallbackRuntimes
//   2. init() calls initKnownRuntimes() which replaces it if
//      manifest.json is readable.
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
func (h *WorkspaceHandler) ensureDefaultConfig(workspaceID string, payload models.CreateWorkspacePayload) map[string][]byte {
	files := make(map[string][]byte)

	// Determine runtime — pass through the allowlist so an attacker
	// can't smuggle `initial_prompt: …` or a path-traversal oracle
	// via a crafted runtime string (#241).
	runtime := sanitizeRuntime(payload.Runtime)

	// Generate a minimal config.yaml
	model := payload.Model
	if model == "" {
		if runtime == "claude-code" {
			model = "sonnet"
		} else {
			model = "anthropic:claude-opus-4-7"
		}
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

	// Model always at top level — config.py reads raw["model"] for all runtimes.
	configYAML += fmt.Sprintf("model: %s\n", quoteModel)

	// Add runtime_config. required_env is intentionally omitted — the
	// platform injects secrets at container-start time via the secrets API,
	// and preflight already validates that the env vars are present before
	// the agent loop starts.  Hardcoding token names here caused #1028
	// (expired CLAUDE_CODE_OAUTH_TOKEN baked into config.yaml).
	configYAML += "runtime_config:\n  timeout: 0\n"

	files["config.yaml"] = []byte(configYAML)

	log.Printf("Provisioner: generated %d config files for workspace %s (runtime: %s)", len(files), workspaceID, runtime)
	return files
}

// deriveProviderFromModelSlug maps a hermes-agent model slug prefix to
// its provider name — a Go translation of the case statement in
// workspace-configs-templates/hermes/scripts/derive-provider.sh that we
// can run at provision time so LLM_PROVIDER lands in workspace_secrets
// (and from there, into /configs/config.yaml via CP user-data) before
// the container ever boots.
//
// Returns "" when the prefix isn't recognized OR when the runtime-only
// override would be needed to pick a provider — the caller skips the
// LLM_PROVIDER write in that case so derive-provider.sh keeps the final
// say at boot. derive-provider.sh remains the source of truth: this is
// strictly a *gating* hint that survives restarts and gives CP a YAML
// field to populate. Without it, "Save+Restart" would lose the user's
// provider choice every time CP regenerates the config.
//
// Two intentional differences from the shell version:
//
//  1. nousresearch/* and openai/* both return "openrouter" here. The
//     shell script special-cases "prefer nous if HERMES_API_KEY set" /
//     "prefer custom if OPENAI_API_KEY set", but those depend on
//     runtime env that may not yet be loaded at provision time. We pick
//     the safe default ("openrouter" reaches both Hermes 3 and OpenAI
//     models without extra config); derive-provider.sh's runtime check
//     can still upgrade to nous/custom when the keys are present.
//
//  2. Unknown prefixes return "" instead of "auto". Persisting "auto"
//     would block a future "Save+Restart" with a known prefix from
//     re-deriving — the CP YAML field is sticky once written. Returning
//     "" means the caller skips the write and the runtime falls through
//     to derive-provider.sh's *=auto branch on its own.
//
// Cover the same prefix list as derive-provider.sh's case statement;
// keep both files in sync when a new provider is added (table-driven
// test in workspace_provision_shared_test.go pins the mapping).
func deriveProviderFromModelSlug(model string) string {
	if model == "" {
		return ""
	}
	idx := strings.Index(model, "/")
	if idx <= 0 {
		return ""
	}
	prefix := model[:idx]
	switch prefix {
	// Direct-SDK providers (clean 1:1 prefix→provider mapping).
	case "minimax":
		return "minimax"
	case "minimax-cn":
		return "minimax-cn"
	case "anthropic":
		return "anthropic"
	case "gemini":
		return "gemini"
	case "deepseek":
		return "deepseek"
	case "zai":
		return "zai"
	case "kimi-coding":
		return "kimi-coding"
	case "kimi-coding-cn":
		return "kimi-coding-cn"
	case "alibaba", "dashscope", "qwen":
		return "alibaba"
	case "xiaomi", "mimo":
		return "xiaomi"
	case "arcee", "arcee-ai":
		return "arcee"
	case "nvidia", "nim":
		return "nvidia"
	case "ollama-cloud":
		return "ollama-cloud"
	case "huggingface", "hf":
		return "huggingface"
	case "ai-gateway", "aigateway":
		return "ai-gateway"
	case "kilocode":
		return "kilocode"
	case "opencode-zen":
		return "opencode-zen"
	case "opencode-go":
		return "opencode-go"
	// Aggregator + explicit catch-alls.
	case "openrouter":
		return "openrouter"
	case "custom":
		return "custom"
	// Runtime-only override candidates. derive-provider.sh's
	// HERMES_API_KEY / OPENAI_API_KEY checks happen at boot; we pick the
	// safe default (openrouter reaches both Hermes 3 and OpenAI without
	// extra config) and let the script upgrade to nous/custom at runtime.
	case "nousresearch", "openai":
		return "openrouter"
	// Additional 1:1 prefix→provider mappings — kept aligned with upstream's
	// HERMES_INFERENCE_PROVIDER list (NousResearch/hermes-agent v0.12.0,
	// 2026-04-30) and the additional case clauses in derive-provider.sh.
	// The drift gate in derive_provider_drift_test.go enforces parity.
	case "xai", "grok":
		return "xai"
	case "bedrock", "aws":
		return "bedrock"
	case "tencent", "tencent-tokenhub":
		return "tencent-tokenhub"
	case "gmi":
		return "gmi"
	case "qwen-oauth":
		return "qwen-oauth"
	case "lmstudio", "lm-studio":
		return "lmstudio"
	case "minimax-oauth":
		return "minimax-oauth"
	case "alibaba-coding-plan":
		return "alibaba-coding-plan"
	case "google-gemini-cli":
		return "google-gemini-cli"
	case "openai-codex":
		return "openai-codex"
	case "copilot-acp":
		return "copilot-acp"
	case "copilot":
		return "copilot"
	}
	// Unknown prefix → don't persist a guess. derive-provider.sh's
	// *=auto fallback handles it at runtime.
	return ""
}

// applyRuntimeModelEnv exposes the workspace's selected model via an
// env var the target runtime's install.sh / start.sh knows to read.
// Each runtime owns its own env-var contract — the tenant just plumbs
// the value through so CP can bake it into user-data.
//
// Why per-runtime rather than a generic MOLECULE_MODEL: each runtime
// installer has its own config schema and naming (hermes writes to
// ~/.hermes/config.yaml with `model.default`; langgraph reads from
// /configs/config.yaml directly; future IoT/robotics targets may have
// firmware manifests). Keeping the contract owned by the runtime
// template means adding a new runtime doesn't require edits on the
// tenant side for each one.
//
// For runtimes with no env-based model override (langgraph etc. read
// model from /configs/config.yaml which CP user-data generates from
// payload.Model at boot), this is a no-op — no harm in the switch
// being empty for those cases.
func applyRuntimeModelEnv(envVars map[string]string, runtime, model string) {
	// Fall back to the MODEL_PROVIDER workspace secret when the caller
	// didn't pass one explicitly. This is the path that "Save+Restart"
	// hits — Restart builds its payload from the workspaces row (no model
	// column there) so payload.Model is always empty, but the user's
	// canvas selection was stored as MODEL_PROVIDER via PUT /model and
	// is already loaded into envVars here. Without this fallback hermes
	// silently boots with the template default and errors "No LLM
	// provider configured" even though the user picked a valid model.
	if model == "" {
		model = envVars["MODEL_PROVIDER"]
	}
	if model == "" {
		return
	}

	// Universal MODEL env var — every adapter that wants to honour the
	// canvas-picked model (instead of its template's default) reads this.
	// molecule-runtime's workspace/config.py already falls back to MODEL
	// for runtime_config.model (#194). Without this line, the user's
	// canvas selection is silently dropped on every templated provision —
	// confirmed via crash-loop diagnosis on 2026-05-02 where MiniMax
	// picks booted with model=sonnet (template default) and demanded
	// CLAUDE_CODE_OAUTH_TOKEN. Set it FIRST so the per-runtime branches
	// below can still layer on additional vendor-specific names without
	// fighting over the canonical one.
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

// loadWorkspaceSecrets loads global + workspace-specific secrets into a map.
// Returns nil map + error string on decrypt failure. Shared by both Docker
// and control plane provisioning paths to avoid duplication.
//
// F1086 / #1206: the returned error string is the SAFE-CANNED message that
// gets persisted to workspaces.last_sample_error AND broadcast as the
// WORKSPACE_PROVISION_FAILED payload. Internal detail (the secret key name,
// the encryption version, the decrypt-error text) is logged here, never
// returned to the caller, so it can't leak via the canvas event stream
// (cf. TestProvisionWorkspace_NoInternalErrorsInBroadcast).
func loadWorkspaceSecrets(ctx context.Context, workspaceID string) (map[string]string, string) {
	envVars := map[string]string{}
	globalRows, globalErr := db.DB.QueryContext(ctx,
		`SELECT key, encrypted_value, encryption_version FROM global_secrets`)
	if globalErr == nil {
		defer globalRows.Close()
		for globalRows.Next() {
			var k string
			var v []byte
			var ver int
			if globalRows.Scan(&k, &v, &ver) == nil {
				decrypted, decErr := crypto.DecryptVersioned(v, ver)
				if decErr != nil {
					log.Printf("Provisioner: FATAL — failed to decrypt global secret %s (version=%d): %v — aborting provision of workspace %s", k, ver, decErr, workspaceID)
					return nil, "failed to decrypt global secret"
				}
				envVars[k] = string(decrypted)
			}
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
				decrypted, decErr := crypto.DecryptVersioned(v, ver)
				if decErr != nil {
					log.Printf("Provisioner: FATAL — failed to decrypt workspace secret %s (version=%d) for %s: %v — aborting provision", k, ver, workspaceID, decErr)
					return nil, "failed to decrypt workspace secret"
				}
				envVars[k] = string(decrypted)
			}
		}
	}
	return envVars, ""
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
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET instance_id = $2, updated_at = now() WHERE id = $1`,
		workspaceID, machineID); err != nil {
		// Non-fatal: provisioning succeeded, the workspace will still run.
		// The row stays without instance_id — terminal falls back to the
		// "CP-provisioned but unreachable" error, not a silent failure.
		log.Printf("CPProvisioner: persist instance_id failed for %s: %v", workspaceID, err)
	}

	log.Printf("CPProvisioner: workspace %s started as machine %s via control plane", workspaceID, machineID)
}
