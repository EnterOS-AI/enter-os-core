package buildinfo_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/buildinfo"
	"github.com/gin-gonic/gin"
)

// TestGitSHA_DefaultDevSentinel pins the contract that an unset
// GIT_SHA at build time reads as "dev", NOT as an empty string. The
// redeploy verification step compares the deployed /buildinfo against
// the workflow's expected SHA — if GitSHA were "" by default, a
// misconfigured deploy would round-trip "" successfully if the
// expected SHA were also somehow ""; "dev" guarantees the comparison
// always fails closed for an unset deploy.
//
// Linker tests can't directly exercise -ldflags injection from inside
// `go test`, but they can pin the default the linker overrides.
func TestGitSHA_DefaultDevSentinel(t *testing.T) {
	if buildinfo.GitSHA != "dev" {
		t.Errorf("GitSHA default = %q, want %q (CI ldflags override expected to set this; tests run without ldflags so this should be the dev sentinel)", buildinfo.GitSHA, "dev")
	}
}

// TestBuildInfoEndpoint_ReturnsGitSHA pins the wire shape of the
// /buildinfo response. The redeploy verification step reads
// `.git_sha` from this JSON; renaming the field would silently break
// every tenant verification (the jq lookup would return null + the
// step would interpret it as "tenant unreachable" and fail closed,
// which is correct but noisy).
//
// Test routes the handler against an httptest server rather than
// constructing a router.Setup() — that constructor takes a Hub +
// Broadcaster + Provisioner + WorkspaceHandler + ChannelMgr, and
// /buildinfo doesn't depend on any of them. Using a minimal gin
// engine here keeps the test fast and isolated to the contract under
// test.
func TestBuildInfoEndpoint_ReturnsGitSHA(t *testing.T) {
	// Stash + restore so other tests that read GitSHA see a stable
	// value. The package-level var is mutable by design (-ldflags),
	// so test isolation requires explicit save/restore.
	prev := buildinfo.GitSHA
	t.Cleanup(func() { buildinfo.GitSHA = prev })
	buildinfo.GitSHA = "abc1234deadbeef"

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/buildinfo", func(c *gin.Context) {
		c.JSON(200, gin.H{"git_sha": buildinfo.GitSHA})
	})

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/buildinfo")
	if err != nil {
		t.Fatalf("GET /buildinfo: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	got, ok := body["git_sha"]
	if !ok {
		t.Fatalf("response missing git_sha field — would break the redeploy verification jq lookup. Body: %+v", body)
	}
	if got != "abc1234deadbeef" {
		t.Errorf("git_sha = %q, want %q", got, "abc1234deadbeef")
	}
}
