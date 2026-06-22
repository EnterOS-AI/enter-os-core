package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestInstallPlatformAgent_BadJSON rejects a payload missing the required id
// before touching the DB (binding:"required" on ID).
func TestInstallPlatformAgent_BadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/org/platform-agent",
		bytes.NewBufferString(`{"name":"Org Concierge"}`)) // no id
	c.Request.Header.Set("Content-Type", "application/json")

	InstallPlatformAgent(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("missing id: expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDefaultPlatformAgentName covers the dynamic "<org name> Agent" name and
// the legacy fallback. MOLECULE_ORG_NAME set → "<org> Agent"; unset → the
// "Org Concierge" default used by both the self-host seed and the CP install
// when no explicit name is passed.
func TestDefaultPlatformAgentName(t *testing.T) {
	t.Run("org name set", func(t *testing.T) {
		t.Setenv("MOLECULE_ORG_NAME", "Molecule AI")
		if got := defaultPlatformAgentName(); got != "Molecule AI Agent" {
			t.Errorf("defaultPlatformAgentName() = %q, want %q", got, "Molecule AI Agent")
		}
	})
	t.Run("org name empty → legacy fallback", func(t *testing.T) {
		t.Setenv("MOLECULE_ORG_NAME", "")
		if got := defaultPlatformAgentName(); got != "Org Concierge" {
			t.Errorf("defaultPlatformAgentName() = %q, want %q", got, "Org Concierge")
		}
	})
}

// TestOrgIdentity asserts the open /org/identity contract: {"name": <env>}.
func TestOrgIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("returns configured org name, slug and id (SaaS)", func(t *testing.T) {
		t.Setenv("MOLECULE_ORG_NAME", "Molecule AI")
		t.Setenv("MOLECULE_ORG_SLUG", "molecule-ai")
		t.Setenv("MOLECULE_ORG_ID", "11111111-2222-3333-4444-555555555555")
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/org/identity", nil)

		OrgIdentity(c)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var body struct {
			Name  string `json:"name"`
			Slug  string `json:"slug"`
			OrgID string `json:"org_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
		}
		if body.Name != "Molecule AI" {
			t.Errorf("name = %q, want %q", body.Name, "Molecule AI")
		}
		if body.Slug != "molecule-ai" {
			t.Errorf("slug = %q, want %q", body.Slug, "molecule-ai")
		}
		if body.OrgID != "11111111-2222-3333-4444-555555555555" {
			t.Errorf("org_id = %q, want the configured uuid", body.OrgID)
		}
	})

	t.Run("name/slug/org_id empty when unset (self-host)", func(t *testing.T) {
		t.Setenv("MOLECULE_ORG_NAME", "")
		t.Setenv("MOLECULE_ORG_SLUG", "")
		t.Setenv("MOLECULE_ORG_ID", "")
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/org/identity", nil)

		OrgIdentity(c)

		var body struct {
			Name  string `json:"name"`
			Slug  string `json:"slug"`
			OrgID string `json:"org_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if body.Name != "" {
			t.Errorf("name = %q, want empty string", body.Name)
		}
		if body.Slug != "" {
			t.Errorf("slug = %q, want empty string", body.Slug)
		}
		if body.OrgID != "" {
			t.Errorf("org_id = %q, want empty string", body.OrgID)
		}
	})

	// platform_managed_available reflects whether a Molecule LLM proxy is wired
	// into the process env — true on SaaS (proxy base URL + usage token set),
	// false on self-host (neither set). The canvas reads it to hide/show the
	// "Platform (proxy)" billing option pre-login.
	t.Run("platform_managed_available true when proxy configured (SaaS)", func(t *testing.T) {
		t.Setenv("MOLECULE_LLM_BASE_URL", "https://proxy.example/v1")
		t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tok-test")
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/org/identity", nil)

		OrgIdentity(c)

		var body struct {
			PlatformManagedAvailable bool `json:"platform_managed_available"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
		}
		if !body.PlatformManagedAvailable {
			t.Errorf("platform_managed_available = false, want true (proxy configured)")
		}
	})

	t.Run("platform_managed_available false when no proxy (self-host)", func(t *testing.T) {
		// Clear every proxy env so neither the molecule nor openai alias is set.
		t.Setenv("MOLECULE_LLM_BASE_URL", "")
		t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "")
		t.Setenv("OPENAI_BASE_URL", "")
		t.Setenv("OPENAI_API_KEY", "")
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/org/identity", nil)

		OrgIdentity(c)

		var body struct {
			PlatformManagedAvailable bool `json:"platform_managed_available"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
		}
		if body.PlatformManagedAvailable {
			t.Errorf("platform_managed_available = true, want false (no proxy / self-host)")
		}
	})

	t.Run("platform_managed_available true via openai alias env", func(t *testing.T) {
		// The proxy can also be wired via the OPENAI_* aliases (non-anthropic
		// runtimes). Either pair satisfies the signal.
		t.Setenv("MOLECULE_LLM_BASE_URL", "")
		t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "")
		t.Setenv("OPENAI_BASE_URL", "https://proxy.example/v1")
		t.Setenv("OPENAI_API_KEY", "tok-test")
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/org/identity", nil)

		OrgIdentity(c)

		var body struct {
			PlatformManagedAvailable bool `json:"platform_managed_available"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
		}
		if !body.PlatformManagedAvailable {
			t.Errorf("platform_managed_available = false, want true (openai alias proxy env)")
		}
	})
}

// stubBootProv is a minimal localProvisionerIsRunning for the boot-provision
// helper test — no Docker daemon required. It deliberately does NOT implement
// ExecRead, so conciergeIdentityPresent's type-assertion misses and a running
// container is treated as already-identified (skip) — the legacy behaviour.
type stubBootProv struct {
	running    bool
	calledWith string
}

func (s *stubBootProv) IsRunning(_ context.Context, id string) (bool, error) {
	s.calledWith = id
	return s.running, nil
}

// stubBootProvExec adds ExecRead so the boot helper can probe for the concierge
// identity on a RUNNING container — the path that restarts a running-but-vanilla
// concierge so it picks up the seeded overlay.
type stubBootProvExec struct {
	stubBootProv
	systemPrompt string // returned for /configs/system-prompt.md; "" with execErr to simulate a probe miss
	execErr      error
}

func (s *stubBootProvExec) ExecRead(_ context.Context, _ /*container*/, _ /*path*/ string) ([]byte, error) {
	if s.execErr != nil {
		return nil, s.execErr
	}
	return []byte(s.systemPrompt), nil
}

const bootPlatformID = "11111111-2222-3333-4444-555555555555"

// TestMaybeProvisionPlatformAgentOnBoot_KicksOffWhenNotRunning: row present +
// container not running ⇒ RestartByID is invoked with the platform agent's id.
func TestMaybeProvisionPlatformAgentOnBoot_KicksOffWhenNotRunning(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, status FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(bootPlatformID, "failed"))

	prov := &stubBootProv{running: false}
	done := make(chan string, 1)
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, prov, func(id string) {
		done <- id
	})

	select {
	case got := <-done:
		if got != bootPlatformID {
			t.Errorf("RestartByID called with %q, want %q", got, bootPlatformID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("RestartByID was not called within timeout")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestMaybeProvisionPlatformAgentOnBoot_SkipsWhenRunning: container already
// running ⇒ RestartByID is NOT called.
func TestMaybeProvisionPlatformAgentOnBoot_SkipsWhenRunning(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, status FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(bootPlatformID, "online"))

	prov := &stubBootProv{running: true}
	called := make(chan string, 1)
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, prov, func(id string) {
		called <- id
	})

	select {
	case got := <-called:
		t.Fatalf("RestartByID should not have been called, got %q", got)
	case <-time.After(200 * time.Millisecond):
		// expected: no call
	}
}

// TestMaybeProvisionPlatformAgentOnBoot_NoRowNoOp: no platform agent row ⇒
// no provision, no panic.
func TestMaybeProvisionPlatformAgentOnBoot_NoRowNoOp(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, status FROM workspaces WHERE kind = 'platform'`).
		WillReturnError(sql.ErrNoRows)

	prov := &stubBootProv{running: false}
	called := make(chan string, 1)
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, prov, func(id string) {
		called <- id
	})

	select {
	case got := <-called:
		t.Fatalf("RestartByID should not have been called, got %q", got)
	case <-time.After(200 * time.Millisecond):
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestMaybeProvisionPlatformAgentOnBoot_NilGuards: nil prov or nil restartFn ⇒
// no-op (no DB access, no panic).
func TestMaybeProvisionPlatformAgentOnBoot_NilGuards(t *testing.T) {
	mock := setupTestDB(t)
	// No ExpectQuery — the helper must return before touching the DB.
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, nil, func(string) {})
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, &stubBootProv{}, nil)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (should have made no queries): %v", err)
	}
}

// TestMaybeProvisionPlatformAgentOnBoot_RestartsRunningButVanilla: a RUNNING
// concierge whose /configs/system-prompt.md lacks the identity (a pre-overlay
// boot) is restarted ONCE so the provision path re-seeds the concierge config.
func TestMaybeProvisionPlatformAgentOnBoot_RestartsRunningButVanilla(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, status FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(bootPlatformID, "online"))

	// The running-but-vanilla path re-declares the management MCP plugin before
	// restarting so the post-restart boot-install sees the declaration.
	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =`
	mock.ExpectQuery(kindQuery).WithArgs(bootPlatformID).
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
		WithArgs(bootPlatformID, conciergePlatformMCPName, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Running, but ExecRead of system-prompt.md returns vanilla content (no
	// "Org Concierge") → identity absent → restart.
	prov := &stubBootProvExec{stubBootProv: stubBootProv{running: true}, systemPrompt: "generic coding assistant"}
	done := make(chan string, 1)
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, prov, func(id string) { done <- id })

	select {
	case got := <-done:
		if got != bootPlatformID {
			t.Errorf("RestartByID called with %q, want %q", got, bootPlatformID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("RestartByID was not called for a running-but-vanilla concierge")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestMaybeProvisionPlatformAgentOnBoot_SkipsRunningWithIdentity: a RUNNING
// concierge that already carries the Org-Concierge identity is left alone.
func TestMaybeProvisionPlatformAgentOnBoot_SkipsRunningWithIdentity(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, status FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(bootPlatformID, "online"))

	prov := &stubBootProvExec{stubBootProv: stubBootProv{running: true}, systemPrompt: "# You are Molecule AI Agent — the Org Concierge"}
	called := make(chan string, 1)
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, prov, func(id string) { called <- id })

	select {
	case got := <-called:
		t.Fatalf("RestartByID should not have been called (identity present), got %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestConciergeIdentityFiles asserts the overlay: a system-prompt.md carrying
// the Org-Concierge identity, and a config.yaml that gains the platform
// mcp_servers entry — appended idempotently onto the base config.
// TestSubstituteConciergeName asserts the {{CONCIERGE_NAME}} substitution
// used by applyConciergeProvisionConfig to bake the per-instance concierge
// name into the template-delivered system-prompt.md at provision time
// (RFC #2843 §10a; PR recommendation (a) — substitute, per the driver's
// "you decide in review" call). The runtime's build_system_prompt does NOT
// template prompt files, so this is the only place the per-instance name
// reaches the agent. Idempotent: re-substituting a name into a
// already-substituted prompt is a no-op (the placeholder is gone).
func TestSubstituteConciergeName(t *testing.T) {
	tmpl := []byte("# You are {{CONCIERGE_NAME}} — the Org Concierge\n\n" +
		"You are the organization's **platform agent**.\n")

	t.Run("replaces the placeholder with the per-instance name", func(t *testing.T) {
		got := substituteConciergeName(tmpl, "Molecule AI Agent")
		if !strings.Contains(string(got), "Molecule AI Agent") {
			t.Errorf("substituted prompt missing the name:\n%s", got)
		}
		if strings.Contains(string(got), "{{CONCIERGE_NAME}}") {
			t.Errorf("placeholder survived substitution:\n%s", got)
		}
	})

	t.Run("replaces all occurrences (not just the first)", func(t *testing.T) {
		multi := []byte("{{CONCIERGE_NAME}} sees {{CONCIERGE_NAME}} and only {{CONCIERGE_NAME}} acts.")
		got := substituteConciergeName(multi, "Mia")
		want := "Mia sees Mia and only Mia acts."
		if string(got) != want {
			t.Errorf("multi-occurrence substitution:\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("is a no-op when the placeholder is absent (idempotent re-provision)", func(t *testing.T) {
		alreadySubstituted := []byte("# You are Mia — the Org Concierge\n")
		got := substituteConciergeName(alreadySubstituted, "Mia")
		if string(got) != string(alreadySubstituted) {
			t.Errorf("idempotent re-provision changed the prompt:\n got: %q\nwant: %q", got, alreadySubstituted)
		}
	})

	t.Run("empty prompt is a no-op (don't panic on len=0)", func(t *testing.T) {
		if got := substituteConciergeName(nil, "Mia"); got != nil {
			t.Errorf("nil prompt should round-trip; got %q", got)
		}
		if got := substituteConciergeName([]byte{}, "Mia"); len(got) != 0 {
			t.Errorf("empty prompt should round-trip; got %q", got)
		}
	})
}

// TestConciergePlatformMCPEnv asserts the platform-MCP env wiring: ADMIN_TOKEN →
// MOLECULE_API_KEY, PLATFORM_URL → MOLECULE_API_URL fallback, and that an
// already-present value is never clobbered.
func TestConciergePlatformMCPEnv(t *testing.T) {
	t.Run("wires from ADMIN_TOKEN + PLATFORM_URL", func(t *testing.T) {
		t.Setenv("ADMIN_TOKEN", "admintok")
		t.Setenv("MOLECULE_API_URL", "")
		t.Setenv("PLATFORM_URL", "http://platform:8080")
		t.Setenv("MOLECULE_ORG_ID", "org-123")
		env := map[string]string{}
		conciergePlatformMCPEnv(env)
		if env["MOLECULE_API_KEY"] != "admintok" {
			t.Errorf("MOLECULE_API_KEY = %q, want admintok", env["MOLECULE_API_KEY"])
		}
		if env["MOLECULE_API_URL"] != "http://platform:8080" {
			t.Errorf("MOLECULE_API_URL = %q, want platform url fallback", env["MOLECULE_API_URL"])
		}
		if env["MOLECULE_ORG_ID"] != "org-123" {
			t.Errorf("MOLECULE_ORG_ID = %q, want org-123", env["MOLECULE_ORG_ID"])
		}
	})

	t.Run("does not clobber existing values", func(t *testing.T) {
		t.Setenv("ADMIN_TOKEN", "admintok")
		env := map[string]string{"MOLECULE_API_KEY": "preset"}
		conciergePlatformMCPEnv(env)
		if env["MOLECULE_API_KEY"] != "preset" {
			t.Errorf("MOLECULE_API_KEY overwritten to %q, want preset preserved", env["MOLECULE_API_KEY"])
		}
	})

	t.Run("MOLECULE_API_URL prefers explicit over PLATFORM_URL", func(t *testing.T) {
		t.Setenv("MOLECULE_API_URL", "http://explicit:9000")
		t.Setenv("PLATFORM_URL", "http://platform:8080")
		env := map[string]string{}
		conciergePlatformMCPEnv(env)
		if env["MOLECULE_API_URL"] != "http://explicit:9000" {
			t.Errorf("MOLECULE_API_URL = %q, want the explicit env", env["MOLECULE_API_URL"])
		}
	})
}

// TestApplyConciergeProvisionConfig_OnlyPlatformGetsOrgMCP locks the security
// invariant the user requires: ONLY the tenant-native concierge (kind='platform')
// receives the org/platform MCP + the org-admin token. An ordinary workspace must
// NOT get the platform MCP config, the system prompt, or MOLECULE_API_KEY (the
// org-admin credential) natively — otherwise any workspace could drive org-admin
// actions (create_workspace, set_secret, …). Gate is keyed off the DB kind column
// (SSOT, protected by the one-platform-root CHECK constraint).
//
// Post RFC #2843 §10a: the concierge's identity (system prompt, model, MCP
// declaration) is delivered via the platform-agent template. The provision
// hook's only remaining work is (1) inject the platform-MCP env and (2) the
// {{CONCIERGE_NAME}} substitution in the template-delivered system-prompt.md.
// This test asserts both halves: the security boundary (only kind=platform gets
// the org-admin token) AND the new substitution behavior.
func TestApplyConciergeProvisionConfig_OnlyPlatformGetsOrgMCP(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "secret-org-admin")
	t.Setenv("PLATFORM_URL", "http://platform:8080")
	h := &WorkspaceHandler{}
	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =`
	// ensureConciergeModel (step 0, platform kind only) reads the stored MODEL
	// secret to decide seed-vs-respect. These subtests are about MCP/name, so
	// they stub an EXISTING model → ensureConciergeModel returns early (no
	// INSERT). The seed path itself is covered by
	// TestApplyConciergeProvisionConfig_SeedsModel.
	const modelSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`
	// ensureConciergeProvider (step 0b, platform kind only) reads the stored
	// LLM_PROVIDER secret to decide seed-vs-respect. In these MCP/name subtests
	// the test env carries no MODEL (loadWorkspaceSecrets is not run), so the
	// provider gate (platform-managed model namespace) is not met and NO
	// LLM_PROVIDER INSERT fires — only the existence SELECT. The seed itself is
	// covered by TestApplyConciergeProvisionConfig_SeedsProvider.
	const providerSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'LLM_PROVIDER'`
	const declaredInsert = `INSERT INTO workspace_declared_plugins`

	t.Run("ordinary workspace gets NO org MCP, NO admin token, NO substitution", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-ordinary").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))
		env := map[string]string{}
		// Ordinary workspaces may have a non-concierge system-prompt.md in
		// configFiles; the hook must NOT touch it.
		cf := map[string][]byte{
			"config.yaml":      []byte("runtime: claude-code\n"),
			"system-prompt.md": []byte("{{CONCIERGE_NAME}} substitute me"),
		}
		out := h.applyConciergeProvisionConfig(context.Background(), "ws-ordinary", "", cf, env, "Worker")
		if _, ok := env["MOLECULE_API_KEY"]; ok {
			t.Errorf("SECURITY: ordinary workspace leaked MOLECULE_API_KEY (org-admin token): %v", env)
		}
		if _, ok := env["MOLECULE_ORG_API_KEY"]; ok {
			t.Errorf("SECURITY: ordinary workspace leaked MOLECULE_ORG_API_KEY: %v", env)
		}
		if strings.Contains(string(out["system-prompt.md"]), "Worker") {
			t.Errorf("ordinary workspace had its system-prompt substituted — the concierge hook must no-op for kind != platform; got:\n%s", out["system-prompt.md"])
		}
		// CR2 RC 11903 SA9003: the previous assertion
		// ('ordinary workspace had its system-prompt substituted — the
		// concierge hook must no-op for kind != platform') is the
		// load-bearing check. A separate '{{CONCIERGE_NAME}}'-survives
		// assertion would be tautological here (ordinary workspaces
		// legitimately carry the placeholder; the hook only runs for
		// kind=platform). Removed the dead if-block.
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("platform agent gets org MCP env + admin token + {{CONCIERGE_NAME}} substitution", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("moonshot/kimi-k2.6"), 0))
		// ensureConciergeProvider existence check (env has no MODEL here → no pin).
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(kindQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-concierge", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{}
		cf := map[string][]byte{
			"config.yaml":      []byte("runtime: claude-code\nmodel: moonshot/kimi-k2.6\n"),
			"system-prompt.md": []byte("# You are {{CONCIERGE_NAME}} — the Org Concierge\n"),
		}
		out := h.applyConciergeProvisionConfig(context.Background(), "ws-concierge", "", cf, env, "Molecule AI Agent")
		if env["MOLECULE_API_KEY"] != "secret-org-admin" {
			t.Errorf("concierge did not receive the org-admin token; env=%v", env)
		}
		if env["MOLECULE_ORG_API_KEY"] != "secret-org-admin" {
			t.Errorf("management tools auth env (MOLECULE_ORG_API_KEY) missing; env=%v", env)
		}
		// The dispatch's recommendation (a): substitute the per-instance name
		// into the template-delivered system-prompt.md. Verify the
		// placeholder is gone and the name is baked in.
		if !strings.Contains(string(out["system-prompt.md"]), "Molecule AI Agent") {
			t.Errorf("{{CONCIERGE_NAME}} was not substituted with the per-instance name:\n%s", out["system-prompt.md"])
		}
		if strings.Contains(string(out["system-prompt.md"]), "{{CONCIERGE_NAME}}") {
			t.Errorf("{{CONCIERGE_NAME}} placeholder survived substitution:\n%s", out["system-prompt.md"])
		}
		// config.yaml must NOT have an mcp_servers block — the template's
		// mcp_servers.yaml overlay handles that, and the dispatch's explicit
		// directive is "PICK ONE, don't double-declare."
		if strings.Contains(string(out["config.yaml"]), "mcp_servers") {
			t.Errorf("config.yaml must not have an mcp_servers block (the seeded mcp_servers.yaml overlay handles it; got double-declaration):\n%s", out["config.yaml"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("idempotent re-provision on the platform agent (no double-substitution)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("moonshot/kimi-k2.6"), 0))
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(kindQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-concierge", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{}
		// Already-substituted prompt (a re-provision of a running concierge).
		cf := map[string][]byte{
			"system-prompt.md": []byte("# You are Molecule AI Agent — the Org Concierge\n"),
		}
		out := h.applyConciergeProvisionConfig(context.Background(), "ws-concierge", "", cf, env, "Molecule AI Agent")
		if !strings.Contains(string(out["system-prompt.md"]), "Molecule AI Agent") {
			t.Errorf("re-provision lost the name:\n%s", out["system-prompt.md"])
		}
		// Count of the name (must be exactly 1 — a naive re-substitute would
		// produce "Molecule AI Agent Molecule AI Agent" or similar).
		if n := strings.Count(string(out["system-prompt.md"]), "Molecule AI Agent"); n != 1 {
			t.Errorf("name appears %d times in re-provisioned prompt; want 1 (idempotent):\n%s", n, out["system-prompt.md"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}

// TestApplyConciergeProvisionConfig_SeedsModel is the CI regression gate for
// the 2026-06-15 incident: #2919 removed the concierge model-seed, so every
// fresh platform-agent provision reached the universal MISSING_MODEL gate
// (core#2594) with no stored model and failed closed ("reached provisioning
// with no model set"). It passed CI because no test (and no e2e) provisions a
// fresh platform agent through the model path. This test asserts the seed
// fires for a model-less platform agent, is SEED-ONLY (respects a customer's
// later pick), and never touches ordinary workspaces.
func TestApplyConciergeProvisionConfig_SeedsModel(t *testing.T) {
	h := &WorkspaceHandler{}
	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =`
	const modelSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`
	const providerSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'LLM_PROVIDER'`
	const declaredInsert = `INSERT INTO workspace_declared_plugins`
	const secretInsert = `INSERT INTO workspace_secrets`

	t.Run("fresh platform agent with NO stored model gets the declared model seeded + persisted", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-fresh").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		// No MODEL row yet (first boot) → readStoredModelSecret returns "".
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-fresh").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		// Seed path must PERSIST the declared model.
		mock.ExpectExec(secretInsert).
			WithArgs("ws-fresh", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		// ensureConciergeProvider: no LLM_PROVIDER yet → existence SELECT empty;
		// the just-seeded MODEL (moonshot/…) meets the platform namespace gate,
		// so the provider pin is PERSISTED too.
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-fresh").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-fresh", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(kindQuery).WithArgs("ws-fresh").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-fresh", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{}
		h.applyConciergeProvisionConfig(context.Background(), "ws-fresh", "", nil, env, "Org Concierge")

		// THE regression assertion: without this seed the provision hits
		// MISSING_MODEL and fails closed. Both canonical env names must carry
		// the declared model so the runtime actually boots on it this provision.
		if env["MODEL"] != conciergeDeclaredModel {
			t.Errorf("fresh concierge did not seed MODEL=%q; got %q (env=%v) — MISSING_MODEL would fail this provision closed", conciergeDeclaredModel, env["MODEL"], env)
		}
		if env["MOLECULE_MODEL"] != conciergeDeclaredModel {
			t.Errorf("fresh concierge did not seed MOLECULE_MODEL=%q; got %q", conciergeDeclaredModel, env["MOLECULE_MODEL"])
		}
		// Companion provider pin: the concierge can't run a turn without it
		// (moonshot/… derives a non-registry provider name → adapter fail-closes).
		if env["LLM_PROVIDER"] != conciergeProvider {
			t.Errorf("fresh concierge did not seed LLM_PROVIDER=%q; got %q (env=%v) — concierge would boot not_configured", conciergeProvider, env["LLM_PROVIDER"], env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (MODEL or LLM_PROVIDER secret not persisted): %v", err)
		}
	})

	t.Run("SEED-ONLY: an existing customer model is respected, never overwritten", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-picked").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		// Customer already picked a model — stored MODEL secret present.
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-picked").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("anthropic:claude-opus-4-8"), 0))
		// NO model ExpectExec: ensureConciergeModel must return early (no re-seed,
		// no INSERT) — re-asserting the default would silently revert the pick.
		// ensureConciergeProvider runs its existence SELECT, but the test env
		// carries no MODEL and the customer's model is non-platform-namespace, so
		// NO provider pin fires either.
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-picked").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))

		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(kindQuery).WithArgs("ws-picked").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-picked", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{}
		h.applyConciergeProvisionConfig(context.Background(), "ws-picked", "", nil, env, "Org Concierge")

		if env["MODEL"] == conciergeDeclaredModel {
			t.Errorf("seed-only violated: ensureConciergeModel overwrote the customer's model with the declared default")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (an unexpected INSERT means it re-seeded over the customer's pick): %v", err)
		}
	})

	t.Run("ordinary workspace never seeds a model (no model queries at all)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-ordinary").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))
		// No model SELECT/INSERT expected — the hook returns before step 0.

		env := map[string]string{}
		h.applyConciergeProvisionConfig(context.Background(), "ws-ordinary", "", nil, env, "Worker")

		if _, ok := env["MODEL"]; ok {
			t.Errorf("ordinary workspace had a model seeded — the concierge model-seed must be platform-kind only; env=%v", env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (ordinary workspace ran a model query): %v", err)
		}
	})
}

// TestApplyConciergeProvisionConfig_SeedsProvider is the CI regression gate for
// the concierge non-response incident (prod 2026-06-18): the concierge booted
// online but configuration_status=not_configured because the runtime wheel
// derives provider="moonshot" from the model id "moonshot/kimi-k2.6" (a
// model-PREFIX on the `platform` provider, NOT a provider NAME), and the
// claude-code adapter fail-closes. The template config.yaml `provider:` field
// does not reach the on-box config, so core MUST seed the LLM_PROVIDER env pin
// (the highest-precedence, restart-surviving signal). Verified on prod test3:
// setting LLM_PROVIDER=platform flipped not_configured → ready + responding.
func TestApplyConciergeProvisionConfig_SeedsProvider(t *testing.T) {
	h := &WorkspaceHandler{}
	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =`
	const modelSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`
	const providerSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'LLM_PROVIDER'`
	const declaredInsert = `INSERT INTO workspace_declared_plugins`
	const secretInsert = `INSERT INTO workspace_secrets`

	t.Run("existing platform-managed concierge with NO provider gets LLM_PROVIDER=platform pinned", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-heal").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		// Existing platform model → ensureConciergeModel respects it (no INSERT).
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-heal").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte(conciergeDeclaredModel), 0))
		// No LLM_PROVIDER yet → existence SELECT empty, then PERSIST the pin.
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-heal").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-heal", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		// Simulate loadWorkspaceSecrets having populated MODEL into the env
		// (the production precondition for an existing-model concierge).
		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(kindQuery).WithArgs("ws-heal").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-heal", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{"MODEL": conciergeDeclaredModel}
		h.applyConciergeProvisionConfig(context.Background(), "ws-heal", "", nil, env, "Org Concierge")

		if env["LLM_PROVIDER"] != conciergeProvider {
			t.Errorf("existing platform-managed concierge did not get LLM_PROVIDER=%q pinned; got %q (env=%v)", conciergeProvider, env["LLM_PROVIDER"], env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (LLM_PROVIDER pin not persisted): %v", err)
		}
	})

	t.Run("SEED-ONLY: a customer-picked provider is respected, never overwritten", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-prov-picked").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-prov-picked").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte(conciergeDeclaredModel), 0))
		// Customer already pinned a provider in the canvas → existence SELECT
		// returns it → NO INSERT (respecting the pick).
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-prov-picked").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("anthropic-api"), 0))

		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(kindQuery).WithArgs("ws-prov-picked").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-prov-picked", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{"MODEL": conciergeDeclaredModel, "LLM_PROVIDER": "anthropic-api"}
		h.applyConciergeProvisionConfig(context.Background(), "ws-prov-picked", "", nil, env, "Org Concierge")

		if env["LLM_PROVIDER"] != "anthropic-api" {
			t.Errorf("seed-only violated: overwrote the customer's provider pick (got %q)", env["LLM_PROVIDER"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (an unexpected INSERT means it re-pinned over the customer's pick): %v", err)
		}
	})

	t.Run("non-platform model namespace does NOT get a platform provider pin", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-byok").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-byok").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("sonnet"), 0))
		// Existence SELECT runs; model "sonnet" resolves on its own (anthropic-
		// oauth alias), so the gate is NOT met → NO provider INSERT.
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-byok").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))

		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(kindQuery).WithArgs("ws-byok").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-byok", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{"MODEL": "sonnet"}
		h.applyConciergeProvisionConfig(context.Background(), "ws-byok", "", nil, env, "Org Concierge")

		if _, ok := env["LLM_PROVIDER"]; ok {
			t.Errorf("non-platform model wrongly got LLM_PROVIDER pinned (%q) — would mis-route a BYOK/self-host concierge", env["LLM_PROVIDER"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}

// TestNoConciergeLiteralsInCore is the regression guard for the RFC #2843
// §10a de-hardcode: the concierge's PROMPT + MCP-wiring identity (system
// prompt template, MCP-servers block, identity files) MUST live in the
// platform-agent template, not as Go string literals in core. A future
// re-introduction of those consts would silently regress the SSOT; this test
// fails on a fresh build if any of the banned identifiers reappear as bare Go
// references in the package source.
//
// NOTE (incident 2026-06-15): conciergeDeclaredModel is DELIBERATELY NOT
// banned. The model is the ONE concierge identity element that legitimately
// lives in core: the universal MISSING_MODEL gate (core#2594) reads the stored
// MODEL secret at provision time — BEFORE any template config.yaml is fetched —
// so the model MUST be seeded from a core-resident declared value, not deferred
// to template delivery. #2919 wrongly lumped the model in with the prompt/MCP
// literals and banned it here; removing the model-seed regressed every fresh
// platform-agent provision to MISSING_MODEL fail-closed. The concierge IS the
// platform-agent product, so it declares its own model exactly as a template
// does (this is SSOT-correct, not a hardcoded platform default). The seeding is
// gated by TestApplyConciergeProvisionConfig_SeedsModel.
//
// This is a grep over the package — brittle by design (intentionally so:
// the concierge-literal pattern was the exact failure mode of the
// pre-#10a code, and a re-introduction must be caught at CI time, not in
// code review).
func TestNoConciergeLiteralsInCore(t *testing.T) {
	banned := []string{
		"conciergeSystemPromptTmpl",
		"conciergeMCPServersBlock",
		"conciergeMCPFragmentFile",
		"conciergeIdentityFiles",
	}
	for _, id := range banned {
		// grep the source tree under workspace-server/internal/handlers for
		// the bare identifier. We allow the identifier to appear inside this
		// test (the regression guard itself) but nowhere else.
		out, err := exec.Command("grep", "-r", "--include=*.go",
			"-l", id, ".").CombinedOutput()
		if err != nil {
			// grep returns 1 when no matches — that's the PASS case.
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				continue
			}
			t.Fatalf("grep failed: %v\n%s", err, out)
		}
		// If grep found matches, every file is either this test or the
		// production file's doc comment. Production file references must be
		// only in the doc-comment block that explains the de-hardcode (and
		// even there, the comments have been carefully worded to avoid
		// bare-identifier resolution). Allow this specific test file.
		for _, line := range bytes.Split(bytes.TrimSpace(out), []byte{'\n'}) {
			fname := string(bytes.TrimPrefix(line, []byte("./")))
			if fname == "platform_agent_test.go" {
				continue // regression guard itself
			}
			t.Errorf("concierge literal %q reappeared in %s — RFC #2843 §10a de-hardcode REGRESSED", id, fname)
		}
	}
}

// TestDefaultCreateParentID covers core#2697: new workspaces nest under the
// platform-agent root when one exists, else fall back to the SOLE plain root
// workspace (the JRS case — a lone SEO Agent at parent_id NULL), else "".
func TestDefaultCreateParentID(t *testing.T) {
	platQ := `SELECT id FROM workspaces WHERE COALESCE\(kind, 'workspace'\) = 'platform'`
	rootQ := `SELECT id FROM workspaces WHERE parent_id IS NULL`

	t.Run("prefers the platform-agent root when exactly one exists", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(platQ).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("plat-1"))
		if got := defaultCreateParentID(context.Background()); got != "plat-1" {
			t.Fatalf("want plat-1, got %q", got)
		}
	})

	t.Run("falls back to the sole plain root when no platform-agent (JRS case)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(platQ).WillReturnRows(sqlmock.NewRows([]string{"id"})) // 0 platform
		mock.ExpectQuery(rootQ).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("seo-agent"))
		if got := defaultCreateParentID(context.Background()); got != "seo-agent" {
			t.Fatalf("want seo-agent (fallback to sole root), got %q", got)
		}
	})

	t.Run("returns empty when no platform and multiple roots (ambiguous)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(platQ).WillReturnRows(sqlmock.NewRows([]string{"id"}))
		mock.ExpectQuery(rootQ).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("r1").AddRow("r2"))
		if got := defaultCreateParentID(context.Background()); got != "" {
			t.Fatalf("want empty (ambiguous multi-root), got %q", got)
		}
	})
	t.Run("returns empty when MULTIPLE platform agents (ambiguous) — no root fallback (CR2 #2783)", func(t *testing.T) {
		mock := setupTestDB(t)
		// >1 platform → fail-soft empty; the root query must NOT run.
		mock.ExpectQuery(platQ).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("p1").AddRow("p2"))
		if got := defaultCreateParentID(context.Background()); got != "" {
			t.Fatalf("want empty (ambiguous multi-platform must NOT fall back to a root), got %q", got)
		}
	})
}

// TestRecordDeclaredPlugin_PrivilegedPluginEntitlement is the security gate for
// the org-management MCP plugin (RFC: rfc-platform-mcp-as-plugin). The privileged
// plugin carries the org-admin tool surface, so recordDeclaredPlugin — the single
// chokepoint every declaration path flows through — must REFUSE it for any
// non-platform workspace, regardless of how the declaration was sourced (template
// seed, org_import, or a user-authored workspace.yaml). This closes the
// privilege-escalation vector where a user workspace lists the plugin to mint
// itself org-admin tools.
func TestRecordDeclaredPlugin_PrivilegedPluginEntitlement(t *testing.T) {
	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =`
	const declaredInsert = `INSERT INTO workspace_declared_plugins`

	t.Run("platform concierge MAY declare the privileged management MCP", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-concierge", conciergePlatformMCPName, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := recordDeclaredPlugin(context.Background(), "ws-concierge", conciergePlatformMCPName, conciergePlatformMCPSource); err != nil {
			t.Fatalf("platform concierge declaration of the management MCP must succeed: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("non-platform workspace is REFUSED — no INSERT (privilege-escalation guard)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-user").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))
		// NO ExpectExec: the gate MUST refuse before any INSERT fires.
		err := recordDeclaredPlugin(context.Background(), "ws-user", conciergePlatformMCPName, conciergePlatformMCPSource)
		if err == nil {
			t.Fatal("a non-platform workspace MUST NOT be able to declare the privileged management MCP plugin")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (an INSERT fired — that is the privilege escalation this gate must stop): %v", err)
		}
	})

	t.Run("an ordinary plugin skips the kind precheck entirely (no extra query)", func(t *testing.T) {
		mock := setupTestDB(t)
		// No kind precheck for non-privileged names — straight to the upsert.
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-user", "browser-automation", sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := recordDeclaredPlugin(context.Background(), "ws-user", "browser-automation", "browser-automation"); err != nil {
			t.Fatalf("ordinary plugin declaration must succeed: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}
