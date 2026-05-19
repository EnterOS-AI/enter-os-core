package handlers

// workspace_provision_shared_test.go — architectural test that pins
// the invariant the shared-prepare refactor relies on: every code
// path that provisions a workspace MUST call mintWorkspaceSecrets.
//
// Closes the drift class that produced the 2026-04-30 RFC #2312
// silent-503 bug. Pre-fix: provisionWorkspaceCP forgot to mint
// platform_inbound_secret because the SaaS path was implemented
// after the Docker path and the original mint call wasn't carried
// forward. Both modes now share mintWorkspaceSecrets via this
// extracted helper; this test ensures it stays that way.
//
// Same shape as the audit-coverage gate from #335 (#2343 PR-5).
// If this test fails: either add mintWorkspaceSecrets to the new
// provision function, OR (if the function legitimately should NOT
// mint) add it to provisionExemptFunctions with a one-line
// justification.

import (
	"bytes"
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/gin-gonic/gin"
)

// provisionExemptFunctions are functions that call a provision-start
// method but legitimately do NOT need to mint (e.g. the wrapper
// `provisionWorkspace` which delegates — the delegate mints; the
// re-spawn loops inside Restart that re-enter provisionWorkspaceOpts).
// Add an entry only with a one-line justification.
var provisionExemptFunctions = map[string]string{
	"provisionWorkspace": "thin wrapper that delegates to provisionWorkspaceOpts; the delegate mints",
}

// TestProvisionFunctions_AllCallMintWorkspaceSecrets asserts every
// function in this package that triggers a workspace provision (i.e.
// calls h.provisioner.Start or h.cpProv.Start) ALSO calls
// mintWorkspaceSecrets at least once in the same body.
//
// Behavior-based — drift-resistant. A future provision function with
// any name still trips this gate as long as it calls one of the
// provisioner Start methods. This replaces an earlier name-list
// version (PR #2366) that missed TeamHandler.Expand (issue #2367) —
// the bug that motivated the upgrade.
//
// Same shape as the audit-coverage gate from #335 (#2343 PR-5).
//
// If this test fails: either add mintWorkspaceSecrets to the
// offending function (preferred — usually you should delegate to
// provisionWorkspace via h.wh), OR add it to provisionExemptFunctions
// with a one-line justification.
func TestProvisionFunctions_AllCallMintWorkspaceSecrets(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	type missing struct {
		file string
		line int
		fn   string
	}
	var violations []missing

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if _, exempt := provisionExemptFunctions[fn.Name.Name]; exempt {
				continue
			}
			if !callsProvisionStart(fn.Body) {
				continue
			}
			if !callsMintWorkspaceSecrets(fn.Body) {
				violations = append(violations, missing{
					file: name,
					line: fset.Position(fn.Pos()).Line,
					fn:   fn.Name.Name,
				})
			}
		}
	}

	for _, v := range violations {
		t.Errorf(
			"%s:%d %s calls a provisioner Start (h.provisioner.Start or h.cpProv.Start) but does not call mintWorkspaceSecrets — every provision path MUST mint auth_token + platform_inbound_secret. Prefer delegating to h.wh.provisionWorkspace; only add %q to provisionExemptFunctions with a one-line justification if mint is genuinely inappropriate.",
			v.file, v.line, v.fn, v.fn,
		)
	}
}

// callsProvisionStart reports whether the function body invokes a
// provisioner-start method. Matches `<x>.provisioner.Start(...)` and
// `<x>.cpProv.Start(...)` — both look like
// `<recv>.<provField>.Start(...)` in the AST. Filtering on the
// provisioner-field name (`provisioner` or `cpProv`) keeps the gate
// from tripping on unrelated `.Start()` calls (e.g. http.Server.Start
// in the same package).
func callsProvisionStart(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name != "Start" {
			return true
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch inner.Sel.Name {
		case "provisioner", "cpProv":
			found = true
			return false
		}
		return true
	})
	return found
}

// callsMintWorkspaceSecrets walks the function body and reports
// whether mintWorkspaceSecrets is called anywhere — direct call OR
// via a helper. Recursion to helpers is shallow: we only check
// immediate calls in this function's body. The shared-prepare
// refactor centralizes mint in mintWorkspaceSecrets itself, so a
// direct call at the top-level is the expected pattern.
func callsMintWorkspaceSecrets(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "mintWorkspaceSecrets" {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestMintWorkspaceSecrets_PersistsInboundSecretInSaaSMode is the
// behavioral counterpart to the AST gate. It pins the structural
// fix for the 2026-04-30 silent-503 chat upload bug (RFC #2312):
// even in SaaS mode (where Docker file injection is skipped),
// mintWorkspaceSecrets MUST persist platform_inbound_secret to the
// workspaces row so platform-side handlers can read it back.
//
// Pre-fix: provisionWorkspaceCP never called the mint helper, so
// every prod workspace had NULL platform_inbound_secret →
// chat_files Upload returned 503 with "workspace not yet enrolled
// in v2 upload" on every attempt.
func TestMintWorkspaceSecrets_PersistsInboundSecretInSaaSMode(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")
	mock := setupTestDB(t)

	// First underlying call: revoke any existing live tokens. SaaS
	// mode early-returns from issueAndInjectToken right after this,
	// so IssueToken is NOT expected.
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET revoked_at = now\(\) WHERE workspace_id = \$1 AND revoked_at IS NULL`).
		WithArgs("ws-saas-mint").
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Second underlying call: persist the platform_inbound_secret.
	// The structural fix — without this UPDATE, the bug recurs.
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), "ws-saas-mint").
		WillReturnResult(sqlmock.NewResult(0, 1))

	handler := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	cfg := provisioner.WorkspaceConfig{WorkspaceID: "ws-saas-mint"}
	handler.mintWorkspaceSecrets(context.Background(), "ws-saas-mint", &cfg)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met — mintWorkspaceSecrets did not persist platform_inbound_secret in SaaS mode (this is the prod bug recurrence): %v", err)
	}

	// Sanity: SaaS mode must NOT have written .platform_inbound_secret
	// into cfg.ConfigFiles — there's no Docker volume to deliver it.
	if _, present := cfg.ConfigFiles[".platform_inbound_secret"]; present {
		t.Errorf("SaaS mode should not inject .platform_inbound_secret into cfg.ConfigFiles (no Docker volume) — got entry")
	}
	if _, present := cfg.ConfigFiles[".auth_token"]; present {
		t.Errorf("SaaS mode should not inject .auth_token into cfg.ConfigFiles (no Docker volume) — got entry")
	}
}

// TestPrepareProvisionContext_ParentIDInjection pins the PARENT_ID env
// contract added in #2367: when payload.ParentID is set (currently only
// TeamHandler.Expand populates it), prepareProvisionContext MUST
// surface it as envVars["PARENT_ID"] so workspace/coordinator.py can
// read it on startup. Pre-fix #2367 the env was set inline in
// TeamHandler.Expand on cfg.EnvVars; the refactor moved it into the
// shared prepare so any future provision path with a parent_id
// inherits it automatically.
func TestPrepareProvisionContext_ParentIDInjection(t *testing.T) {
	cases := []struct {
		name       string
		parentID   *string
		expectKey  bool
		expectVal  string
	}{
		{
			name:      "parentID nil → no PARENT_ID env",
			parentID:  nil,
			expectKey: false,
		},
		{
			name:      "parentID empty string → no PARENT_ID env",
			parentID:  ptrStr(""),
			expectKey: false,
		},
		{
			name:      "parentID set → PARENT_ID env populated",
			parentID:  ptrStr("ws-parent-123"),
			expectKey: true,
			expectVal: "ws-parent-123",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			// loadWorkspaceSecrets queries: empty rows + empty rows = clean prep.
			mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
				WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
			mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
				WithArgs("ws-child").
				WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))

			handler := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
			payload := models.CreateWorkspacePayload{
				Name:     "child",
				Tier:     1,
				ParentID: tc.parentID,
			}
			prepared, abort := handler.prepareProvisionContext(context.Background(), "ws-child", "/nonexistent", nil, payload, false)
			if abort != nil {
				t.Fatalf("unexpected abort: %s", abort.Msg)
			}
			val, present := prepared.EnvVars["PARENT_ID"]
			if present != tc.expectKey {
				t.Errorf("PARENT_ID present=%v, want %v (env=%v)", present, tc.expectKey, prepared.EnvVars)
			}
			if tc.expectKey && val != tc.expectVal {
				t.Errorf("PARENT_ID=%q, want %q", val, tc.expectVal)
			}
		})
	}
}

func ptrStr(s string) *string { return &s }

// TestPrepareProvisionContext_InjectsGitHTTPCredsFromPersonaToken pins
// the end-to-end wiring of the durable-git-auth fix: when a workspace
// is provisioned with a slug-form role matching a persona dir at
// $MOLECULE_PERSONA_ROOT/<role>/token, the prepared envVars MUST
// carry GIT_HTTP_USERNAME / GIT_HTTP_PASSWORD (+ GITEA_USER / GITEA_TOKEN
// fallback) so the in-container askpass helper has something to emit
// on git's auth challenge.
//
// Pre-fix shape (Dev-A/Dev-B live-verified 2026-05-18 ~23:55Z): the
// askpass binary + GIT_ASKPASS env were already wired
// (template-claude-code#30 + mc#1525), but GIT_HTTP_USERNAME and
// GIT_HTTP_PASSWORD were absent from the container env → askpass
// returned empty → git rc=128 "Authentication failed" in <500ms.
// This test fails without applyAgentGitHTTPCreds wired into
// prepareProvisionContext and proves the prod-team path is closed.
func TestPrepareProvisionContext_InjectsGitHTTPCredsFromPersonaToken(t *testing.T) {
	// Stage a persona dir matching the prod-team shape per
	// reference_prod_team_infisical_identities — a flat dir per role
	// with a single mode-600 `token` file.
	root := t.TempDir()
	for _, role := range []string{"agent-dev-a", "agent-dev-b"} {
		roleDir := filepath.Join(root, role)
		if err := os.MkdirAll(roleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Token value pinned to a recognizable string so we can
		// assert exact propagation. Real bootstrap-kit files end in
		// \n; the helper must trim that.
		if err := os.WriteFile(filepath.Join(roleDir, "token"),
			[]byte("token-for-"+role+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	cases := []struct {
		name          string
		role          string
		expectInject  bool
		expectUser    string
		expectPass    string
	}{
		{
			name:         "Dev-A slug role → persona token injected as GIT_HTTP_USERNAME/PASSWORD",
			role:         "agent-dev-a",
			expectInject: true,
			expectUser:   "agent-dev-a",
			expectPass:   "token-for-agent-dev-a",
		},
		{
			name:         "Dev-B slug role → persona token injected",
			role:         "agent-dev-b",
			expectInject: true,
			expectUser:   "agent-dev-b",
			expectPass:   "token-for-agent-dev-b",
		},
		{
			name:         "descriptive multi-word role → silent no-op (no persona dir lookup)",
			role:         "Frontend Engineer",
			expectInject: false,
		},
		{
			name:         "unknown slug role with no persona dir → silent no-op",
			role:         "agent-nonexistent",
			expectInject: false,
		},
		{
			name:         "empty role → silent no-op",
			role:         "",
			expectInject: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
				WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
			mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
				WithArgs("ws-prod-team").
				WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))

			handler := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
			payload := models.CreateWorkspacePayload{
				Name: "Dev-A",
				Role: tc.role,
				Tier: 1,
			}
			prepared, abort := handler.prepareProvisionContext(
				context.Background(), "ws-prod-team", "/nonexistent", nil, payload, false)
			if abort != nil {
				t.Fatalf("unexpected abort: %s", abort.Msg)
			}

			gotUser, hasUser := prepared.EnvVars["GIT_HTTP_USERNAME"]
			gotPass, hasPass := prepared.EnvVars["GIT_HTTP_PASSWORD"]

			if tc.expectInject {
				if !hasUser || gotUser != tc.expectUser {
					t.Errorf("GIT_HTTP_USERNAME: got %q (present=%v), want %q",
						gotUser, hasUser, tc.expectUser)
				}
				if !hasPass || gotPass != tc.expectPass {
					t.Errorf("GIT_HTTP_PASSWORD: got %q (present=%v), want %q",
						gotPass, hasPass, tc.expectPass)
				}
				// Fallback pair should ALSO be set so askpass's
				// GITEA_USER/GITEA_TOKEN fallback chain works
				// (GITEA_TOKEN will then be stripped at
				// buildContainerEnv per forensic #145, but
				// GITEA_USER survives — see provisioner_test.go
				// "persona-file path" subtest).
				if prepared.EnvVars["GITEA_USER"] != tc.expectUser {
					t.Errorf("GITEA_USER fallback: got %q, want %q",
						prepared.EnvVars["GITEA_USER"], tc.expectUser)
				}
				if prepared.EnvVars["GITEA_TOKEN"] != tc.expectPass {
					t.Errorf("GITEA_TOKEN fallback: got %q, want %q",
						prepared.EnvVars["GITEA_TOKEN"], tc.expectPass)
				}
			} else {
				if hasUser {
					t.Errorf("GIT_HTTP_USERNAME should NOT be set for role %q; got %q",
						tc.role, gotUser)
				}
				if hasPass {
					t.Errorf("GIT_HTTP_PASSWORD should NOT be set for role %q; got %q",
						tc.role, gotPass)
				}
			}

			// applyAgentGitIdentity always wires GIT_ASKPASS when
			// payload.Name is non-empty — sanity check that the new
			// wiring didn't accidentally bypass the existing askpass
			// env-set (the helper without env = nothing to emit).
			if prepared.EnvVars["GIT_ASKPASS"] != "/usr/local/bin/molecule-askpass" {
				t.Errorf("GIT_ASKPASS should remain wired by applyAgentGitIdentity; got %q",
					prepared.EnvVars["GIT_ASKPASS"])
			}
		})
	}
}

// TestPrepareProvisionContext_WorkspaceSecretWinsOverPersonaToken pins
// the precedence contract: an operator-supplied workspace_secret named
// GIT_HTTP_USERNAME / GIT_HTTP_PASSWORD (loaded by loadWorkspaceSecrets
// BEFORE applyAgentGitHTTPCreds runs) must beat the persona-file
// default. This is the standard escape hatch — if an operator needs a
// per-workspace override (e.g. a workspace-scoped Gitea token with
// narrower repo access than the persona's), the secrets API still
// works.
func TestPrepareProvisionContext_WorkspaceSecretWinsOverPersonaToken(t *testing.T) {
	root := t.TempDir()
	roleDir := filepath.Join(root, "agent-dev-a")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "token"),
		[]byte("persona-file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOLECULE_PERSONA_ROOT", root)

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	// Workspace secret pre-populates GIT_HTTP_USERNAME / GIT_HTTP_PASSWORD —
	// these come from loadWorkspaceSecrets which runs before applyAgentGitHTTPCreds.
	// encryption_version=0 means raw bytes (crypto disabled in test).
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WithArgs("ws-prod-team").
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("GIT_HTTP_USERNAME", []byte("operator-override-user"), 0).
			AddRow("GIT_HTTP_PASSWORD", []byte("operator-override-pass"), 0))

	handler := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	payload := models.CreateWorkspacePayload{
		Name: "Dev-A",
		Role: "agent-dev-a",
		Tier: 1,
	}
	prepared, abort := handler.prepareProvisionContext(
		context.Background(), "ws-prod-team", "/nonexistent", nil, payload, false)
	if abort != nil {
		t.Fatalf("unexpected abort: %s", abort.Msg)
	}

	if prepared.EnvVars["GIT_HTTP_USERNAME"] != "operator-override-user" {
		t.Errorf("operator override lost — GIT_HTTP_USERNAME: got %q, want %q",
			prepared.EnvVars["GIT_HTTP_USERNAME"], "operator-override-user")
	}
	if prepared.EnvVars["GIT_HTTP_PASSWORD"] != "operator-override-pass" {
		t.Errorf("operator override lost — GIT_HTTP_PASSWORD: got %q, want %q",
			prepared.EnvVars["GIT_HTTP_PASSWORD"], "operator-override-pass")
	}
}

// TestReadOrLazyHealInboundSecret pins the four branches of the
// shared lazy-heal helper directly. Each call site (chat_files,
// registry) has its own integration test, but those go through the
// public handlers and conflate the helper's behavior with the
// caller's response shape. This direct test pins the (secret, healed,
// err) contract on its own so a future refactor that breaks the
// helper signal — e.g., returning healed=true on a read-success path,
// or swallowing a mint error — fails immediately.
//
// The four branches:
//
//   1. Secret already present → (s, false, nil)
//   2. Secret missing, mint succeeds → (minted, true, nil)
//   3. Secret missing, mint fails → ("", false, mint-err)
//   4. Read fails (non-NoInboundSecret) → ("", false, read-err)
func TestReadOrLazyHealInboundSecret(t *testing.T) {
	t.Run("secret already present → no heal, no error", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
			WithArgs("ws-1").
			WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow("present-secret"))

		secret, healed, err := readOrLazyHealInboundSecret(context.Background(), "ws-1", "TestOp")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if secret != "present-secret" {
			t.Errorf("secret: got %q, want %q", secret, "present-secret")
		}
		if healed {
			t.Errorf("healed should be false when secret was already present")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unexpected sqlmock state — read happened but mint should NOT have: %v", err)
		}
	})

	t.Run("secret missing → mint succeeds → returns healed=true", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
			WithArgs("ws-2").
			WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))
		mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
			WithArgs(sqlmock.AnyArg(), "ws-2").
			WillReturnResult(sqlmock.NewResult(0, 1))

		secret, healed, err := readOrLazyHealInboundSecret(context.Background(), "ws-2", "TestOp")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if secret == "" {
			t.Error("expected a freshly-minted secret string, got empty")
		}
		if !healed {
			t.Error("healed should be true after lazy-heal mint succeeded")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock expectations not met — mint did NOT run: %v", err)
		}
	})

	t.Run("secret missing → mint fails → returns err and not healed", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
			WithArgs("ws-3").
			WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))
		mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
			WithArgs(sqlmock.AnyArg(), "ws-3").
			WillReturnError(sql.ErrConnDone)

		secret, healed, err := readOrLazyHealInboundSecret(context.Background(), "ws-3", "TestOp")
		if err == nil {
			t.Fatal("expected mint error, got nil")
		}
		if secret != "" {
			t.Errorf("expected empty secret on mint failure, got %q", secret)
		}
		if healed {
			t.Error("healed must be false when mint failed")
		}
	})

	t.Run("read fails (non-NoInboundSecret) → returns err and not healed", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
			WithArgs("ws-4").
			WillReturnError(sql.ErrConnDone)

		secret, healed, err := readOrLazyHealInboundSecret(context.Background(), "ws-4", "TestOp")
		if err == nil {
			t.Fatal("expected read error, got nil")
		}
		if secret != "" {
			t.Errorf("expected empty secret on read failure, got %q", secret)
		}
		if healed {
			t.Error("healed must be false when read failed")
		}
	})
}

// TestDeriveProviderFromModelSlug pins the slug→provider mapping shared
// with workspace-configs-templates/hermes/scripts/derive-provider.sh.
// Sync-test: when a new prefix is added to the shell script, add it
// here too. The two intentional differences from the shell version
// (nousresearch/openai both → "openrouter" at provision time;
// unknown/no-prefix → "" instead of "auto") are exercised explicitly.
func TestDeriveProviderFromModelSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		model string
		want  string
	}{
		{"minimax", "minimax/MiniMax-M2.7-highspeed", "minimax"},
		{"minimax-cn keeps cn suffix", "minimax-cn/MiniMax-M2.7", "minimax-cn"},
		{"anthropic", "anthropic/claude-sonnet-4-6", "anthropic"},
		{"gemini", "gemini/gemini-2.5-pro", "gemini"},
		{"deepseek", "deepseek/deepseek-v3", "deepseek"},
		{"zai", "zai/glm-4.6", "zai"},
		{"kimi-coding", "kimi-coding/kimi-k2", "kimi-coding"},
		{"kimi-coding-cn keeps cn suffix", "kimi-coding-cn/kimi-k2", "kimi-coding-cn"},
		{"alibaba via dashscope alias", "dashscope/qwen3", "alibaba"},
		{"alibaba via qwen alias", "qwen/qwen3-coder", "alibaba"},
		{"xiaomi via mimo alias", "mimo/mimo-vl", "xiaomi"},
		{"arcee via arcee-ai alias", "arcee-ai/arcee-blitz", "arcee"},
		{"nvidia via nim alias", "nim/llama-3.3-nemotron-super", "nvidia"},
		{"ollama-cloud", "ollama-cloud/qwen3", "ollama-cloud"},
		{"huggingface via hf alias", "hf/Qwen/Qwen3", "huggingface"},
		{"ai-gateway", "ai-gateway/anthropic-claude-sonnet-4-6", "ai-gateway"},
		{"kilocode", "kilocode/kilo-1", "kilocode"},
		{"opencode-zen", "opencode-zen/zen-1", "opencode-zen"},
		{"opencode-go", "opencode-go/code-1", "opencode-go"},
		{"openrouter passthrough", "openrouter/anthropic/claude-sonnet-4-6", "openrouter"},
		{"custom passthrough", "custom/my-private-endpoint", "custom"},
		// Runtime-only override candidates default to openrouter at
		// provision time (derive-provider.sh upgrades to nous/custom at
		// boot if HERMES_API_KEY/OPENAI_API_KEY are present).
		{"nousresearch defaults to openrouter at provision time", "nousresearch/hermes-4-70b", "openrouter"},
		{"openai defaults to openrouter at provision time", "openai/gpt-5", "openrouter"},
		// hermes-agent v0.12.0 / 2026-04-30 provider list — the drift gate
		// in derive_provider_drift_test.go pins parity with the shell case
		// statement.
		{"xai", "xai/grok-4", "xai"},
		{"xai via grok alias", "grok/grok-4", "xai"},
		{"bedrock", "bedrock/anthropic.claude-sonnet-4-6", "bedrock"},
		{"bedrock via aws alias", "aws/anthropic.claude-sonnet-4-6", "bedrock"},
		{"tencent", "tencent/hunyuan-coder", "tencent-tokenhub"},
		{"tencent-tokenhub passthrough", "tencent-tokenhub/hunyuan-coder", "tencent-tokenhub"},
		{"gmi", "gmi/gmi-coder-1", "gmi"},
		{"qwen-oauth", "qwen-oauth/qwen3-coder", "qwen-oauth"},
		{"lmstudio", "lmstudio/qwen3-coder", "lmstudio"},
		{"lmstudio via lm-studio alias", "lm-studio/qwen3-coder", "lmstudio"},
		{"minimax-oauth", "minimax-oauth/MiniMax-M2.7", "minimax-oauth"},
		{"alibaba-coding-plan", "alibaba-coding-plan/qwen3-coder", "alibaba-coding-plan"},
		{"google-gemini-cli", "google-gemini-cli/gemini-2.5-pro", "google-gemini-cli"},
		{"openai-codex", "openai-codex/gpt-5-codex", "openai-codex"},
		{"copilot-acp", "copilot-acp/claude-sonnet-4-6", "copilot-acp"},
		{"copilot", "copilot/claude-sonnet-4-6", "copilot"},
		// Unknowns return "" so the caller skips the LLM_PROVIDER write
		// and lets derive-provider.sh's *=auto branch decide at runtime.
		{"unknown prefix returns empty", "totally-unknown-model/foo", ""},
		{"empty input returns empty", "", ""},
		{"no slash returns empty", "no-slash-here", ""},
		{"leading slash returns empty", "/leading-slash", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := deriveProviderFromModelSlug(tc.model)
			if got != tc.want {
				t.Errorf("deriveProviderFromModelSlug(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

// TestWorkspaceCreate_FirstDeploy_PersistsModelAndProvider pins the
// fix for failed-workspace 95ed3ff2 (2026-05-02). Pre-fix: the canvas
// POSTed minimax/MiniMax-M2.7 in payload.Model, the workspace row was
// created, but neither the model nor the derived provider was ever
// written to workspace_secrets. On any subsequent restart, the
// applyRuntimeModelEnv fallback found nothing and hermes booted with
// the template default (nousresearch/hermes-4-70b) → wrong provider
// keys → /health poll failed → never registered.
//
// Post-fix: the create handler writes both rows after committing the
// workspace row. This test asserts the SQL writes happen with the
// correct keys + values.
//
// 2026-05-19 follow-up: the workspace_secrets row that holds the
// picked model id was renamed MODEL_PROVIDER → MODEL (the column name
// was misleading and bled into applyRuntimeModelEnv as a slug
// fallback). The sqlmock regex below now anchors on 'MODEL' instead
// of 'MODEL_PROVIDER'. See fix/workspace-server-rename-
// MODEL_PROVIDER-to-MODEL + the 20260519000000 rename migration.
func TestWorkspaceCreate_FirstDeploy_PersistsModelAndProvider(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	// External workspace path: the SAME post-commit secret-mint code
	// runs, but no provisioner goroutine spawns to race the
	// sqlmock expectations. external=true is the cleanest way to
	// pin the mint behavior in isolation.
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// The fix: MODEL is upserted with the verbatim model slug
	// (renamed from MODEL_PROVIDER on 2026-05-19 — see file-level
	// docstring). SQL has 3 placeholders ($1=workspace_id, $2=
	// encrypted_value reused in the conflict-update, $3=version
	// reused in the conflict-update), so sqlmock sees 3 args. The
	// 'MODEL' / 'LLM_PROVIDER' key is a literal in the SQL — we
	// distinguish the two writes with the regex match below. The
	// 'MODEL' anchor uses a word boundary (`[^_A-Z]`) so it does
	// NOT silently match the legacy 'MODEL_PROVIDER' name.
	mock.ExpectExec(`INSERT INTO workspace_secrets[\s\S]*'MODEL'`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The fix: LLM_PROVIDER is upserted with the derived provider name.
	mock.ExpectExec(`INSERT INTO workspace_secrets[\s\S]*'LLM_PROVIDER'`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Post-mint side effects (canvas layout + structure_events broadcast
	// + the external-workspace UPDATE/IssueToken chain). Order matches
	// workspace.go.
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// External branch with no URL: status → awaiting_agent + IssueToken.
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// wsauth.IssueToken inserts into workspace_auth_tokens.
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// awaiting_agent broadcast.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"Hermes Minimax Agent","runtime":"hermes","external":true,"model":"minimax/MiniMax-M2.7"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met — first-deploy did NOT persist MODEL + LLM_PROVIDER (this is the prod bug recurrence): %v", err)
	}
}

// TestWorkspaceCreate_FirstDeploy_NoModel_NoSecretWritten asserts that
// when payload.Model is empty, NEITHER MODEL nor LLM_PROVIDER is
// written. Important: the canvas can omit `model` (template inherits
// the runtime default later); we must not poison workspace_secrets with
// empty rows in that case.
func TestWorkspaceCreate_FirstDeploy_NoModel_NoSecretWritten(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// NO INSERT INTO workspace_secrets here — the gate is payload.Model != "".

	mock.ExpectExec("INSERT INTO canvas_layouts").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"No Model Agent","runtime":"hermes","external":true}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met — empty payload.Model should NOT trigger workspace_secrets writes: %v", err)
	}
}

// TestWorkspaceCreate_FirstDeploy_UnknownModel_OnlyMintModelProvider
// asserts the asymmetric case: an unknown model prefix still gets
// MODEL persisted (so the user's exact slug survives restart and
// applyRuntimeModelEnv finds it), but LLM_PROVIDER is skipped (so
// derive-provider.sh's *=auto branch can decide at runtime instead of
// being pre-empted by a guess). The MODEL key was renamed from
// MODEL_PROVIDER on 2026-05-19 — see file-level docstring.
func TestWorkspaceCreate_FirstDeploy_UnknownModel_OnlyMintModelProvider(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Only MODEL — LLM_PROVIDER must NOT be written for unknown
	// prefixes. Same 3-arg shape as above; key is literal in SQL.
	mock.ExpectExec(`INSERT INTO workspace_secrets[\s\S]*'MODEL'`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("INSERT INTO canvas_layouts").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"Unknown Model Agent","runtime":"hermes","external":true,"model":"totally-unknown-model/foo"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met — unknown-prefix model should mint MODEL but skip LLM_PROVIDER: %v", err)
	}
}

// TestApplyRuntimeModelEnv_SetsUniversalMODELForAllRuntimes pins the
// fix for Bug B (2026-05-02): canvas-selected model was silently dropped
// for templated workspaces because the per-runtime switch only set
// HERMES_DEFAULT_MODEL for hermes — every other runtime got nothing.
// The adapter then read its template's default model from /configs/config.yaml
// and demanded the wrong env var (e.g. claude-code/sonnet → CLAUDE_CODE_OAUTH_TOKEN
// even though the user had picked MiniMax-M2 with MINIMAX_API_KEY set).
//
// Post-fix: applyRuntimeModelEnv unconditionally sets MODEL=<picked> for
// every runtime, in addition to any vendor-specific name (HERMES_DEFAULT_MODEL
// stays for backwards compat). Adapters opt in to honouring MODEL by reading
// os.environ["MODEL"] in their executor (claude-code adapter does this since
// the same Bug B fix; see workspace-configs-templates/claude-code-default/adapter.py).
//
// Table-driven so adding a new runtime means adding a row, not writing a
// new test function.
func TestApplyRuntimeModelEnv_SetsUniversalMODELForAllRuntimes(t *testing.T) {
	cases := []struct {
		name              string
		runtime           string
		model             string
		modelProviderEnv  string
		moleculeModelEnv  string
		wantMODEL         string
		wantHermesDefault string // empty string = must be unset
	}{
		{
			name:      "claude-code: picked model populates MODEL + MOLECULE_MODEL",
			runtime:   "claude-code",
			model:     "MiniMax-M2",
			wantMODEL: "MiniMax-M2",
		},
		{
			name:              "hermes: picked model populates MODEL, MOLECULE_MODEL, HERMES_DEFAULT_MODEL",
			runtime:           "hermes",
			model:             "minimax/MiniMax-M2.7",
			wantMODEL:         "minimax/MiniMax-M2.7",
			wantHermesDefault: "minimax/MiniMax-M2.7",
		},
		{
			name:      "langgraph: picked model populates MODEL + MOLECULE_MODEL (no vendor-specific name)",
			runtime:   "langgraph",
			model:     "anthropic:claude-opus-4-7",
			wantMODEL: "anthropic:claude-opus-4-7",
		},
		{
			name:      "crewai: picked model populates MODEL + MOLECULE_MODEL (no vendor-specific name)",
			runtime:   "crewai",
			model:     "openai:gpt-4o",
			wantMODEL: "openai:gpt-4o",
		},
		{
			name:    "empty model + no env fallback: nothing set",
			runtime: "claude-code",
			model:   "",
		},
		{
			name:             "empty model + MODEL_PROVIDER env IGNORED post-2026-05-19 rename (the slug-fallback bug)",
			runtime:          "claude-code",
			model:            "",
			modelProviderEnv: "MiniMax-M2",
			wantMODEL:        "",
		},
		{
			name:             "empty model + MOLECULE_MODEL env fallback hits (canonical name)",
			runtime:          "claude-code",
			model:            "",
			moleculeModelEnv: "opus",
			wantMODEL:        "opus",
		},
		{
			name:             "MOLECULE_MODEL wins even when stale MODEL_PROVIDER is present (back-compat guard)",
			runtime:          "claude-code",
			model:            "",
			moleculeModelEnv: "opus",
			modelProviderEnv: "claude-code",
			wantMODEL:        "opus",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envVars := map[string]string{}
			if tc.modelProviderEnv != "" {
				envVars["MODEL_PROVIDER"] = tc.modelProviderEnv
			}
			if tc.moleculeModelEnv != "" {
				envVars["MOLECULE_MODEL"] = tc.moleculeModelEnv
			}
			applyRuntimeModelEnv(envVars, tc.runtime, tc.model)

			if got := envVars["MODEL"]; got != tc.wantMODEL {
				t.Errorf("MODEL = %q, want %q", got, tc.wantMODEL)
			}
			// MOLECULE_MODEL (the canonical name) must mirror MODEL exactly.
			if got := envVars["MOLECULE_MODEL"]; got != tc.wantMODEL {
				t.Errorf("MOLECULE_MODEL = %q, want %q", got, tc.wantMODEL)
			}
			if got := envVars["HERMES_DEFAULT_MODEL"]; got != tc.wantHermesDefault {
				t.Errorf("HERMES_DEFAULT_MODEL = %q, want %q", got, tc.wantHermesDefault)
			}
		})
	}
}

// TestApplyRuntimeModelEnv_PersonaEnvMODELSecretPreserved locks in the
// 2026-05-08 fix that prevents the MODEL_PROVIDER-as-slug fallback from
// silently overwriting a per-persona MODEL workspace_secret on restart,
// EXTENDED for the 2026-05-19 root-cause fix that drops the
// MODEL_PROVIDER fallback entirely.
//
// Pre-fix bug recurrence guard: when the persona env file (loaded into
// workspace_secrets at /org/import time) declares both MODEL=<id> and
// MODEL_PROVIDER=<slug>, the restart path used to overwrite envVars["MODEL"]
// with the MODEL_PROVIDER slug because applyRuntimeModelEnv's
// payload.Model fallback consulted MODEL_PROVIDER first. Symptom: dev-tree
// workspaces booted fine on first /org/import, then on next restart the
// model id became literal "minimax" and the workspace template's adapter
// failed to match any registry prefix, fell through to anthropic-oauth,
// and wedged at SDK initialize. Caught during Phase 4 verification of
// template-claude-code PR #9.
//
// 2026-05-19 follow-up: the MODEL_PROVIDER fallback is now removed.
// MODEL is the only env-var source for the picked model id.
// MODEL_PROVIDER is intentionally NOT consulted — a stale MODEL_PROVIDER
// row left over from before the 20260519000000 migration must NOT leak
// into envVars["MODEL"]. Verified by the third case below.
func TestApplyRuntimeModelEnv_PersonaEnvMODELSecretPreserved(t *testing.T) {
	cases := []struct {
		name      string
		envMODEL  string
		envMP     string
		wantMODEL string
	}{
		{
			name:      "MODEL secret wins; stale MODEL_PROVIDER ignored (persona-env shape on restart)",
			envMODEL:  "MiniMax-M2.7-highspeed",
			envMP:     "minimax",
			wantMODEL: "MiniMax-M2.7-highspeed",
		},
		{
			name:      "MODEL secret wins even when same as MODEL_PROVIDER",
			envMODEL:  "opus",
			envMP:     "claude-code",
			wantMODEL: "opus",
		},
		{
			name:      "MODEL absent → MODEL_PROVIDER no longer fallback (2026-05-19 fix): nothing set",
			envMODEL:  "",
			envMP:     "MiniMax-M2.7",
			wantMODEL: "",
		},
		{
			name:      "Both absent → no MODEL set",
			envMODEL:  "",
			envMP:     "",
			wantMODEL: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envVars := map[string]string{}
			if tc.envMODEL != "" {
				envVars["MODEL"] = tc.envMODEL
			}
			if tc.envMP != "" {
				envVars["MODEL_PROVIDER"] = tc.envMP
			}
			// payload.Model is empty (the restart case)
			applyRuntimeModelEnv(envVars, "claude-code", "")
			if got := envVars["MODEL"]; got != tc.wantMODEL {
				t.Errorf("MODEL = %q, want %q (envMODEL=%q envMP=%q)",
					got, tc.wantMODEL, tc.envMODEL, tc.envMP)
			}
		})
	}
}

// TestApplyRuntimeModelEnv_StaleMODELPROVIDERNeverLeaksIntoMODEL is the
// 2026-05-19 root-cause pin: workspaces that were live BEFORE the
// 20260519000000_workspace_secrets_model_provider_rename migration ran
// may still have a MODEL_PROVIDER row in workspace_secrets that lands
// in envVars (the loader doesn't filter — anything in workspace_secrets
// gets passed through). Post-fix, applyRuntimeModelEnv MUST NOT consult
// that key for any purpose — neither as a fallback for the picked model
// id nor as an indirect overwrite of MODEL. Asserts the read-out shape:
//
//   - envVars["MODEL"] stays empty when no other source provided one
//   - envVars["MOLECULE_MODEL"] stays empty
//   - envVars["HERMES_DEFAULT_MODEL"] stays empty
//   - envVars["MODEL_PROVIDER"] itself is left as-is (we don't actively
//     scrub it — the rename migration does that on the DB side)
//
// Pairs with workspace_provision.go applyRuntimeModelEnv (line 817
// fallback removed) and secrets.go (workspace_secrets key MODEL).
func TestApplyRuntimeModelEnv_StaleMODELPROVIDERNeverLeaksIntoMODEL(t *testing.T) {
	envVars := map[string]string{
		"MODEL_PROVIDER": "minimax", // legacy slug — the prod-bug shape
	}
	applyRuntimeModelEnv(envVars, "claude-code", "")
	if got, ok := envVars["MODEL"]; ok {
		t.Errorf("MODEL must not be set from MODEL_PROVIDER fallback (post-2026-05-19 fix); got=%q", got)
	}
	if got, ok := envVars["MOLECULE_MODEL"]; ok {
		t.Errorf("MOLECULE_MODEL must not be set from MODEL_PROVIDER fallback; got=%q", got)
	}
	if got, ok := envVars["HERMES_DEFAULT_MODEL"]; ok {
		t.Errorf("HERMES_DEFAULT_MODEL must not be set from MODEL_PROVIDER fallback; got=%q", got)
	}
	if got := envVars["MODEL_PROVIDER"]; got != "minimax" {
		t.Errorf("MODEL_PROVIDER must be passed through untouched (DB-side rename handles cleanup); got=%q", got)
	}

	// Hermes-runtime variant — same shape, same expectation.
	envVarsH := map[string]string{
		"MODEL_PROVIDER": "minimax",
	}
	applyRuntimeModelEnv(envVarsH, "hermes", "")
	if _, ok := envVarsH["HERMES_DEFAULT_MODEL"]; ok {
		t.Errorf("hermes runtime must not leak MODEL_PROVIDER into HERMES_DEFAULT_MODEL")
	}
}
