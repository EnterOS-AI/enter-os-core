package handlers

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/registry"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/creack/pty"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const terminalSessionTimeout = 30 * time.Minute

var termUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" ||
			strings.HasPrefix(origin, "http://localhost:") ||
			strings.HasPrefix(origin, "https://localhost:") {
			return true
		}
		// Also allow origins from CORS_ORIGINS env var
		if corsOrigins := os.Getenv("CORS_ORIGINS"); corsOrigins != "" {
			for _, allowed := range strings.Split(corsOrigins, ",") {
				if strings.TrimSpace(allowed) == origin {
					return true
				}
			}
		}
		return false
	},
}

type TerminalHandler struct {
	docker *client.Client
}

func NewTerminalHandler(cli *client.Client) *TerminalHandler {
	return &TerminalHandler{docker: cli}
}

// canCommunicateCheck is the communication-authorization predicate used by
// HandleConnect to enforce the KI-005 workspace-hierarchy guard.
// Exposed as a package var so tests can stub it without DB fixtures.
var canCommunicateCheck = registry.CanCommunicate

// HandleConnect handles WS /workspaces/:id/terminal. Routes to the remote
// path (aws ec2-instance-connect ssh + docker exec) when the workspace row
// has an instance_id; falls back to local Docker otherwise. Both paths are
// guarded by the KI-005 CanCommunicate check before dispatch.
func (h *TerminalHandler) HandleConnect(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// KI-005 fix: enforce CanCommunicate hierarchy check before granting
	// terminal access. WorkspaceAuth validates the bearer's token, but the
	// token is scoped to a specific workspace ID — Workspace A's token can
	// reach Workspace A's terminal. Without CanCommunicate, Workspace A could
	// also reach Workspace B's terminal if it knows B's UUID (enumeration
	// via canvas, logs, or delegation). Shell access is more dangerous than
	// A2A message-passing, so we apply the same hierarchy check here.
	// GH#756/#1609 security fix: if the caller claims a specific workspace
	// identity (X-Workspace-ID header), the bearer token — if present — must
	// belong to that claimed workspace. Previously ValidateAnyToken accepted
	// ANY valid org token, allowing Workspace A to forge X-Workspace-ID: B
	// and reach B's terminal if A held any valid token. ValidateToken binds
	// the workspace-scoped token to the claimed workspace identity. Org-level
	// tokens are handled separately via the org_token_id context key.
	callerID := c.GetHeader("X-Workspace-ID")
	if callerID != "" && callerID != workspaceID {
		tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
		if tok != "" {
			if err := wsauth.ValidateToken(ctx, db.DB, callerID, tok); err != nil {
				// Org-scoped tokens (org_api_tokens) are validated at the org level
				// by WorkspaceAuth and do not have a workspace_auth_tokens row, so
				// ValidateToken always returns ErrInvalidToken for them. If WorkspaceAuth
				// already validated an org token (org_token_id set in context), trust
				// the X-Workspace-ID claim — the hierarchy is enforced by
				// canCommunicateCheck below. Reject everything else.
				if c.GetString("org_token_id") == "" {
					c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token for claimed workspace"})
					return
				}
			}
		}
		if !canCommunicateCheck(callerID, workspaceID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "not authorized to access this workspace's terminal"})
			return
		}
	}

	// Check for CP-provisioned workspace (instance_id persisted by
	// provisionWorkspaceCP → migration 038). Null instance_id means the
	// workspace runs as a local Docker container on this tenant.
	var instanceID string
	db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(instance_id, '') FROM workspaces WHERE id = $1`,
		workspaceID).Scan(&instanceID)

	if instanceID != "" {
		h.handleRemoteConnect(c, workspaceID, instanceID)
		return
	}

	h.handleLocalConnect(c, workspaceID)
}

// handleLocalConnect attaches to a Docker container running on this
// tenant's Docker daemon. Original behavior preserved exactly.
func (h *TerminalHandler) handleLocalConnect(c *gin.Context, workspaceID string) {
	if h.docker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Docker not available"})
		return
	}

	ctx := c.Request.Context()

	// Try multiple container name patterns:
	// 1. Provisioner naming: ws-{id[:12]}
	// 2. Full workspace ID fallback
	// 3. Workspace name from DB (normalized to lowercase-hyphen)
	name := provisioner.ContainerName(workspaceID)
	candidates := []string{name}
	if name != "ws-"+workspaceID {
		candidates = append(candidates, "ws-"+workspaceID)
	}

	// Look up workspace name for manual container naming
	var wsName string
	if _, err := h.docker.Ping(ctx); err == nil {
		db.DB.QueryRowContext(ctx, `SELECT LOWER(REPLACE(name, ' ', '-')) FROM workspaces WHERE id = $1`, workspaceID).Scan(&wsName)
		if wsName != "" {
			candidates = append(candidates, wsName)
		}
	}

	// Find the first running container that matches
	var containerName string
	for _, name := range candidates {
		info, err := h.docker.ContainerInspect(ctx, name)
		if err == nil && info.State.Running {
			containerName = name
			break
		}
	}

	if containerName == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "container not running"})
		return
	}

	// Upgrade to WebSocket
	conn, err := termUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Terminal WebSocket upgrade error: %v", err)
		return
	}

	// No hard session deadline — terminal stays open as long as there is activity.
	// The idle timeout (terminalSessionTimeout) resets on each keystroke in the
	// WebSocket→stdin bridge loop below.
	// The container exec ends when the user types 'exit' or the container stops.

	// Try bash first for better UX (tab completion, history), fall back to sh.
	// ContainerExecCreate succeeds even if the binary doesn't exist — the error
	// only surfaces at attach/start time, so we must retry at the attach level.
	var resp types.HijackedResponse
	var execErr error
	for _, shell := range []string{"/bin/bash", "/bin/sh"} {
		execCfg := container.ExecOptions{
			Cmd:          []string{shell},
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			Tty:          true,
		}
		execID, createErr := h.docker.ContainerExecCreate(ctx, containerName, execCfg)
		if createErr != nil {
			execErr = createErr
			continue
		}
		resp, execErr = h.docker.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{Tty: true})
		if execErr == nil {
			defer resp.Close()
			break
		}
	}
	if execErr != nil {
		log.Printf("Terminal exec error: %v", execErr)
		conn.WriteMessage(websocket.TextMessage, []byte("Error: failed to create shell session\r\n"))
		conn.Close()
		return
	}

	// Bridge: container stdout → WebSocket
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Reader.Read(buf)
			if n > 0 {
				if writeErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("Terminal read error: %v", err)
				}
				conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
		}
	}()

	// Bridge: WebSocket → container stdin
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if _, err := resp.Conn.Write(msg); err != nil {
			break
		}
		// Reset read deadline on activity
		conn.SetReadDeadline(time.Now().Add(terminalSessionTimeout))
	}

	<-done
}

// eicSSHOptions bundles the per-session inputs for spawning the EIC tunnel
// and the ssh client that rides on top of it. Fields are plain data so
// tests can stub the two factories below without fighting exec.Cmd.
type eicSSHOptions struct {
	InstanceID    string
	OSUser        string
	Region        string
	LocalPort     int
	PrivateKeyPath string
}

// openTunnelCmd builds the argv that opens a TLS-tunneled TCP port from
// the local machine to the workspace EC2's sshd via the EIC Endpoint.
// Long-lived: stays up for the whole terminal session.
var openTunnelCmd = func(o eicSSHOptions) *exec.Cmd {
	args := []string{
		"ec2-instance-connect", "open-tunnel",
		"--instance-id", o.InstanceID,
		"--local-port", fmt.Sprintf("%d", o.LocalPort),
	}
	if o.Region != "" {
		args = append([]string{"--region", o.Region}, args...)
	}
	return exec.Command("aws", args...)
}

// sshCommandCmd builds the argv for the interactive ssh client that rides
// on the open tunnel. The remote side is the workspace EC2's sshd bound
// to 22; with CP provisioning today the workspace runs as a native
// process under the ubuntu user, so landing at ubuntu's shell IS the
// terminal experience.
//
// ConnectTimeout=10 is the user-experience guard — without it, ssh waits
// indefinitely for the remote sshd's banner. When the workspace EC2's
// sshd is unresponsive (mid-restart, SG drop, AMI without ec2-instance-
// connect installed) the canvas's xterm shows the user's typed bytes
// echoed back by the workspace-server's *local* PTY (cooked + echo mode
// before ssh finishes its handshake) and then closes silently when CF's
// idle WebSocket timer fires, with no "Connection refused" or "Permission
// denied" output ever reaching the user. Capping at 10s makes the failure
// surface as a real ssh error message in the terminal — caught 2026-04-30
// when hongmingwang's hermes shell hung after the heartbeat-fix redeploy
// and a probe at /workspaces/<id>/terminal sat for 60s with the only
// frame being the local-PTY echo of a single 'X' typed mid-handshake.
var sshCommandCmd = func(o eicSSHOptions) *exec.Cmd {
	return exec.Command(
		"ssh",
		"-i", o.PrivateKeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-p", fmt.Sprintf("%d", o.LocalPort),
		fmt.Sprintf("%s@127.0.0.1", o.OSUser),
	)
}

// sendSSHPublicKey pushes an ephemeral public key to the EIC service so
// the workspace's sshd accepts the paired private key for the next 60s.
// Exposed as a var so tests can stub the AWS call.
var sendSSHPublicKey = func(ctx context.Context, region, instanceID, osUser, pubKey string) error {
	cmd := exec.CommandContext(ctx, "aws", "ec2-instance-connect", "send-ssh-public-key",
		"--region", region,
		"--instance-id", instanceID,
		"--instance-os-user", osUser,
		"--ssh-public-key", pubKey)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("send-ssh-public-key: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// handleRemoteConnect opens a terminal session on a workspace EC2 using:
//
//	aws ec2-instance-connect send-ssh-public-key   (push ephemeral key)
//	aws ec2-instance-connect open-tunnel           (TLS tunnel to :22)
//	ssh -p <tunnel-port> ubuntu@127.0.0.1          (interactive shell)
//
// CP-provisioned workspaces run as native processes under ubuntu, not
// Docker. Design: docs/infra/workspace-terminal.md.
func (h *TerminalHandler) handleRemoteConnect(c *gin.Context, workspaceID, instanceID string) {
	osUser := os.Getenv("WORKSPACE_EC2_OS_USER")
	if osUser == "" {
		osUser = "ubuntu" // Ubuntu 24.04 AMI, default CP workspace runtime user
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-2" // CP default — override via env
	}

	conn, err := termUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Terminal WebSocket upgrade error (remote): %v", err)
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	// Ephemeral keypair — never hits disk after the session ends, and is
	// only valid for <60s on the instance side regardless.
	keyDir, err := os.MkdirTemp("", "molecule-terminal-*")
	if err != nil {
		log.Printf("Terminal keydir mkdir for ws=%s: %v", workspaceID, err)
		_ = conn.WriteMessage(websocket.TextMessage,
			[]byte("Error: failed to allocate session keypair\r\n"))
		return
	}
	defer func() { _ = os.RemoveAll(keyDir) }()
	keyPath := keyDir + "/id"
	keygen := exec.CommandContext(ctx, "ssh-keygen", "-t", "ed25519", "-f", keyPath, "-N", "", "-q", "-C", "molecule-terminal")
	if out, kerr := keygen.CombinedOutput(); kerr != nil {
		log.Printf("Terminal ssh-keygen for ws=%s: %v (%s)", workspaceID, kerr, out)
		_ = conn.WriteMessage(websocket.TextMessage,
			[]byte("Error: failed to generate session keypair\r\n"))
		return
	}
	pubKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		log.Printf("Terminal pubkey read for ws=%s: %v", workspaceID, err)
		return
	}

	// 1. Push public key — sshd accepts matching private for 60s.
	if err := sendSSHPublicKey(ctx, region, instanceID, osUser, strings.TrimSpace(string(pubKey))); err != nil {
		log.Printf("Terminal EIC send-key for ws=%s instance=%s: %v", workspaceID, instanceID, err)
		_ = conn.WriteMessage(websocket.TextMessage,
			[]byte("Error: failed to push session key (check tenant IAM + see docs/infra/workspace-terminal.md)\r\n"))
		return
	}

	// 2. Open tunnel on an OS-picked free port; retry briefly because
	//    tunnel takes ~1-2s to start listening after exec.
	localPort, err := pickFreePort()
	if err != nil {
		log.Printf("Terminal free port pick failed: %v", err)
		return
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
		log.Printf("Terminal tunnel start for ws=%s: %v", workspaceID, err)
		_ = conn.WriteMessage(websocket.TextMessage,
			[]byte("Error: failed to open EIC tunnel (check EIC Endpoint + SG 22 from endpoint SG; see docs/infra/workspace-terminal.md)\r\n"))
		return
	}
	defer func() {
		if tunnel.Process != nil {
			_ = tunnel.Process.Kill()
		}
		_ = tunnel.Wait()
	}()
	if err := waitForPort(ctx, "127.0.0.1", localPort, 10*time.Second); err != nil {
		log.Printf("Terminal tunnel never listened for ws=%s: %v", workspaceID, err)
		_ = conn.WriteMessage(websocket.TextMessage,
			[]byte("Error: EIC tunnel didn't come up in time\r\n"))
		return
	}

	// 3. SSH over the tunnel, pty-wrapped so bash behaves interactively.
	cmd := sshCommandCmd(opts)
	cmd.Env = os.Environ()

	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Printf("Terminal ssh pty.Start for ws=%s: %v", workspaceID, err)
		_ = conn.WriteMessage(websocket.TextMessage,
			[]byte("Error: failed to launch ssh client\r\n"))
		return
	}
	defer func() { _ = ptmx.Close() }()

	done := make(chan struct{})

	// PTY → WebSocket
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if wErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); wErr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("Terminal remote read error: %v", err)
				}
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
		}
	}()

	// WebSocket → PTY (stdin)
	go func() {
		for {
			_, msg, rErr := conn.ReadMessage()
			if rErr != nil {
				cancel()
				return
			}
			if _, wErr := ptmx.Write(msg); wErr != nil {
				cancel()
				return
			}
			conn.SetReadDeadline(time.Now().Add(terminalSessionTimeout))
		}
	}()

	// Wait on either pipe to finish or context cancel.
	select {
	case <-done:
	case <-ctx.Done():
	}

	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
}

// pickFreePort asks the OS for an unused TCP port in the ephemeral range.
// There's an unavoidable TOCTOU window between close() and the EIC tunnel
// binding the port; in practice the window is short enough that we've
// never seen a collision in testing.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// waitForPort polls 127.0.0.1:<port> until something is listening or the
// deadline passes. Used to wait for the EIC tunnel subprocess to bind
// its local port before we dial ssh at it.
func waitForPort(ctx context.Context, host string, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	// JoinHostPort handles IPv6 bracketing; `%s:%d` does not. Caught by
	// `go vet` on ubuntu-latest (newer Go toolchain than the Mac mini).
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", addr)
}
