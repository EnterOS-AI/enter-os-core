package handlers

// plugins_reconcile.go — post-online declared-plugin reconcile (RFC#2843 #32).
//
// THE RULE (CTO): agent-skills are PLUGINS. They install DYNAMICALLY after a
// workspace boots online, via the EXISTING plugin install pipeline — NEVER via
// the provisioning channel (Secrets Manager or the template-asset relay). The
// old org_import.go path that copied declared plugins into `configFiles` (the
// provisioning channel) is removed; this reconcile replaces it.
//
// TRIGGER CHOICE — the registry heartbeat's transition-to-online, NOT a new
// sweeper or a hook buried in the provisioner:
//
//   - The heartbeat handler (registry.go Heartbeat) is the SINGLE place every
//     workspace flips to `online` from ANY prior state — provisioning,
//     offline, awaiting_agent, failed, and degraded→online recovery. A
//     workspace is only reachable for an install once it is online (the
//     install pipeline needs a running container or a live EC2 instance), so
//     "just reached online" is exactly the right moment.
//   - The existing drift_sweeper reconciles UPDATE drift only (tracked_ref
//     moved) and explicitly does NOT install-missing. Extending it would mean
//     a periodic full-fleet scan for a one-shot, event-driven need, and would
//     install into workspaces that are offline (and fail). The event hook is
//     cheaper and lands the plugin within one heartbeat of boot.
//   - Wiring matches the existing SetQueueDrainFunc pattern: RegistryHandler
//     holds a nil-safe ReconcileFunc, the router wires it to
//     PluginsHandler.ReconcileWorkspacePlugins after both are constructed, and
//     the heartbeat fires it fire-and-forget via globalGoAsync on each
//     transition-to-online.
//
// IDEMPOTENT + RETRY-SAFE: the reconcile diffs the DECLARED set
// (workspace_declared_plugins) against the INSTALLED set (workspace_plugins).
// An already-installed plugin is skipped (no-op). A failed install leaves no
// workspace_plugins row, so the NEXT transition-to-online retries it. Each
// install is logged. The install pipeline itself is idempotent (atomic
// stage→swap on Docker, rm -rf + re-extract on EIC).

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/plugins"
)

// ReconcileFunc installs any declared-but-missing plugins for a workspace
// that has just reached `online`. Injected into RegistryHandler at router
// wiring time to avoid a handler→handler import cycle (same pattern as
// QueueDrainFunc). nil-safe: the heartbeat skips reconcile when unset.
type ReconcileFunc func(ctx context.Context, workspaceID string)

// reconcileDeadline bounds one workspace's full reconcile pass (all its
// declared plugins). A single gitea clone of the seo-all package (~668 KiB)
// finishes in seconds; the per-fetch PLUGIN_INSTALL_FETCH_TIMEOUT (5m
// default) still bounds each individual install inside this budget.
const reconcileDeadline = 10 * time.Minute

// ReconcileWorkspacePlugins is the production ReconcileFunc. It is exported so
// the router can wire it into RegistryHandler.SetReconcileFunc.
//
// Flow:
//  1. Load the declared set (workspace_declared_plugins) — desired state.
//  2. Load the installed set (workspace_plugins names) — current state.
//  3. For each declared plugin NOT installed: resolveAndStage(source_raw) →
//     deliverToContainer → record the install. Skip already-installed (no-op).
//
// Errors per plugin are logged and DO NOT abort the rest of the pass — a
// single bad source must not starve the other plugins. A plugin that fails
// here is retried on the next transition-to-online (no install row written).
func (h *PluginsHandler) ReconcileWorkspacePlugins(ctx context.Context, workspaceID string) {
	if workspaceID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, reconcileDeadline)
	defer cancel()

	declared, err := listDeclaredPlugins(ctx, workspaceID)
	if err != nil {
		log.Printf("Plugin reconcile: workspace=%s list declared failed: %v", workspaceID, err)
		return
	}
	if len(declared) == 0 {
		return // common case: workspace declares no plugins
	}

	installed, err := listInstalledPluginNames(ctx, workspaceID)
	if err != nil {
		log.Printf("Plugin reconcile: workspace=%s list installed failed: %v", workspaceID, err)
		return
	}

	var installedCount, skipped int
	for _, d := range declared {
		if installed[d.PluginName] {
			skipped++
			continue // already installed — idempotent no-op
		}

		// Track value: a tag:/sha: ref opts the plugin into drift tracking;
		// a branch ref or no ref → "none" (the install records the SHA but
		// the sweeper won't chase a moving branch).
		track := trackFromSource(d.SourceRaw)

		stage, stageErr := h.resolveAndStage(ctx, installRequest{Source: d.SourceRaw, Track: track})
		if stageErr != nil {
			log.Printf("Plugin reconcile: workspace=%s plugin=%s stage failed (source=%s): %v",
				workspaceID, d.PluginName, d.SourceRaw, stageErr)
			continue
		}

		// RFC#2843 #38: the runtime-image entrypoint boot-installs declared
		// plugins to the box on EVERY boot, BEFORE this online-transition
		// reconcile runs — so by now the plugin is typically already on the box.
		// If present, record the tracking row WITHOUT re-delivering via EIC +
		// restarting: the EIC push + restartFunc was the redundant churn (one
		// wasted full re-provision per fresh workspace — the observed ~12-min
		// reprovision). Only deliver when the plugin is NOT already present
		// (boot-install disabled/failed, or a non-boot-install path) — the
		// safety net. pluginPresentOnBox is conservative (false on any
		// uncertainty) so a genuinely-missing install is never silently skipped.
		if h.pluginPresentOnBox(ctx, workspaceID, stage.PluginName) {
			log.Printf("Plugin reconcile: workspace=%s plugin=%s already on box (boot-installed) — recording tracking row only, no re-deliver/restart",
				workspaceID, stage.PluginName)
		} else if deliverErr := h.deliver(ctx, workspaceID, stage); deliverErr != nil {
			stage.cleanup()
			log.Printf("Plugin reconcile: workspace=%s plugin=%s deliver failed: %v",
				workspaceID, d.PluginName, deliverErr)
			continue
		}
		stage.cleanup()

		if recErr := recordWorkspacePluginInstall(
			ctx, workspaceID, stage.PluginName, stage.Source.Raw(), track, stage.InstalledSHA,
		); recErr != nil {
			// Install succeeded on the box; the tracking row failed. Log it —
			// the next reconcile will see the plugin missing from
			// workspace_plugins and re-install (idempotent on the box). This is
			// the same trade-off the interactive install path makes.
			log.Printf("Plugin reconcile: workspace=%s plugin=%s install delivered but tracking-row write failed: %v",
				workspaceID, stage.PluginName, recErr)
			continue
		}

		installedCount++
		log.Printf("Plugin reconcile: workspace=%s installed declared plugin %s from %s (sha=%s)",
			workspaceID, stage.PluginName, d.SourceRaw, shortSHA(stage.InstalledSHA))
	}

	if installedCount > 0 {
		log.Printf("Plugin reconcile: workspace=%s complete — installed=%d skipped(already-present)=%d declared=%d",
			workspaceID, installedCount, skipped, len(declared))
	}
}

// pluginPresentOnBox reports whether the plugin is already installed on the
// workspace box — e.g. delivered by the runtime-image boot-install
// (RFC#2843 #32) before this online-transition reconcile runs. Used to skip the
// redundant EIC re-deliver + restart that caused the per-fresh-workspace
// reprovision churn (#38). CONSERVATIVE by design: returns false on any
// uncertainty (not SaaS / no instance / read error / empty manifest), so the
// caller falls back to delivering — a genuinely-missing install is never
// silently skipped, only a confirmed-present one is deduped.
func (h *PluginsHandler) pluginPresentOnBox(ctx context.Context, workspaceID, pluginName string) bool {
	instanceID, runtime := h.lookupSaaSDispatch(workspaceID)
	if instanceID == "" {
		return false // not a SaaS box we can probe — deliver as before
	}
	data, err := readPluginManifestViaEIC(ctx, instanceID, runtime, pluginName)
	if err != nil || len(data) == 0 {
		return false // can't confirm presence — fall back to deliver
	}
	return true
}

// cleanup removes the staged tempdir. Mirrors the defer the interactive
// install path uses; the reconcile must clean up explicitly because it stages
// many plugins in a loop rather than one-per-request.
func (s *stageResult) cleanup() {
	if s != nil && s.StagedDir != "" {
		_ = os.RemoveAll(s.StagedDir)
	}
}

// trackFromSource maps a source-contract ref to a workspace_plugins.tracked_ref
// value. Only tag:/sha: refs are tracked; a branch ref (e.g. "#main") or no
// ref → "none" (the sweeper can't meaningfully chase a branch tip via the
// tracked_ref model).
func trackFromSource(source string) string {
	src, err := plugins.ParseSource(source)
	if err != nil {
		return "none"
	}
	idx := strings.Index(src.Spec, "#")
	if idx < 0 || idx+1 >= len(src.Spec) {
		return "none"
	}
	ref := src.Spec[idx+1:]
	if strings.HasPrefix(ref, "tag:") && len(ref) > 4 {
		return ref
	}
	if strings.HasPrefix(ref, "sha:") && len(ref) > 4 {
		return ref
	}
	return "none"
}
