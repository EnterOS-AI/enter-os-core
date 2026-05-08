package handlers

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadPersonaEnvFile_HappyPath: the standard case — a persona-shaped
// env file exists at <root>/<role>/env and its KEY=VALUE pairs land in
// the out map. Mirrors what the operator-host bootstrap kit ships:
// GITEA_USER, GITEA_TOKEN, GITEA_TOKEN_SCOPES, GITEA_USER_EMAIL,
// GITEA_SSH_KEY_PATH.
func TestLoadPersonaEnvFile_HappyPath(t *testing.T) {
	root := t.TempDir()
	roleDir := filepath.Join(root, "dev-lead")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	envBody := `# Persona env file — mode 600
GITEA_USER=dev-lead
GITEA_USER_EMAIL=dev-lead@agents.moleculesai.app
GITEA_TOKEN=abc123
GITEA_TOKEN_SCOPES=write:repository,write:issue,read:user
GITEA_SSH_KEY_PATH=/etc/molecule-bootstrap/personas/dev-lead/ssh_priv
`
	if err := os.WriteFile(filepath.Join(roleDir, "env"), []byte(envBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	out := map[string]string{}
	loadPersonaEnvFile("dev-lead", out)

	want := map[string]string{
		"GITEA_USER":           "dev-lead",
		"GITEA_USER_EMAIL":     "dev-lead@agents.moleculesai.app",
		"GITEA_TOKEN":          "abc123",
		"GITEA_TOKEN_SCOPES":   "write:repository,write:issue,read:user",
		"GITEA_SSH_KEY_PATH":   "/etc/molecule-bootstrap/personas/dev-lead/ssh_priv",
	}
	if len(out) != len(want) {
		t.Fatalf("got %d keys, want %d: %#v", len(out), len(want), out)
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("out[%q] = %q; want %q", k, out[k], v)
		}
	}
}

// TestLoadPersonaEnvFile_MissingDir: when the persona dir doesn't exist
// (e.g. dev-only host without the bootstrap kit, or a workspace whose
// role isn't a known persona), it's a silent no-op — out stays empty,
// no panic, no log noise that would break callers.
func TestLoadPersonaEnvFile_MissingDir(t *testing.T) {
	t.Setenv("MOLECULE_PERSONA_ROOT", t.TempDir()) // empty dir
	out := map[string]string{}
	loadPersonaEnvFile("nonexistent-role", out)
	if len(out) != 0 {
		t.Errorf("expected empty out, got %#v", out)
	}
}

// TestLoadPersonaEnvFile_EmptyRole: empty role string is the common case
// for non-dev workspaces (research/marketing/etc.). Skip silently.
func TestLoadPersonaEnvFile_EmptyRole(t *testing.T) {
	t.Setenv("MOLECULE_PERSONA_ROOT", t.TempDir())
	out := map[string]string{}
	loadPersonaEnvFile("", out)
	if len(out) != 0 {
		t.Errorf("empty role should produce empty out; got %#v", out)
	}
}

// TestLoadPersonaEnvFile_RejectsTraversal: even though role names come
// from server-side admin-only org templates, defense-in-depth — refuse
// any role string with path separators or "..". Verifies that a maliciously
// crafted template can't read /etc/passwd by setting role: "../../etc".
func TestLoadPersonaEnvFile_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	// Plant a file at /tmp/.../env so a bad traversal would reach it
	if err := os.WriteFile(filepath.Join(root, "env"), []byte("STOLEN=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", filepath.Join(root, "personas"))

	for _, bad := range []string{"..", "../personas", "../etc/passwd", "/abs", "with/slash", "dot.in.middle", "with space", "back\\slash", ".", ""} {
		out := map[string]string{}
		loadPersonaEnvFile(bad, out)
		if len(out) != 0 {
			t.Errorf("role %q should have been rejected; got %#v", bad, out)
		}
	}
}

// TestLoadPersonaEnvFile_DefaultRoot: when MOLECULE_PERSONA_ROOT is unset,
// the helper falls back to /etc/molecule-bootstrap/personas. We don't
// touch real /etc — just verify the function doesn't panic and produces
// empty out (since the test box isn't expected to ship that path).
func TestLoadPersonaEnvFile_DefaultRoot(t *testing.T) {
	t.Setenv("MOLECULE_PERSONA_ROOT", "") // explicit empty
	out := map[string]string{}
	loadPersonaEnvFile("dev-lead", out)
	// Don't assert content — production CI might or might not have the
	// /etc dir mounted. Just verify the call returns cleanly.
	_ = out
}

// TestLoadPersonaEnvFile_PrecedenceCallerOverrides: the contract is "lower
// precedence than later .env files." The helper writes into out without
// removing existing keys, so a caller pre-populating out simulates a
// later layer overriding persona defaults. We verify the helper does NOT
// clobber pre-existing entries… actually, parseEnvFile DOES overwrite,
// so the caller-side ordering (persona → org → workspace) is what enforces
// precedence. This test pins that contract: persona is loaded into a
// fresh map, then later layers can override.
func TestLoadPersonaEnvFile_OverwritesEmptyMap(t *testing.T) {
	root := t.TempDir()
	roleDir := filepath.Join(root, "core-be")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "env"),
		[]byte("GITEA_TOKEN=persona-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	out := map[string]string{"GITEA_TOKEN": "preset"}
	loadPersonaEnvFile("core-be", out)

	// Persona helper is meant to populate a FRESH map first in the
	// caller's flow; calling it on a pre-populated map and seeing the
	// value get overwritten is consistent with parseEnvFile semantics.
	if out["GITEA_TOKEN"] != "persona-value" {
		t.Errorf("loadPersonaEnvFile did not write into existing map; got %q", out["GITEA_TOKEN"])
	}
}

// TestIsSafeRoleName_Acceptance: positive + negative cases for the
// validator. Pinned because every dev-tree role name must pass.
func TestIsSafeRoleName_Acceptance(t *testing.T) {
	good := []string{
		"dev-lead", "core-be", "cp-security", "infra-runtime-be",
		"sdk-dev", "plugin-dev", "documentation-specialist",
		"triage-operator", "fullstack-engineer", "release-manager",
		"core_underscore_ok", "X", "a1", "Z9-0",
	}
	for _, s := range good {
		if !isSafeRoleName(s) {
			t.Errorf("isSafeRoleName(%q) = false; want true", s)
		}
	}
	bad := []string{
		"", ".", "..", "with/slash", "/abs", "dot.in.middle",
		"with space", "back\\slash", "trailing-", // trailing-hyphen is fine actually
		"with$dollar", "with?question", "newline\nsplit",
	}
	// trailing-hyphen IS allowed; remove from "bad" list:
	bad = []string{
		"", ".", "..", "with/slash", "/abs", "dot.in.middle",
		"with space", "back\\slash", "with$dollar", "with?question",
		"newline\nsplit",
	}
	for _, s := range bad {
		if isSafeRoleName(s) {
			t.Errorf("isSafeRoleName(%q) = true; want false", s)
		}
	}
}
