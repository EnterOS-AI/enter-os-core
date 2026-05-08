package handlers

// eic_tunnel_pool_setup.go — production setup shim.
//
// setupRealEICTunnel decomposes the existing realWithEICTunnel into
// its slow half (build the tunnel) and its caller half (run fn). The
// pool calls the slow half once and shares the resulting session
// across N callers, holding cleanup until the last release.
//
// Why decompose instead of refactoring realWithEICTunnel: the
// existing function and its test stub-vars (withEICTunnel,
// sendSSHPublicKey, openTunnelCmd) are load-bearing for the
// dispatch tests. Extracting a sibling setup function preserves the
// existing single-shot path verbatim — the pool wraps it by calling
// realWithEICTunnel through a thin adapter, leaving the tested
// surface unchanged.
//
// The pool's acquire() invokes poolSetupTunnel, which is a `var`
// pointing to setupRealEICTunnel for production and a counting stub
// for tests.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// setupRealEICTunnel is the slow path that the pool consumes when
// no warm entry exists. Mirrors realWithEICTunnel's setup half but
// returns the session + cleanup instead of running fn inline.
//
// The cleanup func owns the tunnel subprocess, ephemeral key dir,
// and a one-time wait. Idempotent — calling it twice is safe; the
// pool guarantees one call per session, but defence-in-depth helps
// when tests run pools in parallel and racy sweeps re-trigger.
func setupRealEICTunnel(ctx context.Context, instanceID string) (
	eicSSHSession, func(), error) {

	if instanceID == "" {
		return eicSSHSession{}, nil,
			fmt.Errorf("workspace has no instance_id — not a SaaS EC2 workspace")
	}
	osUser := os.Getenv("WORKSPACE_EC2_OS_USER")
	if osUser == "" {
		osUser = "ubuntu"
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-2"
	}

	keyDir, err := os.MkdirTemp("", "molecule-eic-pool-*")
	if err != nil {
		return eicSSHSession{}, nil, fmt.Errorf("keydir mkdir: %w", err)
	}
	keyPath := keyDir + "/id"
	if out, kerr := exec.CommandContext(ctx, "ssh-keygen",
		"-t", "ed25519", "-f", keyPath, "-N", "", "-q",
		"-C", "molecule-eic-pool",
	).CombinedOutput(); kerr != nil {
		_ = os.RemoveAll(keyDir)
		return eicSSHSession{}, nil,
			fmt.Errorf("ssh-keygen: %w (%s)", kerr, strings.TrimSpace(string(out)))
	}
	pubKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		_ = os.RemoveAll(keyDir)
		return eicSSHSession{}, nil, fmt.Errorf("read pubkey: %w", err)
	}

	if err := sendSSHPublicKey(ctx, region, instanceID, osUser,
		strings.TrimSpace(string(pubKey))); err != nil {
		_ = os.RemoveAll(keyDir)
		return eicSSHSession{}, nil, fmt.Errorf("send-ssh-public-key: %w", err)
	}

	localPort, err := pickFreePort()
	if err != nil {
		_ = os.RemoveAll(keyDir)
		return eicSSHSession{}, nil, fmt.Errorf("pick free port: %w", err)
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
		_ = os.RemoveAll(keyDir)
		return eicSSHSession{}, nil, fmt.Errorf("open-tunnel start: %w", err)
	}

	if err := waitForPort(ctx, "127.0.0.1", localPort, 10*time.Second); err != nil {
		if tunnel.Process != nil {
			_ = tunnel.Process.Kill()
		}
		_ = tunnel.Wait()
		_ = os.RemoveAll(keyDir)
		return eicSSHSession{}, nil, fmt.Errorf("tunnel never listened: %w", err)
	}

	cleanedUp := false
	cleanup := func() {
		if cleanedUp {
			return
		}
		cleanedUp = true
		if tunnel.Process != nil {
			_ = tunnel.Process.Kill()
		}
		_ = tunnel.Wait()
		_ = os.RemoveAll(keyDir)
	}

	return eicSSHSession{
		keyPath:    keyPath,
		localPort:  localPort,
		osUser:     osUser,
		instanceID: instanceID,
	}, cleanup, nil
}

// init wires the pool into the package-level withEICTunnel var so
// every read/write/list/delete EIC op uses pooled tunnels by default.
// Test files that need single-shot behaviour can swap withEICTunnel
// back via the existing stubWithEICTunnel pattern, OR set poolTTL=0
// to disable pooling without rebinding the var.
func init() {
	initEICTunnelPool()
}
