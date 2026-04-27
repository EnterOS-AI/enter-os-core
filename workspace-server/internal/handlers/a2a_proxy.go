package handlers

// a2a_proxy.go — A2A JSON-RPC proxy: routes canvas and agent-to-agent
// requests to workspace containers. Core proxy path, URL resolution,
// payload normalization, and HTTP dispatch. Error handling, logging, and
// SSRF helpers live in a2a_proxy_helpers.go.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/registry"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// platformInDocker caches whether THIS process is running inside a
// Docker container. The a2a proxy uses this to decide whether stored
// agent URLs like "http://127.0.0.1:<ephemeral>" need to be rewritten
// to the Docker-DNS form "http://ws-<id>:8000". When the platform is
// on the host, 127.0.0.1 IS the host and the ephemeral-port URL works
// as-is; rewriting to container DNS would then break (host can't
// resolve Docker bridge hostnames).
//
// Detection: /.dockerenv is the canonical marker inside the default
// Docker runtime. MOLECULE_IN_DOCKER is an explicit override for
// environments where /.dockerenv is absent (Podman, custom runtimes).
// Accepts any value strconv.ParseBool recognises — 1, 0, t, f, T, F,
// true, false, TRUE, FALSE, True, False. Anything else (including
// "yes"/"on") is treated as unset and falls through to the /.dockerenv
// check.
//
// Exposed as a var (not a const) so tests can toggle it via
// setPlatformInDockerForTest without fiddling with real filesystem
// markers or env vars. Production callers never mutate it.
var platformInDocker = detectPlatformInDocker()

func detectPlatformInDocker() bool {
	if v := os.Getenv("MOLECULE_IN_DOCKER"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

// setPlatformInDockerForTest overrides platformInDocker for the duration of
// a test and returns a function to restore the previous value. Use with
// defer in *_test.go only.
func setPlatformInDockerForTest(v bool) func() {
	prev := platformInDocker
	platformInDocker = v
	return func() { platformInDocker = prev }
}

// maxProxyRequestBody is the maximum size of an A2A proxy request body (1MB).
const maxProxyRequestBody = 1 << 20

// systemCallerPrefixes are caller IDs that bypass workspace access control.
// These are non-workspace internal callers (webhooks, system services, tests).
var systemCallerPrefixes = []string{"webhook:", "system:", "test:", "channel:"}

// isSystemCaller returns true if callerID is a non-workspace internal caller.
func isSystemCaller(callerID string) bool {
	for _, prefix := range systemCallerPrefixes {
		if strings.HasPrefix(callerID, prefix) {
			return true
		}
	}
	return false
}

// maxProxyResponseBody is the maximum size of an A2A proxy response body (10MB).
const maxProxyResponseBody = 10 << 20

// a2aClient is a shared HTTP client for proxying A2A requests to workspace agents.
// No client-level timeout — timeouts are enforced per-request via context
// deadlines: canvas = 5 min (Rule 3), agent-to-agent = 30 min (DoS cap). Do NOT
// set a Client.Timeout here: it is enforced independently of ctx deadlines and
// would pre-empt legitimate slow cold-start flows (e.g. Claude Code first-token
// over OAuth can take 30-60s on boot). Callers that want a safety net should
// build a context.WithTimeout themselves.
var a2aClient = &http.Client{}

type proxyA2AError struct {
	Status   int
	Response gin.H
	// Optional response headers (e.g. Retry-After on 503-busy). Kept separate
	// from Response so the handler can set real HTTP headers, not just JSON.
	Headers map[string]string
}

// busyRetryAfterSeconds is the Retry-After hint returned with 503-busy
// responses when an upstream workspace agent is overloaded (single-threaded
// mid-synthesis). Chosen to be long enough for typical PM synthesis work
// to complete but short enough that a caller's retry loop won't stall
// coordination. See issue #110.
const busyRetryAfterSeconds = 30

// isUpstreamBusyError classifies an http.Client.Do error as a transient
// "upstream busy" condition — a timeout or connection-reset while the
// container is still alive. Distinguishes legitimate busy-agent failures
// from fatal network errors so callers can retry with Retry-After.
func isUpstreamBusyError(err error) bool {
	if err == nil {
		return false
	}
	// Typed sentinels propagate cleanly through *url.Error.Unwrap
	// since Go 1.13, so errors.Is is the primary check for both
	// DeadlineExceeded and Canceled. The substring fallbacks below
	// stay only for shapes net/http does NOT type — bare "EOF" /
	// "connection reset" can arrive as plain *net.OpError with no
	// errors.Is hook to the stdlib sentinels.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// applyIdleTimeout uses context.WithCancel; surfaces here as
	// Canceled, distinct from DeadlineExceeded but the same "upstream
	// busy" class — caller produces a 503 + Retry-After.
	if errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "connection reset")
}

func (e *proxyA2AError) Error() string {
	if e == nil || e.Response == nil {
		return "proxy a2a error"
	}
	if msg, ok := e.Response["error"].(string); ok && msg != "" {
		return msg
	}
	return "proxy a2a error"
}

// ProxyA2ARequest is the public wrapper for proxyA2ARequest, used by the
// cron scheduler and other internal callers that need to send A2A messages
// to workspaces programmatically (not from an HTTP handler).
func (h *WorkspaceHandler) ProxyA2ARequest(ctx context.Context, workspaceID string, body []byte, callerID string, logActivity bool) (int, []byte, error) {
	status, resp, proxyErr := h.proxyA2ARequest(ctx, workspaceID, body, callerID, logActivity)
	if proxyErr != nil {
		return status, resp, proxyErr
	}
	return status, resp, nil
}

// ProxyA2A handles POST /workspaces/:id/a2a
// Proxies A2A JSON-RPC requests from the canvas to workspace agents,
// avoiding CORS and Docker network issues.
func (h *WorkspaceHandler) ProxyA2A(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// X-Timeout: caller-specified timeout in seconds (0 = no timeout).
	// Overrides the default canvas (5 min) / agent (30 min) timeouts.
	if tStr := c.GetHeader("X-Timeout"); tStr != "" {
		if tSec, err := strconv.Atoi(tStr); err == nil && tSec > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(tSec)*time.Second)
			defer cancel()
		}
		// tSec == 0 means no timeout — use the raw context (no deadline)
	}

	// Read the incoming request body (capped at 1MB)
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxProxyRequestBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	callerID := c.GetHeader("X-Workspace-ID")

	// #761 SECURITY: reject requests where the client-supplied X-Workspace-ID
	// contains a system-caller prefix. isSystemCaller() bypasses both token
	// validation and CanCommunicate. On the public /a2a endpoint, system-caller
	// semantics only apply to callerIDs set by trusted server-side code
	// (ProxyA2ARequest), never to HTTP header values. Legitimate system callers
	// (webhooks, scheduler, restart_context) call proxyA2ARequest directly and
	// never go through this HTTP handler.
	if isSystemCaller(callerID) {
		log.Printf("security: system-caller prefix forge attempt — remote=%q header=%q",
			c.ClientIP(), callerID)
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid caller ID"})
		return
	}

	// Phase 30.5 — validate the caller's auth token when the caller IS
	// a workspace (not canvas or a system caller). Canvas requests have
	// no X-Workspace-ID so they bypass this check (the existing
	// access-control layer already trusts them). System callers
	// (webhook:* / system:* / test:*) only reach proxyA2ARequest via
	// the server-side ProxyA2ARequest wrapper, never via this HTTP path.
	//
	// The bind is strict: the token must match `callerID`, not
	// `workspaceID` (the target). A compromised token from workspace A
	// must never authenticate calls from A pretending to be B.
	if callerID != "" && callerID != workspaceID {
		if err := validateCallerToken(ctx, c, callerID); err != nil {
			return // response already written with 401
		}
	}

	status, respBody, proxyErr := h.proxyA2ARequest(ctx, workspaceID, body, callerID, true)
	if proxyErr != nil {
		for k, v := range proxyErr.Headers {
			c.Header(k, v)
		}
		c.JSON(proxyErr.Status, proxyErr.Response)
		return
	}

	c.Data(status, "application/json", respBody)
}

// checkWorkspaceBudget returns a proxyA2AError with 402 when the workspace
// has a budget_limit set and monthly_spend has reached or exceeded it.
// DB errors are logged and treated as fail-open — a budget check failure
// must not block legitimate A2A traffic.
func (h *WorkspaceHandler) checkWorkspaceBudget(ctx context.Context, workspaceID string) *proxyA2AError {
	var budgetLimit sql.NullInt64
	var monthlySpend int64
	err := db.DB.QueryRowContext(ctx,
		`SELECT budget_limit, COALESCE(monthly_spend, 0) FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&budgetLimit, &monthlySpend)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("ProxyA2A: budget check failed for %s: %v", workspaceID, err)
		}
		return nil // fail-open
	}
	if budgetLimit.Valid && monthlySpend >= budgetLimit.Int64 {
		log.Printf("ProxyA2A: budget exceeded for %s (spend=%d limit=%d)", workspaceID, monthlySpend, budgetLimit.Int64)
		return &proxyA2AError{
			Status:   http.StatusPaymentRequired,
			Response: gin.H{"error": "workspace budget limit exceeded"},
		}
	}
	return nil
}

func (h *WorkspaceHandler) proxyA2ARequest(ctx context.Context, workspaceID string, body []byte, callerID string, logActivity bool) (int, []byte, *proxyA2AError) {
	// Access control: workspace-to-workspace requests must pass CanCommunicate check.
	// Canvas requests (callerID == "") and system callers (webhook:*, system:*, test:*)
	// are trusted. Self-calls (callerID == workspaceID) are always allowed.
	if callerID != "" && callerID != workspaceID && !isSystemCaller(callerID) {
		if !registry.CanCommunicate(callerID, workspaceID) {
			log.Printf("ProxyA2A: access denied %s → %s", callerID, workspaceID)
			return 0, nil, &proxyA2AError{
				Status:   http.StatusForbidden,
				Response: gin.H{"error": "access denied: workspaces cannot communicate per hierarchy rules"},
			}
		}
	}

	// Budget enforcement: reject A2A calls when the workspace has exceeded its
	// monthly spend ceiling. Checked after access control so unauthorized calls
	// are rejected first (403 > 429 in the denial hierarchy). Fail-open on DB
	// errors so a budget check failure never blocks legitimate traffic.
	if proxyErr := h.checkWorkspaceBudget(ctx, workspaceID); proxyErr != nil {
		return 0, nil, proxyErr
	}

	agentURL, proxyErr := h.resolveAgentURL(ctx, workspaceID)
	if proxyErr != nil {
		return 0, nil, proxyErr
	}

	normalizedBody, a2aMethod, proxyErr := normalizeA2APayload(body)
	if proxyErr != nil {
		return 0, nil, proxyErr
	}
	body = normalizedBody

	startTime := time.Now()
	resp, cancelFwd, err := h.dispatchA2A(ctx, workspaceID, agentURL, body, callerID)
	if cancelFwd != nil {
		defer cancelFwd()
	}
	durationMs := int(time.Since(startTime).Milliseconds())
	if err != nil {
		return h.handleA2ADispatchError(ctx, workspaceID, callerID, body, a2aMethod, err, durationMs, logActivity)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read agent response (capped at 10MB).
	// #689: Do() succeeded, which means the target received the request and sent
	// back response headers — delivery is confirmed. The body couldn't be
	// fully read (connection drop, timeout mid-stream). Surface
	// delivery_confirmed so callers can distinguish "not delivered" from
	// "delivered, but response body lost". When delivery is confirmed,
	// log the activity as successful (delivery happened) rather than leaving
	// a false "failed" entry in the audit trail.
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxProxyResponseBody))
	if readErr != nil {
		deliveryConfirmed := resp.StatusCode >= 200 && resp.StatusCode < 400
		log.Printf("ProxyA2A: body read failed for %s (status=%d delivery_confirmed=%v bytes_read=%d): %v",
			workspaceID, resp.StatusCode, deliveryConfirmed, len(respBody), readErr)
		if logActivity && deliveryConfirmed {
			h.logA2ASuccess(ctx, workspaceID, callerID, body, respBody, a2aMethod, resp.StatusCode, durationMs)
		}
		return 0, nil, &proxyA2AError{
			Status: http.StatusBadGateway,
			Response: gin.H{
				"error":              "failed to read agent response",
				"delivery_confirmed": deliveryConfirmed,
			},
		}
	}

	if logActivity {
		h.logA2ASuccess(ctx, workspaceID, callerID, body, respBody, a2aMethod, resp.StatusCode, durationMs)
	}

	// Track LLM token usage for cost transparency (#593).
	// Fires in a detached goroutine so token accounting never adds latency
	// to the critical A2A path.
	go extractAndUpsertTokenUsage(context.WithoutCancel(ctx), workspaceID, respBody)

	// Non-2xx agent response: the agent received the request but returned an
	// error status. Return a proxyErr so the caller (DrainQueueForWorkspace)
	// can call MarkQueueItemFailed rather than silently marking completed.
	// 3xx is also treated as failure here (A2A does not follow redirects).
	// Extract a meaningful error from the response body if present.
	if resp.StatusCode >= 300 {
		errMsg := ""
		if len(respBody) > 0 {
			var top map[string]json.RawMessage
			if json.Unmarshal(respBody, &top) == nil {
				if e, ok := top["error"]; ok {
					// Prefer string errors from the agent's JSON body.
					// e is json.RawMessage ([]byte); try to unmarshal as string.
					var errStr string
					if json.Unmarshal(e, &errStr) == nil {
						errMsg = errStr
					}
				}
			}
		}
		if errMsg == "" {
			errMsg = http.StatusText(resp.StatusCode)
		}
		return resp.StatusCode, respBody, &proxyA2AError{
			Status:   resp.StatusCode,
			Response: gin.H{"error": errMsg},
		}
	}

	return resp.StatusCode, respBody, nil
}

// resolveAgentURL returns a routable URL for the target workspace agent. It
// checks the Redis URL cache first, then falls back to a DB lookup, caching
// the result on success. When the platform runs inside Docker, 127.0.0.1:<host
// port> is rewritten to the container's Docker-bridge hostname (host-side
// platforms keep the original URL because the bridge name wouldn't resolve).
func (h *WorkspaceHandler) resolveAgentURL(ctx context.Context, workspaceID string) (string, *proxyA2AError) {
	agentURL, err := db.GetCachedURL(ctx, workspaceID)
	if err != nil {
		var urlNullable sql.NullString
		var status string
		err := db.DB.QueryRowContext(ctx,
			`SELECT url, status FROM workspaces WHERE id = $1`, workspaceID,
		).Scan(&urlNullable, &status)
		if err == sql.ErrNoRows {
			return "", &proxyA2AError{
				Status:   http.StatusNotFound,
				Response: gin.H{"error": "workspace not found"},
			}
		}
		if err != nil {
			log.Printf("ProxyA2A lookup error: %v", err)
			return "", &proxyA2AError{
				Status:   http.StatusInternalServerError,
				Response: gin.H{"error": "lookup failed"},
			}
		}
		if !urlNullable.Valid || urlNullable.String == "" {
			// Auto-wake hibernated workspace on incoming A2A message (#711).
			// Re-provision asynchronously and return 503 with a retry hint so
			// the caller can retry once the workspace is back online (~10s).
			if status == "hibernated" {
				log.Printf("ProxyA2A: waking hibernated workspace %s", workspaceID)
				go h.RestartByID(workspaceID)
				return "", &proxyA2AError{
					Status:  http.StatusServiceUnavailable,
					Headers: map[string]string{"Retry-After": "15"},
					Response: gin.H{
						"error":       "workspace is waking from hibernation — retry in ~15 seconds",
						"waking":      true,
						"retry_after": 15,
					},
				}
			}
			return "", &proxyA2AError{
				Status:   http.StatusServiceUnavailable,
				Response: gin.H{"error": "workspace has no URL", "status": status},
			}
		}
		agentURL = urlNullable.String
		_ = db.CacheURL(ctx, workspaceID, agentURL)
	}

	// When the platform runs inside Docker, 127.0.0.1:{host_port} is
	// unreachable (it's the platform container's own localhost, not the
	// Docker host). Rewrite to the container's Docker-bridge hostname.
	if strings.HasPrefix(agentURL, "http://127.0.0.1:") && h.provisioner != nil && platformInDocker {
		agentURL = provisioner.InternalURL(workspaceID)
	}
	// SSRF defence: reject private/metadata URLs before making outbound call.
	if err := isSafeURL(agentURL); err != nil {
		log.Printf("ProxyA2A: unsafe URL for workspace %s: %v", workspaceID, err)
		return "", &proxyA2AError{
			Status:   http.StatusBadGateway,
			Response: gin.H{"error": "workspace URL is not publicly routable"},
		}
	}
	return agentURL, nil
}

// normalizeA2APayload parses the incoming body, wraps it in a JSON-RPC 2.0
// envelope if absent, ensures params.message.messageId is set, and re-marshals
// to bytes. Also returns the A2A method name (for logging) extracted from the
// payload.
func normalizeA2APayload(body []byte) ([]byte, string, *proxyA2AError) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", &proxyA2AError{
			Status:   http.StatusBadRequest,
			Response: gin.H{"error": "invalid JSON"},
		}
	}

	// Wrap in JSON-RPC envelope if missing
	if _, hasJSONRPC := payload["jsonrpc"]; !hasJSONRPC {
		payload = map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      uuid.New().String(),
			"method":  payload["method"],
			"params":  payload["params"],
		}
	}

	// Ensure params.message.messageId exists (required by a2a-sdk)
	if params, ok := payload["params"].(map[string]interface{}); ok {
		if msg, ok := params["message"].(map[string]interface{}); ok {
			if _, hasID := msg["messageId"]; !hasID {
				msg["messageId"] = uuid.New().String()
			}
		}
	}

	marshaledBody, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return nil, "", &proxyA2AError{
			Status:   http.StatusInternalServerError,
			Response: gin.H{"error": "failed to marshal request"},
		}
	}

	var a2aMethod string
	if m, ok := payload["method"].(string); ok {
		a2aMethod = m
	}
	return marshaledBody, a2aMethod, nil
}

// idleTimeoutDuration is the per-dispatch silence window: if the
// platform's broadcaster emits no events for this workspace for the
// full duration, the dispatch ctx is cancelled. Resets on every
// broadcaster event for the workspace — including the WORKSPACE_HEARTBEAT
// fired by the registry's /heartbeat handler every 30s, so a runtime
// that's just thinking silently between tool calls keeps the connection
// alive without having to emit ACTIVITY_LOGGED noise.
//
// Pre-2026-04-26 this was 60s, picked when the platform only broadcast
// on TASK_UPDATED (which itself only fires when current_task CHANGES).
// A claude-code agent doing a long packaging step or a slow model thought
// kept the same current_task for >60s, fired no broadcast, got cancelled
// mid-flight. Bumped to 5min as a safety net AND the heartbeat handler
// now broadcasts unconditionally — together either one alone closes the
// gap, both together is defence in depth.
//
// Override via A2A_IDLE_TIMEOUT_SECONDS for ops who want to tune (e.g.
// shorter for canary/test runners that want fail-fast on wedge, longer
// for prod tenants running unusually slow plugins).
var idleTimeoutDuration = parseIdleTimeoutEnv(os.Getenv("A2A_IDLE_TIMEOUT_SECONDS"))

// defaultIdleTimeoutDuration is what parseIdleTimeoutEnv returns when
// the env var is unset or invalid. Pulled out as a const so tests can
// reference it without re-deriving the value.
const defaultIdleTimeoutDuration = 5 * time.Minute

// parseIdleTimeoutEnv parses the A2A_IDLE_TIMEOUT_SECONDS value, falling
// back to defaultIdleTimeoutDuration on empty / non-numeric / non-positive
// input. Bad-input cases LOG so an operator who set the wrong value
// doesn't silently get the default and waste hours debugging "why is my
// override not working." Without the log line, A2A_IDLE_TIMEOUT_SECONDS=foo
// or =-30 produces identical observable behaviour to leaving it unset.
func parseIdleTimeoutEnv(v string) time.Duration {
	if v == "" {
		return defaultIdleTimeoutDuration
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("A2A_IDLE_TIMEOUT_SECONDS=%q is not a valid integer; using default %s", v, defaultIdleTimeoutDuration)
		return defaultIdleTimeoutDuration
	}
	if n <= 0 {
		log.Printf("A2A_IDLE_TIMEOUT_SECONDS=%d must be > 0; using default %s", n, defaultIdleTimeoutDuration)
		return defaultIdleTimeoutDuration
	}
	return time.Duration(n) * time.Second
}

// dispatchA2A POSTs `body` to `agentURL`. Uses WithoutCancel so delegation
// chains survive client disconnect (browser tab close). Two layers of
// timeout per dispatch:
//
//   - Idle timeout (always applied): cancels the dispatch when no
//     broadcaster events for the workspace fire for
//     idleTimeoutDuration. Any progress event resets the clock — so
//     a long but actively-streaming reply runs forever, while a
//     wedged runtime fails fast.
//   - Absolute ceiling (agent-to-agent only): 30 min cap as a
//     defence against runaway delegation loops. Canvas dispatches
//     have no absolute ceiling — the user can wait as long as they
//     want, the idle timer is the only hangup signal.
//
// Either layer is overridable by the X-Timeout header upstream in
// ProxyA2A; X-Timeout: 0 explicitly disables the absolute ceiling.
func (h *WorkspaceHandler) dispatchA2A(ctx context.Context, workspaceID, agentURL string, body []byte, callerID string) (*http.Response, context.CancelFunc, error) {
	// #1483 SSRF defense-in-depth: the primary call path through
	// proxyA2ARequest → resolveAgentURL already validates via isSafeURL
	// (a2a_proxy.go:424), but adding the check here closes the gap for
	// any future code path that calls dispatchA2A directly without
	// going through resolveAgentURL. Wrapping as proxyDispatchBuildError
	// keeps the caller's error-classification path unchanged — the same
	// shape it already produces a 500 for.
	if err := isSafeURL(agentURL); err != nil {
		return nil, nil, &proxyDispatchBuildError{err: err}
	}
	forwardCtx := context.WithoutCancel(ctx)
	var ceilingCancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		if callerID != "" {
			forwardCtx, ceilingCancel = context.WithTimeout(forwardCtx, 30*time.Minute)
		}
		// callerID == "" (canvas): no absolute ceiling. The idle
		// timeout below is the only deadline.
	}
	// Idle timeout — cancels the dispatch ctx after
	// idleTimeoutDuration of broadcaster silence for this workspace.
	// Always applied (canvas + agent-to-agent both benefit; the
	// ceiling above is a separate runaway-loop cap that only fires
	// for agent traffic). Combines with the ceiling cancel into a
	// single returned cancel func that the caller defers.
	// applyIdleTimeout needs SubscribeSSE which only lives on the
	// concrete *Broadcaster, not on the EventEmitter interface the
	// handler now stores. Type-assert + fall through to a no-op idle
	// timer if the broadcaster doesn't support subscriptions (the
	// EventEmitter mock used by some tests, e.g.). Production wires
	// the concrete *Broadcaster, so the assertion always succeeds in
	// real deploys.
	var b *events.Broadcaster
	if concrete, ok := h.broadcaster.(*events.Broadcaster); ok {
		b = concrete
	}
	// Per-workspace idle-timeout override (capability primitive #2 —
	// see workspace/adapter_base.py:idle_timeout_override). The
	// adapter declares a longer/shorter window than the platform
	// default in its heartbeat; the heartbeat handler stashes it in
	// runtimeOverrides; we honor it here. Falls through to the global
	// default (env A2A_IDLE_TIMEOUT_SECONDS, default 5min) when no
	// override is registered for this workspace.
	idle := idleTimeoutDuration
	if perWorkspace, ok := runtimeOverrides.IdleTimeout(workspaceID); ok {
		idle = perWorkspace
	}
	forwardCtx, idleCancel := applyIdleTimeout(forwardCtx, b, workspaceID, idle)
	cancel := func() {
		idleCancel()
		if ceilingCancel != nil {
			ceilingCancel()
		}
	}
	req, err := http.NewRequestWithContext(forwardCtx, "POST", agentURL, bytes.NewReader(body))
	if err != nil {
		cancel()
		// Wrap the construction failure so the caller can distinguish it
		// from an upstream Do() error and produce the correct 500 response.
		return nil, nil, &proxyDispatchBuildError{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, doErr := a2aClient.Do(req)
	return resp, cancel, doErr
}

// applyIdleTimeout returns a child ctx that gets cancelled when no
// broadcaster events for `workspaceID` arrive for `idle` duration.
// Any incoming event resets the clock. The returned cancel func
// MUST be called to clean up the goroutine + subscription.
//
// nil broadcaster or non-positive idle returns the parent ctx
// unchanged (and a no-op cancel) so test paths that don't wire a
// broadcaster keep working.
func applyIdleTimeout(parent context.Context, b *events.Broadcaster, workspaceID string, idle time.Duration) (context.Context, context.CancelFunc) {
	if b == nil || idle <= 0 || workspaceID == "" {
		return parent, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	sub, unsub := b.SubscribeSSE(workspaceID)
	go func() {
		defer unsub()
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-sub:
				if !ok {
					// Subscription channel closed — fall back to
					// pure-timer mode. Don't cancel: another caller
					// may have closed our sub but the request itself
					// is still in flight. Let the timer or the
					// caller's defer drive cleanup.
					continue
				}
				// Stop+drain pattern so a fired-but-unread timer
				// doesn't double-cancel after the Reset.
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
			case <-timer.C:
				cancel()
				return
			}
		}
	}()
	return ctx, cancel
}
