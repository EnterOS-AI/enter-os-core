package provisioner

// gitea_template_assets_test.go — tests for the real Gitea
// TemplateAssetFetcher (RFC #2843 #24, PR-B). The previous
// #2855 SCAFFOLD tests covered the interface contract; this
// file covers the PRODUCTION impl.
//
// Tests use httptest.NewServer to serve a real .tar.gz
// generated in-memory (no real Gitea instance needed). The
// dispatch's required test surface:
//   - happy path: assert config.yaml + prompts/* are returned and
//     agent-skills/* are SKIPPED (RFC#2843 #32 — skills are plugins
//     now, fetched at install time, NOT on this asset channel)
//   - allowlist filter: non-allowlisted paths (incl agent-skills) excluded
//   - fail-closed: transport / extract errors surface as errors
//   - identity parsing: malformed identities return errors

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGiteaTemplateAssetFetcher_HappyPath pins the production
// contract: a real .tar.gz archive containing config.yaml +
// prompts/ + agent-skills/ is fetched and parsed; config.yaml +
// prompts/* are returned, and agent-skills/* are SKIPPED (RFC#2843
// #32 — skills are plugins now, fetched at install time by the
// gitea:// plugin resolver, NOT carried on this asset channel). The
// skill files MUST still exist in the repo archive — they are the
// plugin source — they are just not asset-delivered.
func TestGiteaTemplateAssetFetcher_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request shape.
		if r.URL.Path != "/api/v1/repos/molecule-ai/workspace-template-seo/archive/main.tar.gz" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token the-token" {
			t.Errorf("unexpected Authorization header: %q", got)
		}
		// Serve a real .tar.gz with config.yaml + prompts/system.md +
		// agent-skills/seo-audit/* wrapped in the "<repo>-<sha>"
		// top-level dir Gitea uses. The skill files are present in the
		// archive (they ARE the plugin source) but must be SKIPPED by
		// the fetcher (RFC#2843 #32).
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		mustWriteTar(t, tw, "workspace-template-seo-abcd1234/config.yaml", []byte("# SEO config\n"))
		mustWriteTar(t, tw, "workspace-template-seo-abcd1234/prompts/system.md", []byte("# System prompt\n"))
		mustWriteTar(t, tw, "workspace-template-seo-abcd1234/agent-skills/seo-audit/SKILL.md", []byte("# SEO skill\n"))
		mustWriteTar(t, tw, "workspace-template-seo-abcd1234/agent-skills/seo-audit/manifest.yaml", []byte("name: seo-audit\n"))
		_ = tw.Close()
		_ = gz.Close()
	}))
	defer srv.Close()

	f := NewGiteaTemplateAssetFetcher(srv.URL, "the-token", srv.Client())
	assets, err := f.Load(context.Background(), "molecule-ai/workspace-template-seo@main")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// config.yaml + prompts/* are returned; agent-skills/* are SKIPPED
	// (RFC#2843 #32 — skills are plugins now). The skill files exist in
	// the repo archive but must NOT appear in the asset map.
	mustHaveKey(t, assets, "config.yaml")
	mustHaveKey(t, assets, "prompts/system.md")
	mustNotHaveKey(t, assets, "agent-skills/seo-audit/SKILL.md")
	mustNotHaveKey(t, assets, "agent-skills/seo-audit/manifest.yaml")
	if len(assets) != 2 {
		t.Errorf("expected 2 assets (config.yaml + prompts/system.md), got %d: %v", len(assets), keysOf(assets))
	}
}

// TestGiteaTemplateAssetFetcher_AllowsOnlyAllowlistedPaths pins
// the blast-radius guard. A .tar.gz that contains both
// allowlisted AND non-allowlisted paths (e.g. CLAUDE.md,
// MEMORY.md, .claude/sessions/foo) must have the non-allowlisted
// entries EXCLUDED from the returned map (the consumer's
// IsCPTemplateAssetPath check enforces the same invariant —
// the fetcher pre-filters as a perf + audit-log win).
func TestGiteaTemplateAssetFetcher_AllowsOnlyAllowlistedPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		// Allowlisted.
		mustWriteTar(t, tw, "repo-sha/config.yaml", []byte("ok"))
		mustWriteTar(t, tw, "repo-sha/prompts/x.md", []byte("ok"))
		// NOT allowlisted — must be excluded. agent-skills/* are plugins
		// now (RFC#2843 #32) and are no longer asset-eligible.
		mustWriteTar(t, tw, "repo-sha/agent-skills/skill-x/SKILL.md", []byte("plugin-source-not-asset"))
		mustWriteTar(t, tw, "repo-sha/CLAUDE.md", []byte("agent-owned"))
		mustWriteTar(t, tw, "repo-sha/MEMORY.md", []byte("agent-owned"))
		mustWriteTar(t, tw, "repo-sha/USER.md", []byte("agent-owned"))
		mustWriteTar(t, tw, "repo-sha/.claude/sessions/foo.json", []byte("agent-owned"))
		mustWriteTar(t, tw, "repo-sha/adapter.py", []byte("not-template-asset"))
		_ = tw.Close()
		_ = gz.Close()
	}))
	defer srv.Close()

	f := NewGiteaTemplateAssetFetcher(srv.URL, "the-token", srv.Client())
	assets, err := f.Load(context.Background(), "owner/repo@main")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Allowlisted keys present.
	mustHaveKey(t, assets, "config.yaml")
	mustHaveKey(t, assets, "prompts/x.md")
	// Non-allowlisted keys EXCLUDED — incl agent-skills/* (RFC#2843 #32).
	mustNotHaveKey(t, assets, "agent-skills/skill-x/SKILL.md")
	mustNotHaveKey(t, assets, "CLAUDE.md")
	mustNotHaveKey(t, assets, "MEMORY.md")
	mustNotHaveKey(t, assets, "USER.md")
	mustNotHaveKey(t, assets, ".claude/sessions/foo.json")
	mustNotHaveKey(t, assets, "adapter.py")
	if len(assets) != 2 {
		t.Errorf("expected 2 allowlisted assets, got %d: %v", len(assets), keysOf(assets))
	}
}

// TestGiteaTemplateAssetFetcher_FailsClosedOnHTTPError pins
// the fail-closed contract. A non-200 response (401, 404, 500)
// returns an error, NOT a silently-empty result.
func TestGiteaTemplateAssetFetcher_FailsClosedOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"internal server error"}`))
	}))
	defer srv.Close()

	f := NewGiteaTemplateAssetFetcher(srv.URL, "the-token", srv.Client())
	_, err := f.Load(context.Background(), "owner/repo@main")
	if err == nil {
		t.Fatal("expected error on 500 response, got nil (fail-closed violated)")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention the HTTP status, got: %v", err)
	}
}

// TestGiteaTemplateAssetFetcher_FailsClosedOnTransportError pins
// the fail-closed contract for transport-layer failures (DNS
// failure, connection refused, etc.). The fetcher must NOT
// silently return an empty map.
func TestGiteaTemplateAssetFetcher_FailsClosedOnTransportError(t *testing.T) {
	// Use a port that nothing is listening on (reserved by IANA
	// for "tcpmux"; 1 is "tcpmux" too). The dial will fail.
	f := NewGiteaTemplateAssetFetcher("http://127.0.0.1:1", "the-token", &http.Client{Timeout: 100 * time.Millisecond})
	_, err := f.Load(context.Background(), "owner/repo@main")
	if err == nil {
		t.Fatal("expected error on transport failure, got nil (fail-closed violated)")
	}
}

// TestGiteaTemplateAssetFetcher_EmptyToken_OmitsAuthHeader pins
// the public-fetch activation (driver RC 11907 on #2903, the
// runtime defect that the prior code rejected an empty token and
// left SaaS-no-token tenants with ZERO templates). The new
// contract: empty token → UNAUTHENTICATED request (Authorization
// header is OMITTED, NOT sent as "token " with an empty value,
// which Gitea 401s on as a malformed credential). This is the
// load-bearing pin that catches a regression to the buggy
// "always send Authorization" behavior.
//
// The test asserts three things:
//  1. NO "Authorization" header is set on the outgoing request
//     (the request map's lookup returns the zero value and the
//     "explicit" map presence is false — net/http normalizes
//     headers into a map[string][]string where unset keys are
//     simply absent, so a missing key and a ""-valued key are
//     distinguishable).
//  2. Load returns no error (a request was issued and a 200
//     response was processed into assets).
//  3. Assets are returned (the public-fetch path actually
//     delivered a payload, not an empty map).
func TestGiteaTemplateAssetFetcher_EmptyToken_OmitsAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pin: NO Authorization header should reach the server.
		if vals, ok := r.Header["Authorization"]; ok {
			t.Errorf("expected NO Authorization header, got %q (empty-token public-fetch must omit, not send \"token \" with empty value)", vals)
		}
		w.Header().Set("Content-Type", "application/gzip")
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		mustWriteTar(t, tw, "repo-sha/config.yaml", []byte("# public-template config\n"))
		_ = tw.Close()
		_ = gz.Close()
	}))
	defer srv.Close()

	f := NewGiteaTemplateAssetFetcher(srv.URL, "", nil)
	assets, err := f.Load(context.Background(), "owner/repo@main")
	if err != nil {
		t.Fatalf("Load: %v (empty-token public-fetch should succeed, NOT error)", err)
	}
	mustHaveKey(t, assets, "config.yaml")
	if len(assets) != 1 {
		t.Errorf("expected 1 asset, got %d: %v", len(assets), keysOf(assets))
	}
}

// TestGiteaTemplateAssetFetcher_EmptyToken_RealHTTP_NoAuthHeader_Success
// is the dispatch-required end-to-end pin: an empty token results
// in a real httptest-served HTTP request that has NO Authorization
// header, returns 200, and the fetcher's Load returns the parsed
// allowlisted assets. This is the bug that driver RC 11907 caught
// — the prior code's empty-token rejection at Load time meant a
// SaaS tenant with no MOLECULE_TEMPLATE_REPO_TOKEN got ZERO
// templates. The flip-side regression we guard against: a future
// refactor that re-introduces "Authorization: token " (empty value)
// would Gitea-401 and the same runtime defect returns. The header
// MUST be omitted, not just empty-valued.
func TestGiteaTemplateAssetFetcher_EmptyToken_RealHTTP_NoAuthHeader_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server-side assertion: NO Authorization header on the
		// request. We use Header map presence (not just Get,
		// which returns "" for both "absent" and "set to empty").
		// The map check is the load-bearing pin — it would
		// catch a regression to "Authorization: token " (empty
		// value) since net/http would set the key in the map
		// but with a [""] value, which still trips this check.
		if _, ok := r.Header["Authorization"]; ok {
			t.Errorf("server saw Authorization header (value=%q) — empty-token public-fetch must OMIT the header, not send an empty value", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		// Multiple allowlisted paths so the test verifies the
		// full extraction path on the public-fetch activation,
		// not just a minimal 1-asset happy path.
		mustWriteTar(t, tw, "repo-sha/config.yaml", []byte("# public-template config\n"))
		mustWriteTar(t, tw, "repo-sha/prompts/system.md", []byte("# public-template system prompt\n"))
		// agent-skills present in the repo but SKIPPED by the fetcher
		// (RFC#2843 #32 — plugins, not assets).
		mustWriteTar(t, tw, "repo-sha/agent-skills/skill-x/SKILL.md", []byte("# public-template skill\n"))
		_ = tw.Close()
		_ = gz.Close()
	}))
	defer srv.Close()

	f := NewGiteaTemplateAssetFetcher(srv.URL, "", srv.Client())
	assets, err := f.Load(context.Background(), "owner/repo@main")
	if err != nil {
		t.Fatalf("Load: %v (empty-token public-fetch should succeed against a public-template mock)", err)
	}
	mustHaveKey(t, assets, "config.yaml")
	mustHaveKey(t, assets, "prompts/system.md")
	mustNotHaveKey(t, assets, "agent-skills/skill-x/SKILL.md")
	if len(assets) != 2 {
		t.Errorf("expected 2 assets (config.yaml + prompts/system.md), got %d: %v", len(assets), keysOf(assets))
	}
}

// TestParseTemplateIdentity pins the identity parser. Format:
// "<owner>/<repo>@<ref>". Malformed identities return errors.
func TestParseTemplateIdentity(t *testing.T) {
	cases := []struct {
		name      string
		identity  string
		wantOwner string
		wantRepo  string
		wantRef   string
		wantErr   bool
	}{
		{"simple", "owner/repo@main", "owner", "repo", "main", false},
		{"with-sha-ref", "owner/repo@abcd1234", "owner", "repo", "abcd1234", false},
		{"with-tag-ref", "owner/repo@v1.2.3", "owner", "repo", "v1.2.3", false},
		{"nested-owner", "molecule-ai/workspace-template-seo@main", "molecule-ai", "workspace-template-seo", "main", false},
		{"empty", "", "", "", "", true},
		{"no-at", "owner/repo", "", "", "", true},
		{"empty-ref", "owner/repo@", "", "", "", true},
		{"no-slash", "owner@main", "", "", "", true},
		{"empty-owner", "/repo@main", "", "", "", true},
		{"empty-repo", "owner/@main", "", "", "", true},
		{"extra-at", "owner/repo@main@extra", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner, repo, ref, err := parseTemplateIdentity(c.identity)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got nil", c.identity)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error for %q: %v", c.identity, err)
				return
			}
			if owner != c.wantOwner {
				t.Errorf("owner = %q, want %q", owner, c.wantOwner)
			}
			if repo != c.wantRepo {
				t.Errorf("repo = %q, want %q", repo, c.wantRepo)
			}
			if ref != c.wantRef {
				t.Errorf("ref = %q, want %q", ref, c.wantRef)
			}
		})
	}
}

// TestStripArchiveTopDir pins the top-level dir stripper. Gitea
// wraps entries in "<repo>-<sha>/<relpath>"; we want just the
// relpath. Top-level entries (no slash) and traversal attempts
// (../) are rejected.
func TestStripArchiveTopDir(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		wantOk bool
		want   string
	}{
		{"normal", "repo-sha/config.yaml", true, "config.yaml"},
		{"nested", "repo-sha/prompts/system.md", true, "prompts/system.md"},
		{"deep", "repo-sha/agent-skills/seo/SKILL.md", true, "agent-skills/seo/SKILL.md"},
		{"top-level-no-slash", "config.yaml", false, ""},
		{"empty", "", false, ""},
		{"slash-only", "/", false, ""},
		{"dot-only", "repo-sha/.", false, ""},
		{"traversal", "repo-sha/../etc/passwd", false, ""},
		{"deep-traversal", "repo-sha/prompts/../../etc", false, ""},
		{"dotdot-in-name", "repo-sha/foo..bar", true, "foo..bar"}, // not a traversal, just an unusual filename
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := stripArchiveTopDir(c.in)
			if ok != c.wantOk {
				t.Errorf("ok = %v, want %v (input %q)", ok, c.wantOk, c.in)
				return
			}
			if ok && got != c.want {
				t.Errorf("got = %q, want %q (input %q)", got, c.want, c.in)
			}
		})
	}
}

// ---- Test helpers ----

// mustWriteTar writes a single tar entry with the given name +
// data. Errors are reported via t (so the test fails cleanly).
// Wrapped in a helper to keep the test bodies focused on the
// content (not the tar boilerplate).
func mustWriteTar(t *testing.T, tw *tar.Writer, name string, data []byte) {
	t.Helper()
	hdr := &tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar WriteHeader %s: %v", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("tar Write %s: %v", name, err)
	}
}

// mustHaveKey asserts assets contains key. Failure reports the
// key + the full set so the test message is informative.
func mustHaveKey(t *testing.T, assets map[string][]byte, key string) {
	t.Helper()
	if _, ok := assets[key]; !ok {
		t.Errorf("expected key %q in assets, got keys: %v", key, keysOf(assets))
	}
}

// mustNotHaveKey asserts assets does NOT contain key. The
// blast-radius guard's load-bearing assertion.
func mustNotHaveKey(t *testing.T, assets map[string][]byte, key string) {
	t.Helper()
	if _, ok := assets[key]; ok {
		t.Errorf("did NOT expect key %q in assets (not in allowlist), but it's there: %v", key, keysOf(assets))
	}
}

// keysOf returns a stable-ish view of the map's keys for
// assertion error messages. Local helper to avoid coupling to
// test-fixture conventions elsewhere.
func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Compile-time check: archive/tar is imported for the test
// server's .tar.gz generation. (Avoids a "imported and not
// used" lint in case future refactors move the helper.)
var _ = tar.TypeReg

// Compile-time check: bytes is imported for the test sentinel.
// Avoids unused-import in future refactors.
var _ = bytes.NewReader

// Compile-time check: io is imported for io.LimitReader.
// Avoids unused-import in future refactors.
var _ = io.LimitReader
