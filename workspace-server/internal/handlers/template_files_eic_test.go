package handlers

import (
	"strings"
	"testing"
)

// TestResolveWorkspaceFilePath_KnownRuntimes — the runtime → base-path
// map is the source of truth for where saved files land on the workspace
// EC2. Changing a base path without a migration shim silently orphans
// previously-saved files; this test pins the current contract.
func TestResolveWorkspaceFilePath_KnownRuntimes(t *testing.T) {
	cases := []struct {
		runtime string
		relPath string
		want    string
	}{
		{"hermes", "config.yaml", "/home/ubuntu/.hermes/config.yaml"},
		{"HERMES", "config.yaml", "/home/ubuntu/.hermes/config.yaml"}, // case-insensitive
		{"hermes", "nested/a.yaml", "/home/ubuntu/.hermes/nested/a.yaml"},
		// claude-code (and any future containerized runtime) lands at /configs —
		// the path user-data creates and bind-mounts into the container. Pre-fix
		// this fell through to /opt/configs which doesn't exist on workspace EC2s
		// and would 500 with EACCES on save (the bug that motivated this gate).
		{"claude-code", "config.yaml", "/configs/config.yaml"},
		{"CLAUDE-CODE", "config.yaml", "/configs/config.yaml"}, // case-insensitive
		{"langgraph", "config.yaml", "/opt/configs/config.yaml"},
		{"external", "skills.json", "/opt/configs/skills.json"},
		{"", "config.yaml", "/configs/config.yaml"},        // empty → default
		{"unknown", "config.yaml", "/configs/config.yaml"}, // unknown → default
	}
	for _, tc := range cases {
		t.Run(tc.runtime+"/"+tc.relPath, func(t *testing.T) {
			got, err := resolveWorkspaceFilePath(tc.runtime, tc.relPath)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveWorkspaceFilePath(%q,%q) = %q, want %q",
					tc.runtime, tc.relPath, got, tc.want)
			}
		})
	}
}

// TestResolveWorkspaceFilePath_RejectsTraversal — any attempt to escape
// the runtime base path via .. or absolute paths must return an error
// before the ssh install runs. validateRelPath uses filepath.Clean then
// checks for `..` or absolute prefix, so cases like `a/../b` are
// NORMALIZED to `b` and accepted (still safe — stays inside base).
// We only assert the cases that Clean() can't rescue.
func TestResolveWorkspaceFilePath_RejectsTraversal(t *testing.T) {
	bad := []string{
		"../etc/shadow",   // escapes base via ..
		"/etc/shadow",     // absolute path
		"./../../etc",     // multiple ..
		"a/../../etc",     // escapes via deeper ..
	}
	for _, rel := range bad {
		t.Run(rel, func(t *testing.T) {
			_, err := resolveWorkspaceFilePath("hermes", rel)
			if err == nil {
				t.Errorf("resolveWorkspaceFilePath(hermes, %q) should have errored, got nil", rel)
			}
		})
	}
}

// TestShellQuote — the sole piece of variable data in the remote ssh
// command is the absolute path. It's already built from a map + Clean()
// so traversal is impossible, but we still single-quote as defence-in-
// depth. Verify the shell-quoting helper handles the single-quote edge
// case and is always wrapped in single quotes.
func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/home/ubuntu/.hermes/config.yaml": "'/home/ubuntu/.hermes/config.yaml'",
		"":                                 "''",
		"a'b":                              `'a'\''b'`,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got := shellQuote(in)
			if got != want {
				t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
			}
			if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
				t.Errorf("shellQuote(%q) = %q must be single-quote wrapped", in, got)
			}
		})
	}
}
