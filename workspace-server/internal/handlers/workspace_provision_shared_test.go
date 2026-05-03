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
// created, but neither MODEL_PROVIDER nor LLM_PROVIDER was ever
// written to workspace_secrets. On any subsequent restart, the
// applyRuntimeModelEnv fallback found nothing in envVars["MODEL_PROVIDER"]
// and hermes booted with the template default (nousresearch/hermes-4-70b)
// → wrong provider keys → /health poll failed → never registered.
//
// Post-fix: the create handler writes both rows after committing the
// workspace row. This test asserts the SQL writes happen with the
// correct keys + values.
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

	// The fix: MODEL_PROVIDER is upserted with the verbatim model slug.
	// SQL has 3 placeholders ($1=workspace_id, $2=encrypted_value reused
	// in the conflict-update, $3=version reused in the conflict-update),
	// so sqlmock sees 3 args. The 'MODEL_PROVIDER' / 'LLM_PROVIDER' key
	// is a literal in the SQL — we distinguish the two writes with the
	// regex match below.
	mock.ExpectExec(`INSERT INTO workspace_secrets[\s\S]*'MODEL_PROVIDER'`).
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
		t.Errorf("sqlmock expectations not met — first-deploy did NOT persist MODEL_PROVIDER + LLM_PROVIDER (this is the prod bug recurrence): %v", err)
	}
}

// TestWorkspaceCreate_FirstDeploy_NoModel_NoSecretWritten asserts that
// when payload.Model is empty, NEITHER MODEL_PROVIDER nor LLM_PROVIDER
// is written. Important: the canvas can omit `model` (template inherits
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
// MODEL_PROVIDER persisted (so the user's exact slug survives restart
// and applyRuntimeModelEnv finds it), but LLM_PROVIDER is skipped (so
// derive-provider.sh's *=auto branch can decide at runtime instead of
// being pre-empted by a guess).
func TestWorkspaceCreate_FirstDeploy_UnknownModel_OnlyMintModelProvider(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Only MODEL_PROVIDER — LLM_PROVIDER must NOT be written for
	// unknown prefixes. Same 3-arg shape as above; key is literal in SQL.
	mock.ExpectExec(`INSERT INTO workspace_secrets[\s\S]*'MODEL_PROVIDER'`).
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
		t.Errorf("sqlmock expectations not met — unknown-prefix model should mint MODEL_PROVIDER but skip LLM_PROVIDER: %v", err)
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
		wantMODEL         string
		wantHermesDefault string // empty string = must be unset
	}{
		{
			name:      "claude-code: picked model populates MODEL",
			runtime:   "claude-code",
			model:     "MiniMax-M2",
			wantMODEL: "MiniMax-M2",
		},
		{
			name:              "hermes: picked model populates BOTH MODEL and HERMES_DEFAULT_MODEL",
			runtime:           "hermes",
			model:             "minimax/MiniMax-M2.7",
			wantMODEL:         "minimax/MiniMax-M2.7",
			wantHermesDefault: "minimax/MiniMax-M2.7",
		},
		{
			name:      "langgraph: picked model populates MODEL (no vendor-specific name)",
			runtime:   "langgraph",
			model:     "anthropic:claude-opus-4-7",
			wantMODEL: "anthropic:claude-opus-4-7",
		},
		{
			name:      "crewai: picked model populates MODEL (no vendor-specific name)",
			runtime:   "crewai",
			model:     "openai:gpt-4o",
			wantMODEL: "openai:gpt-4o",
		},
		{
			name:    "empty model + empty MODEL_PROVIDER fallback: nothing set",
			runtime: "claude-code",
			model:   "",
		},
		{
			name:             "empty model + MODEL_PROVIDER fallback hits: MODEL set from secret",
			runtime:          "claude-code",
			model:            "",
			modelProviderEnv: "MiniMax-M2",
			wantMODEL:        "MiniMax-M2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envVars := map[string]string{}
			if tc.modelProviderEnv != "" {
				envVars["MODEL_PROVIDER"] = tc.modelProviderEnv
			}
			applyRuntimeModelEnv(envVars, tc.runtime, tc.model)

			if got := envVars["MODEL"]; got != tc.wantMODEL {
				t.Errorf("MODEL = %q, want %q", got, tc.wantMODEL)
			}
			if got := envVars["HERMES_DEFAULT_MODEL"]; got != tc.wantHermesDefault {
				t.Errorf("HERMES_DEFAULT_MODEL = %q, want %q", got, tc.wantHermesDefault)
			}
		})
	}
}
