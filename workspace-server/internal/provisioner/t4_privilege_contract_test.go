package provisioner

import (
	"os"
	"strings"
	"testing"
)

// TestT4PrivilegeContract_AllCapabilitiesHaveRequiredFields enforces
// the invariant that every entry in the contract has at minimum a
// Name, Description, Probe, Severity, and Source — so the YAML the
// templates consume is never partially-filled (a quiet way to drift).
func TestT4PrivilegeContract_AllCapabilitiesHaveRequiredFields(t *testing.T) {
	caps := T4PrivilegeContract()
	if len(caps) == 0 {
		t.Fatal("T4PrivilegeContract returned zero capabilities — the gate would have nothing to assert")
	}
	for _, c := range caps {
		if c.Name == "" {
			t.Errorf("capability missing Name: %+v", c)
		}
		if c.Description == "" {
			t.Errorf("capability %q missing Description", c.Name)
		}
		if c.Probe == "" {
			t.Errorf("capability %q missing Probe", c.Name)
		}
		if c.Severity != SeverityHard && c.Severity != SeverityAdvisory {
			t.Errorf("capability %q has invalid Severity %q (allowed: hard, advisory)", c.Name, c.Severity)
		}
		if c.Source == "" {
			t.Errorf("capability %q missing Source — every capability must cite the RFC section or memory that motivates it", c.Name)
		}
	}
}

// TestT4PrivilegeContract_NamesAreUnique catches a silent
// dup-by-rename: if two capabilities share a name, AsYAML overwrites
// one in any YAML-loader-with-merge implementation, and CI output
// becomes ambiguous.
func TestT4PrivilegeContract_NamesAreUnique(t *testing.T) {
	caps := T4PrivilegeContract()
	seen := make(map[string]bool, len(caps))
	for _, c := range caps {
		if seen[c.Name] {
			t.Errorf("capability name %q appears more than once", c.Name)
		}
		seen[c.Name] = true
	}
}

// TestT4PrivilegeContract_CoreCapabilitiesPresent pins the minimum
// closure of capabilities the gate guarantees. Adding capabilities
// is fine; removing one of these requires updating this test
// (which the reviewer will see and challenge).
//
// These are exactly the post-conditions cited in RFC internal#456 §10–§11
// + task #128 (Files API) + task #174 (this task).
func TestT4PrivilegeContract_CoreCapabilitiesPresent(t *testing.T) {
	required := []string{
		"agent_uid_1000",
		"auth_token_agent_owned",
		"host_root_reach_via_nsenter",
		"docker_socket_reachable",
		"list_peers_http_200",
		"agent_home_writable",
		"network_egress_https",
	}
	caps := T4PrivilegeContract()
	have := make(map[string]bool, len(caps))
	for _, c := range caps {
		have[c.Name] = true
	}
	for _, r := range required {
		if !have[r] {
			t.Errorf("required capability %q missing from contract — RFC internal#456 / task #174 says this MUST be in the closure", r)
		}
	}
}

func TestT4PrivilegeContract_DefaultEgressUsesMoleculeOwnedEndpoint(t *testing.T) {
	for _, c := range T4PrivilegeContract() {
		for _, target := range c.RequiredEgress {
			if strings.Contains(target, "github.com") {
				t.Errorf("capability %q default egress target must not depend on GitHub mirror/API: %s", c.Name, target)
			}
			if strings.Contains(target, "google.com") {
				t.Errorf("capability %q default egress target must not depend on external Google endpoint: %s", c.Name, target)
			}
		}
	}
}

// TestT4PrivilegeContract_HardCapabilitiesMajority sanity-checks that
// the contract is not silently advisory-only. If someone marks
// everything as "advisory" the gate becomes a no-op without anyone
// noticing — fail the test if hard capabilities are not the majority.
func TestT4PrivilegeContract_HardCapabilitiesMajority(t *testing.T) {
	caps := T4PrivilegeContract()
	hard := 0
	for _, c := range caps {
		if c.Severity == SeverityHard {
			hard++
		}
	}
	if hard*2 <= len(caps) {
		t.Errorf("hard capabilities (%d) must be the strict majority of %d total — otherwise the gate is a no-op", hard, len(caps))
	}
}

// TestAsYAML_IsParseableAndStable asserts the AsYAML output is
// stable across invocations (sorted by name) and contains every
// capability's name. We do not depend on a YAML parser here —
// presence of `- name: "<n>"` lines is sufficient and the format
// is deliberately the trivially-greppable subset.
func TestAsYAML_IsParseableAndStable(t *testing.T) {
	caps := T4PrivilegeContract()
	y1 := AsYAML(caps)
	y2 := AsYAML(caps)
	if y1 != y2 {
		t.Error("AsYAML output is not deterministic across calls — sort/format must be stable for CI diff sanity")
	}
	for _, c := range caps {
		needle := "- name: \"" + c.Name + "\""
		if !strings.Contains(y1, needle) {
			t.Errorf("AsYAML output missing %q", needle)
		}
	}
	// Header must cite the RFC so adopters can find the source of truth.
	if !strings.Contains(y1, "internal#456") {
		t.Error("AsYAML header must reference RFC internal#456 — that is the design-of-record")
	}
	if !strings.Contains(y1, "version: 1") {
		t.Error("AsYAML must declare schema version (templates parse-check on this)")
	}
}

// TestAsYAML_EscapesEmbeddedQuotes catches a regression in
// yamlEscape: a probe shell string containing a double-quote would
// produce an unparseable YAML scalar.
func TestAsYAML_EscapesEmbeddedQuotes(t *testing.T) {
	caps := []T4Capability{{
		Name:        "embedded_quote",
		Description: `says "hi"`,
		Probe:       `echo "ok"`,
		Severity:    SeverityHard,
		Source:      "test",
	}}
	y := AsYAML(caps)
	// We expect the embedded `"` to be backslash-escaped.
	if !strings.Contains(y, `\"hi\"`) {
		t.Errorf("AsYAML did not escape embedded double quotes; got:\n%s", y)
	}
	if !strings.Contains(y, `\"ok\"`) {
		t.Errorf("AsYAML did not escape embedded double quotes in Probe; got:\n%s", y)
	}
}

func TestGeneratedT4CapabilitiesYAMLMatchesSSOT(t *testing.T) {
	got, err := os.ReadFile("t4_capabilities.yaml")
	if err != nil {
		t.Fatalf("read generated t4_capabilities.yaml: %v", err)
	}
	want := AsYAML(T4PrivilegeContract())
	if string(got) != want {
		t.Fatal("generated t4_capabilities.yaml drifted from T4PrivilegeContract; regenerate with `go run ./cmd/t4-contract-dump > internal/provisioner/t4_capabilities.yaml`")
	}
}

// TestAgentUIDConsistency ties the contract to the existing
// provisioner-side AgentUID const. The probe for "agent_uid_1000"
// hard-codes `id -u == 1000`; if AgentUID ever changes (no one
// expects it to, but a CI guard is free), the probe must change too.
func TestAgentUIDConsistency(t *testing.T) {
	if AgentUID != 1000 {
		t.Fatalf("AgentUID is %d but the T4 contract's probes assume 1000; update t4_privilege_contract.go probes before changing AgentUID", AgentUID)
	}
}
