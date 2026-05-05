package handlers

// workspace_dispatchers.go — Single-source-of-truth dispatchers for the
// workspace lifecycle verbs (Create / Stop / Restart). Each helper picks
// the right backend (Docker for self-hosted, CP for SaaS) and either
// runs the per-backend body in a goroutine or synchronously, depending
// on caller need.
//
// The dispatchers are the architectural boundary between handler code
// (HTTP / orchestration) and per-backend implementations
// (workspace_provision.go for Docker + CP). Source-level pin tests in
// workspace_provision_auto_test.go enforce that handlers route through
// these helpers rather than calling the per-backend bodies directly —
// see TestNoCallSiteCallsDirectProvisionerExceptAuto, TestNoCallSiteCallsBareStop,
// TestNoBareBothNilCheck, TestOrgImportGate_UsesHasProvisionerNotBareField.
//
// Architectural docs: docs/architecture/backends.md.
//
// History:
//   - PR #2811 introduced provisionWorkspaceAuto + HasProvisioner gate
//     (closed the org-import SaaS-skip silent-drop bug class).
//   - PR #2824 added StopWorkspaceAuto (closed the team-collapse +
//     workspace-delete EC2-leak class — issues #2813, #2814).
//   - PR #2843 + #2846 + #2847 + #2848 added RestartWorkspaceAuto +
//     RestartWorkspaceAutoOpts + provisionWorkspaceAutoSync and
//     migrated the four workspace_restart.go dispatch sites.
//   - This file extracts the helpers from workspace.go so the dispatcher
//     trio + sync variant + gate accessor are visually co-located,
//     making it easier for the next contributor to find and add a new
//     lifecycle verb without inlining dispatch logic.

import (
	"context"
	"log"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
)

// HasProvisioner reports whether either backend (CP or local Docker) is
// wired. Callers that gate prep-work on "do we have something that can
// provision a container?" should use this rather than direct field access
// to either provisioner — those individual checks miss the SaaS path
// (cpProv set, provisioner nil) or the self-hosted path (provisioner set,
// cpProv nil) symmetrically. Org-import + future bulk paths gate their
// template/config/secret prep on this so the work isn't wasted on
// deployments where no backend is available.
func (h *WorkspaceHandler) HasProvisioner() bool {
	return h.cpProv != nil || h.provisioner != nil
}

// IsSaaS reports whether the CP (EC2) provisioner is wired. Each SaaS
// workspace runs on its own sibling EC2, so the per-workspace tier
// boundary is a Docker resource limit applied to the only container
// on that EC2 — there's no neighbour to protect from. Self-hosted
// runs many workspaces in one Docker daemon on a single host, so
// the tier-2-by-default safe-neighbour-share posture stays.
//
// Tier defaults across Create / OrgImport / canvas EmptyState branch
// on IsSaaS so SaaS users get T4 (full host access) by default and
// self-hosted users keep the lower-trust caps.
func (h *WorkspaceHandler) IsSaaS() bool {
	return h.cpProv != nil
}

// DefaultTier is the SaaS-aware default tier. T4 on SaaS (single
// container per EC2 — full host access matches the boundary), T3 on
// self-hosted (read-write workspace mount + Docker daemon access,
// most templates' baseline). Callers default to this when the user
// hasn't explicitly picked a tier.
func (h *WorkspaceHandler) DefaultTier() int {
	if h.IsSaaS() {
		return 4
	}
	return 3
}

// provisionWorkspaceAuto picks the backend (CP for SaaS, local Docker
// for self-hosted) and starts provisioning in a goroutine. Returns true
// when a backend was kicked off, false when neither is wired.
//
// Single source of truth for "start provisioning a workspace" across
// every caller (Create, OrgHandler.createWorkspaceTree, TeamHandler.Expand,
// future paths). Centralized routing here means callers don't repeat
// the "Docker vs CP" decision and can't drift on it.
//
// Self-marks-failed on the no-backend path: pre-2026-05-05 the false
// return was silent, and any caller that forgot to handle it (TeamHandler
// pre-#2367, OrgHandler.createWorkspaceTree pre-this-fix) silently
// dropped workspaces — they sat in 'provisioning' for 10 min until the
// sweeper marked them failed with the misleading "container started but
// never called /registry/register" message. Marking failed inside Auto
// closes that class: even if a future caller bypasses HasProvisioner
// gating or ignores the bool return, the workspace ends in a clean
// failed state with an actionable error message.
//
// Architectural principle: templates own runtime/config/prompts/files/
// plugins; the platform owns where it runs. Anything that picks
// between CP and local Docker belongs in this one helper. Anything
// post-routing-but-pre-Start (mint secrets, render template, etc.)
// lives in prepareProvisionContext (shared by both per-backend
// goroutines).
func (h *WorkspaceHandler) provisionWorkspaceAuto(workspaceID, templatePath string, configFiles map[string][]byte, payload models.CreateWorkspacePayload) bool {
	if h.cpProv != nil {
		go h.provisionWorkspaceCP(workspaceID, templatePath, configFiles, payload)
		return true
	}
	if h.provisioner != nil {
		go h.provisionWorkspace(workspaceID, templatePath, configFiles, payload)
		return true
	}
	// No backend wired — mark failed so the workspace doesn't linger in
	// 'provisioning' for the full 10-minute sweep window. 10s is enough
	// for the broadcast + single UPDATE inside markProvisionFailed.
	log.Printf("provisionWorkspaceAuto: no provisioning backend wired for %s — marking failed (cpProv=nil, provisioner=nil)", workspaceID)
	failCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.markProvisionFailed(failCtx, workspaceID,
		"no provisioning backend available — workspace requires either a Docker daemon (self-hosted) or control-plane provisioner (SaaS)",
		nil)
	return false
}

// provisionWorkspaceAutoSync is the synchronous variant of
// provisionWorkspaceAuto — it BLOCKS in the current goroutine until the
// per-backend provision body returns, instead of spawning a goroutine.
//
// Used by callers that need to coordinate stop+provision as a pair and
// can't return until provision is done — today that's runRestartCycle
// (auto-restart cycle's pending-flag loop relies on synchronous return
// to know when it's safe to start the next cycle without racing the
// in-flight provision goroutine on the next iteration's Stop call).
//
// Backend selection + no-backend fallback are identical to
// provisionWorkspaceAuto. The only difference is the goroutine wrapper.
// Keep these two helpers in sync — when one grows a new arm (third
// backend, retry semantics), the other should too.
func (h *WorkspaceHandler) provisionWorkspaceAutoSync(workspaceID, templatePath string, configFiles map[string][]byte, payload models.CreateWorkspacePayload) bool {
	if h.cpProv != nil {
		h.provisionWorkspaceCP(workspaceID, templatePath, configFiles, payload)
		return true
	}
	if h.provisioner != nil {
		h.provisionWorkspace(workspaceID, templatePath, configFiles, payload)
		return true
	}
	log.Printf("provisionWorkspaceAutoSync: no provisioning backend wired for %s — marking failed (cpProv=nil, provisioner=nil)", workspaceID)
	failCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.markProvisionFailed(failCtx, workspaceID,
		"no provisioning backend available — workspace requires either a Docker daemon (self-hosted) or control-plane provisioner (SaaS)",
		nil)
	return false
}

// StopWorkspaceAuto picks the backend (CP for SaaS, local Docker for
// self-hosted) and stops the workspace synchronously. Returns nil when
// neither backend is wired (a workspace nobody is running can't be
// stopped — that's a no-op, not an error).
//
// Single source of truth for "stop a workspace" — symmetric with
// provisionWorkspaceAuto. Pre-2026-05-05 the stop side had no Auto
// dispatcher and every caller wrote `if h.provisioner != nil { Stop }`,
// which silently leaked EC2s on SaaS:
//   - team.go:208 (Collapse) — issue #2813
//   - workspace_crud.go:432 (stopAndRemove during Delete) — issue #2814
//
// Both bugs reproduced for ~6 months. The pattern is the same drift
// class as the org-import provision bug closed by PR #2811.
//
// Why CP wins when both are wired (matching provisionWorkspaceAuto):
// production runs exactly one backend at a time — a SaaS tenant has
// cpProv set + provisioner nil; a self-hosted operator has provisioner
// set + cpProv nil. The "both set" case only arises in test fixtures,
// and the CP-wins ordering matches how Auto picks for provisioning so
// the test stubs stay on a single side.
//
// Volume cleanup (workspace_crud.go) stays Docker-only — CP-managed
// workspaces have no volumes to clean. Callers that need that extra
// step keep their `if h.provisioner != nil { RemoveVolume(...) }`
// gate AFTER calling StopWorkspaceAuto. The abstraction here is "stop
// the running workload," not "tear down all state."
func (h *WorkspaceHandler) StopWorkspaceAuto(ctx context.Context, workspaceID string) error {
	if h.cpProv != nil {
		return h.cpProv.Stop(ctx, workspaceID)
	}
	if h.provisioner != nil {
		return h.provisioner.Stop(ctx, workspaceID)
	}
	return nil
}

// RestartWorkspaceAuto stops the running workload (with retry semantics
// tuned for the restart hot path) then starts provisioning again, in a
// detached goroutine. Returns true when a backend was kicked off, false
// when neither is wired (caller owns the persist + mark-failed surface
// in that case — symmetric with provisionWorkspaceAuto's bool return).
//
// Single source of truth for "restart a workspace" — third in the
// dispatcher trio alongside provisionWorkspaceAuto and StopWorkspaceAuto.
// Phase 1 of #2799 introduces this helper + migrates one caller; the
// remaining workspace_restart.go sites (Restart HTTP handler goroutine,
// Resume handler, Pause loop) follow in Phase 2/3 because they need
// async-context reasoning beyond a fire-and-return dispatcher.
//
// Retry on the Stop leg is intentional and distinguishes this from
// StopWorkspaceAuto:
//
//   - StopWorkspaceAuto (Stop-on-delete contract): no retry, no-backend
//     is a silent no-op. Different verb, different stakes — a workspace
//     nobody is running can't be stopped.
//
//   - RestartWorkspaceAuto: bounded exponential backoff on cpProv.Stop
//     via cpStopWithRetry. Restart's contract is "make the workspace
//     alive again" — refusing to reprovision when Stop fails strands
//     the user with a dead workspace and no recovery path other than
//     manual canvas intervention. Retry absorbs the transient CP/AWS
//     hiccups that cause most EC2-leak-adjacent incidents. On final
//     exhaustion, cpStopWithRetry logs LEAK-SUSPECT and proceeds with
//     reprovision regardless, bridging to the orphan reconciler.
//
// Docker provisioner.Stop has no retry — a local container that fails
// to stop is a local infrastructure problem (OOM, resource pressure)
// and retries won't help; the subsequent provision attempt will surface
// the underlying daemon failure.
//
// Architectural note: this helper encapsulates the stop+reprovision
// pair. The "which backend for stop" and "which backend for provision"
// decisions live here and stay in sync (CP-stop pairs with CP-provision;
// Docker-stop pairs with Docker-provision). Callers that need only the
// stop half use StopWorkspaceAuto (delete path) or stopForRestart
// (restart-path internal helper) directly.
//
// Payload requirements: caller MUST construct payload from the live
// workspace row (name, runtime, tier, model, workspace_dir, etc.) so
// the reprovision comes up with the workspace's actual configuration.
// runRestartCycle does this synchronously (line ~538) before delegating
// — match that pattern in any new caller.
func (h *WorkspaceHandler) RestartWorkspaceAuto(ctx context.Context, workspaceID, templatePath string, configFiles map[string][]byte, payload models.CreateWorkspacePayload) bool {
	return h.RestartWorkspaceAutoOpts(ctx, workspaceID, templatePath, configFiles, payload, false)
}

// RestartWorkspaceAutoOpts is the variant that carries Docker-only
// per-invocation knobs that don't fit on CreateWorkspacePayload. Today
// the only such knob is resetClaudeSession (issue #12 — clears the
// in-container Claude session before restart so the agent comes up
// fresh). CP doesn't have a session-reset concept (each EC2 boots from
// a fresh image), so the flag is silently ignored on the CP path.
//
// Most callers should call RestartWorkspaceAuto (resetClaudeSession=
// false). The Restart HTTP handler is the one site that exposes the
// flag to operators — it reads ?reset_session=true from the query
// string when an operator wants to force a fresh session.
func (h *WorkspaceHandler) RestartWorkspaceAutoOpts(ctx context.Context, workspaceID, templatePath string, configFiles map[string][]byte, payload models.CreateWorkspacePayload, resetClaudeSession bool) bool {
	// Stop leg first. CP-first ordering matches the other dispatchers
	// (provisionWorkspaceAuto, StopWorkspaceAuto) and the convention
	// documented in docs/architecture/backends.md.
	if h.cpProv != nil {
		h.cpStopWithRetry(ctx, workspaceID, "RestartWorkspaceAuto")
		// resetClaudeSession is Docker-only — CP has no session state to clear.
		go h.provisionWorkspaceCP(workspaceID, templatePath, configFiles, payload)
		return true
	}
	if h.provisioner != nil {
		// Docker.Stop has no retry — see docstring rationale.
		h.provisioner.Stop(ctx, workspaceID)
		go h.provisionWorkspaceOpts(workspaceID, templatePath, configFiles, payload, resetClaudeSession)
		return true
	}
	// No backend wired — same shape as provisionWorkspaceAuto's no-backend
	// arm. Mark the workspace failed so the user sees a meaningful state
	// rather than a hang. 10s context lets markProvisionFailed broadcast
	// + UPDATE; the original ctx may already be cancelled.
	log.Printf("RestartWorkspaceAuto: no provisioning backend wired for %s — marking failed (cpProv=nil, provisioner=nil)", workspaceID)
	failCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.markProvisionFailed(failCtx, workspaceID,
		"no provisioning backend available — workspace requires either a Docker daemon (self-hosted) or control-plane provisioner (SaaS)",
		nil)
	return false
}
