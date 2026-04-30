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
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
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
