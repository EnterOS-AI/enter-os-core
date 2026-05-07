package handlers

// a2a_proxy_helpers.go — A2A proxy error handling, activity logging,
// caller auth validation, token usage tracking, and SSRF safety checks.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
	"github.com/gin-gonic/gin"
)

// proxyDispatchBuildError is a sentinel wrapper for failures inside
// http.NewRequestWithContext. handleA2ADispatchError unwraps it to emit the
// "failed to create proxy request" 500 instead of the standard 502/503 paths.
type proxyDispatchBuildError struct{ err error }

func (e *proxyDispatchBuildError) Error() string { return e.err.Error() }

// handleA2ADispatchError translates a forward-call failure into a proxyA2AError,
// runs the reactive container-health check, and (when `logActivity` is true)
// schedules a detached LogActivity goroutine for the failed attempt.
func (h *WorkspaceHandler) handleA2ADispatchError(ctx context.Context, workspaceID, callerID string, body []byte, a2aMethod string, err error, durationMs int, logActivity bool) (int, []byte, *proxyA2AError) {
	// Build-time failure (couldn't even create the http.Request) — return
	// a 500 without the reactive-health / busy-retry paths.
	if buildErr, ok := err.(*proxyDispatchBuildError); ok {
		_ = buildErr
		return 0, nil, &proxyA2AError{
			Status:   http.StatusInternalServerError,
			Response: gin.H{"error": "failed to create proxy request"},
		}
	}

	log.Printf("ProxyA2A forward error: %v", err)

	containerDead := h.maybeMarkContainerDead(ctx, workspaceID)

	if logActivity {
		h.logA2AFailure(ctx, workspaceID, callerID, body, a2aMethod, err, durationMs)
	}
	if containerDead {
		return 0, nil, &proxyA2AError{
			Status:   http.StatusServiceUnavailable,
			Response: gin.H{"error": "workspace agent unreachable — container restart triggered", "restarting": true},
		}
	}
	// Container is alive but upstream Do() failed with a timeout/EOF-
	// shaped error — the agent is most likely mid-synthesis on a
	// previous request (single-threaded main loop). Surface as 503
	// Busy with a Retry-After hint so callers can distinguish this
	// from a real unreachable-agent (502) and retry with backoff.
	// Issue #110.
	//
	// #1870 Phase 1: before returning 503, enqueue the request for drain
	// on next heartbeat. Returning 202 Accepted {queued:true} as a SUCCESS
	// (not an error) means callers record this as "dispatched — queued"
	// not "failed", eliminating the fan-out-storm drop pattern.
	//
	// Critical: must return (status, body, NIL ERROR) so the caller's
	// `if proxyErr != nil` branch doesn't fire. Returning a proxyA2AError
	// with 202 status here was the original cycle 53 bug — callers saw
	// proxyErr != nil and logged "delegation failed: proxy a2a error".
	if isUpstreamBusyError(err) {
		// Capability primitive #5 — see project memory
		// `project_runtime_native_pluggable.md`. When the target workspace's
		// adapter has declared provides_native_session=True, the SDK
		// owns its own queue/session state (claude-agent-sdk's streaming
		// session, hermes-agent's in-container event log, etc.). Adding
		// the platform's a2a_queue layer on top would double-buffer the
		// same in-flight state — and worse, the platform queue's drain
		// timing has no relationship to the SDK's actual readiness, so
		// the queued request might dispatch while the SDK is STILL busy.
		//
		// For native_session targets, return 503 + Retry-After directly.
		// The caller's adapter handles retry on its own schedule, and
		// the SDK's own queue absorbs the in-flight request when it does.
		// Observability is preserved: logA2AFailure already ran above;
		// activity_logs records the busy event; the broadcaster fires.
		if runtimeOverrides.HasCapability(workspaceID, "session") {
			log.Printf("ProxyA2A: target %s busy and declares native_session — skip enqueue, return 503", workspaceID)
			return 0, nil, &proxyA2AError{
				Status:  http.StatusServiceUnavailable,
				Headers: map[string]string{"Retry-After": strconv.Itoa(busyRetryAfterSeconds)},
				Response: gin.H{
					"error":          "workspace agent busy — adapter handles retry (native_session)",
					"busy":           true,
					"retry_after":    busyRetryAfterSeconds,
					"native_session": true,
				},
			}
		}

		idempotencyKey := extractIdempotencyKey(body)
		// Honor params.expires_in_seconds when the caller specifies one. Zero
		// (the unset default) → expiresAt = nil → infinite TTL preserved by
		// DequeueNext. RFC #2331 Tier 1.
		var expiresAt *time.Time
		if secs := extractExpiresInSeconds(body); secs > 0 {
			t := time.Now().Add(time.Duration(secs) * time.Second)
			expiresAt = &t
		}
		if qid, depth, qerr := EnqueueA2A(
			ctx, workspaceID, callerID, PriorityTask, body, a2aMethod, idempotencyKey, expiresAt,
		); qerr == nil {
			log.Printf("ProxyA2A: target %s busy — enqueued as %s (depth=%d)", workspaceID, qid, depth)
			respBody, _ := json.Marshal(gin.H{
				"queued":      true,
				"queue_id":    qid,
				"queue_depth": depth,
				"message":     "workspace agent busy — request queued, will dispatch when capacity available",
			})
			return http.StatusAccepted, respBody, nil
		} else {
			// Queue insert failed — fall through to legacy 503 behavior
			// so callers still retry. We don't want a queue DB hiccup to
			// make delegation silently disappear.
			log.Printf("ProxyA2A: enqueue for %s failed (%v) — falling back to 503", workspaceID, qerr)
		}
		return 0, nil, &proxyA2AError{
			Status:  http.StatusServiceUnavailable,
			Headers: map[string]string{"Retry-After": strconv.Itoa(busyRetryAfterSeconds)},
			Response: gin.H{
				"error":       "workspace agent busy — retry after a short backoff",
				"busy":        true,
				"retry_after": busyRetryAfterSeconds,
			},
		}
	}
	return 0, nil, &proxyA2AError{
		Status:   http.StatusBadGateway,
		Response: gin.H{"error": "failed to reach workspace agent"},
	}
}

// maybeMarkContainerDead runs the reactive health check after a forward error.
// If the workspace's compute (Docker container OR EC2 instance) is no longer
// running (and the workspace isn't external), it marks the workspace offline,
// clears Redis state, broadcasts WORKSPACE_OFFLINE, and triggers an async
// restart. Returns true when the compute was found dead.
//
// Provisioner selection (mutually exclusive in production):
//   - h.provisioner != nil  → local Docker deployment; IsRunning does docker inspect.
//   - h.cpProv != nil       → SaaS / EC2 deployment; IsRunning calls CP's
//     /cp/workspaces/:id/status to read the EC2 state.
//
// Pre-fix this function ONLY consulted h.provisioner — for SaaS tenants
// (h.provisioner=nil, h.cpProv=set) it short-circuited to false on every
// call, so a dead EC2 agent would propagate upstream 502/503/504 to canvas
// with no auto-recovery and Cloudflare in front would mask the response with
// its own error page. The 2026-04-30 hongmingwang.moleculesai.app
// canvas-chat-to-dead-workspace incident traces to exactly this gap.
func (h *WorkspaceHandler) maybeMarkContainerDead(ctx context.Context, workspaceID string) bool {
	var wsRuntime string
	db.DB.QueryRowContext(ctx, `SELECT COALESCE(runtime, 'langgraph') FROM workspaces WHERE id = $1`, workspaceID).Scan(&wsRuntime)
	if wsRuntime == "external" {
		return false
	}
	if !h.HasProvisioner() {
		return false
	}

	var running bool
	var inspectErr error
	if h.provisioner != nil {
		running, inspectErr = h.provisioner.IsRunning(ctx, workspaceID)
	} else {
		// SaaS path: ask the CP about the EC2 state. Same (true, err) on
		// transport errors contract — keeps the caller on the alive path
		// instead of triggering a restart cascade on a flaky CP call.
		running, inspectErr = h.cpProv.IsRunning(ctx, workspaceID)
	}
	if inspectErr != nil {
		// Transient backend error (Docker daemon EOF, CP HTTP 5xx, etc.).
		// IsRunning's contract returns (true, err) in this case so we stay
		// on the alive path without triggering a restart cascade.
		log.Printf("ProxyA2A: IsRunning for %s returned transient error (assuming alive): %v", workspaceID, inspectErr)
	}
	if running {
		return false
	}
	log.Printf("ProxyA2A: container for %s is dead — marking offline and triggering restart", workspaceID)
	if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2 AND status NOT IN ('removed', 'provisioning')`, models.StatusOffline, workspaceID); err != nil {
		log.Printf("ProxyA2A: failed to mark workspace %s offline: %v", workspaceID, err)
	}
	db.ClearWorkspaceKeys(ctx, workspaceID)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOffline), workspaceID, map[string]interface{}{})
	go h.RestartByID(workspaceID)
	return true
}

// preflightContainerHealth runs a proactive Provisioner.IsRunning check
// (#36) before dispatching the a2a forward. Routed through provisioner's
// SSOT IsRunning, which itself wraps RunningContainerName — same source
// as findRunningContainer in the plugins handler (#10/#12).
//
// Returns nil when the forward should proceed:
//   - container is running, OR
//   - daemon errored transiently (matches IsRunning's (true, err)
//     "fail-soft as alive" contract — let the optimistic forward run
//     and reactive maybeMarkContainerDead catch a real failure).
//
// Returns a structured 503 + triggers the same async restart that
// maybeMarkContainerDead would produce, when:
//   - container is genuinely not running (NotFound / Exited / Created…).
//
// The point of running this BEFORE the forward is to save the caller
// 2-30s of network-timeout cost when the container is missing — a common
// shape post-EC2-replace (see molecule-controlplane#20 incident
// 2026-05-07) where the reconciler hasn't respawned the agent yet.
func (h *WorkspaceHandler) preflightContainerHealth(ctx context.Context, workspaceID string) *proxyA2AError {
	running, err := h.provisioner.IsRunning(ctx, workspaceID)
	if err != nil {
		// Transient daemon error. Provisioner.IsRunning returns (true, err)
		// in this case — fall through to the optimistic forward, reactive
		// maybeMarkContainerDead handles a real failure later.
		log.Printf("ProxyA2A preflight: IsRunning transient error for %s: %v (proceeding with forward)", workspaceID, err)
		return nil
	}
	if running {
		// Container is running — forward as today.
		return nil
	}
	// Container is genuinely not running. Mark offline + trigger restart
	// (same effect as maybeMarkContainerDead's branch), and return the
	// structured 503 immediately so the caller skips the forward.
	log.Printf("ProxyA2A preflight: container for %s is not running — marking offline and triggering restart (#36)", workspaceID)
	if _, dbErr := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2 AND status NOT IN ('removed', 'provisioning')`,
		models.StatusOffline, workspaceID); dbErr != nil {
		log.Printf("ProxyA2A preflight: failed to mark workspace %s offline: %v", workspaceID, dbErr)
	}
	db.ClearWorkspaceKeys(ctx, workspaceID)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOffline), workspaceID, map[string]interface{}{})
	go h.RestartByID(workspaceID)
	return &proxyA2AError{
		Status: http.StatusServiceUnavailable,
		Response: gin.H{
			"error":      "workspace container not running — restart triggered",
			"restarting": true,
			"preflight":  true, // distinguishes from reactive containerDead path
		},
	}
}

// logA2AFailure records a failed A2A attempt to activity_logs in a detached
// goroutine (the request context may already be done by the time it runs).
func (h *WorkspaceHandler) logA2AFailure(ctx context.Context, workspaceID, callerID string, body []byte, a2aMethod string, err error, durationMs int) {
	errMsg := err.Error()
	var errWsName string
	db.DB.QueryRowContext(ctx, `SELECT name FROM workspaces WHERE id = $1`, workspaceID).Scan(&errWsName)
	if errWsName == "" {
		errWsName = workspaceID
	}
	summary := "A2A request to " + errWsName + " failed: " + errMsg
	go func(parent context.Context) {
		logCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
		defer cancel()
		LogActivity(logCtx, h.broadcaster, ActivityParams{
			WorkspaceID:  workspaceID,
			ActivityType: "a2a_receive",
			SourceID:     nilIfEmpty(callerID),
			TargetID:     &workspaceID,
			Method:       &a2aMethod,
			Summary:      &summary,
			RequestBody:  json.RawMessage(body),
			DurationMs:   &durationMs,
			Status:       "error",
			ErrorDetail:  &errMsg,
		})
	}(ctx)
}

// logA2ASuccess records a successful A2A round-trip and (for canvas-initiated
// 2xx/3xx responses) broadcasts an A2A_RESPONSE event so the frontend can
// receive the reply without polling.
func (h *WorkspaceHandler) logA2ASuccess(ctx context.Context, workspaceID, callerID string, body, respBody []byte, a2aMethod string, statusCode, durationMs int) {
	logStatus := "ok"
	if statusCode >= 400 {
		logStatus = "error"
	}
	var wsNameForLog string
	db.DB.QueryRowContext(ctx, `SELECT name FROM workspaces WHERE id = $1`, workspaceID).Scan(&wsNameForLog)
	if wsNameForLog == "" {
		wsNameForLog = workspaceID
	}

	// #817: track outbound activity on the CALLER so orchestrators can detect
	// silent workspaces. Only update when callerID is a real workspace (not
	// canvas, not a system caller) and the target returned 2xx/3xx.
	if callerID != "" && !isSystemCaller(callerID) && statusCode < 400 {
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := db.DB.ExecContext(bgCtx,
				`UPDATE workspaces SET last_outbound_at = NOW() WHERE id = $1`, callerID); err != nil {
				log.Printf("last_outbound_at update failed for %s: %v", callerID, err)
			}
		}()
	}
	summary := a2aMethod + " → " + wsNameForLog
	toolTrace := extractToolTrace(respBody)
	go func(parent context.Context) {
		logCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
		defer cancel()
		LogActivity(logCtx, h.broadcaster, ActivityParams{
			WorkspaceID:  workspaceID,
			ActivityType: "a2a_receive",
			SourceID:     nilIfEmpty(callerID),
			TargetID:     &workspaceID,
			Method:       &a2aMethod,
			Summary:      &summary,
			RequestBody:  json.RawMessage(body),
			ResponseBody: json.RawMessage(respBody),
			ToolTrace:    toolTrace,
			DurationMs:   &durationMs,
			Status:       logStatus,
		})
	}(ctx)

	if callerID == "" && statusCode < 400 {
		h.broadcaster.BroadcastOnly(workspaceID, string(events.EventA2AResponse), map[string]interface{}{
			"response_body": json.RawMessage(respBody),
			"method":        a2aMethod,
			"duration_ms":   durationMs,
		})
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// validateCallerToken enforces the Phase 30.5 auth-token contract on the
// caller of an A2A proxy request. Same lazy-bootstrap shape as
// registry.requireWorkspaceToken: if the caller workspace has any live
// token on file, the Authorization header is mandatory and must match;
// if the caller has zero live tokens, they're grandfathered through
// (their next /registry/register will mint their first token, after
// which this branch never fires again for them).
//
// On auth failure this writes the 401 via c and returns an error so the
// handler aborts without running the proxy.
func validateCallerToken(ctx context.Context, c *gin.Context, callerID string) error {
	hasLive, err := wsauth.HasAnyLiveToken(ctx, db.DB, callerID)
	if err != nil {
		// Fail-open here matches the heartbeat path — A2A caller auth is
		// defense-in-depth on top of access-control hierarchy, not the
		// sole gate on the secret material. A DB hiccup shouldn't take
		// the whole A2A path down.
		log.Printf("wsauth: caller HasAnyLiveToken(%s) failed: %v — allowing A2A", callerID, err)
		return nil
	}
	if !hasLive {
		return nil // legacy / pre-upgrade caller
	}
	tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
	if tok == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing caller auth token"})
		return errInvalidCallerToken
	}
	if err := wsauth.ValidateToken(ctx, db.DB, callerID, tok); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid caller auth token"})
		return err
	}
	return nil
}

// errInvalidCallerToken is a sentinel for validateCallerToken's "missing
// token" branch so the handler-level guard can detect it without string
// matching (the wsauth errors are typed for the invalid case).
var errInvalidCallerToken = errors.New("missing caller auth token")

// extractToolTrace pulls metadata.tool_trace from an A2A JSON-RPC response.
// Returns nil when absent or malformed — callers can pass it straight through.
func extractToolTrace(respBody []byte) json.RawMessage {
	if len(respBody) == 0 {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &top); err != nil {
		return nil
	}
	rawResult, ok := top["result"]
	if !ok {
		return nil
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(rawResult, &result); err != nil {
		return nil
	}
	rawMeta, ok := result["metadata"]
	if !ok {
		return nil
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		return nil
	}
	trace, ok := meta["tool_trace"]
	if !ok || len(trace) == 0 {
		return nil
	}
	return trace
}

// extractAndUpsertTokenUsage parses LLM usage from a raw A2A response body
// and persists it via upsertTokenUsage. Safe to call in a goroutine — logs
// errors but never panics. ctx must already be detached from the request.
func extractAndUpsertTokenUsage(ctx context.Context, workspaceID string, respBody []byte) {
	in, out := parseUsageFromA2AResponse(respBody)
	if in > 0 || out > 0 {
		upsertTokenUsage(ctx, workspaceID, in, out)
	}
}

// parseUsageFromA2AResponse extracts input_tokens / output_tokens from an A2A
// JSON-RPC response. Inspects two locations in order of preference:
//  1. result.usage — the JSON-RPC 2.0 result envelope from workspace agents.
//  2. usage — top-level, for non-JSON-RPC or direct Anthropic-shaped payloads.
//
// Returns (0, 0) when no recognisable usage data is found.
func parseUsageFromA2AResponse(body []byte) (inputTokens, outputTokens int64) {
	if len(body) == 0 {
		return 0, 0
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return 0, 0
	}

	// 1. result.usage (JSON-RPC 2.0 wrapper produced by workspace agents).
	if rawResult, ok := top["result"]; ok {
		var result map[string]json.RawMessage
		if err := json.Unmarshal(rawResult, &result); err == nil {
			if in, out, ok := readUsageMap(result); ok {
				return in, out
			}
		}
	}

	// 2. Fallback: top-level usage (direct Anthropic or non-JSON-RPC response).
	if in, out, ok := readUsageMap(top); ok {
		return in, out
	}
	return 0, 0
}

// lookupDeliveryMode returns the workspace's delivery_mode. On any DB
// error or missing row it returns DeliveryModePush — the fail-closed
// default. "Closed" here means "fall back to today's behavior (synchronous
// dispatch)" rather than "fall back to drop the request silently into
// activity_logs where the agent might never see it." A poll-mode workspace
// that briefly reads as push will get its A2A request dispatched to the
// stored URL (or a 502 if no URL); a push-mode workspace that briefly
// reads as poll would get its request silently queued with no dispatch.
// The first failure is loud + recoverable; the second is silent.
//
// The function is intentionally lookup-only — it never mutates the row.
// The register handler (registry.go) is the only writer for delivery_mode.
//
// See #2339 PR 1 for the column + register-flow side; this is the
// proxy-side read used for the short-circuit in proxyA2ARequest.
func lookupDeliveryMode(ctx context.Context, workspaceID string) string {
	var mode sql.NullString
	err := db.DB.QueryRowContext(ctx,
		`SELECT delivery_mode FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&mode)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("ProxyA2A: lookupDeliveryMode(%s) failed (%v) — defaulting to push", workspaceID, err)
		}
		return models.DeliveryModePush
	}
	if !mode.Valid || mode.String == "" {
		return models.DeliveryModePush
	}
	if !models.IsValidDeliveryMode(mode.String) {
		log.Printf("ProxyA2A: workspace %s has invalid delivery_mode=%q — defaulting to push", workspaceID, mode.String)
		return models.DeliveryModePush
	}
	return mode.String
}

// logA2AReceiveQueued records a poll-mode "queued" A2A receive into
// activity_logs. Same shape as logA2ASuccess but without ResponseBody
// (there is no response yet — the polling agent will produce one when
// it picks the request up). status="ok" because the request was
// successfully queued; the consume side reports its own outcome.
//
// The activity_logs row is what the polling agent's GET /activity?since_id=
// reads in PR 3 — that's how a poll-mode workspace receives inbound A2A
// without a public URL.
func (h *WorkspaceHandler) logA2AReceiveQueued(ctx context.Context, workspaceID, callerID string, body []byte, a2aMethod string) {
	var wsName string
	db.DB.QueryRowContext(ctx, `SELECT name FROM workspaces WHERE id = $1`, workspaceID).Scan(&wsName)
	if wsName == "" {
		wsName = workspaceID
	}
	summary := a2aMethod + " → " + wsName + " (queued for poll)"
	go func(parent context.Context) {
		logCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
		defer cancel()
		LogActivity(logCtx, h.broadcaster, ActivityParams{
			WorkspaceID:  workspaceID,
			ActivityType: "a2a_receive",
			SourceID:     nilIfEmpty(callerID),
			TargetID:     &workspaceID,
			Method:       &a2aMethod,
			Summary:      &summary,
			RequestBody:  json.RawMessage(body),
			Status:       "ok",
		})
	}(ctx)
}

// readUsageMap extracts input_tokens / output_tokens from the "usage" key of m.
// Returns (0, 0, false) when the key is absent or contains no non-zero values.
func readUsageMap(m map[string]json.RawMessage) (inputTokens, outputTokens int64, ok bool) {
	rawUsage, has := m["usage"]
	if !has {
		return 0, 0, false
	}
	var usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(rawUsage, &usage); err != nil {
		return 0, 0, false
	}
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return 0, 0, false
	}
	return usage.InputTokens, usage.OutputTokens, true
}
