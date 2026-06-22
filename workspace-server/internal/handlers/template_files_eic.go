package handlers

// template_files_eic.go — SSH-backed file operations for SaaS workspaces
// (EC2-per-workspace). Pairs with the local-Docker path in templates.go
// (List/Read/Write/Delete) and template_import.go (ReplaceFiles).
//
// Architecture note: every operation goes through `withEICTunnel`, which
// owns the ephemeral-keypair → key-push → tunnel → port-wait dance. Per-
// op helpers (list/read/write/delete) only carry the remote command +
// stdin/stdout shape. This keeps the EIC connection logic in one place
// so a fix to the dance — e.g. PR #2822's `LogLevel=ERROR` shim — only
// touches one helper.
//
// Path translation rules: see resolveWorkspaceFilePath. `/configs`
// is the per-runtime managed-config indirection (claude-code → /configs,
// hermes → /home/ubuntu/.hermes); other allow-listed roots (`/home`,
// `/workspace`, `/plugins`) pass through literally.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// workspaceFilePathPrefix maps a runtime name to the absolute base path on
// the workspace EC2 where the runtime's managed-config dir lives.
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
	"external":    "/opt/configs",
	// Default for unknown / future runtimes is /configs — matches the
	// containerized user-data layout. The `external` entry pre-dates the
	// unified user-data path and does not map to a spawned runtime.
}

// resolveWorkspaceFilePath translates (runtime, root, relPath) into an
// absolute path on the workspace EC2.
//
// `root="/configs"` (or empty / unrecognized) is treated as the
// runtime's MANAGED-config dir via workspaceFilePathPrefix —
// /home/ubuntu/.hermes for hermes, /configs for claude-code, etc.
// This preserves the v1 ReadFile/WriteFile behavior where the canvas's
// Config tab GETs/PUTs "config.yaml" without specifying a root and
// lands in the runtime's own config dir, even though that dir's
// absolute path differs per runtime.
//
// Any other allow-listed root (`/home`, `/workspace`, `/plugins`) is
// treated as a LITERAL absolute path on the EC2 host. Those roots are
// universal Linux paths that don't need per-runtime indirection.
//
// Restricting the literal pass-through to allowedRoots is the
// security boundary — the handler also gates this same set, so the
// resolver is defence-in-depth: even if a future caller forgets the
// handler-side check, the resolver won't translate `?root=/etc` into
// a real absolute path.
//
// relPath is sanitised by validateRelPath (no absolute, no `..`).
func resolveWorkspaceFilePath(runtime, root, relPath string) (string, error) {
	if err := validateRelPath(relPath); err != nil {
		return "", err
	}
	base := resolveWorkspaceRootPath(runtime, root)
	return filepath.Join(base, filepath.Clean(relPath)), nil
}

// resolveWorkspaceRootPath returns the absolute base directory on the
// workspace EC2 for a given (runtime, root) pair, without touching a
// relative file path. Used by listFilesViaEIC to compute the directory
// to walk; resolveWorkspaceFilePath joins this with relPath.
//
// Centralising the runtime-vs-literal indirection here means
// list/read/write/delete agree on what `?root=/configs` means for
// hermes vs claude-code vs an unknown runtime — otherwise list could
// show one directory while read/write target another.
func resolveWorkspaceRootPath(runtime, root string) string {
	root = strings.TrimSpace(root)
	// "/configs" + empty + unrecognized → runtime's managed-config dir.
	// The runtime prefix map is the SSOT for that translation.
	if root == "" || root == "/configs" || !allowedRoots[root] {
		base, ok := workspaceFilePathPrefix[strings.ToLower(strings.TrimSpace(runtime))]
		if !ok {
			base = "/configs"
		}
		return base
	}
	// Literal universal path (`/home`, `/workspace`, `/plugins`).
	return root
}

// eicFileOpTimeout bounds the whole tunnel + ssh dance. Key push is
// <500ms, tunnel is 1-2s, ssh + remote command is <2s for read/write.
// 30s gives headroom for slow EIC pulls + the larger `find` walk that
// listFilesViaEIC issues, without hanging the Files API forever under
// EIC misconfiguration.
const eicFileOpTimeout = 30 * time.Second

// eicSSHSession describes an open EIC tunnel ready for an ssh subprocess.
// Only valid inside the closure passed to withEICTunnel — the underlying
// keypair + tunnel are torn down when the closure returns.
type eicSSHSession struct {
	keyPath    string
	localPort  int
	osUser     string
	instanceID string
}

// withEICTunnel sets up an EIC SSH session (ephemeral keypair → push
// → AWS open-tunnel → wait-for-port), invokes fn with a session handle,
// and tears everything down on return. The caller is responsible for
// applying the per-op context.WithTimeout before calling — this helper
// only owns the EIC dance, not the operation budget, so a caller that
// needs a different timeout (e.g. a large bulk import) doesn't have to
// fight a hard-coded one.
//
// All AWS calls go through the package-level func vars in terminal.go
// (sendSSHPublicKey, openTunnelCmd) so tests can stub them the same way
// terminal_test.go does. The whole helper is also assigned to a
// `var` (`withEICTunnel`) so handler-dispatch tests can stub the entire
// dance instead of having to wire up a fake tunnel + fake ssh server.
var withEICTunnel = realWithEICTunnel

func realWithEICTunnel(ctx context.Context, instanceID string, fn func(s eicSSHSession) error) error {
	if instanceID == "" {
		return fmt.Errorf("workspace has no instance_id — not a SaaS EC2 workspace")
	}
	osUser := os.Getenv("WORKSPACE_EC2_OS_USER")
	if osUser == "" {
		osUser = "ubuntu"
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-2"
	}

	keyDir, err := os.MkdirTemp("", "molecule-eic-*")
	if err != nil {
		return fmt.Errorf("keydir mkdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(keyDir) }()
	keyPath := keyDir + "/id"
	if out, kerr := exec.CommandContext(ctx, "ssh-keygen",
		"-t", "ed25519", "-f", keyPath, "-N", "", "-q",
		"-C", "molecule-eic",
	).CombinedOutput(); kerr != nil {
		return fmt.Errorf("ssh-keygen: %w (%s)", kerr, strings.TrimSpace(string(out)))
	}
	pubKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return fmt.Errorf("read pubkey: %w", err)
	}

	if err := sendSSHPublicKey(ctx, region, instanceID, osUser, strings.TrimSpace(string(pubKey))); err != nil {
		return fmt.Errorf("send-ssh-public-key: %w", err)
	}

	localPort, err := pickFreePort()
	if err != nil {
		return fmt.Errorf("pick free port: %w", err)
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

	return fn(eicSSHSession{
		keyPath:    keyPath,
		localPort:  localPort,
		osUser:     osUser,
		instanceID: instanceID,
	})
}

// sshArgs returns the standard ssh CLI args for an EIC session pointed
// at the local tunnel port + a single remote command string.
//
// `LogLevel=ERROR` silences the benign "Warning: Permanently added
// '[127.0.0.1]:NNNNN' to known hosts" notice that ssh emits on every
// fresh tunnel connection. Without this, the notice lands on stderr
// and fools the read/list "empty stdout + empty stderr → not found"
// classifiers into thinking the warning is a real ssh-layer error → 500
// instead of 404 (Hermes config.yaml load, hongming tenant, 2026-05-05
// 02:38; PR #2822). Real auth/tunnel errors stay visible because they're
// emitted at ERROR level.
//
// Originally each helper assembled its own ssh args inline, so PR #2822's
// LogLevel=ERROR fix had to be applied to every copy. Centralising here
// means future ssh-option tweaks only land in one place.
func (s eicSSHSession) sshArgs(remoteCommand string) []string {
	return []string{
		"-i", s.keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ServerAliveInterval=15",
		"-p", fmt.Sprintf("%d", s.localPort),
		fmt.Sprintf("%s@127.0.0.1", s.osUser),
		remoteCommand,
	}
}

// buildInstallShell returns the remote command for atomically writing
// `/dev/stdin` to absPath with mode 0644 via `sudo -n install -D`.
// `install -D` creates any missing parent dirs and writes via
// temp-file-rename (atomic). Pure function for direct testability —
// the only variable input (absPath) is shellQuote-wrapped to defeat
// any shell metachar in a future caller's path.
func buildInstallShell(absPath string) string {
	return fmt.Sprintf("sudo -n install -D -m 0644 /dev/stdin %s", shellQuote(absPath))
}

// buildCatShell returns the remote command for reading absPath and
// swallowing missing-file stderr (so the empty-stdout + non-zero-exit
// case is unambiguous → os.ErrNotExist at the caller).
func buildCatShell(absPath string) string {
	return fmt.Sprintf("sudo -n cat %s 2>/dev/null", shellQuote(absPath))
}

// buildRmShell returns the remote command for `sudo -n rm -f` against
// absPath. `-f` (not `-rf`) is intentional — directory removal needs
// its own explicit endpoint if/when the canvas grows that affordance,
// and `-rf` would let a misclassified directory entry trigger a
// recursive delete.
func buildRmShell(absPath string) string {
	return fmt.Sprintf("sudo -n rm -f %s", shellQuote(absPath))
}

// buildFindShell returns the remote command for enumerating files
// under listPath up to maxDepth, emitting `TYPE|SIZE|REL_PATH` lines
// (matches the local-Docker container path's parser exactly).
//
// `2>/dev/null` swallows find's "No such file" error so a missing
// listing root surfaces as empty stdout (handler returns []) rather
// than 500.
//
// `stat -c %s` is GNU coreutils; `stat -f %z` is BSD. Try GNU first,
// fall back to BSD, then 0 — same shape the local-Docker `sh -c`
// version uses so a future cross-runtime fleet (Alpine vs Ubuntu)
// doesn't regress.
//
// Hidden / cache dir pruning matches the container path: .git,
// __pycache__, node_modules, .DS_Store. Without these the tree drowns
// in transient artefacts on a /workspace listing.
func buildFindShell(listPath string, maxDepth int) string {
	return fmt.Sprintf(
		`sudo -n find %s -maxdepth %d -not -path '*/.git/*' -not -path '*/__pycache__/*' -not -path '*/node_modules/*' -not -name .DS_Store 2>/dev/null | while IFS= read -r f; do `+
			`rel="${f#%s/}"; [ "$rel" = %s ] && continue; [ -z "$rel" ] && continue; `+
			`if [ -d "$f" ]; then echo "d|0|$rel"; else `+
			`s=$(stat -c %%s "$f" 2>/dev/null || stat -f %%z "$f" 2>/dev/null || echo 0); echo "f|$s|$rel"; `+
			`fi; done`,
		shellQuote(listPath), maxDepth, shellQuote(listPath), shellQuote(listPath),
	)
}

// parseFindOutput parses TYPE|SIZE|REL_PATH lines emitted by
// buildFindShell into eicFileEntry rows. Whitespace-only lines and
// malformed rows are silently skipped — the same behaviour as the
// local-Docker container parser for symmetric output.
func parseFindOutput(raw []byte) []eicFileEntry {
	files := make([]eicFileEntry, 0)
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		var size int64
		fmt.Sscanf(parts[1], "%d", &size)
		files = append(files, eicFileEntry{
			Path: parts[2],
			Size: size,
			Dir:  parts[0] == "d",
		})
	}
	return files
}

// shellQuote wraps a value in single quotes + escapes embedded single
// quotes for POSIX sh. Used for the variable parts of remote ssh
// commands (absolute paths). The paths are already built from a
// validated allowlist + Clean(), so traversal is blocked regardless;
// this is defence-in-depth against a future refactor that might accept
// user paths directly here.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeFileViaEIC writes a single file to the workspace EC2 at the
// absolute path that resolveWorkspaceFilePath computed. On success,
// optionally invokes the runtime's reload hook (not implemented yet —
// tracked as follow-up; for today the canvas issues a separate Restart
// after Save).
//
// `install -D` creates any missing parent dirs and writes atomically
// via temp-file-rename. Permissions 0644 match the existing tar-unpack
// defaults on the Docker path.
//
// `sudo -n` (non-interactive) prefix: the canonical containerized
// workspace layout puts /configs at the root, owned by root because
// cloud-init runs as root (see
// molecule-controlplane/internal/provisioner/userdata_containerized.go).
// SSH-as-ubuntu can't write into /configs without escalation. Ubuntu
// has passwordless sudo on EC2 by default; sudo -n fails fast (no
// prompt) if that ever changes, surfacing a clean error instead of a
// hang. The hermes path /home/ubuntu/.hermes is ubuntu-owned and
// doesn't strictly need sudo, but using it uniformly avoids per-runtime
// branching here.
func writeFileViaEIC(ctx context.Context, instanceID, runtime, root, relPath string, content []byte) error {
	absPath, err := resolveWorkspaceFilePath(runtime, root, relPath)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, eicFileOpTimeout)
	defer cancel()

	return withEICTunnel(ctx, instanceID, func(s eicSSHSession) error {
		sshCmd := exec.CommandContext(ctx, "ssh", s.sshArgs(buildInstallShell(absPath))...)
		sshCmd.Env = os.Environ()
		sshCmd.Stdin = bytes.NewReader(content)
		var stderr bytes.Buffer
		sshCmd.Stderr = &stderr
		if err := sshCmd.Run(); err != nil {
			// When the per-op context deadline (eicFileOpTimeout) fires,
			// exec.CommandContext SIGKILLs the ssh subprocess and Run()
			// returns the bare "signal: killed" with empty stderr. That
			// surfaced to the canvas as an opaque
			// `500 {"error":"ssh install: signal: killed ()"}` which gave
			// the operator no idea the workspace was simply mid-provision
			// with a slow/unready EIC tunnel (internal#423). Detect the
			// deadline explicitly and return an actionable message instead
			// — the EIC mechanism, timeout value, and success path are all
			// unchanged; this only improves the error a stuck write emits.
			if cerr := ctx.Err(); cerr != nil {
				reason := "timed out after " + eicFileOpTimeout.String()
				if errors.Is(cerr, context.Canceled) && !errors.Is(cerr, context.DeadlineExceeded) {
					reason = "was cancelled"
				}
				return fmt.Errorf(
					"ssh install: EIC tunnel to workspace %s — "+
						"the workspace may still be provisioning (slow/unready SSH); "+
						"retry once it is online, or apply provider credentials via "+
						"Settings → Secrets (encrypted, does not use this file-write path)",
					reason)
			}
			return fmt.Errorf("ssh install: %w (%s)", err, strings.TrimSpace(stderr.String()))
		}
		log.Printf("writeFileViaEIC: ws instance=%s runtime=%s root=%s wrote %d bytes → %s",
			instanceID, runtime, root, len(content), absPath)
		return nil
	})
}

// readFileViaEIC reads a single file from the workspace EC2 at the
// absolute path that resolveWorkspaceFilePath computes. Mirrors
// writeFileViaEIC (ephemeral keypair, EIC tunnel, ssh) so the canvas's
// Config tab can GET back what it just PUT.
//
// Returns ("", os.ErrNotExist) when the remote path doesn't exist so
// the handler can map it to HTTP 404 cleanly. Other errors propagate.
//
// `sudo -n cat`: /configs is root-owned (same reason writeFileViaEIC
// needs sudo). The path is built from a validated map + Clean(), so no
// user-controlled string reaches the shell here. `2>/dev/null` swallows
// `cat: ...: No such file` so the missing-file case returns empty
// stdout + non-zero exit, which we translate to os.ErrNotExist.
func readFileViaEIC(ctx context.Context, instanceID, runtime, root, relPath string) ([]byte, error) {
	absPath, err := resolveWorkspaceFilePath(runtime, root, relPath)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, eicFileOpTimeout)
	defer cancel()

	var out []byte
	runErr := withEICTunnel(ctx, instanceID, func(s eicSSHSession) error {
		sshCmd := exec.CommandContext(ctx, "ssh", s.sshArgs(buildCatShell(absPath))...)
		sshCmd.Env = os.Environ()
		var stdout, stderr bytes.Buffer
		sshCmd.Stdout = &stdout
		sshCmd.Stderr = &stderr
		err := sshCmd.Run()
		out = stdout.Bytes()
		if err != nil {
			// `cat` returns 1 on missing file; with 2>/dev/null we have no
			// stderr distinguisher. Treat empty-stdout + empty-stderr +
			// non-zero exit as not-found rather than a tunnel/auth error
			// (those usually produce stderr from ssh itself, not from the
			// remote command).
			if len(out) == 0 && stderr.Len() == 0 {
				return os.ErrNotExist
			}
			return fmt.Errorf("ssh cat: %w (%s)", err, strings.TrimSpace(stderr.String()))
		}
		log.Printf("readFileViaEIC: ws instance=%s runtime=%s root=%s read %d bytes ← %s",
			instanceID, runtime, root, len(out), absPath)
		return nil
	})
	if runErr != nil {
		return nil, runErr
	}
	return out, nil
}

// eicFileEntry is the wire shape returned by listFilesViaEIC. It
// matches the inline `fileEntry` in templates.go::ListFiles so the
// handler can emit either path's output without a translation layer.
type eicFileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Dir  bool   `json:"dir"`
}

// listFilesViaEIC enumerates files under <root>/<sub> on the workspace
// EC2 host, up to the given depth, returning entries with paths
// relative to the listing root (matching the local-Docker path's
// output). Closes the symmetry gap that left ListFiles silently
// returning [] for SaaS workspaces — see issue #2999.
//
// Output line format: TYPE|SIZE|REL_PATH (matches the container's find
// shell so the parser is identical). `find -maxdepth N` traverses up
// to N levels; the canvas requests depth=1 by default and re-fetches
// when the user expands a directory.
//
// Pruning: same hidden / cache dirs as the container path (.git,
// __pycache__, node_modules, .DS_Store) so the canvas's tree doesn't
// drown in transient artefacts.
//
// `sudo -n` matches the read/write paths — even though the universal
// roots (/home, /workspace, /plugins) are typically ubuntu-owned and
// don't need it, /configs and runtime-prefix dirs do (root-owned by
// cloud-init), and using sudo uniformly avoids per-root branching.
func listFilesViaEIC(ctx context.Context, instanceID, runtime, root, sub string, depth int) ([]eicFileEntry, error) {
	if sub != "" {
		if err := validateRelPath(sub); err != nil {
			return nil, fmt.Errorf("invalid sub: %w", err)
		}
	}
	if depth < 1 {
		depth = 1
	}
	if depth > 5 {
		depth = 5
	}
	listPath := resolveWorkspaceRootPath(runtime, root)
	if sub != "" {
		listPath = filepath.Join(listPath, filepath.Clean(sub))
	}

	ctx, cancel := context.WithTimeout(ctx, eicFileOpTimeout)
	defer cancel()

	var rawOutput []byte
	runErr := withEICTunnel(ctx, instanceID, func(s eicSSHSession) error {
		sshCmd := exec.CommandContext(ctx, "ssh", s.sshArgs(buildFindShell(listPath, depth))...)
		sshCmd.Env = os.Environ()
		var stdout, stderr bytes.Buffer
		sshCmd.Stdout = &stdout
		sshCmd.Stderr = &stderr
		if err := sshCmd.Run(); err != nil {
			// Empty stdout + empty stderr after we swallowed find's
			// own error stream means the listing root genuinely
			// doesn't exist on this workspace — return an empty
			// slice rather than a 500. Real ssh/tunnel errors emit
			// to stderr at LogLevel=ERROR.
			if stdout.Len() == 0 && stderr.Len() == 0 {
				rawOutput = nil
				return nil
			}
			return fmt.Errorf("ssh find: %w (%s)", err, strings.TrimSpace(stderr.String()))
		}
		rawOutput = stdout.Bytes()
		return nil
	})
	if runErr != nil {
		return nil, runErr
	}

	files := parseFindOutput(rawOutput)
	log.Printf("listFilesViaEIC: ws instance=%s runtime=%s root=%s sub=%s depth=%d → %d entries from %s",
		instanceID, runtime, root, sub, depth, len(files), listPath)
	return files, nil
}

// deleteFileViaEIC removes a single file from the workspace EC2.
// Returns nil for both "deleted" and "didn't exist" — `rm -f` doesn't
// distinguish, and the canvas's delete-then-refresh flow doesn't need
// it to.
//
// Symmetry note: pre-fix DeleteFile (templates.go:514) had no EIC
// branch, so right-click delete on a SaaS workspace would fall through
// to the local-Docker path, find no container (dockerCli is nil on
// SaaS), and try the ephemeral-volume path which itself only handles
// local Docker volumes. Net effect: silent no-op. Closing this gap is
// part of issue #2999.
func deleteFileViaEIC(ctx context.Context, instanceID, runtime, root, relPath string) error {
	absPath, err := resolveWorkspaceFilePath(runtime, root, relPath)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, eicFileOpTimeout)
	defer cancel()

	return withEICTunnel(ctx, instanceID, func(s eicSSHSession) error {
		sshCmd := exec.CommandContext(ctx, "ssh", s.sshArgs(buildRmShell(absPath))...)
		sshCmd.Env = os.Environ()
		var stderr bytes.Buffer
		sshCmd.Stderr = &stderr
		if err := sshCmd.Run(); err != nil {
			return fmt.Errorf("ssh rm: %w (%s)", err, strings.TrimSpace(stderr.String()))
		}
		log.Printf("deleteFileViaEIC: ws instance=%s runtime=%s root=%s removed %s",
			instanceID, runtime, root, absPath)
		return nil
	})
}
