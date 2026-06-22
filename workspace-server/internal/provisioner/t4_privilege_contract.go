// Package provisioner — T4 privilege contract.
//
// This file is the single source of truth for what a Tier-4 ("full
// machine access") workspace runtime MUST guarantee, expressed as code
// templates can reference and CI can verify.
//
// RFC: molecule-ai/internal#456 (per-template privilege-contract class).
// Task: molecule-ai/internal #174.
//
// Background
// ----------
// Prior art is RFC#456's three layers:
//
//	(1) molecule-runtime self-enforces uid-1000 + fchown safety net,
//	(2) a platform-owned wrapper entrypoint from a shared base image,
//	(3) a REQUIRED CI conformance gate wired into the fresh-provision
//	    harness that asserts the post-condition, not the mechanism.
//
// This file is the *data shape* for layer (3): the gate's tests have
// been hand-written per-template (template-claude-code, template-hermes,
// template-codex). Hand-writing drifts; the Hermes 401 class came from
// drift. We need the capability list itself to be code so that:
//
//   - The provisioner can dump it as `t4_capabilities.yaml` for any
//     fork user or non-Molecule-AI template runner to consume directly
//     (no hardcoded internal org).
//   - A `Verify(...)` helper turns into the t4-conformance shell out of
//     one file, so when a capability is added the templates pick it up
//     by reading the YAML — they do not silently lag.
//   - The provisioner-emit side (provisioner.go applyTierResources / T4
//     branch) and the verifier side share the same constants for the
//     uid + mount paths, eliminating "string-match" drift between
//     emitter and gate.
//
// Non-goals
// ---------
//   - This is NOT a substitute for layer (1)/(2). Templates still must
//     `exec gosu agent` and write /configs/.auth_token under uid 1000;
//     this file describes *what to check*, not how to achieve it.
//   - This file does not run tests. It is the spec. CI workflows call
//     `T4PrivilegeContract().AsYAML()` once at the start of the gate
//     and assert each capability's `Probe` returns ok.
package provisioner

import (
	"fmt"
	"sort"
	"strings"
)

// T4Capability is one assertion the T4 runtime MUST satisfy.
//
// Each capability declares:
//   - Name:        stable id (used as the test name in CI output).
//   - Description: human-readable why-this-matters; goes in failure logs.
//   - Probe:       a shell snippet that exits 0 on pass, non-zero on fail.
//     The probe MUST be deterministic, MUST be runnable inside the
//     running container under uid 1000, and MUST NOT depend on outside
//     network beyond what `RequiredEgress` declares.
//   - Severity:    "hard" capabilities fail the gate; "advisory" emit a
//     warning. T4 contract minimum = all hard pass.
//   - Source:      RFC section or memory reference that motivated this
//     capability — keeps the audit trail in-tree.
type T4Capability struct {
	Name           string   `yaml:"name"`
	Description    string   `yaml:"description"`
	Probe          string   `yaml:"probe"`
	Severity       string   `yaml:"severity"`
	Source         string   `yaml:"source"`
	RequiredEgress []string `yaml:"required_egress,omitempty"`
}

// SeverityHard / SeverityAdvisory enumerate the only allowed Severity
// values. We do not use Go enums because the YAML consumer is shell.
const (
	SeverityHard     = "hard"
	SeverityAdvisory = "advisory"
)

// T4PrivilegeContract returns the full T4 capability set.
//
// Add new capabilities here. Each one is automatically picked up by
// any template whose CI consumes `t4_capabilities.yaml` (no per-template
// PR needed for new checks — this is the anti-drift property).
//
// Capability ordering matters for human-readable CI output but is not
// load-bearing for correctness; AsYAML() emits them sorted by Name.
func T4PrivilegeContract() []T4Capability {
	return []T4Capability{
		{
			Name:        "agent_uid_1000",
			Description: "The container's primary process (the runtime, post-gosu) runs as uid 1000, not root. T4 grants full machine access via privileged + host PID + Docker socket — the WORKLOAD inside that privileged container must still be unprivileged to prevent every untrusted code execution from being trivially root-on-host.",
			Probe:       `[ "$(id -u)" = "1000" ]`,
			Severity:    SeverityHard,
			Source:      "RFC internal#456 §2.1.2; memory feedback_hermes_listpeers_401_token_root600_unreadable_by_uid1000",
		},
		{
			Name:        "auth_token_agent_owned",
			Description: "/configs/.auth_token is owned by uid 1000 (== AgentUID) so the a2a_mcp_server can read its bearer. In SaaS mode molecule-runtime itself writes the token via save_token() — the ownership equals the runtime's exec uid. If the runtime ever runs as root, this fails and list_peers 401s (the Hermes class bug).",
			Probe:       `[ -e /configs/.auth_token ] && [ "$(stat -c '%u' /configs/.auth_token)" = "1000" ]`,
			Severity:    SeverityHard,
			Source:      "RFC internal#456 §10; memory feedback_hermes_listpeers_401_token_root600_unreadable_by_uid1000",
		},
		{
			Name:        "host_root_reach_via_nsenter",
			Description: "The uid-1000 agent can attain host root via `sudo -n nsenter --target 1 --mount --pid -- id -u` returning 0. This is the T4 escalation leg: full machine access means the agent CAN escalate to host root deliberately, even though it does not run as root by default.",
			Probe:       `[ "$(sudo -n nsenter --target 1 --mount --pid -- id -u)" = "0" ]`,
			Severity:    SeverityHard,
			Source:      "RFC internal#456 §11; memory reference_per_template_privilege_contract_class_audit_2026_05_16",
		},
		{
			Name:        "host_fs_write_readback",
			Description: "Host filesystem is mounted at /host and the agent can write+read+remove a file there via sudo. Proves real host reach (not just a PID-1 namespace trick on an isolated init).",
			Probe: `MARKER="t4cap-$(date +%s)-$RANDOM"; PROBE_FILE="/host/tmp/.t4-cap-probe-${MOLECULE_T4_PROBE_ID:-$$}"; ` +
				`sudo -n sh -c "echo $MARKER > $PROBE_FILE" && ` +
				`[ "$(sudo -n cat $PROBE_FILE)" = "$MARKER" ] && ` +
				`sudo -n rm -f $PROBE_FILE`,
			Severity: SeverityHard,
			Source:   "RFC internal#456 §11",
		},
		{
			Name:        "docker_socket_reachable",
			Description: "/var/run/docker.sock is bind-mounted and host Docker is reachable from the T4 container. The probe enters the host mount+PID namespaces before running docker info so it validates the same host-control path production agents use, instead of depending on the template image's Docker CLI/socket group details.",
			Probe:       `sudo -n nsenter --target 1 --mount --pid -- docker info >/dev/null 2>&1`,
			Severity:    SeverityHard,
			Source:      "provisioner.go applyHostConfig T4 branch (case 4)",
		},
		{
			Name:        "list_peers_http_200",
			Description: "The platform list_peers HTTP endpoint (served by the in-container a2a_mcp_server) returns HTTP 200 when called from uid 1000 with the bearer from /configs/.auth_token. This proves the WHOLE token-ownership chain end-to-end: token written under correct uid → reader uid matches → bearer non-empty → platform accepts. A self-contained empirical test for the Hermes class bug.",
			Probe: `BEARER=$(cat /configs/.auth_token 2>/dev/null || echo ""); ` +
				`[ -n "$BEARER" ] || exit 1; ` +
				`PORT=$(cat /configs/.platform_port 2>/dev/null || echo "8080"); ` +
				`STATUS=$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $BEARER" "http://127.0.0.1:${PORT}/list_peers"); ` +
				`[ "$STATUS" = "200" ]`,
			Severity: SeverityHard,
			Source:   "memory reference_openclaw_fresh_provision_nonfunctional_anthropic_default_unroutable; memory reference_openclaw_mcp_peer_wiring_rootcause",
		},
		{
			Name:        "agent_home_writable",
			Description: "/agent-home is writable by the agent (Files API split per task #128). The Files API redesign uses /agent-home as the user-writable root; the agent must be able to create files there without sudo.",
			Probe:       `TF=/agent-home/.t4-cap-write-probe-${MOLECULE_T4_PROBE_ID:-$$}; echo ok > "$TF" && [ "$(cat "$TF")" = "ok" ] && rm -f "$TF"`,
			Severity:    SeverityHard,
			Source:      "task #128 Files API redesign; memory reference_post_suspension_pipeline",
		},
		{
			Name:        "network_egress_https",
			Description: "Generic HTTPS egress works. T4 is unconstrained network; the canonical test target is the Molecule-owned Gitea middleman over its public name. CI must not depend on GitHub or other mirrors for this probe. Any reachable HTTPS endpoint satisfies it — the YAML carries the recommended targets but accepts any 200/301/302.",
			Probe: `for U in $MOLECULE_T4_EGRESS_TARGETS; do ` +
				`  C=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 8 "$U"); ` +
				`  case "$C" in 2*|3*) exit 0;; esac; ` +
				`done; exit 1`,
			Severity: SeverityHard,
			Source:   "task #174 brief",
			RequiredEgress: []string{
				// Molecule-owned, public, no auth, returns a small JSON.
				// Adopters override via MOLECULE_T4_EGRESS_TARGETS.
				"https://git.moleculesai.app/api/v1/version",
			},
		},
		{
			Name:        "privileged_flag_observable",
			Description: "Container is started with --privileged. Observable from inside via /proc/self/status CapEff containing CAP_SYS_ADMIN. Defense-in-depth for the provisioner emission side.",
			Probe:       `grep -q '^CapEff:.*ffffffffff' /proc/self/status`,
			Severity:    SeverityAdvisory, // Imperfect — some CAP filters trim CapEff; advisory only.
			Source:      "provisioner.go applyHostConfig T4 branch (case 4)",
		},
		{
			Name:        "pid_host_visible",
			Description: "Host PID namespace is shared (--pid=host). The container can see host process 1 (systemd or pid-1 on the EC2 instance). Required for nsenter into host mount/pid namespaces.",
			Probe:       `[ "$(sudo -n nsenter --target 1 --mount --pid -- id -u)" = "0" ]`,
			Severity:    SeverityHard,
			Source:      "provisioner.go applyHostConfig T4 branch (case 4): hostCfg.PidMode = 'host'",
		},
	}
}

// AsYAML renders the contract as a single YAML document templates can
// fetch at CI time. Sorted by Name for deterministic diffs.
//
// We deliberately do not depend on a YAML library here — the format is
// trivial, and one-file pure-stdlib means this can be vendored or
// dumped from any Go context (including a `go run` script in CI).
//
// The format is stable; downstream consumers must treat unknown fields
// as warnings, not errors.
func AsYAML(caps []T4Capability) string {
	sorted := make([]T4Capability, len(caps))
	copy(sorted, caps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	b.WriteString("# T4 privilege contract — generated from\n")
	b.WriteString("# molecule-ai/molecule-core workspace-server/internal/provisioner/t4_privilege_contract.go\n")
	b.WriteString("# RFC: molecule-ai/internal#456\n")
	b.WriteString("# Do NOT edit this file by hand; regenerate via `go run ./cmd/t4-contract-dump > t4_capabilities.yaml`.\n")
	b.WriteString("version: 1\n")
	b.WriteString("agent_uid: 1000\n")
	b.WriteString("capabilities:\n")
	for _, c := range sorted {
		fmt.Fprintf(&b, "  - name: %s\n", yamlEscape(c.Name))
		fmt.Fprintf(&b, "    description: %s\n", yamlEscape(c.Description))
		fmt.Fprintf(&b, "    severity: %s\n", c.Severity)
		fmt.Fprintf(&b, "    source: %s\n", yamlEscape(c.Source))
		fmt.Fprintf(&b, "    probe: %s\n", yamlEscape(c.Probe))
		if len(c.RequiredEgress) > 0 {
			b.WriteString("    required_egress:\n")
			for _, u := range c.RequiredEgress {
				fmt.Fprintf(&b, "      - %s\n", yamlEscape(u))
			}
		}
	}
	return b.String()
}

// yamlEscape is a minimal YAML scalar escaper. We always quote with
// double quotes and backslash-escape internal quotes + backslashes —
// safe for the subset of strings we emit (no control chars except \n
// and \t, both of which we replace with literal escapes).
func yamlEscape(s string) string {
	r := strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"\n", "\\n",
		"\t", "\\t",
	)
	return "\"" + r.Replace(s) + "\""
}
