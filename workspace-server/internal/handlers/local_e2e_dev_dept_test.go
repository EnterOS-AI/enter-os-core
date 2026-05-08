package handlers

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// Local E2E for the dev-department extraction (RFC internal#77).
//
// Pre-conditions: both repos cloned as siblings under
// /tmp/local-e2e-deploy/{molecule-dev, molecule-dev-department}.
// (Set up by the orchestrator before running this test.)
//
// What this proves end-to-end through real platform code:
//   1. resolveYAMLIncludes follows the dev-lead symlink at the parent's
//      template root and pulls in the dev-department subtree.
//   2. Recursive !include's inside the symlinked subtree resolve
//      correctly via the chain dev-lead/workspace.yaml →
//      ./core-lead/workspace.yaml → ./core-be/workspace.yaml etc.
//   3. The resolved YAML unmarshals into a complete OrgTemplate with the
//      expected count of workspaces (parent's PM+Marketing+Research +
//      dev-department's atomized 28 workspaces).
//
// Skipped if the local-e2e-deploy fixture isn't present — won't block
// CI on hosts that haven't set it up.
func TestLocalE2E_DevDepartmentExtraction(t *testing.T) {
	parent := "/tmp/local-e2e-deploy/molecule-dev"
	if _, err := os.Stat(filepath.Join(parent, "org.yaml")); err != nil {
		t.Skipf("local-e2e fixture not present at %s: %v", parent, err)
	}

	orgYAML, err := os.ReadFile(filepath.Join(parent, "org.yaml"))
	if err != nil {
		t.Fatalf("read org.yaml: %v", err)
	}

	expanded, err := resolveYAMLIncludes(orgYAML, parent)
	if err != nil {
		t.Fatalf("resolveYAMLIncludes failed: %v", err)
	}

	var tmpl OrgTemplate
	if err := yaml.Unmarshal(expanded, &tmpl); err != nil {
		t.Fatalf("unmarshal expanded OrgTemplate: %v", err)
	}

	// Walk the full workspace tree, collect names.
	names := []string{}
	var walk func([]OrgWorkspace)
	walk = func(ws []OrgWorkspace) {
		for _, w := range ws {
			names = append(names, w.Name)
			walk(w.Children)
		}
	}
	walk(tmpl.Workspaces)

	t.Logf("org name: %q", tmpl.Name)
	t.Logf("total workspaces (recursive): %d", len(names))
	for _, n := range names {
		t.Logf("  - %q", n)
	}

	// Expected: PM + Marketing Lead + Dev Lead at top level, plus the
	// full sub-trees under each. After atomization, we expect:
	//   - PM tree: PM + Research Lead + 3 research roles = 5
	//   - Marketing tree: Marketing Lead + 5 marketing roles = 6
	//   - Dev Lead tree: Dev Lead + (5 sub-team leads × ~6 each) +
	//     3 floaters + Triage Operator = ~32
	// Roughly ~43 total. Be liberal; just assert a floor.
	if len(names) < 30 {
		t.Errorf("workspace count too low (%d) — expected ~40+ (PM+Marketing+Dev tree)", len(names))
	}

	// Specific sentinel names we expect to find:
	expected := []string{
		"PM",
		"Marketing Lead",
		"Dev Lead",
		"Core Platform Lead",
		"Controlplane Lead",
		"App & Docs Lead",
		"Infra Lead",
		"SDK Lead",
		"Documentation Specialist", // Q1 — should be under app-lead
		"Triage Operator",          // Q2 — should be under dev-lead
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	for _, want := range expected {
		if !found[want] {
			t.Errorf("missing expected workspace %q", want)
		}
	}
}
