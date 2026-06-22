package handlers

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// maxExecOutput limits container exec output to 5MB to prevent OOM.
const maxExecOutput = 5 * 1024 * 1024

// findContainer finds a running container for the workspace.
// Checks provisioner name, full ID, and DB workspace name (same candidates as terminal handler).
func (h *TemplatesHandler) findContainer(ctx context.Context, workspaceID string) string {
	if h.docker == nil {
		return ""
	}
	name := provisioner.ContainerName(workspaceID)
	candidates := []string{name}
	if name != "ws-"+workspaceID {
		candidates = append(candidates, "ws-"+workspaceID)
	}
	// Also check by workspace name from DB
	var wsName string
	db.DB.QueryRowContext(ctx, `SELECT LOWER(REPLACE(name, ' ', '-')) FROM workspaces WHERE id = $1`, workspaceID).Scan(&wsName)
	if wsName != "" {
		candidates = append(candidates, wsName)
	}
	for _, c := range candidates {
		info, err := h.docker.ContainerInspect(ctx, c)
		if err == nil && info.State.Running {
			return c
		}
	}
	return ""
}

// execInContainer runs a command in a container and returns stdout (capped at maxExecOutput).
func (h *TemplatesHandler) execInContainer(ctx context.Context, containerName string, cmd []string) (string, error) {
	execCfg := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}
	execID, err := h.docker.ContainerExecCreate(ctx, containerName, execCfg)
	if err != nil {
		return "", err
	}
	resp, err := h.docker.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer resp.Close()
	var stdout bytes.Buffer
	// Use stdcopy to correctly demux Docker multiplexed stream (stdout/stderr)
	stdcopy.StdCopy(&stdout, io.Discard, io.LimitReader(resp.Reader, maxExecOutput))
	return strings.TrimSpace(stdout.String()), nil
}

// copyFilesToContainer creates a tar archive from a map of files and copies it into a container.
// The destPath is prepended to each file name. File names must be relative and must not escape
// destPath via ".." segments — otherwise the tar header name could escape the mounted volume.
func (h *TemplatesHandler) copyFilesToContainer(ctx context.Context, containerName, destPath string, files map[string]string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	createdDirs := map[string]bool{}
	for name, content := range files {
		// Block absolute paths and traversal attempts at the archive-write boundary.
		// Files are written inside destPath (typically /configs); anything that escapes
		// via ".." or an absolute name could reach other volumes or system paths.
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return fmt.Errorf("unsafe file path in archive: %s", name)
		}
		// Prepend destPath so relative paths land inside the volume mount.
		// Use cleaned name so validation (which checks clean) and usage stay consistent.
		archiveName := filepath.Join(destPath, clean)
		// Defence-in-depth: ensure the joined path doesn't escape destPath.
		// This guards against platform-specific filepath.Join behaviour where
		// joining a relative name containing ".." with a destPath can still
		// produce an absolute path outside the intended directory.
		if !strings.HasPrefix(archiveName, destPath) && archiveName != destPath {
			return fmt.Errorf("path escapes destination: %s", name)
		}

		// Create parent directories in tar (deduplicated)
		dir := filepath.Dir(archiveName)
		if dir != destPath && !createdDirs[dir] {
			tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     dir + "/",
				Mode:     0755,
			})
			createdDirs[dir] = true
		}

		data := []byte(content)
		header := &tar.Header{
			Name: archiveName,
			Mode: 0644,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header for %s: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return fmt.Errorf("failed to write tar data for %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}

	return h.docker.CopyToContainer(ctx, containerName, destPath, &buf, container.CopyToContainerOptions{})
}

// writeViaEphemeral writes files to a named volume using an ephemeral Alpine container.
// Used when the workspace container is offline (e.g., during provisioning).
func (h *TemplatesHandler) writeViaEphemeral(ctx context.Context, volumeName string, files map[string]string) error {
	if h.docker == nil {
		return fmt.Errorf("docker not available")
	}

	// Create ephemeral container mounting the volume
	resp, err := h.docker.ContainerCreate(ctx, &container.Config{
		Image: "alpine:latest",
		Cmd:   []string{"sleep", "10"},
	}, &container.HostConfig{
		Binds: []string{volumeName + ":/configs"},
	}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create ephemeral container: %w", err)
	}
	defer h.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})

	if err := h.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start ephemeral container: %w", err)
	}

	// Copy files via tar, then stop container cleanly
	if err := h.copyFilesToContainer(ctx, resp.ID, "/configs", files); err != nil {
		return err
	}
	// Wait for container to be ready for removal (copy is synchronous, but be safe)
	timeout := 5
	h.docker.ContainerStop(ctx, resp.ID, container.StopOptions{Timeout: &timeout})
	return nil
}

// deleteViaEphemeral deletes a file from a named volume using an ephemeral container.
func (h *TemplatesHandler) deleteViaEphemeral(ctx context.Context, volumeName, filePath string) error {
	// CWE-78/CWE-22: exec form binds rm to the /configs volume regardless
	// of path traversal in filePath. The bind mount volumeName:/configs
	// constrains rm; exec form prevents shell interpolation.
	// validateRelPath is defense-in-depth (blocks ".." in raw input).
	// The concat form is the critical fix: rm receives ONE path argument
	// so ".." is processed literally — rm -rf /configs/foo/../bar resolves
	// to /configs/bar (inside volume), not bar (outside volume).
	//
	// Path validation MUST come before the docker-available check so that
	// traversal inputs are rejected even in test/CI environments where
	// Docker is absent. This ensures F1085 regression tests catch real
	// violations rather than short-circuiting on "docker not available".
	if err := validateRelPath(filePath); err != nil {
		return err
	}
	if h.docker == nil {
		return fmt.Errorf("docker not available")
	}

	resp, err := h.docker.ContainerCreate(ctx, &container.Config{
		Image: "alpine:latest",
		Cmd:   []string{"rm", "-rf", "/configs/" + filePath},
	}, &container.HostConfig{
		Binds: []string{volumeName + ":/configs"},
	}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create ephemeral container: %w", err)
	}
	defer h.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})

	if err := h.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return err
	}
	// Wait for the rm command to finish before removing the container
	statusCh, errCh := h.docker.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case <-statusCh:
		return nil
	case err := <-errCh:
		return err
	}
}
