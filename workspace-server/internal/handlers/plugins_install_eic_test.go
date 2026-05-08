package handlers

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// expectAllowlistAllowAll programs the package-shared withMockDB sqlmock
// so the org-allowlist gate (org_plugin_allowlist.go) returns "allow-all"
// for the duration of one Install call. The gate fires three queries —
// resolveOrgID, allowlist EXISTS, allowlist COUNT — and we satisfy each
// with the empty/zero shape that means "no allowlist configured."
//
// Without this, tests that exercise the full Install flow panic on a
// nil DB. The handlers package already ships withMockDB in
// tokens_sqlmock_test.go; we just layer the allowlist-specific
// expectations on top.
func expectAllowlistAllowAll(mock sqlmock.Sqlmock) {
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`SELECT parent_id FROM workspaces WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))
	mock.ExpectQuery(`SELECT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM org_plugin_allowlist`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
}

// stagePluginRegistry creates a single-plugin registry under dir so the
// install handler's local resolver can find it. Returns the path to the
// plugin dir for any caller that wants to assert tar contents.
//
// Centralised so a future tweak to the registry shape (e.g. plugin.yaml
// schema bump) only updates one place. Tests use the source spec
// `local://<name>` which the local resolver maps to <dir>/<name>/.
func stagePluginRegistry(t *testing.T, dir, name string) string {
	t.Helper()
	pluginDir := filepath.Join(dir, name)
	if err := os.Mkdir(pluginDir, 0755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	manifest := "name: " + name + "\nversion: \"1.0.0\"\ndescription: SaaS dispatch test plugin\n"
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatalf("write plugin.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "rule.md"), []byte("# rule\n"), 0644); err != nil {
		t.Fatalf("write rule.md: %v", err)
	}
	return pluginDir
}

// stubInstallPluginViaEIC swaps the package-level installPluginViaEIC for
// the duration of the test; restored by t.Cleanup. Mirrors the existing
// withEICTunnel stub pattern (template_files_eic_dispatch_test.go).
func stubInstallPluginViaEIC(t *testing.T, fn func(ctx context.Context, instanceID, runtime, pluginName, stagedDir string) error) {
	t.Helper()
	prev := installPluginViaEIC
	installPluginViaEIC = fn
	t.Cleanup(func() { installPluginViaEIC = prev })
}

func stubUninstallPluginViaEIC(t *testing.T, fn func(ctx context.Context, instanceID, runtime, pluginName string) error) {
	t.Helper()
	prev := uninstallPluginViaEIC
	uninstallPluginViaEIC = fn
	t.Cleanup(func() { uninstallPluginViaEIC = prev })
}

func stubReadPluginManifestViaEIC(t *testing.T, fn func(ctx context.Context, instanceID, runtime, pluginName string) ([]byte, error)) {
	t.Helper()
	prev := readPluginManifestViaEIC
	readPluginManifestViaEIC = fn
	t.Cleanup(func() { readPluginManifestViaEIC = prev })
}

// ---------- pure-function shell shape ----------

func TestBuildPluginInstallShell_QuotesPath(t *testing.T) {
	got := buildPluginInstallShell("/configs/plugins/my-plugin")
	want := "sudo -n sh -c 'rm -rf '/configs/plugins/my-plugin' && mkdir -p '/configs/plugins/my-plugin' && tar -xzf - --no-same-owner -C '/configs/plugins/my-plugin' && chown -R 1000:1000 '/configs/plugins/my-plugin''"
	if got != want {
		t.Errorf("buildPluginInstallShell mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestBuildPluginUninstallShell_QuotesPath(t *testing.T) {
	got := buildPluginUninstallShell("/configs/plugins/my-plugin")
	want := "sudo -n rm -rf '/configs/plugins/my-plugin'"
	if got != want {
		t.Errorf("buildPluginUninstallShell mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestBuildPluginManifestReadShell_QuotesPath(t *testing.T) {
	got := buildPluginManifestReadShell("/configs/plugins/my-plugin")
	want := "sudo -n cat '/configs/plugins/my-plugin'/plugin.yaml 2>/dev/null"
	if got != want {
		t.Errorf("buildPluginManifestReadShell mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestHostPluginPath_PerRuntime(t *testing.T) {
	cases := []struct {
		runtime string
		plugin  string
		want    string
	}{
		{"claude-code", "browser-automation", "/configs/plugins/browser-automation"},
		{"hermes", "browser-automation", "/home/ubuntu/.hermes/plugins/browser-automation"},
		{"langgraph", "browser-automation", "/opt/configs/plugins/browser-automation"},
		// Unknown / empty runtime falls back to /configs (containerized
		// user-data layout) so a future runtime added to workspaces table
		// without a workspaceFilePathPrefix entry doesn't blow up the
		// install path silently.
		{"", "browser-automation", "/configs/plugins/browser-automation"},
		{"some-future-runtime", "x", "/configs/plugins/x"},
	}
	for _, c := range cases {
		t.Run(c.runtime+"/"+c.plugin, func(t *testing.T) {
			got := hostPluginPath(c.runtime, c.plugin)
			if got != c.want {
				t.Errorf("hostPluginPath(%q, %q) = %q, want %q", c.runtime, c.plugin, got, c.want)
			}
		})
	}
}

// ---------- dispatch: install ----------

// TestPluginInstall_SaaS_DispatchesToEIC — the most-load-bearing test in
// this file. With h.docker == nil and instanceIDLookup returning a real
// instance_id, Install MUST push the staged plugin to the EC2 over EIC
// (not 503). Asserts the EIC stub is called with the right (instance,
// runtime, plugin) tuple AND that the staged dir has the manifest +
// rule files we put there — proves the staging side wasn't bypassed.
func TestPluginInstall_SaaS_DispatchesToEIC(t *testing.T) {
	registry := t.TempDir()
	stagePluginRegistry(t, registry, "browser-automation")

	type capture struct {
		called      bool
		instanceID  string
		runtime     string
		pluginName  string
		stagedFiles []string
	}
	var got capture

	stubInstallPluginViaEIC(t, func(ctx context.Context, instanceID, runtime, pluginName, stagedDir string) error {
		got.called = true
		got.instanceID = instanceID
		got.runtime = runtime
		got.pluginName = pluginName
		entries, err := os.ReadDir(stagedDir)
		if err != nil {
			t.Fatalf("read staged dir: %v", err)
		}
		for _, e := range entries {
			got.stagedFiles = append(got.stagedFiles, e.Name())
		}
		return nil
	})

	mock, cleanup := withMockDB(t)
	defer cleanup()
	expectAllowlistAllowAll(mock)

	h := NewPluginsHandler(registry, nil, nil).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return "i-0e0951a3cfd9bbf75", nil })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "c7244ed9-f623-4cba-8873-020e5c9fe104"}}
	c.Request = httptest.NewRequest(
		"POST",
		"/workspaces/c7244ed9-f623-4cba-8873-020e5c9fe104/plugins",
		bytes.NewBufferString(`{"source":"local://browser-automation"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !got.called {
		t.Fatalf("installPluginViaEIC was not called")
	}
	if got.instanceID != "i-0e0951a3cfd9bbf75" {
		t.Errorf("instanceID = %q, want i-0e0951a3cfd9bbf75", got.instanceID)
	}
	if got.runtime != "claude-code" {
		t.Errorf("runtime = %q, want claude-code", got.runtime)
	}
	if got.pluginName != "browser-automation" {
		t.Errorf("pluginName = %q, want browser-automation", got.pluginName)
	}
	// Staged dir must carry the resolver's actual fetch — manifest + rule.
	// Anything missing here means the stage step was bypassed.
	hasManifest, hasRule := false, false
	for _, f := range got.stagedFiles {
		if f == "plugin.yaml" {
			hasManifest = true
		}
		if f == "rule.md" {
			hasRule = true
		}
	}
	if !hasManifest || !hasRule {
		t.Errorf("staged dir missing files: %v (want plugin.yaml + rule.md)", got.stagedFiles)
	}
}

// TestPluginInstall_SaaS_PropagatesEICError — when the EIC push fails
// (tunnel down, sudo denied), Install MUST surface 502 rather than swallow
// the error and report 200. 502 is the right status for "we tried, the
// remote side wasn't there" — distinct from 503 ("nothing wired") and
// 500 ("our bug"). The body deliberately doesn't echo the underlying
// error string (would leak ssh stderr / instance metadata).
func TestPluginInstall_SaaS_PropagatesEICError(t *testing.T) {
	registry := t.TempDir()
	stagePluginRegistry(t, registry, "browser-automation")

	mock, cleanup := withMockDB(t)
	defer cleanup()
	expectAllowlistAllowAll(mock)

	stubInstallPluginViaEIC(t, func(ctx context.Context, instanceID, runtime, pluginName, stagedDir string) error {
		return errors.New("ssh: tunnel exited 255")
	})

	h := NewPluginsHandler(registry, nil, nil).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return "i-aaaa", nil })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest(
		"POST",
		"/workspaces/ws-1/plugins",
		bytes.NewBufferString(`{"source":"local://browser-automation"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for EIC failure, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tunnel exited") {
		t.Errorf("response body must not echo raw EIC error: %s", w.Body.String())
	}
}

// TestPluginInstall_NoBackends_Returns503 — lookup is wired but returns
// empty instance_id (e.g. workspace pre-provision, or local-Docker
// deploy without a running container). The handler MUST 503, not silently
// dispatch to EIC with an empty instance_id.
func TestPluginInstall_NoBackends_Returns503(t *testing.T) {
	registry := t.TempDir()
	stagePluginRegistry(t, registry, "browser-automation")

	mock, cleanup := withMockDB(t)
	defer cleanup()
	expectAllowlistAllowAll(mock)

	stubInstallPluginViaEIC(t, func(ctx context.Context, instanceID, runtime, pluginName, stagedDir string) error {
		t.Errorf("EIC must not be called when instance_id is empty")
		return nil
	})

	h := NewPluginsHandler(registry, nil, nil).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return "", nil }) // empty

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest(
		"POST",
		"/workspaces/ws-1/plugins",
		bytes.NewBufferString(`{"source":"local://browser-automation"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPluginInstall_InstanceLookupError_Returns503 — a DB hiccup on the
// instance_id lookup must NOT crash or 502; the handler logs and falls
// through to 503. Same fail-open shape h.runtimeLookup uses (see
// TestPluginInstall_NoRuntimeLookup_FailsOpen). Pinning this prevents a
// future "tighten error handling" refactor from quietly converting a DB
// blip into a five-minute outage on the install endpoint.
func TestPluginInstall_InstanceLookupError_Returns503(t *testing.T) {
	registry := t.TempDir()
	stagePluginRegistry(t, registry, "browser-automation")

	mock, cleanup := withMockDB(t)
	defer cleanup()
	expectAllowlistAllowAll(mock)

	h := NewPluginsHandler(registry, nil, nil).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return "", errors.New("db: connection refused") })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest(
		"POST",
		"/workspaces/ws-1/plugins",
		bytes.NewBufferString(`{"source":"local://browser-automation"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 on instance-id lookup error, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- dispatch: uninstall ----------

func TestPluginUninstall_SaaS_DispatchesToEIC(t *testing.T) {
	stubReadPluginManifestViaEIC(t, func(ctx context.Context, instanceID, runtime, pluginName string) ([]byte, error) {
		return []byte("name: browser-automation\nskills:\n  - browse\n"), nil
	})

	type capture struct {
		called     bool
		instanceID string
		runtime    string
		pluginName string
	}
	var got capture
	stubUninstallPluginViaEIC(t, func(ctx context.Context, instanceID, runtime, pluginName string) error {
		got.called = true
		got.instanceID = instanceID
		got.runtime = runtime
		got.pluginName = pluginName
		return nil
	})

	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return "i-bbbb", nil })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "name", Value: "browser-automation"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-1/plugins/browser-automation", nil)

	h.Uninstall(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !got.called {
		t.Fatalf("uninstallPluginViaEIC was not called")
	}
	if got.instanceID != "i-bbbb" || got.runtime != "claude-code" || got.pluginName != "browser-automation" {
		t.Errorf("dispatch args wrong: %+v", got)
	}
}

func TestPluginUninstall_SaaS_PropagatesEICError(t *testing.T) {
	stubReadPluginManifestViaEIC(t, func(ctx context.Context, instanceID, runtime, pluginName string) ([]byte, error) {
		return nil, nil
	})
	stubUninstallPluginViaEIC(t, func(ctx context.Context, instanceID, runtime, pluginName string) error {
		return errors.New("ssh: connection refused")
	})

	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return "i-cccc", nil })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "name", Value: "browser-automation"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-1/plugins/browser-automation", nil)

	h.Uninstall(c)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPluginUninstall_NoBackends_Returns503(t *testing.T) {
	stubUninstallPluginViaEIC(t, func(ctx context.Context, instanceID, runtime, pluginName string) error {
		t.Errorf("EIC uninstall must not be called with empty instance_id")
		return nil
	})

	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return "", nil })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "name", Value: "browser-automation"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/ws-1/plugins/browser-automation", nil)

	h.Uninstall(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- tarball shape ----------

// TestRealInstallPluginViaEIC_TarPayloadShape — the production
// installPluginViaEIC packs the staged dir as gzipped tar. Stub
// withEICTunnel + run the real installPluginViaEIC body, capturing the
// ssh stdin via a fake exec.Command — except go's exec is hard to fake
// without hijacking $PATH. Instead we exercise the tar packer directly:
// streamDirAsTar's behaviour is what we actually depend on, and a
// regression in either streamDirAsTar OR the gzip wrapping will be
// visible here.
func TestRealInstallPluginViaEIC_TarPayloadShape(t *testing.T) {
	staged := t.TempDir()
	if err := os.WriteFile(filepath.Join(staged, "plugin.yaml"), []byte("name: x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(staged, "skills", "browse"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "skills", "browse", "instructions.md"), []byte("step 1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := streamDirAsTar(staged, tw); err != nil {
		t.Fatalf("streamDirAsTar: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}

	// Round-trip: the same payload the production flow would pipe into
	// `tar -xzf -` on the remote should unpack to plugin.yaml +
	// skills/browse/instructions.md.
	gr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gr)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		seen[hdr.Name] = true
	}
	for _, want := range []string{"plugin.yaml", "skills/browse/instructions.md"} {
		// Tar entries on Linux normally use forward slashes regardless
		// of host separator; double-check both forms so a Windows test
		// runner doesn't go red on a path-sep difference. Production
		// always runs on Linux (CI + tenant EC2).
		alt := filepath.FromSlash(want)
		if !seen[want] && !seen[alt] {
			t.Errorf("tar payload missing %q (saw %v)", want, seen)
		}
	}
}
