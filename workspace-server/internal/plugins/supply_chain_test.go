package plugins

// TDD specification for plugin supply-chain hardening — issue #768.
//
// Two security controls are being added to github.go and a new
// supply_chain.go (or plugins_install_pipeline.go):
//
//  1. SHA256 content-integrity: after fetching a plugin, if the staged
//     directory contains a manifest.json with a "sha256" field, that field
//     must match the computed hash of the staged tree. A mismatch aborts
//     install before any files reach a workspace.
//
//  2. Pinned-ref enforcement: GithubResolver.Fetch rejects bare
//     "org/repo" specs that carry no "#tag" or "#sha" fragment. Only
//     pinned refs ("org/repo#v1.2.3", "org/repo#abc1234") are accepted.
//     PLUGIN_ALLOW_UNPINNED=true skips this check for local dev.
//
// All tests in this file are intentionally RED:
//   - TestPluginInstall_SHA256*   → compile error: VerifyManifestIntegrity
//                                   is not yet defined in this package.
//   - TestPluginInstall_Unpinned* → runtime assertion failure: GithubResolver
//                                   currently accepts bare refs without error.
//   - TestPluginInstall_Pinned*   → runtime pass (already green before impl).
//
// Backend Engineer: implement VerifyManifestIntegrity in a new
// supply_chain.go (package plugins) and add the pinned-ref gate to
// GithubResolver.Fetch in github.go. All 7 tests must be GREEN before merge.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────────
// Test helpers — canonical hash shared by tests and the implementation
// ──────────────────────────────────────────────────────────────────────────────

// stagedDirDigest computes the canonical SHA256 that VerifyManifestIntegrity
// uses to validate staged plugin content. Algorithm:
//
//  1. Walk all regular files in dir, skipping "manifest.json" itself.
//  2. For each file, build the string "<rel-path>\x00<file-content>".
//  3. Sort the strings lexicographically by relative path.
//  4. Concatenate and SHA256-hash the result.
//  5. Return the lower-case hex digest.
//
// The implementation MUST use this same algorithm so tests are deterministic.
// The choice of a sorted walk over individual file hashes avoids sensitivity
// to filesystem entry ordering across operating systems.
func stagedDirDigest(t *testing.T, dir string) string {
	t.Helper()
	var entries []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		// Exclude the manifest itself — it is the verifier, not the verified.
		if rel == "manifest.json" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, rel+"\x00"+string(content))
		return nil
	})
	if err != nil {
		t.Fatalf("stagedDirDigest: walk error: %v", err)
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "")))
	return hex.EncodeToString(sum[:])
}

// writeManifestJSON writes {"sha256": digest} to dir/manifest.json.
func writeManifestJSON(t *testing.T, dir, digest string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{"sha256": digest})
	if err != nil {
		t.Fatalf("writeManifestJSON: marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("writeManifestJSON: write: %v", err)
	}
}

// writeStagedPlugin writes a minimal but realistic plugin tree to dir.
func writeStagedPlugin(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"plugin.yaml":            "name: test-plugin\nversion: 1.0.0\ndescription: supply chain test\n",
		"rules/guidelines.md":    "# Plugin Guidelines\nFollow the rules.\n",
		"skills/helper/SKILL.md": "---\nid: helper\nname: Helper\ndescription: does stuff\n---\n",
	}
	for relPath, content := range files {
		full := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("writeStagedPlugin: mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("writeStagedPlugin: write %s: %v", relPath, err)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// SHA256 content-integrity tests (#768 Control 1)
//
// These tests call VerifyManifestIntegrity, which does not yet exist in this
// package. They will cause a COMPILE ERROR (build failure) until the Backend
// Engineer adds supply_chain.go with the following exported signature:
//
//   func VerifyManifestIntegrity(stagedDir string) error
//
// Behaviour contract:
//   - manifest.json absent          → nil (backward compat)
//   - manifest.json present, no sha256 field → nil (backward compat)
//   - sha256 field matches computed digest   → nil
//   - sha256 field doesn't match            → non-nil error
// ──────────────────────────────────────────────────────────────────────────────

// TestPluginInstall_SHA256Match_Succeeds verifies that when manifest.json
// carries the correct sha256 of the staged tree, VerifyManifestIntegrity
// returns nil and install is allowed to proceed.
func TestPluginInstall_SHA256Match_Succeeds(t *testing.T) {
	dir := t.TempDir()
	writeStagedPlugin(t, dir)

	// Compute the canonical digest of the staged files, then write a
	// manifest.json that claims exactly that digest (correct attestation).
	digest := stagedDirDigest(t, dir)
	writeManifestJSON(t, dir, digest)

	// VerifyManifestIntegrity is defined in the not-yet-written supply_chain.go.
	// This line causes a compile error until the implementation exists.
	if err := VerifyManifestIntegrity(dir); err != nil {
		t.Errorf("expected nil error when SHA256 matches: got %v", err)
	}
}

// TestPluginInstall_SHA256Mismatch_AbortsInstall verifies that when
// manifest.json carries the WRONG sha256, VerifyManifestIntegrity returns
// a non-nil error. No files should be staged (the pipeline must abort before
// deliverToContainer).
func TestPluginInstall_SHA256Mismatch_AbortsInstall(t *testing.T) {
	dir := t.TempDir()
	writeStagedPlugin(t, dir)

	// Write a manifest.json with a deliberately wrong digest.
	writeManifestJSON(t, dir, "0000000000000000000000000000000000000000000000000000000000000000")

	err := VerifyManifestIntegrity(dir) // compile error until supply_chain.go exists
	if err == nil {
		t.Error("expected non-nil error when SHA256 mismatches, got nil — " +
			"a tampered/corrupted plugin must not be staged")
	}
	// The error message must be informative enough for operators.
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "sha256") {
		t.Errorf("error must mention 'sha256', got: %v", err)
	}
}

// TestPluginInstall_SHA256Missing_Skips_Check verifies backward compatibility:
// when manifest.json is absent (or present but has no sha256 field), the check
// is skipped and VerifyManifestIntegrity returns nil. This preserves install
// behaviour for plugins that pre-date the supply-chain hardening.
func TestPluginInstall_SHA256Missing_Skips_Check(t *testing.T) {
	t.Run("no manifest.json", func(t *testing.T) {
		dir := t.TempDir()
		writeStagedPlugin(t, dir)
		// No manifest.json at all — check must be skipped.
		if err := VerifyManifestIntegrity(dir); err != nil { // compile error until impl
			t.Errorf("no manifest.json → expected nil error, got %v", err)
		}
	})

	t.Run("manifest.json without sha256 field", func(t *testing.T) {
		dir := t.TempDir()
		writeStagedPlugin(t, dir)
		// Write a manifest.json that has other metadata but no sha256 key.
		data, _ := json.Marshal(map[string]string{
			"name":    "test-plugin",
			"version": "1.0.0",
		})
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := VerifyManifestIntegrity(dir); err != nil { // compile error until impl
			t.Errorf("manifest.json without sha256 → expected nil error, got %v", err)
		}
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// Pinned-ref enforcement tests (#768 Control 2)
//
// GithubResolver.Fetch currently accepts bare "org/repo" specs (no "#ref").
// After the implementation adds the pinned-ref gate to github.go, bare refs
// must be rejected with an error whose message contains "pinned ref".
//
// RED state: TestPluginInstall_UnpinnedRef_Rejected and
//            TestPluginInstall_UnpinnedRef_AllowedByEnvVar will both fail at
//            runtime because GithubResolver.Fetch currently returns nil for
//            bare refs. TestPluginInstall_Pinned*_Accepted tests may already
//            pass (positive case) but are included to pin the contract.
// ──────────────────────────────────────────────────────────────────────────────

// TestPluginInstall_UnpinnedRef_Rejected verifies that a bare GitHub spec
// without a "#ref" fragment ("org/repo") is rejected before any network
// activity. The error must mention "pinned ref" so operators understand the
// fix (add a tag or SHA to the install spec).
func TestPluginInstall_UnpinnedRef_Rejected(t *testing.T) {
	// Ensure PLUGIN_ALLOW_UNPINNED is not set (the default production state).
	t.Setenv("PLUGIN_ALLOW_UNPINNED", "")

	r := &GithubResolver{
		GitRunner: func(ctx context.Context, dir string, args ...string) error {
			// If this is called, the pinned-ref gate did NOT fire — test failure.
			t.Error("GitRunner must not be called for unpinned refs: " +
				"the rejection must happen before any clone attempt")
			return nil
		},
		BaseURL: "file:///dev/null",
	}

	_, err := r.Fetch(context.Background(), "org/repo", t.TempDir())
	if err == nil {
		t.Fatal("expected non-nil error for unpinned ref 'org/repo', got nil — " +
			"bare GitHub refs must be rejected to prevent supply-chain drift")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "pinned ref") {
		t.Errorf("error must mention 'pinned ref' so operators know the fix; got: %v", err)
	}
}

// TestPluginInstall_PinnedTagRef_Accepted verifies that a ref pinned to a
// semantic-version tag ("org/repo#v1.2.3") is accepted by the gate and
// passed through to git clone.
func TestPluginInstall_PinnedTagRef_Accepted(t *testing.T) {
	t.Setenv("PLUGIN_ALLOW_UNPINNED", "")

	r := &GithubResolver{
		GitRunner: stubGit(map[string]string{
			"plugin.yaml": "name: pinned-tag-plugin\nversion: 1.2.3\n",
		}),
		BaseURL: "file:///dev/null",
	}

	_, err := r.Fetch(context.Background(), "org/repo#v1.2.3", t.TempDir())
	if err != nil {
		t.Fatalf("pinned tag ref 'org/repo#v1.2.3' must be accepted: %v", err)
	}
}

// TestPluginInstall_PinnedSHARef_Accepted verifies that a ref pinned to a
// full 40-char git SHA ("org/repo#abc1234...") is accepted by the gate.
// Partial SHAs (e.g. "abc1234") are also accepted — the gate only requires
// a non-empty fragment, not a canonical SHA length.
func TestPluginInstall_PinnedSHARef_Accepted(t *testing.T) {
	t.Setenv("PLUGIN_ALLOW_UNPINNED", "")

	fullSHA := "abc1234567890abcdef1234567890abcdef123456"
	r := &GithubResolver{
		GitRunner: stubGit(map[string]string{
			"plugin.yaml": "name: pinned-sha-plugin\nversion: 0.0.1\n",
		}),
		BaseURL: "file:///dev/null",
	}

	_, err := r.Fetch(context.Background(), "org/repo#"+fullSHA, t.TempDir())
	if err != nil {
		t.Fatalf("pinned SHA ref must be accepted: %v", err)
	}
}

// TestPluginInstall_UnpinnedRef_AllowedByEnvVar verifies that setting
// PLUGIN_ALLOW_UNPINNED=true bypasses the pinned-ref gate. This is the
// local-development escape hatch — it must never be set in production.
func TestPluginInstall_UnpinnedRef_AllowedByEnvVar(t *testing.T) {
	t.Setenv("PLUGIN_ALLOW_UNPINNED", "true")

	r := &GithubResolver{
		GitRunner: stubGit(map[string]string{
			"plugin.yaml": "name: dev-unpinned-plugin\nversion: 0.0.0-dev\n",
		}),
		BaseURL: "file:///dev/null",
	}

	// With the escape hatch enabled, the bare ref must be accepted.
	_, err := r.Fetch(context.Background(), "org/repo", t.TempDir())
	if err != nil {
		t.Fatalf("unpinned ref must be accepted when PLUGIN_ALLOW_UNPINNED=true: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Contract pinning: SHA256 + pinned-ref together (#768 end-to-end)
// ──────────────────────────────────────────────────────────────────────────────

// TestPluginInstall_PinnedRef_And_ValidSHA256_Succeeds confirms that a
// correctly pinned ref combined with a matching sha256 is the fully
// hardened path that must succeed end-to-end.
func TestPluginInstall_PinnedRef_And_ValidSHA256_Succeeds(t *testing.T) {
	t.Setenv("PLUGIN_ALLOW_UNPINNED", "")

	dir := t.TempDir()
	r := &GithubResolver{
		GitRunner: func(ctx context.Context, cloneDir string, args ...string) error {
			// Simulate clone: write plugin files to the clone target.
			target := args[len(args)-1]
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			return os.WriteFile(
				filepath.Join(target, "plugin.yaml"),
				[]byte("name: hardened-plugin\nversion: 2.0.0\n"),
				0o600,
			)
		},
		BaseURL: "file:///dev/null",
	}

	// Fetch into dir with a pinned ref — pinned-ref gate must pass.
	pluginName, err := r.Fetch(context.Background(), "org/repo#v2.0.0", dir)
	if err != nil {
		t.Fatalf("pinned-ref fetch failed: %v", err)
	}
	if pluginName == "" {
		t.Error("expected non-empty plugin name")
	}

	// Now compute digest and verify SHA256 integrity — must also pass.
	digest := stagedDirDigest(t, dir)
	writeManifestJSON(t, dir, digest)

	if err := VerifyManifestIntegrity(dir); err != nil { // compile error until impl
		t.Errorf("expected nil for matching SHA256 on pinned-ref fetch: %v", err)
	}
}
