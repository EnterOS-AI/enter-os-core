package handlers

// template_files_eic.go — SSH-backed file write for SaaS workspaces
// (EC2-per-workspace). Pairs with the existing Docker-path in templates.go
// (WriteFile) and template_import.go (ReplaceFiles).
//
// Flow for a single file write:
//  1. Generate ephemeral ed25519 keypair (on-disk for ≤ write duration).
//  2. Push the public key via `aws ec2-instance-connect send-ssh-public-key`
//     so the target sshd accepts it for the next 60s.
//  3. Open a TLS-tunnelled TCP port via `aws ec2-instance-connect open-tunnel`
//     from a local free port → workspace's sshd on 22.
//  4. Pipe content to `ssh ... "install -D -m 0644 /dev/stdin <abs path>"`.
//     `install -D` creates any missing parent dirs atomically. File is owned
//     by whichever $OSUser we authenticated as (ubuntu by default).
//  5. Close tunnel + wipe keydir.
//
// All the AWS calls + ssh tunnel exec go through the same package-level
// func vars defined in terminal.go (openTunnelCmd, sendSSHPublicKey) so
// tests can stub them the same way the terminal tests do.

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// workspaceFilePathPrefix maps a runtime name to the absolute base path on
// the workspace EC2 where the Files API's relative paths land. New runtimes
// can be added here without touching handler code.
//
// Keep these stable — changing the base path for an existing runtime
// without a migration shim will make previously-saved files disappear from
// the runtime's POV.
//
// Path source-of-truth: cloud-init in
// `molecule-controlplane/internal/provisioner/userdata_containerized.go`
// runs `mkdir -p /configs` and writes the canonical config.yaml there.
// The workspace container bind-mounts host `/configs` to read it back.
// Files written anywhere else on the host are invisible to the runtime,
// so `claude-code` (and any future containerized runtime) must point here.
//
// `/configs` is root-owned (cloud-init runs as root); the SSH-as-ubuntu
// install command at the call site below uses `sudo` to write into it.
var workspaceFilePathPrefix = map[string]string{
	"hermes":      "/home/ubuntu/.hermes",
	"claude-code": "/configs",
	"langgraph":   "/opt/configs",
	"external":    "/opt/configs",
	// Default for unknown / future runtimes is /configs — matches the
	// containerized user-data layout. The `langgraph` / `external`
	// entries pre-date the unified user-data path and are retained
	// until a migration audit confirms what the running tenants of
	// those runtimes actually have on disk.
}

func resolveWorkspaceFilePath(runtime, relPath string) (string, error) {
	if err := validateRelPath(relPath); err != nil {
		return "", err
	}
	base, ok := workspaceFilePathPrefix[strings.ToLower(strings.TrimSpace(runtime))]
	if !ok {
		base = "/configs"
	}
	return filepath.Join(base, filepath.Clean(relPath)), nil
}

// eicFileWriteTimeout bounds the whole dance. Key push is <500ms, tunnel
// is 1-2s, ssh + write is <2s. 30s gives headroom for slow pulls without
// hanging the Files API forever under EIC misconfiguration.
const eicFileWriteTimeout = 30 * time.Second

// writeFileViaEIC writes a single file to the workspace EC2 at the
// absolute path that resolveWorkspaceFilePath computed. On success,
// optionally invokes the runtime's reload hook (not implemented yet —
// tracked as follow-up; for today the canvas issues a separate Restart
// after Save).
//
// instanceID: AWS EC2 instance id from workspaces.instance_id.
// runtime: used only for path-prefix resolution.
// relPath: the relative path the caller validated (no /, no ..).
// content: file body bytes.
func writeFileViaEIC(ctx context.Context, instanceID, runtime, relPath string, content []byte) error {
	if instanceID == "" {
		return fmt.Errorf("workspace has no instance_id — not a SaaS EC2 workspace")
	}
	absPath, err := resolveWorkspaceFilePath(runtime, relPath)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}

	osUser := os.Getenv("WORKSPACE_EC2_OS_USER")
	if osUser == "" {
		osUser = "ubuntu"
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-2"
	}

	ctx, cancel := context.WithTimeout(ctx, eicFileWriteTimeout)
	defer cancel()

	// Ephemeral keypair.
	keyDir, err := os.MkdirTemp("", "molecule-filewrite-*")
	if err != nil {
		return fmt.Errorf("keydir mkdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(keyDir) }()
	keyPath := keyDir + "/id"
	if out, kerr := exec.CommandContext(ctx, "ssh-keygen",
		"-t", "ed25519", "-f", keyPath, "-N", "", "-q",
		"-C", "molecule-filewrite",
	).CombinedOutput(); kerr != nil {
		return fmt.Errorf("ssh-keygen: %w (%s)", kerr, strings.TrimSpace(string(out)))
	}
	pubKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return fmt.Errorf("read pubkey: %w", err)
	}

	// 1. Push key.
	if err := sendSSHPublicKey(ctx, region, instanceID, osUser, strings.TrimSpace(string(pubKey))); err != nil {
		return fmt.Errorf("send-ssh-public-key: %w", err)
	}

	// 2. Open tunnel on an OS-picked free port.
	localPort, err := pickFreePort()
	if err != nil {
		return fmt.Errorf("pick free port: %w", err)
	}
	opts := eicSSHOptions{
		InstanceID:     instanceID,
		OSUser:         osUser,
		Region:         region,
		LocalPort:      localPort,
		PrivateKeyPath: keyPath,
	}
	tunnel := openTunnelCmd(opts)
	tunnel.Env = os.Environ()
	if err := tunnel.Start(); err != nil {
		return fmt.Errorf("open-tunnel start: %w", err)
	}
	defer func() {
		if tunnel.Process != nil {
			_ = tunnel.Process.Kill()
		}
		_ = tunnel.Wait()
	}()
	if err := waitForPort(ctx, "127.0.0.1", localPort, 10*time.Second); err != nil {
		return fmt.Errorf("tunnel never listened: %w", err)
	}

	// 3. SSH + install -D. `install` creates any missing parent dirs and
	// writes the file atomically via temp-file-rename. Permissions 0644
	// match the existing tar-unpack defaults on the Docker path.
	//
	// `sudo -n` (non-interactive) prefix: the canonical containerized
	// workspace layout puts /configs at the root, owned by root because
	// cloud-init runs as root (see
	// molecule-controlplane/internal/provisioner/userdata_containerized.go).
	// SSH-as-ubuntu can't write into /configs without escalation.
	// Ubuntu has passwordless sudo on EC2 by default; sudo -n fails fast
	// (no prompt) if that ever changes, surfacing a clean error instead
	// of a hang. The hermes path /home/ubuntu/.hermes is ubuntu-owned
	// and doesn't strictly need sudo, but using it uniformly avoids
	// per-runtime branching here.
	//
	// The remote command is fully deterministic — no user-controlled
	// input reaches a shell eval (absPath is built from a map + Clean()).
	sshArgs := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		// LogLevel=ERROR silences the benign "Warning: Permanently
		// added '[127.0.0.1]:NNNNN' to known hosts" notice that ssh
		// emits on every fresh tunnel connection. Without this, the
		// notice lands on stderr and fools readFileViaEIC's "empty
		// stdout + empty stderr → file not found" classifier into
		// thinking the warning is a real ssh-layer error → 500
		// instead of 404 (Hermes config.yaml load, hongming tenant,
		// 2026-05-05 02:38). Real auth/tunnel errors stay visible
		// because they're emitted at ERROR level.
		"-o", "LogLevel=ERROR",
		"-o", "ServerAliveInterval=15",
		"-p", fmt.Sprintf("%d", localPort),
		fmt.Sprintf("%s@127.0.0.1", osUser),
		fmt.Sprintf("sudo -n install -D -m 0644 /dev/stdin %s", shellQuote(absPath)),
	}
	sshCmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	sshCmd.Env = os.Environ()
	sshCmd.Stdin = bytes.NewReader(content)
	var stderr bytes.Buffer
	sshCmd.Stderr = &stderr
	if err := sshCmd.Run(); err != nil {
		return fmt.Errorf("ssh install: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	log.Printf("writeFileViaEIC: ws instance=%s runtime=%s wrote %d bytes → %s",
		instanceID, runtime, len(content), absPath)
	return nil
}

// shellQuote wraps a value in single quotes + escapes embedded single
// quotes for POSIX sh. Used for the sole piece of variable data in the
// remote ssh command. (absPath is already built from a map + Clean() so
// traversal is blocked regardless; this is defence-in-depth against
// future refactor that might accept user paths here.)
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readFileViaEIC reads a single file from the workspace EC2 at the
// absolute path that resolveWorkspaceFilePath computes. Mirrors
// writeFileViaEIC end-to-end (ephemeral keypair, EIC tunnel, ssh) so
// canvas's Config tab can GET back what it just PUT. Pre-fix the GET
// path (templates.go ReadFile) only handled local Docker containers
// + a host-side template fallback; SaaS workspaces (EC2-per-workspace)
// always 404'd because neither handles their on-EC2 layout.
//
// Returns ("", os.ErrNotExist) when the remote path doesn't exist so
// the handler can map it to HTTP 404 cleanly. Other errors propagate.
func readFileViaEIC(ctx context.Context, instanceID, runtime, relPath string) ([]byte, error) {
	if instanceID == "" {
		return nil, fmt.Errorf("workspace has no instance_id — not a SaaS EC2 workspace")
	}
	absPath, err := resolveWorkspaceFilePath(runtime, relPath)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	osUser := os.Getenv("WORKSPACE_EC2_OS_USER")
	if osUser == "" {
		osUser = "ubuntu"
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-2"
	}

	ctx, cancel := context.WithTimeout(ctx, eicFileWriteTimeout)
	defer cancel()

	keyDir, err := os.MkdirTemp("", "molecule-fileread-*")
	if err != nil {
		return nil, fmt.Errorf("keydir mkdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(keyDir) }()
	keyPath := keyDir + "/id"
	if out, kerr := exec.CommandContext(ctx, "ssh-keygen",
		"-t", "ed25519", "-f", keyPath, "-N", "", "-q",
		"-C", "molecule-fileread",
	).CombinedOutput(); kerr != nil {
		return nil, fmt.Errorf("ssh-keygen: %w (%s)", kerr, strings.TrimSpace(string(out)))
	}
	pubKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return nil, fmt.Errorf("read pubkey: %w", err)
	}

	if err := sendSSHPublicKey(ctx, region, instanceID, osUser, strings.TrimSpace(string(pubKey))); err != nil {
		return nil, fmt.Errorf("send-ssh-public-key: %w", err)
	}

	localPort, err := pickFreePort()
	if err != nil {
		return nil, fmt.Errorf("pick free port: %w", err)
	}
	tunnel := openTunnelCmd(eicSSHOptions{
		InstanceID:     instanceID,
		OSUser:         osUser,
		Region:         region,
		LocalPort:      localPort,
		PrivateKeyPath: keyPath,
	})
	tunnel.Env = os.Environ()
	if err := tunnel.Start(); err != nil {
		return nil, fmt.Errorf("open-tunnel start: %w", err)
	}
	defer func() {
		if tunnel.Process != nil {
			_ = tunnel.Process.Kill()
		}
		_ = tunnel.Wait()
	}()
	if err := waitForPort(ctx, "127.0.0.1", localPort, 10*time.Second); err != nil {
		return nil, fmt.Errorf("tunnel never listened: %w", err)
	}

	// `sudo -n cat`: /configs is root-owned by cloud-init (same reason
	// writeFileViaEIC needs sudo to install). The path is built from a
	// validated map + Clean(), so no user-controlled string reaches the
	// shell here. `2>/dev/null` swallows `cat: ...: No such file` so
	// the missing-file case returns empty stdout + non-zero exit, which
	// we translate to os.ErrNotExist below.
	sshCmd := exec.CommandContext(ctx, "ssh",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		// LogLevel=ERROR silences the benign "Warning: Permanently
		// added '[127.0.0.1]:NNNNN' to known hosts" notice that ssh
		// emits on every fresh tunnel connection. Without this, the
		// notice lands on stderr and fools readFileViaEIC's "empty
		// stdout + empty stderr → file not found" classifier into
		// thinking the warning is a real ssh-layer error → 500
		// instead of 404 (Hermes config.yaml load, hongming tenant,
		// 2026-05-05 02:38). Real auth/tunnel errors stay visible
		// because they're emitted at ERROR level.
		"-o", "LogLevel=ERROR",
		"-o", "ServerAliveInterval=15",
		"-p", fmt.Sprintf("%d", localPort),
		fmt.Sprintf("%s@127.0.0.1", osUser),
		fmt.Sprintf("sudo -n cat %s 2>/dev/null", shellQuote(absPath)),
	)
	sshCmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	sshCmd.Stdout = &stdout
	sshCmd.Stderr = &stderr
	runErr := sshCmd.Run()
	out := stdout.Bytes()
	if runErr != nil {
		// `cat` returns 1 on missing file; with 2>/dev/null we have no
		// stderr distinguisher. Treat empty-stdout + non-zero exit as
		// not-found rather than a tunnel/auth error (those usually
		// produce stderr from ssh itself, not from the remote command).
		if len(out) == 0 && stderr.Len() == 0 {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("ssh cat: %w (%s)", runErr, strings.TrimSpace(stderr.String()))
	}
	log.Printf("readFileViaEIC: ws instance=%s runtime=%s read %d bytes ← %s",
		instanceID, runtime, len(out), absPath)
	return out, nil
}
