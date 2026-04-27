package handlers

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/crypto"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
)

// provisionWorkspace handles async container deployment with timeout.
func (h *WorkspaceHandler) provisionWorkspace(workspaceID, templatePath string, configFiles map[string][]byte, payload models.CreateWorkspacePayload) {
	h.provisionWorkspaceOpts(workspaceID, templatePath, configFiles, payload, false)
}

// provisionWorkspaceOpts is the workhorse variant of provisionWorkspace that
// accepts extra per-invocation knobs (e.g. resetClaudeSession for issue #12)
// that should NOT be persisted on CreateWorkspacePayload because they're
// request-scoped flags.
func (h *WorkspaceHandler) provisionWorkspaceOpts(workspaceID, templatePath string, configFiles map[string][]byte, payload models.CreateWorkspacePayload, resetClaudeSession bool) {
	ctx, cancel := context.WithTimeout(context.Background(), provisioner.ProvisionTimeout)
	defer cancel()

	// Load global secrets first, then workspace-specific secrets (which override globals).
	envVars := map[string]string{}

	// 1. Global secrets (platform-wide defaults). Uses DecryptVersioned
	// so plaintext rows written before encryption was enabled (#85)
	// keep working. A decrypt failure aborts provisioning — silent skip
	// used to manifest as opaque "missing OAuth token" preflight crashes.
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
					h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_PROVISION_FAILED", workspaceID, map[string]interface{}{
						"error": "failed to decrypt global secret",
					})
					db.DB.ExecContext(ctx, `UPDATE workspaces SET status = 'failed', updated_at = now() WHERE id = $1`, workspaceID)
					return
				}
				envVars[k] = string(decrypted)
			}
		}
	}

	// 2. Workspace-specific secrets (override globals with same key)
	rows, err := db.DB.QueryContext(ctx,
		`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1`, workspaceID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var k string
			var v []byte
			var ver int
			if rows.Scan(&k, &v, &ver) == nil {
				decrypted, decErr := crypto.DecryptVersioned(v, ver)
				if decErr != nil {
					log.Printf("Provisioner: FATAL — failed to decrypt workspace secret %s (version=%d) for %s: %v — aborting provision", k, ver, workspaceID, decErr)
					h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_PROVISION_FAILED", workspaceID, map[string]interface{}{
						"error": "failed to decrypt workspace secret",
					})
					db.DB.ExecContext(ctx, `UPDATE workspaces SET status = 'failed', updated_at = now() WHERE id = $1`, workspaceID)
					return
				}
				envVars[k] = string(decrypted)
			}
		}
	}

	pluginsPath, _ := filepath.Abs(filepath.Join(h.configsDir, "..", "plugins"))
	awarenessNamespace := h.loadAwarenessNamespace(ctx, workspaceID)

	// Per-agent git identity (Option 3 of agent-separation rollout).
	// Sets GIT_AUTHOR_* / GIT_COMMITTER_* so commits from each workspace
	// carry a distinct author in `git log` / `git blame` — instead of
	// every agent appearing as whoever the shared PAT belongs to. PR +
	// issue authorship is still tied to GITHUB_TOKEN (shared PAT); that
	// gets solved by the GitHub App migration (Option 1, follow-up PR).
	// Runs after secret loads so an operator can still override via a
	// workspace_secret named GIT_AUTHOR_NAME if they want custom identity.
	applyAgentGitIdentity(envVars, payload.Name)
	applyRuntimeModelEnv(envVars, payload.Runtime, payload.Model)

	// Propagate the workspace's role into env so role-aware plugins
	// (gh-identity — molecule-core#1957) can read it without the
	// plugin interface having to carry the full payload. Role is
	// cosmetic metadata — no auth weight on it — safe to surface as env.
	if payload.Role != "" {
		envVars["MOLECULE_AGENT_ROLE"] = payload.Role
	}

	// Plugin extension point: run any registered EnvMutators (e.g.
	// github-app-auth, vault-secrets) AFTER built-in identity injection so
	// plugins can override or augment GIT_AUTHOR_*, GITHUB_TOKEN, etc.
	// A failure here aborts provisioning — a missing GitHub App token
	// would manifest later as opaque "git push 401" loops, and the agent
	// never recovers. Failing fast here surfaces the cause to the operator.
	if err := h.envMutators.Run(ctx, workspaceID, envVars); err != nil {
		log.Printf("Provisioner: env mutator chain failed for %s: %v", workspaceID, err)
		// F1086 / #1206: broadcast and db last_sample_error use generic messages —
		// env mutator errors (missing tokens, vault paths, etc.) can include
		// internal credential URIs and file paths that must not reach the caller.
		h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_PROVISION_FAILED", workspaceID, map[string]interface{}{
			"error": "plugin env mutator chain failed",
		})
		if _, dbErr := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET status = 'failed', last_sample_error = $2, updated_at = now() WHERE id = $1`,
			workspaceID, "plugin env mutator chain failed"); dbErr != nil {

			log.Printf("Provisioner: failed to mark workspace %s as failed after mutator error: %v", workspaceID, dbErr)
		}
		return
	}

	// Preflight: refuse to launch when config.yaml declares required env vars
	// that are not set. Without this, a missing CLAUDE_CODE_OAUTH_TOKEN (or
	// similar) crashes the in-container preflight, the container never calls
	// /registry/register, and the workspace sits in `provisioning` until a
	// sweeper flips it or the user retries. Failing fast here gives the user
	// an immediate, actionable error in the Events tab.
	if missing := missingRequiredEnv(configFiles, envVars); len(missing) > 0 {
		msg := formatMissingEnvError(missing)
		log.Printf("Provisioner: %s (workspace=%s)", msg, workspaceID)
		if _, dbErr := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET status = 'failed', last_sample_error = $2, updated_at = now() WHERE id = $1`,
			workspaceID, msg); dbErr != nil {
			log.Printf("Provisioner: failed to mark workspace %s as failed: %v", workspaceID, dbErr)
		}
		h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_PROVISION_FAILED", workspaceID, map[string]interface{}{
			"error":   msg,
			"missing": missing,
		})
		return
	}

	cfg := h.buildProvisionerConfig(workspaceID, templatePath, configFiles, payload, envVars, pluginsPath, awarenessNamespace)
	cfg.ResetClaudeSession = resetClaudeSession // #12

	// Preflight #17: detect + auto-recover the "empty config volume" crashloop.
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
						cfg = h.buildProvisionerConfig(workspaceID, templatePath, configFiles, payload, envVars, pluginsPath, awarenessNamespace)
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
				if _, dbErr := db.DB.ExecContext(ctx,
					`UPDATE workspaces SET status = 'failed', last_sample_error = $2, updated_at = now() WHERE id = $1`,
					workspaceID, msg); dbErr != nil {
					log.Printf("Provisioner: failed to mark workspace %s as failed: %v", workspaceID, dbErr)
				}
				h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_PROVISION_FAILED", workspaceID, map[string]interface{}{
					"error": msg,
				})
				return
			}
		}
	}

	// Issue/rotate the workspace auth token and inject the plaintext into the
	// config volume so the workspace always has a valid bearer credential on
	// disk, even after a container rebuild wiped the volume (issue #418).
	//
	// We must rotate (revoke-then-issue) rather than reuse because the DB only
	// stores sha256(plaintext) — we cannot reconstruct the original token to
	// write it back. The new plaintext is written into /configs/.auth_token via
	// WriteFilesToContainer, which runs immediately after ContainerStart and
	// wins the race against the Python adapter's startup time (~1-2 s).
	h.issueAndInjectToken(ctx, workspaceID, &cfg)

	url, err := h.provisioner.Start(ctx, cfg)
	if err != nil {
		// F1086 / #1206: persist a generic message so the canvas and
		// GET /workspaces/:id expose something actionable without leaking
		// docker/error internals (image pull messages, volume paths, etc.).
		errMsg := "workspace start failed"
		log.Printf("Provisioner: %s for %s: %v", errMsg, workspaceID, err)
		if _, dbErr := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET status = 'failed', last_sample_error = $2, updated_at = now() WHERE id = $1`,
			workspaceID, "workspace start failed"); dbErr != nil {
			log.Printf("Provisioner: failed to mark workspace %s as failed: %v", workspaceID, dbErr)
		}
		h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_PROVISION_FAILED", workspaceID, map[string]interface{}{
			"error": "workspace start failed",
		})
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
		return "langgraph"
	}
	if _, ok := knownRuntimes[raw]; ok {
		return raw
	}
	log.Printf("provisioner: rejected unknown runtime %q, falling back to langgraph", raw)
	return "langgraph"
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
	switch runtime {
	case "langgraph", "deepagents":
		// These runtimes read API keys from env directly, no runtime_config needed.
	default:
		configYAML += "runtime_config:\n  timeout: 0\n"
	}

	files["config.yaml"] = []byte(configYAML)

	log.Printf("Provisioner: generated %d config files for workspace %s (runtime: %s)", len(files), workspaceID, runtime)
	return files
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
					return nil, fmt.Sprintf("cannot decrypt global secret %s: %v", k, decErr)
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
					return nil, fmt.Sprintf("cannot decrypt workspace secret %s: %v", k, decErr)
				}
				envVars[k] = string(decrypted)
			}
		}
	}
	return envVars, ""
}

// provisionWorkspaceCP provisions a workspace via the control plane API.
func (h *WorkspaceHandler) provisionWorkspaceCP(workspaceID, templatePath string, configFiles map[string][]byte, payload models.CreateWorkspacePayload) {
	ctx, cancel := context.WithTimeout(context.Background(), provisioner.ProvisionTimeout)
	defer cancel()

	envVars, decryptErr := loadWorkspaceSecrets(ctx, workspaceID)
	if decryptErr != "" {
		log.Printf("CPProvisioner: %s for %s", decryptErr, workspaceID)
		db.DB.ExecContext(ctx, `UPDATE workspaces SET status = 'failed', last_sample_error = $2, updated_at = now() WHERE id = $1`,
			workspaceID, decryptErr)
		return
	}

	applyAgentGitIdentity(envVars, payload.Name)
	applyRuntimeModelEnv(envVars, payload.Runtime, payload.Model)
	// Propagate role for role-aware plugins (#1957). See provisionWorkspace
	// above for rationale.
	if payload.Role != "" {
		envVars["MOLECULE_AGENT_ROLE"] = payload.Role
	}
	if err := h.envMutators.Run(ctx, workspaceID, envVars); err != nil {
		log.Printf("CPProvisioner: env mutator failed for %s: %v", workspaceID, err)
		// F1086 / #1206: env mutator errors (missing tokens, vault paths) must not
		// leak into last_sample_error — use generic message.
		db.DB.ExecContext(ctx, `UPDATE workspaces SET status = 'failed', last_sample_error = $2, updated_at = now() WHERE id = $1`,
				workspaceID, "plugin env mutator chain failed")
		return
	}

	cfg := provisioner.WorkspaceConfig{
		WorkspaceID: workspaceID,
		Tier:        payload.Tier,
		Runtime:     payload.Runtime,
		EnvVars:     envVars,
		PlatformURL: h.platformURL,
	}

	machineID, err := h.cpProv.Start(ctx, cfg)
	if err != nil {
		// F1086 / #1206: CP errors can include machine type, AMI IDs, VPC
		// paths — use generic message for broadcast and last_sample_error.
		errMsg := "workspace start failed"
		log.Printf("CPProvisioner: %s for %s: %v", errMsg, workspaceID, err)
		h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_PROVISION_FAILED", workspaceID, map[string]interface{}{
			"error": "provisioning failed",
		})
		db.DB.ExecContext(ctx, `UPDATE workspaces SET status = 'failed', last_sample_error = $2, updated_at = now() WHERE id = $1`,
			workspaceID, "provisioning failed")
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
	// Token issuance is deliberately deferred to the workspace's first
	// /registry/register call. Minting here without also delivering the
	// plaintext to the workspace (via user-data or a follow-up callback)
	// would leave a live token in DB that the workspace has no copy of —
	// RegistryHandler.requireWorkspaceToken would then 401 every
	// /registry/register attempt because the workspace is no longer in the
	// "no live tokens → bootstrap-allowed" state. The register handler
	// already mints a token on first successful register and returns it in
	// the response body for the workspace to persist.
}
