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
