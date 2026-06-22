package handlers

import (
	"archive/tar"
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// Stage-2 of the local e2e: prove every resolved workspace's `files_dir`
// path actually consumes correctly through the rest of the import chain.
// resolveYAMLIncludes returning a populated OrgTemplate is necessary but
// not sufficient — `POST /org/import` then does:
//
//   1. resolveInsideRoot(orgBaseDir, ws.FilesDir) → must return a path
//      that exists and stat-resolves to a directory (org_import.go:313-317).
//   2. CopyTemplateToContainer(ctx, containerID, templatePath) → walks
//      the dir with filepath.Walk and tars its contents into the
//      workspace's /configs/ mount (provisioner.go:766-820).
//
// This stage-2 test exercises both #1 and #2 against every workspace in
// the resolved tree, mimicking what the platform does post-include-
// resolution. Catches: files_dir paths that don't resolve through the
// symlink, paths that exist but are empty (silently produces empty
// /configs/), or filepath.Walk failing to descend through cross-repo
// symlink boundaries.
func TestLocalE2E_FilesDirConsumption(t *testing.T) {
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
		t.Fatalf("resolveYAMLIncludes: %v", err)
	}
	var tmpl OrgTemplate
	if err := yaml.Unmarshal(expanded, &tmpl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Flatten every workspace — including children, grandchildren, etc.
	flat := []OrgWorkspace{}
	var walk func([]OrgWorkspace)
	walk = func(ws []OrgWorkspace) {
		for _, w := range ws {
			flat = append(flat, w)
			walk(w.Children)
		}
	}
	walk(tmpl.Workspaces)

	checked := 0
	for _, w := range flat {
		if w.FilesDir == "" {
			continue // workspace declared inline (no files_dir) — skip
		}
		checked++
		t.Run(w.Name+"/"+w.FilesDir, func(t *testing.T) {
			// Step 1: resolveInsideRoot returns a path that's-inside-root.
			abs, err := resolveInsideRoot(parent, w.FilesDir)
			if err != nil {
				t.Fatalf("resolveInsideRoot(%q, %q): %v", parent, w.FilesDir, err)
			}
			info, err := os.Stat(abs)
			if err != nil {
				t.Fatalf("stat %q (resolved from files_dir %q): %v", abs, w.FilesDir, err)
			}
			if !info.IsDir() {
				t.Fatalf("files_dir %q resolved to %q which is not a directory", w.FilesDir, abs)
			}

			// Step 2: walk the dir like CopyTemplateToContainer does.
			// Mirror the platform's symlink-resolution at the root —
			// filepath.Walk doesn't descend into a symlink leaf, so
			// CopyTemplateToContainer (provisioner.go) calls
			// EvalSymlinks on templatePath first. Replicate exactly.
			if resolved, err := filepath.EvalSymlinks(abs); err == nil {
				abs = resolved
			}
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			fileCount := 0
			fileNames := []string{}
			err = filepath.Walk(abs, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(abs, path)
				if err != nil {
					return err
				}
				if rel == "." {
					return nil
				}
				header, _ := tar.FileInfoHeader(info, "")
				header.Name = rel
				if err := tw.WriteHeader(header); err != nil {
					return err
				}
				if !info.IsDir() {
					fileCount++
					fileNames = append(fileNames, rel)
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					header.Size = int64(len(data))
					tw.Write(data)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("filepath.Walk %q (mimics CopyTemplateToContainer): %v", abs, err)
			}
			tw.Close()

			if fileCount == 0 {
				t.Errorf("files_dir %q at %q is empty — CopyTemplateToContainer would produce empty /configs/",
					w.FilesDir, abs)
			}

			// Sanity: every workspace folder should have AT LEAST one of
			// {workspace.yaml, system-prompt.md, initial-prompt.md} —
			// these are the markers a workspace folder is recognizable
			// as a workspace (mirrors validator's WORKSPACE_FOLDER_MARKERS).
			markers := []string{"workspace.yaml", "system-prompt.md", "initial-prompt.md"}
			hasMarker := false
			for _, name := range fileNames {
				for _, m := range markers {
					if name == m || strings.HasSuffix(name, "/"+m) {
						hasMarker = true
						break
					}
				}
				if hasMarker {
					break
				}
			}
			if !hasMarker {
				t.Errorf("files_dir %q at %q has %d files but none of the workspace markers %v — found: %v",
					w.FilesDir, abs, fileCount, markers, fileNames)
			}
		})
	}
	t.Logf("checked %d workspaces with files_dir", checked)
	if checked < 25 {
		t.Errorf("expected ~28 workspaces with files_dir (post-atomization); only saw %d", checked)
	}
}

// PR-C from the Phase 3a phasing (task #234): real-Gitea e2e for the
// !external resolver against the LIVE molecule-ai/molecule-dev-department
// repo. Verifies the production gitFetcher fetches the dev tree and the
// resolver grafts it correctly into a parent template that has NO
// symlink — composition is purely platform-side.
//
// Skipped if Gitea isn't reachable (offline / firewall / CI without
// network). Requires `git` binary on PATH.
func TestLocalE2E_ExternalDevDepartment(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary not found: %v", err)
	}

	// Skip if Gitea host isn't reachable (TCP probe). Avoids network-
	// dependent tests failing on offline runners.
	conn, err := net.DialTimeout("tcp", "git.moleculesai.app:443", 3*time.Second)
	if err != nil {
		t.Skipf("git.moleculesai.app:443 unreachable: %v", err)
	}
	conn.Close()

	// Build a minimal parent template inline — no need for the
	// /tmp/local-e2e-deploy/ symlinked fixture. The whole point of
	// !external is that the parent template is self-contained;
	// composition resolves over the network at import time.
	parent := t.TempDir()

	orgYAML := []byte(`name: External-Only Test Parent
description: Parent template that pulls the entire dev tree via !external.
defaults:
  runtime: claude-code
  tier: 2
workspaces:
  - !external
    repo: molecule-ai/molecule-dev-department
    ref: main
    path: dev-lead/workspace.yaml
`)
	if err := os.WriteFile(filepath.Join(parent, "org.yaml"), orgYAML, 0o644); err != nil {
		t.Fatalf("write org.yaml: %v", err)
	}

	out, err := resolveYAMLIncludes(orgYAML, parent)
	if err != nil {
		t.Fatalf("resolveYAMLIncludes (!external against live Gitea): %v", err)
	}

	var tmpl OrgTemplate
	if err := yaml.Unmarshal(out, &tmpl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Walk the workspace tree, collect names + check files_dir paths.
	flat := []OrgWorkspace{}
	var walk func([]OrgWorkspace)
	walk = func(ws []OrgWorkspace) {
		for _, w := range ws {
			flat = append(flat, w)
			walk(w.Children)
		}
	}
	walk(tmpl.Workspaces)

	t.Logf("workspaces resolved through !external: %d", len(flat))
	if len(flat) < 25 {
		t.Errorf("expected ~28 dev-tree workspaces via !external; got %d", len(flat))
	}

	// Sentinel checks — same as TestLocalE2E_DevDepartmentExtraction
	// (Q1+Q2 placements verified).
	expected := []string{
		"Dev Lead",
		"Core Platform Lead",
		"Controlplane Lead",
		"App & Docs Lead",
		"Documentation Specialist", // Q1
		"Triage Operator",          // Q2
	}
	found := map[string]bool{}
	for _, w := range flat {
		found[w.Name] = true
	}
	for _, want := range expected {
		if !found[want] {
			t.Errorf("missing expected workspace %q", want)
		}
	}

	// Every workspace's files_dir must be cache-prefixed (proves the
	// path-rewrite ran end-to-end).
	cachePrefix := ".external-cache"
	for _, w := range flat {
		if w.FilesDir == "" {
			continue
		}
		if !strings.HasPrefix(w.FilesDir, cachePrefix) {
			t.Errorf("workspace %q files_dir %q missing cache prefix %q", w.Name, w.FilesDir, cachePrefix)
		}
	}

	// Verify the fetched cache exists and resolveInsideRoot accepts
	// every workspace's files_dir (would cause provisioning to fail
	// if not).
	for _, w := range flat {
		if w.FilesDir == "" {
			continue
		}
		abs, err := resolveInsideRoot(parent, w.FilesDir)
		if err != nil {
			t.Errorf("workspace %q files_dir %q: resolveInsideRoot: %v", w.Name, w.FilesDir, err)
			continue
		}
		info, err := os.Stat(abs)
		if err != nil {
			t.Errorf("workspace %q: stat %q: %v", w.Name, abs, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("workspace %q files_dir %q is not a directory", w.Name, w.FilesDir)
		}
	}
}
