package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
)

// isProvisionerHostPortURL reports whether u is a loopback URL with a port
// other than 8000. Such URLs are written by the provisioner (via
// MOLECULE_WORKSPACE_URL) and should be preserved across runtime registrations.
// The 8000 fallback is the runtime's own default and is allowed to be
// overwritten.
func isProvisionerHostPortURL(u string) bool {
	if u == "" {
		return false
	}
	if !strings.HasPrefix(u, "http://127.0.0.1:") && !strings.HasPrefix(u, "http://localhost:") {
		return false
	}
	// Extract the trailing port.
	port := u[strings.LastIndex(u, ":")+1:]
	if port == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	return n != 8000
}

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
// the workspace reports spare capacity (#1870 Phase 1), and from the
// periodic A2A queue sweeper (#2930).
type QueueDrainFunc func(ctx context.Context, workspaceID string, capacity int)

type RegistryHandler struct {
	broadcaster *events.Broadcaster
	drainQueue  QueueDrainFunc // nil-safe: Heartbeat skips drain when unset
	// reconcilePlugins installs declared-but-missing plugins when a workspace
	// transitions to online (RFC#2843 #32). nil-safe: Heartbeat skips the
	// reconcile when unset (e.g. unit tests, CP/SaaS mode without a plugins
	// handler). Wired by the router to PluginsHandler.ReconcileWorkspacePlugins.
	reconcilePlugins ReconcileFunc
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

// SetReconcileFunc wires the post-online plugin reconcile hook (RFC#2843).
// Router wires this to PluginsHandler.ReconcileWorkspacePlugins after both
// handlers are constructed (same late-wiring pattern as SetQueueDrainFunc),
// keeping RegistryHandler free of a plugins-handler import.
func (h *RegistryHandler) SetReconcileFunc(f ReconcileFunc) {
	h.reconcilePlugins = f
}

// fireReconcileOnline fires the declared-plugin reconcile for a workspace that
// has just transitioned to online. Fire-and-forget via globalGoAsync so the
// heartbeat handler returns immediately; the reconcile owns its own deadline.
// nil-safe + uses context.WithoutCancel because the heartbeat ctx expires when
// the handler returns, well before a plugin clone+deliver completes.
func (h *RegistryHandler) fireReconcileOnline(ctx context.Context, workspaceID string) {
	if h.reconcilePlugins == nil {
		return
	}
	rctx := context.WithoutCancel(ctx)
	wsID := workspaceID
	globalGoAsync(func() { h.reconcilePlugins(rctx, wsID) })
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
// F1083/#1130 (SSRF on direct A2A URL resolution): in
// addition to blocking IP literals, DNS names are now resolved and each
// returned IP is checked against the blocklist. This closes the gap where
// an attacker could register agent.example.com pointing to 169.254.169.254.
//
// resolveDeliveryMode returns the EFFECTIVE delivery mode for a register
// call given the payload's explicit value (which may be empty) and the
// row's existing stored value (which may not exist yet on first
// registration).
//
// Resolution order:
//  1. payload value if non-empty (caller validated it's push/poll already)
//  2. existing row's delivery_mode if the row exists
//  3. "poll" if the existing row's runtime is "external" — most external
//     operators run on a laptop without public HTTPS; poll is the
//     no-public-URL path. This default flipped 2026-04-30 (issue #10
//     in molecule-cli) when `molecule connect` shipped — push-mode
//     stays available via explicit payload.delivery_mode="push" for
//     VM/server operators who opt in.
//  4. "push" (the schema default — safe fallback for non-external
//     runtimes whose row exists with NULL delivery_mode, which is
//     forward-defensive only)
//
// Returns ("", err) only on a real DB error; sql.ErrNoRows is treated
// as "no row yet, default to push" — that's the first-register flow,
// and at that point we don't know the runtime yet so push is the
// historical compatible default.
func (h *RegistryHandler) resolveDeliveryMode(ctx context.Context, workspaceID, payloadMode string) (string, error) {
	if payloadMode != "" {
		// Validated by IsValidDeliveryMode in the caller.
		return payloadMode, nil
	}
	var existing sql.NullString
	var runtime sql.NullString
	err := db.DB.QueryRowContext(ctx,
		`SELECT delivery_mode, runtime FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&existing, &runtime)
	if errors.Is(err, sql.ErrNoRows) {
		return models.DeliveryModePush, nil
	}
	if err != nil {
		return "", err
	}
	if existing.Valid && existing.String != "" {
		return existing.String, nil
	}
	if runtime.Valid && isExternalLikeRuntime(runtime.String) {
		return models.DeliveryModePoll, nil
	}
	return models.DeliveryModePush, nil
}

// errPlatformNotRoot is the client-facing message when a register call tried to
// mark a non-root workspace as a platform agent.
const errPlatformNotRoot = "a platform agent must be the org root (parent_id must be null) and there can be only one per org"

// managementMCPUnloadedGrace is the startup/warmup grace window for the
// core#3082 management-MCP gate. The gate degrades a platform concierge whose
// declared management MCP is not present in the heartbeat's loaded_mcp_tools
// (or whose runtime omits the list entirely) ONLY once the absence has
// persisted continuously for at least this long — tracked via the
// workspaces.mcp_unloaded_since timestamp.
//
// Why a grace window: the management MCP connects asynchronously after the
// agent process starts, and the runtime can only observe its loaded tool list
// from a live turn. A heartbeat that fires before the first turn / before the
// MCP finishes connecting legitimately reports an absent/partial tool list;
// degrading on that single sample (then recovering on the next) is the
// ~50/50 online<->degraded flap this window eliminates. A genuinely-missing
// management MCP stays absent past the window and degrades — preserving the
// fail-closed RCA#2970 intent (steady-state guarantee: sustained-missing DOES
// degrade).
//
// 90s ≈ 4-5 heartbeats at the runtime's 20s cadence: comfortably longer than
// MCP warmup + first-turn latency, short enough that a real regression is
// surfaced within ~1.5 min.
const managementMCPUnloadedGrace = 90 * time.Second

// isPlatformRootViolation reports whether err is the DB rejecting a register
// that tried to mark a non-root workspace as a platform agent (the
// workspaces_platform_root_check CHECK constraint). The handler maps it to a
// friendly HTTP 409 instead of a raw 500. The invariant — platform == org root,
// which structurally also guarantees one platform agent per org — is enforced
// race-proof at the DB level; this is just the friendly surface.
func isPlatformRootViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// workspaces_platform_root_check: tried to mark a non-root (parented) row
	// platform. uniq_workspaces_one_platform_root: tried to create a SECOND
	// platform root. Both surface as a friendly 409 instead of a raw 500.
	return strings.Contains(msg, "workspaces_platform_root_check") ||
		strings.Contains(msg, "uniq_workspaces_one_platform_root")
}

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
		// #36/#2421: a freshly-provisioned CROSS-CLOUD workspace advertises its
		// per-workspace Cloudflare tunnel hostname (ws-<id>.<appDomain>). That DNS
		// record is eventually-consistent, and a FAST-booting box (a Hetzner cpx
		// reports "workspace ready after ~1s") registers BEFORE the record
		// propagates → the lookup fails → 400 → and the runtime does not retry a
		// 4xx → agent_card never lands and the agent never comes online. AWS boots
		// slowly enough to miss the race, which is why only the fast cloud broke.
		//
		// Such a hostname is NOT an SSRF vector: it lives under the platform's own
		// domain (only the platform can create records there, so it can't be
		// pointed at 169.254/127/private space by an attacker), and it resolves to
		// nothing right now. So in SaaS mode allow a platform-tunnel hostname
		// through while its DNS settles; everything else stays blocked. The
		// unconditional metadata/loopback blocks above still apply once it
		// resolves. (Restores the pre-#1130 "let an unresolvable platform URL
		// through" behaviour, scoped to the trusted tunnel domain.)
		if saasMode() && isPlatformTunnelHostname(hostname) {
			log.Printf("Registry validateAgentURL: allowing not-yet-resolvable platform tunnel hostname %q (DNS still propagating)", hostname)
			return nil
		}
		// DNS lookup failed for a non-platform hostname — block it. The platform
		// has no use for a workspace it cannot reach.
		return fmt.Errorf("hostname %q cannot be resolved (DNS error): %w", hostname, lookupErr)
	}
	for _, ip := range ips {
		if err := checkIP(ip); err != nil {
			return fmt.Errorf("hostname %q resolves to forbidden address: %w", hostname, err)
		}
	}
	return nil
}

// isPlatformTunnelHostname reports whether h is a platform-provisioned per-
// workspace Cloudflare tunnel hostname — `ws-<id>.<appDomain>` under the
// platform's OWN domain. Only the platform controls DNS there, so a not-yet-
// resolvable such hostname is a pending-DNS tunnel (DNS propagation race), never
// an attacker-controlled SSRF URL. The domain defaults to moleculesai.app
// (covers prod `*.moleculesai.app` and staging `*.staging.moleculesai.app`) and
// is overridable via MOLECULE_APP_DOMAIN for other deployments.
func isPlatformTunnelHostname(h string) bool {
	// Normalize: net/url's Hostname() does NOT lowercase and keeps a trailing dot,
	// so a legitimate `WS-…MOLECULESAI.APP` or FQDN-form `ws-x.moleculesai.app.`
	// would otherwise fail this case-sensitive match and get blocked (the exact
	// availability bug this allowance exists to cure). DNS is case-insensitive and
	// the trailing dot is the same name, so fold both before comparing.
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if !strings.HasPrefix(h, "ws-") {
		return false
	}
	domain := strings.ToLower(strings.TrimSpace(os.Getenv("MOLECULE_APP_DOMAIN")))
	if domain == "" {
		domain = "moleculesai.app"
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	return strings.HasSuffix(h, "."+domain)
}

// platformAgentHasModelSecret reports whether the workspace has a MODEL
// workspace_secret. The concierge's declared model is seeded by
// ensureConciergeModel before every platform-agent provision; a platform agent
// that reaches registration without this secret has not received its identity
// and must not be marked online.
func (h *RegistryHandler) platformAgentHasModelSecret(ctx context.Context, workspaceID string) (bool, error) {
	var exists bool
	err := db.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM workspace_secrets WHERE workspace_id = $1 AND key = 'MODEL')`,
		workspaceID).Scan(&exists)
	return exists, err
}

// platformAgentMCPServerPresent reports whether the concierge's MCP server
// should be considered present for the fail-closed online gate (#2970).
//
// The payload field is a pointer to distinguish three states:
//   - nil   → the runtime did NOT report the field at all. This is a runtime
//     PREDATING the #147 contract (mcp_server_present on register/heartbeat).
//     The concierge image bakes /opt/molecule-mcp-server unconditionally, so an
//     old runtime that simply can't SPEAK the contract must NOT be fail-closed —
//     that would take every concierge offline the moment this gate deploys ahead
//     of the runtime release that adds the field (the exact rollout-order hazard
//     that fail-closed #2989 + a pre-#147 concierge image hit on 2026-06-18:
//     test3 + the fleet would be marked failed despite a present MCP binary).
//     Treat nil as ALLOW (unknown ⇒ don't block) for backward-compat.
//   - &true  → runtime affirmatively reports the binary present → allow.
//   - &false → a #147-aware runtime affirmatively reports it ABSENT → fail-closed
//     (the real signal this gate exists to catch).
func (h *RegistryHandler) platformAgentMCPServerPresent(present *bool) bool {
	return present == nil || *present
}

// platformAgentManagementMCPLoaded reports whether the concierge's declared
// management MCP is actually loaded into the LLM's runtime tool list. It
// returns true (caller marks degraded) only when:
//   - the workspace has the management plugin declared in
//     workspace_declared_plugins (the install NAME conciergePlatformMCPName),
//     AND
//   - the reported loaded tool list does NOT contain the literal required
//     tool identifier (conciergePlatformMCPCreateWorkspaceTool).
//
// Why this checks the TOOL identifier and not the plugin name: the heartbeat's
// loaded_mcp_tools carries namespaced tool ids (`mcp__<server>__<tool>`), not
// plugin names. The management MCP's server is "molecule-platform" (the
// PluginNameFromSource derivation), so its create_workspace tool is
// "mcp__molecule-platform__create_workspace" — a different value from the
// plugin name "molecule-ai-plugin-molecule-platform-mcp". Comparing the
// plugin NAME against TOOL ids was a no-op false-green (CR2 #12653).
//
// If the management plugin is not declared (non-platform workspace, or a
// platform concierge before plugin reconciliation), it returns false (NOT
// missing) so we don't false-alarm on workspaces that legitimately don't
// declare it. Errors are returned to the caller and MUST be treated as
// fail-loud/degraded — a failed lookup must not silently look healthy.
func (h *RegistryHandler) platformAgentManagementMCPLoaded(ctx context.Context, workspaceID string, loaded []string) (bool, error) {
	declared, err := listDeclaredPlugins(ctx, workspaceID)
	if err != nil {
		return false, fmt.Errorf("declared-plugin lookup: %w", err)
	}

	hasDeclaredManagement := false
	for _, d := range declared {
		if d.PluginName == conciergePlatformMCPName {
			hasDeclaredManagement = true
			break
		}
	}
	if !hasDeclaredManagement {
		return false, nil
	}

	for _, t := range loaded {
		if t == conciergePlatformMCPCreateWorkspaceTool {
			return false, nil
		}
	}
	return true, nil
}

// markWorkspaceFailed updates a workspace row to status='failed' and broadcasts
// WORKSPACE_PROVISION_FAILED. It is a RegistryHandler-local fallback for the
// fail-closed platform-agent identity gate; the WorkspaceHandler's
// markProvisionFailed is the primary path during provisioning.
func (h *RegistryHandler) markWorkspaceFailed(ctx context.Context, workspaceID, msg, reason string) {
	extra := map[string]interface{}{
		"error":  msg,
		"code":   "PLATFORM_AGENT_IDENTITY_GATE",
		"reason": reason,
	}
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisionFailed), workspaceID, extra)
	if _, dbErr := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $3, last_sample_error = $2, updated_at = now() WHERE id = $1`,
		workspaceID, msg, models.StatusFailed); dbErr != nil {
		log.Printf("markWorkspaceFailed: db update failed for %s: %v", workspaceID, dbErr)
	}
}

// Register handles POST /registry/register
// Upserts workspace, sets Redis TTL, broadcasts WORKSPACE_ONLINE.
func (h *RegistryHandler) Register(c *gin.Context) {
	var payload models.RegisterPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		// pre-ctx: the workspace's existing row state isn't fetched yet
		// (we don't have ctx + we don't know the workspace ID until
		// parse succeeds). Log with an empty diagnostics struct —
		// the row state defaults to "(new)" in the log line, which is
		// the right framing for a fresh-register parse failure.
		logRegister400Reason("invalid_json", "", models.RegisterPayload{}, registerDiagnostics{}, err.Error())
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// #2500 instrumentation: log non-200 boot Register outcomes so operators
	// can distinguish 401 (C18 token race), 400 (push-URL invalid/empty),
	// 403 (platform kind guard), 5xx (DB/internal error), or success from
	// client timeout / unreachable $PLATFORM_URL.
	registerStart := time.Now()
	authOK := false
	defer func(wsID string) {
		if status := c.Writer.Status(); status != http.StatusOK {
			log.Printf("Registry register: workspace=%s boot_register_failed status=%d duration=%s", wsID, status, time.Since(registerStart))
			// #2530: record register failure so heartbeat can surface degraded status.
			// #2585 hardening: only stamp after the caller has authenticated
			// (requireWorkspaceToken succeeded). Unauthenticated 401s must NOT
			// mutate workspace state — otherwise anyone can POST /registry/register
			// without a bearer and force a false-degraded status via the heartbeat
			// 5-minute failure window.
			if authOK {
				if _, err := db.DB.ExecContext(context.Background(), `UPDATE workspaces SET last_register_failure_at = now() WHERE id = $1`, wsID); err != nil {
					log.Printf("Registry register: failed to record failure timestamp for %s: %v", wsID, err)
				}
			}
		}
	}(payload.ID)

	// Validate explicit delivery_mode if the agent declared one; empty is
	// allowed and resolves to the row's existing value (or "push" default)
	// in the upsert below. See #2339 for the poll/push split rationale.
	if payload.DeliveryMode != "" && !models.IsValidDeliveryMode(payload.DeliveryMode) {
		// pre-existingState (see L398): pass an empty struct; the row
		// state defaults to "(new)" in the log line.
		logRegister400Reason("invalid_delivery_mode", payload.ID, payload, registerDiagnostics{}, "payload.delivery_mode="+payload.DeliveryMode)
		c.JSON(http.StatusBadRequest, gin.H{"error": "delivery_mode must be 'push' or 'poll'"})
		return
	}

	// Validate explicit kind if the agent declared one; empty is allowed and
	// resolves to the row's existing value (or "workspace" default) in
	// resolveKind below. Only the platform-agent container declares 'platform'.
	if payload.Kind != "" && !models.IsValidKind(payload.Kind) {
		logRegister400Reason("invalid_kind", payload.ID, payload, registerDiagnostics{}, "payload.kind="+payload.Kind)
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be 'workspace' or 'platform'"})
		return
	}

	ctx := c.Request.Context()

	// #2680 residual: register-400 diagnostics on the recreate path.
	// When a recreated container's first /registry/register call returns
	// 400, we need to know WHICH validation step fired (URL missing? URL
	// in a private range? delivery_mode? kind? invalid JSON?) AND what
	// the workspace's existing row state was at the time. Without this,
	// the next restart run produces a 400 with no actionable signal —
	// the deferred boot_register_failed log at the end of the function
	// only fires AFTER the validation has already returned, so by the
	// time it logs we have the status code but not the reason.
	//
	// Helper: logRegister400Reason captures the failure reason + the
	// workspace's existing row state in a single grep-able line. Called
	// by every 400 path below. Idempotent: writes to log.Printf only,
	// does not mutate state.
	existingState := h.fetchExistingWorkspaceStateForDiagnostics(ctx, payload.ID)

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
	authOK = true

	// SECURITY (privilege-escalation fix): the public register path must never
	// CREATE or PROMOTE a row to kind='platform'. The org root is minted only by
	// the AdminAuth/boot-gated install paths (InstallPlatformAgent /
	// EnsureSelfHostedPlatformAgent). Without this, an ordinary in-VPC workspace
	// could register a fresh UUID as {"kind":"platform"} (a bootstrap-allowed call,
	// parent_id defaults NULL so the per-row CHECK is satisfied) and then be
	// provisioned with the tenant org-admin token (MOLECULE_API_KEY=ADMIN_TOKEN).
	// A platform agent re-registering its already-platform row (or omitting kind)
	// is unaffected. uniq_workspaces_one_platform_root is the structural backstop;
	// this is the friendly app-layer guard. Placed after the token check so it
	// doesn't side-channel row existence (mirrors resolveDeliveryMode below).
	if payload.Kind == models.KindPlatform {
		var existingKind string
		kErr := db.DB.QueryRowContext(ctx,
			`SELECT kind FROM workspaces WHERE id = $1`, payload.ID).Scan(&existingKind)
		switch {
		case errors.Is(kErr, sql.ErrNoRows), kErr == nil && existingKind != models.KindPlatform:
			c.JSON(http.StatusForbidden, gin.H{"error": "kind='platform' may only be assigned by the platform-agent install path"})
			return
		case kErr != nil && !errors.Is(kErr, sql.ErrNoRows):
			log.Printf("Registry register: kind precheck failed for %s: %v", payload.ID, kErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "registration failed"})
			return
		}
	}

	// Resolve the EFFECTIVE delivery mode for THIS register call: the
	// payload's explicit value wins; falling back to the existing row's
	// stored value; falling back to push (the schema default). Done AFTER
	// the C18 token check so a hijack attempt fails on auth before we
	// reveal whether a workspace row exists at all (resolveDeliveryMode
	// would otherwise side-channel that via timing). #2339.
	effectiveMode, err := h.resolveDeliveryMode(ctx, payload.ID, payload.DeliveryMode)
	if err != nil {
		log.Printf("Registry register: resolveDeliveryMode failed for %s: %v", payload.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "registration failed"})
		return
	}

	// Issue #2970: fail CLOSED if a platform agent reaches registration without
	// BOTH the seeded MODEL workspace_secret AND the platform-agent image's baked
	// /opt/molecule-mcp-server binary. The MISSING_MODEL gate in
	// prepareProvisionContext is the primary defense, but if a model-less/identity-
	// less/mcp-less concierge somehow boots on a path that bypasses that gate (e.g.
	// an old or generic image), this second-layer guard prevents it from ever marking
	// itself online-routable. Instead we mark the workspace failed so the canvas
	// surfaces a provision failure rather than serving users a generic Claude Code.
	//
	// The runtime declares mcp-server availability via payload.mcp_server_present.
	// A nil/false value is fail-closed: an undeclared or missing MCP server cannot be
	// trusted for a concierge.
	//
	// existingState.ExistingKind is populated by fetchExistingWorkspaceStateForDiagnostics
	// (best-effort). We treat "platform" literally; any other value (including "(new)"
	// or "(unavailable)") means the gate does not apply unless payload.Kind itself is
	// "platform" (covered by the privilege-escalation precheck above).
	if payload.Kind == models.KindPlatform || existingState.ExistingKind == models.KindPlatform {
		hasModel, mErr := h.platformAgentHasModelSecret(ctx, payload.ID)
		if mErr != nil {
			log.Printf("Registry register: model secret lookup failed for %s: %v", payload.ID, mErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "registration failed"})
			return
		}
		hasMCP := h.platformAgentMCPServerPresent(payload.MCPServerPresent)
		if !hasModel || !hasMCP {
			var msg, reason, logCode string
			switch {
			case !hasModel:
				msg = "platform agent registered without a seeded MODEL secret; refusing online"
				reason = "model_missing"
				logCode = "platform_agent_model_missing"
			case !hasMCP:
				msg = "platform agent registered without /opt/molecule-mcp-server; refusing online"
				reason = "mcp_server_missing"
				logCode = "platform_agent_mcp_server_missing"
			}
			log.Printf("Registry register: %s (workspace=%s)", msg, payload.ID)
			h.markWorkspaceFailed(ctx, payload.ID, msg, reason)
			logRegister400Reason(logCode, payload.ID, payload, existingState, msg)
			c.JSON(http.StatusBadRequest, gin.H{"error": "platform agent identity incomplete"})
			return
		}
	}

	// URL handling diverges by mode:
	//   push: URL is required and must pass the SSRF safety check —
	//     same as pre-#2339 behavior (the workspace must be reachable for
	//     the proxy to dispatch).
	//   poll: URL is optional and ignored when present. We don't even
	//     validate it because the platform never dispatches to it. Skipping
	//     validateAgentURL is intentional — a poll-mode workspace doesn't
	//     need a publicly-routable URL, so a localhost / private IP /
	//     missing URL is correct, not a mis-configuration.
	// effectiveURL is where the workspace is reachable. Normally it's the
	// runtime-supplied payload.URL. CROSS-CLOUD FALLBACK: an egress-only box
	// (GCP/Hetzner, fronted by a Cloudflare tunnel) advertises its tunnel URL in
	// the agent_card but registers with an EMPTY top-level url — its NIC-derived
	// register URL is a private/unroutable address it correctly omits. Without a
	// URL the push-mode guard rejects the registration, so the box never becomes
	// deliverable (url/delivery_mode stay unset) and the scheduler fails every tick
	// with "workspace has no URL". Recover the reachable URL from the agent_card —
	// it is the tunnel hostname the platform itself provisioned for this box. AWS
	// intra-VPC boxes send a non-empty url and are unaffected; poll-mode is untouched
	// (it needs no url). Found via the agents-team SEO agent stuck 'failed' after the
	// GCP-default flip (2026-06-11).
	effectiveURL := payload.URL
	if effectiveMode == models.DeliveryModePush {
		// SSRF hardening (Researcher #2132 / RC 103771, step C):
		// isSafeURL the embedded agent_card.url at WRITE time. payload.URL
		// alone is insufficient — the agent_card URL is the surface
		// that gets broadcast via the registry + read by every other
		// component (transcript proxy, A2A dispatch, etc.). Without
		// this check, a Register with a safe payload.URL but a
		// poisoned agent_card.url stores a SSRF landmine that the
		// transcript proxy's front-door isSafeURL would catch per
		// request, but the BROADCAST surface is still the bad URL
		// (peer A2A dispatch could follow it). The UpdateCard path
		// already does this check at line 1226; Register is the
		// symmetric WRITE surface.
		if cardURL := agentCardURL(payload.AgentCard); cardURL != "" {
			if err := isSafeURL(cardURL); err != nil {
				logRegister400Reason("agent_card_url_rejected", payload.ID, payload, existingState, err.Error())
				c.JSON(http.StatusBadRequest, gin.H{"error": "workspace agent_card URL not allowed"})
				return
			}
		}
		if effectiveURL == "" {
			if cardURL := agentCardURL(payload.AgentCard); cardURL != "" {
				effectiveURL = cardURL
			}
		}
		if effectiveURL == "" {
			// Detail: which surface had a URL (so the operator can
			// tell "no URL anywhere" from "URL in agent_card but
			// not in payload"). NEVER log the raw URL (see RC
			// #11335).
			logRegister400Reason("url_required_for_push", payload.ID, payload, existingState, "effective_url_empty (payload_url_present="+urlPresence(payload.URL)+", agent_card_url_present="+urlPresence(agentCardURL(payload.AgentCard))+")")
			c.JSON(http.StatusBadRequest, gin.H{"error": "url is required for push-mode workspaces"})
			return
		}
		if err := validateAgentURL(effectiveURL); err != nil {
			// validateAgentURL returns a friendly CIDR label
			// (e.g. "url targets a blocked address: RFC-1918
			// private address") that does NOT contain the actual
			// address. Safe to log.
			logRegister400Reason("url_validate_failed", payload.ID, payload, existingState, err.Error())
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	// Reconcile the runtime-supplied card's identity fields against the
	// trusted workspaces row before storing. The runtime builds its card
	// from config.name, which the CP-regenerated /configs/config.yaml
	// sets to the workspace UUID — so without this the stored card
	// served at /.well-known/agent-card.json and returned to peers via
	// agent_card_url has name = UUID, description = "", role = null even
	// though the operator-controlled workspaces.name holds the friendly
	// name the canvas shows. We only FILL gaps from the DB (never
	// downgrade a card that already carries a real name); identity stays
	// platform-controlled — the agent cannot self-set these. Best-effort:
	// a lookup failure leaves the card exactly as the runtime sent it
	// (no-worse-than-before). See agent_card_reconcile.go.
	reconciledCard := payload.AgentCard
	{
		var dbName, dbRole sql.NullString
		if qErr := db.DB.QueryRowContext(ctx,
			`SELECT name, role FROM workspaces WHERE id = $1`, payload.ID,
		).Scan(&dbName, &dbRole); qErr == nil {
			name := ""
			if dbName.Valid {
				name = dbName.String
			}
			role := ""
			if dbRole.Valid {
				role = dbRole.String
			}
			if rc, did := reconcileAgentCardIdentity(
				payload.AgentCard, payload.ID, name, role,
			); did {
				reconciledCard = rc
				log.Printf("Registry register: reconciled agent_card identity for %s from workspaces row", payload.ID)
			}
		}
	}
	agentCardStr := string(reconciledCard)

	// urlForUpsert: poll-mode workspaces don't need a URL. Empty input
	// becomes NULL via sql.NullString so the row's URL stays clean (the
	// CASE below also preserves an existing provisioner-set URL, which
	// matters for hybrid setups where a workspace was previously push
	// and is being re-registered as poll).
	var urlForUpsert sql.NullString
	if effectiveURL != "" {
		urlForUpsert = sql.NullString{String: effectiveURL, Valid: true}
	}

	// modeForUpsert: empty payload value means "keep what's already on the
	// row, or default to push for new rows". The COALESCE in the CASE on
	// the UPDATE branch and the EXCLUDED.delivery_mode on the INSERT branch
	// implement that. We pass effectiveMode (already resolved above) so
	// the row's mode is consistent with the URL-validation decision we
	// just made.
	modeForUpsert := effectiveMode

	// RFC#2843 #32: capture the pre-upsert status so we can fire the
	// declared-plugin reconcile when THIS register performs the
	// provisioning→online transition. On the CP/SaaS boot path the runtime
	// calls POST /registry/register FIRST (before any heartbeat) and the
	// upsert below sets status='online' unconditionally — so the heartbeat
	// handler's prevStatus=='provisioning' trigger never matches (the row is
	// already 'online' by the first heartbeat). Register is therefore the real
	// fresh-boot provisioning→online transition for CP workspaces, and the
	// reconcile must fire here too. Best-effort: a failed read leaves
	// prevStatusForReconcile = "" and simply skips the fire (the heartbeat path
	// remains the fallback for any runtime that registers without flipping
	// status). status is a NOT-NULL enum — select it bare (never COALESCE to '',
	// which Postgres rejects as an invalid enum literal and fails the scan).
	//
	// Guarded on h.reconcilePlugins != nil so the extra read only runs when the
	// reconcile hook is actually wired (production router). Unit tests that don't
	// wire a ReconcileFunc skip this query entirely (no mock churn).
	var prevStatusForReconcile string
	if h.reconcilePlugins != nil {
		if err := db.DB.QueryRowContext(ctx, `SELECT status FROM workspaces WHERE id = $1`, payload.ID).Scan(&prevStatusForReconcile); err != nil {
			// sql.ErrNoRows on a brand-new workspace is expected (INSERT path);
			// leave prevStatusForReconcile empty so we don't fire on first create
			// (provisioning is recorded by POST /workspaces; the row exists by the
			// time the runtime registers, so the provisioning→online case is the
			// UPDATE branch below).
			prevStatusForReconcile = ""
		}
	}

	// Upsert workspace: update url, agent_card, status, delivery_mode if already exists.
	// On INSERT (workspace not yet created via POST /workspaces), use ID as name placeholder.
	// Keep existing URL if provisioner already set a host-accessible one (starts with http://127.0.0.1).
	//
	// #73 guard: `WHERE workspaces.status IS DISTINCT FROM 'removed'` prevents
	// a late heartbeat from a workspace that was just deleted from resurrecting
	// the row. Without this guard, bulk deletes left tier-3 stragglers because
	// the last pre-teardown heartbeat flipped status back to 'online' after
	// Delete's UPDATE.
	// kind ($6) is the raw payload value (validated above; "" = unspecified).
	// COALESCE(NULLIF($6,''), …) means: an explicit kind wins; an unspecified
	// kind defaults to 'workspace' for a NEW row and KEEPS the existing kind on
	// re-register (so a platform agent re-registering without kind is never
	// downgraded). A non-root row asking for 'platform' is rejected by the
	// workspaces_platform_root_check constraint → friendly 409 below.
	_, err = db.DB.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, url, agent_card, status, last_heartbeat_at, delivery_mode, kind)
		VALUES ($1, $2, $3, $4::jsonb, 'online', now(), $5, COALESCE(NULLIF($6, ''), 'workspace'))
		ON CONFLICT (id) DO UPDATE SET
			-- Preserve the provisioner-set host-port URL. The provisioner
			-- injects MOLECULE_WORKSPACE_URL=<host-port> into the container
			-- env (buildStartWorkspaceEnv in workspace-server), so the
			-- runtime should register that same URL. The runtime's
			-- resolve_workspace_url honors MOLECULE_WORKSPACE_URL at highest
			-- precedence, so when the env propagation is correct, the
			-- runtime's URL == provisioner's URL. When env propagation is
			-- broken (real-image lifecycle E2E gap that bit 3 rounds
			-- running), the runtime falls back to http://HOSTNAME:8000
			-- — the port 8000 makes it distinguishable from the
			-- provisioner's host-port (typically >30000). Preserve the
			-- provisioner's URL when its port != 8000.
			--
			-- Researcher #11798: round-3 fix changed the provisioner
			-- from http://127.0.0.1 to http://localhost (the
			-- workspaceAdvertiseURL default), but the Register handler
			-- only matched the legacy 127.0.0.1 prefix, so the upsert
			-- overwrote the provisioner's URL with the runtime's 8000
			-- fallback. Generalize to match any host-prefixed
			-- host-port URL whose port != 8000.
			url = CASE
				WHEN workspaces.url IS NOT NULL
				     AND workspaces.url != ''
				     AND (workspaces.url LIKE 'http://127.0.0.1:%'
				          OR workspaces.url LIKE 'http://localhost:%')
				     AND CAST(substring(workspaces.url FROM ':([0-9]+)$') AS int) <> 8000
				THEN workspaces.url
				ELSE EXCLUDED.url
			END,
			agent_card = EXCLUDED.agent_card,
			status = 'online',
			last_heartbeat_at = now(),
			delivery_mode = EXCLUDED.delivery_mode,
			kind = COALESCE(NULLIF($6, ''), workspaces.kind),
			updated_at = now()
		WHERE workspaces.status IS DISTINCT FROM 'removed'
	`, payload.ID, payload.ID, urlForUpsert, agentCardStr, modeForUpsert, payload.Kind)
	if err != nil {
		if isPlatformRootViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": errPlatformNotRoot})
			return
		}
		log.Printf("Registry register error: %v (id=%s)", err, payload.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "registration failed"})
		return
	}

	// Set Redis liveness key
	if err := db.SetOnline(ctx, payload.ID); err != nil {
		log.Printf("Registry redis error: %v", err)
	}

	// RFC#2843 #32: fire the declared-plugin reconcile when THIS register just
	// transitioned the workspace provisioning→online. This is the primary
	// fresh-boot transition on the CP/SaaS path (runtime registers before it
	// heartbeats, and the upsert above set status='online'), so without firing
	// here the seo-agent's declared seo-all plugin never installs. Idempotent
	// (ReconcileWorkspacePlugins diffs declared-vs-installed and no-ops when
	// present) and nil-safe via fireReconcileOnline; fire-and-forget so register
	// returns immediately. The heartbeat handler keeps its own
	// prevStatus=='provisioning' fire as a fallback for runtimes that reach
	// online via heartbeat self-heal rather than register.
	if prevStatusForReconcile == string(models.StatusProvisioning) {
		h.fireReconcileOnline(ctx, payload.ID)
	}

	// Cache URL — prefer existing provisioner URL over agent-reported one.
	// The DB CASE already preserves provisioner URLs, so read from DB as source of truth
	// instead of adding a Redis round-trip on every registration.
	//
	// Poll-mode workspaces typically have no URL at all; skip the cache
	// writes entirely in that case so we don't poison the cache with an
	// empty string that another caller might mistake for "registered with
	// no URL" vs "not yet registered". The proxy short-circuits poll-mode
	// before consulting the URL cache anyway (see #2339 PR 2).
	cachedURL := effectiveURL
	var dbURL string
	if err := db.DB.QueryRowContext(ctx, `SELECT url FROM workspaces WHERE id = $1`, payload.ID).Scan(&dbURL); err == nil {
		if isProvisionerHostPortURL(dbURL) {
			cachedURL = dbURL
		}
	}
	if cachedURL != "" {
		if err := db.CacheURL(ctx, payload.ID, cachedURL); err != nil {
			log.Printf("Registry cache url error: %v", err)
		}
	}

	// Cache agent-reported URL separately for workspace-to-workspace discovery
	// (Docker containers can reach each other by hostname but not via host ports).
	// Same skip-when-empty rule as above.
	if payload.URL != "" {
		if err := db.CacheInternalURL(ctx, payload.ID, payload.URL); err != nil {
			log.Printf("Registry cache internal url error: %v", err)
		}
	}

	// Broadcast WORKSPACE_ONLINE — use the reconciled card so the canvas
	// Agent Card view live-updates with the friendly name, matching what
	// was just persisted (not the runtime's raw UUID-name card).
	if err := h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOnline), payload.ID, map[string]interface{}{
		"url":           cachedURL,
		"agent_card":    reconciledCard,
		"delivery_mode": effectiveMode,
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
	response := gin.H{"status": "registered", "delivery_mode": effectiveMode}
	if hasLive, hasLiveErr := wsauth.HasLiveInstanceToken(ctx, db.DB, payload.ID); hasLiveErr == nil && !hasLive {
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

	// RFC #2312 PR-F: return the workspace's platform_inbound_secret so SaaS
	// workspaces (which have no persistent /configs volume across container
	// restarts) can re-populate /configs/.platform_inbound_secret on every
	// register call. Docker-mode workspaces also receive it — the workspace-
	// side write is idempotent (same value every call until a future
	// rotation flow lands), so the duplication is harmless.
	//
	// NOT gated by hasLive: the inbound secret is minted at workspace
	// creation in workspace_provision.go (PR-A), independent of the
	// outbound auth_token's "issue once" lifecycle. Returning it here is
	// the only delivery path for SaaS, where the platform's CP provisioner
	// has no volume to write into.
	//
	// Lazy-heal (2026-04-30): if the column is NULL (legacy workspace
	// provisioned before the shared-mint refactor), mint inline and
	// include in the response. Without this, legacy workspaces would
	// need two round-trips before chat upload works — chat_files
	// lazy-heals platform-side on first attempt, then the workspace
	// must heartbeat to receive the freshly-minted secret.
	// Heal-on-register collapses that to one round-trip.
	if secret, _, healErr := readOrLazyHealInboundSecret(ctx, payload.ID, "Registry"); healErr == nil {
		response["platform_inbound_secret"] = secret
	}
	// Errors are non-fatal here — the workspace is online and can serve
	// non-/internal traffic. The lazy-heal helper has already logged
	// whichever sub-step failed (read or mint). If the secret never lands,
	// chat upload surfaces the issue loudly with the RFC-#2312 hint.

	// #2530: clear register failure on success — the workspace is healthy.
	if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET last_register_failure_at = NULL WHERE id = $1`, payload.ID); err != nil {
		log.Printf("Registry register: failed to clear failure timestamp for %s: %v", payload.ID, err)
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

	// Read previous current_task + status to detect changes (before the UPDATE).
	//
	// prevStatus is load-bearing for the RFC#2843 #32 plugin reconcile: the
	// main heartbeat UPDATE below SELF-HEALS status provisioning→online INLINE
	// (the `CASE WHEN status = 'provisioning' THEN 'online'` clause). That flip
	// happens BEFORE evaluateStatus runs, so by the time evaluateStatus reads
	// currentStatus it is already 'online' and its `currentStatus ==
	// "provisioning"` branch (which fires fireReconcileOnline) never matches on
	// the normal fresh-boot path — the runtime only ever calls
	// /registry/heartbeat, never /registry/register, so this IS the path every
	// new workspace takes. The result: declared plugins never installed on a
	// fresh seo-agent (the #32 regression). Capturing prevStatus here lets us
	// fire the reconcile when THIS heartbeat performed the provisioning→online
	// flip, independent of evaluateStatus. evaluateStatus still owns the OTHER
	// recovery transitions (offline/degraded/awaiting_agent/failed→online),
	// which the inline CASE does not touch.
	//
	// IMPORTANT (enum scan): `status` is a NOT-NULL `workspace_status` ENUM.
	// Do NOT wrap it in COALESCE(status, '') — the '' literal is coerced to
	// the enum type and Postgres rejects it with `invalid input value for
	// enum workspace_status: ""`, failing the WHOLE row scan. That left
	// prevStatus = "" on every heartbeat, so the prevStatus=='provisioning'
	// reconcile trigger NEVER fired (the #32 regression returned). Select the
	// column bare; it is never NULL.
	var prevTask string
	var prevSpend int64
	var prevStatus string
	if err := db.DB.QueryRowContext(ctx, `SELECT COALESCE(current_task, ''), COALESCE(monthly_spend, 0), status FROM workspaces WHERE id = $1`, payload.WorkspaceID).Scan(&prevTask, &prevSpend, &prevStatus); err != nil {
		log.Printf("registry heartbeat: prev_task query failed for workspace %s: %v", payload.WorkspaceID, err)
	}

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

	// Multi-period budget (#49): record the spend INCREMENT into the
	// workspace_spend_events ledger so the server can compute rolling per-period
	// windows (hourly/daily/weekly/monthly) — see budget_periods.go. The agent
	// still reports a cumulative monthly figure; we derive the delta vs the
	// last-seen cumulative (prevSpend). A DECREASE means the agent reset its
	// monthly cumulative (new month) → treat the new value as fresh spend.
	// Best-effort: a ledger failure must never break the heartbeat.
	if payload.MonthlySpend > 0 {
		delta := payload.MonthlySpend - prevSpend
		if delta < 0 {
			delta = payload.MonthlySpend
		}
		if delta > 0 {
			if err := recordSpendDelta(ctx, db.DB, payload.WorkspaceID, delta); err != nil {
				log.Printf("registry heartbeat: spend-ledger insert failed for workspace %s: %v", payload.WorkspaceID, err)
			}
		}
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
				status            = CASE WHEN status = 'provisioning' THEN 'online' ELSE status END,
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
				status            = CASE WHEN status = 'provisioning' THEN 'online' ELSE status END,
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

	// RFC#2843 #32: fire the declared-plugin reconcile when THIS heartbeat just
	// performed the provisioning→online self-heal (the inline CASE in the UPDATE
	// above). This is the primary fresh-boot transition: a newly-provisioned
	// workspace is created with status='provisioning', the runtime's first
	// heartbeat flips it to 'online' via that CASE, and there is no
	// /registry/register on the boot path. Without firing here, the reconcile
	// hook in evaluateStatus never sees a provisioning→online transition (the
	// CASE already moved the row to 'online' before evaluateStatus reads
	// currentStatus), so declared plugins (e.g. seo-all) never install. Firing
	// is idempotent — ReconcileWorkspacePlugins diffs declared-vs-installed and
	// no-ops when everything is present — and nil-safe via fireReconcileOnline.
	if prevStatus == string(models.StatusProvisioning) {
		h.fireReconcileOnline(ctx, payload.WorkspaceID)
	}

	// #2421: backfill agent_card when the initial register failed and the
	// heartbeat carries it. Only writes when NULL — never overwrites a
	// reconciled or updated card. This is the recovery path for fast-cloud
	// workspaces whose DNS wasn't ready at first register.
	if len(payload.AgentCard) > 0 {
		res, err := db.DB.ExecContext(ctx, `
			UPDATE workspaces
			SET agent_card = $2
			WHERE id = $1 AND agent_card IS NULL
		`, payload.WorkspaceID, payload.AgentCard)
		if err != nil {
			log.Printf("Registry heartbeat: agent_card backfill failed for %s: %v", payload.WorkspaceID, err)
		} else {
			if rows, _ := res.RowsAffected(); rows > 0 {
				log.Printf("Registry heartbeat: backfilled agent_card for %s (initial register had failed)", payload.WorkspaceID)
			}
		}
	}

	// #2659/#2665/#2739: clear last_register_failure_at whenever a heartbeat
	// carries a valid agent_card — NOT only on the agent_card-was-NULL backfill
	// path above. A heartbeat with a card proves the runtime is alive and
	// re-advertising the SAME reachable card the platform already trusts, so any
	// recent register-400 was transient and must not keep the workspace pinned
	// in 'degraded' until the 5-minute failure window ages out.
	//
	// #2739: on RESTART (vs first provision) the agent_card row is ALREADY
	// populated, so the NULL-scoped backfill never fires and never cleared the
	// marker. The restarted container's authenticated /registry/register can
	// 400 with url_validate_failed (its Docker-internal hostname e.g.
	// "212851b5693d" is not resolvable from the platform), stamping
	// last_register_failure_at; with the card already present the marker stuck
	// and evaluateStatus held the workspace degraded past the Local Provision
	// Lifecycle restart-survival window (run 358593). Credentials are correctly
	// projected post-restart (core#2709/#2712); this is purely the degraded->online
	// recovery gap. Clearing on a card-bearing heartbeat is the same trust signal
	// the success-on-register clear (register handler) relies on.
	if len(payload.AgentCard) > 0 {
		res, err := db.DB.ExecContext(ctx, `
			UPDATE workspaces
			SET last_register_failure_at = NULL
			WHERE id = $1 AND last_register_failure_at IS NOT NULL
		`, payload.WorkspaceID)
		if err != nil {
			log.Printf("Registry heartbeat: clear register-failure marker failed for %s: %v", payload.WorkspaceID, err)
		} else if rows, _ := res.RowsAffected(); rows > 0 {
			log.Printf("Registry heartbeat: cleared register-failure marker for %s (live card-bearing heartbeat after a transient register-400)", payload.WorkspaceID)
		}
	}

	// Refresh Redis TTL
	if err := db.RefreshTTL(ctx, payload.WorkspaceID); err != nil {
		log.Printf("Heartbeat redis error: %v", err)
	}

	// Evaluate status transitions
	h.evaluateStatus(c, payload)

	// Broadcast current task update only when it changed (avoid spamming on every heartbeat)
	if payload.CurrentTask != prevTask {
		h.broadcaster.BroadcastOnly(payload.WorkspaceID, string(events.EventTaskUpdated), map[string]interface{}{
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
	h.broadcaster.BroadcastOnly(payload.WorkspaceID, string(events.EventWorkspaceHeartbeat), map[string]interface{}{
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

	resp := gin.H{"status": "ok"}

	// Deliver the platform_inbound_secret on every heartbeat. Mirrors
	// the same field on /registry/register, but heartbeats are the
	// only periodic platform↔workspace channel — register fires once
	// at workspace startup, so without this delivery path a lazy-heal
	// (chat_files.go's "secret was just minted, retry in 30s" branch)
	// could ONLY recover via a workspace restart.
	//
	// Symptom this fixes: 2026-04-30 user report on hongmingwang —
	// chat upload returned 503 "workspace will pick it up on its
	// next heartbeat", then 401 on retry. The 503 message was
	// misleading because heartbeat used to discard the
	// platform_inbound_secret entirely; only register delivered it.
	//
	// Lazy-heal here instead of a column read because:
	//   - register-time heal already covers cold-start workspaces
	//   - heartbeat-time heal covers the rotate / mid-life recover case
	//   - the helper short-circuits to the existing column read when
	//     the secret is already present (cheap, idempotent)
	//
	// Errors are non-fatal: heartbeat's primary job is liveness, and
	// the chat-upload path will lazy-heal again if needed. Logging
	// happens inside the helper.
	if secret, _, healErr := readOrLazyHealInboundSecret(ctx, payload.WorkspaceID, "Heartbeat"); healErr == nil && secret != "" {
		resp["platform_inbound_secret"] = secret
	}

	c.JSON(http.StatusOK, resp)
}

func (h *RegistryHandler) evaluateStatus(c *gin.Context, payload models.HeartbeatPayload) {
	ctx := c.Request.Context()

	var currentStatus string
	var currentKind string
	var lastRegisterFailure sql.NullTime
	var mcpUnloadedSince sql.NullTime
	err := db.DB.QueryRowContext(ctx, `SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id = $1`, payload.WorkspaceID).
		Scan(&currentStatus, &currentKind, &lastRegisterFailure, &mcpUnloadedSince)
	if err != nil {
		return
	}
	hasRecentRegisterFailure := lastRegisterFailure.Valid && time.Since(lastRegisterFailure.Time) < 5*time.Minute

	// managementMCPUnloaded tracks whether THIS heartbeat observed the declared
	// management MCP as absent/incomplete. It gates the degraded->online
	// recovery branch below so a platform agent in sustained #3082 violation is
	// not flapped back to online (Bug B: the generic recovery branch keys only
	// on error_rate/runtime_state and would otherwise resurrect a genuinely
	// MCP-less concierge). Set inside the #3082 block.
	managementMCPUnloaded := false

	// FAIL-CLOSED concierge online-marking gate (RCA #2970).
	// A kind='platform' workspace that has lost either its seeded MODEL secret or
	// the image-baked /opt/molecule-mcp-server binary must never be allowed back
	// to status='online' via heartbeat recovery. The Register handler already gates
	// the initial online marking; this gate closes the heartbeat-driven recovery
	// paths (provisioning/failed/offline/awaiting_agent/degraded → online) that
	// would otherwise resurrect a model-less/mcp-less concierge and let it serve
	// users generic Claude Code.
	//
	// The runtime now declares mcp-server availability via
	// payload.mcp_server_present on every heartbeat/register call. nil/false is
	// fail-closed: an old/generic runtime cannot prove it is a real concierge.
	if currentKind == models.KindPlatform {
		hasModel, mErr := h.platformAgentHasModelSecret(ctx, payload.WorkspaceID)
		if mErr != nil {
			log.Printf("Heartbeat: model secret lookup failed for platform agent %s: %v", payload.WorkspaceID, mErr)
			return
		}
		hasMCP := h.platformAgentMCPServerPresent(payload.MCPServerPresent)
		if !hasModel || !hasMCP {
			var msg, reason string
			switch {
			case !hasModel:
				msg = "platform agent heartbeat denied: no seeded MODEL workspace_secret; refusing to mark online (RCA #2970 FAIL-CLOSED)"
				reason = "model_missing"
			case !hasMCP:
				msg = "platform agent heartbeat denied: /opt/molecule-mcp-server missing; refusing to mark online (RCA #2970 FAIL-CLOSED)"
				reason = "mcp_server_missing"
			}
			log.Printf("Heartbeat: %s (workspace=%s)", msg, payload.WorkspaceID)
			h.markWorkspaceFailed(ctx, payload.WorkspaceID, msg, reason)
			return
		}

		// core#3082: post-online fail-loud for a missing declared management MCP.
		//
		// Triggered when the runtime AFFIRMATIVELY reports mcp_server_present=true
		// (the #147 contract). For pre-#147 runtimes where the field is nil,
		// platformAgentMCPServerPresent above already returned true under
		// backward-compat — we DO NOT run the #3082 check in that case so
		// legacy runtimes don't flip to degraded before the runtime-side
		// loaded_mcp_tools producer lands.
		//
		// The gate has two fail-loud conditions:
		//   - loaded_mcp_tools present but missing the required tool
		//     (mcp__molecule-platform__create_workspace).
		//   - loaded_mcp_tools ABSENT (runtime says server is up but won't
		//     report the tools list).
		//
		// GRACE WINDOW (flap fix): neither condition degrades on a SINGLE
		// heartbeat. The management MCP connects asynchronously after process
		// start and the runtime can only observe its loaded tool list from a
		// live turn — so an absent/partial list on an early heartbeat is a
		// warmup signal, not a fault. We record the first-seen-unloaded time in
		// workspaces.mcp_unloaded_since and only degrade once the absence has
		// persisted continuously past managementMCPUnloadedGrace. The moment a
		// heartbeat reports the required tool loaded we clear the stamp. This
		// eliminates the ~50/50 online<->degraded oscillation (the agent is
		// functional throughout) while PRESERVING the RCA#2970 fail-closed
		// intent: a genuinely-missing management MCP stays absent past the
		// window and degrades, and stays degraded (the recovery branch below is
		// gated on managementMCPUnloaded).
		if payload.MCPServerPresent != nil && *payload.MCPServerPresent {
			loaded := payload.LoadedMCPTools
			var (
				managementMissing bool
				mErr              error
				absentToolsList   bool
			)
			if loaded == nil {
				// Runtime speaks #147 (server_present=true) but omits the new
				// loaded_mcp_tools producer → we cannot verify the specific
				// required tool is loaded. Treated as unloaded (subject to the
				// grace window below).
				managementMissing = true
				absentToolsList = true
			} else {
				managementMissing, mErr = h.platformAgentManagementMCPLoaded(ctx, payload.WorkspaceID, loaded)
			}
			switch {
			case mErr != nil:
				// A lookup error is a system fault, not a warmup signal — it is
				// not subject to the grace window. Fail-loud immediately.
				msg := fmt.Sprintf("platform agent declared management MCP lookup failed: %v; marking degraded (core#3082)", mErr)
				log.Printf("Heartbeat: %s (workspace=%s)", msg, payload.WorkspaceID)
				managementMCPUnloaded = true
				if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, last_sample_error = $2, updated_at = now() WHERE id = $3 AND status = 'online'`, models.StatusDegraded, msg, payload.WorkspaceID); err != nil {
					log.Printf("Heartbeat: failed to mark %s degraded (management MCP lookup error): %v", payload.WorkspaceID, err)
				}
				h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceDegraded), payload.WorkspaceID, map[string]interface{}{
					"management_mcp_lookup_failed": true,
					"sample_error":                 msg,
				})
			case managementMissing:
				managementMCPUnloaded = true
				// Stamp the first-seen-unloaded time if not already set, then
				// decide whether the absence has outlasted the grace window.
				now := time.Now()
				firstUnloaded := mcpUnloadedSince
				if !firstUnloaded.Valid {
					firstUnloaded = sql.NullTime{Time: now, Valid: true}
					// Best-effort: persist the stamp so the window survives
					// across heartbeats and CP restarts. Don't gate the rest of
					// evaluateStatus on the write succeeding.
					if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET mcp_unloaded_since = COALESCE(mcp_unloaded_since, now()), updated_at = now() WHERE id = $1`, payload.WorkspaceID); err != nil {
						log.Printf("Heartbeat: failed to stamp mcp_unloaded_since for %s: %v", payload.WorkspaceID, err)
					}
				}
				withinGrace := now.Sub(firstUnloaded.Time) < managementMCPUnloadedGrace
				if withinGrace {
					// Warmup window — do NOT degrade. The agent stays online (or
					// continues whatever transition the rest of evaluateStatus
					// drives) while the management MCP finishes connecting.
					log.Printf("Heartbeat: platform agent %s management MCP not yet loaded (absent_tools_list=%v); within %s grace window (unloaded for %s) — not degrading (core#3082)", payload.WorkspaceID, absentToolsList, managementMCPUnloadedGrace, now.Sub(firstUnloaded.Time).Truncate(time.Second))
				} else {
					msg := "platform agent management MCP declared but not loaded; marking degraded (core#3082)"
					if absentToolsList {
						msg = "platform agent runtime did not report loaded_mcp_tools on a mcp_server_present=true heartbeat; cannot verify create_workspace tool is loaded — marking degraded (core#3082)"
					}
					log.Printf("Heartbeat: %s (workspace=%s, unloaded for %s)", msg, payload.WorkspaceID, now.Sub(firstUnloaded.Time).Truncate(time.Second))
					if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, last_sample_error = $2, updated_at = now() WHERE id = $3 AND status = 'online'`, models.StatusDegraded, msg, payload.WorkspaceID); err != nil {
						log.Printf("Heartbeat: failed to mark %s degraded (management MCP missing): %v", payload.WorkspaceID, err)
					}
					h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceDegraded), payload.WorkspaceID, map[string]interface{}{
						"management_mcp_missing":  true,
						"loaded_mcp_tools_absent": absentToolsList,
						"sample_error":            msg,
					})
				}
			default:
				// Management MCP confirmed loaded this heartbeat. Clear any
				// outstanding unloaded stamp so a future absence starts a fresh
				// grace window rather than degrading instantly.
				if mcpUnloadedSince.Valid {
					if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET mcp_unloaded_since = NULL, updated_at = now() WHERE id = $1 AND mcp_unloaded_since IS NOT NULL`, payload.WorkspaceID); err != nil {
						log.Printf("Heartbeat: failed to clear mcp_unloaded_since for %s: %v", payload.WorkspaceID, err)
					}
				}
			}
		}
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
			`UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2 AND status = 'online'`,
			models.StatusDegraded, payload.WorkspaceID)
		if err != nil {
			log.Printf("Heartbeat: failed to mark %s degraded (wedged): %v", payload.WorkspaceID, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceDegraded), payload.WorkspaceID, map[string]interface{}{
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
		// #73 guard: heartbeat degrade must not resurrect a removed workspace.
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2 AND status = 'online'`, models.StatusDegraded, payload.WorkspaceID); err != nil {
			log.Printf("Heartbeat: failed to mark %s degraded: %v", payload.WorkspaceID, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceDegraded), payload.WorkspaceID, map[string]interface{}{
			"error_rate":   payload.ErrorRate,
			"sample_error": payload.SampleError,
		})
	}

	// #2530: degrade when register has persistently failed within the last
	// 5 minutes. A workspace whose auth token was lost after container re-create
	// will 401 on every boot register; heartbeats keep it looking online while
	// canvas chat delivery silently starves. Surfacing degraded gives the user
	// a visible restart/credential-repair hint.
	if currentStatus == "online" && hasRecentRegisterFailure {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2 AND status = 'online'`, models.StatusDegraded, payload.WorkspaceID); err != nil {
			log.Printf("Heartbeat: failed to mark %s degraded (register failure): %v", payload.WorkspaceID, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceDegraded), payload.WorkspaceID, map[string]interface{}{
			"register_failure": true,
			"sample_error":     "Register failed — workspace auth token may be stale. Restart or reprovision to recover.",
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
	//
	// #2530: also require no recent register failure — the workspace stays
	// degraded until a successful register clears the failure timestamp.
	if !nativeStatus && currentStatus == "degraded" && payload.ErrorRate < 0.1 && payload.RuntimeState == "" && !hasRecentRegisterFailure && !managementMCPUnloaded {
		// #73 guard: heartbeat recovery must not resurrect a removed workspace.
		// core#3082 (Bug B): also require !managementMCPUnloaded so a platform
		// agent whose declared management MCP is still absent THIS heartbeat is
		// not flapped back to online — otherwise a genuinely MCP-less concierge
		// would oscillate degraded->online forever (the recovery condition is
		// satisfied by any functional agent with low error_rate). Recovery now
		// requires the management MCP to actually be observed loaded again,
		// which clears mcp_unloaded_since and leaves managementMCPUnloaded=false.
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2 AND status = 'degraded'`, models.StatusOnline, payload.WorkspaceID); err != nil {
			log.Printf("Heartbeat: failed to recover %s to online: %v", payload.WorkspaceID, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOnline), payload.WorkspaceID, map[string]interface{}{})
		// RFC#2843: reconcile declared plugins on transition-to-online.
		h.fireReconcileOnline(ctx, payload.WorkspaceID)
	}

	// Recovery: if workspace was offline but is now sending heartbeats, bring it back online.
	// #73 guard: `AND status = 'offline'` makes the flip conditional in a single statement,
	// so a Delete that races with this recovery can't flip 'removed' back to 'online'.
	if currentStatus == "offline" {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2 AND status = 'offline'`, models.StatusOnline, payload.WorkspaceID); err != nil {
			log.Printf("Heartbeat: failed to recover %s from offline: %v", payload.WorkspaceID, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOnline), payload.WorkspaceID, map[string]interface{}{})
		// RFC#2843: reconcile declared plugins on transition-to-online.
		h.fireReconcileOnline(ctx, payload.WorkspaceID)
	}

	// Auto-recovery: if a workspace is STILL marked "provisioning" by the time
	// this branch runs, transition it to "online". Defense-in-depth only: the
	// main heartbeat UPDATE above already self-heals provisioning→online via its
	// inline CASE, so on the normal path currentStatus is 'online' here and this
	// branch is a no-op. It still covers any future path that reaches
	// evaluateStatus with a 'provisioning' row that the inline CASE missed. (#1784)
	//
	// NOTE (RFC#2843 #32): because the inline CASE pre-empts this branch on the
	// real fresh-boot path, the declared-plugin reconcile is fired from the
	// heartbeat handler itself (on prevStatus=='provisioning'), NOT only here —
	// see the fireReconcileOnline call right after the main UPDATE. Do not rely
	// on this branch as the reconcile trigger; it does not fire for new boxes.
	if currentStatus == "provisioning" {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2 AND status = 'provisioning'`, models.StatusOnline, payload.WorkspaceID); err != nil {
			log.Printf("Heartbeat: failed to transition %s from provisioning to online: %v", payload.WorkspaceID, err)
		} else {
			log.Printf("Heartbeat: transitioned %s from provisioning to online (heartbeat received)", payload.WorkspaceID)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOnline), payload.WorkspaceID, map[string]interface{}{
			"recovered_from": currentStatus,
		})
		// RFC#2843: reconcile declared plugins on transition-to-online. This
		// is the primary path for a freshly-provisioned workspace's first
		// boot — provisioning→online is the only transition a new box makes.
		h.fireReconcileOnline(ctx, payload.WorkspaceID)
	}

	// Auto-recovery from awaiting_agent: external workspaces are flipped
	// to 'awaiting_agent' by registry/healthsweep when their heartbeat
	// goes stale (>staleAfter). When the operator's poller comes back —
	// for example when their laptop wakes from sleep — the heartbeat
	// resumes but does NOT re-register. Without this branch the
	// workspace would stay 'awaiting_agent' forever (visible as OFFLINE
	// in the canvas with a "Restart" CTA) even though the agent is
	// actively heartbeating.
	//
	// Discovered while smoke-testing the universal MCP path against a
	// freshly-registered external workspace: register set status=online
	// + sent one heartbeat → healthsweep then flipped back to
	// awaiting_agent because the smoke didn't loop. The molecule-mcp
	// console script's built-in heartbeat thread (PR #2413) drives
	// continuous heartbeats now, but without THIS branch those
	// heartbeats can't lift the workspace out of awaiting_agent on
	// their own.
	if currentStatus == "awaiting_agent" {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2 AND status = 'awaiting_agent'`, models.StatusOnline, payload.WorkspaceID); err != nil {
			log.Printf("Heartbeat: failed to recover %s from awaiting_agent: %v", payload.WorkspaceID, err)
		} else {
			log.Printf("Heartbeat: transitioned %s from awaiting_agent to online (heartbeat received)", payload.WorkspaceID)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOnline), payload.WorkspaceID, map[string]interface{}{
			"recovered_from": currentStatus,
		})
		// RFC#2843: reconcile declared plugins on transition-to-online.
		h.fireReconcileOnline(ctx, payload.WorkspaceID)
	}

	// Auto-recovery from failed: the provision-timeout sweeper
	// (registry/provisiontimeout.go) flips a workspace to 'failed' when it sits
	// in 'provisioning' past DefaultProvisioningTimeout (10m for claude-code).
	// But a slow cold-boot (EC2 image pull + LLM preflight) can still finish and
	// start heartbeating AFTER the flip — agent_card is written unconditionally
	// on register, so the box is genuinely serving while its status is stuck
	// 'failed'. A live heartbeat is authoritative: recover to online. Without
	// this, a healthy-but-slow workspace (e.g. a model that preflights slower
	// than the 10m budget) shows 'failed' forever despite working — the
	// mechanism behind the intermittent multi-provider e2e "boot failures". The
	// `AND status = 'failed'` guard keeps the flip conditional (won't override
	// 'removed').
	if currentStatus == "failed" {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2 AND status = 'failed'`, models.StatusOnline, payload.WorkspaceID); err != nil {
			log.Printf("Heartbeat: failed to recover %s from failed: %v", payload.WorkspaceID, err)
		} else {
			log.Printf("Heartbeat: transitioned %s from failed to online (late heartbeat after provision-timeout)", payload.WorkspaceID)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOnline), payload.WorkspaceID, map[string]interface{}{
			"recovered_from": currentStatus,
		})
		// RFC#2843: reconcile declared plugins on transition-to-online.
		h.fireReconcileOnline(ctx, payload.WorkspaceID)
	}

	// #1870 Phase 1: drain one queued A2A request if the target reports
	// spare capacity. The heartbeat's active_tasks field reflects what the
	// workspace runtime is ACTUALLY running right now, independent of
	// whatever we've counted server-side. Fire-and-forget goroutine — the
	// drain dispatches via ProxyA2ARequest which already has its own
	// timeouts, retry logic, and activity_logs wiring.
	if h.drainQueue != nil {
		var maxConcurrent int
		if err := db.DB.QueryRowContext(ctx,
			`SELECT COALESCE(max_concurrent_tasks, 1) FROM workspaces WHERE id = $1`,
			payload.WorkspaceID,
		).Scan(&maxConcurrent); err != nil {
			log.Printf("registry heartbeat: max_concurrent query failed for workspace %s: %v", payload.WorkspaceID, err)
		}
		if payload.ActiveTasks < maxConcurrent {
			// context.WithoutCancel: heartbeat handler's ctx is about to
			// expire as soon as we return. The drain needs to outlive it.
			// RFC internal#524 Layer 1: drainQueue reads db.DB; route
			// through globalGoAsync so test cleanup waits for it.
			drainCtx := context.WithoutCancel(ctx)
			wsID := payload.WorkspaceID
			capacity := maxConcurrent - payload.ActiveTasks
			if capacity < 1 {
				capacity = 1
			}
			globalGoAsync(func() { h.drainQueue(drainCtx, wsID, capacity) })
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

	// Validate agent_card.url if present — prevents SSRF via transcript proxy
	// (issue #2130). An attacker who compromises a workspace token could
	// register a metadata endpoint as the agent_card url and trick the
	// platform into forwarding the caller's bearer token to it.
	var card struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(payload.AgentCard, &card); err == nil && card.URL != "" {
		if err := isSafeURL(card.URL); err != nil {
			log.Printf("UpdateCard: workspace %s agent_card url rejected: %v", payload.WorkspaceID, err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "workspace URL not allowed"})
			return
		}
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

	h.broadcaster.RecordAndBroadcast(c.Request.Context(), string(events.EventAgentCardUpdated), payload.WorkspaceID, map[string]interface{}{
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
//
// core#2611 bootstrap-recovery re-check: when a bearer is presented and
// ValidateToken rejects it, re-query HasLiveInstanceToken. If the
// workspace now has ZERO live tokens (the previously-valid token was
// revoked between the first check and the validation — e.g. the SaaS
// provisioner's "revoke all then bootstrap-mint" sequence ran twice
// during a double-provision race), the request is allowed through as a
// fresh bootstrap. The agent's first register of the new incarnation
// will mint a token. Without this re-check, the second box in the race
// gets a permanent 401 — there is no live token to present, the box
// is dead, and the runtime's 401-as-terminal posture wedges the
// workspace "online-but-braindead".
//
// The re-check window is small (a few ms between the first HasLive
// call and ValidateToken) and the bootstrap branch is already gated
// to the no-live-tokens state, so the re-open does not weaken the
// C18 anti-hijack guarantee (an attacker still cannot bootstrap
// while ANY live token exists).
func (h *RegistryHandler) requireWorkspaceToken(
	ctx gincontext, c *gin.Context, workspaceID string,
) error {
	// Bootstrap allowance keys on INSTANCE tokens only: a live API-kind
	// token (the Create 201 bearer a platform caller holds) must not force
	// the fresh, credential-less instance to present a bearer. core#1644.
	hasLive, err := wsauth.HasLiveInstanceToken(ctx, db.DB, workspaceID)
	if err != nil {
		// DB error checking token existence — fail open so we don't take
		// the whole heartbeat path down on a transient hiccup. Log loudly.
		log.Printf("wsauth: HasLiveInstanceToken(%s) failed: %v — allowing request", workspaceID, err)
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
		// core#2611: re-check for zero live tokens before returning 401.
		// If the previously-valid token was revoked in the gap between
		// HasLiveInstanceToken and ValidateToken (a SaaS provisioner's
		// "revoke all" passing through, a CP double-provision race
		// revoking the winner's token, etc.), the presented bearer is
		// stale by definition — the workspace has no live token, so the
		// only safe action is to re-open bootstrap. The caller's next
		// /registry/register iteration will mint a fresh token.
		nowLive, nowLiveErr := wsauth.HasLiveInstanceToken(ctx, db.DB, workspaceID)
		if nowLiveErr == nil && !nowLive {
			log.Printf("wsauth: core#2611 bootstrap-recovery for %s — bearer was revoked between HasLive and ValidateToken; re-opening bootstrap", workspaceID)
			return nil
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid workspace auth token"})
		return err
	}
	return nil
}

// gincontext is an alias for context.Context kept separate so callers can
// see "gin.Context.Request.Context() is what we want" without re-typing
// the import-heavy standard type.
type gincontext = context.Context

// fetchExistingWorkspaceStateForDiagnostics reads the workspace's
// current row state (url, kind, delivery_mode) for the diagnostic
// log line. Best-effort: a DB error here is logged+ignored; the
// diagnostic line emits "(unavailable)" for the missing fields.
//
// Why best-effort: the diagnostic MUST NOT introduce a new failure
// path. The function is called before the validation chain runs;
// if it errored loudly the operator would see a SECOND 500 on top
// of the original 400, and the new error would mask the original
// cause. The defer boot_register_failed log captures the row's
// failure timestamp for follow-up triage.
func (h *RegistryHandler) fetchExistingWorkspaceStateForDiagnostics(ctx context.Context, workspaceID string) registerDiagnostics {
	var d registerDiagnostics
	if workspaceID == "" {
		return d
	}
	var url, kind, mode sql.NullString
	if err := db.DB.QueryRowContext(ctx,
		`SELECT url, kind, delivery_mode FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&url, &kind, &mode); err == nil {
		if url.Valid {
			d.ExistingURL = url.String
		}
		if kind.Valid {
			d.ExistingKind = kind.String
		}
		if mode.Valid {
			d.ExistingDeliveryMode = mode.String
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		log.Printf("Registry register: diagnostics fetch failed for %s: %v", workspaceID, err)
	}
	return d
}

// registerDiagnostics captures the workspace's existing row state at
// the time of a 400 response, so the operator can compare the
// request payload against the row to identify the drift source.
// All fields are best-effort; empty string means "unavailable"
// (either the row didn't exist, the column was NULL, or the fetch
// errored).
type registerDiagnostics struct {
	ExistingURL          string
	ExistingKind         string
	ExistingDeliveryMode string
}

// logRegister400Reason emits a single grep-able log line for every
// 400 path in /registry/register. The line shape is stable
// (`registry_register_400`) so operators can grep Loki for the
// class. Fields:
//
//	workspace_id         — the requested ID
//	reason               — short key (invalid_json | invalid_delivery_mode |
//	                       invalid_kind | url_required_for_push | url_validate_failed)
//	payload_url          — the URL the agent sent in the payload (may be empty)
//	payload_card_url     — the URL the agent put in its agent_card (may be empty)
//	payload_kind         — the kind the agent sent
//	payload_delivery_mode — the delivery_mode the agent sent
//	existing_url         — the URL already on the workspaces row (or "(new)")
//	existing_kind        — the kind already on the row (or "(new)")
//	existing_delivery_mode — the delivery_mode already on the row (or "(new)")
//	detail               — the failure-specific detail (parse error, validation
//	                       error, missing-field note, etc.)
//
// The class is part of the #2680 residual: a recreated container's
// first /registry/register call has been returning 400 with no
// actionable signal. The deferred boot_register_failed log fires too
// late (after the 400 has already been returned to the client) and
// only carries the status code, not the reason. This line is
// emitted synchronously inside each 400 path, BEFORE the response
// is written, so the next restart run will surface the cause
// directly.
func logRegister400Reason(reason, workspaceID string, payload models.RegisterPayload, existing registerDiagnostics, detail string) {
	cardURLPresence := urlPresence(agentCardURL(payload.AgentCard))
	payloadURLPresence := urlPresence(payload.URL)
	exURLPresence := "(new)"
	if existing.ExistingURL != "" {
		exURLPresence = "present"
	}
	exKind := existing.ExistingKind
	if exKind == "" {
		exKind = "(new)"
	}
	exMode := existing.ExistingDeliveryMode
	if exMode == "" {
		exMode = "(new)"
	}
	log.Printf("registry_register_400 workspace=%s reason=%s payload_url=%s payload_card_url=%s payload_kind=%q payload_delivery_mode=%q existing_url=%s existing_kind=%q existing_delivery_mode=%q detail=%q",
		workspaceID, reason,
		payloadURLPresence, cardURLPresence, payload.Kind, payload.DeliveryMode,
		exURLPresence, exKind, exMode,
		detail,
	)
}

// urlPresence reports "present" vs "absent" for a URL string,
// without ever logging the URL value. Used by logRegister400Reason
// to redact the URL columns (see RC #11335: workspace URLs can be
// private — Hetzner 10.0.0.x, GCP 10.x.x.x, in-VPC 172.31.x.x —
// and the prior implementation leaked them to anyone with Loki
// read access).
func urlPresence(url string) string {
	if url == "" {
		return "absent"
	}
	return "present"
}
