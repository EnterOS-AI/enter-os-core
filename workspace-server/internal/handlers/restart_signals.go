package handlers

// restart_signals.go — #125 Phase 1: graceful pre-restart drain for
// native-session workspaces.
//
// Before a container restart, the platform sends POST /signals/restart_pending
// to the workspace agent. The agent receives this as a JSON-RPC signal and
// begins draining in-flight work. The platform then waits for acknowledgment
// before calling stopForRestart.
//
// This preserves in-flight A2A requests that would otherwise be lost when
// the container dies mid-request (the core bug: native_session targets bypass
// the platform's a2a_queue buffering, so any message dispatched directly to
// the SDK session disappears when the container restarts).
//
// Phase 2 (not yet implemented): workspace SDK actually processes the signal
// and drains its message loop. This file implements the platform-side call
// site; the SDK-side handler is in molecule-workspace (adapter_base.py or
// similar).

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
)

const (
	// restartSignalTimeout is how long the platform waits for the workspace
	// to acknowledge the pre-restart signal. A workspace that doesn't implement
	// the handler will simply time out — the platform proceeds with the stop
	// anyway, which is the same as the pre-fix behaviour (no graceful drain).
	restartSignalTimeout = 10 * time.Second

	// restartSignalDrainDuration is how long the workspace should wait before
	// acknowledging. Gives in-flight A2A requests time to complete.
	// Sent as JSON-RPC signal.params.drain_seconds in the POST body.
	restartSignalDrainDuration = 20 * time.Second
)

// gracefulPreRestart sends the pre-restart drain signal to the workspace
// agent before the container is stopped. Called from runRestartCycle.
//
// Returns immediately — the signal is fire-and-forget with a 10s timeout.
// If the workspace doesn't implement the handler (404) or times out, the
// platform proceeds with the stop anyway (same as pre-fix behaviour).
//
// The signal is sent via HTTP POST to the workspace's internal agent URL.
// On self-hosted (platform-in-Docker), the platform rewrites 127.0.0.1 to
// the Docker-DNS form ws-<id>:8000. On SaaS/CP, the stored agent URL
// (an externally routable address) is used directly.
func (h *WorkspaceHandler) gracefulPreRestart(ctx context.Context, workspaceID string) {
	// Non-blocking send — don't stall the restart cycle.
	// Run in a detached goroutine so the caller (runRestartCycle) can
	// proceed to stopForRestart without waiting.
	go func() {
		signalCtx, cancel := context.WithTimeout(context.Background(), restartSignalTimeout)
		defer cancel()

		url, err := h.resolveAgentURLForRestartSignal(signalCtx, workspaceID)
		if err != nil {
			log.Printf("A2AGracefulRestart: resolve URL failed for %s: %v — proceeding with stop", workspaceID, err)
			return
		}
		url = url + "/signals/restart_pending"

		payload := map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "signals/restart_pending",
			"params": map[string]interface{}{
				"drain_seconds": int(restartSignalDrainDuration.Seconds()),
				"workspace_id":  workspaceID,
			},
			"id": nil,
		}
		body, _ := json.Marshal(payload)

		req, reqErr := http.NewRequestWithContext(signalCtx, http.MethodPost, url, bytes.NewReader(body))
		if reqErr != nil {
			log.Printf("A2AGracefulRestart: build request failed for %s: %v — proceeding with stop", workspaceID, reqErr)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		// X-Restart-Signal header identifies this as a platform-initiated
		// restart signal (not a regular A2A message). The SDK can check
		// for this header to distinguish a restart signal from other messages.
		req.Header.Set("X-Restart-Signal", "true")

		client := &http.Client{Timeout: restartSignalTimeout}
		resp, doErr := client.Do(req)
		if doErr != nil {
			// Timeout, connection refused, etc. — workspace is either not
			// listening or didn't implement the handler. Proceed with stop.
			log.Printf("A2AGracefulRestart: signal failed for %s: %v — proceeding with stop", workspaceID, doErr)
			return
		}
		defer resp.Body.Close()

		// 200 = workspace acknowledged and will drain. 404 = old SDK version
		// without the handler — same as no handler, proceed. 5xx = workspace
		// error but it's still alive — proceed. Any other status = also proceed.
		if resp.StatusCode == http.StatusOK {
			log.Printf("A2AGracefulRestart: %s acknowledged pre-restart signal (status=%d)", workspaceID, resp.StatusCode)
		} else {
			log.Printf("A2AGracefulRestart: %s returned status %d — proceeding with stop", workspaceID, resp.StatusCode)
		}
	}()
}

// resolveAgentURLForRestartSignal returns the routable URL for the workspace
// agent, suitable for the pre-restart signal HTTP call. Falls back to the DB
// value if the Redis cache miss occurs. On self-hosted (platform-in-Docker),
// rewrites 127.0.0.1 to the Docker-DNS form ws-<id>:8000.
func (h *WorkspaceHandler) resolveAgentURLForRestartSignal(ctx context.Context, workspaceID string) (string, error) {
	// Try Redis cache first.
	agentURL, err := db.GetCachedURL(ctx, workspaceID)
	if err == nil && agentURL != "" {
		return rewriteForDocker(agentURL, workspaceID), nil
	}

	// Cache miss — fall back to DB.
	var urlNullable *string
	err = db.DB.QueryRowContext(ctx,
		`SELECT url FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&urlNullable)
	if err != nil {
		return "", err
	}
	if urlNullable == nil || *urlNullable == "" {
		return "", nil // workspace has no URL yet — shouldn't happen at restart time
	}
	agentURL = *urlNullable
	_ = db.CacheURL(ctx, workspaceID, agentURL)
	return rewriteForDocker(agentURL, workspaceID), nil
}

// rewriteForDocker rewrites a 127.0.0.1 agent URL to the Docker-DNS form
// when the platform is running inside a Docker container. When platform is
// on the host (non-Docker), 127.0.0.1 IS the host and the original URL works.
func rewriteForDocker(agentURL, workspaceID string) string {
	if platformInDocker && h.provisioner != nil {
		// Only rewrite if the URL points to localhost (the ephemeral port
		// binding the container published to the host). Internal Docker
		// URLs (e.g. http://ws-abc123def:8000) are already correct.
		if len(agentURL) >= 17 && agentURL[:16] == "http://127.0.0.1" {
			return provisioner.InternalURL(workspaceID)
		}
	}
	return agentURL
}
