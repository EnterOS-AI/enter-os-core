package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
	"github.com/gin-gonic/gin"
)

// blockedRange is a named CIDR block so the conditional blocklist in
// validateAgentURL reads as a slice of homogeneous values instead of
// repeated anonymous struct literals.
type blockedRange struct {
	cidr  string
	label string
}

// saasMode reports whether this tenant platform is running in SaaS cross-EC2
// mode, where workspaces live on sibling EC2s in the same VPC and register
// themselves by their RFC-1918 VPC-private IP (typically 172.31.x.x on AWS
// default VPCs). In that shape, the SSRF hardening that blocks RFC-1918
// addresses would reject every legitimate workspace registration — the
// control plane provisioned these instances, so their intra-VPC URLs are
// trusted by construction.
//
// Resolution order:
//  1. MOLECULE_DEPLOY_MODE set — explicit operator flag is authoritative.
//     Recognised values: "saas" → true. "self-hosted" / "selfhosted" /
//     "standalone" → false. Any other non-empty value logs a warning and
//     falls closed (false) so a typo like MOLECULE_DEPLOY_MODE=prod can't
//     silently flip a self-hosted deployment into the relaxed SSRF posture.
//  2. MOLECULE_DEPLOY_MODE unset — fall back to the MOLECULE_ORG_ID presence
//     signal for deployments that predate the explicit flag.
//
// Self-hosted / single-container deployments set neither and keep the strict
// blocklist.
func saasMode() bool {
	raw := os.Getenv("MOLECULE_DEPLOY_MODE")
	trimmed := strings.TrimSpace(raw)
	if trimmed != "" {
		switch strings.ToLower(trimmed) {
		case "saas":
			return true
		case "self-hosted", "selfhosted", "standalone":
			return false
		default:
			// Warn-once so operators notice the typo without spamming logs.
			saasModeWarnUnknownOnce.Do(func() {
				log.Printf("saasMode: MOLECULE_DEPLOY_MODE=%q not recognised; falling back to strict (non-SaaS) mode. Valid values: saas | self-hosted.", raw)
			})
			return false
		}
	}
	return strings.TrimSpace(os.Getenv("MOLECULE_ORG_ID")) != ""
}

var saasModeWarnUnknownOnce sync.Once

// QueueDrainFunc dispatches one queued A2A item on behalf of the caller.
// Injected at construction to avoid a WorkspaceHandler import cycle in
// RegistryHandler. Called from a goroutine spawned inside Heartbeat when
// the workspace reports spare capacity (#1870 Phase 1).
type QueueDrainFunc func(ctx context.Context, workspaceID string)

type RegistryHandler struct {
	broadcaster *events.Broadcaster
	drainQueue  QueueDrainFunc // nil-safe: Heartbeat skips drain when unset
}

func NewRegistryHandler(b *events.Broadcaster) *RegistryHandler {
	return &RegistryHandler{broadcaster: b}
}

// SetQueueDrainFunc wires the drain hook. Router wires this to
// WorkspaceHandler.DrainQueueForWorkspace after both are constructed, which
// keeps RegistryHandler's import list clean.
func (h *RegistryHandler) SetQueueDrainFunc(f QueueDrainFunc) {
	h.drainQueue = f
}

// validateAgentURL rejects URLs that could be used as SSRF vectors against
// cloud metadata services or other internal infrastructure.
//
// Allowed: http:// or https:// only (no file://, ftp://, etc.).
// Allowed: public routable addresses and DNS hostnames (including "localhost").
//
// Blocked IP ranges — agents MUST register using DNS hostnames, not IP literals:
//   - 169.254.0.0/16  link-local — AWS/GCP/Azure metadata (IMDSv1/v2)
//   - 127.0.0.0/8     loopback   — self-SSRF: redirects A2A traffic back to platform
//   - 10.0.0.0/8      RFC-1918   — lateral movement within private networks
//   - 172.16.0.0/12   RFC-1918   — includes Docker bridge/overlay ranges
//   - 192.168.0.0/16  RFC-1918   — home/office LAN ranges
//   - fe80::/10        IPv6 link-local — same threat class as 169.254.x.x
//   - ::1/128          IPv6 loopback
//   - fc00::/7         IPv6 ULA (RFC-4193 private ranges)
//
// IPv4-mapped IPv6 (e.g. ::ffff:169.254.169.254) is normalised to IPv4 by
// Go's net.ParseIP.To4() before Contains() runs, so the IPv4 rules above
// catch those without a separate entry.
//
// F1083/#1130 (SSRF on mcpResolveURL / a2a_proxy resolveAgentURL): in
// addition to blocking IP literals, DNS names are now resolved and each
// returned IP is checked against the blocklist. This closes the gap where
// an attacker could register agent.example.com pointing to 169.254.169.254.
//
// Returns a non-nil error suitable for including in a 400 Bad Request response.
func validateAgentURL(rawURL string) error {
	if rawURL == "" {
		return errors.New("url is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("url is not valid: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https, got %q", parsed.Scheme)
	}
	hostname := parsed.Hostname()

	// Link-local / loopback / IPv6 metadata classes are blocked in every
	// mode — they are never a legitimate agent URL and they cover the AWS/
	// GCP/Azure IMDS endpoints. RFC-1918 ranges are conditionally blocked:
	// in SaaS mode workspaces register with their VPC-private IP and the
	// control plane is the source of truth for which instances exist, so
	// allowing 10/8, 172.16/12, 192.168/16 is safe. In self-hosted mode
	// we keep the strict blocklist — those deployments have no legitimate
	// reason to accept private-range URLs from agents.
	blockedRanges := []blockedRange{
		{"169.254.0.0/16", "link-local address (cloud metadata endpoint)"},
		{"127.0.0.0/8", "loopback address"},
		{"fe80::/10", "IPv6 link-local address (cloud metadata analogue)"},
		{"::1/128", "IPv6 loopback address"},
		// Always-blocked regardless of deploy mode: these ranges are never valid
		// agent URLs in any deployment. TEST-NET (RFC-5737) are documentation-only
		// ranges. CGNAT (RFC-6598) is never used for VPC subnets on any cloud
		// provider. IPv4 multicast is never a unicast endpoint. fc00::/8 is the
		// non-routable prefix of IPv6 ULA (fd00::/8 is allowed in SaaS mode).
		// RFC 3849: 2001:db8::/32 is the IPv6 documentation prefix.
		{"192.0.2.0/24", "TEST-NET-1 documentation range (RFC-5737)"},
		{"198.51.100.0/24", "TEST-NET-2 documentation range (RFC-5737)"},
		{"203.0.113.0/24", "TEST-NET-3 documentation range (RFC-5737)"},
		{"100.64.0.0/10", "carrier-grade NAT address (RFC-6598)"},
		{"224.0.0.0/4", "IPv4 multicast address"},
		{"fc00::/8", "IPv6 ULA non-routable prefix (fc00::/8)"},
		{"2001:db8::/32", "IPv6 documentation address (RFC-3849 reserved)"},
	}
	if !saasMode() {
		blockedRanges = append(blockedRanges,
			blockedRange{"10.0.0.0/8", "RFC-1918 private address"},
			blockedRange{"172.16.0.0/12", "RFC-1918 private address"},
			blockedRange{"192.168.0.0/16", "RFC-1918 private address"},
			// In SaaS mode fd00::/8 (common ULA prefix) is allowed for VPC-internal
			// routing. fc00::/8 is already always-blocked above. In non-SaaS mode
			// block the entire fc00::/7 supernet (covers both fd00 and fc00).
			blockedRange{"fd00::/8", "IPv6 ULA address (RFC-4193 private)"},
		)
	}

	// Helper: check a single IP against the blocklist.
	checkIP := func(ip net.IP) error {
		for _, r := range blockedRanges {
			_, network, _ := net.ParseCIDR(r.cidr)
			if network.Contains(ip) {
				return fmt.Errorf("url targets a blocked address: %s", r.label)
			}
		}
		return nil
	}

	if ip := net.ParseIP(hostname); ip != nil {
		// All private and reserved ranges are rejected. Agents must register
		// using DNS hostnames so the platform can reach them; raw IP literals
		// in registration payloads have no legitimate use case and enable SSRF.
		return checkIP(ip)
	}

	// "localhost" is allowed by name (no DNS lookup) — it is a standard dev-
	// environment alias for 127.0.0.1 and agents in local dev rely on it.
	// The existing test suite expects this behaviour to be preserved.
	if hostname == "localhost" {
		return nil
	}

	// F1083/#1130: hostname is a DNS name — resolve it and check each returned IP.
	// Skip the lookup if the hostname fails to resolve (network issues, etc.);
	// the agent won't be reachable anyway, so blocking on DNS failure is safe.
	ips, lookupErr := net.LookupIP(hostname)
	if lookupErr != nil {
		// DNS lookup failed — block the URL rather than allow a potentially-
		// unreachable or intentionally-unresolvable hostname through. The
		// platform has no use for a workspace it cannot reach.
		return fmt.Errorf("hostname %q cannot be resolved (DNS error): %w", hostname, lookupErr)
	}
	for _, ip := range ips {
		if err := checkIP(ip); err != nil {
			return fmt.Errorf("hostname %q resolves to forbidden address: %w", hostname, err)
		}
	}
	return nil
}

// Register handles POST /registry/register
// Upserts workspace, sets Redis TTL, broadcasts WORKSPACE_ONLINE.
func (h *RegistryHandler) Register(c *gin.Context) {
	var payload models.RegisterPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// C6: reject SSRF-capable URLs before persisting or caching them.
	if err := validateAgentURL(payload.URL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// C6: reject SSRF-capable URLs before persisting or caching them.
	if err := validateAgentURL(payload.URL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	// C18: prevent workspace URL hijacking on re-registration.
	//
	// An attacker can overwrite any workspace's agent_card URL by calling
	// /registry/register with that workspace's ID and their own URL, redirecting
	// all A2A messages to their server.
	//
	// Fix: if this workspace already has any live auth tokens on file, the caller
	// must prove they own it by supplying a valid bearer token in Authorization.
	// First-ever registration (no tokens yet) is bootstrap-allowed — the token
	// is issued at the end of this function. This mirrors the same pattern used
	// for /registry/heartbeat and /registry/update-card.
	if err := h.requireWorkspaceToken(ctx, c, payload.ID); err != nil {
		return // 401 response already written by requireWorkspaceToken
	}

	agentCardStr := string(payload.AgentCard)

	// Upsert workspace: update url, agent_card, status if already exists.
	// On INSERT (workspace not yet created via POST /workspaces), use ID as name placeholder.
	// Keep existing URL if provisioner already set a host-accessible one (starts with http://127.0.0.1).
	//
	// #73 guard: `WHERE workspaces.status IS DISTINCT FROM 'removed'` prevents
	// a late heartbeat from a workspace that was just deleted from resurrecting
	// the row. Without this guard, bulk deletes left tier-3 stragglers because
	// the last pre-teardown heartbeat flipped status back to 'online' after
	// Delete's UPDATE.
	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, url, agent_card, status, last_heartbeat_at)
		VALUES ($1, $2, $3, $4::jsonb, 'online', now())
		ON CONFLICT (id) DO UPDATE SET
			url = CASE
				WHEN workspaces.url LIKE 'http://127.0.0.1%' THEN workspaces.url
				ELSE EXCLUDED.url
			END,
			agent_card = EXCLUDED.agent_card,
			status = 'online',
			last_heartbeat_at = now(),
			updated_at = now()
		WHERE workspaces.status IS DISTINCT FROM 'removed'
	`, payload.ID, payload.ID, payload.URL, agentCardStr)
	if err != nil {
		log.Printf("Registry register error: %v (id=%s)", err, payload.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "registration failed"})
		return
	}

	// Set Redis liveness key
	if err := db.SetOnline(ctx, payload.ID); err != nil {
		log.Printf("Registry redis error: %v", err)
	}

	// Cache URL — prefer existing provisioner URL over agent-reported one.
	// The DB CASE already preserves provisioner URLs, so read from DB as source of truth
	// instead of adding a Redis round-trip on every registration.
	cachedURL := payload.URL
	var dbURL string
	if err := db.DB.QueryRowContext(ctx, `SELECT url FROM workspaces WHERE id = $1`, payload.ID).Scan(&dbURL); err == nil {
		if strings.HasPrefix(dbURL, "http://127.0.0.1") {
			cachedURL = dbURL
		}
	}
	if err := db.CacheURL(ctx, payload.ID, cachedURL); err != nil {
		log.Printf("Registry cache url error: %v", err)
	}

	// Cache agent-reported URL separately for workspace-to-workspace discovery
	// (Docker containers can reach each other by hostname but not via host ports)
	if err := db.CacheInternalURL(ctx, payload.ID, payload.URL); err != nil {
		log.Printf("Registry cache internal url error: %v", err)
	}

	// Broadcast WORKSPACE_ONLINE
	if err := h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_ONLINE", payload.ID, map[string]interface{}{
		"url":        cachedURL,
		"agent_card": payload.AgentCard,
	}); err != nil {
		log.Printf("Registry broadcast error: %v", err)
	}

	// Phase 30.1: issue a workspace auth token on first registration.
	//
	// On re-registration (agent restart), we DON'T issue a new token —
	// the agent is expected to keep the one it got the first time.
	// Issuing on every register would flood the table and make log
	// forensics noisier than it needs to be.
	//
	// Legacy workspaces that registered before tokens existed have no
	// live token; they bootstrap one here on their next register call.
	// New workspaces always pass through this path on their first boot.
	response := gin.H{"status": "registered"}
	if hasLive, hasLiveErr := wsauth.HasAnyLiveToken(ctx, db.DB, payload.ID); hasLiveErr == nil && !hasLive {
		token, tokErr := wsauth.IssueToken(ctx, db.DB, payload.ID)
		if tokErr != nil {
			// Don't fail the whole register on token-issuance error — the
			// agent is already online per the upsert above. Log and continue.
			// If needed, the agent can call /registry/register again and
			// we'll retry issuance. Alternative paths (/workspaces/:id/
			// tokens POST, to be added in a later phase) can also mint one.
			log.Printf("Registry: failed to issue auth token for %s: %v", payload.ID, tokErr)
		} else {
			response["auth_token"] = token
		}
	} else if hasLiveErr != nil {
		log.Printf("Registry: token existence check failed for %s: %v", payload.ID, hasLiveErr)
	}

	c.JSON(http.StatusOK, response)
}

// Heartbeat handles POST /registry/heartbeat
func (h *RegistryHandler) Heartbeat(c *gin.Context) {
	var payload models.HeartbeatPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	ctx := c.Request.Context()

	// Phase 30.1: require a valid workspace auth token on every heartbeat
	// IF the workspace has any live tokens on file. Legacy workspaces that
	// registered before tokens existed are grandfathered through (tokens
	// get issued on their next /registry/register call); new workspaces
	// always have one. This design lets us ship auth without forcing a
	// synchronized restart of every running workspace.
	if err := h.requireWorkspaceToken(ctx, c, payload.WorkspaceID); err != nil {
		return // response already written
	}

	// Read previous current_task to detect changes (before the UPDATE)
	var prevTask string
	_ = db.DB.QueryRowContext(ctx, `SELECT COALESCE(current_task, '') FROM workspaces WHERE id = $1`, payload.WorkspaceID).Scan(&prevTask)

	// #615: Clamp monthly_spend to a safe range before any DB write.
	// A malicious or buggy agent could report math.MaxInt64, causing
	// NUMERIC overflow or incorrect budget-enforcement comparisons.
	// Negatives are meaningless (spend is always ≥ 0); the upper cap of
	// $10 billion in cents is an intentionally astronomical value that no
	// legitimate workspace will ever reach.
	const maxMonthlySpend = int64(1_000_000_000_000) // $10B in cents
	if payload.MonthlySpend < 0 {
		payload.MonthlySpend = 0
	}
	if payload.MonthlySpend > maxMonthlySpend {
		payload.MonthlySpend = maxMonthlySpend
	}

	// Update heartbeat columns. #73 guard: exclude 'removed' rows so a
	// late heartbeat from a container that's being torn down doesn't
	// refresh last_heartbeat_at on a tombstoned workspace (which would
	// otherwise confuse the liveness monitor).
	//
	// monthly_spend: updated when the agent reports a positive value (cumulative
	// USD cents for the current month). Zero means "no update" — never write
	// zero to avoid accidentally clearing a previously-reported spend value.
	var err error
	if payload.MonthlySpend > 0 {
		_, err = db.DB.ExecContext(ctx, `
			UPDATE workspaces SET
				last_heartbeat_at = now(),
				last_error_rate   = $2,
				last_sample_error = $3,
				active_tasks      = $4,
				uptime_seconds    = $5,
				current_task      = $6,
				monthly_spend     = $7,
				updated_at        = now()
			WHERE id = $1 AND status != 'removed'
		`, payload.WorkspaceID, payload.ErrorRate, payload.SampleError,
			payload.ActiveTasks, payload.UptimeSeconds, payload.CurrentTask,
			payload.MonthlySpend)
	} else {
		_, err = db.DB.ExecContext(ctx, `
			UPDATE workspaces SET
				last_heartbeat_at = now(),
				last_error_rate   = $2,
				last_sample_error = $3,
				active_tasks      = $4,
				uptime_seconds    = $5,
				current_task      = $6,
				updated_at        = now()
			WHERE id = $1 AND status != 'removed'
		`, payload.WorkspaceID, payload.ErrorRate, payload.SampleError,
			payload.ActiveTasks, payload.UptimeSeconds, payload.CurrentTask)
	}
	if err != nil {
		log.Printf("Heartbeat update error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update"})
		return
	}

	// Refresh Redis TTL
	if err := db.RefreshTTL(ctx, payload.WorkspaceID); err != nil {
		log.Printf("Heartbeat redis error: %v", err)
	}

	// Evaluate status transitions
	h.evaluateStatus(c, payload)

	// Broadcast current task update only when it changed (avoid spamming on every heartbeat)
	if payload.CurrentTask != prevTask {
		h.broadcaster.BroadcastOnly(payload.WorkspaceID, "TASK_UPDATED", map[string]interface{}{
			"current_task": payload.CurrentTask,
			"active_tasks": payload.ActiveTasks,
		})
	}

	// Always emit a lightweight heartbeat broadcast — load-bearing for
	// the a2a-proxy's per-dispatch idle timeout (a2a_proxy.go:applyIdleTimeout).
	// Before this, the proxy's idle timer reset on TASK_UPDATED but
	// TASK_UPDATED only fires when current_task CHANGES. A long-running
	// agent that keeps the same task value for >idleTimeoutDuration
	// (claude-code packaging a ZIP, slow tool call, model thinking time)
	// hit no broadcast → idle timer fired → user's message got cancelled
	// mid-flight with "context canceled". Symptom users hit on the
	// 2026-04-26 director-bypass investigation: 15+ failures in 1hr
	// across 6 workspaces, all silent during the gap.
	//
	// Cost: BroadcastOnly skips the DB write (no activity_logs row),
	// so per-heartbeat cost is one in-memory channel send per active
	// SSE subscriber and one WS hub fan-out. At 30s heartbeat cadence
	// this is far below any noise floor on either path.
	h.broadcaster.BroadcastOnly(payload.WorkspaceID, "WORKSPACE_HEARTBEAT", map[string]interface{}{
		"active_tasks":   payload.ActiveTasks,
		"uptime_seconds": payload.UptimeSeconds,
	})

	// Refresh per-workspace runtime overrides from the heartbeat's
	// runtime_metadata block (introduced for the native+pluggable
	// runtime principle — see project memory). Both idle_timeout_seconds
	// and capability flags are stored. Each consumer (a2a_proxy.dispatchA2A
	// for idle timeout, scheduler.tick for native scheduler, etc.) reads
	// what it needs from the cache. nil RuntimeMetadata or absent field
	// clears the corresponding override so the dispatch path uses the
	// global default.
	if payload.RuntimeMetadata != nil && payload.RuntimeMetadata.IdleTimeoutSeconds != nil {
		runtimeOverrides.SetIdleTimeout(
			payload.WorkspaceID,
			time.Duration(*payload.RuntimeMetadata.IdleTimeoutSeconds)*time.Second,
		)
	} else {
		runtimeOverrides.SetIdleTimeout(payload.WorkspaceID, 0) // clear
	}
	if payload.RuntimeMetadata != nil {
		runtimeOverrides.SetCapabilities(payload.WorkspaceID, payload.RuntimeMetadata.Capabilities)
	} else {
		runtimeOverrides.SetCapabilities(payload.WorkspaceID, nil) // clear
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *RegistryHandler) evaluateStatus(c *gin.Context, payload models.HeartbeatPayload) {
	ctx := c.Request.Context()

	var currentStatus string
	err := db.DB.QueryRowContext(ctx, `SELECT status FROM workspaces WHERE id = $1`, payload.WorkspaceID).
		Scan(&currentStatus)
	if err != nil {
		return
	}

	// Self-reported runtime wedge: takes precedence over the error_rate
	// path. The heartbeat task lives in its own asyncio task and keeps
	// firing 200s even after claude_agent_sdk locks up on
	// `Control request timeout: initialize` — so error_rate stays at 0
	// (no calls have been recorded as errors yet) while every actual
	// /a2a POST hangs. The workspace tells us about that case via
	// runtime_state="wedged"; we honor it directly. Sample_error from
	// the heartbeat carries the human-readable reason ("SDK init
	// timeout — restart workspace"), which the canvas surfaces in the
	// degraded card without the operator scraping container logs.
	if payload.RuntimeState == "wedged" && currentStatus == "online" {
		_, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET status = 'degraded', updated_at = now() WHERE id = $1 AND status = 'online'`,
			payload.WorkspaceID)
		if err != nil {
			log.Printf("Heartbeat: failed to mark %s degraded (wedged): %v", payload.WorkspaceID, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_DEGRADED", payload.WorkspaceID, map[string]interface{}{
			"runtime_state": "wedged",
			"sample_error":  payload.SampleError,
		})
	}

	// Skip the inferred-status branches when the adapter has declared
	// native_status_mgmt — its SDK reports its own ready/degraded/failed
	// state explicitly (typically via runtime_state above), and inferring
	// status from error_rate would fight that. Capability primitive #4
	// (task #117) — see project memory `project_runtime_native_pluggable.md`.
	//
	// The wedged-branch above (RuntimeState == "wedged") is NOT skipped:
	// it's the adapter's own self-report, not an inference. Adapters with
	// native_status_mgmt can keep using runtime_state to drive transitions.
	nativeStatus := runtimeOverrides.HasCapability(payload.WorkspaceID, "status_mgmt")

	if !nativeStatus && currentStatus == "online" && payload.ErrorRate >= 0.5 {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = 'degraded', updated_at = now() WHERE id = $1`, payload.WorkspaceID); err != nil {
			log.Printf("Heartbeat: failed to mark %s degraded: %v", payload.WorkspaceID, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_DEGRADED", payload.WorkspaceID, map[string]interface{}{
			"error_rate":   payload.ErrorRate,
			"sample_error": payload.SampleError,
		})
	}

	// Recovery from degraded → online when BOTH the error rate has
	// fallen back AND the workspace is no longer reporting a wedge.
	// The wedge condition is sticky for the process lifetime
	// (claude_sdk_executor only clears it on restart), so when the
	// container restarts and starts heartbeating fresh — RuntimeState
	// is empty, error_rate is 0 — this branch flips us back to online.
	//
	// Skipped under native_status_mgmt for the same reason as the
	// degrade branch above: the adapter owns the transition.
	if !nativeStatus && currentStatus == "degraded" && payload.ErrorRate < 0.1 && payload.RuntimeState == "" {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = 'online', updated_at = now() WHERE id = $1`, payload.WorkspaceID); err != nil {
			log.Printf("Heartbeat: failed to recover %s to online: %v", payload.WorkspaceID, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_ONLINE", payload.WorkspaceID, map[string]interface{}{})
	}

	// Recovery: if workspace was offline but is now sending heartbeats, bring it back online.
	// #73 guard: `AND status = 'offline'` makes the flip conditional in a single statement,
	// so a Delete that races with this recovery can't flip 'removed' back to 'online'.
	if currentStatus == "offline" {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = 'online', updated_at = now() WHERE id = $1 AND status = 'offline'`, payload.WorkspaceID); err != nil {
			log.Printf("Heartbeat: failed to recover %s from offline: %v", payload.WorkspaceID, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_ONLINE", payload.WorkspaceID, map[string]interface{}{})
	}

	// Auto-recovery: if a workspace is marked "provisioning" but is actively sending
	// heartbeats, it has successfully started up. Transition to "online" so the scheduler
	// and A2A proxy can dispatch tasks to it. The provisioner does not call
	// /registry/register on container start — only the heartbeat loop does, so this
	// transition is the only mechanism that moves newly-started workspaces out of
	// the phantom-idle state. (#1784)
	if currentStatus == "provisioning" {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = 'online', updated_at = now() WHERE id = $1 AND status = 'provisioning'`, payload.WorkspaceID); err != nil {
			log.Printf("Heartbeat: failed to transition %s from provisioning to online: %v", payload.WorkspaceID, err)
		} else {
			log.Printf("Heartbeat: transitioned %s from provisioning to online (heartbeat received)", payload.WorkspaceID)
		}
		h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_ONLINE", payload.WorkspaceID, map[string]interface{}{
			"recovered_from": currentStatus,
		})
	}

	// #1870 Phase 1: drain one queued A2A request if the target reports
	// spare capacity. The heartbeat's active_tasks field reflects what the
	// workspace runtime is ACTUALLY running right now, independent of
	// whatever we've counted server-side. Fire-and-forget goroutine — the
	// drain dispatches via ProxyA2ARequest which already has its own
	// timeouts, retry logic, and activity_logs wiring.
	if h.drainQueue != nil {
		var maxConcurrent int
		_ = db.DB.QueryRowContext(ctx,
			`SELECT COALESCE(max_concurrent_tasks, 1) FROM workspaces WHERE id = $1`,
			payload.WorkspaceID,
		).Scan(&maxConcurrent)
		if payload.ActiveTasks < maxConcurrent {
			// context.WithoutCancel: heartbeat handler's ctx is about to
			// expire as soon as we return. The drain needs to outlive it.
			drainCtx := context.WithoutCancel(ctx)
			go h.drainQueue(drainCtx, payload.WorkspaceID)
		}
	}
}

// UpdateCard handles POST /registry/update-card
func (h *RegistryHandler) UpdateCard(c *gin.Context) {
	var payload models.UpdateCardPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Phase 30.1 — same bootstrap-aware token gate as Heartbeat.
	if err := h.requireWorkspaceToken(c.Request.Context(), c, payload.WorkspaceID); err != nil {
		return // response already written
	}

	agentCardStr := string(payload.AgentCard)
	_, err := db.DB.ExecContext(c.Request.Context(), `
		UPDATE workspaces SET agent_card = $2::jsonb, updated_at = now() WHERE id = $1
	`, payload.WorkspaceID, agentCardStr)
	if err != nil {
		log.Printf("UpdateCard error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update card"})
		return
	}

	h.broadcaster.RecordAndBroadcast(c.Request.Context(), "AGENT_CARD_UPDATED", payload.WorkspaceID, map[string]interface{}{
		"agent_card": payload.AgentCard,
	})

	c.JSON(http.StatusOK, gin.H{"status": "updated"})
}

// requireWorkspaceToken enforces the Phase 30.1 auth-token contract on an
// inbound registry request (heartbeat / update-card today).
//
// The function has two distinct behaviours gated on whether the workspace
// has any live tokens on file:
//
//   - workspace has at least one live token → Authorization: Bearer <token>
//     is mandatory. Missing / malformed / wrong-workspace → 401.
//   - workspace has zero live tokens → grandfathered. We let the request
//     through and log a single DEBUG line. The agent's next
//     /registry/register call will mint its first token, after which this
//     branch never fires again for that workspace.
//
// Returns a non-nil error (and writes the 401 response via c) when the
// caller should abort. A nil return means the handler may continue.
//
// SECURITY NOTE: the grandfathering path is only safe during the
// transition window. Once every running workspace has re-registered
// post-upgrade, step 30.5 flips this to hard-require.
func (h *RegistryHandler) requireWorkspaceToken(
	ctx gincontext, c *gin.Context, workspaceID string,
) error {
	hasLive, err := wsauth.HasAnyLiveToken(ctx, db.DB, workspaceID)
	if err != nil {
		// DB error checking token existence — fail open so we don't take
		// the whole heartbeat path down on a transient hiccup. Log loudly.
		log.Printf("wsauth: HasAnyLiveToken(%s) failed: %v — allowing request", workspaceID, err)
		return nil
	}
	if !hasLive {
		// Legacy / pre-upgrade workspace. Next register issues a token.
		return nil
	}
	token := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing workspace auth token"})
		return errors.New("missing token")
	}
	if err := wsauth.ValidateToken(ctx, db.DB, workspaceID, token); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid workspace auth token"})
		return err
	}
	return nil
}

// gincontext is an alias for context.Context kept separate so callers can
// see "gin.Context.Request.Context() is what we want" without re-typing
// the import-heavy standard type.
type gincontext = context.Context
