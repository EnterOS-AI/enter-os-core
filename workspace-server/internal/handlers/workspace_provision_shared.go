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
	"log"
	"path/filepath"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
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
	EnvVars            map[string]string
	PluginsPath        string
	AwarenessNamespace string
	Config             provisioner.WorkspaceConfig
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
	envVars, decryptErr := loadWorkspaceSecrets(ctx, workspaceID)
	if decryptErr != "" {
		return nil, &provisionAbort{Msg: decryptErr}
	}

	pluginsPath, _ := filepath.Abs(filepath.Join(h.configsDir, "..", "plugins"))
	awarenessNamespace := h.loadAwarenessNamespace(ctx, workspaceID)

	// Per-agent git identity (#1957) — must run after secret loads so
	// a workspace_secret named GIT_AUTHOR_NAME can override.
	applyAgentGitIdentity(envVars, payload.Name)
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

	cfg := h.buildProvisionerConfig(ctx, workspaceID, templatePath, configFiles, payload, envVars, pluginsPath, awarenessNamespace)
	cfg.ResetClaudeSession = resetClaudeSession

	return &preparedProvisionContext{
		EnvVars:            envVars,
		PluginsPath:        pluginsPath,
		AwarenessNamespace: awarenessNamespace,
		Config:             cfg,
	}, nil
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
