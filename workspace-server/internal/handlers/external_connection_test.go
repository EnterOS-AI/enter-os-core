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
