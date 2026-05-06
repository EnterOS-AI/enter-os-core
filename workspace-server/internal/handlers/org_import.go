package handlers

// org_import.go — workspace tree creation during org template import.
// Contains createWorkspaceTree (recursive provisioning) and countWorkspaces.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/channels"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/crypto"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provlog"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/scheduler"
	"github.com/google/uuid"
)

// createWorkspaceTree recursively materialises an OrgWorkspace (and its
// descendants) into the workspaces + canvas_layouts tables and kicks off
// Docker provisioning. absX/absY are THIS workspace's absolute canvas
// coordinates — roots inherit them from ws.Canvas, children receive
// parent.abs + childSlotInGrid(index, siblingSizes) computed by the
// caller. Storing already-absolute coords means a child that is itself
// a parent can simply compound the grid without any per-call math.
// relX / relY are THIS workspace's position RELATIVE to its parent's
// absolute origin (i.e. childSlotInGrid output for children; 0,0 for
// roots since a root's absolute IS its relative). The broadcast
// payload ships relative coords so the canvas can drop the node
// straight into the parent's child-coordinate space without doing a
// canvas-wide absolute-position walk.
func (h *OrgHandler) createWorkspaceTree(ws OrgWorkspace, parentID *string, absX, absY, relX, relY float64, defaults OrgDefaults, orgBaseDir string, results *[]map[string]interface{}, provisionSem chan struct{}) error {
	// Apply defaults
	runtime := ws.Runtime
	if runtime == "" {
		runtime = defaults.Runtime
	}
	if runtime == "" {
		runtime = "langgraph"
	}
	model := ws.Model
	if model == "" {
		model = defaults.Model
	}
	if model == "" {
		// SSOT: per-runtime defaults live in models/runtime_defaults.go
		// (see RFC #2873). Consolidated from a duplicate of the same
		// branch in workspace_provision.go.
		model = models.DefaultModel(runtime)
	}
	tier := ws.Tier
	if tier == 0 {
		tier = defaults.Tier
	}
	if tier == 0 {
		// Resolved via the same DefaultTier helper Create + Templates
		// use (#2910 PR-E). SaaS → T4 (one container per sibling EC2,
		// no neighbour to protect from), self-hosted → T3. Pre-#2910
		// this path returned T2 on self-hosted, asymmetric with
		// workspace.go's T3 — undocumented drift. Lifting to
		// DefaultTier collapses both call sites onto one source of
		// truth so a future tier-default change sweeps every entry
		// point at once. Templates that want a different floor still
		// declare `tier:` in config.yaml or `defaults.tier` in
		// org.yaml.
		if h.workspace != nil {
			tier = h.workspace.DefaultTier()
		} else {
			tier = 3
		}
	}

	// 5s timeout bounds the lookup independently of any HTTP request
	// context. createWorkspaceTree runs in goroutines spawned from the
	// /org/import handler, so plumbing the request context here would
	// cascade-cancel into provisionWorkspaceAuto and abort in-flight
	// EC2 provisioning if the client disconnected mid-import — that's
	// the wrong behaviour. A short bounded timeout protects the
	// per-row SELECT against a wedged DB without taking the
	// drop-everything-on-disconnect tradeoff.
	ctxLookup, cancelLookup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelLookup()
	// Idempotency: if a workspace with the same (parent_id, name) already
	// exists, skip the INSERT + canvas_layouts + broadcast + provisioning.
	// This is what makes /org/import safe to call multiple times — the
	// historical leak was every call recreating the entire tree (see
	// tenant-hongming, 72 distinct child workspaces in 4 days, all from
	// repeated org-template spawns of the same template).
	//
	// Recursion still runs on the existing id so partial-match templates
	// (parent exists, some children missing) backfill the missing children
	// instead of either no-op'ing the whole subtree or duplicating the
	// existing children.
	//
	// /org/import is ADDITIVE-ONLY, never destructive. Children present
	// in the existing tree but absent from the new template are
	// preserved (no DELETE on diff). Skip-path also does NOT propagate
	// updates to existing nodes — a re-import that adds an
	// initial_memory or schedule to an existing workspace is silently
	// dropped (the function bypasses seedInitialMemories, schedule SQL,
	// channel config for skipped rows). To force-update an existing
	// tree, delete and re-import or use a future /org/sync route.
	existingID, existing, lookupErr := h.lookupExistingChild(ctxLookup, ws.Name, parentID)
	if lookupErr != nil {
		return fmt.Errorf("idempotency check for %s: %w", ws.Name, lookupErr)
	}
	if existing {
		log.Printf("Org import: %q already exists (id=%s) — skipping create+provision, recursing into children for partial-match", ws.Name, existingID)
		parentRef := ""
		if parentID != nil {
			parentRef = *parentID
		}
		provlog.Event("provision.skip_existing", map[string]any{
			"name":        ws.Name,
			"existing_id": existingID,
			"parent_id":   parentRef,
			"tier":        tier,
		})
		*results = append(*results, map[string]interface{}{
			"id":      existingID,
			"name":    ws.Name,
			"tier":    tier,
			"skipped": true,
		})
		return h.recurseChildrenForImport(ws, existingID, absX, absY, defaults, orgBaseDir, results, provisionSem)
	}

	id := uuid.New().String()
	awarenessNS := workspaceAwarenessNamespace(id)

	var role interface{}
	if ws.Role != "" {
		role = ws.Role
	}

	// Expand ${VAR} references in workspace_dir against the org's .env files
	// before validation. Without this, a template that ships
	// `workspace_dir: ${WORKSPACE_DIR}` (so each operator can pick the host
	// path to bind-mount) reaches validateWorkspaceDir as the literal
	// "${WORKSPACE_DIR}" string and fails with "must be an absolute path".
	// Other fields (channel config, prompts) already go through expandWithEnv;
	// workspace_dir was the last hold-out.
	if ws.WorkspaceDir != "" {
		ws.WorkspaceDir = expandWithEnv(ws.WorkspaceDir, loadWorkspaceEnv(orgBaseDir, ws.FilesDir))
	}

	// Validate and convert workspace_dir to NULL if empty
	var workspaceDir interface{}
	if ws.WorkspaceDir != "" {
		if err := validateWorkspaceDir(ws.WorkspaceDir); err != nil {
			return fmt.Errorf("workspace %s: %w", ws.Name, err)
		}
		workspaceDir = ws.WorkspaceDir
	}

	// #65: validate workspace_access (defaults to "none" when empty).
	workspaceAccess := ws.WorkspaceAccess
	if workspaceAccess == "" {
		workspaceAccess = provisioner.WorkspaceAccessNone
	}
	if err := provisioner.ValidateWorkspaceAccess(workspaceAccess, ws.WorkspaceDir); err != nil {
		return fmt.Errorf("workspace %s: %w", ws.Name, err)
	}

	ctx := context.Background()

	// Org-template imports default to expanded so children render
	// visually nested inside their parent — matches the user's mental
	// model ("all children should be in front of its parent"). The
	// topology rescue heuristic lays any children whose YAML coords
	// fall outside the computed parent bbox into a tidy 2-column grid
	// (see canvas-topology.ts), so imports don't spray the viewport.
	initialCollapsed := false

	maxConcurrent := ws.MaxConcurrentTasks
	if maxConcurrent <= 0 {
		maxConcurrent = models.DefaultMaxConcurrentTasks
	}
	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, role, tier, runtime, awareness_namespace, status, parent_id, workspace_dir, workspace_access, max_concurrent_tasks)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, id, ws.Name, role, tier, runtime, awarenessNS, "provisioning", parentID, workspaceDir, workspaceAccess, maxConcurrent)
	if err != nil {
		log.Printf("Org import: failed to create %s: %v", ws.Name, err)
		return fmt.Errorf("failed to create %s: %w", ws.Name, err)
	}

	// Canvas layout — absX/absY were computed by the caller using the
	// subtree-aware grid (childSlotInGrid) so a nested-parent child
	// doesn't clip into its siblings. Raw YAML canvas coords are only
	// consulted at the root: many templates predate the nested-parent
	// model and author them as a flat horizontal row (y=180, x=100..1220),
	// which overlaps chaotically once the cards render inside a parent
	// container.
	//
	// `collapsed` lives on canvas_layouts (005_canvas_layouts.sql), not
	// on workspaces; the UI-only flag is intentionally decoupled from
	// the workspace row.
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO canvas_layouts (workspace_id, x, y, collapsed) VALUES ($1, $2, $3, $4)`, id, absX, absY, initialCollapsed); err != nil {
		log.Printf("Org import: canvas layout insert failed for %s: %v", ws.Name, err)
	}

	// Broadcast — include runtime so the canvas pill renders the right
	// badge immediately instead of "unknown". parent_id + x/y let the
	// canvas's org-deploy animation spawn the child from the parent's
	// current coords and tween into its reserved slot, instead of
	// landing in a default grid position first and snapping on the
	// next hydrate.
	payload := map[string]interface{}{
		"name": ws.Name, "tier": tier, "runtime": runtime,
		// Parent-relative coords — the canvas's React Flow node uses
		// these as the node's position when parent_id is set (React
		// Flow treats node.position as parent-relative when the node
		// has a parentId). For roots, relX/relY == absX/absY.
		"x": relX, "y": relY,
	}
	if parentID != nil {
		payload["parent_id"] = *parentID
	}
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), id, payload)

	// Seed initial memories from workspace config or defaults (issue #1050).
	// Per-workspace initial_memories override defaults; if workspace has none,
	// fall back to defaults.initial_memories.
	wsMemories := ws.InitialMemories
	if len(wsMemories) == 0 {
		wsMemories = defaults.InitialMemories
	}
	seedInitialMemories(ctx, id, wsMemories, awarenessNS)

	// Handle external workspaces
	if ws.External {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, url = $2 WHERE id = $3`, models.StatusOnline, ws.URL, id); err != nil {
			log.Printf("Org import: external workspace status update failed for %s: %v", ws.Name, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOnline), id, map[string]interface{}{
			"name": ws.Name, "external": true,
		})
	} else if h.workspace.HasProvisioner() {
		// Provision container — either backend (CP for SaaS, local Docker
		// for self-hosted) is fine. Pre-2026-05-05 this gate was
		// `h.provisioner != nil`, which only checked the Docker pointer
		// and silently dropped every workspace on a SaaS tenant: the prep
		// block was skipped, no Auto call ever fired, and the row sat in
		// 'provisioning' until the 600s sweeper marked it failed with the
		// misleading "container started but never called /registry/register"
		// (incident: hongming tenant org-import 2026-05-05 01:14, 7-of-7
		// claude-code workspaces stuck). Routing to the right backend
		// happens inside provisionWorkspaceAuto — this gate just decides
		// whether to do prep at all.
		payload := models.CreateWorkspacePayload{
			Name: ws.Name, Tier: tier, Runtime: runtime, Model: model,
			WorkspaceDir:    ws.WorkspaceDir,
			WorkspaceAccess: workspaceAccess,
		}
		templatePath := ""
		if ws.Template != "" {
			// `template` comes from the uploaded YAML — treat as untrusted.
			// Only accept paths that stay inside h.configsDir.
			if tp, err := resolveInsideRoot(h.configsDir, ws.Template); err == nil {
				if _, statErr := os.Stat(tp); statErr == nil {
					templatePath = tp
				}
			}
		}
		if templatePath == "" {
			// #241: sanitizeRuntime() allowlists the runtime string so a
			// crafted org.yaml cannot use it as a path-traversal oracle.
			safeRuntime := sanitizeRuntime(runtime)
			runtimeDefault := filepath.Join(h.configsDir, safeRuntime+"-default")
			if _, err := os.Stat(runtimeDefault); err == nil {
				templatePath = runtimeDefault
			}
		}

		// Always generate default config.yaml (runtime, model, tier, etc.)
		configFiles := h.workspace.ensureDefaultConfig(id, payload)

		// Copy files_dir contents on top (system-prompt.md, CLAUDE.md, skills/, etc.)
		// Uses templatePath for CopyTemplateToContainer — runs AFTER configFiles are written
		if ws.FilesDir != "" && orgBaseDir != "" {
			// `files_dir` also comes from untrusted YAML. Join inside orgBaseDir
			// (already validated above) and reject anything that escapes.
			if filesPath, err := resolveInsideRoot(orgBaseDir, ws.FilesDir); err == nil {
				if info, statErr := os.Stat(filesPath); statErr == nil && info.IsDir() {
					templatePath = filesPath
				}
			}
		}

		// Pre-install plugins: copy from registry into configFiles as plugins/<name>/*.
		// Per-workspace plugins UNION with defaults.plugins (issue #68).
		// A leading "!" or "-" on a per-workspace entry opts that plugin out.
		plugins := mergePlugins(defaults.Plugins, ws.Plugins)
		if len(plugins) > 0 {
			if configFiles == nil {
				configFiles = map[string][]byte{}
			}
			pluginsBase, _ := filepath.Abs(filepath.Join(h.configsDir, "..", "plugins"))
			for _, pluginName := range plugins {
				pluginSrc := filepath.Join(pluginsBase, pluginName)
				if info, err := os.Stat(pluginSrc); err != nil || !info.IsDir() {
					log.Printf("Org import: plugin %s not found at %s, skipping", pluginName, pluginSrc)
					continue
				}
				filepath.Walk(pluginSrc, func(path string, info os.FileInfo, err error) error {
					if err != nil || info.IsDir() {
						return nil
					}
					rel, _ := filepath.Rel(pluginSrc, path)
					data, readErr := os.ReadFile(path)
					if readErr == nil {
						configFiles["plugins/"+pluginName+"/"+rel] = data
					}
					return nil
				})
			}
		}

		// Render category_routing into config.yaml so the agent can read its routing
		// table at runtime without hardcoded role names in prompts (issue #51).
		// Per-workspace keys replace defaults per-key (empty list drops the key);
		// see mergeCategoryRouting for exact semantics.
		routing := mergeCategoryRouting(defaults.CategoryRouting, ws.CategoryRouting)
		if len(routing) > 0 {
			if configFiles == nil {
				configFiles = map[string][]byte{}
			}
			block, err := renderCategoryRoutingYAML(routing)
			if err != nil {
				log.Printf("Org import: failed to render category_routing for %s: %v", ws.Name, err)
			} else {
				configFiles["config.yaml"] = appendYAMLBlock(configFiles["config.yaml"], block)
			}
		}

		// Resolve initial_prompt — inline wins, then file-ref, then defaults
		// (inline → file → defaults.inline → defaults.file). File refs are
		// rooted at <orgBaseDir>/<files_dir>/ per resolvePromptRef semantics.
		initialPrompt, err := resolvePromptRef(ws.InitialPrompt, ws.InitialPromptFile, orgBaseDir, ws.FilesDir)
		if err != nil {
			log.Printf("Org import: failed to resolve initial_prompt for %s: %v", ws.Name, err)
		}
		if initialPrompt == "" {
			// Fall back to defaults. Defaults live at the org root, so they
			// resolve with empty filesDir (relative to orgBaseDir).
			var defaultErr error
			initialPrompt, defaultErr = resolvePromptRef(defaults.InitialPrompt, defaults.InitialPromptFile, orgBaseDir, "")
			if defaultErr != nil {
				log.Printf("Org import: failed to resolve defaults.initial_prompt for %s: %v", ws.Name, defaultErr)
			}
		}
		if initialPrompt != "" {
			if configFiles == nil {
				configFiles = map[string][]byte{}
			}
			// Append initial_prompt to config.yaml using YAML block scalar.
			// Trim each line to avoid trailing whitespace issues.
			trimmed := strings.TrimSpace(initialPrompt)
			lines := strings.Split(trimmed, "\n")
			for i, line := range lines {
				lines[i] = strings.TrimRight(line, " \t")
			}
			indented := strings.Join(lines, "\n  ")
			configFiles["config.yaml"] = appendYAMLBlock(configFiles["config.yaml"], fmt.Sprintf("initial_prompt: |\n  %s\n", indented))
			log.Printf("Org import: injected initial_prompt (%d chars) into config.yaml for %s", len(trimmed), ws.Name)
		}

		// Resolve idle_prompt — same precedence (ws inline → ws file → defaults).
		// Inject into config.yaml alongside idle_interval_seconds so the
		// workspace's heartbeat loop picks up the idle-reflection cadence on
		// boot (see workspace/heartbeat.py + config.py).
		idlePrompt, err := resolvePromptRef(ws.IdlePrompt, ws.IdlePromptFile, orgBaseDir, ws.FilesDir)
		if err != nil {
			log.Printf("Org import: failed to resolve idle_prompt for %s: %v", ws.Name, err)
		}
		if idlePrompt == "" {
			var defaultErr error
			idlePrompt, defaultErr = resolvePromptRef(defaults.IdlePrompt, defaults.IdlePromptFile, orgBaseDir, "")
			if defaultErr != nil {
				log.Printf("Org import: failed to resolve defaults.idle_prompt for %s: %v", ws.Name, defaultErr)
			}
		}
		idleInterval := ws.IdleIntervalSeconds
		if idleInterval == 0 {
			idleInterval = defaults.IdleIntervalSeconds
		}
		if idlePrompt != "" {
			if configFiles == nil {
				configFiles = map[string][]byte{}
			}
			trimmed := strings.TrimSpace(idlePrompt)
			lines := strings.Split(trimmed, "\n")
			for i, line := range lines {
				lines[i] = strings.TrimRight(line, " \t")
			}
			indented := strings.Join(lines, "\n  ")
			// idle_interval_seconds belongs with idle_prompt — empty idle_prompt
			// means the idle loop never fires regardless of interval, so we
			// only emit interval when there's a body to go with it.
			if idleInterval <= 0 {
				idleInterval = 600 // same default as workspace/config.py
			}
			block := fmt.Sprintf("idle_interval_seconds: %d\nidle_prompt: |\n  %s\n", idleInterval, indented)
			configFiles["config.yaml"] = appendYAMLBlock(configFiles["config.yaml"], block)
			log.Printf("Org import: injected idle_prompt (%d chars, interval=%ds) into config.yaml for %s", len(trimmed), idleInterval, ws.Name)
		}

		// Inline system_prompt (only if no files_dir provides one)
		if ws.SystemPrompt != "" {
			if configFiles == nil {
				configFiles = map[string][]byte{}
			}
			configFiles["system-prompt.md"] = []byte(ws.SystemPrompt)
		}

		// Inject secrets from .env files as workspace secrets.
		// Resolution: workspace .env → org root .env (workspace overrides org root).
		// Each line: KEY=VALUE → stored as encrypted workspace secret.
		envVars := map[string]string{}
		if orgBaseDir != "" {
			// 1. Org root .env (shared defaults)
			parseEnvFile(filepath.Join(orgBaseDir, ".env"), envVars)
			// 2. Workspace-specific .env (overrides)
			if ws.FilesDir != "" {
				parseEnvFile(filepath.Join(orgBaseDir, ws.FilesDir, ".env"), envVars)
			}
		}
		// Store as workspace secrets via DB (encrypted if key is set, raw otherwise)
		for key, value := range envVars {
			var encrypted []byte
			if crypto.IsEnabled() {
				var err error
				encrypted, err = crypto.Encrypt([]byte(value))
				if err != nil {
					log.Printf("Org import: failed to encrypt secret %s for %s: %v", key, ws.Name, err)
					continue
				}
			} else {
				encrypted = []byte(value) // store raw when encryption disabled
			}
			if _, err := db.DB.ExecContext(ctx, `
				INSERT INTO workspace_secrets (workspace_id, key, encrypted_value)
				VALUES ($1, $2, $3)
				ON CONFLICT (workspace_id, key) DO UPDATE SET encrypted_value = $3, updated_at = now()
			`, id, key, encrypted); err != nil {
				log.Printf("Org import: failed to insert secret %s for %s: %v", key, ws.Name, err)
			}
		}

		// #1084: limit concurrent provisioning via semaphore.
		// Use provisionWorkspaceAuto so SaaS deployments route through
		// the CP (EC2) path — calling provisionWorkspace directly was
		// the same silent-drop bug that bit TeamHandler.Expand on
		// 2026-05-04 (see workspace.go:121-125 comment + #2486). Symptom:
		// every claude-code workspace from org-import on SaaS sat in
		// "provisioning" until the 600s sweeper marked it failed with
		// "container started but never called /registry/register" —
		// because there was no container, just a workspace row.
		// provisionWorkspaceAuto picks CP-mode when h.cpProv is wired,
		// Docker-mode otherwise; the org-import call site doesn't need
		// to know which.
		provisionSem <- struct{}{} // acquire
		go func(wID, tPath string, cFiles map[string][]byte, p models.CreateWorkspacePayload) {
			defer func() { <-provisionSem }() // release
			h.workspace.provisionWorkspaceAuto(wID, tPath, cFiles, p)
		}(id, templatePath, configFiles, payload)
	}

	// Insert schedules if defined. Resolve each schedule's prompt body from
	// either inline `prompt:` or `prompt_file:` (file ref relative to the
	// workspace's files_dir). Inline wins; empty prompt after resolution is
	// a configuration error (cron with no body would never do anything).
	for _, sched := range ws.Schedules {
		tz := sched.Timezone
		if tz == "" {
			tz = "UTC"
		}
		enabled := true
		if sched.Enabled != nil {
			enabled = *sched.Enabled
		}
		prompt, promptErr := resolvePromptRef(sched.Prompt, sched.PromptFile, orgBaseDir, ws.FilesDir)
		if promptErr != nil {
			log.Printf("Org import: failed to resolve prompt for schedule '%s' on %s: %v — skipping insert", sched.Name, ws.Name, promptErr)
			continue
		}
		if prompt == "" {
			log.Printf("Org import: schedule '%s' on %s has empty prompt (neither prompt nor prompt_file set) — skipping insert", sched.Name, ws.Name)
			continue
		}
		// #722: surface the error rather than silently using time.Time{} (zero)
		// which lib/pq stores as 0001-01-01 and may confuse the fire query.
		nextRun, nextRunErr := scheduler.ComputeNextRun(sched.CronExpr, tz, time.Now())
		if nextRunErr != nil {
			log.Printf("Org import: invalid cron expression for schedule '%s' on %s: %v — skipping insert",
				sched.Name, ws.Name, nextRunErr)
			continue
		}
		if _, err := db.DB.ExecContext(context.Background(), orgImportScheduleSQL,
			id, sched.Name, sched.CronExpr, tz, prompt, enabled, nextRun); err != nil {
			log.Printf("Org import: failed to upsert schedule '%s' for %s: %v", sched.Name, ws.Name, err)
		} else {
			log.Printf("Org import: schedule '%s' (%s, %d chars) upserted for %s (source=template)", sched.Name, sched.CronExpr, len(prompt), ws.Name)
		}
	}

	// Insert channels if defined (Telegram, Slack, etc.). Config values
	// support ${VAR} expansion from .env files. The manager is reloaded
	// once at the end of org import (in Import), not per-workspace.
	channelEnv := loadWorkspaceEnv(orgBaseDir, ws.FilesDir)
	wsChannelsCreated := []string{}
	wsChannelsSkipped := []map[string]string{}
	// skipChannel records a skipped channel with consistent shape across all reasons.
	skipChannel := func(channelType, reason string) {
		wsChannelsSkipped = append(wsChannelsSkipped, map[string]string{
			"workspace": ws.Name,
			"type":      channelType, // empty string when type field was missing
			"reason":    reason,
		})
	}

	for _, ch := range ws.Channels {
		if ch.Type == "" {
			skipChannel("", "empty type")
			log.Printf("Org import: skipping channel with empty type for %s", ws.Name)
			continue
		}
		// Validate adapter exists upfront — fail fast instead of inserting orphan rows
		adapter, ok := channels.GetAdapter(ch.Type)
		if !ok {
			skipChannel(ch.Type, "unknown adapter")
			log.Printf("Org import: skipping %s channel for %s — no adapter registered", ch.Type, ws.Name)
			continue
		}

		expandedConfig := make(map[string]interface{}, len(ch.Config))
		missing := []string{}
		for k, v := range ch.Config {
			expanded := expandWithEnv(v, channelEnv)
			if hasUnresolvedVarRef(v, expanded) {
				missing = append(missing, v)
			}
			expandedConfig[k] = expanded
		}
		if len(missing) > 0 {
			skipChannel(ch.Type, fmt.Sprintf("missing env: %v", missing))
			log.Printf("Org import: skipping %s channel for %s — env vars not set: %v", ch.Type, ws.Name, missing)
			continue
		}

		// Adapter-level config validation
		if err := adapter.ValidateConfig(expandedConfig); err != nil {
			skipChannel(ch.Type, err.Error())
			log.Printf("Org import: skipping %s channel for %s — invalid config: %v", ch.Type, ws.Name, err)
			continue
		}

		configJSON, err := json.Marshal(expandedConfig)
		if err != nil {
			log.Printf("Org import: failed to marshal config for %s channel: %v", ch.Type, err)
			continue
		}
		allowedJSON, err := json.Marshal(ch.AllowedUsers)
		if err != nil {
			log.Printf("Org import: failed to marshal allowed_users for %s channel: %v", ch.Type, err)
			continue
		}
		enabled := true
		if ch.Enabled != nil {
			enabled = *ch.Enabled
		}
		// Idempotent insert — if same workspace+type already exists, update config
		if _, err := db.DB.ExecContext(context.Background(), `
			INSERT INTO workspace_channels (workspace_id, channel_type, channel_config, enabled, allowed_users)
			VALUES ($1, $2, $3::jsonb, $4, $5::jsonb)
			ON CONFLICT (workspace_id, channel_type) DO UPDATE
			SET channel_config = EXCLUDED.channel_config,
			    enabled = EXCLUDED.enabled,
			    allowed_users = EXCLUDED.allowed_users,
			    updated_at = now()
		`, id, ch.Type, string(configJSON), enabled, string(allowedJSON)); err != nil {
			log.Printf("Org import: failed to create %s channel for %s: %v", ch.Type, ws.Name, err)
		} else {
			wsChannelsCreated = append(wsChannelsCreated, ch.Type)
			log.Printf("Org import: %s channel created for %s", ch.Type, ws.Name)
		}
	}

	resultEntry := map[string]interface{}{
		"id":   id,
		"name": ws.Name,
		"tier": tier,
	}
	if len(wsChannelsCreated) > 0 {
		resultEntry["channels"] = wsChannelsCreated
	}
	if len(wsChannelsSkipped) > 0 {
		resultEntry["channels_skipped"] = wsChannelsSkipped
	}
	*results = append(*results, resultEntry)

	// Recurse into children — both create-path and skip-path use the
	// same helper so partial-match (parent exists, some children missing)
	// backfills correctly without duplicating the recursion logic.
	return h.recurseChildrenForImport(ws, id, absX, absY, defaults, orgBaseDir, results, provisionSem)
}

// lookupExistingChild returns the id of an existing workspace under
// (parent_id, name) if any, with idempotency-friendly semantics:
//   - parent_id IS NOT DISTINCT FROM matches NULL too (root workspaces)
//   - status='removed' rows are ignored — collapsed teams or deleted
//     workspaces shouldn't block a re-import
//
// On sql.ErrNoRows: returns ("", false, nil) — caller should INSERT.
// On a real DB error: returns ("", false, err) — caller propagates.
//
// errors.Is is wrap-safe — a future caller wrapping the error
// (database/sql can wrap driver errors with %w in some setups) would
// silently break a `err == sql.ErrNoRows` equality check, causing the
// no-rows path to fall through to the "real DB error" branch and
// abort the import. errors.Is unwraps.
func (h *OrgHandler) lookupExistingChild(ctx context.Context, name string, parentID *string) (string, bool, error) {
	var existingID string
	err := db.DB.QueryRowContext(ctx, `
		SELECT id FROM workspaces
		WHERE name = $1
		  AND parent_id IS NOT DISTINCT FROM $2
		  AND status != 'removed'
		LIMIT 1
	`, name, parentID).Scan(&existingID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return existingID, true, nil
}

// recurseChildrenForImport walks ws.Children once, computing each child's
// absolute + parent-relative canvas coordinates from the subtree-aware
// grid (so nested-parent children don't clip into leaf siblings) and
// dispatching createWorkspaceTree for each. Pacing prevents Docker
// container-spam thundering on the self-hosted backend; SaaS dispatches
// the EC2 provision in a goroutine so the main loop is not blocked.
func (h *OrgHandler) recurseChildrenForImport(ws OrgWorkspace, parentID string, absX, absY float64, defaults OrgDefaults, orgBaseDir string, results *[]map[string]interface{}, provisionSem chan struct{}) error {
	if len(ws.Children) == 0 {
		return nil
	}
	siblingSizes := make([]nodeSize, len(ws.Children))
	for i, c := range ws.Children {
		siblingSizes[i] = sizeOfSubtree(c)
	}
	for i, child := range ws.Children {
		slotX, slotY := childSlotInGrid(i, siblingSizes)
		childAbsX := absX + slotX
		childAbsY := absY + slotY
		// slotX/slotY are already parent-relative — that's
		// exactly what childSlotInGrid returns.
		if err := h.createWorkspaceTree(child, &parentID, childAbsX, childAbsY, slotX, slotY, defaults, orgBaseDir, results, provisionSem); err != nil {
			return err
		}
		time.Sleep(workspaceCreatePacingMs * time.Millisecond)
	}
	return nil
}

// envVarNamePattern guards template-supplied env var names against
// pathological inputs. A malicious template could ship
// required_env: ["'; DROP …"] or whitespace-only entries that would
// flow through collectOrgEnv → into the 412 response body and,
// worse, into the modal's PUT /settings/secrets input. Schema
// already has `key TEXT NOT NULL UNIQUE` and our queries are
// parameterised so SQL injection isn't the threat — the real risks
// are UI rendering weirdness (newlines, NUL bytes, zero-width chars)
// and downstream env-var semantics (POSIX requires uppercase +
// underscore + digit). A strict regex filters both classes of
// problem at a single choke point.
var envVarNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)

// sanitizeEnvMembers filters a requirement's member list through the
// name-validation regex, logging rejections. Returns the filtered
// list and a boolean indicating whether any valid members remain.
// Used so a group containing one valid + one bogus name is kept
// (valid member carries the group) rather than silently dropped.
func sanitizeEnvMembers(members []string, where string) ([]string, bool) {
	out := make([]string, 0, len(members))
	for _, k := range members {
		if !envVarNamePattern.MatchString(k) {
			if k != "" {
				log.Printf("collectOrgEnv: rejecting invalid env var name %q from %s (must match %s)", k, where, envVarNamePattern)
			}
			continue
		}
		out = append(out, k)
	}
	return out, len(out) > 0
}

// envRequirementKey canonicalises a requirement for dedup — sorted
// member list joined with NUL so `any_of: [A, B]` and `any_of: [B, A]`
// collapse to the same key. Single requirements are length-1 groups.
func envRequirementKey(members []string) string {
	cp := append([]string(nil), members...)
	sort.Strings(cp)
	return strings.Join(cp, "\x00")
}

// collectOrgEnv walks the whole template tree and returns the union of
// required_env and recommended_env declared anywhere — at the org
// level, on root workspaces, or on any nested child. Deduplicates by
// group membership (same set of members = same requirement) and
// sorts deterministically so the canvas sees a stable order.
//
// "Required wins" rules:
//
//   - A requirement that appears in BOTH required and recommended
//     (same members) surfaces only as required.
//   - A single-name requirement (e.g. "API_KEY") and a group that
//     contains that same name (e.g. {any_of: [API_KEY, OTHER]}) are
//     NOT deduplicated — they're semantically different (strict vs
//     satisfiable-by-alternative) and the stricter "single" one wins,
//     so the any-of group is dropped when its members overlap with a
//     strict requirement declared elsewhere.
//
// Invalid names fail envVarNamePattern; the filter is applied per
// group so a group with one bogus entry keeps the rest. A group
// whose ALL members are invalid is dropped entirely with a log.
func collectOrgEnv(tmpl *OrgTemplate) (required, recommended []EnvRequirement) {
	reqByKey := map[string]EnvRequirement{}
	recByKey := map[string]EnvRequirement{}
	// Names covered by strict (single) required entries. A group in
	// EITHER tier whose any-of contains ONE of these names is
	// dominated by the strict requirement and gets dropped on the
	// second pass.
	strictRequiredNames := map[string]struct{}{}

	accept := func(into map[string]EnvRequirement, src []EnvRequirement, where string, markStrict bool) {
		for _, req := range src {
			members, ok := sanitizeEnvMembers(req.Members(), where)
			if !ok {
				continue
			}
			key := envRequirementKey(members)
			if _, exists := into[key]; exists {
				continue
			}
			if req.Name != "" && len(members) == 1 {
				into[key] = EnvRequirement{Name: members[0]}
				if markStrict {
					strictRequiredNames[members[0]] = struct{}{}
				}
			} else {
				into[key] = EnvRequirement{AnyOf: members}
			}
		}
	}
	accept(reqByKey, tmpl.RequiredEnv, "template root", true)
	accept(recByKey, tmpl.RecommendedEnv, "template root", false)
	var walk func([]OrgWorkspace)
	walk = func(ws []OrgWorkspace) {
		for _, w := range ws {
			accept(reqByKey, w.RequiredEnv, "workspace "+w.Name, true)
			accept(recByKey, w.RecommendedEnv, "workspace "+w.Name, false)
			walk(w.Children)
		}
	}
	walk(tmpl.Workspaces)

	// Required wins across tiers: any requirement whose members
	// overlap with a strict required name gets dropped from
	// recommended. Keeps the canvas modal from showing the same
	// key in both sections.
	prune := func(from map[string]EnvRequirement) {
		for k, r := range from {
			for _, m := range r.Members() {
				if _, strict := strictRequiredNames[m]; strict {
					delete(from, k)
					break
				}
			}
		}
	}
	prune(recByKey)

	// Same-tier: a strict required X dominates any-of groups in
	// required that CONTAIN X (a group saying "any of X, Y" is
	// automatically satisfied when X is required anyway, so it's
	// redundant). Same logic applied to recommended.
	pruneSameTier := func(tier map[string]EnvRequirement) {
		strictInTier := map[string]struct{}{}
		for _, r := range tier {
			if r.Name != "" {
				strictInTier[r.Name] = struct{}{}
			}
		}
		for k, r := range tier {
			if len(r.AnyOf) == 0 {
				continue
			}
			for _, m := range r.AnyOf {
				if _, strict := strictInTier[m]; strict {
					delete(tier, k)
					break
				}
			}
		}
	}
	pruneSameTier(reqByKey)
	pruneSameTier(recByKey)

	required = flattenAndSortRequirements(reqByKey)
	recommended = flattenAndSortRequirements(recByKey)
	return required, recommended
}

func flattenAndSortRequirements(by map[string]EnvRequirement) []EnvRequirement {
	out := make([]EnvRequirement, 0, len(by))
	for _, r := range by {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		// Sort singles first by name; groups after, ordered by
		// joined-member string. Gives the canvas a deterministic
		// render order so the same template always produces the
		// same modal layout.
		iSingle := out[i].Name != ""
		jSingle := out[j].Name != ""
		if iSingle != jSingle {
			return iSingle
		}
		if iSingle {
			return out[i].Name < out[j].Name
		}
		return envRequirementKey(out[i].AnyOf) < envRequirementKey(out[j].AnyOf)
	})
	return out
}

// loadConfiguredGlobalSecretKeys returns the set of key names present
// in global_secrets WHERE the encrypted_value is non-empty. Filtering
// on the payload size catches the failure mode where a row was
// upserted with an empty value (historical rows predating the
// binding:"required" guard on SetGlobal, or a future direct SQL
// path that skips it) — the preflight would otherwise report the
// key as "configured" and the per-container preflight would still
// fail at start time, defeating the whole feature.
// The LIMIT is a sanity cap: at realistic tenant sizes (< 1k
// secrets) it's a no-op; at pathological sizes it stops one slow
// query from wedging org imports. A hit gets logged so operators
// can investigate.
const globalSecretsPreflightLimit = 10000

func loadConfiguredGlobalSecretKeys(ctx context.Context) (map[string]struct{}, error) {
	rows, err := db.DB.QueryContext(ctx,
		`SELECT key FROM global_secrets WHERE octet_length(encrypted_value) > 0 LIMIT $1`,
		globalSecretsPreflightLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var k string
		if scanErr := rows.Scan(&k); scanErr == nil && k != "" {
			out[k] = struct{}{}
		}
	}
	if len(out) == globalSecretsPreflightLimit {
		log.Printf("loadConfiguredGlobalSecretKeys: hit LIMIT %d — org-import preflight may be incomplete", globalSecretsPreflightLimit)
	}
	return out, rows.Err()
}

func countWorkspaces(workspaces []OrgWorkspace) int {
	count := len(workspaces)
	for _, ws := range workspaces {
		count += countWorkspaces(ws.Children)
	}
	return count
}
