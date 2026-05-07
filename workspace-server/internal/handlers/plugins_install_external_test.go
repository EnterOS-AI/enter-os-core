package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestPluginInstall_ExternalRuntime_Returns422 — molecule-core#10.
// Install on a `runtime='external'` workspace must NOT fall through to
// findRunningContainer (which would 503 with a misleading "container not
// running"). It must return 422 with a hint pointing at the pull-mode
// download endpoint.
func TestPluginInstall_ExternalRuntime_Returns422(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithRuntimeLookup(func(workspaceID string) (string, error) {
			return "external", nil
		})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ba1789b0-4d21-4f4f-a878-fa226bf77cf5"}}
	c.Request = httptest.NewRequest(
		"POST",
		"/workspaces/ba1789b0-4d21-4f4f-a878-fa226bf77cf5/plugins",
		bytes.NewBufferString(`{"source":"local://my-plugin"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 (Unprocessable Entity) for runtime='external', got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "external runtimes") {
		t.Errorf("expected error body to mention 'external runtimes', got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "download") {
		t.Errorf("expected error body to point at the download endpoint, got: %s", w.Body.String())
	}
}

// TestPluginUninstall_ExternalRuntime_Returns422 — symmetric guard on the
// uninstall path (DELETE /workspaces/:id/plugins/:name). External
// workspaces manage their own plugin directory locally; the platform
// can't docker-exec into them.
func TestPluginUninstall_ExternalRuntime_Returns422(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithRuntimeLookup(func(workspaceID string) (string, error) {
			return "external", nil
		})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ba1789b0-4d21-4f4f-a878-fa226bf77cf5"},
		{Key: "name", Value: "my-plugin"},
	}
	c.Request = httptest.NewRequest(
		"DELETE",
		"/workspaces/ba1789b0-4d21-4f4f-a878-fa226bf77cf5/plugins/my-plugin",
		nil,
	)

	h.Uninstall(c)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for runtime='external', got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "external runtimes") {
		t.Errorf("expected error body to mention 'external runtimes', got: %s", w.Body.String())
	}
}

// TestPluginInstall_ContainerBackedRuntime_FallsThroughGuard — the runtime
// guard MUST NOT short-circuit container-backed runtimes. With
// `runtime='claude-code'` the install proceeds past the guard; without a
// real plugin source it'll fail downstream (here: 404 from local resolver
// because no plugin staged), which is the correct error to surface.
//
// This is the mutation-test partner: deleting the `runtime == "external"`
// check would still pass TestPluginInstall_ExternalRuntime (because Install
// would 404 instead of 422 — but the test asserts 422), and would still
// pass this test (because both pre-fix and post-fix produce 404 here).
// What this case pins is "non-external still falls through," catching
// any over-eager guard that rejects all runtimes.
func TestPluginInstall_ContainerBackedRuntime_FallsThroughGuard(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithRuntimeLookup(func(workspaceID string) (string, error) {
			return "claude-code", nil
		})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "c7c28c0b-4ea5-4e75-9728-3ba860081708"}}
	c.Request = httptest.NewRequest(
		"POST",
		"/workspaces/c7c28c0b-4ea5-4e75-9728-3ba860081708/plugins",
		bytes.NewBufferString(`{"source":"local://nonexistent-plugin"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	if w.Code == http.StatusUnprocessableEntity {
		t.Errorf("runtime='claude-code' must fall through the external guard; got 422: %s", w.Body.String())
	}
	// The local resolver will fail to find the plugin → 404. Anything
	// other than 422 (which would mean we mis-classified) is fine.
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 (plugin not found in registry), got %d: %s", w.Code, w.Body.String())
	}
}

// TestPluginInstall_NoRuntimeLookup_FailsOpen — when the runtime lookup
// is unwired (test fixtures, niche deploy shapes) the guard MUST default
// to allowing the install attempt. The downstream findRunningContainer
// step still gates on a real container, so failing open here doesn't
// expose a bypass — it just preserves backwards-compat with deployments
// that haven't wired the lookup.
func TestPluginInstall_NoRuntimeLookup_FailsOpen(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil) // NO WithRuntimeLookup

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-no-lookup"}}
	c.Request = httptest.NewRequest(
		"POST",
		"/workspaces/ws-no-lookup/plugins",
		bytes.NewBufferString(`{"source":"local://nonexistent"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	if w.Code == http.StatusUnprocessableEntity {
		t.Errorf("nil runtimeLookup must fall through (fail-open); got 422: %s", w.Body.String())
	}
}

// TestPluginInstall_RuntimeLookupErrors_FailsOpen — same fail-open story
// for transient DB errors in the lookup. We don't want a momentary
// Postgres hiccup to flip every plugin install into a 422.
func TestPluginInstall_RuntimeLookupErrors_FailsOpen(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithRuntimeLookup(func(workspaceID string) (string, error) {
			return "", errFakeDB
		})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-db-flake"}}
	c.Request = httptest.NewRequest(
		"POST",
		"/workspaces/ws-db-flake/plugins",
		bytes.NewBufferString(`{"source":"local://nonexistent"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	if w.Code == http.StatusUnprocessableEntity {
		t.Errorf("runtimeLookup error must fall through (fail-open); got 422: %s", w.Body.String())
	}
}

// errFakeDB is a sentinel for the fail-open lookup-error case.
var errFakeDB = &fakeError{msg: "synthetic db error"}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }
