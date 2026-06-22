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
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/envx"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/registry"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
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

// maxProxyRequestBody is the maximum size of an A2A proxy request body (16MB).
const maxProxyRequestBody = 16 << 20

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

// maxProxyResponseBody is the maximum size of an A2A proxy response body (64MB).
const maxProxyResponseBody = 64 << 20

// errA2ABodyTooLarge is returned by readBodyWithLimit when a body exceeds the
// configured limit. Callers surface it as a loud 413 / truncated proxy error
// instead of silently cutting the payload.
var errA2ABodyTooLarge = errors.New("A2A body exceeds size limit")

// readBodyWithLimit reads up to limit bytes from r. It returns an error
// (wrapping errA2ABodyTooLarge) when the input is larger than limit so the
// caller can fail loud instead of silently truncating. The returned body is
// capped at limit bytes; on truncation it contains the first limit bytes read.
func readBodyWithLimit(r io.Reader, limit int, kind string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return body, err
	}
	if len(body) > limit {
		return body[:limit], fmt.Errorf("%s body exceeds %d byte limit: %w", kind, limit, errA2ABodyTooLarge)
	}
	return body, nil
}

// a2aClient is a shared HTTP client for proxying A2A requests to workspace agents.
//
// Timeout model — three independent budgets, none of which gets in each other's way:
//
//  1. Client.Timeout — DELIBERATELY UNSET. Client.Timeout is a hard wall on
//     the entire request including streamed body reads, and would pre-empt
//     legitimate slow cold-start flows (Claude Code first-token over OAuth
//     can take 30-60s on boot; long-running agent synthesis can stream
//     tokens for minutes). Total-request budget is enforced per-request
//     via context deadline (canvas = idle-only, agent-to-agent = 30 min ceiling).
//
//  2. Transport.DialContext — 10s connect timeout. When a workspace's EC2
//     black-holes TCP connects (instance terminated mid-flight, security group
//     flipped, NACL bug), the OS default is 75s on Linux / 21s on macOS — long
//     enough that Cloudflare's ~100s edge timeout can fire first and surface
//     a generic 502 page to canvas. 10s is well above realistic intra-region
//     latencies and well below CF's edge timeout.
//
//  3. Transport.ResponseHeaderTimeout — 30min default. From request-body-end
//     to response-headers-start. Configurable via
//     A2A_PROXY_RESPONSE_HEADER_TIMEOUT (envx.Duration). Covers cold-start
//     first-byte (30-60s OAuth flow above) AND long SYNCHRONOUS autonomous
//     turns where the runtime computes the whole response before sending
//     headers (it does NOT always stream a 200 early). A real "migrate from
//     blob" turn ran 443s and was killed at the old 5min default with
//     `timeout awaiting response headers` (core#2723 class — the SAME long-turn
//     cut as the idle watchdog). Aligned to 30min = the agent-to-agent
//     ceiling + the canvas idle default, so no LEGIT turn (none exceed that)
//     trips it; a genuinely-stuck agent is surfaced by DialContext (fast,
//     connection-level) + the reactive-health/heartbeat path, not by cutting
//     a working turn short. Body streaming after headers is governed by the
//     per-request context deadline, NOT this timeout.
//
// The point of (2) and (3) is to surface a *structured* 503 from
// handleA2ADispatchError when the workspace agent is unreachable, so canvas
// gets `{"error":"workspace agent unreachable","restarting":true}` instead
// of Cloudflare's opaque 502 error page. Without these, dead workspaces hang
// long enough that CF gives up first and shows its own page.
var a2aClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: envx.Duration("A2A_PROXY_RESPONSE_HEADER_TIMEOUT", 30*time.Minute),
		TLSHandshakeTimeout:   10 * time.Second,
		// MaxIdleConns / IdleConnTimeout: stdlib defaults are fine; agent
		// fan-in is bounded by the platform's broadcaster fan-out, not by
		// connection-pool sizing.
	},
}

type proxyA2AError struct {
	Status   int
	Response gin.H
	// Optional response headers (e.g. Retry-After on 503-busy). Kept separate
	// from Response so the handler can set real HTTP headers, not just JSON.
	Headers map[string]string
	// Classification lets callers and monitoring distinguish the distinct
	// failure modes the proxy used to collapse into a single opaque
	// "proxy a2a error" string. Possible values:
	//   - "" (uncategorized; backward-compatible default — most existing call
	//     sites that did not set this field pre-fix)
	//   - "busy_retryable" — agent is mid-turn; safe to retry with Retry-After.
	//     The target is alive and the message was likely delivered / is being
	//     processed. Monitoring MUST NOT count this as a failure.
	//   - "delivered" — 2xx response was received, the error is a post-response
	//     transport blip (e.g. connection reset after the agent wrote the body).
	//     The agent completed the work; the delegation should be marked
	//     completed/success, not failed. Monitoring MUST NOT count this as a
	//     failure.
	//   - "upstream_dead" — dead-origin status family (502/503-restarting/504
	//     plus CF 521/522/523/524). Triggers reactive container restart.
	//     Genuine failure; counts as a delegation failure.
	//
	// Per the 2026-06-19 a2a RCA (#3056): the previous opaque
	// "proxy a2a error" string forced monitoring to read the same string for
	// transient backpressure, post-response blips, AND dead containers, so a
	// single-threaded busy spike looked like a fleet outage. With this field,
	// PM/monitoring consume the classification, not the string.
	Classification string
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

// isUpstreamDeadStatus returns true when the upstream HTTP status indicates
// the workspace agent is unreachable / unresponsive at the network layer
// (vs an agent-authored 5xx with a real body). Used by the proxy to gate
// reactive container-dead detection + auto-restart.
//
//   - 502 Bad Gateway, 503 Service Unavailable, 504 Gateway Timeout: standard
//     proxy-layer "upstream is broken" codes (Cloudflare, ELB, agent tunnel).
//   - 521 Web Server Is Down: Cloudflare can't open TCP to origin (most
//     direct dead-EC2 signal).
//   - 522 Connection Timed Out: Cloudflare opened TCP but no response within
//     ~15s — typical of SG/NACL flap or agent process hung.
//   - 523 Origin Is Unreachable: Cloudflare can't route to origin (DNS or
//     network-path failure).
//   - 524 A Timeout Occurred: TCP succeeded, but origin didn't return
//     headers within ~100s — agent process alive but wedged.
//
// We always probe IsRunning before acting, so a transient false positive
// from this set just costs one CP API call.
func isUpstreamDeadStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout,     // 504
		521, 522, 523, 524:            // CF dead-origin family
		return true
	}
	return false
}

// classificationFromDeliveryConfirmed returns the proxyA2AError
// classification that corresponds to the deliveryConfirmed predicate
// set by the 2xx-body-read-error path. The classification "delivered"
// must align exactly with the success condition in
// executeDelegation.isDeliveryConfirmedSuccess (which promotes the
// delegation to handleSuccess on proxyErr != nil, 200 <= status < 300,
// len(respBody) > 0), otherwise monitoring/PM would under-count failures:
//   - A 2xx response with body-read error and len(respBody) > 0 is a
//     real "delivered" — the work is done, the error is on the wire.
//   - A 2xx response with body-read error and len(respBody) == 0 is
//     NOT a real "delivered" — the agent wrote status + headers but no
//     body bytes reached us, and executeDelegation's
//     isDeliveryConfirmedSuccess gates on len(respBody) > 0, so the
//     delegation is recorded as a failure. Classifying this as
//     "delivered" would under-count failures.
//   - A 3xx response with body-read error is a server-authored redirect
//     rejection (A2A does not follow redirects). executeDelegation
//     keeps the failure status. Classifying as "delivered" would also
//     under-count failures.
//
// CR2 review 12458: the original implementation used
// `resp.StatusCode >= 200 && resp.StatusCode < 400` (any 2xx OR 3xx)
// which is broader than the success gate and recreates the same
// false-classification problem in a narrower shape. This stricter
// predicate restores the alignment with isDeliveryConfirmedSuccess.
//
// 2026-06-19 a2a RCA (#3056). See proxyA2AError.Classification for the
// full set of possible values.
func classificationFromDeliveryConfirmed(status int, bodyNonEmpty bool) string {
	if status >= 200 && status < 300 && bodyNonEmpty {
		return "delivered"
	}
	return ""
}

func (e *proxyA2AError) Error() string {
	if e == nil {
		return ""
	}
	base := "proxy a2a error"
	if e.Response != nil {
		if msg, ok := e.Response["error"].(string); ok && msg != "" {
			base = msg
		}
	}
	if e.Classification == "" {
		return base
	}
	// Suffix the classification so callers (and humans tailing logs) can
	// tell apart the three distinct conditions without having to inspect
	// the response body shape. Monitoring/PM should consume the
	// Classification field directly, not parse this string — this is for
	// log readability only.
	return base + " [" + e.Classification + "]"
}

// EnqueueA2A is a method wrapper around the package-level EnqueueA2A function so
// that *WorkspaceHandler satisfies the scheduler's A2AProxy interface. The
// scheduler cannot call the package function directly (it would have to import
// internal/handlers, but handlers already imports internal/scheduler → import
// cycle), so it goes through this method on the proxy it already holds. Used by
// the cron scheduler to durably buffer a tick when the target workspace is busy.
func (h *WorkspaceHandler) EnqueueA2A(ctx context.Context, workspaceID, callerID string, priority int, body []byte, method, idempotencyKey string, expiresAt *time.Time) (string, int, error) {
	return EnqueueA2A(ctx, workspaceID, callerID, priority, body, method, idempotencyKey, expiresAt)
}

// ProxyA2ARequest is the public wrapper for proxyA2ARequest, used by the
// cron scheduler and other internal callers that need to send A2A messages
// to workspaces programmatically (not from an HTTP handler).
func (h *WorkspaceHandler) ProxyA2ARequest(ctx context.Context, workspaceID string, body []byte, callerID string, logActivity bool) (int, []byte, error) {
	status, resp, proxyErr := h.proxyA2ARequest(ctx, workspaceID, body, callerID, logActivity, false)
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

	// Read the incoming request body (capped at maxProxyRequestBody). If the
	// caller sends a larger body, fail LOUD with 413 instead of silently
	// truncating mid-message (core#2677).
	body, err := readBodyWithLimit(c.Request.Body, maxProxyRequestBody, "request")
	if err != nil {
		if errors.Is(err, errA2ABodyTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":     err.Error(),
				"truncated": true,
				"max_bytes": maxProxyRequestBody,
			})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}

	callerID := c.GetHeader("X-Workspace-ID")

	// #2306: when X-Workspace-ID isn't set, derive callerID from the bearer
	// token's owning workspace. External callers (third-party SDKs, the
	// channel plugin, etc.) authenticate purely via bearer and frequently
	// don't set the header — without this, activity_logs.source_id ends up
	// NULL and downstream consumers (notification peer_id, "Agent Comms by
	// peer" tab, analytics) can't identify the sender. The bearer is the
	// authoritative caller identity per the wsauth contract; the header is
	// just a display/routing hint that must agree with it.
	//
	// Skip when an org-level token is in play (canvas/admin path) — those
	// tokens grant org-wide access and don't bind to a single workspace.
	if callerID == "" {
		if _, isOrg := c.Get("org_token_id"); !isOrg {
			if tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization")); tok != "" {
				if wsID, err := wsauth.WorkspaceFromToken(ctx, db.DB, tok); err == nil {
					callerID = wsID
				}
			}
		}
	}

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
	//
	// Post-RFC#637: canvas users now send X-Workspace-ID (their identity
	// workspace). validateCallerToken detects canvas/admin auth on a
	// tokenless workspace and returns isCanvasUser=true so the proxy can
	// bypass CanCommunicate (human users sit outside the hierarchy).
	isCanvasUser := false
	if callerID != "" && callerID != workspaceID && !isSystemCaller(callerID) {
		var err error
		isCanvasUser, err = validateCallerToken(ctx, c, callerID)
		if err != nil {
			return // response already written with 401
		}
	}

	// core#2691: attach the authenticated human sender's identity to canvas_user
	// messages, and fail-closed to UNAUTHENTICATED when it cannot be resolved.
	if isCanvasUser {
		ident := resolveCanvasIdentity(c)
		body, err = injectCanvasUserIdentity(body, ident)
		if err != nil {
			log.Printf("ProxyA2A: failed to inject canvas identity: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid A2A body"})
			return
		}
	}

	// CANVAS CAP-AND-QUEUE (core#2751, DEFAULT ON). The canvas→agent POST is
	// held for the whole turn; a turn longer than Cloudflare's ~100s edge limit
	// returns a 524 (the recurring "Failed to send"). By default we cap the
	// SYNCHRONOUS wait for canvas callers at 90s (just under CF's edge): if the
	// turn hasn't finished by the budget, ack `{status:"queued"}` and let the
	// dispatch finish on its own — proxyA2ARequest's dispatch already runs on a
	// context.WithoutCancel forward ctx (idle-bounded), so it survives this
	// handler returning, and the agent's reply reaches the canvas via the
	// AGENT_MESSAGE WebSocket broadcast (the exact poll-mode contract). The work
	// runs on a detached ctx so its DB logging isn't cancelled when we return.
	//
	// Operators can tune the budget via the env var (e.g. 60s for more
	// conservative environments). The default of 90s is the durable fix that
	// removes the CF 100s ceiling for the canvas path. envx.Duration treats
	// 0/negative as "not set" and falls through to the default — to disable
	// the cap, an operator must explicitly patch the default (a code change).
	// See the design on core#2751.
	//
	// The budget lookup is extracted into canvasA2ASyncBudget (below) so the
	// default value is unit-testable without source-string matching.
	//
	// Runtime kill-switch (core#2751 RC #11552): canvasA2ASyncDisabled()
	// is the explicit opt-out that takes precedence over the budget value.
	// When set, the entire cap-and-queue goroutine is skipped and the
	// legacy synchronous path runs. Ops can flip this at runtime to
	// disable the async path if it misbehaves in prod.
	if !canvasA2ASyncDisabled() && canvasA2ASyncBudget() > 0 && (callerID == "" || isCanvasUser) {
		type a2aResult struct {
			status int
			body   []byte
			perr   *proxyA2AError
		}
		detached := context.WithoutCancel(ctx)
		budget := canvasA2ASyncBudget() // local copy for the time.After below
		done := make(chan a2aResult, 1)
		h.asyncWG.Add(1)
		go func() {
			defer h.asyncWG.Done()
			s, b, pe := h.proxyA2ARequest(detached, workspaceID, body, callerID, true, isCanvasUser)
			done <- a2aResult{s, b, pe}
		}()
		select {
		case r := <-done:
			if r.perr != nil {
				for k, v := range r.perr.Headers {
					c.Header(k, v)
				}
				c.JSON(r.perr.Status, r.perr.Response)
				return
			}
			c.Data(r.status, "application/json", r.body)
			return
		case <-time.After(budget):
			// Outlived CF's edge limit — ack queued; the goroutine finishes and
			// the reply lands via WS. The canvas already treats `queued` as
			// "still processing" (delivery_mode mirrors poll-mode).
			c.JSON(http.StatusOK, gin.H{"status": "queued", "delivery_mode": "push-async", "method": "message/send"})
			return
		}
	}

	status, respBody, proxyErr := h.proxyA2ARequest(ctx, workspaceID, body, callerID, true, isCanvasUser)
	if proxyErr != nil {
		for k, v := range proxyErr.Headers {
			c.Header(k, v)
		}
		c.JSON(proxyErr.Status, proxyErr.Response)
		return
	}

	c.Data(status, "application/json", respBody)
}

// checkWorkspaceBudget returns a proxyA2AError with 402 when the workspace has
// exceeded ANY of its configured per-period budget limits (hourly/daily/weekly/
// monthly — see budget_periods.go). Per-period spend is the rolling-window sum
// over the workspace_spend_events ledger. DB errors are logged and treated as
// fail-open — a budget check failure must not block legitimate A2A traffic.
func (h *WorkspaceHandler) checkWorkspaceBudget(ctx context.Context, workspaceID string) *proxyA2AError {
	var limitsRaw []byte
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(budget_limits, '{}'::jsonb) FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&limitsRaw); err != nil {
		if err != sql.ErrNoRows {
			log.Printf("ProxyA2A: budget check failed for %s: %v", workspaceID, err)
		}
		return nil // fail-open
	}
	limits := parseBudgetLimits(limitsRaw)
	if len(limits) == 0 {
		return nil // no limits configured
	}
	spend, err := spendByPeriod(ctx, db.DB, workspaceID)
	if err != nil {
		log.Printf("ProxyA2A: budget spend query failed for %s: %v", workspaceID, err)
		return nil // fail-open
	}
	if over := exceededPeriods(limits, spend); len(over) > 0 {
		log.Printf("ProxyA2A: budget exceeded for %s (periods=%v limits=%v spend=%v)", workspaceID, over, limits, spend)
		return &proxyA2AError{
			Status: http.StatusPaymentRequired,
			Response: gin.H{
				"error":            "workspace budget limit exceeded",
				"exceeded_periods": over,
			},
		}
	}
	return nil
}

func (h *WorkspaceHandler) proxyA2ARequest(ctx context.Context, workspaceID string, body []byte, callerID string, logActivity bool, isCanvasUser bool) (int, []byte, *proxyA2AError) {
	// Access control: workspace-to-workspace requests must pass CanCommunicate check.
	// Canvas requests (callerID == "") and system callers (webhook:*, system:*, test:*)
	// are trusted. Self-calls (callerID == workspaceID) are always allowed.
	// Post-RFC#637: canvas-user identity workspaces also bypass CanCommunicate
	// because human users sit outside the org hierarchy.
	if callerID != "" && callerID != workspaceID && !isSystemCaller(callerID) && !isCanvasUser {
		if !registry.CanCommunicate(callerID, workspaceID) {
			log.Printf("ProxyA2A: access denied %s → %s", callerID, workspaceID)
			return 0, nil, &proxyA2AError{
				Status:   http.StatusForbidden,
				Response: gin.H{"error": "access denied: workspaces cannot communicate per hierarchy rules"},
			}
		}

		// #1953 cross-tenant isolation. CanCommunicate alone does NOT enforce
		// org boundaries: its "root-level siblings — both have no parent" rule
		// treats every tenant's org root as a sibling, so a caller that is an
		// org root could resolve and route a2a to another tenant's org root
		// (and resolveAgentURL accepts ANY workspace id with no org check).
		// Gate on the SAME parent_id-chain org scoping the OFFSEC-015 broadcast
		// fix uses: reject before resolveAgentURL when caller and target are in
		// different orgs. Fail-closed — a DB error denies cross-org routing.
		ok, err := sameOrg(ctx, db.DB, callerID, workspaceID)
		if err != nil {
			log.Printf("ProxyA2A: org-scope check failed %s → %s: %v — denying", callerID, workspaceID, err)
			return 0, nil, &proxyA2AError{
				Status:   http.StatusForbidden,
				Response: gin.H{"error": "access denied: org isolation check failed"},
			}
		}
		if !ok {
			log.Printf("ProxyA2A: cross-org routing denied %s → %s (#1953)", callerID, workspaceID)
			return 0, nil, &proxyA2AError{
				Status:   http.StatusForbidden,
				Response: gin.H{"error": "access denied: target workspace is in a different org"},
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

	// Normalize the JSON-RPC envelope BEFORE the poll-mode short-circuit
	// so the activity_logs entry carries the protocol method name (initialize,
	// message/send, etc.) — the polling agent uses that to dispatch the
	// request body to the right handler. Doing it here also means a
	// malformed payload fails the same way for push and poll callers
	// (consistent 400 instead of "queued garbage").
	normalizedBody, a2aMethod, proxyErr := normalizeA2APayload(body)
	if proxyErr != nil {
		return 0, nil, proxyErr
	}
	body = normalizedBody

	// #2560 (chat UX: persist in-flight exchange across leave/refresh):
	// write the user message to activity_logs AT RECEIPT — before any of
	// the downstream short-circuits (poll-mode, mock-runtime, push dispatch)
	// run — so a mid-turn leave/refresh re-hydrates the user message
	// instead of an empty pane. Idempotent on messageId via the partial
	// unique index (idx_activity_logs_msg_id) + ON CONFLICT DO UPDATE in
	// logActivityExec — a poll-mode re-persist (or a duplicated ingest
	// from a retry) collapses to a single row, and the completion path
	// (logA2ASuccess / logA2AReceiveQueued) stamps response_body onto
	// the same row. No duplicate bubble in chat-history.
	//
	// Skipped when the body has no messageId (system callers, legacy
	// a2a_send payloads) — the completion path remains authoritative.
	if a2aMethod == "message/send" {
		h.persistUserMessageAtIngest(ctx, workspaceID, callerID, body, a2aMethod)
	}

	// #2339 PR 2 — poll-mode short-circuit. When the target workspace
	// is registered as delivery_mode=poll (e.g. an operator's laptop
	// running molecule-mcp-claude-channel), the platform does NOT
	// dispatch over HTTP — the agent has no public URL. Instead we record
	// the A2A request to activity_logs and the agent picks it up via
	// GET /activity?since_id= (PR 3).
	//
	// Returning here means we skip resolveAgentURL entirely (no SSRF check
	// needed — there's no URL to validate; no DNS lookup against potentially-
	// changing operator-side IPs) and skip the dispatch path completely
	// (no Do(), no maybeMarkContainerDead). The response is a synthetic
	// {status:"queued"} envelope so the caller (canvas, another workspace)
	// knows delivery is acknowledged but pending consumption.
	deliveryMode, deliveryModeErr := lookupDeliveryMode(ctx, workspaceID)
	if deliveryModeErr != nil {
		// internal#497 fail-closed: a real DB/context error on the
		// delivery-mode read MUST NOT silently fall through to the push
		// dispatch path — that is exactly what silently misrouted every
		// poll-mode peer for 5 days under the ce2db75f regression. Surface
		// a structured error so the delegation is marked failed (loud +
		// retryable) instead of dispatched to the wrong path.
		log.Printf("ProxyA2A: delivery-mode lookup failed for %s: %v — failing closed", workspaceID, deliveryModeErr)
		return 0, nil, &proxyA2AError{
			Status:   http.StatusServiceUnavailable,
			Response: gin.H{"error": "delivery-mode lookup failed; refusing to dispatch to avoid silent misrouting"},
		}
	}
	if deliveryMode == models.DeliveryModePoll {
		if logActivity {
			h.logA2AReceiveQueued(ctx, workspaceID, callerID, body, a2aMethod)
		}
		respBody, marshalErr := json.Marshal(gin.H{
			"status":        "queued",
			"delivery_mode": models.DeliveryModePoll,
			"method":        a2aMethod,
		})
		if marshalErr != nil {
			return 0, nil, &proxyA2AError{
				Status:   http.StatusInternalServerError,
				Response: gin.H{"error": "failed to marshal poll-mode response"},
			}
		}
		return http.StatusOK, respBody, nil
	}

	// Mock-runtime short-circuit. Workspaces with runtime='mock' have
	// no container, no EC2, no URL — every reply is synthesised here
	// from a small canned-variant pool. Built for the "200-workspace
	// mock org" demo: a CEO/VPs/Managers/ICs hierarchy that renders
	// at scale on the canvas without burning real LLM credits or
	// provisioning 200 EC2 instances. See mock_runtime.go for the
	// full rationale + reply shape contract.
	//
	// Position: AFTER poll-mode (mock isn't a delivery mode, it's a
	// runtime; treating poll-set-on-mock as poll matches operator
	// intent if anyone ever does that), BEFORE resolveAgentURL (mock
	// has no URL — going through resolveAgentURL would 404 on the
	// SELECT url since the row is provisioned as NULL).
	if status, respBody, handled := h.handleMockA2A(ctx, workspaceID, callerID, isCanvasUser, body, a2aMethod, logActivity); handled {
		return status, respBody, nil
	}

	agentURL, proxyErr := h.resolveAgentURL(ctx, workspaceID)
	if proxyErr != nil {
		return 0, nil, proxyErr
	}

	// Pre-flight container-health check (#36). The dispatchA2A path below
	// does Docker-DNS forwarding to `ws-<wsShort>:8000` and only catches a
	// missing/dead container REACTIVELY via maybeMarkContainerDead in
	// handleA2ADispatchError. That works but costs the caller a full
	// network-timeout (2-30s) before the structured 503 surfaces.
	//
	// When we KNOW the workspace is container-backed (h.docker != nil + we
	// rewrite to Docker-DNS form below), do a single proactive
	// RunningContainerName lookup. If the container is genuinely missing,
	// short-circuit with the same structured 503 + async restart that
	// maybeMarkContainerDead would produce — but immediately, without the
	// network round-trip.
	//
	// Three outcomes of provisioner.RunningContainerName(ctx, h.docker, id):
	//   ("ws-<id>", nil) → forward as today.
	//   ("",        nil) → container is genuinely not running. Fast-503.
	//   ("",        err) → transient daemon error. Fall through to optimistic
	//                       forward — matches Provisioner.IsRunning's
	//                       (true, err) "fail-soft as alive" contract.
	//
	// Same SSOT as findRunningContainer (#10/#12). See AST gate
	// TestProxyA2A_RoutesThroughProvisionerSSOT.
	if h.provisioner != nil && platformInDocker && strings.HasPrefix(agentURL, "http://"+provisioner.ContainerName(workspaceID)+":") {
		if proxyErr := h.preflightContainerHealth(ctx, workspaceID); proxyErr != nil {
			return 0, nil, proxyErr
		}
	}

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

	// Read agent response (capped at maxProxyResponseBody).
	// #689: Do() succeeded, which means the target received the request and sent
	// back response headers — delivery is confirmed. The body couldn't be
	// fully read (connection drop, timeout mid-stream, OR it exceeded the
	// maxProxyResponseBody limit). Surface delivery_confirmed so callers can
	// distinguish "not delivered" from "delivered, but response body lost".
	// When delivery is confirmed, log the activity as successful (delivery
	// happened) rather than leaving a false "failed" entry in the audit trail.
	//
	// core#2677: readBodyWithLimit detects oversize responses and returns an
	// errA2ABodyTooLarge-wrapped error so we surface a loud "truncated" flag
	// instead of silently cutting long agent replies.
	respBody, readErr := readBodyWithLimit(resp.Body, maxProxyResponseBody, "response")
	if readErr != nil {
		deliveryConfirmed := resp.StatusCode >= 200 && resp.StatusCode < 400
		log.Printf("ProxyA2A: body read failed for %s (status=%d delivery_confirmed=%v bytes_read=%d): %v",
			workspaceID, resp.StatusCode, deliveryConfirmed, len(respBody), readErr)
		if logActivity && deliveryConfirmed {
			h.logA2ASuccess(ctx, workspaceID, callerID, isCanvasUser, body, respBody, a2aMethod, resp.StatusCode, durationMs)
		}
		// Preserve the actual HTTP status code and any body bytes already read.
		// Previously this returned (0, nil, error) which discarded both.
		// Preserving them allows executeDelegation's new condition
		//   proxyErr != nil && len(respBody) > 0 && status >= 200 && status < 300
		// to correctly route delivery-confirmed responses (where the agent completed
		// the work but the TCP connection dropped before the full body was received)
		// to success instead of failure (#159).
		//
		// For non-2xx responses (server explicitly rejected with 3xx+), preserve
		// resp.StatusCode in the proxyA2AError.Status so isTransientProxyError
		// returns false — a server-authored rejection is not a transient transport
		// error and must not be retried. Only 2xx body-read errors keep Status=502
		// (the agent completed work but the TCP layer dropped the response).
		errStatus := http.StatusBadGateway
		if resp.StatusCode >= 300 {
			errStatus = resp.StatusCode
		}
		errMsg := "failed to read agent response"
		if errors.Is(readErr, errA2ABodyTooLarge) {
			errMsg = readErr.Error()
		}
		return resp.StatusCode, respBody, &proxyA2AError{
			Status: errStatus,
			Response: gin.H{
				"error":              errMsg,
				"delivery_confirmed": deliveryConfirmed,
				"truncated":          errors.Is(readErr, errA2ABodyTooLarge),
				"max_bytes":          maxProxyResponseBody,
			},
			// 2026-06-19 a2a RCA (#3056): when the agent completed the
			// work and the proxy just failed to read the full body, the
			// delegation is a SUCCESS — executeDelegation's
			// isDeliveryConfirmedSuccess() check promotes it to the
			// handleSuccess path. Marking it as "delivered" makes the
			// classification visible to monitoring/PM; the prior opaque
			// "proxy a2a error" string made it look like a failure
			// even when the agent returned a 2xx body. Skipped when
			// deliveryConfirmed is false (the response was non-2xx or
			// the body was empty) — those are real failures, not
			// delivery blips.
			Classification: classificationFromDeliveryConfirmed(resp.StatusCode, len(respBody) > 0),
		}
	}

	// 2xx with empty body: the agent completed the request but returned no content.
	// An A2A agent must always return a JSON body; empty means the agent is
	// broken or the connection closed before any body bytes were written.
	// Return a proxyA2AError so executeDelegation routes this to failure rather
	// than silently marking it as completed with a nil body.
	// logA2ASuccess is intentionally NOT called here — delivery was not confirmed.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && len(respBody) == 0 {
		log.Printf("ProxyA2A: agent %s returned %d with empty body — treating as failure",
			workspaceID, resp.StatusCode)
		return resp.StatusCode, respBody, &proxyA2AError{
			Status:   resp.StatusCode,
			Response: gin.H{"error": "agent returned empty response body"},
		}
	}

	if logActivity {
		h.logA2ASuccess(ctx, workspaceID, callerID, isCanvasUser, body, respBody, a2aMethod, resp.StatusCode, durationMs)
	}

	// Track LLM token usage for cost transparency (#593).
	// Fires in a detached goroutine so token accounting never adds latency
	// to the critical A2A path.
	// RFC internal#524 Layer 1: extractAndUpsertTokenUsage reads db.DB
	// (INSERT INTO llm_token_usage). Without globalGoAsync, the detached
	// write races a subsequent test's db.DB swap exactly like the
	// maybeMarkContainerDead path that 69d9b4e3 fixed.
	tokCtx := context.WithoutCancel(ctx)
	wsID := workspaceID
	tokBody := respBody
	globalGoAsync(func() { extractAndUpsertTokenUsage(tokCtx, wsID, tokBody) })

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

		// Upstream returned 502/503/504 (gateway/proxy failure). This is
		// the "agent process is dead but the tunnel between us and the
		// workspace is still up" signal — handleA2ADispatchError's
		// network-error path doesn't run because Do() succeeded at the
		// HTTP layer. Without this branch, the dead-agent failure mode
		// surfaces to canvas as a generic 502 (and CF in front of the
		// platform masks it with its own error page, hiding any
		// structured response we might write).
		//
		// Treatment matches handleA2ADispatchError's container-dead path:
		//   1. Probe IsRunning via maybeMarkContainerDead. If the
		//      container truly is dead, mark workspace offline + kick
		//      a restart goroutine.
		//   2. Return a structured 503 with restarting=true + Retry-After
		//      so canvas shows a useful "agent is restarting" message
		//      (and CF doesn't intercept the 503 the way it does 502).
		// If IsRunning reports the container is alive, we leave the
		// upstream status untouched — the agent legitimately returned
		// 502/503/504 (e.g. it's returning its own Bad-Gateway from
		// some downstream call) and we shouldn't mistakenly recycle it.
		//
		// Empty body is the strong signal here — a CF-tunnel "no-origin"
		// 502 has 0 bytes; an agent-authored 502 typically has a JSON
		// error body. We probe IsRunning regardless (it's the
		// authoritative check) but the empty-body case is what makes
		// this fix necessary.
		if isUpstreamDeadStatus(resp.StatusCode) {
			dead, queuedStatus, queuedBody := h.maybeMarkContainerDead(ctx, workspaceID, callerID, body, a2aMethod, durationMs, logActivity)
			if queuedStatus != 0 {
				return queuedStatus, queuedBody, nil
			}
			if dead {
				return 0, nil, &proxyA2AError{
					Status:   http.StatusServiceUnavailable,
					Headers:  map[string]string{"Retry-After": "15"},
					Response: gin.H{"error": "workspace agent unreachable — container restart triggered", "restarting": true, "retry_after": 15},
					// 2026-06-19 a2a RCA (#3056): the dead-origin family
					// (502/504/521/522/523/524 + 503-restarting) is the only
					// classification that genuinely counts as a failure.
					// Distinct from busy_retryable (alive agent, mid-turn)
					// and delivered (2xx with transport blip).
					Classification: "upstream_dead",
				}
			}
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
				h.goAsync(func() { h.RestartByID(workspaceID) })
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

	// When the platform runs inside Docker, a managed workspace's
	// 127.0.0.1:{host_port} URL points at the Docker host and must be
	// rewritten to the workspace container's Docker-bridge hostname.
	// External runtimes are not managed containers; their local test/runtime
	// URL is the target and must not be synthesized into ws-<id>:8000.
	if strings.HasPrefix(agentURL, "http://127.0.0.1:") && h.provisioner != nil && platformInDocker {
		var wsRuntime string
		if err := db.DB.QueryRowContext(ctx,
			`SELECT COALESCE(runtime, 'claude-code') FROM workspaces WHERE id = $1`,
			workspaceID,
		).Scan(&wsRuntime); err != nil {
			log.Printf("ProxyA2A: runtime lookup before Docker URL rewrite failed for %s: %v", workspaceID, err)
		}
		if !isExternalLikeRuntime(wsRuntime) {
			agentURL = provisioner.InternalURL(workspaceID)
		}
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
	// AND v0.2→v0.3 compat (#2345): when sender supplies
	// params.message.content (v0.2) instead of params.message.parts
	// (v0.3), wrap the content as a single text Part so the downstream
	// a2a-sdk's v0.3 Pydantic validator accepts the message.
	//
	// Pre-fix: Design Director silently dropped briefs whose sender
	// used v0.2 shape — Pydantic rejected at parse time, the rejection
	// went only to logs, and the sender saw a happy 200/202.
	//
	// Reject loud (HTTP 400) when neither content nor parts is present;
	// previously the SDK's own rejection happened post-handler-dispatch
	// and was invisible to the original sender.
	if params, ok := payload["params"].(map[string]interface{}); ok {
		if msg, ok := params["message"].(map[string]interface{}); ok {
			if _, hasID := msg["messageId"]; !hasID {
				msg["messageId"] = uuid.New().String()
			}
			// #2251: default params.message.role to "user" when absent.
			// The downstream a2a-sdk v0.3 Pydantic validator marks role a
			// REQUIRED field; a role-less envelope fails parse with
			// "params.message.role Field required". The Go builders
			// (mcp_tools/delegation/scheduler/channels) already set it, but
			// raw external/canvas POSTs to ProxyA2A may omit it — making this
			// the single canonical choke that guarantees a schema-valid role.
			// Mirror the messageId default exactly: inject only when missing,
			// never overwrite a caller-supplied role (e.g. "agent").
			if _, hasRole := msg["role"]; !hasRole {
				msg["role"] = "user"
			}
			_, hasParts := msg["parts"]
			rawContent, hasContent := msg["content"]
			if !hasParts {
				if hasContent {
					switch v := rawContent.(type) {
					case string:
						msg["parts"] = []interface{}{
							map[string]interface{}{"kind": "text", "text": v},
						}
					case []interface{}:
						msg["parts"] = v
					default:
						return nil, "", &proxyA2AError{
							Status: http.StatusBadRequest,
							Response: gin.H{
								"error": "invalid params.message.content type",
								"hint":  "content must be a string (v0.2 compat) or omitted in favour of parts (v0.3)",
							},
						}
					}
					delete(msg, "content")
				} else {
					return nil, "", &proxyA2AError{
						Status: http.StatusBadRequest,
						Response: gin.H{
							"error": "params.message must contain either 'parts' (v0.3) or 'content' (v0.2 compat)",
							"hint":  "v0.3 example: {\"parts\":[{\"kind\":\"text\",\"text\":\"...\"}]}",
						},
					}
				}
			}
			// #2251: wire hygiene — the A2A v0.3 Part discriminator is
			// "kind", but some builders/clients emit the legacy "type" key
			// (e.g. delegation.go). The v0.3 Pydantic validator keys on
			// "kind"; a stray "type" leaves the Part untagged. Rename
			// "type" → "kind" on every Part that lacks an explicit "kind"
			// so the discriminator is always present on the wire.
			if parts, ok := msg["parts"].([]interface{}); ok {
				for _, p := range parts {
					part, ok := p.(map[string]interface{})
					if !ok {
						continue
					}
					if _, hasKind := part["kind"]; hasKind {
						continue
					}
					if t, hasType := part["type"]; hasType {
						part["kind"] = t
						delete(part, "type")
					}
				}
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
// 2026-06-13 (core#2723): raised 5min → 30min. The 30s WORKSPACE_HEARTBEAT
// normally resets this long before 5min, so the window only matters when
// the heartbeat STALLS — which happens when the runtime's asyncio
// heartbeat task is starved by a long *blocking* tool call (e.g. a bulk
// asset migration). A real long autonomous turn was getting cancelled at
// ~300s mid-work ("tool chain lost"). The complete fix is runtime-side
// (heartbeat on an independent thread, tracked in #2723); raising this
// ceiling is the deployable safety margin so a multi-minute blocking step
// survives. 30min matches the agent-to-agent absolute ceiling. The canvas
// path has no separate ceiling, so this is its only deadline; a genuinely
// dead agent is still surfaced by the reactive-health path, not this.
//
// Override via A2A_IDLE_TIMEOUT_SECONDS for ops who want to tune (e.g.
// shorter for canary/test runners that want fail-fast on wedge, longer
// for prod tenants running unusually slow plugins).
var idleTimeoutDuration = parseIdleTimeoutEnv(os.Getenv("A2A_IDLE_TIMEOUT_SECONDS"))

// defaultIdleTimeoutDuration is what parseIdleTimeoutEnv returns when
// the env var is unset or invalid. Pulled out as a const so tests can
// reference it without re-deriving the value.
const defaultIdleTimeoutDuration = 30 * time.Minute

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
	// goAsync-exempt (RFC internal#524 Layer 2.2 annotation): this
	// goroutine owns the parent ctx's cancel and exits only on
	// ctx.Done() / sub-channel close — wrapping it in globalGoAsync would
	// deadlock drainTestAsync because the request that owns ctx hasn't
	// completed when t.Cleanup fires. Does NOT read db.DB; idle-timer
	// management only.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("a2a_proxy: PANIC in SSE idle watcher for %s: %v", workspaceID, r)
			}
			unsub()
		}()
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

// canvasA2ASyncBudget is the extracted lookup for the cap-and-queue synchronous
// wait (core#2751). Extracted from the ProxyA2A handler so the default value
// can be unit-tested directly without source-string matching — a regression of
// the default to 0 (legacy always-sync, which would re-expose the canvas path
// to the 524+WS-starvation class) is caught by the unit test on this function.
//
// Default is 90s — just under Cloudflare's ~100s edge limit, so a turn that
// outlives the budget triggers the queued ack before CF drops the request.
//
// The envx.Duration wrapper lets operators tune the budget
// (A2A_CANVAS_SYNC_BUDGET=60s for more conservative environments) without a
// code change. envx treats 0 / negative / invalid values as "not set" and
// falls through to the default.
//
// **Runtime kill-switch (core#2751 RC #11552):** the cap-and-queue can
// also be disabled at runtime by setting A2A_CANVAS_SYNC_DISABLE=1 (or any
// truthy envx.Bool value). When set, ProxyA2A skips the cap-and-queue
// goroutine entirely and uses the legacy synchronous path, regardless of
// the budget value. Ops can flip this without a deploy if the async path
// misbehaves in prod. The kill-switch is implemented as a SEPARATE env
// var (not via the budget=0 path) because envx.Duration treats 0/negative
// as "not set" and falls through to the default — there was no other
// way to disable at runtime without a code change.
func canvasA2ASyncBudget() time.Duration {
	return envx.Duration("A2A_CANVAS_SYNC_BUDGET", 90*time.Second)
}

// canvasA2ASyncDisabled is the runtime kill-switch for the cap-and-queue
// async-dispatch path. Returns true when A2A_CANVAS_SYNC_DISABLE is set
// to a truthy value (per envx.Bool semantics: 1, t, true, TRUE, T —
// NOT 0, f, false, F, empty). When true, ProxyA2A skips the
// cap-and-queue goroutine entirely and uses the legacy synchronous
// path. Extracted so the kill-switch default is unit-testable
// independently of the budget value.
func canvasA2ASyncDisabled() bool {
	return envx.Bool("A2A_CANVAS_SYNC_DISABLE", false)
}
