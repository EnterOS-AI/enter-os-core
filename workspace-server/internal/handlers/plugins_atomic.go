package handlers

// plugins_atomic.go — atomic install pattern for plugin delivery into a
// running workspace container. Closes molecule-core#114.
//
// Replaces the prior "tar + docker.CopyToContainer to /configs/plugins/<name>"
// single-step write (no atomicity, no marker, no rollback) with a 4-step
// dance:
//
//   1. STAGE     — extract tar into /configs/plugins/.staging/<name>.<ts>/
//   2. SNAPSHOT  — if /configs/plugins/<name>/ exists, mv to .previous/<name>.<ts>/
//   3. SWAP      — mv /configs/plugins/.staging/<name>.<ts>/ → /configs/plugins/<name>/
//   4. MARKER    — touch /configs/plugins/<name>/.complete
//
// On any post-snapshot failure we attempt a best-effort rollback by mv-ing
// the previous snapshot back into place. The .complete marker is the
// canonical "this install is fully landed" signal — workspace-side plugin
// loaders should refuse to load a plugin dir without it.
//
// Scope: docker path only (workspace running as a local container). The
// SaaS path (deliverViaEIC, SSH-into-EC2) is unchanged in this PR; tracked
// as a follow-up. The same stage-then-swap shape applies but the exec
// primitives differ (ssh vs docker exec), and shipping both paths in one
// PR doubles the test surface.

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

const (
	pluginsRoot       = "/configs/plugins"
	pluginsStagingDir = "/configs/plugins/.staging"
	pluginsPrevDir    = "/configs/plugins/.previous"
	completeMarker    = ".complete"
)

// installVersion identifies one install attempt — the plugin name plus a
// monotonic-ish UTC timestamp suffix. Used to namespace the staging dir
// and any snapshot of the previous version, so a reinstall mid-flight
// can't collide with a concurrent reinstall.
type installVersion struct {
	plugin string
	stamp  string // e.g. 20260508T141530Z
}

func newInstallVersion(plugin string) installVersion {
	return installVersion{
		plugin: plugin,
		stamp:  time.Now().UTC().Format("20060102T150405Z"),
	}
}

// stagedPath is the container path where the new content lands during fetch.
// e.g. /configs/plugins/.staging/molecule-skill-foo.20260508T141530Z
func (v installVersion) stagedPath() string {
	return path.Join(pluginsStagingDir, v.plugin+"."+v.stamp)
}

// previousPath is where the prior live version is moved before swap.
// e.g. /configs/plugins/.previous/molecule-skill-foo.20260508T141530Z
func (v installVersion) previousPath() string {
	return path.Join(pluginsPrevDir, v.plugin+"."+v.stamp)
}

// livePath is the destination after swap.
// e.g. /configs/plugins/molecule-skill-foo
func (v installVersion) livePath() string {
	return path.Join(pluginsRoot, v.plugin)
}

// markerPath is the .complete file inside the live dir written last.
func (v installVersion) markerPath() string {
	return path.Join(v.livePath(), completeMarker)
}

// atomicCopyToContainer does a stage→snapshot→swap→marker install of a
// host-side staged plugin tree into a running container's
// /configs/plugins/<name>/. Returns nil on success.
//
// On post-snapshot failure (swap or marker write), best-effort rollback
// restores the previous snapshot to the live path. Returns the original
// error wrapped — the caller should surface it; rollback success is
// logged separately.
func (h *PluginsHandler) atomicCopyToContainer(
	ctx context.Context, containerName, hostDir, pluginName string,
) error {
	v := newInstallVersion(pluginName)

	// Step 0a: ensure staging + previous root dirs exist (idempotent).
	if _, err := h.execAsRoot(ctx, containerName, []string{
		"mkdir", "-p", pluginsStagingDir, pluginsPrevDir,
	}); err != nil {
		return fmt.Errorf("atomic install: mkdir staging/previous: %w", err)
	}

	// Step 0b: tar the host content with a path prefix that lands it in the
	// staging dir — NOT directly into the live name. The prefix has no
	// leading "/" because docker.CopyToContainer extracts paths relative
	// to the dstPath argument we pass below.
	stagedRel := strings.TrimPrefix(v.stagedPath(), "/")
	tarBuf, err := tarHostDirWithPrefix(hostDir, stagedRel)
	if err != nil {
		return fmt.Errorf("atomic install: tar host dir: %w", err)
	}

	// Step 1: STAGE — extract tar into /configs/plugins/.staging/<name>.<ts>/
	if err := h.docker.CopyToContainer(ctx, containerName, "/", &tarBuf,
		container.CopyToContainerOptions{}); err != nil {
		// Best-effort: clean up any partial staging extract before returning.
		_, _ = h.execAsRoot(ctx, containerName, []string{
			"rm", "-rf", v.stagedPath(),
		})
		return fmt.Errorf("atomic install: copy to container: %w", err)
	}

	// Step 2: SNAPSHOT — if a live version exists, move it aside.
	// `test -d` exits 0 if the dir exists, non-zero otherwise; the helper
	// returns a non-nil error in the non-zero case which we treat as
	// "no previous version" rather than a real failure.
	snapshotted := false
	if _, err := h.execAsRoot(ctx, containerName, []string{
		"test", "-d", v.livePath(),
	}); err == nil {
		if _, err := h.execAsRoot(ctx, containerName, []string{
			"mv", v.livePath(), v.previousPath(),
		}); err != nil {
			// Snapshot failure: roll back the staged extract before failing.
			_, _ = h.execAsRoot(ctx, containerName, []string{
				"rm", "-rf", v.stagedPath(),
			})
			return fmt.Errorf("atomic install: snapshot previous version: %w", err)
		}
		snapshotted = true
	}

	// Step 3: SWAP — atomic rename of the staged dir into the live name.
	// `mv` on the same filesystem is a single rename(2), atomic at the FS level.
	if _, err := h.execAsRoot(ctx, containerName, []string{
		"mv", v.stagedPath(), v.livePath(),
	}); err != nil {
		// Swap failure: roll back if we had a snapshot.
		if snapshotted {
			if _, rbErr := h.execAsRoot(ctx, containerName, []string{
				"mv", v.previousPath(), v.livePath(),
			}); rbErr != nil {
				return fmt.Errorf("atomic install: swap failed AND rollback failed: swap=%w, rollback=%v", err, rbErr)
			}
		}
		// Best-effort cleanup of the still-staged dir.
		_, _ = h.execAsRoot(ctx, containerName, []string{
			"rm", "-rf", v.stagedPath(),
		})
		return fmt.Errorf("atomic install: swap to live path: %w", err)
	}

	// Step 4: MARKER — touch .complete inside the live dir as the last write.
	// Workspace-side plugin loaders treat a plugin dir without this marker
	// as half-installed and skip it (or surface a clear error to the
	// operator instead of loading a possibly-partial tree).
	if _, err := h.execAsRoot(ctx, containerName, []string{
		"touch", v.markerPath(),
	}); err != nil {
		// Marker write failure with the new content already in place is a
		// weird state — content is fine on disk, but the plugin loader
		// will refuse to use it. Log loudly; do NOT roll back, since the
		// content is the latest, just unmarked. Operator can manually
		// `touch <plugin>/.complete` to recover.
		return fmt.Errorf("atomic install: write .complete marker (content landed but unmarked, manual recovery: touch %s): %w", v.markerPath(), err)
	}

	// Step 5: GC — best-effort delete the previous snapshot. Failures here
	// just leave a directory; not load-bearing for correctness, the next
	// install or a separate sweeper will reclaim the space.
	if snapshotted {
		_, _ = h.execAsRoot(ctx, containerName, []string{
			"rm", "-rf", v.previousPath(),
		})
	}

	return nil
}

// tarHostDirWithPrefix walks hostDir and writes a tar to a buffer with
// every entry's name prefixed by `prefix`. Mirrors the prior streaming
// shape used in copyPluginToContainer but with a configurable prefix
// (the prior version hardcoded "plugins/<name>/"; we use a full
// staging path so the extracted layout is the staging dir directly).
//
// Symlinks are skipped — same posture as streamDirAsTar elsewhere in
// this file. Skipping prevents a hostile plugin from injecting a
// symlink that, post-extract, points outside the plugin's own dir.
func tarHostDirWithPrefix(hostDir, prefix string) (bytes.Buffer, error) {
	var buf bytes.Buffer
	tw := newTarWriter(&buf)
	defer tw.Close()
	if err := tarWalk(hostDir, prefix, tw); err != nil {
		return bytes.Buffer{}, err
	}
	return buf, nil
}
