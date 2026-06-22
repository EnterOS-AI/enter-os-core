package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// VerifyManifestIntegrity checks the SHA256 content hash declared in
// manifest.json against the actual contents of stagedDir.
//
// Behaviour:
//   - manifest.json absent              → nil (backward compat with pre-#768 plugins)
//   - manifest.json present, no sha256  → nil (same backward compat)
//   - sha256 field matches digest       → nil
//   - sha256 field doesn't match        → non-nil error
func VerifyManifestIntegrity(stagedDir string) error {
	manifestPath := filepath.Join(stagedDir, "manifest.json")

	data, err := os.ReadFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil // no manifest — backward compat, skip check
	}
	if err != nil {
		return fmt.Errorf("supply chain: read manifest.json: %w", err)
	}

	var manifest map[string]interface{}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("supply chain: parse manifest.json: %w", err)
	}

	declaredRaw, ok := manifest["sha256"]
	if !ok {
		return nil // no sha256 field — backward compat
	}
	declared, ok := declaredRaw.(string)
	if !ok {
		return fmt.Errorf("supply chain: sha256 field must be a string")
	}

	computed := computeStagedDigest(stagedDir)
	if !strings.EqualFold(declared, computed) {
		return fmt.Errorf("supply chain: sha256 mismatch — declared %s, computed %s", declared, computed)
	}
	return nil
}

// computeStagedDigest computes the canonical SHA256 digest of a staged plugin
// directory. Algorithm:
//  1. Walk all regular files, skipping manifest.json itself.
//  2. For each file, build "<rel-path>\x00<content>".
//  3. Sort lexicographically by relative path.
//  4. Concatenate and SHA256-hash.
//  5. Return lower-case hex digest.
func computeStagedDigest(dir string) string {
	var entries []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
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
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "")))
	return hex.EncodeToString(sum[:])
}
