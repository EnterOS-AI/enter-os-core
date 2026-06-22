package plugins

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// LocalResolver fetches plugins from a filesystem directory shipped with
// the platform (the canonical /plugins registry). This is the default
// source for bare names in the install API; most deployments point it
// at the repo's `plugins/` directory.
type LocalResolver struct {
	// BaseDir is the absolute path to the directory that contains one
	// subdirectory per available plugin (e.g. repo-root/plugins).
	BaseDir string
}

// NewLocalResolver constructs a LocalResolver pointing at baseDir.
func NewLocalResolver(baseDir string) *LocalResolver {
	return &LocalResolver{BaseDir: baseDir}
}

// Scheme returns "local".
func (r *LocalResolver) Scheme() string { return "local" }

// localNameRE constrains plugin names to safe identifiers. Matches
// validatePluginName in the handlers package; duplicated here so the
// plugins package has no reverse dependency.
//
// Length-bounded at 128 chars (1 + 127 tail). agentskills.io caps
// skill names at 64; our plugin-level names are a superset (collection
// of skills) so we allow a bit more headroom, but not unbounded.
var localNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// Fetch copies the plugin directory from BaseDir/<spec> into dst.
//
// `spec` is the plain plugin name (e.g. "molecule-dev"). Path-traversal
// attempts (slashes, "..", empty) are rejected.
func (r *LocalResolver) Fetch(ctx context.Context, spec string, dst string) (string, error) {
	name := strings.TrimSpace(spec)
	if name == "" {
		return "", fmt.Errorf("local resolver: empty plugin name")
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", fmt.Errorf("local resolver: invalid plugin name %q", name)
	}
	if !localNameRE.MatchString(name) {
		return "", fmt.Errorf("local resolver: plugin name %q must match %s", name, localNameRE)
	}

	src := filepath.Join(r.BaseDir, name)
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("local resolver: plugin %q: %w", name, ErrPluginNotFound)
		}
		return "", fmt.Errorf("local resolver: stat %s: %w", src, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("local resolver: %q is not a directory", src)
	}

	// Copy the directory tree into dst (which the caller has created).
	if err := copyTree(ctx, src, dst); err != nil {
		return "", fmt.Errorf("local resolver: copy failed: %w", err)
	}

	return name, nil
}

// copyTree does a recursive copy honouring ctx cancellation. Avoids a
// dependency on os/exec (no need to shell out to cp).
//
// Symlinks are SKIPPED, not followed. filepath.Walk reports a symlink as a
// non-directory entry, and copyFile uses os.Open (which follows symlinks) —
// so a committed symlink in the source tree (e.g. `leak.txt -> /etc/passwd`
// or `-> ../../SECRET`) would otherwise copy the TARGET's content into the
// staged plugin dir, escaping the source subtree. We Lstat each entry and
// skip symlinks with a warning rather than hard-failing: a template author's
// stray symlink shouldn't block an otherwise-valid install, and a skipped
// link simply means the (untrusted) link target is never read. The Walk-
// supplied info is from os.Lstat already, so we can use it directly.
func copyTree(ctx context.Context, src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// filepath.Walk lstats entries, so info already reflects the link
		// itself (not its target). Skip any symlink so we never read through
		// it into content outside the source subtree.
		if info.Mode()&os.ModeSymlink != 0 {
			log.Printf("plugins: copyTree skipping symlink %q (symlinks are not followed for safety)", path)
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode()&os.ModePerm)
		}
		return copyFile(path, target, info.Mode()&os.ModePerm)
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
