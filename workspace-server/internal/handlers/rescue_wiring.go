package handlers

// rescue_wiring.go — bridges the leaf internal/rescue package to the
// handlers package's EIC/SSH runner + secret redactor, and exposes the
// boot-failure rescue hook used by both boot-failure verdict paths
// (handlers.BootstrapFailed here, registry.sweepStuckProvisioning via
// an injected hook wired in main.go).
//
// Why the indirection: internal/rescue is a leaf so registry (which
// must NOT import handlers — that's an import cycle) can call it. The
// two heavy dependencies live here in handlers — `withEICTunnel`
// (the EIC keypair → push → tunnel → ssh dance) and `redactSecrets`
// (the SAFE-T1201 secret-scan) — so we inject them into rescue's
// package-level func vars at init().
//
// RFC internal#742 Part 2.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/rescue"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/rescuestore"
)

func init() {
	// Wire the leaf rescue package to handlers' EIC runner + redactor.
	// Done in init() (not main.go) so the binding is present for any
	// caller of rescue.Capture, including the registry sweeper hook and
	// the handler path, without each call site re-wiring it.
	rescue.RunRemote = rescueRunRemoteViaEIC
	rescue.Redact = func(workspaceID, content string) string {
		out, _ := redactSecrets(workspaceID, content)
		return out
	}
	// Part 3: persist the redacted bundle to the queryable store on
	// capture so GET /workspaces/:id/rescue can serve it without obs/Loki
	// read creds. db.DB is resolved per-call (rescuestore guards a nil
	// handle) so wiring at init() is safe even before InitPostgres has
	// run; a capture before the DB is up logs + skips the persist rather
	// than failing the boot-failure path.
	rescue.PersistBundle = func(ctx context.Context, b rescue.Bundle) error {
		return rescuestore.NewPostgres(db.DB).Persist(ctx, b)
	}
}

// rescueRunRemoteViaEIC runs a single shell command on the still-running
// (but boot-failed) workspace EC2 over an EIC tunnel and returns its
// combined stdout+stderr. Reuses the same `withEICTunnel` dance as the
// canvas file ops (ephemeral keypair → SendSSHPublicKey → open-tunnel →
// ssh) so the rescue path inherits every fix to the EIC mechanism (e.g.
// PR #2822's LogLevel=ERROR shim) for free.
//
// Combined output (2>&1) is intentional: a boot-failed box's most
// useful signal is often on stderr (a panic, a missing-file error), and
// the rescue bundle is a forensic blob, not a parsed value — we want
// everything the command emitted.
func rescueRunRemoteViaEIC(ctx context.Context, instanceID, command string) (string, error) {
	var combined []byte
	runErr := withEICTunnel(ctx, instanceID, func(s eicSSHSession) error {
		sshCmd := exec.CommandContext(ctx, "ssh", s.sshArgs(command)...)
		sshCmd.Env = os.Environ()
		var buf bytes.Buffer
		sshCmd.Stdout = &buf
		sshCmd.Stderr = &buf
		// A non-zero remote exit is NOT a transport error for the rescue
		// path — each section command already falls back to an
		// `|| echo '(...)'` marker, so a clean exit is expected. Only
		// surface an error when ssh/tunnel itself failed AND produced no
		// output to ship.
		err := sshCmd.Run()
		combined = buf.Bytes()
		if err != nil && len(combined) == 0 {
			return fmt.Errorf("rescue ssh exec: %w", err)
		}
		return nil
	})
	if runErr != nil {
		return "", runErr
	}
	return strings.TrimRight(string(combined), "\n"), nil
}

// captureRescueBundle fires a best-effort, non-blocking rescue capture
// for a boot-failed workspace. It is the single entry point both
// boot-failure verdict paths funnel through.
//
// NON-BLOCKING: the actual collection runs in its own goroutine with
// its own timeout (rescue.CaptureTimeout), detached from the caller's
// request/sweep context so it can't add latency to — or be cancelled
// by — the failure-handling path that triggered it. We snapshot the
// identity into a fresh context.Background() for the same reason: a
// gin request context is cancelled the instant the HTTP handler
// returns, which would kill the EIC tunnel mid-collection.
//
// instanceID/orgID are resolved here (best-effort) so the two call
// sites only need the workspace id. A missing instance id → rescue.Capture
// no-ops (logged), so an early-failure workspace that never got an EC2
// is handled cleanly.
func captureRescueBundle(workspaceID, reason string) {
	rescueDispatch(func() {
		ctx := context.Background()
		instanceID, err := rescueResolveInstanceID(ctx, workspaceID)
		if err != nil {
			// Best-effort: a resolve failure is logged inside Capture's
			// caller chain; pass empty so Capture no-ops cleanly.
			instanceID = ""
		}
		rescue.Capture(ctx, rescue.Input{
			InstanceID:  instanceID,
			WorkspaceID: workspaceID,
			OrgID:       os.Getenv("MOLECULE_ORG_ID"),
			Reason:      reason,
		})
	})
}

// rescueDispatch runs the rescue collection off the request path. In
// production it's `go fn()` so the capture never blocks or adds latency
// to the boot-failure handler. Tests swap it for a synchronous runner so
// they can assert the capture fired (or didn't) deterministically
// without racing the goroutine.
var rescueDispatch = func(fn func()) { go fn() }

// BootFailureRescueHook is the registry-facing adapter wired into
// registry.BootFailureRescueHook from main.go. The registry sweeper
// already resolved the instance id (it's in the candidate row), so this
// path uses it directly rather than re-querying — symmetric with the
// captureRescueBundle handler path but skipping the lookup.
//
// Best-effort + non-blocking: dispatches the capture on its own
// goroutine with its own timeout, so the sweep loop is never slowed.
func BootFailureRescueHook(workspaceID, instanceID, reason string) {
	go rescue.Capture(context.Background(), rescue.Input{
		InstanceID:  instanceID,
		WorkspaceID: workspaceID,
		OrgID:       os.Getenv("MOLECULE_ORG_ID"),
		Reason:      reason,
	})
}

// rescueResolveInstanceID looks up the EC2 instance id for a workspace.
// Package var so tests can stub it without a sqlmock. Mirrors
// provisioner.resolveInstanceID (same query) but lives here to keep the
// rescue wiring self-contained and avoid widening the provisioner
// surface.
var rescueResolveInstanceID = func(ctx context.Context, workspaceID string) (string, error) {
	if db.DB == nil {
		return "", nil // nil in unit tests
	}
	var instanceID sql.NullString
	err := db.DB.QueryRowContext(ctx,
		`SELECT instance_id FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&instanceID)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if !instanceID.Valid {
		return "", nil
	}
	return instanceID.String, nil
}
