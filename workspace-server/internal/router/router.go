package router

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/buildinfo"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/channels"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/handlers"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/pendinguploads"
	memwiring "github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/wiring"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/metrics"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/middleware"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/supervised"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/ws"
	"github.com/docker/docker/client"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func Setup(hub *ws.Hub, broadcaster *events.Broadcaster, prov *provisioner.Provisioner, platformURL, configsDir string, wh *handlers.WorkspaceHandler, channelMgr *channels.Manager, memBundle *memwiring.Bundle) *gin.Engine {
	r := gin.Default()

	// Issue #179 — trust no reverse-proxy headers. Without this call Gin's
	// default is to trust ALL X-Forwarded-For values, which lets any caller
	// spoof their IP and bypass per-IP rate limiting. With nil, c.ClientIP()
	// always returns the real TCP RemoteAddr.
	if err := r.SetTrustedProxies(nil); err != nil {
		panic("router: SetTrustedProxies: " + err.Error())
	}

	// CORS origins — configurable via CORS_ORIGINS env var (comma-separated)
	corsOrigins := []string{"http://localhost:3000", "http://localhost:3001"}
	if v := os.Getenv("CORS_ORIGINS"); v != "" {
		corsOrigins = strings.Split(v, ",")
	}
	r.Use(cors.New(cors.Config{
		AllowOrigins:     corsOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "X-Workspace-ID", "Authorization"},
		AllowCredentials: true,
	}))

	// Rate limiting — configurable via RATE_LIMIT env var (default 600 req/min)
	// 15 workspaces × 2 heartbeats/min + canvas polling + user actions needs headroom
	rateLimit := 600
	if v := os.Getenv("RATE_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rateLimit = n
		}
	}
	limiter := middleware.NewRateLimiter(rateLimit, time.Minute, context.Background())
	r.Use(limiter.Middleware())

	// Prometheus metrics middleware — records every request's method/path/status/latency.
	// Must be registered after rate limiter so aborted requests are also counted.
	r.Use(metrics.Middleware())

	// Tenant guard — the public repo's only SaaS hook. When MOLECULE_ORG_ID is
	// set (only by the private molecule-controlplane provisioner on tenant Fly
	// Machines), rejects requests whose X-Molecule-Org-Id header doesn't match.
	// Unset (self-hosted / dev / CI) → no-op. Registered after metrics so
	// rejected requests still land on the 4xx counter.
	r.Use(middleware.TenantGuard())

	// Security headers (#151) — sets X-Content-Type-Options, X-Frame-Options,
	// Referrer-Policy, Content-Security-Policy, Permissions-Policy, HSTS on
	// every response. Tests in securityheaders_test.go assert each header is
	// present and that handler-set headers are not overridden. Registered
	// last so a handler can still opt out by setting its own header before
	// c.Next() returns.
	r.Use(middleware.SecurityHeaders())

	// Health
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// Build info — public, no auth. Returns the git SHA the binary was
	// linked from. Existence reason is in buildinfo/buildinfo.go: lets the
	// redeploy workflow verify each tenant is actually running the
	// published code (closing #2395 — ssm_status=Success is "the deploy
	// command ran", not "the new code is running"). Public is intentional:
	// it's a build identifier, not operational state. The same string is
	// already published as org.opencontainers.image.revision on the
	// container image, so no new info is exposed.
	r.GET("/buildinfo", func(c *gin.Context) {
		c.JSON(200, gin.H{"git_sha": buildinfo.GitSHA})
	})

	// /admin/liveness — per-subsystem last-tick timestamps. Operators read this
	// to catch stuck-but-not-crashed goroutines (the failure mode that caused
	// the 12h scheduler outage of 2026-04-14, issue #85). Any subsystem whose
	// last tick is older than 2× its expected interval is stale.
	//
	// #166: gated behind AdminAuth. Internal health state is an ops-intel leak
	// in production (scheduler tick cadence reveals fleet size + work pattern).
	r.GET("/admin/liveness", middleware.AdminAuth(db.DB), func(c *gin.Context) {
		snap := supervised.Snapshot()
		out := make(map[string]interface{}, len(snap))
		now := time.Now()
		for name, last := range snap {
			out[name] = gin.H{
				"last_tick_at":    last,
				"seconds_ago":     int(now.Sub(last).Seconds()),
			}
		}
		c.JSON(200, gin.H{"subsystems": out})
	})

	// Prometheus metrics — exempt from rate limiter via separate registration
	// (registered before Use(limiter) takes effect on this specific route — the
	// middleware.Middleware() still records it for observability).
	// Scrape with: curl http://localhost:8080/metrics
	r.GET("/metrics", metrics.Handler())

	// Single-workspace read — open so canvas nodes can fetch their own state
	// without a token (used by WorkspaceNode polling and health checks).
	r.GET("/workspaces/:id", wh.Get)

	// C1 + C20: workspace list and life-cycle mutations gated behind AdminAuth.
	// Fail-open when no tokens exist anywhere (fresh install / pre-Phase-30).
	// Blocks:
	//   C1   — unauthenticated GET /workspaces (workspace topology exposure)
	//   C20  — unauthenticated DELETE /workspaces/:id (mass-deletion attack)
	//          unauthenticated POST /workspaces (workspace creation)
	{
		wsAdmin := r.Group("", middleware.AdminAuth(db.DB))
		wsAdmin.GET("/workspaces", wh.List)
		wsAdmin.POST("/workspaces", wh.Create)
		wsAdmin.DELETE("/workspaces/:id", wh.Delete)
		// Out-of-band bootstrap signal: CP's watcher POSTs here when it
		// detects "RUNTIME CRASHED" in a workspace EC2 console output,
		// so the canvas flips to failed in seconds instead of waiting
		// for the 10-minute provision-timeout sweeper.
		wsAdmin.POST("/admin/workspaces/:id/bootstrap-failed", wh.BootstrapFailed)
		// Proxy to CP's serial-console endpoint so the canvas's "View
		// Logs" button can render the actual boot trace without handing
		// the tenant AWS credentials. Admin-gated because console output
		// can include user-data snippets we treat as semi-sensitive.
		wsAdmin.GET("/workspaces/:id/console", wh.Console)

		// Admin memory backup/restore (#1051) — bulk export/import of agent
		// memories for safe Docker rebuilds. Matches workspaces by name on import.
		// F1084/#1131: Export applies redactSecrets before returning content.
		// F1085/#1132: Import applies redactSecrets before persisting content.)
		adminMemH := handlers.NewAdminMemoriesHandler()
		if memBundle != nil {
			adminMemH.WithMemoryV2(memBundle.Plugin, memBundle.Resolver)
		}
		wsAdmin.GET("/admin/memories/export", adminMemH.Export)
		wsAdmin.POST("/admin/memories/import", adminMemH.Import)
	}

	// A2A proxy — registered outside the auth group; already enforces CanCommunicate access control.
	r.POST("/workspaces/:id/a2a", wh.ProxyA2A)

	// A2A queue status lookup (RFC #2331 Tier 1) — registered outside the
	// workspace auth group because the row's caller_id may be a DIFFERENT
	// workspace than :id. The handler runs its own access check (caller
	// must match queue.caller_id OR queue.workspace_id, OR hold an
	// org-level token). Existence-non-inferring 404 on auth failure.
	r.GET("/workspaces/:id/a2a/queue/:queue_id", wh.GetA2AQueueStatus)

	// Auth-gated workspace sub-routes — ALL /workspaces/:id/* paths except /a2a.
	// Fix A (Cycle 5): single WorkspaceAuth middleware blocks C2-C5, C7-C9, C12, C13
	// by requiring a valid bearer token for any workspace that has one on file.
	// Legacy workspaces (no token) are grandfathered to allow rolling upgrades.
	wsAuth := r.Group("/workspaces/:id", middleware.WorkspaceAuth(db.DB))
	{
		// #680: PATCH /workspaces/:id moved under WorkspaceAuth (#680 IDOR fix).
		// WorkspaceAuth enforces that the caller holds a valid bearer token for
		// this specific workspace — both auth AND ownership in one check. Cosmetic
		// updates (x/y drag-reposition, inline rename) from the combined tenant
		// image canvas still pass via the isSameOriginCanvas bypass in WorkspaceAuth.
		wsAuth.PATCH("", wh.Update)

		// Lifecycle
		wsAuth.GET("/state", wh.State)
		wsAuth.POST("/restart", wh.Restart)
		wsAuth.POST("/pause", wh.Pause)
		wsAuth.POST("/resume", wh.Resume)
		// Manual hibernate (opt-in, #711) — stops the container and sets status
		// to 'hibernated'. The workspace auto-wakes on the next A2A message.
		wsAuth.POST("/hibernate", wh.Hibernate)

		// External-workspace credential lifecycle (issue #319 follow-up to
		// the Create flow). Both endpoints reject runtime ≠ external with
		// 400 — see external_rotate.go for the rationale.
		//
		//   POST .../external/rotate     — mint fresh token, revoke prior,
		//                                  return ExternalConnectionInfo
		//   GET  .../external/connection — return ExternalConnectionInfo
		//                                  with auth_token="" (re-show
		//                                  instructions without rotating)
		wsAuth.POST("/external/rotate", wh.RotateExternalCredentials)
		wsAuth.GET("/external/connection", wh.GetExternalConnection)

		// Async Delegation
		delh := handlers.NewDelegationHandler(wh, broadcaster)
		wsAuth.POST("/delegate", delh.Delegate)
		wsAuth.GET("/delegations", delh.ListDelegations)
		// Record-only endpoint for agent-initiated delegations (#64). Agent-side
		// delegate_to_workspace fires A2A directly for speed + OTEL propagation;
		// this endpoint just adds an activity_logs row so GET /delegations returns
		// the same set the agent's local `check_delegation_status` sees.
		wsAuth.POST("/delegations/record", delh.Record)
		wsAuth.POST("/delegations/:delegation_id/update", delh.UpdateStatus)

		// Traces (Langfuse proxy)
		trh := handlers.NewTracesHandler()
		wsAuth.GET("/traces", trh.List)

		// Live agent transcript proxy — surfaces the runtime-specific session
		// log (claude-code reads ~/.claude/projects/<cwd>/<session>.jsonl).
		// Lets canvas / operators see live tool calls + AI thinking instead
		// of waiting for the high-level activity log to flush.
		trsh := handlers.NewTranscriptHandler()
		wsAuth.GET("/transcript", trsh.Get)

		// Agent Memories (HMA)
		memsh := handlers.NewMemoriesHandler()
		wsAuth.POST("/memories", memsh.Commit)
		wsAuth.GET("/memories", memsh.Search)
		wsAuth.DELETE("/memories/:memoryId", memsh.Delete)
		wsAuth.PATCH("/memories/:memoryId", memsh.Update)

		// Approvals
		apph := handlers.NewApprovalsHandler(broadcaster)
		wsAuth.POST("/approvals", apph.Create)
		wsAuth.GET("/approvals", apph.List)
		wsAuth.POST("/approvals/:approvalId/decide", apph.Decide)
		// /approvals/pending is a cross-workspace admin path; WorkspaceAuth cannot
		// be used here (no workspace scope), but it still needs auth so an
		// unauthenticated caller cannot enumerate all pending approvals across the
		// entire platform. Gated behind AdminAuth (issue #180).
		r.GET("/approvals/pending", middleware.AdminAuth(db.DB), apph.ListAll)

		// Team handlers — Collapse only. The bulk-Expand path is gone:
		// every workspace can have children via the regular CreateWorkspace
		// flow with parent_id set, so a separate handler that bulk-creates
		// from sub_workspaces (and was non-idempotent — calling it twice
		// duplicated the team) earned its way out.
		teamh := handlers.NewTeamHandler(broadcaster, wh, platformURL, configsDir)
		wsAuth.POST("/collapse", teamh.Collapse)

		// Agents
		ah := handlers.NewAgentHandler(broadcaster)
		wsAuth.POST("/agent", ah.Assign)
		wsAuth.PATCH("/agent", ah.Replace)
		wsAuth.DELETE("/agent", ah.Remove)
		wsAuth.POST("/agent/move", ah.Move)
	}

	// Registry
	rh := handlers.NewRegistryHandler(broadcaster)
	// #1870 Phase 1: wire the queue drain hook so Heartbeat can dispatch
	// a queued A2A request when the workspace reports spare capacity.
	rh.SetQueueDrainFunc(wh.DrainQueueForWorkspace)
	r.POST("/registry/register", rh.Register)
	r.POST("/registry/heartbeat", rh.Heartbeat)
	r.POST("/registry/update-card", rh.UpdateCard)

	// Webhooks
	whh := handlers.NewWebhookHandlerWithWorkspace(wh)
	r.POST("/webhooks/github", whh.GitHub)
	r.POST("/webhooks/github/:id", whh.GitHub)

	// Discovery
	dh := handlers.NewDiscoveryHandler()
	r.GET("/registry/discover/:id", dh.Discover)
	r.GET("/registry/:id/peers", dh.Peers)
	r.POST("/registry/check-access", dh.CheckAccess)

	// Events — #165: gated behind AdminAuth. The raw event log contains org
	// topology, workspace names, and agent-card fragments; an unauth read
	// leaks the entire fleet structure. GET /events/:workspaceId is still
	// a cross-workspace read so it uses AdminAuth, not WorkspaceAuth.
	eh := handlers.NewEventsHandler()
	{
		eventsAdmin := r.Group("", middleware.AdminAuth(db.DB))
		eventsAdmin.GET("/events", eh.List)
		eventsAdmin.GET("/events/:workspaceId", eh.ListByWorkspace)
	}

	// Remaining auth-gated workspace sub-routes — appended to wsAuth group declared above.
	{
		// Activity Logs
		acth := handlers.NewActivityHandler(broadcaster)
		wsAuth.GET("/activity", acth.List)
		wsAuth.GET("/session-search", acth.SessionSearch)
		wsAuth.POST("/activity", acth.Report)
		wsAuth.POST("/notify", acth.Notify)

		// Config
		cfgh := handlers.NewConfigHandler()
		wsAuth.GET("/config", cfgh.Get)
		wsAuth.PATCH("/config", cfgh.Patch)

		// Schedules (cron tasks)
		schedh := handlers.NewScheduleHandler()
		wsAuth.GET("/schedules", schedh.List)
		wsAuth.POST("/schedules", schedh.Create)
		wsAuth.PATCH("/schedules/:scheduleId", schedh.Update)
		wsAuth.DELETE("/schedules/:scheduleId", schedh.Delete)
		wsAuth.POST("/schedules/:scheduleId/run", schedh.RunNow)
		wsAuth.GET("/schedules/:scheduleId/history", schedh.History)
		// Schedule health — open to CanCommunicate peers (no workspace bearer token
		// required) so peer agents can detect silent cron failures without admin auth.
		// Auth is enforced inside the handler via X-Workspace-ID + CanCommunicate
		// (mirrors the /workspaces/:id/a2a pattern). Issue #249.
		r.GET("/workspaces/:id/schedules/health", schedh.Health)

		// Budget — per-workspace spend ceiling and current usage (#541).
		// GET stays on wsAuth — a workspace agent reading its own budget is legitimate.
		// PATCH is admin-only — workspace agents must not be able to self-clear their
		// spending ceiling (that would defeat the entire budget enforcement feature).
		budgeth := handlers.NewBudgetHandler()
		wsAuth.GET("/budget", budgeth.GetBudget)
		r.PATCH("/workspaces/:id/budget", middleware.AdminAuth(db.DB), budgeth.PatchBudget)

		// Token management (user-facing create/list/revoke)
		tokh := handlers.NewTokenHandler()
		wsAuth.GET("/tokens", tokh.List)
		wsAuth.POST("/tokens", tokh.Create)
		wsAuth.DELETE("/tokens/:tokenId", tokh.Revoke)

		// Memory
		memh := handlers.NewMemoryHandler()
		wsAuth.GET("/memory", memh.List)
		wsAuth.GET("/memory/:key", memh.Get)
		wsAuth.POST("/memory", memh.Set)
		wsAuth.DELETE("/memory/:key", memh.Delete)

		// Secrets (auto-restart workspace after secret change)
		sech := handlers.NewSecretsHandler(wh.RestartByID)
		wsAuth.GET("/secrets", sech.List)
		// Phase 30.2 — decrypted values pull, token-gated. Canvas uses List
		// (keys + metadata only); remote agents use Values to bootstrap env.
		wsAuth.GET("/secrets/values", sech.Values)
		wsAuth.POST("/secrets", sech.Set)
		wsAuth.PUT("/secrets", sech.Set)
		wsAuth.DELETE("/secrets/:key", sech.Delete)
		wsAuth.GET("/model", sech.GetModel)
		wsAuth.PUT("/model", sech.SetModel)
		wsAuth.GET("/provider", sech.GetProvider)
		wsAuth.PUT("/provider", sech.SetProvider)

		// Token usage metrics — cost transparency (#593).
		// WorkspaceAuth middleware (on wsAuth) binds the bearer to :id.
		mtrh := handlers.NewMetricsHandler()
		wsAuth.GET("/metrics", mtrh.GetMetrics)

		// Cloudflare Artifacts demo integration (#595).
		// All four routes require workspace-scoped bearer auth (wsAuth).
		// CF credentials read from CF_ARTIFACTS_API_TOKEN / CF_ARTIFACTS_NAMESPACE;
		// missing credentials return 503 so the handler still registers in
		// every deployment — the demo is gated on env vars, not compilation.
		arth := handlers.NewArtifactsHandler()
		wsAuth.POST("/artifacts", arth.Create)
		wsAuth.GET("/artifacts", arth.Get)
		wsAuth.POST("/artifacts/fork", arth.Fork)
		wsAuth.POST("/artifacts/token", arth.Token)

		// Temporal workflow checkpoints — step-level persistence for resumable
		// workflows (#788, #837, parent #583). WorkspaceAuth on wsAuth ensures each
		// workspace can only read/write its own checkpoints.
		// NOTE: /checkpoints/latest must be registered BEFORE /checkpoints/:wfid
		// so Gin's static-segment resolution takes precedence over the wildcard.
		cpth := handlers.NewCheckpointsHandler(db.DB)
		wsAuth.POST("/checkpoints", cpth.Upsert)
		wsAuth.GET("/checkpoints/latest", cpth.Latest)
		wsAuth.GET("/checkpoints/:wfid", cpth.List)
		wsAuth.DELETE("/checkpoints/:wfid", cpth.Delete)

		// MCP bridge — opencode / Claude Code integration (#800).
		// Exposes A2A delegation, peer discovery, and workspace operations as a
		// remote MCP server over HTTP (Streamable HTTP + SSE transports).
		//
		// Security:
		//   C1: WorkspaceAuth on wsAuth validates bearer token before any MCP logic.
		//   C2: MCPRateLimiter caps tool calls at 120/min/token so a long-lived
		//       opencode session cannot saturate the platform.
		//   C3: commit_memory/recall_memory with scope=GLOBAL → permission error;
		//       send_message_to_user excluded unless MOLECULE_MCP_ALLOW_SEND_MESSAGE=true.
		mcpH := handlers.NewMCPHandler(db.DB, broadcaster)
		if memBundle != nil {
			mcpH.WithMemoryV2(memBundle.Plugin, memBundle.Resolver)
		}
		mcpRl := middleware.NewMCPRateLimiter(120, time.Minute, context.Background())
		wsAuth.GET("/mcp/stream", mcpRl.Middleware(), mcpH.Stream)
		wsAuth.POST("/mcp", mcpRl.Middleware(), mcpH.Call)
	}

	// Global secrets — /settings/secrets is the canonical path; /admin/secrets kept for backward compat.
	// Fix (Cycle 7): protected by AdminAuth — any valid workspace bearer token grants access.
	// Fail-open when no tokens exist (fresh install / pre-Phase-30 upgrade).
	{
		adminAuth := r.Group("", middleware.AdminAuth(db.DB))
		sechGlobal := handlers.NewSecretsHandler(wh.RestartByID)
		adminAuth.GET("/settings/secrets", sechGlobal.ListGlobal)
		adminAuth.PUT("/settings/secrets", sechGlobal.SetGlobal)
		adminAuth.POST("/settings/secrets", sechGlobal.SetGlobal)
		adminAuth.DELETE("/settings/secrets/:key", sechGlobal.DeleteGlobal)
		adminAuth.GET("/admin/secrets", sechGlobal.ListGlobal)
		adminAuth.POST("/admin/secrets", sechGlobal.SetGlobal)
		adminAuth.DELETE("/admin/secrets/:key", sechGlobal.DeleteGlobal)
	}

	// Platform instructions — configurable rules with global/workspace scope.
	// Admin endpoints for CRUD; workspace-facing resolve endpoint for agent bootstrap.
	// (Team scope is reserved in the schema but not yet wired — needs teams/team_members
	// migration first.)
	{
		instrH := handlers.NewInstructionsHandler()
		adminInstr := r.Group("", middleware.AdminAuth(db.DB))
		adminInstr.GET("/instructions", instrH.List)
		adminInstr.POST("/instructions", instrH.Create)
		adminInstr.PUT("/instructions/:id", instrH.Update)
		adminInstr.DELETE("/instructions/:id", instrH.Delete)
		// Resolve mounted under wsAuth — caller must hold a valid bearer token
		// for :id, preventing cross-workspace enumeration of operator policy.
		wsAuth.GET("/instructions/resolve", instrH.Resolve)
	}

	// Admin — cross-workspace schedule health monitoring (issue #618).
	// Lets cron-audit agents and operators detect silent schedule failures
	// across all workspaces without holding individual workspace bearer tokens.
	// AdminAuth mirrors the /admin/liveness gate — fail-open on fresh install,
	// strict bearer-only once any token exists.
	{
		asHealth := handlers.NewAdminSchedulesHealthHandler()
		r.GET("/admin/schedules/health", middleware.AdminAuth(db.DB), asHealth.Health)
	}

	// Admin — stale a2a_queue cleanup (issue #1947). Marks queued items older
	// than max_age_minutes as 'dropped' so PM agents stop processing post-incident
	// noise. POST to avoid accidental GET-triggered side-effects; scoped to one
	// workspace_id or all workspaces if omitted.
	{
		qH := handlers.NewAdminQueueHandler()
		r.POST("/admin/a2a-queue/drop-stale", middleware.AdminAuth(db.DB), qH.DropStale)
	}

	// Admin — RFC #2829 PR-4 dashboard endpoints over the durable
	// `delegations` ledger (PR-1 schema). Operators triage in-flight,
	// stuck, or failed delegations without direct DB access.
	{
		adH := handlers.NewAdminDelegationsHandler(db.DB)
		r.GET("/admin/delegations", middleware.AdminAuth(db.DB), adH.List)
		r.GET("/admin/delegations/stats", middleware.AdminAuth(db.DB), adH.Stats)
	}

	// Admin — workspace template image refresh. Pulls latest images from GHCR
	// and recreates running ws-* containers so they adopt the new image.
	// Final step of the runtime CD chain — see docs/workspace-runtime-package.md.
	// Operators (or post-publish automation) hit this after a runtime release.
	// Reuses the provisioner's Docker client; no-op when prov is nil
	// (test / non-Docker deploy).
	if prov != nil {
		imgH := handlers.NewAdminWorkspaceImagesHandler(prov.DockerClient())
		r.POST("/admin/workspace-images/refresh", middleware.AdminAuth(db.DB), imgH.Refresh)
	}

	// Admin — test token minting (issue #6). Hidden in production via TestTokensEnabled().
	// NOT behind AdminAuth — this is the bootstrap endpoint E2E tests and
	// fresh installs use to obtain their first admin bearer. Adding AdminAuth
	// (#612) broke the chicken-and-egg: after first workspace provision creates
	// a live token in the DB, AdminAuth requires auth for ALL requests, but the
	// client has no token yet because it needs this endpoint to get one.
	// The handler itself rejects calls when MOLECULE_ENV=prod (TestTokensEnabled).
	{
		tokh := handlers.NewAdminTestTokenHandler()
		r.GET("/admin/workspaces/:id/test-token", tokh.GetTestToken)
	}

	// Admin — GitHub App installation token refresh (issue #547).
	// Long-running workspaces (>60 min) use this endpoint to refresh
	// GH_TOKEN without restarting. Returns the current installation token
	// from the github-app-auth plugin's in-process cache (which proactively
	// refreshes 5 min before expiry). 404 when no GitHub App is configured
	// (dev / self-hosted without GITHUB_APP_ID).
	{
		ghTokH := handlers.NewGitHubTokenHandler(wh.TokenRegistry())
		// #1068: moved from AdminAuth to allow any authenticated workspace to
		// refresh its GitHub token. The credential helper in containers calls
		// this endpoint with a workspace bearer token — AdminAuth (PR #729)
		// rejects those, breaking token refresh after 60 min.
		// Keep the old path as an alias for backward compat.
		r.GET("/admin/github-installation-token", middleware.AdminAuth(db.DB), ghTokH.GetInstallationToken)
		wsAuth.GET("/github-installation-token", ghTokH.GetInstallationToken)
	}

	// Terminal — shares Docker client with provisioner
	var dockerCli *client.Client
	if prov != nil {
		dockerCli = prov.DockerClient()
	}
	th := handlers.NewTerminalHandler(dockerCli)
	wsAuth.GET("/terminal", th.HandleConnect)
	wsAuth.GET("/terminal/diagnose", th.HandleDiagnose)

	// Canvas Viewport — #166 + #168: GET stays fully open for bootstrap.
	// PUT uses CanvasOrBearer (accepts Origin-match OR bearer token) so the
	// browser canvas can persist drag/zoom state without a bearer, while
	// bearer-carrying clients (molecli, integration tests) still work.
	// Viewport corruption is cosmetic-only — worst case a user refreshes
	// the page — so the softer check is acceptable here. This middleware
	// MUST NOT be used on routes that leak prompts, create workspaces,
	// or write files (#164/#165/#190 class).
	vh := handlers.NewViewportHandler()
	r.GET("/canvas/viewport", vh.Get)
	r.PUT("/canvas/viewport", middleware.CanvasOrBearer(db.DB), vh.Save)

	// Templates — wh threaded so generateDefaultConfig picks the
	// SaaS-aware default tier in Import + ReplaceFiles (#2910 PR-B).
	tmplh := handlers.NewTemplatesHandler(configsDir, dockerCli, wh)
	// #686: GET /templates lists all template names+metadata from configsDir.
	// Open access lets unauthenticated callers enumerate org configurations and
	// installed plugins. AdminAuth-gate it alongside POST /templates/import.
	// #190: POST /templates/import writes arbitrary files into configsDir.
	// Must be admin-gated — same class as /bundles/import (#164) and /org/import.
	{
		tmplAdmin := r.Group("", middleware.AdminAuth(db.DB))
		tmplAdmin.GET("/templates", tmplh.List)
		tmplAdmin.POST("/templates/import", tmplh.Import)
	}
	wsAuth.PUT("/files", tmplh.ReplaceFiles)
	wsAuth.GET("/files", tmplh.ListFiles)
	wsAuth.GET("/files/*path", tmplh.ReadFile)
	wsAuth.PUT("/files/*path", tmplh.WriteFile)
	wsAuth.DELETE("/files/*path", tmplh.DeleteFile)

	// Chat attachments — file upload (user → agent) and binary-safe
	// streaming download (agent → user). Namespaced under /chat/ so
	// the security model is obviously distinct from /files/* (which
	// handles workspace config/templates and has a different caller).
	chatfh := handlers.NewChatFilesHandler(tmplh).
		WithPendingUploads(pendinguploads.NewPostgres(db.DB), broadcaster)
	wsAuth.POST("/chat/uploads", chatfh.Upload)
	wsAuth.GET("/chat/download", chatfh.Download)

	// Phase 1 RFC: poll-mode chat upload — endpoints the workspace's
	// inbox poller hits to fetch staged file content + ack delivery.
	// Same wsAuth gate as the activity poll, so a token leak from
	// workspace A can't read workspace B's pending uploads (the
	// handler also re-checks workspace_id on each row).
	puh := handlers.NewPendingUploadsHandler(pendinguploads.NewPostgres(db.DB))
	wsAuth.GET("/pending-uploads/:file_id/content", puh.GetContent)
	wsAuth.POST("/pending-uploads/:file_id/ack", puh.Ack)

	// Plugins
	pluginsDir := findPluginsDir(configsDir)
	// Runtime lookup lets the plugins handler filter the registry to plugins
	// that declare support for the workspace's runtime, without taking a
	// direct DB dependency in the handler package.
	runtimeLookup := func(workspaceID string) (string, error) {
		var runtime string
		err := db.DB.QueryRowContext(
			context.Background(),
			`SELECT COALESCE(runtime, 'langgraph') FROM workspaces WHERE id = $1`,
			workspaceID,
		).Scan(&runtime)
		return runtime, err
	}
	plgh := handlers.NewPluginsHandler(pluginsDir, dockerCli, wh.RestartByID).
		WithRuntimeLookup(runtimeLookup)
	r.GET("/plugins", plgh.ListRegistry)
	r.GET("/plugins/sources", plgh.ListSources)
	wsAuth.GET("/plugins", plgh.ListInstalled)
	wsAuth.GET("/plugins/available", plgh.ListAvailableForWorkspace)
	wsAuth.GET("/plugins/compatibility", plgh.CheckRuntimeCompatibility)
	wsAuth.POST("/plugins", plgh.Install)
	wsAuth.DELETE("/plugins/:name", plgh.Uninstall)
	// Phase 30.3 — stream plugin as tar.gz so remote agents can pull +
	// unpack locally instead of going through Docker exec.
	wsAuth.GET("/plugins/:name/download", plgh.Download)

	// Bundles — #164 + #165: both gated behind AdminAuth.
	//   POST /bundles/import — CRITICAL: anon creation of arbitrary workspaces
	//                          with user-supplied config (system prompts,
	//                          plugins, secrets envelope). #164.
	//   GET /bundles/export/:id — HIGH: full system prompts + memory for any
	//                             workspace by UUID probe. #165.
	bh := handlers.NewBundleHandler(broadcaster, prov, platformURL, configsDir, dockerCli)
	{
		bundleAdmin := r.Group("", middleware.AdminAuth(db.DB))
		bundleAdmin.GET("/bundles/export/:id", bh.Export)
		bundleAdmin.POST("/bundles/import", bh.Import)
	}

	// Org Templates
	orgDir := findOrgDir(configsDir)
	orgh := handlers.NewOrgHandler(wh, broadcaster, prov, channelMgr, configsDir, orgDir)
	// #686: GET /org/templates exposes the org template catalogue (names, roles,
	// configured system prompts). AdminAuth-gate to match /org/import.
	r.GET("/org/templates", middleware.AdminAuth(db.DB), orgh.ListTemplates)

	// Organization-scoped API tokens — user-facing replacement for
	// ADMIN_TOKEN. Same AdminAuth gate: you need ADMIN_TOKEN, a
	// session cookie, OR an existing org token to mint more. That's
	// bootstrap-friendly (first token from ADMIN_TOKEN or canvas
	// session) and self-sustaining afterwards (tokens mint tokens).
	//
	// The mint endpoint gets an extra per-IP rate limiter — a
	// compromised session or leaked bearer could otherwise mint
	// thousands of tokens per second, making forensic cleanup
	// painful. 10 mints per hour per IP is ample for real usage;
	// legitimate bursts fit in the ceiling and abuse bounces off.
	// List + Delete don't need the extra limit (they can't be used
	// to generate new secret material).
	{
		orgTokenHandler := handlers.NewOrgTokenHandler()
		orgTokenAdmin := r.Group("", middleware.AdminAuth(db.DB))
		orgTokenAdmin.GET("/org/tokens", orgTokenHandler.List)
		orgTokenMintLimiter := middleware.NewRateLimiter(10, time.Hour, context.Background())
		orgTokenAdmin.POST("/org/tokens", orgTokenMintLimiter.Middleware(), orgTokenHandler.Create)
		orgTokenAdmin.DELETE("/org/tokens/:id", orgTokenHandler.Revoke)
	}

	// /org/import can create arbitrary workspaces from an uploaded YAML — it
	// must be an admin-gated route. The handler also path-sanitizes
	// `dir`/`template`/`files_dir` via resolveInsideRoot, but defence-in-
	// depth keeps the route behind AdminAuth regardless.
	r.POST("/org/import", middleware.AdminAuth(db.DB), orgh.Import)

	// Org plugin allowlist — tool governance (#591).
	// Both endpoints are admin-gated: reading the allowlist reveals approved
	// tooling policy; writing it enforces org-level install governance.
	{
		allowlistAdmin := r.Group("", middleware.AdminAuth(db.DB))
		aplh := handlers.NewOrgPluginAllowlistHandler()
		allowlistAdmin.GET("/orgs/:id/plugins/allowlist", aplh.GetAllowlist)
		allowlistAdmin.PUT("/orgs/:id/plugins/allowlist", aplh.PutAllowlist)
	}

	// Channels (social integrations — Telegram, Slack, Discord, etc.)
	chh := handlers.NewChannelHandler(channelMgr)
	r.GET("/channels/adapters", chh.ListAdapters)
	wsAuth.GET("/channels", chh.List)
	wsAuth.POST("/channels", chh.Create)
	wsAuth.PATCH("/channels/:channelId", chh.Update)
	wsAuth.DELETE("/channels/:channelId", chh.Delete)
	wsAuth.POST("/channels/:channelId/send", chh.Send)
	wsAuth.POST("/channels/:channelId/test", chh.Test)
	// #250: /channels/discover is an admin-setup helper (takes a bot
	// token, asks the vendor "what chats is this token a member of?").
	// Leaving it unauthenticated turned it into a bot-token oracle plus
	// a drive-by deleteWebhook side effect against any valid token an
	// attacker could probe. AdminAuth matches the intent — it's a
	// platform-operator helper, not a per-workspace route.
	r.POST("/channels/discover", middleware.AdminAuth(db.DB), chh.Discover)
	r.POST("/webhooks/:type", chh.Webhook)

	// Audit — EU AI Act Annex III compliance endpoint (#594).
	// Returns append-only HMAC-chained agent event log with optional inline
	// chain verification when AUDIT_LEDGER_SALT is configured.
	audh := handlers.NewAuditHandler()
	wsAuth.GET("/audit", audh.Query)

	// SSE — AG-UI compatible event stream per workspace (#590).
	// WorkspaceAuth middleware (on wsAuth) binds the bearer token to :id.
	sseh := handlers.NewSSEHandler(broadcaster)
	wsAuth.GET("/events/stream", sseh.StreamEvents)

	// WebSocket
	sh := handlers.NewSocketHandler(hub)
	r.GET("/ws", sh.HandleConnect)

	// Control-plane reverse proxy — forwards /cp/* to the SaaS CP.
	// Canvas's browser bundle fetches /cp/auth/me, /cp/orgs, etc. on
	// SAME ORIGIN (the tenant's <slug>.moleculesai.app). Those paths
	// aren't mounted on the tenant platform; without this proxy they
	// 404 and login breaks. When CP_UPSTREAM_URL is empty (self-
	// hosted / local dev where no CP exists), we skip the mount so
	// Gin's default 404 surfaces cleanly instead of proxying to a
	// placeholder.
	//
	// Mounted via NoRoute-style group BEFORE the canvas NoRoute so
	// /cp/* wins over the UI fallback.
	if cpURL := os.Getenv("CP_UPSTREAM_URL"); cpURL != "" {
		cpProxy := newCPProxy(cpURL)
		r.Any("/cp/*path", cpProxy)
	}

	// Canvas reverse proxy — when running as a combined tenant image
	// (Dockerfile.tenant), the Next.js canvas server runs on :3000 inside
	// the same container. Any route not matched by the API handlers above
	// gets proxied to the canvas so the browser only ever talks to :8080.
	//
	// When CANVAS_PROXY_URL is empty (self-hosted / local dev), this is a
	// no-op and Gin returns its default 404. The canvas dev server runs
	// separately on :3000 in that setup.
	if canvasURL := os.Getenv("CANVAS_PROXY_URL"); canvasURL != "" {
		canvasProxy := newCanvasProxy(canvasURL)
		r.NoRoute(canvasProxy)
	}

	return r
}

func findPluginsDir(configsDir string) string {
	// configsDir-relative is most reliable; plugins live at repo-root plugins/
	candidates := []string{
		filepath.Join(configsDir, "..", "plugins"),
		"../plugins",
		"plugins",
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			// Must have at least one plugin subfolder to be valid
			entries, _ := os.ReadDir(c)
			for _, e := range entries {
				if e.IsDir() {
					abs, _ := filepath.Abs(c)
					return abs
				}
			}
		}
	}
	abs, _ := filepath.Abs(filepath.Join(configsDir, "..", "plugins"))
	return abs
}

func findOrgDir(configsDir string) string {
	candidates := []string{
		"org-templates",
		"../org-templates",
		filepath.Join(configsDir, "..", "org-templates"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return "org-templates"
}
