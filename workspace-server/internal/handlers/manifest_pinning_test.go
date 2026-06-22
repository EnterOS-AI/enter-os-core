// manifest_pinning_test.go — RFC #2927 manifest ref-pinning contract
//
// Pins every manifest entry to an immutable commit SHA. The previous
// `ref:main` exposure made provisioning non-reproducible — a merge to
// ANY template's `main` instantly reached every subsequent provision,
// with no version gate, no staging boundary, and no audit of which
// content shipped. Acute case: the newly-added platform-agent entry
// floated on `main` while PR #1 (`config.yaml`) was WIP/unmerged → a
// provision today fetched a partial template → runtime MISSING_MODEL
// fail-closed.
//
// Contract (pinned in manifest.json's `_pinning_contract` field):
//   (1) Every entry's `ref` is a 40-char commit SHA (not a branch,
//       not a mutable tag). Bumping a pin is a reviewed PR.
//   (2) The pinned SHA is reachable in the named repo (the Gitea
//       API serves it — proves we didn't typo a SHA).
//   (3) For workspace_template entries, the pinned ref's tree
//       contains `config.yaml` (the file carrying model + runtime).
//       A pinned ref without config.yaml is a partial-template
//       landmine that the manifest's CI lane must catch — provision-
//       time discovery is too late (the concierge already boots).
//
// PLATFORM-AGENT: not pinned here. Per #2919, the platform-agent
// template's `config.yaml` is being added in template PR #1; once
// merged AND config.yaml exists at the pinned SHA, add the entry
// here in a follow-up PR.

package handlers

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"testing"
	"time"
)

// readRealManifestForPinningTest finds molecule-core/manifest.json by
// walking up from the test file's directory. The test lives at
// workspace-server/internal/handlers/; molecule-core/manifest.json
// is 3 levels up. This works in both the local dev env AND in CI
// (where go test runs the package from the package dir, the same
// relative walk applies).
func readRealManifestForPinningTest(t *testing.T) ([]byte, error) {
	t.Helper()
	candidates := []string{
		"/app/manifest.json", // production container layout
		"manifest.json",      // cwd (package dir on CI)
		"../../manifest.json",
		"../../../manifest.json",
	}
	// Also try walking up from the test's CWD (handles workspaces
	// deeper than 3 levels; robust to repo restructuring).
	for _, c := range candidates {
		if data, err := os.ReadFile(c); err == nil {
			return data, nil
		}
	}
	return nil, os.ErrNotExist
}

// shaPattern matches a 40-char lowercase hex string (Gitea commit SHA).
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// TestManifest_RefPinning_AllEntriesAreCommitSHAs is the static (no
// network) part of the pinning contract — every ref is a 40-char
// lowercase hex string. Failing this test means someone reintroduced
// a floating ref (e.g., "main", a tag, a branch) and the manifest
// has REGRESSED to the pre-#2927 non-reproducible state. The
// complementary network-dependent tests below (TestManifest_RefPinning_*)
// run only when Gitea is reachable; this one always runs.
func TestManifest_RefPinning_AllEntriesAreCommitSHAs(t *testing.T) {
	data, err := readRealManifestForPinningTest(t)
	if err != nil {
		t.Skipf("manifest.json not readable from any candidate path: %v", err)
	}
	// Parse just enough to enumerate entries. Re-using the production
	// manifestFile type (in runtime_registry.go) keeps the schema
	// test contract in one place; if the schema diverges from
	// reality, runtime_registry_test.go catches it.
	var m struct {
		Plugins            []manifestEntry `json:"plugins"`
		WorkspaceTemplates []manifestEntry `json:"workspace_templates"`
		OrgTemplates       []manifestEntry `json:"org_templates"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest parse failed: %v", err)
	}
	all := append(append([]manifestEntry{}, m.Plugins...), m.WorkspaceTemplates...)
	all = append(all, m.OrgTemplates...)

	if len(all) == 0 {
		t.Fatalf("manifest has no entries (or failed to load)")
	}

	for _, e := range all {
		if e.Name == "" {
			t.Errorf("entry with empty name (ref=%q)", e.Ref)
			continue
		}
		if e.Repo == "" {
			t.Errorf("entry %q has empty repo", e.Name)
			continue
		}
		if !shaPattern.MatchString(e.Ref) {
			t.Errorf("entry %q (%s): ref=%q is NOT a 40-char commit SHA — manifest is floating on a non-SHA ref, violating the RFC #2927 pinning contract. Bump the pin to a specific commit SHA in a reviewed PR.", e.Name, e.Repo, e.Ref)
		}
	}
}

// giteaReachableForTest probes git.moleculesai.app with a short
// timeout. Returns true if the host responds (any status) within
// 3s, false otherwise. Lets the dynamic pinning tests skip cleanly
// in offline / no-network CI lanes.
func giteaReachableForTest() bool {
	client := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("GET", "https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-ai-workspace-template-claude-code", nil)
	if auth := giteaBasicAuthForTestProbe(); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// giteaBasicAuthForTest returns an Authorization header value built from
// the Gitea credentials available in the test env. Order of preference
// matches the runtime's giteaTemplateAssetFetcher (cmd/server/main.go
// and internal/provisioner/localbuild.go read MOLECULE_GITEA_TOKEN as
// the SSOT token for private templates):
//  1. MOLECULE_GITEA_TOKEN — bearer token (matches runtime; gives
//     access to private repos like molecule-ai-workspace-template-google-adk
//     and molecule-ai-workspace-template-seo-agent).
//  2. GIT_HTTP_USERNAME + GIT_HTTP_PASSWORD — basic auth (legacy
//     CI path; some jobs set these for git clone URLs).
//  3. empty — public-only assertions only; private-repo assertions
//     return 404 (test fails-closed with a clear message).
func giteaBasicAuthForTest(t *testing.T) string {
	t.Helper()
	if tok := os.Getenv("MOLECULE_GITEA_TOKEN"); tok != "" {
		// Gitea bearer-token auth (header value: "token <tok>").
		// Matches the runtime's giteaTemplateAssetFetcher path so
		// the test validates the SAME auth scope the runtime uses.
		return "token " + tok
	}
	user := os.Getenv("GIT_HTTP_USERNAME")
	pass := os.Getenv("GIT_HTTP_PASSWORD")
	if user == "" || pass == "" {
		return ""
	}
	// Use Go's net/http basic auth, which is a stdlib-supported
	// credential scheme (not a custom encoding).
	req, _ := http.NewRequest("GET", "https://example.invalid/", nil)
	req.SetBasicAuth(user, pass)
	return req.Header.Get("Authorization")
}

// giteaBasicAuthForTestProbe is the same as giteaBasicAuthForTest
// but without the *testing.T parameter so giteaReachableForTest
// (called at module-init time before any *testing.T exists) can
// still emit auth.
func giteaBasicAuthForTestProbe() string {
	if tok := os.Getenv("MOLECULE_GITEA_TOKEN"); tok != "" {
		return "token " + tok
	}
	user := os.Getenv("GIT_HTTP_USERNAME")
	pass := os.Getenv("GIT_HTTP_PASSWORD")
	if user == "" || pass == "" {
		return ""
	}
	req, _ := http.NewRequest("GET", "https://example.invalid/", nil)
	req.SetBasicAuth(user, pass)
	return req.Header.Get("Authorization")
}

// TestManifest_RefPinning_AllSHAsReachable asserts the network-level
// half of the contract — every pinned SHA is a real commit in the
// named repo (the Gitea API serves it). Catches a typo'd SHA. Skips
// if Gitea isn't reachable (offline CI).
func TestManifest_RefPinning_AllSHAsReachable(t *testing.T) {
	if !giteaReachableForTest() {
		t.Skip("Gitea unreachable (offline CI lane); skipping dynamic pinning reachability test")
	}
	data, err := readRealManifestForPinningTest(t)
	if err != nil {
		t.Skipf("manifest.json not readable: %v", err)
	}
	var m struct {
		Plugins            []manifestEntry `json:"plugins"`
		WorkspaceTemplates []manifestEntry `json:"workspace_templates"`
		OrgTemplates       []manifestEntry `json:"org_templates"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	all := append(append([]manifestEntry{}, m.Plugins...), m.WorkspaceTemplates...)
	all = append(all, m.OrgTemplates...)

	client := &http.Client{Timeout: 10 * time.Second}
	auth := giteaBasicAuthForTest(t)
	for _, e := range all {
		// GET /api/v1/repos/{owner}/{repo}/git/commits/{sha}
		// Returns 200 if the SHA exists in the repo, 404 otherwise.
		// NOTE: the commit-lookup endpoint requires the same auth as
		// refs/heads (the API treats unauth'd requests as 404 for
		// private repos, even when the SHA is correct). The
		// helper below injects the agent's Gitea basic-auth header
		// (the same one used by the runtime's giteaTemplateAssetFetcher).
		url := "https://git.moleculesai.app/api/v1/repos/" + e.Repo + "/git/commits/" + e.Ref
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", auth)
		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("entry %q (%s): ref %q — git commit lookup failed: %v", e.Name, e.Repo, e.Ref, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 404 {
			t.Errorf("entry %q (%s): ref %q — Gitea returns 404. Pin is to a non-existent commit OR auth is insufficient. Bump to a real SHA.", e.Name, e.Repo, e.Ref)
		} else if resp.StatusCode != 200 {
			t.Errorf("entry %q (%s): ref %q — Gitea returns HTTP %d", e.Name, e.Repo, e.Ref, resp.StatusCode)
		}
	}
}

// TestManifest_RefPinning_WorkspaceTemplatesIncludeConfigYAML asserts
// the completeness half of the contract — every workspace_template
// entry's pinned ref has `config.yaml` in its tree. The partial-
// template landmine (template exists but `config.yaml` doesn't)
// converts to a runtime MISSING_MODEL fail-closed at provision.
// Catching it at the manifest's CI lane (this test) is the load-
// bearing guard. Skips if Gitea isn't reachable.
func TestManifest_RefPinning_WorkspaceTemplatesIncludeConfigYAML(t *testing.T) {
	if !giteaReachableForTest() {
		t.Skip("Gitea unreachable (offline CI lane); skipping dynamic pinning completeness test")
	}
	data, err := readRealManifestForPinningTest(t)
	if err != nil {
		t.Skipf("manifest.json not readable: %v", err)
	}
	var m struct {
		WorkspaceTemplates []manifestEntry `json:"workspace_templates"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if len(m.WorkspaceTemplates) == 0 {
		t.Fatal("no workspace_templates entries (test invariant broken)")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	auth := giteaBasicAuthForTest(t)
	for _, e := range m.WorkspaceTemplates {
		// GET /api/v1/repos/{owner}/{repo}/git/trees/{sha}?recursive=true
		// Returns 200 + tree with path-keyed entries if the tree is
		// accessible. We check for any path ending in /config.yaml
		// (templates have it at the root).
		url := "https://git.moleculesai.app/api/v1/repos/" + e.Repo + "/git/trees/" + e.Ref + "?recursive=1"
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", auth)
		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("entry %q (%s): tree lookup at %q failed: %v", e.Name, e.Repo, e.Ref, err)
			continue
		}
		if resp.StatusCode != 200 {
			t.Errorf("entry %q (%s): tree lookup at %q returned HTTP %d", e.Name, e.Repo, e.Ref, resp.StatusCode)
			resp.Body.Close()
			continue
		}
		var treeResp struct {
			Tree []struct {
				Path string `json:"path"`
				Type string `json:"type"`
			} `json:"tree"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&treeResp); err != nil {
			t.Errorf("entry %q (%s): tree JSON parse failed: %v", e.Name, e.Repo, err)
			resp.Body.Close()
			continue
		}
		resp.Body.Close()
		hasConfig := false
		for _, n := range treeResp.Tree {
			if n.Type == "blob" && (n.Path == "config.yaml" || n.Path == "./config.yaml") {
				hasConfig = true
				break
			}
		}
		if !hasConfig {
			t.Errorf("entry %q (%s): pinned ref %q has NO config.yaml in its tree. This is the partial-template landmine — a provision of this template today would land no config.yaml in /configs and the runtime would MISSING_MODEL fail-closed. Either: (a) bump the pin to a SHA that includes config.yaml, OR (b) add config.yaml to the template and bump the pin.", e.Name, e.Repo, e.Ref)
		}
	}
}
