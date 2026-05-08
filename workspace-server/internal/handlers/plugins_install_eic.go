package handlers

// plugins_install_eic.go — SaaS (EC2-per-workspace) plugin install + uninstall
// over the EIC SSH primitive that template_files_eic.go already plumbs. Pairs
// with the local-Docker path in plugins_install.go / plugins_install_pipeline.go,
// closing the 🔴 docker-only row in docs/architecture/backends.md.
//
// Architecture note: every operation goes through `withEICTunnel` (ephemeral
// keypair → AWS push → tunnel → ssh). This file owns the plugin-shaped
// remote commands; the tunnel mechanics live in template_files_eic.go so a
// fix to the dance lands in one place.
//
// Why direct host write (not docker cp via SSH): on the workspace EC2, the
// runtime's managed-config dir (/configs for claude-code, /home/ubuntu/.hermes
// for hermes — see workspaceFilePathPrefix) is bind-mounted into the
// runtime's container by cloud-init. Writing into <prefix>/plugins/<name>/
// on the host is exactly what the runtime sees on the next start. No
// docker-cp needed, and we avoid coupling to any specific container layout
// inside the workspace EC2.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// eicPluginOpTimeout bounds the whole EIC-tunnel + ssh + tar-pipe dance
// for a plugin install or uninstall. Larger than eicFileOpTimeout (30s)
// because plugin trees can carry skill markdown, MCP server binaries,
// and config files — easily a few MB through ssh + sudo on a fresh
// tunnel. 2 min gives headroom on a cold tunnel; the install pipeline's
// PLUGIN_INSTALL_FETCH_TIMEOUT (5 min default) still bounds the outer
// request.
const eicPluginOpTimeout = 2 * time.Minute

// hostPluginPath returns the absolute directory on the workspace EC2
// where /configs/plugins/<name>/ lives for a given runtime. Keeps the
// per-runtime indirection in one place (mirrors resolveWorkspaceRootPath
// in template_files_eic.go) so future runtimes only edit
// workspaceFilePathPrefix.
//
// The plugin name is shellQuote-wrapped at the call site, not here,
// because a couple of callers want the unquoted form for log lines.
func hostPluginPath(runtime, pluginName string) string {
	base := resolveWorkspaceRootPath(runtime, "/configs")
	return filepath.Join(base, "plugins", pluginName)
}

// buildPluginInstallShell returns the remote command for receiving a tar.gz
// stream on stdin and unpacking it into <hostPluginDir>/, owned by the agent
// user (uid 1000 — matches the local-Docker path's chown 1000:1000).
//
// The script is a single `sudo sh -c '...'` so the tar-receive + chown run
// under one privileged invocation; ssh-as-ubuntu has passwordless sudo on
// the standard tenant AMI.
//
//   - rm -rf clears any prior install of the same plugin (idempotent
//     reinstall — the user re-clicked Install or version-bumped the source).
//   - mkdir -p makes the parent dir (host /configs is root-owned + always
//     present; the per-plugin dir is what we're creating).
//   - tar -xzf - reads stdin (the gzipped tar). --no-same-owner keeps the
//     archive's tar-recorded uid/gid out of the picture; the chown -R
//     after is the canonical owner.
//   - chown -R 1000:1000 matches the local-Docker handler's exec at
//     plugins_install_pipeline.go:273 — agent user inside the runtime
//     container is uid 1000 on every workspace-template image we ship.
//
// shellQuote on the path is defence-in-depth: the path is composed from
// a runtime allowlist (workspaceFilePathPrefix) + validated plugin name,
// so traversal is already blocked.
func buildPluginInstallShell(hostPluginDir string) string {
	q := shellQuote(hostPluginDir)
	return fmt.Sprintf(
		"sudo -n sh -c 'rm -rf %s && mkdir -p %s && tar -xzf - --no-same-owner -C %s && chown -R 1000:1000 %s'",
		q, q, q, q,
	)
}

// buildPluginUninstallShell returns the remote command for `sudo -n rm -rf
// <hostPluginDir>`. -rf (vs -f) is intentional here, unlike buildRmShell:
// uninstall really does need to remove the plugin's whole subtree.
func buildPluginUninstallShell(hostPluginDir string) string {
	return fmt.Sprintf("sudo -n rm -rf %s", shellQuote(hostPluginDir))
}

// buildPluginManifestReadShell returns the remote command for reading the
// plugin's manifest (plugin.yaml). Mirrors buildCatShell — swallows the
// missing-file stderr so the missing-manifest case lands as empty stdout
// + non-zero exit, which uninstall translates to "no skills to clean".
func buildPluginManifestReadShell(hostPluginDir string) string {
	return fmt.Sprintf("sudo -n cat %s/plugin.yaml 2>/dev/null", shellQuote(hostPluginDir))
}

// installPluginViaEIC pushes a staged plugin directory to a SaaS workspace
// EC2 via the EIC SSH tunnel. On success the plugin lives at
// <runtime-config-prefix>/plugins/<name>/ on the host, owned by 1000:1000,
// ready for the next workspace restart to pick up.
//
// The caller (deliverToContainer SaaS branch) owns:
//   - the staged dir (created + cleaned up by resolveAndStage)
//   - the workspace restart trigger after install
//
// Errors here are wrapped with the instance + runtime so triage can tell
// "tunnel failed" from "tar payload corrupt" without grep-ing the EC2's
// auth.log.
var installPluginViaEIC = realInstallPluginViaEIC

func realInstallPluginViaEIC(ctx context.Context, instanceID, runtime, pluginName, stagedDir string) error {
	if instanceID == "" {
		return fmt.Errorf("installPluginViaEIC: empty instance_id")
	}
	if err := validatePluginName(pluginName); err != nil {
		return fmt.Errorf("installPluginViaEIC: %w", err)
	}

	// Build the tar.gz payload up-front so a tar-walk failure is surfaced
	// before we open the EIC tunnel — saves a 1-2s tunnel setup on every
	// "broken plugin tree" case.
	var payload bytes.Buffer
	gz := gzip.NewWriter(&payload)
	tw := tar.NewWriter(gz)
	if err := streamDirAsTar(stagedDir, tw); err != nil {
		return fmt.Errorf("installPluginViaEIC: tar pack: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("installPluginViaEIC: tar close: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("installPluginViaEIC: gzip close: %w", err)
	}

	hostDir := hostPluginPath(runtime, pluginName)
	cmd := buildPluginInstallShell(hostDir)

	ctx, cancel := context.WithTimeout(ctx, eicPluginOpTimeout)
	defer cancel()

	return withEICTunnel(ctx, instanceID, func(s eicSSHSession) error {
		sshCmd := exec.CommandContext(ctx, "ssh", s.sshArgs(cmd)...)
		sshCmd.Env = os.Environ()
		sshCmd.Stdin = bytes.NewReader(payload.Bytes())
		var stderr bytes.Buffer
		sshCmd.Stderr = &stderr
		if err := sshCmd.Run(); err != nil {
			return fmt.Errorf(
				"ssh install: %w (instance=%s runtime=%s plugin=%s payload=%dB stderr=%s)",
				err, instanceID, runtime, pluginName, payload.Len(),
				strings.TrimSpace(stderr.String()),
			)
		}
		log.Printf(
			"installPluginViaEIC: ws instance=%s runtime=%s plugin=%s payload=%dB → %s",
			instanceID, runtime, pluginName, payload.Len(), hostDir,
		)
		return nil
	})
}

// uninstallPluginViaEIC removes the plugin's directory from the workspace
// EC2 via SSH. Symmetric with installPluginViaEIC but no payload — the
// remote command is a single `rm -rf`.
//
// Best-effort by design: the local-Docker path also doesn't fail
// uninstall on a missing directory (the pre-existing exec returns 0 when
// the dir is absent), so we mirror that here. Real ssh-layer failures
// (tunnel down, sudo denied) still propagate.
var uninstallPluginViaEIC = realUninstallPluginViaEIC

func realUninstallPluginViaEIC(ctx context.Context, instanceID, runtime, pluginName string) error {
	if instanceID == "" {
		return fmt.Errorf("uninstallPluginViaEIC: empty instance_id")
	}
	if err := validatePluginName(pluginName); err != nil {
		return fmt.Errorf("uninstallPluginViaEIC: %w", err)
	}

	hostDir := hostPluginPath(runtime, pluginName)
	cmd := buildPluginUninstallShell(hostDir)

	ctx, cancel := context.WithTimeout(ctx, eicPluginOpTimeout)
	defer cancel()

	return withEICTunnel(ctx, instanceID, func(s eicSSHSession) error {
		sshCmd := exec.CommandContext(ctx, "ssh", s.sshArgs(cmd)...)
		sshCmd.Env = os.Environ()
		var stderr bytes.Buffer
		sshCmd.Stderr = &stderr
		if err := sshCmd.Run(); err != nil {
			return fmt.Errorf(
				"ssh rm: %w (instance=%s runtime=%s plugin=%s stderr=%s)",
				err, instanceID, runtime, pluginName,
				strings.TrimSpace(stderr.String()),
			)
		}
		log.Printf(
			"uninstallPluginViaEIC: ws instance=%s runtime=%s plugin=%s → removed %s",
			instanceID, runtime, pluginName, hostDir,
		)
		return nil
	})
}

// readPluginManifestViaEIC reads the plugin's plugin.yaml from the
// workspace EC2 so uninstall can learn the skills list to clean up.
// Returns ("", nil) when the manifest doesn't exist (best-effort: the
// local-Docker path treats a missing manifest as "no skills to remove",
// not a failure).
var readPluginManifestViaEIC = realReadPluginManifestViaEIC

func realReadPluginManifestViaEIC(ctx context.Context, instanceID, runtime, pluginName string) ([]byte, error) {
	if instanceID == "" {
		return nil, fmt.Errorf("readPluginManifestViaEIC: empty instance_id")
	}
	if err := validatePluginName(pluginName); err != nil {
		return nil, fmt.Errorf("readPluginManifestViaEIC: %w", err)
	}

	hostDir := hostPluginPath(runtime, pluginName)
	cmd := buildPluginManifestReadShell(hostDir)

	ctx, cancel := context.WithTimeout(ctx, eicPluginOpTimeout)
	defer cancel()

	var out []byte
	runErr := withEICTunnel(ctx, instanceID, func(s eicSSHSession) error {
		sshCmd := exec.CommandContext(ctx, "ssh", s.sshArgs(cmd)...)
		sshCmd.Env = os.Environ()
		var stdout, stderr bytes.Buffer
		sshCmd.Stdout = &stdout
		sshCmd.Stderr = &stderr
		// Don't fail on non-zero exit: missing-manifest case returns 1
		// from cat with empty stdout, which is the "no skills" signal.
		_ = sshCmd.Run()
		out = stdout.Bytes()
		return nil
	})
	if runErr != nil {
		return nil, runErr
	}
	return out, nil
}
