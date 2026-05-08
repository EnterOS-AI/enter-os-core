package handlers

// plugins_classifier.go — diff classifier for plugin updates.
//
// Closes molecule-core#112. Composes with #114 (atomic install) so the
// platform can decide *before* triggering restartFunc whether the
// update is content-only (SKILL.md text changed; agent re-reads at next
// Skill invocation) or structural (hooks/settings/plugin.yaml/file added
// or removed; agent must restart to pick up the new state).
//
// SKILL.md content is hot-reloadable because Claude Code reads the file
// on each Skill invocation — no in-memory cache. Hooks and settings.json
// are loaded at session start and need a session restart. plugin.yaml
// changes are structural by definition (manifest controls everything
// else).
//
// CLASSIFICATION RULE
//   classify(staged, live) → "skill-content-only" if and only if
//     every file present in either tree is one of:
//       - identical between staged and live, OR
//       - a **/SKILL.md file with content change (text body modified)
//     AND no files were added or removed.
//   Anything else → "cold" (the safe default).
//
// The classifier reads live-tree files from inside the container via
// `docker exec cat`. Comparison is by SHA-256 over file content, not
// mtime — mtime changes on every install regardless of content.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// classifyKindSkillContentOnly: install can skip restartFunc; the
	// only changes are SKILL.md body text.
	classifyKindSkillContentOnly = "skill-content-only"
	// classifyKindCold: must restart the workspace container; structural
	// or hook/settings change.
	classifyKindCold = "cold"
)

// classifyInstallChanges compares the staged plugin tree (host filesystem)
// against the currently-live plugin tree inside the container. Returns
// classifyKindSkillContentOnly when the only diff is SKILL.md content
// changes, classifyKindCold otherwise (added/removed files, hooks/
// settings.json edits, plugin.yaml edits, anything else).
//
// `noLive` is the sentinel returned when /configs/plugins/<name> doesn't
// exist (first install for this plugin). Treated as cold — no live state
// to hot-reload into.
func (h *PluginsHandler) classifyInstallChanges(
	ctx context.Context, containerName, hostStagedDir, pluginName string,
) (string, error) {
	livePath := "/configs/plugins/" + pluginName

	// Probe: does live exist? If not, this is a first install — cold.
	if _, err := h.execAsRoot(ctx, containerName, []string{
		"test", "-d", livePath,
	}); err != nil {
		return classifyKindCold, nil
	}

	// Build hash maps for both trees.
	stagedHashes, err := hashLocalTree(hostStagedDir)
	if err != nil {
		return classifyKindCold, fmt.Errorf("classifier: hash staged: %w", err)
	}
	liveHashes, err := h.hashContainerTree(ctx, containerName, livePath)
	if err != nil {
		// Live tree read failure: be conservative, cold-restart.
		return classifyKindCold, nil
	}

	// Drop the .complete marker from comparison — its mtime/atime can
	// vary across installs but content is empty/trivial; including it
	// would force-cold every reinstall.
	delete(stagedHashes, ".complete")
	delete(liveHashes, ".complete")

	// Set difference: any file in one but not the other → cold.
	for rel := range stagedHashes {
		if _, ok := liveHashes[rel]; !ok {
			return classifyKindCold, nil // file added
		}
	}
	for rel := range liveHashes {
		if _, ok := stagedHashes[rel]; !ok {
			return classifyKindCold, nil // file removed
		}
	}

	// Same set of files. Walk the diff.
	for rel, stagedHash := range stagedHashes {
		liveHash := liveHashes[rel]
		if stagedHash == liveHash {
			continue
		}
		// Content differs. Allow if and only if it's a SKILL.md.
		if !isSkillMarkdown(rel) {
			return classifyKindCold, nil
		}
	}
	return classifyKindSkillContentOnly, nil
}

// isSkillMarkdown returns true for any path whose basename is SKILL.md
// (case-sensitive, matches Claude Code's skill discovery rule).
func isSkillMarkdown(rel string) bool {
	return filepath.Base(rel) == "SKILL.md"
}

// hashLocalTree walks a host directory and returns rel-path → sha256-hex.
// Symlinks are skipped (same posture as the tar walker).
func hashLocalTree(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// hashContainerTree reads every regular file under livePath via docker
// exec sh -c 'cd <livePath> && find . -type f -not -name .complete | xargs -I {} sh -c "echo {}; sha256sum {}"'.
//
// The output is parsed line-by-line into rel-path → sha256-hex.
func (h *PluginsHandler) hashContainerTree(
	ctx context.Context, containerName, livePath string,
) (map[string]string, error) {
	out, err := h.execAsRoot(ctx, containerName, []string{
		"sh", "-c",
		// Find regular files, hash each, output `<hex>  ./<relpath>`.
		// `cd` then `find .` keeps paths relative to livePath.
		fmt.Sprintf("cd %s && find . -type f -print0 | xargs -0 -r sha256sum 2>/dev/null", shQuote(livePath)),
	})
	if err != nil {
		return nil, fmt.Errorf("hash container tree: %w", err)
	}
	hashes := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// sha256sum output: "<hex>  <path>" (two spaces). Path starts with "./".
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			continue
		}
		hash := parts[0]
		rel := strings.TrimPrefix(parts[1], "./")
		hashes[rel] = hash
	}
	return hashes, nil
}

// shQuote single-quotes a string for safe insertion into a shell command.
// Returns the input unchanged if it's already shell-safe (alphanumeric +
// /._-). Otherwise wraps in single quotes and escapes inner '.
func shQuote(s string) string {
	safe := true
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '/' || c == '.' || c == '_' || c == '-':
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
