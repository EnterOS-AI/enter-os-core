package handlers

import (
	"strings"
	"testing"
)

// TestExternalTemplates_NoMoleculeOrgIDPlaceholder pins the invariant
// that operator-facing connection snippets do NOT advertise a
// MOLECULE_ORG_ID env var.
//
// Why: MOLECULE_ORG_ID is consumed only by the workspace-server's
// TenantGuard middleware (server-side, set by control plane via
// user-data on tenant boxes). The molecule_runtime MCP subprocess
// that codex/openclaw/hermes-channel spawns authenticates the client
// using Origin + Bearer token + X-Workspace-ID — it never reads
// MOLECULE_ORG_ID. Including the placeholder leaves operators with a
// "<your org id>" they can't fill, and external agents (codex CLI in
// particular) flag it as an unresolved setup blocker.
//
// The universal_mcp snippet is the reference: it calls into the same
// molecule_runtime and intentionally omits MOLECULE_ORG_ID.
func TestExternalTemplates_NoMoleculeOrgIDPlaceholder(t *testing.T) {
	templates := map[string]string{
		"externalCurlTemplate":            externalCurlTemplate,
		"externalUniversalMcpTemplate":    externalUniversalMcpTemplate,
		"externalPythonTemplate":          externalPythonTemplate,
		"externalHermesChannelTemplate":   externalHermesChannelTemplate,
		"externalCodexTemplate":           externalCodexTemplate,
		"externalOpenClawTemplate":        externalOpenClawTemplate,
	}
	for name, body := range templates {
		if strings.Contains(body, "MOLECULE_ORG_ID") {
			t.Errorf("%s contains MOLECULE_ORG_ID — operator-facing templates must not advertise this env var (TenantGuard reads it server-side from the tenant's own env, not the client)", name)
		}
		if strings.Contains(body, "<your org id>") {
			t.Errorf("%s contains \"<your org id>\" placeholder — operators have no value to substitute, drop the line", name)
		}
	}
}

// TestExternalMcpTemplates_UseMoleculeMcpWrapper pins the invariant
// that operator-facing snippets configuring an MCP server entry point
// use the ``molecule-mcp`` console-script wrapper (mcp_cli.main),
// NOT the bare ``a2a_mcp_server`` module.
//
// Why: a2a_mcp_server exposes the MCP tools but does NOT call
// /registry/register or run the 20s heartbeat thread. mcp_cli wraps
// it with both, which is what flips the canvas presence indicator
// from awaiting_agent (OFFLINE) to online and keeps it that way.
// Originally tracked by molecule-core#2957 — operator hit the
// silent-OFFLINE failure mode when the Codex tab pointed at the bare
// module.
//
// The hermes-channel template intentionally uses the bare module: it
// owns the platform plugin path and runs its own
// register_platform/heartbeat code in-process, so wrapping with
// mcp_cli would double-heartbeat. universalMcp / codex / openclaw
// must all use the wrapper.
func TestExternalMcpTemplates_UseMoleculeMcpWrapper(t *testing.T) {
	mustUseWrapper := map[string]string{
		"externalUniversalMcpTemplate": externalUniversalMcpTemplate,
		"externalCodexTemplate":        externalCodexTemplate,
		"externalOpenClawTemplate":     externalOpenClawTemplate,
	}
	for name, body := range mustUseWrapper {
		if !strings.Contains(body, "molecule-mcp") {
			t.Errorf("%s does not reference 'molecule-mcp' — operator-facing MCP snippets must point at the heartbeat-wrapping console script, not the bare a2a_mcp_server module (#2957)", name)
		}
		if strings.Contains(body, `"-m", "molecule_runtime.a2a_mcp_server"`) {
			t.Errorf("%s spawns 'python3 -m molecule_runtime.a2a_mcp_server' — that bypasses the standalone register/heartbeat wrapper, leaving the canvas showing the workspace OFFLINE (#2957). Use 'molecule-mcp' instead.", name)
		}
		if strings.Contains(body, `["-m", "molecule_runtime.a2a_mcp_server"]`) {
			t.Errorf("%s spawns 'python3 -m molecule_runtime.a2a_mcp_server' — that bypasses the standalone register/heartbeat wrapper, leaving the canvas showing the workspace OFFLINE (#2957). Use 'molecule-mcp' instead.", name)
		}
	}
}

// TestExternalTemplates_NoBrokenMoleculeAIGitHubURLs pins the invariant
// that operator-facing snippets never embed github.com URLs pointing at
// Molecule-AI repos.
//
// Why: the Molecule-AI GitHub org was suspended 2026-05-06 and the
// canonical SCM is now git.moleculesai.app. Any `pip install
// git+https://github.com/Molecule-AI/...` or marketplace-add Molecule-AI/
// URL emitted to an external operator hits a 404 / org-suspended page,
// breaking onboarding silently. RFC #229 P2-5.
//
// Third-party github URLs (gin, openai/codex, NousResearch/hermes-agent
// upstream issue trackers, npm @openai/codex) remain valid — only
// Molecule-AI/ paths are broken.
func TestExternalTemplates_NoBrokenMoleculeAIGitHubURLs(t *testing.T) {
	templates := map[string]string{
		"externalCurlTemplate":          externalCurlTemplate,
		"externalChannelTemplate":       externalChannelTemplate,
		"externalUniversalMcpTemplate":  externalUniversalMcpTemplate,
		"externalPythonTemplate":        externalPythonTemplate,
		"externalHermesChannelTemplate": externalHermesChannelTemplate,
		"externalCodexTemplate":         externalCodexTemplate,
		"externalOpenClawTemplate":      externalOpenClawTemplate,
	}
	// Substrings that imply the snippet is pointing an operator at the
	// suspended Molecule-AI GitHub org.
	bannedSubstrings := []string{
		"github.com/Molecule-AI/",
		"github.com/molecule-ai/",
		// Bare `Molecule-AI/<repo>` form used by `/plugin marketplace add`
		// resolves through GitHub by default — explicit Gitea URL is
		// required post-suspension.
		"marketplace add Molecule-AI/",
		"marketplace add molecule-ai/",
	}
	for name, body := range templates {
		for _, banned := range bannedSubstrings {
			if strings.Contains(body, banned) {
				t.Errorf("%s contains %q — Molecule-AI GitHub org is suspended; use git.moleculesai.app/molecule-ai/<repo> instead (RFC #229 P2-5)", name, banned)
			}
		}
	}
}

// TestExternalChannelTemplate_LaunchFlagShape pins the Claude Code channel
// snippet to the working launch invocation. The channel spec must be the
// VALUE of --dangerously-load-development-channels, NOT a separate
// --channels flag. The two-flag form (`--dangerously-load-development-channels
// --channels plugin:molecule@...`) errors with "entries must be tagged:
// --channels" on current Claude Code builds (2.1.143+) and silently no-ops
// on older ones — either way, new users hit a wall on first launch.
//
// Empirical: hit by a session walking through this exact snippet 2026-05-21;
// the broken form was copy-pasted from this template, ran, errored, and
// confused the operator into believing the plugin install was broken when
// the snippet itself was the bug.
func TestExternalChannelTemplate_LaunchFlagShape(t *testing.T) {
	// The broken two-flag form. If this string ever appears in the
	// snippet again, the same onboarding pothole returns.
	bannedFormBroken := "--dangerously-load-development-channels \\\n  --channels plugin:molecule@molecule-channel"
	if strings.Contains(externalChannelTemplate, bannedFormBroken) {
		t.Errorf("externalChannelTemplate contains the broken two-flag launch form. " +
			"Use --dangerously-load-development-channels plugin:molecule@molecule-channel (spec as value, not a separate --channels flag).")
	}

	// The single-flag form must be present.
	requiredFormGood := "--dangerously-load-development-channels plugin:molecule@molecule-channel"
	if !strings.Contains(externalChannelTemplate, requiredFormGood) {
		t.Errorf("externalChannelTemplate must contain %q so operators see the working launch invocation", requiredFormGood)
	}
}

// TestExternalChannelTemplate_CanonicalEnvShape pins the canvas-served
// .env example to the canonical SSOT shape (MOLECULE_WORKSPACES_JSON)
// rather than the legacy single-platform shape. The legacy form
// (MOLECULE_PLATFORM_URL + comma-separated IDs/TOKENS) is still accepted
// by the channel plugin's parseWorkspaceTargets but is single-tenant
// only — it silently fails to onboard users who want to watch multiple
// platforms (e.g. hongming + agents-team from the same plugin instance),
// which is the post-PR#15 expected use case.
func TestExternalChannelTemplate_CanonicalEnvShape(t *testing.T) {
	if !strings.Contains(externalChannelTemplate, "MOLECULE_WORKSPACES_JSON=") {
		t.Errorf("externalChannelTemplate must use MOLECULE_WORKSPACES_JSON as the canonical .env shape (the post-PR#15 SSOT)")
	}
	// The JSON example must contain the workspace_id + platform_url placeholders
	// so the canvas substitutes them at serve time.
	for _, ph := range []string{"{{WORKSPACE_ID}}", "{{PLATFORM_URL}}"} {
		if !strings.Contains(externalChannelTemplate, ph) {
			t.Errorf("externalChannelTemplate must contain placeholder %q so the canvas substitutes per-workspace values", ph)
		}
	}
}

// TestPollingTemplates_OptIntoPeerInfo pins the invariant that any template
// which calls /workspaces/:id/activity for inbound delivery requests the
// Layer 1 enrichment via ?include=peer_info. Without this opt-in, the
// platform returns bare activity rows and the operator's bridge / channel
// loses peer_name / peer_role / agent_card_url / attachments[] — they're
// available on the server but not delivered.
//
// Pre-Layer-1 platforms ignore unknown query params (HTTP spec: filters
// not understood are dropped), so this is back-compat across deploys.
//
// The Claude Code channel template doesn't include the poll URL in this
// snippet — its polling lives in the plugin's own server.ts (handled by
// molecule-mcp-claude-channel PR#21). The Kimi template DOES include a
// poll loop in its kimi_bridge.py block, so the invariant applies there.
func TestPollingTemplates_OptIntoPeerInfo(t *testing.T) {
	pollingTemplates := map[string]string{
		"externalKimiTemplate": externalKimiTemplate,
	}
	for name, body := range pollingTemplates {
		// If the snippet polls /activity, it must opt into peer_info.
		// The detection is intentionally loose ("/activity" appears in
		// the script) — operators who customize the script keep the
		// invariant only if the include hint is in the template.
		if !strings.Contains(body, "/activity") {
			t.Errorf("%s no longer polls /activity — review whether this test still applies", name)
			continue
		}
		if !strings.Contains(body, `"include": "peer_info"`) && !strings.Contains(body, "include=peer_info") {
			t.Errorf("%s polls /activity without ?include=peer_info — operators lose Layer 1 enrichment "+
				"(peer_name / peer_role / agent_card_url / attachments[]). Add the param to the poll URL.", name)
		}
	}
}
