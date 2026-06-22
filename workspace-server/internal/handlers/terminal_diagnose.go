package handlers

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
)

// syncBuf is a goroutine-safe writer that wraps bytes.Buffer with a mutex.
// Used to capture subprocess stderr without racing the os/exec stderr-copy
// goroutine: ``cmd.Stderr = io.Writer`` spawns a background goroutine that
// reads from the subprocess's stderr fd and calls Write on our writer, so
// reading the buffer from another goroutine (e.g., on wait-for-port
// timeout while the tunnel may still be writing) without synchronization
// is a data race that ``go test -race`` would flag. ``strings.Builder``
// and bare ``bytes.Buffer`` aren't goroutine-safe; this tiny shim is the
// cheapest fix.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// unwrapGoError extracts subprocess stderr from a Go-wrapped error that
// includes combined output. e.g. from sendSSHPublicKey's
// fmt.Errorf("send-ssh-public-key: %w (%s)", err, combinedOut), this
// returns the " (%s)" portion — the actionable subprocess signal like
// "AccessDeniedException: ... is not authorized to perform:
// ec2-instance-connect:OpenTunnel". Returns "" when the output is
// identical to the error string (no stderr captured).
func unwrapGoError(errMsg string) string {
	// Extract content between the last '(' and trailing ')'. The
	// sendSSHPublicKey wrapper uses fmt.Errorf("...: %w (%s)", err, combinedOut)
	// so the subprocess stderr is always the last parenthesised segment,
	// e.g. "send-ssh-public-key: exit status 1 (AccessDeniedException: ...)"
	// — note the closing ')' is at the very end with no trailing space.
	open := strings.LastIndex(errMsg, "(")
	if open < 0 {
		return ""
	}
	inner := errMsg[open+1:]
	return strings.TrimSuffix(inner, ")")
}

// HandleDiagnose handles GET /workspaces/:id/terminal/diagnose. It runs the
// same per-step pipeline as HandleConnect (ssh-keygen → EIC send-key → tunnel
// → ssh) but non-interactively, captures the first failing step and its
// stderr, and returns the result as JSON.
//
// Why this exists: when the canvas terminal silently disconnects ("Session
// ended" with no error frame), there is no remote-readable signal of which
// stage failed. The ssh client's stderr lives in the workspace-server's
// process logs on the tenant CP EC2 — invisible without shell access.
// HandleConnect can't trivially expose stderr because it has already
// upgraded to WebSocket binary frames by the time ssh runs. HandleDiagnose
// stays pure HTTP/JSON, so the same auth (WorkspaceAuth + ADMIN_TOKEN
// fallback) gives operators a one-call probe of the whole shell pipeline.
//
// Stages mirrored from handleRemoteConnect:
//
//	1. ssh-keygen          (ephemeral session keypair)
//	2. send-ssh-public-key (AWS EIC API push, IAM-gated)
//	3. pick-free-port      (local port for the tunnel)
//	4. open-tunnel         (aws ec2-instance-connect open-tunnel start)
//	5. wait-for-port       (the tunnel actually listens)
//	6. ssh-probe           (`ssh ... 'echo MARKER'` — proves end-to-end auth+shell)
//
// Local Docker workspaces (no instance_id row) get a smaller probe:
// container-found + container-running. Same response shape so callers
// don't need to branch.
func (h *TerminalHandler) HandleDiagnose(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	// KI-005 hierarchy check — same shape as HandleConnect. Without this,
	// an org-level token holder can probe any workspace in their tenant by
	// guessing the UUID, learning its diagnostic state (which IAM call
	// fails, what sshd says) even when they don't own it. Per-workspace
	// bearer tokens are already URL-bound by WorkspaceAuth, so the gap is
	// org tokens — same vector KI-005 closed for /terminal (#1609).
	callerID := c.GetHeader("X-Workspace-ID")
	if callerID != "" && callerID != workspaceID {
		tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
		if tok != "" {
			if err := wsauth.ValidateToken(ctx, db.DB, callerID, tok); err != nil {
				if c.GetString("org_token_id") == "" {
					c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token for claimed workspace"})
					return
				}
			}
		}
		if !canCommunicateCheck(callerID, workspaceID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "not authorized to diagnose this workspace's terminal"})
			return
		}
	}

	var instanceID string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(instance_id, '') FROM workspaces WHERE id = $1`,
		workspaceID).Scan(&instanceID); err != nil {
		log.Printf("terminal diagnose: instance_id query failed for workspace %s: %v", workspaceID, err)
	}

	var res diagnoseResult
	if instanceID != "" {
		res = h.diagnoseRemote(ctx, workspaceID, instanceID)
	} else {
		res = h.diagnoseLocal(ctx, workspaceID)
	}
	c.JSON(http.StatusOK, res)
}

// diagnoseStep is one row in the diagnostic report. Always carries Name +
// OK + DurationMs; Error/Detail filled when the step fails.
type diagnoseStep struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// diagnoseResult is the full report. ``OK`` is true only when every step
// passed; ``FirstFailure`` names the step that broke the chain so callers
// can route alerts (e.g., "send-ssh-public-key" → IAM team; "ssh-probe" →
// SG/sshd team).
type diagnoseResult struct {
	WorkspaceID  string         `json:"workspace_id"`
	InstanceID   string         `json:"instance_id,omitempty"`
	Remote       bool           `json:"remote"`
	OK           bool           `json:"ok"`
	FirstFailure string         `json:"first_failure,omitempty"`
	Steps        []diagnoseStep `json:"steps"`
}

// sshProbeMarker is the string the ssh probe echoes back. Distinct from any
// shell builtin output so we can grep for it unambiguously even when the
// remote prints a banner or motd.
const sshProbeMarker = "MOLECULE_TERMINAL_PROBE_OK"

// sshProbeCmd builds the non-interactive ssh probe command. Exposed as a
// var so tests can stub it without spinning up a real sshd. BatchMode=yes
// ensures ssh fails fast on prompt instead of hanging on a TTY.
var sshProbeCmd = func(o eicSSHOptions) *exec.Cmd {
	return exec.Command(
		"ssh",
		"-i", o.PrivateKeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-p", fmt.Sprintf("%d", o.LocalPort),
		fmt.Sprintf("%s@127.0.0.1", o.OSUser),
		"echo "+sshProbeMarker,
	)
}

// diagnoseRemote runs the full EIC + ssh probe and reports per-step status.
// Bails on the first failure so the operator sees which stage breaks; later
// stages stay in the report as zero-value rows so the response shape is
// stable regardless of where the chain stopped.
func (h *TerminalHandler) diagnoseRemote(ctx context.Context, workspaceID, instanceID string) diagnoseResult {
	res := diagnoseResult{
		WorkspaceID: workspaceID,
		InstanceID:  instanceID,
		Remote:      true,
	}

	osUser := os.Getenv("WORKSPACE_EC2_OS_USER")
	if osUser == "" {
		osUser = "ubuntu"
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-2"
	}

	stop := func(name string, step diagnoseStep) diagnoseResult {
		res.Steps = append(res.Steps, step)
		res.FirstFailure = name
		return res
	}

	// Step 1: ssh-keygen
	t0 := time.Now()
	keyDir, err := os.MkdirTemp("", "molecule-diagnose-*")
	if err != nil {
		return stop("ssh-keygen", diagnoseStep{
			Name:       "ssh-keygen",
			DurationMs: time.Since(t0).Milliseconds(),
			Error:      fmt.Sprintf("mkdir tmp: %v", err),
		})
	}
	defer func() { _ = os.RemoveAll(keyDir) }()
	keyPath := keyDir + "/id"
	keygen := exec.CommandContext(ctx, "ssh-keygen", "-t", "ed25519", "-f", keyPath, "-N", "", "-q", "-C", "molecule-diagnose")
	if out, kerr := keygen.CombinedOutput(); kerr != nil {
		return stop("ssh-keygen", diagnoseStep{
			Name:       "ssh-keygen",
			DurationMs: time.Since(t0).Milliseconds(),
			Error:      kerr.Error(),
			Detail:     strings.TrimSpace(string(out)),
		})
	}
	res.Steps = append(res.Steps, diagnoseStep{Name: "ssh-keygen", OK: true, DurationMs: time.Since(t0).Milliseconds()})

	pubKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return stop("read-pubkey", diagnoseStep{
			Name:  "read-pubkey",
			Error: fmt.Sprintf("read pubkey: %v", err),
		})
	}

	// Step 2: send-ssh-public-key (AWS Instance Connect)
	// mc#687: populate Detail so the E2E smoke sees the AWS permission error
	// verbatim. The subprocess stderr (e.g. "AccessDeniedException: ... is not
	// authorized to perform: ec2-instance-connect:OpenTunnel") is captured by
	// sendSSHPublicKey's CombinedOutput() and embedded in the Go error string.
	t0 = time.Now()
	if err := sendSSHPublicKey(ctx, region, instanceID, osUser, strings.TrimSpace(string(pubKey))); err != nil {
		errMsg := err.Error()
		return stop("send-ssh-public-key", diagnoseStep{
			Name:       "send-ssh-public-key",
			DurationMs: time.Since(t0).Milliseconds(),
			Error:      errMsg,
			Detail:     unwrapGoError(errMsg),
		})
	}
	res.Steps = append(res.Steps, diagnoseStep{Name: "send-ssh-public-key", OK: true, DurationMs: time.Since(t0).Milliseconds()})

	// Step 3: pick-free-port
	t0 = time.Now()
	localPort, err := pickFreePort()
	if err != nil {
		return stop("pick-free-port", diagnoseStep{
			Name:       "pick-free-port",
			DurationMs: time.Since(t0).Milliseconds(),
			Error:      err.Error(),
		})
	}
	res.Steps = append(res.Steps, diagnoseStep{
		Name:       "pick-free-port",
		OK:         true,
		DurationMs: time.Since(t0).Milliseconds(),
		Detail:     fmt.Sprintf("port=%d", localPort),
	})

	// Step 4: open-tunnel (long-running subprocess; we hold its stderr so
	// we can include it in failure detail for the next two stages).
	opts := eicSSHOptions{
		InstanceID:     instanceID,
		OSUser:         osUser,
		Region:         region,
		LocalPort:      localPort,
		PrivateKeyPath: keyPath,
	}
	t0 = time.Now()
	tunnel := openTunnelCmd(opts)
	tunnel.Env = os.Environ()
	var tunnelStderr syncBuf
	tunnel.Stderr = &tunnelStderr
	if err := tunnel.Start(); err != nil {
		return stop("open-tunnel", diagnoseStep{
			Name:       "open-tunnel",
			DurationMs: time.Since(t0).Milliseconds(),
			Error:      err.Error(),
			Detail:     tunnelStderr.String(),
		})
	}
	defer func() {
		if tunnel.Process != nil {
			_ = tunnel.Process.Kill()
		}
		_ = tunnel.Wait()
	}()
	res.Steps = append(res.Steps, diagnoseStep{Name: "open-tunnel", OK: true, DurationMs: time.Since(t0).Milliseconds()})

	// Step 5: wait-for-port — verifies the tunnel actually bound the port.
	// Tunnel-side errors (auth, SG, missing endpoint) usually surface here
	// because the subprocess exits before binding. Fold its stderr into the
	// detail so the operator sees the real reason.
	t0 = time.Now()
	if err := waitForPort(ctx, "127.0.0.1", localPort, 10*time.Second); err != nil {
		return stop("wait-for-port", diagnoseStep{
			Name:       "wait-for-port",
			DurationMs: time.Since(t0).Milliseconds(),
			Error:      err.Error(),
			Detail:     tunnelStderr.String(),
		})
	}
	res.Steps = append(res.Steps, diagnoseStep{Name: "wait-for-port", OK: true, DurationMs: time.Since(t0).Milliseconds()})

	// Step 6: ssh-probe — non-interactive `ssh ... 'echo MARKER'`. Proves
	// auth (key push reached sshd), shell ready (bash returns echo output),
	// and the network path end-to-end. Captures combined output + exit
	// error so we see "Permission denied", "Connection refused", or "Host
	// key verification failed" verbatim.
	t0 = time.Now()
	probe := sshProbeCmd(opts)
	probe.Env = os.Environ()
	out, perr := probe.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	durMs := time.Since(t0).Milliseconds()
	if perr != nil || !strings.Contains(outStr, sshProbeMarker) {
		errStr := ""
		if perr != nil {
			errStr = perr.Error()
		}
		return stop("ssh-probe", diagnoseStep{
			Name:       "ssh-probe",
			DurationMs: durMs,
			Error:      errStr,
			Detail:     outStr,
		})
	}
	res.Steps = append(res.Steps, diagnoseStep{Name: "ssh-probe", OK: true, DurationMs: durMs})

	res.OK = true
	return res
}

// diagnoseLocal probes the Docker container path. Smaller surface: just
// "is the named container running on this Docker daemon".
func (h *TerminalHandler) diagnoseLocal(ctx context.Context, workspaceID string) diagnoseResult {
	res := diagnoseResult{WorkspaceID: workspaceID, Remote: false}
	if h.docker == nil {
		res.Steps = append(res.Steps, diagnoseStep{
			Name:  "docker-available",
			Error: "docker client not configured on this workspace-server",
		})
		res.FirstFailure = "docker-available"
		return res
	}

	candidates := []string{provisioner.ContainerName(workspaceID), "ws-" + workspaceID}
	var foundName string
	var lastErr error
	var running bool
	var stateStatus string
	t0 := time.Now()
	for _, n := range candidates {
		info, err := h.docker.ContainerInspect(ctx, n)
		if err == nil {
			foundName = n
			running = info.State.Running
			stateStatus = info.State.Status
			break
		}
		lastErr = err
	}
	if foundName == "" {
		errMsg := "no matching container"
		if lastErr != nil {
			errMsg = lastErr.Error()
		}
		res.Steps = append(res.Steps, diagnoseStep{
			Name:       "container-found",
			DurationMs: time.Since(t0).Milliseconds(),
			Error:      errMsg,
			Detail:     fmt.Sprintf("tried: %s", strings.Join(candidates, ", ")),
		})
		res.FirstFailure = "container-found"
		return res
	}
	res.Steps = append(res.Steps, diagnoseStep{
		Name:       "container-found",
		OK:         true,
		DurationMs: time.Since(t0).Milliseconds(),
		Detail:     foundName,
	})

	if !running {
		res.Steps = append(res.Steps, diagnoseStep{
			Name:   "container-running",
			Error:  "container not running",
			Detail: stateStatus,
		})
		res.FirstFailure = "container-running"
		return res
	}
	res.Steps = append(res.Steps, diagnoseStep{Name: "container-running", OK: true, Detail: stateStatus})
	res.OK = true
	return res
}
