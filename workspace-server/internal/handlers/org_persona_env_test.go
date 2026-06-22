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
		"GITEA_USER":         "dev-lead",
		"GITEA_USER_EMAIL":   "dev-lead@agents.moleculesai.app",
		"GITEA_TOKEN":        "abc123",
		"GITEA_TOKEN_SCOPES": "write:repository,write:issue,read:user",
		"GITEA_SSH_KEY_PATH": "/etc/molecule-bootstrap/personas/dev-lead/ssh_priv",
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
	// trailing-hyphen IS allowed; only include actually-bad names:
	bad := []string{
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

// TestLoadPersonaTokenFile_TokenOnlyPersona: the prod-team personas
// (agent-dev-a / agent-dev-b / agent-pm) ship `token` only — no `env`
// file. loadPersonaEnvFile's fallback path must populate GITEA_TOKEN /
// GITEA_USER / GITEA_USER_EMAIL from the token contents + role name so
// the GIT_ASKPASS helper has something to emit.
func TestLoadPersonaTokenFile_TokenOnlyPersona(t *testing.T) {
	root := t.TempDir()
	roleDir := filepath.Join(root, "agent-dev-a")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "token"),
		[]byte("token-bytes-redacted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	out := map[string]string{}
	loadPersonaEnvFile("agent-dev-a", out)

	want := map[string]string{
		"GITEA_TOKEN":      "token-bytes-redacted",
		"GITEA_USER":       "agent-dev-a",
		"GITEA_USER_EMAIL": "agent-dev-a@" + gitIdentityEmailDomain,
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

// TestLoadPersonaTokenFile_EnvFileWins: when BOTH an env file and a
// token file exist in the same persona dir, the env file is the more-
// specific declaration and wins outright — the fallback must not fire
// at all. This pins precedence so a persona later migrated to the
// richer env-file form (carrying GITEA_TOKEN_SCOPES / GITEA_SSH_KEY_PATH)
// doesn't get its token silently overridden by the fallback.
func TestLoadPersonaTokenFile_EnvFileWins(t *testing.T) {
	root := t.TempDir()
	roleDir := filepath.Join(root, "agent-dev-b")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	envBody := "GITEA_USER=env-form-user\nGITEA_TOKEN=env-form-token\n" +
		"GITEA_USER_EMAIL=env-form@example.invalid\nGITEA_TOKEN_SCOPES=write:repository\n"
	if err := os.WriteFile(filepath.Join(roleDir, "env"), []byte(envBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "token"),
		[]byte("token-form-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	out := map[string]string{}
	loadPersonaEnvFile("agent-dev-b", out)

	if out["GITEA_USER"] != "env-form-user" {
		t.Errorf("env file should win for GITEA_USER; got %q", out["GITEA_USER"])
	}
	if out["GITEA_TOKEN"] != "env-form-token" {
		t.Errorf("env file should win for GITEA_TOKEN; got %q", out["GITEA_TOKEN"])
	}
	if out["GITEA_USER_EMAIL"] != "env-form@example.invalid" {
		t.Errorf("env file should win for GITEA_USER_EMAIL; got %q", out["GITEA_USER_EMAIL"])
	}
	if out["GITEA_TOKEN_SCOPES"] != "write:repository" {
		t.Errorf("env file extras must be preserved; got GITEA_TOKEN_SCOPES=%q", out["GITEA_TOKEN_SCOPES"])
	}
}

// TestLoadPersonaTokenFile_NeitherFile: persona dir exists but ships
// neither env nor token — silent no-op. This is the legitimate case
// for a partially-provisioned persona during bootstrap; callers expect
// an empty map, no error, no log noise.
func TestLoadPersonaTokenFile_NeitherFile(t *testing.T) {
	root := t.TempDir()
	roleDir := filepath.Join(root, "agent-pm")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	out := map[string]string{}
	loadPersonaEnvFile("agent-pm", out)
	if len(out) != 0 {
		t.Errorf("expected empty out when neither env nor token exists; got %#v", out)
	}
}

// TestLoadPersonaTokenFile_EmptyToken: a token file with only
// whitespace must be treated as absent — never emit
// GITEA_TOKEN="" / GITEA_USER=<role> / GITEA_USER_EMAIL=<role>@... because
// that would set GITEA_USER without a usable token, and the askpass
// helper would then prompt with an empty password. Silent no-op is the
// correct behavior — let downstream auth fall through to its existing
// "no credentials available" path.
func TestLoadPersonaTokenFile_EmptyToken(t *testing.T) {
	root := t.TempDir()
	roleDir := filepath.Join(root, "agent-dev-a")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Whitespace-only contents: spaces, tabs, newlines.
	if err := os.WriteFile(filepath.Join(roleDir, "token"),
		[]byte("   \t\n  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	out := map[string]string{}
	loadPersonaEnvFile("agent-dev-a", out)
	if len(out) != 0 {
		t.Errorf("expected empty out when token file is whitespace-only; got %#v", out)
	}
}

// TestLoadPersonaTokenFile_TrimsWhitespace: tokens shipped from the
// operator-host bootstrap kit may have a trailing newline (the
// canonical `printf "%s\n" "$token" > token` shape). The fallback must
// trim leading + trailing whitespace so the askpass helper emits the
// raw token bytes — Gitea's PAT validator rejects tokens with embedded
// whitespace.
func TestLoadPersonaTokenFile_TrimsWhitespace(t *testing.T) {
	root := t.TempDir()
	roleDir := filepath.Join(root, "agent-dev-b")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "token"),
		[]byte("\n  raw-token-bytes  \n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	out := map[string]string{}
	loadPersonaEnvFile("agent-dev-b", out)
	if out["GITEA_TOKEN"] != "raw-token-bytes" {
		t.Errorf("token whitespace not trimmed; got %q", out["GITEA_TOKEN"])
	}
}

// TestLoadPersonaTokenFile_RejectsUnsafeRole: defense-in-depth — even
// in the fallback path, role names that fail isSafeRoleName must not
// touch the filesystem. Mirrors TestLoadPersonaEnvFile_RejectsTraversal.
func TestLoadPersonaTokenFile_RejectsUnsafeRole(t *testing.T) {
	root := t.TempDir()
	// Plant a token at /tmp/.../token so a bad traversal would reach it.
	if err := os.WriteFile(filepath.Join(root, "token"),
		[]byte("stolen-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", filepath.Join(root, "personas"))

	for _, bad := range []string{"..", "../personas", "/abs", "with/slash", "."} {
		out := map[string]string{}
		loadPersonaTokenFile(bad, out)
		if len(out) != 0 {
			t.Errorf("role %q should have been rejected; got %#v", bad, out)
		}
	}
}

// TestLoadPersonaTokenFile_NilMapSafe: callers pass a fresh map in
// practice, but defense-in-depth — a nil map must not panic.
func TestLoadPersonaTokenFile_NilMapSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil map caused panic: %v", r)
		}
	}()
	loadPersonaTokenFile("agent-dev-a", nil)
}
