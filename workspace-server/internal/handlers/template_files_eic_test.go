package handlers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestResolveWorkspaceFilePath_RuntimeIndirection pins the
// `?root="/configs"` (or empty / unrecognized) → runtime managed-config
// dir behavior. Hermes uses /home/ubuntu/.hermes; claude-code uses
// /configs; unknowns fall back to /configs. This indirection is the
// reason hermes Config-tab edits land in the right place even though
// the canvas only ever sends `?root=/configs`. Changing it without a
// migration shim silently orphans previously-saved files.
func TestResolveWorkspaceFilePath_RuntimeIndirection(t *testing.T) {
	cases := []struct {
		runtime string
		root    string
		relPath string
		want    string
	}{
		{"hermes", "/configs", "config.yaml", "/home/ubuntu/.hermes/config.yaml"},
		{"HERMES", "/configs", "config.yaml", "/home/ubuntu/.hermes/config.yaml"}, // case-insensitive
		{"hermes", "/configs", "nested/a.yaml", "/home/ubuntu/.hermes/nested/a.yaml"},
		{"hermes", "", "config.yaml", "/home/ubuntu/.hermes/config.yaml"},     // empty root → runtime indirection
		{"hermes", "/etc", "config.yaml", "/home/ubuntu/.hermes/config.yaml"}, // out-of-allowlist → runtime indirection
		// claude-code (and any future containerized runtime) lands at /configs —
		// the path user-data creates and bind-mounts into the container. Pre-fix
		// this fell through to /opt/configs which doesn't exist on workspace EC2s
		// and would 500 with EACCES on save (the bug that motivated this gate).
		{"claude-code", "/configs", "config.yaml", "/configs/config.yaml"},
		{"CLAUDE-CODE", "/configs", "config.yaml", "/configs/config.yaml"}, // case-insensitive
		{"external", "/configs", "skills.json", "/opt/configs/skills.json"},
		{"", "/configs", "config.yaml", "/configs/config.yaml"},        // empty runtime → default
		{"unknown", "/configs", "config.yaml", "/configs/config.yaml"}, // unknown → default
	}
	for _, tc := range cases {
		t.Run(tc.runtime+"+"+tc.root+"/"+tc.relPath, func(t *testing.T) {
			got, err := resolveWorkspaceFilePath(tc.runtime, tc.root, tc.relPath)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveWorkspaceFilePath(%q,%q,%q) = %q, want %q",
					tc.runtime, tc.root, tc.relPath, got, tc.want)
			}
		})
	}
}

// TestResolveWorkspaceFilePath_LiteralRoots pins that the universal
// allow-listed roots (`/home`, `/workspace`, `/plugins`) pass through
// LITERALLY rather than getting rewritten to the runtime prefix. This
// is the half of the resolver that the FilesTab "/home" selector
// depends on — without it, picking /home on a hermes workspace would
// route to /home/ubuntu/.hermes (the runtime indirection) and the
// canvas's tree row would never line up with what the user sees on
// the EC2 host.
func TestResolveWorkspaceFilePath_LiteralRoots(t *testing.T) {
	cases := []struct {
		runtime string
		root    string
		relPath string
		want    string
	}{
		// /home is always literal regardless of runtime — it's a
		// universal Linux path, not a managed-config indirection.
		{"hermes", "/home", "ubuntu/.bashrc", "/home/ubuntu/.bashrc"},
		{"claude-code", "/home", "ubuntu/notes.md", "/home/ubuntu/notes.md"},
		{"codex", "/home", "ubuntu/x", "/home/ubuntu/x"},
		// /workspace and /plugins are also literal — runtime is ignored.
		{"hermes", "/workspace", "src/main.go", "/workspace/src/main.go"},
		{"claude-code", "/plugins", "p/manifest.yaml", "/plugins/p/manifest.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.runtime+"+"+tc.root+"/"+tc.relPath, func(t *testing.T) {
			got, err := resolveWorkspaceFilePath(tc.runtime, tc.root, tc.relPath)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveWorkspaceFilePath(%q,%q,%q) = %q, want %q",
					tc.runtime, tc.root, tc.relPath, got, tc.want)
			}
		})
	}
}

// TestResolveWorkspaceRootPath pins the directory-only translation
// used by listFilesViaEIC. Same indirection rules as
// resolveWorkspaceFilePath but without joining a relative path.
func TestResolveWorkspaceRootPath(t *testing.T) {
	cases := []struct {
		runtime string
		root    string
		want    string
	}{
		{"hermes", "/configs", "/home/ubuntu/.hermes"},
		{"claude-code", "/configs", "/configs"},
		{"hermes", "", "/home/ubuntu/.hermes"},
		{"hermes", "/home", "/home"},
		{"claude-code", "/workspace", "/workspace"},
		{"hermes", "/plugins", "/plugins"},
		{"unknown", "/configs", "/configs"},
		{"hermes", "/etc", "/home/ubuntu/.hermes"}, // not allowlisted → runtime indirection
	}
	for _, tc := range cases {
		t.Run(tc.runtime+"+"+tc.root, func(t *testing.T) {
			got := resolveWorkspaceRootPath(tc.runtime, tc.root)
			if got != tc.want {
				t.Errorf("resolveWorkspaceRootPath(%q,%q) = %q, want %q",
					tc.runtime, tc.root, got, tc.want)
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
		"../etc/shadow", // escapes base via ..
		"/etc/shadow",   // absolute path
		"./../../etc",   // multiple ..
		"a/../../etc",   // escapes via deeper ..
	}
	for _, rel := range bad {
		t.Run(rel, func(t *testing.T) {
			_, err := resolveWorkspaceFilePath("hermes", "/configs", rel)
			if err == nil {
				t.Errorf("resolveWorkspaceFilePath(hermes,/configs,%q) should have errored, got nil", rel)
			}
		})
	}
}

// TestSSHArgs_HardenedFlags pins the ssh option set returned by
// eicSSHSession.sshArgs(). Centralising the args was deliberate so a
// fix like PR #2822's `LogLevel=ERROR` (silences the benign
// known-hosts warning that fooled the read/list "empty stderr → not
// found" classifier) only needs to land in one place.
//
// Caught 2026-05-05 02:38 on hongming.moleculesai.app: opening
// Hermes workspace's Config tab returned 500 with body
// `ssh cat: exit status 1 (Warning: Permanently added '[127.0.0.1]:37951'…)`.
//
// Asserts each load-bearing flag appears in the args slice — fires if
// a future edit removes any of them.
func TestSSHArgs_HardenedFlags(t *testing.T) {
	s := eicSSHSession{keyPath: "/tmp/k", localPort: 12345, osUser: "ubuntu", instanceID: "i-x"}
	got := s.sshArgs("echo hi")
	wantFragments := [][]string{
		{"-i", "/tmp/k"},
		{"-o", "StrictHostKeyChecking=no"},
		{"-o", "UserKnownHostsFile=/dev/null"},
		{"-o", "LogLevel=ERROR"},
		{"-o", "ServerAliveInterval=15"},
		{"-p", "12345"},
	}
	joined := strings.Join(got, " ")
	for _, frag := range wantFragments {
		if !strings.Contains(joined, strings.Join(frag, " ")) {
			t.Errorf("sshArgs() missing fragment %v; got: %v", frag, got)
		}
	}
	// Last two args must be `<user>@127.0.0.1` then the remote command.
	if got[len(got)-2] != "ubuntu@127.0.0.1" {
		t.Errorf("sshArgs() second-last must be user@127.0.0.1; got: %q", got[len(got)-2])
	}
	if got[len(got)-1] != "echo hi" {
		t.Errorf("sshArgs() last must be the remote command; got: %q", got[len(got)-1])
	}
}

// TestEicSSHSessionSingleSourceForSSHFlags is a structural guard: the
// production EIC source must invoke s.sshArgs() exclusively for ssh
// invocations — direct ssh args inlined in any helper would re-open
// the regression that PR #2822 closed (LogLevel=ERROR drift between
// helpers). Counts `s.sshArgs(` occurrences (one per file op) and
// fails if anyone copy-pastes a raw ssh args slice.
func TestEicSSHSessionSingleSourceForSSHFlags(t *testing.T) {
	src, err := os.ReadFile("template_files_eic.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	// Each of write/read/list/delete should call s.sshArgs once.
	matches := regexp.MustCompile(`s\.sshArgs\(`).FindAllIndex(src, -1)
	if len(matches) < 4 {
		t.Errorf("expected ≥4 s.sshArgs() callers (write/read/list/delete); found %d", len(matches))
	}
	// Belt and braces: no helper should be assembling its own
	// `LogLevel=ERROR` literal outside of sshArgs.
	literal := regexp.MustCompile(`"-o", "LogLevel=ERROR"`).FindAllIndex(src, -1)
	if len(literal) != 1 {
		t.Errorf("LogLevel=ERROR must appear exactly once (in sshArgs); found %d occurrences — drift risk", len(literal))
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
