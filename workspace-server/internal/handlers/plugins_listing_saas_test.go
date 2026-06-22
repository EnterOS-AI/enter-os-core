package handlers

// plugins_listing_saas_test.go — ListInstalled SaaS (EIC) path.
//
// Regression: ListInstalled only ls'd a LOCAL Docker container, so every SaaS
// tenant (no local container) read back [] for GET /workspaces/:id/plugins even
// when plugins were installed on its EC2 — the "[] readback after a successful
// install" bug. These tests pin the EIC dispatch + manifest read + fail-soft.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// stubListPluginsViaEIC swaps the package-level listPluginsViaEIC for the test.
func stubListPluginsViaEIC(t *testing.T, fn func(ctx context.Context, instanceID, runtime string) ([]string, error)) {
	t.Helper()
	prev := listPluginsViaEIC
	listPluginsViaEIC = fn
	t.Cleanup(func() { listPluginsViaEIC = prev })
}

func newSaaSListHandler() *PluginsHandler {
	// docker == nil → findRunningContainer returns "" → SaaS branch is taken.
	return NewPluginsHandler(t_TempDirNoop(), nil, nil).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return "i-saaslist", nil })
}

// t_TempDirNoop avoids importing testing into the helper signature above; the
// registry path is unused on the list path.
func t_TempDirNoop() string { return "/tmp" }

func callListInstalled(t *testing.T, h *PluginsHandler) (int, []pluginInfo) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "c7244ed9-f623-4cba-8873-020e5c9fe104"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/x/plugins", nil)
	h.ListInstalled(c)
	var out []pluginInfo
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestListInstalled_SaaS_ReturnsInstalledPlugins(t *testing.T) {
	h := newSaaSListHandler()
	stubListPluginsViaEIC(t, func(_ context.Context, instanceID, runtime string) ([]string, error) {
		if instanceID != "i-saaslist" || runtime != "claude-code" {
			t.Fatalf("unexpected dispatch instance=%q runtime=%q", instanceID, runtime)
		}
		return []string{"molecule-ai-plugin-image-gen", "molecule-ai-plugin-molecule-platform-mcp"}, nil
	})
	stubReadPluginManifestViaEIC(t, func(_ context.Context, _, _, name string) ([]byte, error) {
		return []byte("name: " + name + "\nversion: 0.1.0\n"), nil
	})

	code, plugins := callListInstalled(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(plugins) != 2 {
		t.Fatalf("got %d plugins, want 2: %+v", len(plugins), plugins)
	}
	names := map[string]bool{}
	for _, p := range plugins {
		names[p.Name] = true
	}
	if !names["molecule-ai-plugin-image-gen"] {
		t.Errorf("user plugin image-gen missing from listing: %+v", plugins)
	}
}

func TestListInstalled_SaaS_MissingManifestStillListsName(t *testing.T) {
	h := newSaaSListHandler()
	stubListPluginsViaEIC(t, func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"bare-plugin"}, nil
	})
	stubReadPluginManifestViaEIC(t, func(_ context.Context, _, _, _ string) ([]byte, error) {
		return nil, nil // no manifest
	})

	code, plugins := callListInstalled(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(plugins) != 1 || plugins[0].Name != "bare-plugin" {
		t.Fatalf("got %+v, want single bare-plugin entry", plugins)
	}
}

func TestListInstalled_SaaS_ListErrorFailsSoftEmpty(t *testing.T) {
	h := newSaaSListHandler()
	stubListPluginsViaEIC(t, func(_ context.Context, _, _ string) ([]string, error) {
		return nil, errors.New("tunnel down")
	})

	code, plugins := callListInstalled(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-soft)", code)
	}
	if len(plugins) != 0 {
		t.Fatalf("got %d plugins, want 0 on list error", len(plugins))
	}
}

func TestListInstalled_SaaS_RejectsInvalidPluginName(t *testing.T) {
	h := newSaaSListHandler()
	stubListPluginsViaEIC(t, func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"../etc", "good-plugin"}, nil
	})
	stubReadPluginManifestViaEIC(t, func(_ context.Context, _, _, _ string) ([]byte, error) {
		return nil, nil
	})

	code, plugins := callListInstalled(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(plugins) != 1 || plugins[0].Name != "good-plugin" {
		t.Fatalf("traversal name should be dropped; got %+v", plugins)
	}
}
