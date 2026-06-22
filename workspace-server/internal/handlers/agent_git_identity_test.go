package handlers

import (
	"os"
	"path/filepath"
	"testing"
)

// applyAgentGitIdentity is the platform-level chokepoint for per-agent
// commit authorship. These tests pin the generated name/email format
// and the operator-override semantics (workspace_secrets wins).

func TestApplyAgentGitIdentity_FillsFourVars(t *testing.T) {
	env := map[string]string{}
	applyAgentGitIdentity(env, "Frontend Engineer")

	cases := map[string]string{
		"GIT_AUTHOR_NAME":     "Molecule AI Frontend Engineer",
		"GIT_AUTHOR_EMAIL":    "frontend-engineer@agents.moleculesai.app",
		"GIT_COMMITTER_NAME":  "Molecule AI Frontend Engineer",
		"GIT_COMMITTER_EMAIL": "frontend-engineer@agents.moleculesai.app",
	}
	for k, want := range cases {
		if got := env[k]; got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}
}

func TestApplyAgentGitIdentity_RespectsOperatorOverride(t *testing.T) {
	// If a workspace_secret already provides GIT_AUTHOR_NAME (the secret
	// loader runs before us), that operator intent wins. We only fill in
	// what isn't already set.
	env := map[string]string{
		"GIT_AUTHOR_NAME":  "Custom Name",
		"GIT_AUTHOR_EMAIL": "custom@example.com",
	}
	applyAgentGitIdentity(env, "Backend Engineer")

	if env["GIT_AUTHOR_NAME"] != "Custom Name" {
		t.Errorf("GIT_AUTHOR_NAME should not be overwritten, got %q", env["GIT_AUTHOR_NAME"])
	}
	if env["GIT_AUTHOR_EMAIL"] != "custom@example.com" {
		t.Errorf("GIT_AUTHOR_EMAIL should not be overwritten, got %q", env["GIT_AUTHOR_EMAIL"])
	}
	// The COMMITTER pair wasn't pre-set, so defaults fill it in.
	if env["GIT_COMMITTER_NAME"] != "Molecule AI Backend Engineer" {
		t.Errorf("GIT_COMMITTER_NAME should be filled, got %q", env["GIT_COMMITTER_NAME"])
	}
}

func TestApplyAgentGitIdentity_EmptyNameIsNoop(t *testing.T) {
	// A provisioning glitch where the workspace name arrived empty
	// shouldn't emit `unknown@agents.moleculesai.app` — those commits
	// are worse than no identity at all (they look like a real misconfig
	// rather than a recoverable state).
	env := map[string]string{}
	applyAgentGitIdentity(env, "")
	if len(env) != 0 {
		t.Errorf("empty name should leave env untouched, got %v", env)
	}
	// Whitespace-only name also counts as empty.
	applyAgentGitIdentity(env, "   ")
	if len(env) != 0 {
		t.Errorf("whitespace name should leave env untouched, got %v", env)
	}
}

func TestApplyAgentGitIdentity_NilMapIsSafe(t *testing.T) {
	// Defensive: never panic on a nil map (buildProvisionerConfig signature
	// doesn't guarantee non-nil). Tests the explicit nil-check.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("applyAgentGitIdentity panicked on nil map: %v", r)
		}
	}()
	applyAgentGitIdentity(nil, "PM")
}

func TestApplyAgentGitIdentity_SetsGitAskpass(t *testing.T) {
	// GIT_ASKPASS is what wires container-side HTTPS git auth to the
	// persona credentials (GITEA_USER/GITEA_TOKEN, etc.) that
	// loadPersonaEnvFile delivers via workspace_secrets. Without this,
	// `git push` inside the container would fall through to interactive
	// prompts (impossible) or a missing credential.helper (401).
	env := map[string]string{}
	applyAgentGitIdentity(env, "Frontend Engineer")
	if env["GIT_ASKPASS"] != "/usr/local/bin/molecule-askpass" {
		t.Errorf("GIT_ASKPASS: got %q, want %q",
			env["GIT_ASKPASS"], "/usr/local/bin/molecule-askpass")
	}
}

func TestApplyAgentGitIdentity_RespectsAskpassOverride(t *testing.T) {
	// A workspace_secret or env-mutator plugin must be able to point at
	// a custom askpass helper without us clobbering it. Symmetric with
	// the GIT_AUTHOR_NAME override test above.
	env := map[string]string{
		"GIT_ASKPASS": "/opt/custom/askpass",
	}
	applyAgentGitIdentity(env, "Backend Engineer")
	if env["GIT_ASKPASS"] != "/opt/custom/askpass" {
		t.Errorf("GIT_ASKPASS should not be overwritten, got %q", env["GIT_ASKPASS"])
	}
}

func TestApplyAgentGitIdentity_AskpassSkippedOnEmptyName(t *testing.T) {
	// The empty-name early-return covers GIT_ASKPASS too — a provisioning
	// glitch that dropped the workspace name shouldn't half-configure the
	// container (identity vars empty but askpass wired). All-or-nothing.
	env := map[string]string{}
	applyAgentGitIdentity(env, "")
	if _, ok := env["GIT_ASKPASS"]; ok {
		t.Errorf("empty name should not set GIT_ASKPASS, got %q", env["GIT_ASKPASS"])
	}
}

func TestApplyGitAskpass_NilMapIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("applyGitAskpass panicked on nil map: %v", r)
		}
	}()
	applyGitAskpass(nil)
}

// TestApplyAgentGitHTTPCreds_HappyPath: the prod-team shape — a persona
// dir at /etc/molecule-bootstrap/personas/<role>/token ships a write
// token. applyAgentGitHTTPCreds reads it and emits both the
// askpass-preferred GIT_HTTP_* pair and the GITEA_* fallback.
func TestApplyAgentGitHTTPCreds_HappyPath(t *testing.T) {
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

	env := map[string]string{}
	applyAgentGitHTTPCreds(env, "agent-dev-a")

	cases := map[string]string{
		"GIT_HTTP_USERNAME": "agent-dev-a",
		"GIT_HTTP_PASSWORD": "token-bytes-redacted",
		"GITEA_USER":        "agent-dev-a",
		"GITEA_TOKEN":       "token-bytes-redacted",
	}
	for k, want := range cases {
		if got := env[k]; got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}
}

// TestApplyAgentGitHTTPCreds_TrimsWhitespace: bootstrap-kit-written
// token files canonically end in \n. Must trim like loadPersonaTokenFile
// does — Gitea PAT validator rejects embedded whitespace.
func TestApplyAgentGitHTTPCreds_TrimsWhitespace(t *testing.T) {
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

	env := map[string]string{}
	applyAgentGitHTTPCreds(env, "agent-dev-b")
	if env["GIT_HTTP_PASSWORD"] != "raw-token-bytes" {
		t.Errorf("GIT_HTTP_PASSWORD: token whitespace not trimmed; got %q", env["GIT_HTTP_PASSWORD"])
	}
}

// TestApplyAgentGitHTTPCreds_RespectsOperatorOverride: if a workspace
// secret (loaded earlier by loadWorkspaceSecrets) already set the
// askpass pair, those values must win — operator intent ranks above
// persona-file defaults. Symmetric with applyAgentGitIdentity's
// GIT_AUTHOR_* override semantics.
func TestApplyAgentGitHTTPCreds_RespectsOperatorOverride(t *testing.T) {
	root := t.TempDir()
	roleDir := filepath.Join(root, "agent-dev-a")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "token"),
		[]byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	env := map[string]string{
		"GIT_HTTP_USERNAME": "operator-user",
		"GIT_HTTP_PASSWORD": "operator-secret",
	}
	applyAgentGitHTTPCreds(env, "agent-dev-a")

	if env["GIT_HTTP_USERNAME"] != "operator-user" {
		t.Errorf("GIT_HTTP_USERNAME should not be overwritten, got %q", env["GIT_HTTP_USERNAME"])
	}
	if env["GIT_HTTP_PASSWORD"] != "operator-secret" {
		t.Errorf("GIT_HTTP_PASSWORD should not be overwritten, got %q", env["GIT_HTTP_PASSWORD"])
	}
	// Fallback pair was not pre-set, so persona-file fills it in.
	if env["GITEA_TOKEN"] != "file-token" {
		t.Errorf("GITEA_TOKEN fallback should be filled, got %q", env["GITEA_TOKEN"])
	}
}

// TestApplyAgentGitHTTPCreds_EmptyKeyIsNoop: a workspace with an empty
// payload.Role (descriptive multi-word role, or no role) must take the
// silent-no-op branch — no FS read, no env keys touched.
func TestApplyAgentGitHTTPCreds_EmptyKeyIsNoop(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	env := map[string]string{}
	applyAgentGitHTTPCreds(env, "")
	if len(env) != 0 {
		t.Errorf("empty persona key should leave env untouched, got %v", env)
	}
	applyAgentGitHTTPCreds(env, "   ")
	if len(env) != 0 {
		t.Errorf("whitespace persona key should leave env untouched, got %v", env)
	}
	applyAgentGitHTTPCreds(env, "Frontend Engineer")
	if len(env) != 0 {
		t.Errorf("multi-word descriptive role should leave env untouched (silent no-op via isSafeRoleName), got %v", env)
	}
}

// TestApplyAgentGitHTTPCreds_MissingTokenFile: persona dir exists but
// ships no token (legitimate for read-only personas like agent-pm pre-
// CTO-cred or partially-provisioned bootstrap). Silent no-op — no env
// keys set so first push surfaces "Authentication failed" cleanly
// instead of half-configured creds.
func TestApplyAgentGitHTTPCreds_MissingTokenFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agent-pm"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	env := map[string]string{}
	applyAgentGitHTTPCreds(env, "agent-pm")
	if len(env) != 0 {
		t.Errorf("missing token file should leave env untouched, got %v", env)
	}
}

// TestApplyAgentGitHTTPCreds_EmptyTokenIsNoop: a whitespace-only token
// file (botched bootstrap) must be treated as absent — never emit
// GIT_HTTP_PASSWORD="" because the askpass helper would then return
// empty on the password prompt and git would surface a confusing 401
// rather than a clean "no credentials" state.
func TestApplyAgentGitHTTPCreds_EmptyTokenIsNoop(t *testing.T) {
	root := t.TempDir()
	roleDir := filepath.Join(root, "agent-dev-a")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "token"),
		[]byte("   \t\n  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	env := map[string]string{}
	applyAgentGitHTTPCreds(env, "agent-dev-a")
	if len(env) != 0 {
		t.Errorf("whitespace-only token should leave env untouched, got %v", env)
	}
}

// TestApplyAgentGitHTTPCreds_RejectsUnsafeRole: defense-in-depth — a
// crafted role with path separators / "../" must NOT touch the FS,
// even if a token file exists at the traversed location.
func TestApplyAgentGitHTTPCreds_RejectsUnsafeRole(t *testing.T) {
	root := t.TempDir()
	// Plant a token at <root>/token so a successful traversal would land here.
	if err := os.WriteFile(filepath.Join(root, "token"),
		[]byte("stolen-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", filepath.Join(root, "personas"))

	for _, bad := range []string{"..", "../personas", "/abs", "with/slash", "."} {
		env := map[string]string{}
		applyAgentGitHTTPCreds(env, bad)
		if len(env) != 0 {
			t.Errorf("unsafe role %q must leave env untouched, got %v", bad, env)
		}
	}
}

// TestApplyAgentGitHTTPCreds_NilMapIsSafe: defensive — never panic
// on a nil map. Symmetric with applyAgentGitIdentity's nil-map test.
func TestApplyAgentGitHTTPCreds_NilMapIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("applyAgentGitHTTPCreds panicked on nil map: %v", r)
		}
	}()
	applyAgentGitHTTPCreds(nil, "agent-dev-a")
}

// TestApplyAgentGitHTTPCreds_DefaultPersonaRoot: when
// MOLECULE_PERSONA_ROOT is unset, the helper falls back to
// /etc/molecule-bootstrap/personas — the canonical operator-host path
// per the bootstrap kit shape. We can't write into /etc in a test,
// but we CAN assert the helper takes the silent-no-op branch when
// that real path is absent (the prod-default case on a dev laptop).
func TestApplyAgentGitHTTPCreds_DefaultPersonaRoot(t *testing.T) {
	t.Setenv("MOLECULE_PERSONA_ROOT", "")

	env := map[string]string{}
	applyAgentGitHTTPCreds(env, "agent-dev-a")
	// The /etc/molecule-bootstrap/personas/agent-dev-a/token path
	// almost certainly does not exist on a dev/CI host. The contract
	// here is "silent no-op when token unreadable", not "exact env
	// state" — so we only assert no panic + no half-state pair.
	if _, ok := env["GIT_HTTP_USERNAME"]; ok {
		if _, ok2 := env["GIT_HTTP_PASSWORD"]; !ok2 {
			t.Errorf("USERNAME set without PASSWORD — half-state; got %v", env)
		}
	}
}

func TestSlugifyForEmail(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Frontend Engineer", "frontend-engineer"},
		{"Product Marketing Manager", "product-marketing-manager"},
		{"UIUX Designer", "uiux-designer"},
		{"PM", "pm"},
		{"SEO Growth Analyst", "seo-growth-analyst"},
		{"Social Media Brand", "social-media-brand"},
		// Odd cases: multiple spaces, punctuation, edge hyphens.
		{"  Extra  Spaces  ", "extra-spaces"},
		{"Role (with parens)", "role-with-parens"},
		{"em—dash", "em-dash"},
		{"---weird---", "weird"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := slugifyForEmail(tc.in); got != tc.want {
				t.Errorf("slugifyForEmail(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
