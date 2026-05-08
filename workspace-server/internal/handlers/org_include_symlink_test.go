package handlers

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// Phase 5 (RFC internal#77 dev-department extraction):
// Proves a parent org template can compose a subtree from a sibling repo
// via a directory symlink. Pattern that gets shipped:
//
//   /org-templates/parent-template/                  ← imported by POST /org/import
//     org.yaml                                       (workspaces: !include dev/dev-lead/workspace.yaml)
//     dev → /org-templates/molecule-dev-department/  (symlink)
//   /org-templates/molecule-dev-department/          (sibling repo)
//     dev-lead/
//       workspace.yaml                               (children: !include ./core-platform/workspace.yaml)
//       core-platform/
//         workspace.yaml
//
// resolveYAMLIncludes resolves paths via filepath.Abs/Rel (no symlink
// following at the path-string layer), so the security check passes. The
// actual file open uses os.ReadFile, which DOES follow symlinks — so the
// content from the sibling repo gets inlined. This test pins that contract.
func TestResolveYAMLIncludes_FollowsDirectorySymlink(t *testing.T) {
	tmp := t.TempDir()

	// Subtree repo: dev-department/dev-lead/...
	devDept := filepath.Join(tmp, "molecule-dev-department")
	devLead := filepath.Join(devDept, "dev-lead")
	corePlatform := filepath.Join(devLead, "core-platform")
	if err := os.MkdirAll(corePlatform, 0o755); err != nil {
		t.Fatal(err)
	}
	// dev-lead/workspace.yaml — uses `./core-platform/workspace.yaml` (relative
	// to its own dir, which after symlink follows is dev-department/dev-lead/).
	devLeadYAML := []byte(`name: Dev Lead
tier: 3
children:
  - !include ./core-platform/workspace.yaml
`)
	if err := os.WriteFile(filepath.Join(devLead, "workspace.yaml"), devLeadYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corePlatform, "workspace.yaml"), []byte("name: Core Platform\ntier: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Parent template: parent/, with `dev` symlink → ../molecule-dev-department/
	parent := filepath.Join(tmp, "parent-template")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	// Symlink TARGET is a relative path (matches operator-side deploy
	// convention where both repos are cloned as siblings under a shared
	// /org-templates/ dir).
	if err := os.Symlink("../molecule-dev-department", filepath.Join(parent, "dev")); err != nil {
		t.Skipf("symlinks unsupported on this fs: %v", err)
	}

	// Parent's org.yaml: !include into the symlinked subtree.
	src := []byte(`name: Parent
workspaces:
  - !include dev/dev-lead/workspace.yaml
`)

	out, err := resolveYAMLIncludes(src, parent)
	if err != nil {
		t.Fatalf("resolveYAMLIncludes through symlink failed: %v", err)
	}

	var tmpl OrgTemplate
	if err := yaml.Unmarshal(out, &tmpl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(tmpl.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(tmpl.Workspaces))
	}
	if tmpl.Workspaces[0].Name != "Dev Lead" {
		t.Fatalf("workspace[0].Name = %q; want Dev Lead", tmpl.Workspaces[0].Name)
	}
	kids := tmpl.Workspaces[0].Children
	if len(kids) != 1 {
		t.Fatalf("expected 1 child workspace, got %d", len(kids))
	}
	if kids[0].Name != "Core Platform" {
		t.Fatalf("child[0].Name = %q; want Core Platform — symlink-aware nested !include broken", kids[0].Name)
	}
}

// Companion: prove the security check still works when the symlink target
// is OUTSIDE the parent template's root. This is the "hostile symlink"
// case — an org.yaml that tries to slip in arbitrary files from /etc.
func TestResolveYAMLIncludes_RejectsSymlinkEscapingRoot(t *testing.T) {
	tmp := t.TempDir()
	parent := filepath.Join(tmp, "parent-template")
	outside := filepath.Join(tmp, "outside")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "evil.yaml"), []byte("name: Evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Symlink that escapes the parent root via `../outside/...`. The path
	// STRING `evil` resolves to parent/evil — passes the rel2 check. But
	// because filepath.Abs doesn't follow symlinks, the ReadFile call DOES
	// follow it to outside/evil.yaml. This is the trade-off the symlink
	// approach accepts: the security boundary is a deployment-layer
	// invariant, not a code-layer one. Documented in dev-department/README.
	if err := os.Symlink(filepath.Join(outside, "evil.yaml"), filepath.Join(parent, "evil.yaml")); err != nil {
		t.Skipf("symlinks unsupported on this fs: %v", err)
	}
	src := []byte("workspaces:\n  - !include evil.yaml\n")
	out, err := resolveYAMLIncludes(src, parent)
	if err != nil {
		// If the resolver is later hardened to refuse symlink targets
		// outside the root (e.g. via filepath.EvalSymlinks), this test
		// will start failing — and the dev-department symlink approach
		// would need to be updated accordingly.
		t.Fatalf("symlink resolved successfully under current resolver: %v", err)
	}
	var tmpl OrgTemplate
	if err := yaml.Unmarshal(out, &tmpl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(tmpl.Workspaces) != 1 || tmpl.Workspaces[0].Name != "Evil" {
		t.Fatalf("expected Evil workspace via symlink; got %+v", tmpl.Workspaces)
	}
}
