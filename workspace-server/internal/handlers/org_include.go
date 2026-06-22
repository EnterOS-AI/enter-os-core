package handlers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxIncludeDepth caps !include recursion to prevent runaway chains or
// cycles that slip past the visited-set check (e.g. relative paths that
// normalize differently on different OSes). A depth of 16 easily covers
// any realistic team/role hierarchy.
const maxIncludeDepth = 16

// resolveYAMLIncludes expands `!include <path>` directives in a YAML
// document. Used by POST /org/import + GET /org/templates to support
// splitting a single large org.yaml into per-team or per-role files.
//
// Semantics:
//   - A scalar node tagged `!include` with a string value is replaced by
//     the parsed content of the referenced file.
//   - Paths resolve relative to the INCLUDING file's directory (natural
//     sibling/cousin refs, matches C-include / Sass @import convention).
//     When the including file is the top-level org.yaml, that's baseDir.
//     When it's a nested team file, that's the team file's own dir.
//   - Security: every resolved absolute path must stay inside `rootDir`
//     (the original baseDir from the top-level call). This allows natural
//     `../sibling-dir/file.yaml` refs while still blocking traversal
//     outside the org template root.
//   - Includes may be nested (a team file can !include a role file).
//     Cycles are detected via a visited set keyed on absolute path;
//     `maxIncludeDepth` caps total recursion depth as a belt-and-braces
//     check.
//   - Missing files return an error — fail loud during import, not at
//     runtime.
//
// Returns the expanded YAML as bytes (to keep the caller's existing
// `yaml.Unmarshal(data, ...)` flow unchanged).
func resolveYAMLIncludes(data []byte, baseDir string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	visited := map[string]bool{}
	// At the top-level call, the "including file's dir" and the "security
	// root" are the same. They diverge as we descend into nested includes.
	if err := expandNode(&root, baseDir, baseDir, visited, 0); err != nil {
		return nil, err
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		return nil, fmt.Errorf("marshal expanded yaml: %w", err)
	}
	return out, nil
}

// expandNode walks the yaml.Node tree in-place and replaces any
// `!include`-tagged scalar with the parsed content of the referenced
// file. `currentDir` is the dir of the file currently being processed
// (used for path resolution); `rootDir` is the original org base dir
// (used to bound the security check).
func expandNode(n *yaml.Node, currentDir, rootDir string, visited map[string]bool, depth int) error {
	if n == nil {
		return nil
	}
	if depth > maxIncludeDepth {
		return fmt.Errorf("!include: max depth %d exceeded (possible cycle)", maxIncludeDepth)
	}

	if n.Kind == yaml.ScalarNode && n.Tag == "!include" {
		return resolveIncludeScalar(n, currentDir, rootDir, visited, depth)
	}

	// `!external`-tagged mapping: gitops cross-repo subtree composition.
	// See org_external.go (internal#77 / task #222).
	if n.Kind == yaml.MappingNode && n.Tag == "!external" {
		return resolveExternalMapping(n, currentDir, rootDir, visited, depth)
	}

	for _, child := range n.Content {
		if err := expandNode(child, currentDir, rootDir, visited, depth); err != nil {
			return err
		}
	}
	return nil
}

// resolveIncludeScalar replaces an `!include <path>` scalar with the
// parsed content of the referenced file.
func resolveIncludeScalar(n *yaml.Node, currentDir, rootDir string, visited map[string]bool, depth int) error {
	rel := n.Value
	if rel == "" {
		return fmt.Errorf("!include at line %d: empty path", n.Line)
	}
	if rootDir == "" {
		return fmt.Errorf("!include %q at line %d requires a dir-based org template (no baseDir in inline-template mode)", rel, n.Line)
	}

	// Resolve relative to the including file's dir. Result must stay
	// inside the original rootDir — sibling dirs (../foo/bar.yaml) are
	// fine as long as they don't escape the template root.
	abs := filepath.Clean(filepath.Join(currentDir, rel))
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return fmt.Errorf("!include %q at line %d: cannot abs rootDir: %w", rel, n.Line, err)
	}
	absTarget, err := filepath.Abs(abs)
	if err != nil {
		return fmt.Errorf("!include %q at line %d: cannot abs target: %w", rel, n.Line, err)
	}
	// Ensure target is inside root. `filepath.Rel` returns "../..." if target
	// is outside; we reject that.
	rel2, err := filepath.Rel(absRoot, absTarget)
	if err != nil || strings.HasPrefix(rel2, "..") || rel2 == ".." {
		return fmt.Errorf("!include %q at line %d: path escapes root", rel, n.Line)
	}

	if visited[absTarget] {
		return fmt.Errorf("!include cycle detected at %q (line %d)", rel, n.Line)
	}
	data, err := os.ReadFile(absTarget)
	if err != nil {
		return fmt.Errorf("!include %q at line %d: %w", rel, n.Line, err)
	}

	var sub yaml.Node
	if err := yaml.Unmarshal(data, &sub); err != nil {
		return fmt.Errorf("!include %q: parse: %w", rel, err)
	}
	// yaml.Unmarshal of a full file yields a DocumentNode wrapping the
	// actual root. Peel one layer so the includer sees the real content.
	root := &sub
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}

	// Mark visited for the whole descent through this file, then recurse.
	// Relative refs inside the included file resolve against THAT file's
	// dir (subDir), but security stays bounded by the original rootDir.
	visited[absTarget] = true
	defer delete(visited, absTarget)

	subDir := filepath.Dir(absTarget)
	if err := expandNode(root, subDir, rootDir, visited, depth+1); err != nil {
		return err
	}

	// Replace the !include scalar with the resolved content in-place.
	*n = *root
	if n.Tag == "!include" {
		n.Tag = ""
	}
	return nil
}
