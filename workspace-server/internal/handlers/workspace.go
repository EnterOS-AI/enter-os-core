package handlers

// workspace.go — WorkspaceHandler struct, constructor, Create, List, Get,
// and the shared scanWorkspaceRow helper. State/Update/Delete and validators
// live in workspace_crud.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/crypto"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
	"github.com/Molecule-AI/molecule-monorepo/platform/pkg/provisionhook"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type WorkspaceHandler struct {
	// broadcaster narrowed from `*events.Broadcaster` to the
	// events.EventEmitter interface (#1814) so tests can substitute a
	// capture-only stub without standing up the real Redis + WS-hub
	// topology. Production callers still pass *events.Broadcaster, which
	// satisfies the interface — see the compile-time assertion in
	// internal/events/broadcaster.go.
	broadcaster events.EventEmitter
	provisioner *provisioner.Provisioner
	// cpProv narrowed from `*provisioner.CPProvisioner` to the
	// provisioner.CPProvisionerAPI interface (#1814) so tests can
	// substitute a stub without standing up the real CP HTTP client +
	// auth chain. Production callers still pass *CPProvisioner via
	// SetCPProvisioner, which satisfies the interface — see the
	// compile-time assertion in internal/provisioner/cp_provisioner.go.
	cpProv      provisioner.CPProvisionerAPI
	platformURL string
	configsDir  string // path to workspace-configs-templates/ (for reading templates)
	// envMutators runs registered EnvMutator plugins right before
	// container Start, after built-in secret loads. Nil = no plugins
	// registered; Registry.Run handles a nil receiver as a no-op so the
	// hot path stays a single nil-pointer compare.
	envMutators *provisionhook.Registry
	// stopFnOverride is set exclusively in tests to intercept provisioner.Stop
	// calls made by HibernateWorkspace without requiring a running Docker daemon.
	// Always nil in production; the real provisioner path is used when nil.
	stopFnOverride func(ctx context.Context, workspaceID string)
	// provisionTimeouts caches per-runtime provision-timeout values from
	// template manifests (#2054 phase 2). Lazy-init on first scan; see
	// runtime_provision_timeouts.go for the loader contract.
	provisionTimeouts runtimeProvisionTimeoutsCache
}

func NewWorkspaceHandler(b events.EventEmitter, p *provisioner.Provisioner, platformURL, configsDir string) *WorkspaceHandler {
	return &WorkspaceHandler{
		broadcaster: b,
		provisioner: p,
		platformURL: platformURL,
		configsDir:  configsDir,
	}
}

// SetCPProvisioner wires the control plane provisioner for SaaS tenants.
// Auto-activated when MOLECULE_ORG_ID is set (no manual config needed).
//
// Parameter is the CPProvisionerAPI interface (#1814) — production passes
// the *CPProvisioner from NewCPProvisioner; tests pass a stub.
func (h *WorkspaceHandler) SetCPProvisioner(cp provisioner.CPProvisionerAPI) {
	h.cpProv = cp
}

// SetEnvMutators wires a provisionhook.Registry into the handler. Plugins
// living in separate repos register on the same Registry instance during
// boot (see cmd/server/main.go) and main.go calls this setter once before
// router.Setup. Re-callable for tests but not safe under concurrent
// provisions — only invoke during single-threaded init.
func (h *WorkspaceHandler) SetEnvMutators(r *provisionhook.Registry) {
	h.envMutators = r
}

// TokenRegistry returns the provisionhook.Registry so the router can
// wire the GET /admin/github-installation-token handler without coupling
// to WorkspaceHandler's internals. Returns nil when no plugin has been
// registered (dev / self-hosted deployments without a GitHub App).
func (h *WorkspaceHandler) TokenRegistry() *provisionhook.Registry {
	return h.envMutators
}

// Create handles POST /workspaces
func (h *WorkspaceHandler) Create(c *gin.Context) {
	var payload models.CreateWorkspacePayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace payload"})
		return
	}

	// #685/#688: validate field lengths and reject injection characters before
	// any DB or provisioner interaction.
	if err := validateWorkspaceFields(payload.Name, payload.Role, payload.Model, payload.Runtime); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace fields"})
		return
	}

	id := uuid.New().String()
	awarenessNamespace := workspaceAwarenessNamespace(id)
	if payload.Tier == 0 {
		// Default to T3 ("Privileged"). T3 gives agents a read_write
		// workspace mount + Docker daemon access — the level most
		// templates need to do real work. Lower tiers (T1 sandboxed,
		// T2 standard) stay available as explicit opt-ins for
		// low-trust agents. Matches the Canvas CreateWorkspaceDialog
		// default for self-hosted hosts (SaaS defaults to T4 via
		// CreateWorkspaceDialog because each SaaS workspace runs on
		// its own sibling EC2).
		payload.Tier = 3
	}

	// Detect runtime + default model from template config.yaml when the
	// caller omitted them. Must happen before DB insert so persisted
	// fields match the template's intent.
	//
	// Model default pre-fills the hermes-trap gap (PR #1714 + TemplatePalette
	// patch): any create path (canvas dialog, TemplatePalette, direct API)
	// that names a template but forgets a model slug now inherits the
	// template's `runtime_config.model` — without it, hermes-agent falls
	// back to its compiled-in Anthropic default and 401s when the user's
	// key is for a different provider. Non-hermes runtimes are unaffected
	// (the server still passes model through, they just don't use it).
	if payload.Template != "" && (payload.Runtime == "" || payload.Model == "") {
		// #226: payload.Template is attacker-controllable. resolveInsideRoot
		// rejects absolute paths and any ".." that escapes configsDir so the
		// provisioner can't be pointed at host directories.
		candidatePath, resolveErr := resolveInsideRoot(h.configsDir, payload.Template)
		if resolveErr != nil {
			log.Printf("Create: invalid template path %q: %v", payload.Template, resolveErr)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid template"})
			return
		}
		cfgData, readErr := os.ReadFile(filepath.Join(candidatePath, "config.yaml"))
		if readErr != nil {
			log.Printf("Create: could not read config.yaml for template %q: %v", payload.Template, readErr)
		}
		// Two-pass line scanner: the old parser found top-level `runtime:`
		// by substring match on trimmed lines. We extend it to also find
		// the nested `runtime_config.model:` (new format) and top-level
		// `model:` (legacy format). A minimal state var tracks whether
		// we're inside the runtime_config block based on indentation.
		inRuntimeConfig := false
		for _, rawLine := range strings.Split(string(cfgData), "\n") {
			// Track indentation to detect block transitions.
			trimmed := strings.TrimLeft(rawLine, " \t")
			indented := len(rawLine) > len(trimmed)
			if !indented {
				// Left the runtime_config block (or never entered it).
				inRuntimeConfig = strings.HasPrefix(trimmed, "runtime_config:")
			}
			stripped := strings.TrimSpace(rawLine)
			switch {
			case payload.Runtime == "" && !indented && strings.HasPrefix(stripped, "runtime:") && !strings.HasPrefix(stripped, "runtime_config"):
				payload.Runtime = strings.TrimSpace(strings.TrimPrefix(stripped, "runtime:"))
			case payload.Model == "" && !indented && strings.HasPrefix(stripped, "model:"):
				// Legacy top-level `model:` — pre-runtime_config templates.
				payload.Model = strings.Trim(strings.TrimSpace(strings.TrimPrefix(stripped, "model:")), `"'`)
			case payload.Model == "" && indented && inRuntimeConfig && strings.HasPrefix(stripped, "model:"):
				// Nested `runtime_config.model:` — current format (hermes etc.).
				payload.Model = strings.Trim(strings.TrimSpace(strings.TrimPrefix(stripped, "model:")), `"'`)
			}
			if payload.Runtime != "" && payload.Model != "" {
				break
			}
		}
	}
	if payload.Runtime == "" {
		payload.Runtime = "langgraph"
	}

	ctx := c.Request.Context()

	// Convert empty role to NULL
	var role interface{}
	if payload.Role != "" {
		role = payload.Role
	}

	// Validate and convert workspace_dir
	var workspaceDir interface{}
	if payload.WorkspaceDir != "" {
		if err := validateWorkspaceDir(payload.WorkspaceDir); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace directory"})
			return
		}
		workspaceDir = payload.WorkspaceDir
	}

	// #65: validate workspace_access, default to "none".
	workspaceAccess := payload.WorkspaceAccess
	if workspaceAccess == "" {
		workspaceAccess = provisioner.WorkspaceAccessNone
	}
	if err := provisioner.ValidateWorkspaceAccess(workspaceAccess, payload.WorkspaceDir); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace access"})
		return
	}

	// Begin a transaction so the workspace row and any initial secrets are
	// committed atomically.  A secret-encrypt or DB error rolls back the
	// workspace insert so we never leave a workspace row with missing secrets.
	tx, txErr := db.DB.BeginTx(ctx, nil)
	if txErr != nil {
		log.Printf("Create workspace: begin tx error: %v", txErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace"})
		return
	}

	maxConcurrent := payload.MaxConcurrentTasks
	if maxConcurrent <= 0 {
		maxConcurrent = models.DefaultMaxConcurrentTasks
	}
	// Insert workspace with runtime persisted in DB (inside transaction)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, role, tier, runtime, awareness_namespace, status, parent_id, workspace_dir, workspace_access, budget_limit, max_concurrent_tasks)
		VALUES ($1, $2, $3, $4, $5, $6, 'provisioning', $7, $8, $9, $10, $11)
	`, id, payload.Name, role, payload.Tier, payload.Runtime, awarenessNamespace, payload.ParentID, workspaceDir, workspaceAccess, payload.BudgetLimit, maxConcurrent)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		log.Printf("Create workspace error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace"})
		return
	}

	// Persist initial secrets from the create payload (inside same transaction).
	// nil/empty map is a no-op.  Any failure rolls back the workspace insert
	// so we never have a workspace row without its intended secrets.
	for k, v := range payload.Secrets {
		encrypted, encErr := crypto.Encrypt([]byte(v))
		if encErr != nil {
			tx.Rollback() //nolint:errcheck
			log.Printf("Create workspace %s: failed to encrypt secret %q: %v", id, k, encErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt secret: " + k})
			return
		}
		version := crypto.CurrentEncryptionVersion()
		if _, dbErr := tx.ExecContext(ctx, `
			INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (workspace_id, key) DO UPDATE
				SET encrypted_value = $3, encryption_version = $4, updated_at = now()
		`, id, k, encrypted, version); dbErr != nil {
			tx.Rollback() //nolint:errcheck
			log.Printf("Create workspace %s: failed to persist secret %q: %v", id, k, dbErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save secret: " + k})
			return
		}
	}

	if commitErr := tx.Commit(); commitErr != nil {
		log.Printf("Create workspace %s: transaction commit failed: %v", id, commitErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace"})
		return
	}

	// Insert canvas layout — non-fatal: workspace can be dragged into position later
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO canvas_layouts (workspace_id, x, y) VALUES ($1, $2, $3)
	`, id, payload.Canvas.X, payload.Canvas.Y); err != nil {
		log.Printf("Create: canvas layout insert failed for %s (workspace will appear at 0,0): %v", id, err)
	}

	// Seed initial memories from the create payload (issue #1050).
	// Non-fatal: failures are logged but don't block workspace creation.
	seedInitialMemories(ctx, id, payload.InitialMemories, awarenessNamespace)

	// Broadcast provisioning event. Include `runtime` so the canvas can
	// populate the Runtime pill on the side panel immediately — without it
	// the node lives as "runtime: unknown" until something refetches the
	// workspace row (which nothing does during provisioning).
	h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_PROVISIONING", id, map[string]interface{}{
		"name":    payload.Name,
		"tier":    payload.Tier,
		"runtime": payload.Runtime,
	})

	// External workspaces: no container provisioning. Two shapes:
	//   (a) URL supplied up-front  — the operator already has their
	//       agent running somewhere reachable; we mark it online
	//       immediately. Legacy flow, preserved for callers that
	//       don't need the copy-this-snippet UX (org-import, etc.).
	//   (b) URL omitted             — the operator will install
	//       molecule-sdk-python or another A2A server later. We
	//       mint a workspace_auth_token now and return it alongside
	//       workspace_id + platform_url so the canvas UI can show
	//       one copy-paste connection snippet. Status is set to
	//       "awaiting_agent" — distinct from "provisioning" (which
	//       implies docker work in flight) so the canvas can render
	//       a "waiting for external agent to connect" state without
	//       tripping the provisioning-timeout UX.
	if payload.External || payload.Runtime == "external" {
		var connectionToken string
		if payload.URL != "" {
			db.DB.ExecContext(ctx, `UPDATE workspaces SET url = $1, status = 'online', runtime = 'external', updated_at = now() WHERE id = $2`, payload.URL, id)
			if err := db.CacheURL(ctx, id, payload.URL); err != nil {
				log.Printf("External workspace: failed to cache URL for %s: %v", id, err)
			}
			h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_ONLINE", id, map[string]interface{}{
				"name": payload.Name, "external": true,
			})
		} else {
			// Pre-register flow: mint a token and park the workspace
			// in awaiting_agent. First POST /registry/register call
			// from the external agent (with this token + its URL)
			// flips the row to online.
			db.DB.ExecContext(ctx, `UPDATE workspaces SET status = 'awaiting_agent', runtime = 'external', updated_at = now() WHERE id = $1`, id)
			tok, tokErr := wsauth.IssueToken(ctx, db.DB, id)
			if tokErr != nil {
				log.Printf("External workspace %s: token issuance failed: %v", id, tokErr)
				// Non-fatal — the workspace row still exists; the
				// operator can call POST /workspaces/:id/tokens later
				// to mint one. Return a 201 with a hint instead of
				// 500'ing a partial-success write.
			} else {
				connectionToken = tok
			}
			h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_AWAITING_AGENT", id, map[string]interface{}{
				"name": payload.Name, "external": true,
			})
		}
		log.Printf("Created external workspace %s (%s) url=%q awaiting=%v",
			payload.Name, id, payload.URL, payload.URL == "")
		resp := gin.H{
			"id":       id,
			"external": true,
		}
		if payload.URL != "" {
			resp["status"] = "online"
		} else {
			resp["status"] = "awaiting_agent"
			// Connection snippet payload. Returned ONCE on create —
			// the token is not recoverable from any later read. UI
			// is responsible for surfacing this in a copy-paste modal.
			platformURL := strings.TrimSuffix(externalPlatformURL(c), "/")
			resp["connection"] = gin.H{
				"workspace_id": id,
				"platform_url": platformURL,
				"auth_token":   connectionToken, // may be "" if IssueToken failed above
				"registry_endpoint": platformURL + "/registry/register",
				"heartbeat_endpoint": platformURL + "/registry/heartbeat",
				// Pre-formatted snippet that a non-Go operator can
				// paste verbatim. curl-based so there's no SDK
				// install dependency. The external agent only
				// needs to replace $AGENT_URL with its own public URL.
				"curl_register_template": strings.ReplaceAll(
					strings.ReplaceAll(externalCurlTemplate,
						"{{PLATFORM_URL}}", platformURL),
					"{{WORKSPACE_ID}}", id,
				),
				// Python/SDK snippet. molecule-sdk-python PR #13
				// shipped A2AServer + RemoteAgentClient specifically
				// for this flow. The SDK is not yet on PyPI — the
				// snippet pins @main until we cut a release.
				"python_snippet": strings.ReplaceAll(
					strings.ReplaceAll(externalPythonTemplate,
						"{{PLATFORM_URL}}", platformURL),
					"{{WORKSPACE_ID}}", id,
				),
			}
		}
		c.JSON(http.StatusCreated, resp)
		return
	}

	// Resolve template config — needed for both Docker provisioning and
	// config-only persistence (tenant SaaS without Docker).
	var templatePath string
	var configFiles map[string][]byte
	if payload.Template != "" {
		candidatePath, resolveErr := resolveInsideRoot(h.configsDir, payload.Template)
		if resolveErr != nil {
			log.Printf("Create provision: rejecting template %q: %v", payload.Template, resolveErr)
			return
		}
		if _, err := os.Stat(candidatePath); err == nil {
			templatePath = candidatePath
		} else {
			log.Printf("Create: template %q not found, falling back for %s", payload.Template, payload.Name)
			safeRuntime := sanitizeRuntime(payload.Runtime)
			runtimeDefault := filepath.Join(h.configsDir, safeRuntime+"-default")
			if _, err := os.Stat(runtimeDefault); err == nil {
				templatePath = runtimeDefault
			} else {
				configFiles = h.ensureDefaultConfig(id, payload)
			}
		}
	} else {
		configFiles = h.ensureDefaultConfig(id, payload)
	}

	// Auto-provision — pick backend: control plane (SaaS) or Docker (self-hosted)
	if h.cpProv != nil {
		go h.provisionWorkspaceCP(id, templatePath, configFiles, payload)
	} else if h.provisioner != nil {
		go h.provisionWorkspace(id, templatePath, configFiles, payload)
	} else {
		// No Docker available (SaaS tenant). Persist basic config as JSON
		// so the Config tab shows the correct runtime/model/name. Then mark
		// the workspace as failed with a clear message.
		cfgJSON := fmt.Sprintf(`{"name":%q,"runtime":%q,"tier":%d,"template":%q}`,
			payload.Name, payload.Runtime, payload.Tier, payload.Template)
		db.DB.ExecContext(ctx, `
			INSERT INTO workspace_config (workspace_id, data) VALUES ($1, $2::jsonb)
			ON CONFLICT (workspace_id) DO UPDATE SET data = $2::jsonb
		`, id, cfgJSON)
		db.DB.ExecContext(ctx,
			`UPDATE workspaces SET status = 'failed', last_sample_error = 'Docker not available — workspace containers require a Docker daemon or external provisioning.', updated_at = now() WHERE id = $1`, id)
		h.broadcaster.RecordAndBroadcast(ctx, "WORKSPACE_PROVISION_FAILED", id, map[string]interface{}{
			"error": "Docker not available on this platform instance",
		})
		log.Printf("Create: no Docker daemon — workspace %s config persisted, marked failed", id)
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":                  id,
		"status":              "provisioning",
		"awareness_namespace": awarenessNamespace,
		"workspace_access":    workspaceAccess,
	})
}

// addProvisionTimeoutMs decorates a workspace response map with the
// per-runtime provision-timeout override (#2054 phase 2) when one is
// declared in the runtime's template manifest. No-op when the runtime
// has no declared timeout — the canvas-side resolver falls through to
// its runtime-profile default.
func (h *WorkspaceHandler) addProvisionTimeoutMs(ws map[string]interface{}, runtime string) {
	if secs := h.provisionTimeouts.get(h.configsDir, runtime); secs > 0 {
		ws["provision_timeout_ms"] = secs * 1000
	}
}

// scanWorkspaceRow is a helper to scan workspace+layout rows into a clean JSON map.
func scanWorkspaceRow(rows interface {
	Scan(dest ...interface{}) error
}) (map[string]interface{}, error) {
	var id, name, role, status, url, sampleError, currentTask, runtime, workspaceDir string
	var tier, activeTasks, maxConcurrentTasks, uptimeSeconds int
	var errorRate, x, y float64
	var collapsed bool
	var parentID *string
	var agentCard []byte
	var budgetLimit sql.NullInt64
	var monthlySpend int64

	err := rows.Scan(&id, &name, &role, &tier, &status, &agentCard, &url,
		&parentID, &activeTasks, &maxConcurrentTasks, &errorRate, &sampleError, &uptimeSeconds,
		&currentTask, &runtime, &workspaceDir, &x, &y, &collapsed,
		&budgetLimit, &monthlySpend)
	if err != nil {
		return nil, err
	}

	ws := map[string]interface{}{
		"id":                id,
		"name":              name,
		"tier":              tier,
		"status":            status,
		"url":               url,
		"parent_id":         parentID,
		"active_tasks":          activeTasks,
		"max_concurrent_tasks":  maxConcurrentTasks,
		"last_error_rate":       errorRate,
		"last_sample_error": sampleError,
		"uptime_seconds":    uptimeSeconds,
		"current_task":      currentTask,
		"runtime":           runtime,
		"workspace_dir":     nilIfEmpty(workspaceDir),
		"monthly_spend":     monthlySpend,
		"x":                 x,
		"y":                 y,
		"collapsed":         collapsed,
	}

	// budget_limit: nil when no limit set, int64 otherwise
	if budgetLimit.Valid {
		ws["budget_limit"] = budgetLimit.Int64
	} else {
		ws["budget_limit"] = nil
	}

	// Only include non-empty values
	if role != "" {
		ws["role"] = role
	} else {
		ws["role"] = nil
	}

	// Parse agent_card as raw JSON
	if len(agentCard) > 0 && string(agentCard) != "null" {
		ws["agent_card"] = json.RawMessage(agentCard)
	} else {
		ws["agent_card"] = nil
	}

	return ws, nil
}

const workspaceListQuery = `
	SELECT w.id, w.name, COALESCE(w.role, ''), w.tier, w.status,
		   COALESCE(w.agent_card, 'null'::jsonb), COALESCE(w.url, ''),
		   w.parent_id, w.active_tasks, COALESCE(w.max_concurrent_tasks, 1),
		   w.last_error_rate,
		   COALESCE(w.last_sample_error, ''), w.uptime_seconds,
		   COALESCE(w.current_task, ''), COALESCE(w.runtime, 'langgraph'),
		   COALESCE(w.workspace_dir, ''),
		   COALESCE(cl.x, 0), COALESCE(cl.y, 0), COALESCE(cl.collapsed, false),
		   w.budget_limit, COALESCE(w.monthly_spend, 0)
	FROM workspaces w
	LEFT JOIN canvas_layouts cl ON cl.workspace_id = w.id
	WHERE w.status != 'removed'
	ORDER BY w.created_at`

// List handles GET /workspaces
func (h *WorkspaceHandler) List(c *gin.Context) {
	rows, err := db.DB.QueryContext(c.Request.Context(), workspaceListQuery)
	if err != nil {
		log.Printf("List workspaces error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	workspaces := make([]map[string]interface{}, 0)
	for rows.Next() {
		ws, err := scanWorkspaceRow(rows)
		if err != nil {
			log.Printf("List scan error: %v", err)
			continue
		}
		// #2054 phase 2: surface per-runtime provision-timeout for
		// canvas's ProvisioningTimeout banner. Decorating per-row
		// (vs map-once-and-reuse) keeps the helper self-contained;
		// the cache hit is sub-microsecond.
		if rt, _ := ws["runtime"].(string); rt != "" {
			h.addProvisionTimeoutMs(ws, rt)
		}
		workspaces = append(workspaces, ws)
	}
	if err := rows.Err(); err != nil {
		log.Printf("List rows error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query iteration failed"})
		return
	}

	c.JSON(http.StatusOK, workspaces)
}

// Get handles GET /workspaces/:id
func (h *WorkspaceHandler) Get(c *gin.Context) {
	id := c.Param("id")

	// #687: reject non-UUID IDs before hitting the DB.
	if err := validateWorkspaceID(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}

	row := db.DB.QueryRowContext(c.Request.Context(), `
		SELECT w.id, w.name, COALESCE(w.role, ''), w.tier, w.status,
			   COALESCE(w.agent_card, 'null'::jsonb), COALESCE(w.url, ''),
			   w.parent_id, w.active_tasks, COALESCE(w.max_concurrent_tasks, 1),
			   w.last_error_rate,
			   COALESCE(w.last_sample_error, ''), w.uptime_seconds,
			   COALESCE(w.current_task, ''), COALESCE(w.runtime, 'langgraph'),
			   COALESCE(w.workspace_dir, ''),
			   COALESCE(cl.x, 0), COALESCE(cl.y, 0), COALESCE(cl.collapsed, false),
			   w.budget_limit, COALESCE(w.monthly_spend, 0)
		FROM workspaces w
		LEFT JOIN canvas_layouts cl ON cl.workspace_id = w.id
		WHERE w.id = $1
	`, id)

	ws, err := scanWorkspaceRow(row)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		log.Printf("Get workspace error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	// Strip sensitive fields — GET /workspaces/:id is on the open router.
	// Any caller with a valid UUID would otherwise read operational data.
	delete(ws, "budget_limit")
	delete(ws, "monthly_spend")
	delete(ws, "current_task")      // operational surveillance risk (#955)
	delete(ws, "last_sample_error") // internal error details
	delete(ws, "workspace_dir")     // host path disclosure

	// #817: expose last_outbound_at so orchestrators can detect silent
	// workspaces. Non-sensitive — just a timestamp of the most recent
	// outbound A2A. Null if the workspace has never sent anything.
	var lastOutbound sql.NullTime
	if err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT last_outbound_at FROM workspaces WHERE id = $1`, id,
	).Scan(&lastOutbound); err == nil && lastOutbound.Valid {
		ws["last_outbound_at"] = lastOutbound.Time
	} else {
		ws["last_outbound_at"] = nil
	}

	// #2054 phase 2: per-runtime provision-timeout for canvas's
	// ProvisioningTimeout banner.
	if rt, _ := ws["runtime"].(string); rt != "" {
		h.addProvisionTimeoutMs(ws, rt)
	}

	c.JSON(http.StatusOK, ws)
}
