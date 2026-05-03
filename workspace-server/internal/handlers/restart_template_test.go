package handlers

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests for resolveRestartTemplate — the pure helper that implements the
// priority chain documented on the function. Each test builds a minimal
// temp configsDir, fabricates the specific precondition it exercises,
// and asserts (templatePath, configLabel).
//
// The regression this suite locks in: a default restart (no flags) must
// never auto-apply a template that happens to match the workspace name.
// That was the "model reverts on Save+Restart" bug from
// fix/restart-preserves-user-config.

// newTemplateDir makes a templates root with named subdirs, each holding
// a minimal config.yaml so findTemplateByName's dir-scan path has
// something to read. Returns the absolute root.
func newTemplateDir(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		dir := filepath.Join(root, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		cfg := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(cfg, []byte("name: "+n+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", cfg, err)
		}
	}
	return root
}

// TestResolveRestartTemplate_DefaultRestart_PreservesVolume is the
// regression test for the Canvas Save+Restart bug. A workspace named
// "Hermes Agent" normalises to "hermes-agent" — no dir match — but the
// findTemplateByName second pass would also scan config.yaml's `name:`
// field. We seed a template whose config.yaml DOES have the matching
// name, exactly the worst case. Without apply_template, the helper
// MUST still return empty templatePath.
func TestResolveRestartTemplate_DefaultRestart_PreservesVolume(t *testing.T) {
	root := newTemplateDir(t, "hermes")
	// Overwrite config.yaml so the name-scan would hit:
	cfg := filepath.Join(root, "hermes", "config.yaml")
	if err := os.WriteFile(cfg, []byte("name: Hermes Agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, label := resolveRestartTemplate(root, "Hermes Agent", "hermes", restartTemplateInput{
		// ApplyTemplate intentionally omitted — this is the default restart.
	})
	if path != "" {
		t.Errorf("default restart must NOT resolve a template; got path=%q", path)
	}
	if label != "existing-volume" {
		t.Errorf("expected 'existing-volume' label on default restart; got %q", label)
	}
}

// TestResolveRestartTemplate_ExplicitTemplate_AlwaysHonoured verifies
// that passing Template by name works regardless of ApplyTemplate —
// the caller named a template, that's unambiguous consent.
func TestResolveRestartTemplate_ExplicitTemplate_AlwaysHonoured(t *testing.T) {
	root := newTemplateDir(t, "langgraph")

	path, label := resolveRestartTemplate(root, "Some Agent", "", restartTemplateInput{
		Template: "langgraph",
	})
	if path == "" || label != "langgraph" {
		t.Errorf("explicit template must resolve; got path=%q label=%q", path, label)
	}
}

// TestResolveRestartTemplate_ApplyTemplate_NameMatch verifies that
// setting ApplyTemplate re-enables the name-based auto-match for
// operators who actually want "reset this workspace to its template".
func TestResolveRestartTemplate_ApplyTemplate_NameMatch(t *testing.T) {
	root := newTemplateDir(t, "hermes")

	path, label := resolveRestartTemplate(root, "Hermes", "", restartTemplateInput{
		ApplyTemplate: true,
	})
	if path == "" || label != "hermes" {
		t.Errorf("apply_template should name-match; got path=%q label=%q", path, label)
	}
}

// TestResolveRestartTemplate_ApplyTemplate_RuntimeDefault verifies the
// runtime-change flow: when the Canvas Config tab changes the runtime,
// the restart handler needs to lay down the new runtime's base files
// via `<runtime>-default/`. Matches the existing behaviour comment.
func TestResolveRestartTemplate_ApplyTemplate_RuntimeDefault(t *testing.T) {
	root := newTemplateDir(t, "hermes-default")

	path, label := resolveRestartTemplate(root, "Some Workspace", "hermes", restartTemplateInput{
		ApplyTemplate: true,
	})
	if path == "" || label != "hermes-default" {
		t.Errorf("apply_template + dbRuntime should resolve runtime-default; got path=%q label=%q", path, label)
	}
}

// TestResolveRestartTemplate_ApplyTemplate_NoMatch_NoRuntime falls all
// the way through to the reuse-volume path when neither name nor
// runtime-default resolves.
func TestResolveRestartTemplate_ApplyTemplate_NoMatch_NoRuntime(t *testing.T) {
	root := newTemplateDir(t) // empty templates dir

	path, label := resolveRestartTemplate(root, "Orphan", "", restartTemplateInput{
		ApplyTemplate: true,
	})
	if path != "" {
		t.Errorf("nothing to apply → expected empty path; got %q", path)
	}
	if label != "existing-volume" {
		t.Errorf("expected 'existing-volume' fallback; got %q", label)
	}
}

// TestResolveRestartTemplate_InvalidExplicitTemplate_ProceedsWithout
// covers the defensive path where an explicit Template doesn't resolve
// to a valid dir (e.g. traversal attempt, deleted template). The helper
// must log + fall through, not crash or escape the root.
func TestResolveRestartTemplate_InvalidExplicitTemplate_ProceedsWithout(t *testing.T) {
	root := newTemplateDir(t, "langgraph")

	path, label := resolveRestartTemplate(root, "Some Agent", "", restartTemplateInput{
		Template: "../../etc/passwd",
	})
	if path != "" {
		t.Errorf("traversal attempt must not resolve; got %q", path)
	}
	if label != "existing-volume" {
		t.Errorf("expected 'existing-volume' fallback on invalid template; got %q", label)
	}
}

// TestResolveRestartTemplate_NonExistentExplicitTemplate mirrors the
// above but for a syntactically-valid name that simply doesn't exist
// on disk (e.g. template was manually deleted). Must fall through.
func TestResolveRestartTemplate_NonExistentExplicitTemplate(t *testing.T) {
	root := newTemplateDir(t, "langgraph")

	path, label := resolveRestartTemplate(root, "Some Agent", "", restartTemplateInput{
		Template: "deleted-template",
	})
	if path != "" {
		t.Errorf("missing template must not resolve; got %q", path)
	}
	if label != "existing-volume" {
		t.Errorf("expected 'existing-volume' fallback on missing template; got %q", label)
	}
}

// TestResolveRestartTemplate_Priority_ExplicitBeatsApplyTemplate proves
// that an explicit Template takes precedence over a name-based match.
// Scenario: workspace "Hermes" with ApplyTemplate=true + explicit
// Template="langgraph" — caller wants langgraph, not hermes.
func TestResolveRestartTemplate_Priority_ExplicitBeatsApplyTemplate(t *testing.T) {
	root := newTemplateDir(t, "hermes", "langgraph")

	path, label := resolveRestartTemplate(root, "Hermes", "", restartTemplateInput{
		Template:      "langgraph",
		ApplyTemplate: true,
	})
	if label != "langgraph" {
		t.Errorf("explicit Template must win; got label=%q", label)
	}
	// Verify the path is actually inside the langgraph template dir
	expected := filepath.Join(root, "langgraph")
	if path != expected {
		t.Errorf("expected path %q, got %q", expected, path)
	}
}

// TestResolveRestartTemplate_CWE22_TraversalRuntime_FallsThrough is the
// regression test for CWE-22 in Tier 4 of resolveRestartTemplate.
//
// An attacker who holds a workspace token can set the runtime field to a
// path-traversal string (e.g. "../../../etc").  Before the fix, the code
// did:
//   runtimeTemplate := filepath.Join(configsDir, dbRuntime+"-default")
// which on a host with /configs/../../../etc-default would return /etc-default,
// injecting arbitrary host files into the workspace container.
//
// After the fix, sanitizeRuntime is called first.  Unknown runtimes
// (including traversal strings) are remapped to "langgraph".  The attacker
// cannot choose an arbitrary host path — they can at most trigger
// langgraph-default if that template happens to exist.
//
// This test verifies that a traversal string in dbRuntime falls through to
// "existing-volume" when no langgraph-default template is present.
func TestResolveRestartTemplate_CWE22_TraversalRuntime_FallsThrough(t *testing.T) {
	root := newTemplateDir(t) // no template dirs at all

	for _, tc := range []struct {
		name     string
		dbRuntime string
	}{
		{"simple traversal", "../../../etc"},
		{"mid-path traversal", "langgraph/../../../etc"},
		{"absolute-path attempt", "/etc/passwd"},
		{"double-dot chain", "../.."},
		{"deep traversal", "a/b/c/../../../d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, label := resolveRestartTemplate(root, "Some Workspace", tc.dbRuntime, restartTemplateInput{
				ApplyTemplate: true,
			})
			// Must NOT return a path that escapes root
			if path != "" {
				t.Errorf("CWE-22: traversal runtime %q must not resolve; got path=%q", tc.dbRuntime, path)
			}
			if label != "existing-volume" {
				t.Errorf("CWE-22: traversal runtime %q must fall through to existing-volume; got label=%q", tc.dbRuntime, label)
			}
		})
	}
}

// TestResolveRestartTemplate_CWE22_TraversalRuntime_CannotOverrideKnownRuntime
// verifies that even if a langgraph-default template exists, a traversal
// string in dbRuntime resolves langgraph-default (the safe default) rather
// than any attacker-chosen path.  The attacker gains no additional access.
func TestResolveRestartTemplate_CWE22_TraversalRuntime_CannotOverrideKnownRuntime(t *testing.T) {
	root := newTemplateDir(t, "claude-code-default")

	path, label := resolveRestartTemplate(root, "Some Workspace", "../../../etc", restartTemplateInput{
		ApplyTemplate: true,
	})
	// Must resolve to claude-code-default (the safe default after sanitizeRuntime),
	// not to an escaped path
	expected := filepath.Join(root, "claude-code-default")
	if path != expected {
		t.Errorf("traversal runtime must resolve to claude-code-default; got path=%q", path)
	}
	if label != "claude-code-default" {
		t.Errorf("label must be claude-code-default; got %q", label)
	}
}
