package handlers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/plugins"
	"github.com/DATA-DOG/go-sqlmock"
)

// newReconcileHandler builds a PluginsHandler whose local resolver serves a
// single plugin "seo-all" from a temp registry, and whose deliver step is
// captured (no Docker / EC2). Returns the handler and a pointer to the slice
// of delivered plugin names.
func newReconcileHandler(t *testing.T) (*PluginsHandler, *[]string) {
	t.Helper()
	reg := t.TempDir()
	pluginDir := filepath.Join(reg, "seo-all")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"),
		[]byte("name: seo-all\nversion: 1.0.0\nskills:\n  - seo-all\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := NewPluginsHandler(reg, nil, nil)
	var mu sync.Mutex
	delivered := []string{}
	h.deliverOverride = func(ctx context.Context, workspaceID string, r *stageResult) error {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, r.PluginName)
		return nil
	}
	return h, &delivered
}

// expectDeclared programs the two reconcile read queries: the declared set and
// the installed-name set.
func expectDeclared(mock sqlmock.Sqlmock, declared []DeclaredPlugin, installed []string) {
	dRows := sqlmock.NewRows([]string{"plugin_name", "source_raw"})
	for _, d := range declared {
		dRows.AddRow(d.PluginName, d.SourceRaw)
	}
	mock.ExpectQuery(`SELECT plugin_name, source_raw\s+FROM workspace_declared_plugins`).
		WillReturnRows(dRows)

	iRows := sqlmock.NewRows([]string{"plugin_name"})
	for _, n := range installed {
		iRows.AddRow(n)
	}
	mock.ExpectQuery(`SELECT plugin_name FROM workspace_plugins WHERE workspace_id`).
		WillReturnRows(iRows)
}

func TestReconcile_DeclaredButMissing_Installs(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	h, delivered := newReconcileHandler(t)

	expectDeclared(mock,
		[]DeclaredPlugin{{PluginName: "seo-all", SourceRaw: "local://seo-all"}},
		nil, // nothing installed yet
	)
	// The install records a workspace_plugins row.
	mock.ExpectExec(`INSERT INTO workspace_plugins`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	h.ReconcileWorkspacePlugins(context.Background(), "ws-1")

	if len(*delivered) != 1 || (*delivered)[0] != "seo-all" {
		t.Fatalf("expected seo-all delivered, got %v", *delivered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

func TestReconcile_AlreadyInstalled_NoOp(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	h, delivered := newReconcileHandler(t)

	expectDeclared(mock,
		[]DeclaredPlugin{{PluginName: "seo-all", SourceRaw: "local://seo-all"}},
		[]string{"seo-all"}, // already installed
	)
	// No INSERT expected — already installed is a pure no-op.

	h.ReconcileWorkspacePlugins(context.Background(), "ws-1")

	if len(*delivered) != 0 {
		t.Fatalf("already-installed must be a no-op, but delivered %v", *delivered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

func TestReconcile_PartialDiff_InstallsOnlyMissing(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	h, delivered := newReconcileHandler(t)

	// Add a second installable plugin to the registry.
	second := filepath.Join(h.pluginsDir, "research")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(second, "plugin.yaml"), []byte("name: research\n"), 0o644)

	expectDeclared(mock,
		[]DeclaredPlugin{
			{PluginName: "seo-all", SourceRaw: "local://seo-all"},
			{PluginName: "research", SourceRaw: "local://research"},
		},
		[]string{"seo-all"}, // seo-all installed, research missing
	)
	mock.ExpectExec(`INSERT INTO workspace_plugins`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	h.ReconcileWorkspacePlugins(context.Background(), "ws-1")

	if len(*delivered) != 1 || (*delivered)[0] != "research" {
		t.Fatalf("expected only research installed, got %v", *delivered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

func TestReconcile_NoDeclared_NoQueriesBeyondFirst(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	h, delivered := newReconcileHandler(t)

	// Empty declared set → reconcile returns after the first query, never
	// reading the installed set or attempting an install.
	mock.ExpectQuery(`SELECT plugin_name, source_raw\s+FROM workspace_declared_plugins`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}))

	h.ReconcileWorkspacePlugins(context.Background(), "ws-1")

	if len(*delivered) != 0 {
		t.Fatalf("no declared plugins must mean no installs, got %v", *delivered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

func TestTrackFromSource(t *testing.T) {
	cases := map[string]string{
		"gitea://o/r/sub#main":      "none",
		"gitea://o/r/sub#tag:v1.0":  "tag:v1.0",
		"gitea://o/r/sub#sha:abc12": "sha:abc12",
		"local://seo-all":           "none",
		"github://o/r#v1.0":         "none", // bare branch/tag ref, not tag:/sha:
	}
	for in, want := range cases {
		if got := trackFromSource(in); got != want {
			t.Errorf("trackFromSource(%q) = %q, want %q", in, got, want)
		}
	}
}

// Compile-time check that the production ReconcileFunc signature matches the
// method (catches a future signature drift between handler + registry wiring).
var _ ReconcileFunc = (*PluginsHandler)(nil).ReconcileWorkspacePlugins

// stageResult.cleanup must tolerate a nil receiver / empty dir.
func TestStageResultCleanup_Safe(t *testing.T) {
	var s *stageResult
	s.cleanup() // must not panic
	(&stageResult{}).cleanup()
}

// Ensure the gitea resolver is registered on a production handler so a
// declared gitea:// source resolves (RFC#2843 source contract).
func TestNewPluginsHandler_RegistersGiteaScheme(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)
	src, err := plugins.ParseSource(
		"gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.sources.Resolve(src); err != nil {
		t.Errorf("gitea scheme must be registered on the production handler: %v", err)
	}
}

// TestProvisioningChannelCarriesNoPlugins is a regression guard for RFC#2843:
// declared plugins must NOT travel through the provisioning channel
// (configFiles) anymore. It asserts org_import.go no longer bundles plugins
// into configFiles and instead records them as declared (for the post-online
// reconcile). A reintroduction of the old `configFiles["plugins/...]` copy
// fails this test — fail-closed against the anti-pattern coming back.
func TestProvisioningChannelCarriesNoPlugins(t *testing.T) {
	src, err := os.ReadFile("org_import.go")
	if err != nil {
		t.Fatalf("read org_import.go: %v", err)
	}
	s := string(src)

	// The old anti-pattern keyed plugin files under "plugins/<name>/..." in
	// the configFiles map. That key prefix must not appear anymore.
	for _, banned := range []string{
		`configFiles["plugins/`,
		`configFiles[ "plugins/`,
		`"plugins/"+pluginName+`,
		`"plugins/" + pluginName`,
	} {
		if strings.Contains(s, banned) {
			t.Errorf("org_import.go still bundles plugins into the provisioning "+
				"channel (found %q) — RFC#2843 forbids this; plugins install "+
				"dynamically post-online", banned)
		}
	}

	// And it MUST persist declared plugins for the reconcile.
	if !strings.Contains(s, "recordDeclaredPlugin(") {
		t.Error("org_import.go must record declared plugins (recordDeclaredPlugin) " +
			"so the post-online reconcile can install them")
	}
}

// TestReconcile_BootInstalled_RecordsWithoutDeliver pins the RFC#2843 #38 fix:
// when the plugin is already on the box (boot-installed by the runtime-image
// entrypoint before this online-transition reconcile runs), the reconcile must
// record the tracking row but NOT re-deliver via EIC + restart (the churn that
// caused one wasted re-provision per fresh workspace).
func TestReconcile_BootInstalled_RecordsWithoutDeliver(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	h, delivered := newReconcileHandler(t)
	// SaaS box present + manifest read returns non-empty → pluginPresentOnBox=true.
	h.instanceIDLookup = func(string) (string, error) { return "i-boot", nil }
	orig := readPluginManifestViaEIC
	readPluginManifestViaEIC = func(ctx context.Context, instanceID, runtime, pluginName string) ([]byte, error) {
		return []byte("name: seo-all\n"), nil
	}
	defer func() { readPluginManifestViaEIC = orig }()

	expectDeclared(mock,
		[]DeclaredPlugin{{PluginName: "seo-all", SourceRaw: "local://seo-all"}},
		nil,
	)
	// Tracking row IS still recorded (so drift/UI work) — just no deliver/restart.
	mock.ExpectExec(`INSERT INTO workspace_plugins`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	h.ReconcileWorkspacePlugins(context.Background(), "ws-1")

	if len(*delivered) != 0 {
		t.Fatalf("boot-installed plugin must NOT be re-delivered (no EIC push/restart churn), but delivered %v", *delivered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations (tracking row must still be recorded): %v", err)
	}
}
