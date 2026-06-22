package handlers

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/middleware"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/orgtoken"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provisioner"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/registry"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
)

type DiscoveryHandler struct{}

func NewDiscoveryHandler() *DiscoveryHandler {
	return &DiscoveryHandler{}
}

// Discover handles GET /registry/discover/:id
func (h *DiscoveryHandler) Discover(c *gin.Context) {
	targetID := c.Param("id")
	callerID := c.GetHeader("X-Workspace-ID")

	if callerID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Workspace-ID header is required"})
		return
	}

	// Phase 30.6 — verify the caller's bearer token before revealing any
	// peer URL. Without this, a random internet host that knows a
	// workspace ID could enumerate siblings. Legacy workspaces (no
	// live tokens) grandfather through the same way heartbeat does.
	if err := validateDiscoveryCaller(c.Request.Context(), c, callerID); err != nil {
		return // response already written
	}

	if !registry.CanCommunicate(callerID, targetID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "not authorized to discover this workspace"})
		return
	}

	ctx := c.Request.Context()

	// Workspace-to-workspace: return Docker-internal URL (containers can't
	// reach host ports). External targets need their registered URL with
	// 127.0.0.1/localhost rewritten to host.docker.internal when the caller
	// is itself a Docker container.
	if callerID != "" {
		discoverWorkspacePeer(ctx, c, callerID, targetID)
		return
	}
	discoverHostPeer(ctx, c, targetID)
}

// discoverHostPeer handles the canvas/external (no X-Workspace-ID) branch of
// Discover. It returns the host-accessible URL for `targetID`, following any
// forwarding chain (max 5 hops). Currently unreachable because Discover
// requires the X-Workspace-ID header up front, but kept to preserve the
// original code path 1:1 in case the requirement is relaxed.
func discoverHostPeer(ctx context.Context, c *gin.Context, targetID string) {
	if url, err := db.GetCachedURL(ctx, targetID); err == nil {
		c.JSON(http.StatusOK, gin.H{"id": targetID, "url": url})
		return
	}

	var url sql.NullString
	var status string
	var forwardedTo sql.NullString
	err := db.DB.QueryRowContext(ctx,
		`SELECT url, status, forwarded_to FROM workspaces WHERE id = $1`, targetID,
	).Scan(&url, &status, &forwardedTo)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	// Follow forwarding chain (max 5 hops to prevent loops)
	resolvedID := targetID
	for i := 0; i < 5 && forwardedTo.Valid && forwardedTo.String != ""; i++ {
		resolvedID = forwardedTo.String
		err = db.DB.QueryRowContext(ctx,
			`SELECT url, status, forwarded_to FROM workspaces WHERE id = $1`, resolvedID,
		).Scan(&url, &status, &forwardedTo)
		if err != nil {
			break
		}
	}

	if !url.Valid || url.String == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace has no URL", "status": status})
		return
	}

	// #1484 SSRF defense-in-depth: the URL came from the workspaces table
	// without any per-write validation (workspace runtimes register their
	// own URLs via /registry/register, and a misbehaving / compromised
	// runtime could register a 169.254.169.254 metadata URL). Validate
	// before handing it to the caller, who might dispatch HTTP against it.
	// Currently gated by the bearer-required Discover handler, but this
	// guard makes discoverHostPeer safe regardless of upstream auth shape.
	if err := isSafeURL(url.String); err != nil {
		log.Printf("Discovery: rejecting unsafe registered URL for %s (#1484): %v", resolvedID, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace URL failed safety check", "status": status})
		return
	}

	db.CacheURL(ctx, resolvedID, url.String)
	c.JSON(http.StatusOK, gin.H{
		"id":     resolvedID,
		"url":    url.String,
		"status": status,
	})
}

// discoverWorkspacePeer handles the workspace-to-workspace branch of Discover —
// resolves an internal/Docker-routable URL for `targetID` from the perspective
// of `callerID` and writes the JSON response (or an appropriate 404/503 error).
func discoverWorkspacePeer(ctx context.Context, c *gin.Context, callerID, targetID string) {
	var wsName, wsRuntime string
	db.DB.QueryRowContext(ctx, `SELECT COALESCE(name,''), COALESCE(runtime,'claude-code') FROM workspaces WHERE id = $1`, targetID).Scan(&wsName, &wsRuntime)

	// External workspaces: return their registered URL.
	// Rewrite 127.0.0.1/localhost → host.docker.internal ONLY when the
	// caller itself is a Docker container; a remote (external) caller
	// lives on the other side of the wire and needs the URL as-is
	// (localhost rewrites wouldn't resolve from its host anyway).
	// Phase 30.6.
	if isExternalLikeRuntime(wsRuntime) {
		if handled := writeExternalWorkspaceURL(ctx, c, callerID, targetID, wsName); handled {
			return
		}
	}

	// Try cached internal URL first
	if internalURL, err := db.GetCachedInternalURL(ctx, targetID); err == nil && internalURL != "" {
		c.JSON(http.StatusOK, gin.H{"id": targetID, "url": internalURL, "name": wsName})
		return
	}
	// Fallback: only synthesize a URL if the workspace exists and is online/degraded
	var wsStatus string
	dbErr := db.DB.QueryRowContext(ctx,
		`SELECT status FROM workspaces WHERE id = $1`, targetID,
	).Scan(&wsStatus)
	if dbErr == nil && (wsStatus == "online" || wsStatus == "degraded") {
		internalURL := provisioner.InternalURL(targetID)
		if cacheErr := db.CacheInternalURL(ctx, targetID, internalURL); cacheErr != nil {
			log.Printf("Discovery: failed to cache internal URL for %s: %v", targetID, cacheErr)
		}
		c.JSON(http.StatusOK, gin.H{"id": targetID, "url": internalURL, "name": wsName})
		return
	}
	// Workspace is not reachable — don't fall through to host URL path
	if dbErr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace not available", "status": wsStatus})
	} else {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
	}
}

// writeExternalWorkspaceURL resolves the registered URL for an external-runtime
// target and writes the response. Returns true when a response was written
// (URL present); returns false when the external workspace has no URL on
// file, leaving the caller to fall through to the internal-URL path.
func writeExternalWorkspaceURL(ctx context.Context, c *gin.Context, callerID, targetID, wsName string) bool {
	var wsURL string
	db.DB.QueryRowContext(ctx, `SELECT COALESCE(url,'') FROM workspaces WHERE id = $1`, targetID).Scan(&wsURL)
	if wsURL == "" {
		return false
	}
	outURL := wsURL
	var callerRuntime string
	db.DB.QueryRowContext(ctx, `SELECT COALESCE(runtime,'claude-code') FROM workspaces WHERE id = $1`, callerID).Scan(&callerRuntime)
	if !isExternalLikeRuntime(callerRuntime) {
		outURL = strings.Replace(outURL, "127.0.0.1", "host.docker.internal", 1)
		outURL = strings.Replace(outURL, "localhost", "host.docker.internal", 1)
	}

	// #1484 SSRF defense-in-depth — same rationale as discoverHostPeer.
	// We validate the post-rewrite URL because the rewrite changes which
	// host the caller would dispatch against (host.docker.internal is
	// only reachable inside a docker network; isSafeURL accepts it but
	// blocks a metadata IP that survived the rewrite untouched).
	if err := isSafeURL(outURL); err != nil {
		log.Printf("Discovery: rejecting unsafe external workspace URL for %s (#1484): %v", targetID, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace URL failed safety check"})
		return true
	}

	c.JSON(http.StatusOK, gin.H{"id": targetID, "url": outURL, "name": wsName})
	return true
}

// Peers handles GET /registry/:id/peers
//
// Optional “?q=<substring>“ filters the result by case-insensitive
// substring match against “name“ or “role“ (#1038). Filtering is done
// in Go after the DB read — keeps the SQL identical to the no-filter path
// (no injection risk, no DB-driver collation surprises) at the cost of
// loading the unfiltered set first. Acceptable because the peer set is
// always bounded by the small fanout of a single workspace's parent +
// children + siblings (typically <50 rows).
func (h *DiscoveryHandler) Peers(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Phase 30.6 — the peer list leaks sibling identities and URLs.
	// Require the bearer token bound to `workspaceID` before returning it.
	// The caller HERE is identified by the URL path param, not a header,
	// because `/registry/:id/peers` is scoped to "my own peers" — a
	// workspace asking for its own view of the team.
	if err := validateDiscoveryCaller(ctx, c, workspaceID); err != nil {
		return // response already written
	}

	var parentID sql.NullString
	err := db.DB.QueryRowContext(ctx, `SELECT parent_id FROM workspaces WHERE id = $1`, workspaceID).
		Scan(&parentID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	var peers []map[string]interface{}

	// Siblings — workspaces sharing the caller's parent.
	//
	// #1953 cross-tenant isolation: the OLD code's else-branch handled the
	// org-root caller (parent_id IS NULL) by returning EVERY workspace with
	// parent_id IS NULL — i.e. every other tenant's org root, since the
	// workspaces table has no org_id column. That leaked peer identities/URLs
	// across tenants. An org root has no siblings inside its own org (each
	// tenant is a distinct org root), so the org-root caller now gets an empty
	// sibling set; its real peers are its children, returned below. Only the
	// parent_id-bound branch enumerates siblings, and that is already scoped to
	// one parent (one tenant).
	if parentID.Valid {
		siblings, _ := queryPeerMaps(ctx, `
			SELECT w.id, w.name, COALESCE(w.role, ''), w.tier, w.status,
				   COALESCE(w.agent_card, 'null'::jsonb), COALESCE(w.url, ''),
				   w.parent_id, w.active_tasks
			FROM workspaces w WHERE w.parent_id = $1 AND w.id != $2 AND w.status != 'removed'`,
			parentID.String, workspaceID)
		peers = append(peers, siblings...)
	}

	// Children — exclude self defensively. A child row whose parent_id
	// equals the requesting workspaceID can never legitimately be the
	// caller (a workspace can't be its own child), but a future data-
	// integrity defect (e.g. self-loop introduced by a buggy register
	// path) would otherwise smuggle the caller back into its own peer
	// list. The agent then attempts `delegate_task(<own_id>)`, which
	// either deadlocks _run_lock (sync path) or hits the platform's
	// self-delegation 400 in a tight loop (#383). The `w.id != $2`
	// clause makes self-delegation-via-peer-list impossible regardless
	// of DB state.
	children, _ := queryPeerMaps(ctx, `
		SELECT w.id, w.name, COALESCE(w.role, ''), w.tier, w.status,
			   COALESCE(w.agent_card, 'null'::jsonb), COALESCE(w.url, ''),
			   w.parent_id, w.active_tasks
		FROM workspaces w WHERE w.parent_id = $1 AND w.id != $2 AND w.status != 'removed'`,
		workspaceID, workspaceID)
	peers = append(peers, children...)

	// Parent — same defense-in-depth. A workspace whose parent_id points
	// to itself is data corruption, but the peer-list endpoint must not
	// propagate that corruption back to the agent as a "peer who is also
	// you" entry.
	if parentID.Valid {
		parent, _ := queryPeerMaps(ctx, `
			SELECT w.id, w.name, COALESCE(w.role, ''), w.tier, w.status,
				   COALESCE(w.agent_card, 'null'::jsonb), COALESCE(w.url, ''),
				   w.parent_id, w.active_tasks
			FROM workspaces w WHERE w.id = $1 AND w.id != $2 AND w.status != 'removed'`,
			parentID.String, workspaceID)
		peers = append(peers, parent...)
	}

	// #383 final-line defense: even if a future code path adds a query
	// that doesn't filter self, strip the caller's own row before
	// returning. Cheap O(n) over a peer set bounded at <50 rows.
	peers = excludeSelfFromPeers(peers, workspaceID)

	peers = filterPeersByQuery(peers, c.Query("q"))

	if peers == nil {
		peers = make([]map[string]interface{}, 0)
	}
	c.JSON(http.StatusOK, peers)
}

// excludeSelfFromPeers strips any peer entry whose “id“ equals
// “workspaceID“ (the caller's own row). Final-line defense for #383
// (self-delegation 400-loop on external workspaces): a peer-list that
// includes the requester's own row is the root mechanism by which an
// agent ends up delegating to itself. The pre-DB filters in Peers
// already enforce `w.id != $caller` on each branch; this function
// guarantees the contract holds regardless of which query path
// returned the row, including future ones added without a self-filter.
//
// O(n) over a peer set bounded at <50 rows per `Peers` comment — well
// below the hot-path overhead of the existing filterPeersByQuery.
func excludeSelfFromPeers(peers []map[string]interface{}, workspaceID string) []map[string]interface{} {
	if len(peers) == 0 {
		return peers
	}
	out := make([]map[string]interface{}, 0, len(peers))
	for _, p := range peers {
		id, _ := p["id"].(string)
		if id == workspaceID {
			continue
		}
		out = append(out, p)
	}
	return out
}

// filterPeersByQuery returns peers whose name or role case-insensitively
// contains q. Whitespace-trimmed empty q is a no-op (returns input unchanged).
func filterPeersByQuery(peers []map[string]interface{}, q string) []map[string]interface{} {
	q = strings.TrimSpace(q)
	if q == "" {
		return peers
	}
	needle := strings.ToLower(q)
	out := make([]map[string]interface{}, 0, len(peers))
	for _, p := range peers {
		name, _ := p["name"].(string) // nil → "" — safe on empty-role rows
		role, _ := p["role"].(string) // nil → "" — queryPeerMaps sets nil when DB role is empty
		if strings.Contains(strings.ToLower(name), needle) ||
			strings.Contains(strings.ToLower(role), needle) {
			out = append(out, p)
		}
	}
	return out
}

// queryPeerMaps returns clean JSON-serializable maps instead of Workspace structs.
func queryPeerMaps(ctx context.Context, query string, args ...interface{}) ([]map[string]interface{}, error) {
	rows, err := db.DB.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("queryPeerMaps error: %v", err)
		return nil, err
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var id, name, role, status, url string
		var tier, activeTasks int
		var parentID *string
		var agentCard []byte

		err := rows.Scan(&id, &name, &role, &tier, &status, &agentCard, &url, &parentID, &activeTasks)
		if err != nil {
			log.Printf("queryPeerMaps scan error: %v", err)
			continue
		}

		peer := map[string]interface{}{
			"id":           id,
			"name":         name,
			"tier":         tier,
			"status":       status,
			"url":          url,
			"parent_id":    parentID,
			"active_tasks": activeTasks,
		}

		if role != "" {
			peer["role"] = role
		} else {
			peer["role"] = nil
		}

		if len(agentCard) > 0 && string(agentCard) != "null" {
			peer["agent_card"] = json.RawMessage(agentCard)
		} else {
			peer["agent_card"] = nil
		}

		result = append(result, peer)
	}
	if err := rows.Err(); err != nil {
		log.Printf("queryPeerMaps rows.Err: %v", err)
	}
	return result, nil
}

// CheckAccess handles POST /registry/check-access
func (h *DiscoveryHandler) CheckAccess(c *gin.Context) {
	var payload struct {
		CallerID string `json:"caller_id" binding:"required"`
		TargetID string `json:"target_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	allowed := registry.CanCommunicate(payload.CallerID, payload.TargetID)
	c.JSON(http.StatusOK, gin.H{"allowed": allowed})
}

// validateDiscoveryCaller enforces the Phase 30.6 bearer-token contract
// on the discovery endpoints. Same lazy-bootstrap shape as the registry
// and secrets handlers: legacy workspaces with no tokens are grandfathered,
// workspaces with tokens must present a matching Bearer, token binding
// is strict (A's token cannot authenticate caller B).
//
// (harden/no-fail-open-auth) Fails CLOSED on DB error. This used to return nil
// (allow) on a HasAnyLiveToken hiccup "because discovery only exposes peer URLs
// already behind CanCommunicate" — but the CTO "nothing fail-open" directive is
// absolute, and a request must never gain access because the auth datastore is
// unreachable. A datastore error now writes 503 (availability tradeoff that
// grants NO access) and returns a non-nil error; the caller already does
// `if err != nil { return }` so the 503 body is what the client sees.
func validateDiscoveryCaller(ctx context.Context, c *gin.Context, workspaceID string) error {
	hasLive, err := wsauth.HasAnyLiveToken(ctx, db.DB, workspaceID)
	if err != nil {
		log.Printf("wsauth: discovery HasAnyLiveToken(%s): datastore lookup failed (returning 503): %v", workspaceID, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "platform datastore unavailable — retry shortly",
			"code":  "platform_unavailable",
		})
		return errors.New("auth datastore unavailable")
	}
	if !hasLive {
		return nil // legacy / pre-upgrade
	}
	// (harden/no-fail-open-auth) The former dev-mode escape hatch that
	// returned nil (allow) here when MOLECULE_ENV=dev + ADMIN_TOKEN unset
	// has been REMOVED. Discovery callers must present a verified CP
	// session or a valid bearer in every environment. Local dev now
	// authenticates the Canvas with a provisioned ADMIN_TOKEN /
	// NEXT_PUBLIC_ADMIN_TOKEN (see scripts/dev-start.sh), so the Details
	// tab loads peers with a real credential rather than via fail-open.

	// Precedence MUST match middleware.WorkspaceAuth: try the bearer token
	// first (admin → org → per-workspace), and only fall back to a verified
	// CP-session cookie when no bearer is presented. Keeping the two auth
	// surfaces in the same order means a credential that passes one passes
	// the other — divergent precedence is how an admin/org bearer ended up
	// 401'ing on one surface but not the other.
	tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
	if tok != "" {
		// Admin-token fallback — lets the canvas operator (dashboard /
		// concierge Settings config tabs) read a workspace's peers with the
		// single admin credential, mirroring middleware.WorkspaceAuth.
		// Without this the operator's admin bearer fell through to the
		// per-workspace ValidateToken below and 401'd for any workspace it
		// doesn't personally hold a token for — e.g. the platform agent
		// surfaced in the concierge config tabs.
		if adminSecret := os.Getenv("ADMIN_TOKEN"); adminSecret != "" &&
			subtle.ConstantTimeCompare([]byte(tok), []byte(adminSecret)) == 1 {
			return nil
		}
		// Org-scoped API token — grants access to every workspace in the org
		// (same product spec as WorkspaceAuth). Checked before the
		// per-workspace token so an org-key presenter doesn't hit the
		// narrower failure path.
		if _, _, _, err := orgtoken.Validate(ctx, db.DB, tok, orgtoken.AuditLogRequestContextFromGin(c), "", false); err == nil {
			return nil
		} else if !errors.Is(err, orgtoken.ErrInvalidToken) {
			log.Printf("wsauth: discovery orgtoken.Validate(%s): datastore lookup failed (returning 503): %v", workspaceID, err)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "platform datastore unavailable — retry shortly",
				"code":  "platform_unavailable",
			})
			return err
		}
		if err := wsauth.ValidateToken(ctx, db.DB, workspaceID, tok); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid workspace auth token"})
			return err
		}
		return nil
	}

	// No bearer: SaaS-canvas path authenticates via a CP-session cookie.
	// VerifiedCPSession returns (valid, presented, userID):
	//   - (false, "", false) = no cookie, 401 (missing auth)
	//   - (true, userID, true)   = valid session, allow
	//   - (false, "", true)  = cookie presented but invalid, 401
	ok, presented, _ := middleware.VerifiedCPSession(c.GetHeader("Cookie"))
	if ok {
		return nil
	}
	if presented {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid session"})
		return errors.New("invalid session")
	}
	c.JSON(http.StatusUnauthorized, gin.H{"error": "missing workspace auth token"})
	return errors.New("missing token")
}
