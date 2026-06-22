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

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
	"github.com/DATA-DOG/go-sqlmock"
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
		name      string
		parentID  *string
		expectKey bool
		expectVal string
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

	// Supply the CP proxy env so the platform-managed default does not abort
	// with MISSING_PLATFORM_PROXY (molecule-core#2162).
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

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
				// core#2594: model required by the provision gate; unrelated to this test.
				Model: "anthropic:claude-opus-4-7",
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
	// Supply the CP proxy env so the platform-managed default does not abort
	// with MISSING_PLATFORM_PROXY (molecule-core#2162).
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	cases := []struct {
		name         string
		role         string
		expectInject bool
		expectUser   string
		expectPass   string
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
				// core#2594: model required by the provision gate; unrelated to this test.
				Model: "anthropic:claude-opus-4-7",
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
	// Supply the CP proxy env so the platform-managed default does not abort
	// with MISSING_PLATFORM_PROXY (molecule-core#2162).
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

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
		// core#2594: model required by the provision gate; unrelated to this test.
		Model: "anthropic:claude-opus-4-7",
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

// TestPrepareProvisionContext_ByokWithTenantGlobalOAuthSucceeds is the
// molecule-core#1994 (corrected-model) end-to-end inversion of the former
// internal#711 fail-closed test, for the live Reno Stars byok agents. A byok
// workspace whose LLM credential is the TENANT's own scope:global
// CLAUDE_CODE_OAUTH_TOKEN (a global_secrets row, no workspace override) must:
//
//  1. KEEP that oauth in the prepared container env (it is the tenant's own
//     credential — exactly what byok runs on, direct), and
//  2. NOT abort — the provision proceeds.
//
// Pre-fix (internal#711) prepared.EnvVars stripped the global oauth and the
// provision aborted MISSING_BYOK_CREDENTIAL → the agent was dead. This is the
// discriminating end-to-end guard for the fix.
func TestPrepareProvisionContext_ByokWithTenantGlobalOAuthSucceeds(t *testing.T) {
	const wsID = "352e3c2b-0546-4e9c-b487-1e2ff1cf29fc" // Reno Stars SEO agent
	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)

	mock := setupTestDB(t)
	// global_secrets carries the TENANT's own scope:global OAuth token + the
	// stored MODEL (so the resolver derives byok from opus).
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("CLAUDE_CODE_OAUTH_TOKEN", []byte("TENANT-OWN-GLOBAL-OAUTH"), 0))
	// Workspace set its own MODEL (no LLM cred of its own — relies on global).
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("MODEL", []byte("opus"), 0))
	// Resolver: workspace override = byok.
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModeBYOK))

	handler := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	payload := models.CreateWorkspacePayload{
		Name:    "Reno Stars SEO",
		Runtime: "claude-code",
		Tier:    1,
		// core#2594: NO payload model on purpose — this test derives byok from
		// the STORED MODEL secret ("opus"); the gate is satisfied by that stored
		// model (loaded into envVars), so adding a payload model here would both
		// override the derivation and is unnecessary.
	}
	prepared, abort := handler.prepareProvisionContext(
		context.Background(), wsID, "/nonexistent", nil, payload, false)

	if abort != nil {
		t.Fatalf("expected provision to proceed (byok on tenant's own global oauth), got abort=%v", abort.Extra)
	}
	if prepared == nil {
		t.Fatalf("prepared context is nil despite no abort")
	}
	// The tenant's own global oauth must be present in the container env.
	if prepared.EnvVars["CLAUDE_CODE_OAUTH_TOKEN"] != "TENANT-OWN-GLOBAL-OAUTH" {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN = %q, want the tenant's own global oauth preserved for byok",
			prepared.EnvVars["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	// byok must not have been routed through the platform proxy.
	if _, ok := prepared.EnvVars["MOLECULE_LLM_USAGE_TOKEN"]; ok {
		t.Fatalf("byok provision must NOT inject the platform usage token")
	}
	if got := prepared.EnvVars["MOLECULE_LLM_BILLING_MODE_RESOLVED"]; got != LLMBillingModeBYOK {
		t.Fatalf("MOLECULE_LLM_BILLING_MODE_RESOLVED = %q, want byok", got)
	}
}

// TestPrepareProvisionContext_ByokNoCredentialAtAnyScopeFailsClosed is the
// companion: the fail-closed abort is UNCHANGED for a byok workspace with no
// LLM credential at ANY scope (no global row, no workspace row). It still
// aborts MISSING_BYOK_CREDENTIAL rather than starting credential-less.
func TestPrepareProvisionContext_ByokNoCredentialAtAnyScopeFailsClosed(t *testing.T) {
	const wsID = "352e3c2b-0546-4e9c-b487-1e2ff1cf29fc"
	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)

	mock := setupTestDB(t)
	// No global LLM cred — only the stored MODEL so the resolver derives byok.
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("MODEL", []byte("opus"), 0))
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModeBYOK))

	handler := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	payload := models.CreateWorkspacePayload{
		Name:    "Reno Stars SEO",
		Runtime: "claude-code",
		Tier:    1,
		// core#2594: NO payload model — derives byok from the STORED MODEL
		// ("opus"), which also satisfies the model gate. The abort under test is
		// MISSING_BYOK_CREDENTIAL (no LLM cred), reached only because the model
		// IS resolved.
	}
	prepared, abort := handler.prepareProvisionContext(
		context.Background(), wsID, "/nonexistent", nil, payload, false)

	if abort == nil {
		t.Fatalf("expected MISSING_BYOK_CREDENTIAL abort, got success (prepared=%v)", prepared)
	}
	if code, _ := abort.Extra["code"].(string); code != "MISSING_BYOK_CREDENTIAL" {
		t.Fatalf("abort.Extra[code] = %v, want MISSING_BYOK_CREDENTIAL", abort.Extra["code"])
	}
	if mode, _ := abort.Extra["billing_mode"].(string); mode != LLMBillingModeBYOK {
		t.Fatalf("abort.Extra[billing_mode] = %v, want %q", abort.Extra["billing_mode"], LLMBillingModeBYOK)
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
//  1. Secret already present → (s, false, nil)
//  2. Secret missing, mint succeeds → (minted, true, nil)
//  3. Secret missing, mint fails → ("", false, mint-err)
//  4. Read fails (non-NoInboundSecret) → ("", false, read-err)
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

// internal#718 P4 closure: TestDeriveProviderFromModelSlug was the
// table-driven sync test that pinned deriveProviderFromModelSlug
// (retire-list #3) against
// workspace-configs-templates/hermes/scripts/derive-provider.sh.
//
// Both the Go function and this test (with its 35+ slug→provider
// cases) are retired. The slug→provider mapping is now covered by
// providers.Manifest.DeriveProvider against the registry SSOT
// (TestDeriveProvider_RealManifest in
// internal/providers/derive_provider_test.go). The shell script
// remains the in-container fallback; its byte-identity with the
// registry view of hermes is a P4 follow-up gated on registry data
// growth (see PR-2 codegen of hermes config.yaml from the registry).
//
// TestWorkspaceCreate_FirstDeploy_PersistsModelAndProvider, which
// asserted that Create writes BOTH MODEL and LLM_PROVIDER rows, is
// replaced by TestWorkspaceCreate_FirstDeploy_OnlyPersistsMODEL
// below — the LLM_PROVIDER half of the contract is retired.
//
// TestWorkspaceCreate_FirstDeploy_UnknownModel_OnlyMintModelProvider
// is subsumed by the same: with LLM_PROVIDER never written, the
// known-vs-unknown distinction at Create disappears.

// TestWorkspaceCreate_FirstDeploy_OnlyPersistsMODEL pins the post-P4
// contract: WorkspaceHandler.Create writes the MODEL workspace_secret
// (so the canvas-picked model survives restart and applyRuntimeModelEnv
// finds it via the fallback chain) and writes NOTHING ELSE in the
// secret-mint window. Specifically: NO LLM_PROVIDER row is written,
// regardless of payload.LLMProvider or the slug-prefix.
//
// Pre-P4 the create handler also wrote LLM_PROVIDER via setProviderSecret
// — either from payload.LLMProvider verbatim or from
// deriveProviderFromModelSlug(payload.Model). Both code paths were
// retired in internal#718 P4 closure together with the LLM_PROVIDER
// workspace_secret itself (no consumer remains; the provider is derived
// at every decision point from (runtime, model) via the registry).
//
// sqlmock failure on this expectation set is the canonical regression
// signal: if a future PR re-introduces an LLM_PROVIDER write at create,
// sqlmock surfaces "ExpectExec was not called" for any added insert.
// The "MODEL anchor uses no LLM_PROVIDER" assertion below is the
// stronger version of the same gate.
func TestWorkspaceCreate_FirstDeploy_OnlyPersistsMODEL(t *testing.T) {
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

	// MODEL upsert — the only post-commit workspace_secrets write that
	// survived the P4 closure. The 'MODEL' key is literal in the SQL.
	mock.ExpectExec(`INSERT INTO workspace_secrets[\s\S]*'MODEL'`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Post-mint side effects (canvas layout + structure_events broadcast
	// + the external-workspace UPDATE/IssueToken chain). Order matches
	// workspace.go. CRITICALLY: no second `INSERT INTO workspace_secrets`
	// is expected — sqlmock fails if Create attempts an LLM_PROVIDER
	// write.
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
	// Body carries an explicit llm_provider AND a slug-prefixed model — both
	// of which would have triggered an LLM_PROVIDER write pre-P4. The
	// payload field is preserved for backward-compat (older canvases
	// still send it) but the value is intentionally ignored by Create.
	body := `{"name":"External Minimax Agent","runtime":"external","external":true,"model":"minimax/MiniMax-M2.7","llm_provider":"minimax"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met — Create wrote an unexpected workspace_secrets row (likely a re-introduced LLM_PROVIDER write): %v", err)
	}
}

// TestWorkspaceCreate_FirstDeploy_NoModel_Returns422 inverts the prior
// premise (CTO 2026-05-22 SSOT directive — see
// feedback_workspace_model_required_no_platform_default_dynamic_credential_intake
// and TestCreate_ModelRequired_Returns422 in handlers_extended_test.go).
//
// Pre-2026-05-22 the canvas was allowed to omit `model` and the workspace
// would 201 with no workspace_secrets rows for MODEL/LLM_PROVIDER (the
// thinking being that templates inherit the runtime default later). That
// "soft fallback" was the load-bearing bug magnet — `DefaultModel(runtime)`
// would later return `anthropic:claude-opus-4-7`, and codex workspaces
// wedged forever at adapter init.
//
// New contract: empty model is a 422 MODEL_REQUIRED, with NO DB writes
// at all. The gate fires at the Create boundary before INSERT INTO
// workspaces. The follow-on workspace_secrets gate (which the original
// test pinned) is therefore unreachable on the empty-model path — there
// is no row to mint secrets for.
func TestWorkspaceCreate_FirstDeploy_NoModel_Returns422(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	// NO mock.ExpectBegin / INSERT INTO workspaces — the Create gate
	// MUST fire before any DB write. If the gate fires late, sqlmock
	// will surface "call to ExecQuery 'INSERT INTO workspaces' was not
	// expected" — which is exactly the failure mode we want to flag.

	// Body: hermes runtime WITHOUT external:true (the external-runtime
	// exemption — see TestCreate_ExternalRuntime_NoModel_OK — does NOT
	// apply here; hermes spawns a real adapter and model selection
	// matters at adapter init). This is exactly the shape the old
	// "no-model-no-secret-write" test pinned, minus the external flag.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"No Model Agent","runtime":"hermes"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 MODEL_REQUIRED for empty model, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"code":"MODEL_REQUIRED"`)) {
		t.Errorf("expected code=MODEL_REQUIRED in body, got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock saw an unexpected DB write — the MODEL_REQUIRED gate fired too late: %v", err)
	}
}

// internal#718 P4 closure: the asymmetric "known prefix → both
// MODEL+LLM_PROVIDER; unknown prefix → MODEL only" contract is moot —
// Create never writes LLM_PROVIDER for ANY model now. The equivalent
// coverage is TestWorkspaceCreate_FirstDeploy_OnlyPersistsMODEL above
// (uses a slug-prefixed model that pre-P4 WOULD have triggered an
// LLM_PROVIDER write; sqlmock fails if Create attempts one).

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
			name:      "claude-code: picked model populates MODEL + MOLECULE_MODEL (no vendor-specific name)",
			runtime:   "claude-code",
			model:     "anthropic:claude-opus-4-7",
			wantMODEL: "anthropic:claude-opus-4-7",
		},
		{
			name:      "openclaw: picked model populates MODEL + MOLECULE_MODEL (no vendor-specific name)",
			runtime:   "openclaw",
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

// core#2594: the MOLECULE_LLM_DEFAULT_MODEL env fail-open was REMOVED. Even
// with the env set, a workspace provisioned with no model must NOT silently
// inherit it — MODEL/MOLECULE_MODEL stay empty so the universal MISSING_MODEL
// gate (in prepareProvisionContext) fails the provision CLOSED. The proxy creds
// (the credential axis) are still wired; only the opaque model substitution is
// gone.
func TestApplyPlatformManagedLLMEnv_DoesNotInheritEnvDefaultModel_FailClosed(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BILLING_MODE", "platform_managed")
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")
	t.Setenv("MOLECULE_LLM_USAGE_URL", "https://api.example.test/api/v1/internal/llm/usage")
	// Even though the (legacy) env default is present, it must be ignored.
	t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", "moonshot/kimi-k2.6")

	envVars := map[string]string{}
	applyPlatformManagedLLMEnv(context.Background(), envVars, "", "codex", "", nil)
	applyRuntimeModelEnv(envVars, "codex", "")

	// Credential axis still wired (proxy token + base url).
	if got := envVars["OPENAI_BASE_URL"]; got != "https://api.example.test/api/v1/internal/llm/openai/v1" {
		t.Fatalf("OPENAI_BASE_URL = %q", got)
	}
	if got := envVars["OPENAI_API_KEY"]; got != "tenant-admin-token" {
		t.Fatalf("OPENAI_API_KEY = %q", got)
	}
	if got := envVars["MOLECULE_LLM_USAGE_TOKEN"]; got != "tenant-admin-token" {
		t.Fatalf("MOLECULE_LLM_USAGE_TOKEN = %q", got)
	}
	// Model axis: the env default must NOT leak in — fail closed.
	if got := envVars["MODEL"]; got != "" {
		t.Fatalf("MODEL = %q, want empty (env default must not be inherited)", got)
	}
	if got := envVars["MOLECULE_MODEL"]; got != "" {
		t.Fatalf("MOLECULE_MODEL = %q, want empty (env default must not be inherited)", got)
	}
}

func TestApplyPlatformManagedLLMEnv_StripsWorkspaceOpenAIKeyForClaudeCode(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BILLING_MODE", "platform_managed")
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	envVars := map[string]string{
		"OPENAI_API_KEY":  "user-openai-key",
		"OPENAI_BASE_URL": "https://api.openai.com/v1",
		"MODEL":           "openai/gpt-5.5",
	}
	applyPlatformManagedLLMEnv(context.Background(), envVars, "", "claude-code", "", nil)

	if _, ok := envVars["OPENAI_API_KEY"]; ok {
		t.Fatalf("OPENAI_API_KEY should be stripped for claude-code platform-managed mode")
	}
	if _, ok := envVars["OPENAI_BASE_URL"]; ok {
		t.Fatalf("OPENAI_BASE_URL should be stripped for claude-code platform-managed mode")
	}
	if got := envVars["MOLECULE_LLM_USAGE_TOKEN"]; got != "tenant-admin-token" {
		t.Fatalf("MOLECULE_LLM_USAGE_TOKEN = %q", got)
	}
	if got := envVars["MODEL"]; got != "openai/gpt-5.5" {
		t.Fatalf("MODEL = %q", got)
	}
}

func TestApplyPlatformManagedLLMEnv_ClaudeCodeUsesAnthropicProxyOverOAuth(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BILLING_MODE", "platform_managed")
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	// A PLATFORM-namespaced model id ⇒ the workspace SELECTED the Platform
	// provider ⇒ platform_managed ⇒ the stray oauth is stripped and the proxy
	// is injected. (A vendor model id would instead resolve byok and KEEP the
	// oauth — covered by the byok tests.)
	envVars := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "user-oauth-token",
		"MODEL":                   "anthropic/claude-sonnet-4-6",
	}
	applyPlatformManagedLLMEnv(context.Background(), envVars, "", "claude-code", "", nil)

	if _, ok := envVars["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN should be stripped in platform-managed mode")
	}
	if got := envVars["ANTHROPIC_API_KEY"]; got != "tenant-admin-token" {
		t.Fatalf("ANTHROPIC_API_KEY = %q", got)
	}
	if got := envVars["ANTHROPIC_BASE_URL"]; got != "https://api.example.test/api/v1/internal/llm/anthropic/v1" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q", got)
	}
	if got := envVars["MOLECULE_LLM_ANTHROPIC_BASE_URL"]; got != "https://api.example.test/api/v1/internal/llm/anthropic/v1" {
		t.Fatalf("MOLECULE_LLM_ANTHROPIC_BASE_URL = %q", got)
	}
}

func TestApplyPlatformManagedLLMEnv_ClaudeCodeInjectsAnthropicProxyWhenNoWorkspaceKey(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BILLING_MODE", "platform_managed")
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	envVars := map[string]string{}
	applyPlatformManagedLLMEnv(context.Background(), envVars, "", "claude-code", "minimax/MiniMax-M2.7", nil)

	if got := envVars["ANTHROPIC_BASE_URL"]; got != "https://api.example.test/api/v1/internal/llm/anthropic/v1" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q", got)
	}
	if got := envVars["ANTHROPIC_API_KEY"]; got != "tenant-admin-token" {
		t.Fatalf("ANTHROPIC_API_KEY = %q", got)
	}
	if got := envVars["MOLECULE_LLM_USAGE_TOKEN"]; got != "tenant-admin-token" {
		t.Fatalf("MOLECULE_LLM_USAGE_TOKEN = %q", got)
	}
}

// TestApplyPlatformManagedLLMEnv_BYOKMiniMaxProjectsAnthropicAdapterCreds is
// core#2709: a claude-code workspace using BYOK MiniMax arrives with only
// MINIMAX_API_KEY. The Anthropic SDK adapter needs ANTHROPIC_AUTH_TOKEN and
// ANTHROPIC_BASE_URL to authenticate against api.minimax.io/anthropic.
func TestApplyPlatformManagedLLMEnv_BYOKMiniMaxProjectsAnthropicAdapterCreds(t *testing.T) {
	envVars := map[string]string{
		"MINIMAX_API_KEY": "user-minimax-key",
		"MODEL":           "MiniMax-M2.7",
	}
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, "", "claude-code", "MiniMax-M2.7", nil)

	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Fatalf("resolved mode = %q, want byok", res.ResolvedMode)
	}
	if got := envVars["ANTHROPIC_AUTH_TOKEN"]; got != "user-minimax-key" {
		t.Fatalf("ANTHROPIC_AUTH_TOKEN = %q, want user-minimax-key", got)
	}
	// core#2748: adapter base must NOT carry a trailing /v1 — the claude-code
	// Anthropic SDK appends /v1/messages itself. The proxy-shaped registry value
	// .../anthropic/v1 is normalized to .../anthropic so the effective endpoint is
	// .../anthropic/v1/messages (HTTP 200), not the double-/v1 .../v1/v1/messages
	// (HTTP 404) that took the coding engines down.
	if got := envVars["ANTHROPIC_BASE_URL"]; got != "https://api.minimax.io/anthropic" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want https://api.minimax.io/anthropic (no double /v1, core#2748)", got)
	}
	// The original MiniMax key is preserved too.
	if got := envVars["MINIMAX_API_KEY"]; got != "user-minimax-key" {
		t.Fatalf("MINIMAX_API_KEY was overwritten: %q", got)
	}
}

func TestApplyPlatformManagedLLMEnv_ClaudeCodeStripsVendorBYOK(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BILLING_MODE", "platform_managed")
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	// A PLATFORM-namespaced model ⇒ platform_managed ⇒ a stray vendor key is
	// stripped (the proxy is used). (If the workspace had SELECTED the MiniMax
	// vendor model, it would resolve byok and KEEP its MiniMax key instead.)
	envVars := map[string]string{
		"MINIMAX_API_KEY": "user-minimax-key",
		"MODEL":           "anthropic/claude-sonnet-4-6",
	}
	applyPlatformManagedLLMEnv(context.Background(), envVars, "", "claude-code", "", nil)

	if _, ok := envVars["MINIMAX_API_KEY"]; ok {
		t.Fatalf("MINIMAX_API_KEY should be stripped in platform-managed mode")
	}
	if got := envVars["ANTHROPIC_API_KEY"]; got != "tenant-admin-token" {
		t.Fatalf("ANTHROPIC_API_KEY = %q", got)
	}
	if got := envVars["ANTHROPIC_BASE_URL"]; got != "https://api.example.test/api/v1/internal/llm/anthropic/v1" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q", got)
	}
	if got := envVars["MOLECULE_LLM_USAGE_TOKEN"]; got != "tenant-admin-token" {
		t.Fatalf("MOLECULE_LLM_USAGE_TOKEN = %q", got)
	}
}

// internal#718 P2-B: byok is now DERIVED, not org-env-driven. A claude-code
// workspace with NO explicit override + a non-platform-deriving model
// (kimi-for-coding → kimi-coding) resolves byok and must NOT get the CP proxy
// creds injected. (Pre-P2 this was driven by the org env MOLECULE_LLM_BILLING_MODE
// with an empty workspace id; that mechanism is retired.)
func TestApplyPlatformManagedLLMEnv_NoopsOutsidePlatformManaged(t *testing.T) {
	const wsID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	mock := setupTestDB(t)
	// No explicit override → derive from (claude-code, kimi-for-coding) → byok.
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(nil))

	t.Setenv("MOLECULE_LLM_BILLING_MODE", "") // no org default; derivation decides
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	envVars := map[string]string{}
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "claude-code", "kimi-for-coding", nil)

	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Fatalf("resolved mode = %q, want byok (derived from non-platform model)", res.ResolvedMode)
	}
	if _, ok := envVars["OPENAI_API_KEY"]; ok {
		t.Fatalf("OPENAI_API_KEY should not be set outside platform-managed mode")
	}
	if _, ok := envVars["MOLECULE_LLM_USAGE_TOKEN"]; ok {
		t.Fatalf("MOLECULE_LLM_USAGE_TOKEN should not be set outside platform-managed mode")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestApplyPlatformManagedLLMEnv_ClaudeCodeByokKeepsOwnProviderEnv is the
// internal#703 regression guard: a per-workspace byok override (org-level
// MOLECULE_LLM_BILLING_MODE left at the platform_managed bootstrap floor)
// must resolve to byok and leave the workspace own provider env intact —
// the CP-injected proxy ANTHROPIC_BASE_URL / usage token must NOT be forced,
// the OAuth token must NOT be stripped, and MOLECULE_LLM_BILLING_MODE in the
// container must read the RESOLVED mode (byok), not the hardcoded literal.
//
// This is the discriminating test for the byok end-to-end fix: pre-fix the
// strip path was the only emitter of MOLECULE_LLM_BILLING_MODE (hardcoded
// "platform_managed"), so a byok container carried no truthful billing mode.
func TestApplyPlatformManagedLLMEnv_ClaudeCodeByokKeepsOwnProviderEnv(t *testing.T) {
	const wsID = "77777777-7777-7777-7777-777777777777"
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModeBYOK))

	// Org-level env left at the bootstrap floor — the per-workspace override
	// is what must flip this workspace to byok (the realistic prod shape).
	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	// The workspace brought its own Claude Code OAuth token (BYOK via the
	// subscription provider). It must survive untouched.
	envVars := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "user-oauth-token",
		"MODEL":                   "sonnet",
	}
	applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "claude-code", "", nil)

	// 1. OAuth token intact — not stripped.
	if got := envVars["CLAUDE_CODE_OAUTH_TOKEN"]; got != "user-oauth-token" {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN = %q, want it left intact for byok", got)
	}
	// 2. No CP proxy base URL / usage token forced onto the workspace.
	if got, ok := envVars["ANTHROPIC_BASE_URL"]; ok {
		t.Fatalf("ANTHROPIC_BASE_URL must NOT be injected for byok, got %q", got)
	}
	if got, ok := envVars["ANTHROPIC_API_KEY"]; ok {
		t.Fatalf("ANTHROPIC_API_KEY must NOT be injected for byok, got %q", got)
	}
	if got, ok := envVars["MOLECULE_LLM_ANTHROPIC_BASE_URL"]; ok {
		t.Fatalf("MOLECULE_LLM_ANTHROPIC_BASE_URL must NOT be injected for byok, got %q", got)
	}
	if got, ok := envVars["MOLECULE_LLM_USAGE_TOKEN"]; ok {
		t.Fatalf("MOLECULE_LLM_USAGE_TOKEN must NOT be injected for byok, got %q", got)
	}
	// 3. Billing mode in the container reflects the RESOLVED mode (byok).
	if got := envVars["MOLECULE_LLM_BILLING_MODE"]; got != LLMBillingModeBYOK {
		t.Fatalf("MOLECULE_LLM_BILLING_MODE = %q, want %q (resolver-driven, not hardcoded)", got, LLMBillingModeBYOK)
	}
	if got := envVars["MOLECULE_LLM_BILLING_MODE_RESOLVED"]; got != LLMBillingModeBYOK {
		t.Fatalf("MOLECULE_LLM_BILLING_MODE_RESOLVED = %q, want %q", got, LLMBillingModeBYOK)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestApplyPlatformManagedLLMEnv_ClaudeCodeAttributionHeader is the CP#752 WS1b
// regression guard: platform-managed claude-code workspaces must send the
// per-workspace attribution header on every proxied Anthropic call, while BYOK
// and non-claude-code platform-managed workspaces must NOT receive it.
func TestApplyPlatformManagedLLMEnv_ClaudeCodeAttributionHeader(t *testing.T) {
	const wsID = "88888888-8888-8888-8888-888888888888"
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModePlatformManaged))

	t.Setenv("MOLECULE_LLM_BILLING_MODE", "")
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	// Platform-managed claude-code: header present and exact.
	envVars := map[string]string{}
	applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "claude-code", "sonnet", nil)
	if got := envVars["ANTHROPIC_CUSTOM_HEADERS"]; got != "X-Molecule-Workspace-Id: "+wsID {
		t.Fatalf("ANTHROPIC_CUSTOM_HEADERS = %q, want X-Molecule-Workspace-Id: %s", got, wsID)
	}

	// Non-claude-code platform-managed runtime: header absent (only claude-code
	// uses the Anthropic CLI's ANTHROPIC_CUSTOM_HEADERS env var in this PR).
	envVars = map[string]string{}
	applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "codex", "gpt-5.5", nil)
	if got, ok := envVars["ANTHROPIC_CUSTOM_HEADERS"]; ok {
		t.Fatalf("ANTHROPIC_CUSTOM_HEADERS must NOT be injected for codex, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestApplyPlatformManagedLLMEnv_ClaudeCodeAttributionHeaderAbsentForBYOK(t *testing.T) {
	const wsID = "88888888-8888-8888-8888-888888888888"
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModeBYOK))

	t.Setenv("MOLECULE_LLM_BILLING_MODE", "")
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	// BYOK claude-code: header absent (BYOK workspaces do not use the CP proxy).
	envVars := map[string]string{}
	applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "claude-code", "", nil)
	if got, ok := envVars["ANTHROPIC_CUSTOM_HEADERS"]; ok {
		t.Fatalf("ANTHROPIC_CUSTOM_HEADERS must NOT be injected for byok, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestApplyPlatformManagedLLMEnv_ByokGlobalScopeOAuthSurvivesAndRunsDirect is
// the molecule-core#1994 (corrected-model) inversion of the former
// internal#711 strip test, exercised through applyPlatformManagedLLMEnv. The
// live failure this guards: the Reno Stars Marketing/SEO byok agents whose
// Claude oauth lives at GLOBAL scope (the tenant's own credential, shared
// across the tenant's workspaces) were stripped + failed-closed under the
// inverted "global == platform's own" premise → MISSING_BYOK_CREDENTIAL →
// dead. Under the corrected model `global_secrets` is the TENANT's store, so
// that oauth is exactly what byok runs on: it must SURVIVE and route direct.
//
// Mutation (load-bearing): re-add stripGlobalOriginLLMCreds on the byok branch
// → the oauth disappears → this test RED on both survival + HasUsableLLMCred.
func TestApplyPlatformManagedLLMEnv_ByokGlobalScopeOAuthSurvivesAndRunsDirect(t *testing.T) {
	const wsID = "352e3c2b-0546-4e9c-b487-1e2ff1cf29fc" // Reno Stars SEO agent
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModeBYOK))

	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	// The tenant's own oauth at GLOBAL scope (a global_secrets row). The
	// workspace set no separate row of its own; it relies on the tenant global.
	envVars := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "TENANT-OWN-GLOBAL-OAUTH",
		"MODEL":                   "opus",
	}

	res := applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "claude-code", "", nil)

	// 1. The tenant's own global-scope oauth SURVIVES — byok runs on it.
	if envVars["CLAUDE_CODE_OAUTH_TOKEN"] != "TENANT-OWN-GLOBAL-OAUTH" {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN = %q, want the tenant's own global-scope token preserved for byok", envVars["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	// 2. No CP proxy creds forced (byok = workspace talks to its own provider).
	if got, ok := envVars["ANTHROPIC_API_KEY"]; ok {
		t.Fatalf("ANTHROPIC_API_KEY must NOT be injected for byok, got %q", got)
	}
	if _, ok := envVars["MOLECULE_LLM_USAGE_TOKEN"]; ok {
		t.Fatalf("MOLECULE_LLM_USAGE_TOKEN must NOT be injected for byok")
	}
	// 3. byok WITH a usable credential → caller does NOT fail closed.
	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Fatalf("ResolvedMode = %q, want %q", res.ResolvedMode, LLMBillingModeBYOK)
	}
	if !res.HasUsableLLMCred {
		t.Fatalf("HasUsableLLMCred = false, want true (tenant's own global-scope oauth is the usable credential)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// =========================================================================
// internal#718 P2-B BEHAVIOR DELTA — billing/credential decision DERIVES the
// provider (no stored LLM_PROVIDER, no override). These three tests are the
// explicit delta the RFC calls out, exercised through the real provision path
// (applyPlatformManagedLLMEnv) with the registry derivation driving the mode:
//   - platform-derived → platform_managed → platform creds (UNCHANGED)
//   - non-platform-derived → byok → #1963 strip + fail-closed (THE FIX)
//   - unset model → platform default (CTO-confirmed)
// All use NO explicit override (override read returns NULL) so the DERIVATION
// is what decides — this is what supersedes #1966's stored-LLM_PROVIDER read.
// =========================================================================

// PLATFORM-DERIVED → UNCHANGED. A claude-code workspace with a platform-
// namespaced model (anthropic/claude-opus-4-7) derives to the closed `platform`
// provider → platform_managed → CP proxy creds injected, exactly as before.
func TestApplyPlatformManagedLLMEnv_DERIVED_PlatformModelKeepsPlatformCreds(t *testing.T) {
	const wsID = "11111111-2222-3333-4444-555555555555"
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(nil)) // NO override → derive

	t.Setenv("MOLECULE_LLM_BILLING_MODE", "") // no org default; derivation decides
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	envVars := map[string]string{}
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "claude-code", "anthropic/claude-opus-4-7", nil)

	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Fatalf("platform-derived model must resolve platform_managed, got %q (source=%s)", res.ResolvedMode, res.Source)
	}
	if res.Source != BillingModeSourceDerivedProvider {
		t.Errorf("source: got %q want derived_provider", res.Source)
	}
	// Platform path injects the CP proxy creds (UNCHANGED behavior).
	if got := envVars["ANTHROPIC_API_KEY"]; got != "tenant-admin-token" {
		t.Errorf("platform path must inject the CP proxy token as ANTHROPIC_API_KEY, got %q", got)
	}
	if !res.HasUsableLLMCred {
		t.Errorf("platform path always has a usable cred (the proxy token)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// NON-PLATFORM-DERIVED + NO CREDENTIAL AT ALL → byok + FAIL-CLOSED. This is
// the legitimate remaining fail-closed path under the corrected model
// (molecule-core#1994): a claude-code workspace with a non-platform model
// (kimi-for-coding → byok) and NO override and NO LLM credential at ANY scope
// (no global row, no workspace row) has nothing to run on → HasUsableLLMCred=
// false → caller (prepareProvisionContext) aborts MISSING_BYOK_CREDENTIAL. The
// fail-closed branch is unchanged by the strip removal; only its trigger
// narrowed from "no workspace-scoped cred" to "no cred at any scope".
func TestApplyPlatformManagedLLMEnv_DERIVED_ByokNoCredentialFailsClosed(t *testing.T) {
	const wsID = "99999999-8888-7777-6666-555555555555"
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(nil)) // NO override → derive

	t.Setenv("MOLECULE_LLM_BILLING_MODE", "") // no org default; derivation decides
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	// No LLM credential at all — neither global nor workspace scope.
	envVars := map[string]string{}

	res := applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "claude-code", "kimi-for-coding", nil)

	// 1. DERIVED byok (NOT the old platform_managed default).
	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Fatalf("non-platform-derived model must resolve byok, got %q (source=%s)", res.ResolvedMode, res.Source)
	}
	if res.Source != BillingModeSourceDerivedProvider {
		t.Errorf("source: got %q want derived_provider", res.Source)
	}
	// 2. No CP proxy creds forced.
	if got, ok := envVars["ANTHROPIC_API_KEY"]; ok {
		t.Fatalf("ANTHROPIC_API_KEY must NOT be injected for byok, got %q", got)
	}
	// 3. No usable cred at any scope → caller fails closed.
	if res.HasUsableLLMCred {
		t.Fatalf("HasUsableLLMCred = true, want false (no LLM credential present at any scope)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// UNSET model → PLATFORM DEFAULT (CTO-confirmed "unset → platform default").
// No model means nothing to derive; the workspace defaults closed to
// platform_managed and keeps the platform creds (UNCHANGED for the no-model case).
func TestApplyPlatformManagedLLMEnv_DERIVED_UnsetModelPlatformDefault(t *testing.T) {
	const wsID = "00000000-1111-2222-3333-444444444444"
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(nil)) // NO override

	t.Setenv("MOLECULE_LLM_BILLING_MODE", "") // no org default; derivation decides
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	envVars := map[string]string{}
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "claude-code", "", nil)

	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Fatalf("unset model must default platform_managed, got %q (source=%s)", res.ResolvedMode, res.Source)
	}
	if res.Source != BillingModeSourceDerivedDefault {
		t.Errorf("source: got %q want derived_default", res.Source)
	}
	if got := envVars["ANTHROPIC_API_KEY"]; got != "tenant-admin-token" {
		t.Errorf("unset-model platform default must inject the CP proxy token, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestApplyPlatformManagedLLMEnv_ByokKeepsWorkspaceOwnOAuth is the
// workspace-scope companion to the global-scope survival test: a byok
// workspace that set its own CLAUDE_CODE_OAUTH_TOKEN via the canvas Secrets
// tab (a workspace_secrets row) keeps it and runs direct. Under the corrected
// model (molecule-core#1994) the tenant's credential survives at EITHER scope;
// this pins the workspace-scope half.
func TestApplyPlatformManagedLLMEnv_ByokKeepsWorkspaceOwnOAuth(t *testing.T) {
	const wsID = "6b66de8d-9337-4fb4-be8d-6d49dca0d809" // Reno Stars Marketing agent
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModeBYOK))

	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	// Workspace set its OWN OAuth token (a workspace_secrets row).
	envVars := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "CUSTOMER-OWN-OAUTH-TOKEN",
		"MODEL":                   "opus",
	}

	res := applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "claude-code", "", nil)

	if got := envVars["CLAUDE_CODE_OAUTH_TOKEN"]; got != "CUSTOMER-OWN-OAUTH-TOKEN" {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN = %q, want the workspace's own token left intact", got)
	}
	if !res.HasUsableLLMCred {
		t.Fatalf("HasUsableLLMCred = false, want true (workspace brought its own credential)")
	}
	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Fatalf("ResolvedMode = %q, want %q", res.ResolvedMode, LLMBillingModeBYOK)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestApplyPlatformManagedLLMEnv_DisabledKeepsTenantGlobalNoProxy proves the
// corrected-model behavior for "disabled": the tenant's own global-scope LLM
// cred is NOT stripped and the CP proxy is NOT forced. "disabled" means the
// workspace runs no platform-billed LLM, but the tenant's own credential is
// still the tenant's to keep; the caller's fail-closed abort is byok-only so a
// disabled workspace boots regardless. The previous internal#711 behavior
// stripped the global cred here on the same inverted premise; that strip is
// removed.
//
// Mutation (load-bearing): re-add stripGlobalOriginLLMCreds on the non-platform
// branch → the oauth disappears → this test RED on the survival assertion.
func TestApplyPlatformManagedLLMEnv_DisabledKeepsTenantGlobalNoProxy(t *testing.T) {
	const wsID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModeDisabled))

	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)

	envVars := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "TENANT-OWN-GLOBAL-OAUTH",
	}

	res := applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "claude-code", "", nil)

	// The tenant's own global cred survives (not stripped).
	if envVars["CLAUDE_CODE_OAUTH_TOKEN"] != "TENANT-OWN-GLOBAL-OAUTH" {
		t.Fatalf("tenant's own global cred must survive for disabled mode; got %q", envVars["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	// No proxy forced for disabled.
	if _, ok := envVars["MOLECULE_LLM_USAGE_TOKEN"]; ok {
		t.Fatalf("disabled must not inject the platform usage token")
	}
	if res.ResolvedMode != LLMBillingModeDisabled {
		t.Fatalf("ResolvedMode = %q, want %q", res.ResolvedMode, LLMBillingModeDisabled)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestApplyPlatformManagedLLMEnv_PlatformManagedStillReceivesGlobalCreds is
// the no-regression guard for the metered platform_managed path
// (molecule-core#1994): a platform-managed workspace MUST still strip any
// direct oauth and route through the CP proxy. The direct OAuth token is
// replaced by the proxy usage token (HasUsableLLMCred=true). This path is
// UNCHANGED by the byok strip removal — only the byok/disabled branch changed.
func TestApplyPlatformManagedLLMEnv_PlatformManagedStillReceivesGlobalCreds(t *testing.T) {
	const wsID = "99999999-9999-9999-9999-999999999999"
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModePlatformManaged))

	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	envVars := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "DIRECT-OAUTH-TOKEN",
		"MODEL":                   "opus",
	}

	res := applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "claude-code", "", nil)

	// Platform-managed routes through the CP proxy: OAuth stripped, proxy creds forced.
	if _, ok := envVars["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN should be stripped + replaced by the proxy token for platform_managed")
	}
	if got := envVars["ANTHROPIC_API_KEY"]; got != "tenant-admin-token" {
		t.Fatalf("ANTHROPIC_API_KEY = %q, want proxy usage token for platform_managed", got)
	}
	if !res.HasUsableLLMCred {
		t.Fatalf("HasUsableLLMCred = false, want true for platform_managed (proxy token is the credential)")
	}
	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Fatalf("ResolvedMode = %q, want %q", res.ResolvedMode, LLMBillingModePlatformManaged)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestApplyPlatformManagedLLMEnv_PlatformManagedStillEmitsResolvedMode is the
// no-regression companion: a workspace that resolves to platform_managed must
// still strip + force the proxy AND emit MOLECULE_LLM_BILLING_MODE=
// platform_managed (now resolver-driven, internal#703). Proves the byok fix
// did not alter the platform_managed contract.
func TestApplyPlatformManagedLLMEnv_PlatformManagedStillEmitsResolvedMode(t *testing.T) {
	const wsID = "88888888-8888-8888-8888-888888888888"
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModePlatformManaged))

	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	envVars := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "user-oauth-token",
		"MODEL":                   "sonnet",
	}
	applyPlatformManagedLLMEnv(context.Background(), envVars, wsID, "claude-code", "", nil)

	// OAuth stripped, proxy forced — unchanged platform_managed contract.
	if _, ok := envVars["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN should be stripped for platform_managed")
	}
	if got := envVars["ANTHROPIC_BASE_URL"]; got != "https://api.example.test/api/v1/internal/llm/anthropic" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want proxy forced for platform_managed", got)
	}
	if got := envVars["ANTHROPIC_API_KEY"]; got != "tenant-admin-token" {
		t.Fatalf("ANTHROPIC_API_KEY = %q, want usage token for platform_managed", got)
	}
	if got := envVars["MOLECULE_LLM_BILLING_MODE"]; got != LLMBillingModePlatformManaged {
		t.Fatalf("MOLECULE_LLM_BILLING_MODE = %q, want %q", got, LLMBillingModePlatformManaged)
	}
	if got := envVars["MOLECULE_LLM_BILLING_MODE_RESOLVED"]; got != LLMBillingModePlatformManaged {
		t.Fatalf("MOLECULE_LLM_BILLING_MODE_RESOLVED = %q, want %q", got, LLMBillingModePlatformManaged)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
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

// TestPrepareProvisionContext_NoModelFailsClosed is the core#2594 universal
// model gate: a platform-managed workspace that reaches provisioning with NO
// model (none in the payload, none stored) must abort MISSING_MODEL rather than
// launch on the runtime's opaque default. The CP proxy env is supplied so the
// credential gate passes and we reach the model gate.
func TestPrepareProvisionContext_NoModelFailsClosed(t *testing.T) {
	const wsID = "ws-no-model-2594"
	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	// No stored MODEL — the workspace_secrets result is empty.
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))

	handler := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	// No payload model, ordinary (non-platform) workspace → applyConciergeProvisionConfig
	// is a no-op (the kind probe returns "workspace"), so nothing sets a model.
	payload := models.CreateWorkspacePayload{
		Name:    "no-model",
		Runtime: "claude-code",
		Tier:    1,
	}
	prepared, abort := handler.prepareProvisionContext(
		context.Background(), wsID, "/nonexistent", nil, payload, false)

	if abort == nil {
		t.Fatalf("expected MISSING_MODEL abort, got success (prepared=%v)", prepared)
	}
	if code, _ := abort.Extra["code"].(string); code != "MISSING_MODEL" {
		t.Fatalf("abort.Extra[code] = %v, want MISSING_MODEL", abort.Extra["code"])
	}
}
