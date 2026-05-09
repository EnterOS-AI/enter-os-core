package handlers

// org.go — core org handler: types, struct, ListTemplates, Import.
// Tree creation logic is in org_import.go; utility helpers in org_helpers.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/channels"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"gopkg.in/yaml.v3"
)

// OrgHandler manages org template import/export.
// workspaceCreatePacingMs is the brief delay between sibling workspace creations
// during org import. Prevents overwhelming Docker when creating many containers.
const workspaceCreatePacingMs = 2000

// defaultProvisionConcurrency is the fallback cap for parallel
// workspace-provision goroutines when MOLECULE_PROVISION_CONCURRENCY
// is unset. Originally a hard constant of 3 (PR #1084) calibrated for
// Docker-mode workspaces. The constant is now a default — operators
// running on EC2 (where each provision is a RunInstances call AWS
// happily parallelises) typically want a much higher cap, while
// Docker-mode dev environments still prefer the conservative 3.
//
// 3 keeps the existing Docker-mode behavior. SaaS deployments override
// via env (see resolveProvisionConcurrency below).
const defaultProvisionConcurrency = 3

// resolveProvisionConcurrency returns the effective semaphore size for
// org-import workspace provisioning, honoring MOLECULE_PROVISION_CONCURRENCY:
//
//   - unset / empty / non-numeric → defaultProvisionConcurrency (3)
//   - "0"                          → unlimited (a very large cap;
//                                    practically no semaphore — used on
//                                    SaaS where AWS RunInstances is the
//                                    rate-limiter, not us)
//   - any positive integer N       → N
//   - negative integer             → defaultProvisionConcurrency (3),
//                                    log warning so operator notices
//                                    the misconfiguration
//
// The "0 = unlimited" mapping was a deliberate choice: an env var of "0"
// is the natural shorthand for "no cap" without forcing operators to
// type a magic large number. The implementation hands off a large but
// finite value (1<<20) so the channel still works as a regular
// buffered chan; goroutines will never block on the semaphore in
// practice.
func resolveProvisionConcurrency() int {
	raw := strings.TrimSpace(os.Getenv("MOLECULE_PROVISION_CONCURRENCY"))
	if raw == "" {
		return defaultProvisionConcurrency
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("org_import: MOLECULE_PROVISION_CONCURRENCY=%q is not an integer; falling back to default %d",
			raw, defaultProvisionConcurrency)
		return defaultProvisionConcurrency
	}
	if n < 0 {
		log.Printf("org_import: MOLECULE_PROVISION_CONCURRENCY=%d is negative; falling back to default %d",
			n, defaultProvisionConcurrency)
		return defaultProvisionConcurrency
	}
	if n == 0 {
		// Unlimited semantics — use a large but finite cap so the
		// chan-based semaphore stays a no-op. 1M is well past any
		// realistic org-import size; AWS RunInstances rate-limit and
		// account vCPU quota are the real backpressure here.
		return 1 << 20
	}
	return n
}

// Child grid layout constants — kept in sync with canvas-topology.ts on
// the client. Children laid on import use the same 2-column grid so the
// nested view is clean out of the box. Before this, YAML-declared
// canvas coords (absolute, horizontally fanned at y=180) produced an
// overlapping mess under the nested render (see screenshot in PR
// #1981 thread).
const (
	childDefaultWidth    = 240.0
	childDefaultHeight   = 130.0
	childGutter          = 14.0
	parentHeaderPadding  = 130.0
	parentSidePadding    = 16.0
	childGridColumnCount = 2
)

// childSlot computes the child-relative position for the N-th sibling in
// a parent's 2-column grid. Matches defaultChildSlot in
// canvas-topology.ts exactly — change them together. Leaf-sized slots
// only; for variable-size siblings use childSlotInGrid below.
func childSlot(index int) (x, y float64) {
	col := index % childGridColumnCount
	row := index / childGridColumnCount
	x = parentSidePadding + float64(col)*(childDefaultWidth+childGutter)
	y = parentHeaderPadding + float64(row)*(childDefaultHeight+childGutter)
	return
}

type nodeSize struct {
	width, height float64
}

// sizeOfSubtree computes the bounding-box size for a workspace and its
// entire descendant tree as rendered by the canvas grid layout.
// Post-order: leaves return the CHILD_DEFAULT footprint; parents return
// the size that fits all direct children (which may themselves be
// parents with grandchildren). Matches the client's
// `subtreeSize` pass in canvas-topology.ts so the server can lay out
// org imports the same way the canvas will render them.
func sizeOfSubtree(ws OrgWorkspace) nodeSize {
	if len(ws.Children) == 0 {
		return nodeSize{childDefaultWidth, childDefaultHeight}
	}
	cols := childGridColumnCount
	if len(ws.Children) < cols {
		cols = len(ws.Children)
	}
	rows := (len(ws.Children) + cols - 1) / cols
	childSizes := make([]nodeSize, len(ws.Children))
	maxColW := 0.0
	for i, c := range ws.Children {
		childSizes[i] = sizeOfSubtree(c)
		if childSizes[i].width > maxColW {
			maxColW = childSizes[i].width
		}
	}
	rowHeights := make([]float64, rows)
	for i, cs := range childSizes {
		row := i / cols
		if cs.height > rowHeights[row] {
			rowHeights[row] = cs.height
		}
	}
	totalRowH := 0.0
	for _, h := range rowHeights {
		totalRowH += h
	}
	return nodeSize{
		width:  parentSidePadding*2 + maxColW*float64(cols) + childGutter*float64(cols-1),
		height: parentHeaderPadding + totalRowH + childGutter*float64(rows-1) + parentSidePadding,
	}
}

// childSlotInGrid computes the relative position of sibling `index`
// given all siblings' subtree sizes. Uniform column width (= max width
// across siblings), per-row max height, so a nested parent sibling
// pushes its row down without displacing the column grid. Matches the
// TS mirror in canvas-topology.ts.
func childSlotInGrid(index int, siblingSizes []nodeSize) (x, y float64) {
	if len(siblingSizes) == 0 {
		return parentSidePadding, parentHeaderPadding
	}
	cols := childGridColumnCount
	if len(siblingSizes) < cols {
		cols = len(siblingSizes)
	}
	rows := (len(siblingSizes) + cols - 1) / cols
	maxColW := 0.0
	for _, s := range siblingSizes {
		if s.width > maxColW {
			maxColW = s.width
		}
	}
	rowHeights := make([]float64, rows)
	for i, s := range siblingSizes {
		row := i / cols
		if s.height > rowHeights[row] {
			rowHeights[row] = s.height
		}
	}
	col := index % cols
	row := index / cols
	x = parentSidePadding + float64(col)*(maxColW+childGutter)
	y = parentHeaderPadding
	for r := 0; r < row; r++ {
		y += rowHeights[r] + childGutter
	}
	return
}

// orgImportScheduleSQL is the upsert executed for every schedule during
// org/import. Extracted to a const so TestImport_OrgScheduleSQLShape can
// assert its shape without regex-scanning org.go (issue #24 follow-up).
//
// Guarantees, in one statement:
//   - INSERT new rows with source='template'
//   - On (workspace_id, name) collision, only refresh template-source rows
//     (runtime-added schedules are preserved across re-imports)
//   - No DELETE — removal is out of scope (additive semantics)
const orgImportScheduleSQL = `
INSERT INTO workspace_schedules (workspace_id, name, cron_expr, timezone, prompt, enabled, next_run_at, source)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'template')
ON CONFLICT (workspace_id, name) DO UPDATE
    SET cron_expr   = EXCLUDED.cron_expr,
        timezone    = EXCLUDED.timezone,
        prompt      = EXCLUDED.prompt,
        enabled     = EXCLUDED.enabled,
        next_run_at = EXCLUDED.next_run_at,
        updated_at  = now()
    WHERE workspace_schedules.source = 'template'
`

type OrgHandler struct {
	workspace   *WorkspaceHandler
	broadcaster *events.Broadcaster
	provisioner *provisioner.Provisioner
	channelMgr  *channels.Manager
	configsDir  string
	orgDir      string // path to org-templates/
}

func NewOrgHandler(wh *WorkspaceHandler, b *events.Broadcaster, p *provisioner.Provisioner, channelMgr *channels.Manager, configsDir, orgDir string) *OrgHandler {
	return &OrgHandler{
		workspace:   wh,
		broadcaster: b,
		provisioner: p,
		channelMgr:  channelMgr,
		configsDir:  configsDir,
		orgDir:      orgDir,
	}
}

// EnvRequirement is either a single env var name (strict: that exact
// var must be configured) or an any-of group (any one of the listed
// names satisfies the requirement).
//
// YAML shapes accepted:
//
//	required_env:
//	  - GITHUB_TOKEN                              # single
//	  - any_of: [ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN]   # OR group
//
// The any-of form exists because some runtimes accept either of two
// credential shapes — Claude Code takes ANTHROPIC_API_KEY or an OAuth
// token interchangeably, and forcing an org template to pick one
// would falsely block the other. For JSON (GET /org/templates),
// the same shapes round-trip: strings stay strings, groups stay
// {any_of: [...]}.
type EnvRequirement struct {
	// Name is non-empty for a single required env var.
	Name string
	// AnyOf is non-empty for an OR group; any one member satisfies.
	AnyOf []string
}

// Members returns every env name this requirement considers —
// [Name] for single, AnyOf for groups. Used by preflight, collect,
// and the name-validation regex gate.
func (e EnvRequirement) Members() []string {
	if e.Name != "" {
		return []string{e.Name}
	}
	return e.AnyOf
}

// IsSatisfied reports whether any member of the requirement is
// present in `configured`. Single: exact-match. AnyOf: at least
// one hit.
func (e EnvRequirement) IsSatisfied(configured map[string]struct{}) bool {
	for _, m := range e.Members() {
		if _, ok := configured[m]; ok {
			return true
		}
	}
	return false
}

// UnmarshalYAML accepts either a scalar (string → single) or a map
// with an `any_of` list (→ group).
func (e *EnvRequirement) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		e.Name = s
		return nil
	}
	var alt struct {
		AnyOf []string `yaml:"any_of"`
	}
	if err := value.Decode(&alt); err != nil {
		return fmt.Errorf("env requirement must be a string or {any_of: [...]}: %w", err)
	}
	if len(alt.AnyOf) == 0 {
		return fmt.Errorf("env requirement any_of must contain at least one env var")
	}
	e.AnyOf = alt.AnyOf
	return nil
}

// MarshalJSON emits the dual shape so GET /org/templates callers get
// {"required_env": ["GITHUB_TOKEN", {"any_of": [...]}]}, matching
// the YAML syntax.
func (e EnvRequirement) MarshalJSON() ([]byte, error) {
	if e.Name != "" {
		return json.Marshal(e.Name)
	}
	return json.Marshal(struct {
		AnyOf []string `json:"any_of"`
	}{AnyOf: e.AnyOf})
}

// UnmarshalJSON is the inverse — accepts the same dual shape so
// POST /org/import with an inline `template` body works too.
func (e *EnvRequirement) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		e.Name = s
		return nil
	}
	var alt struct {
		AnyOf []string `json:"any_of"`
	}
	if err := json.Unmarshal(data, &alt); err != nil {
		return fmt.Errorf("env requirement must be a string or {any_of: [...]}: %w", err)
	}
	if len(alt.AnyOf) == 0 {
		return fmt.Errorf("env requirement any_of must contain at least one env var")
	}
	e.AnyOf = alt.AnyOf
	return nil
}

// OrgTemplate is the YAML structure for an org hierarchy.
type OrgTemplate struct {
	Name           string              `yaml:"name" json:"name"`
	Description    string              `yaml:"description" json:"description"`
	Defaults       OrgDefaults         `yaml:"defaults" json:"defaults"`
	Workspaces     []OrgWorkspace      `yaml:"workspaces" json:"workspaces"`
	// GlobalMemories is a list of org-wide memories seeded as GLOBAL scope
	// on the first root workspace (PM) during org import. Issue #1050.
	GlobalMemories []models.MemorySeed `yaml:"global_memories" json:"global_memories"`
	// RequiredEnv lists env vars that MUST be configured globally (or
	// on every workspace in the subtree that needs them) before import
	// succeeds. Each entry is either a plain string (strict) or an
	// {any_of: [...]} group (at least one member must be set). Declared
	// at the org level for shared creds; also extensible per-workspace
	// via OrgWorkspace.RequiredEnv for team-scoped credentials.
	RequiredEnv []EnvRequirement `yaml:"required_env" json:"required_env"`
	// RecommendedEnv is the "nice-to-have" tier — import still succeeds
	// without them, but features degrade. Same single|any_of shape as
	// RequiredEnv so a recommended OR group reads "set any one of these
	// to unlock the feature; all missing = warning".
	RecommendedEnv []EnvRequirement `yaml:"recommended_env" json:"recommended_env"`
}

type OrgDefaults struct {
	Runtime       string   `yaml:"runtime" json:"runtime"`
	Tier          int      `yaml:"tier" json:"tier"`
	Model         string   `yaml:"model" json:"model"`
	Plugins       []string `yaml:"plugins" json:"plugins"`
	InitialPrompt string   `yaml:"initial_prompt" json:"initial_prompt"`
	// InitialPromptFile is a file ref alternative to InitialPrompt. Path is
	// resolved relative to the workspace's files_dir (or the org base dir
	// when used at defaults level — defaults don't have their own files_dir,
	// so the file must live at the org root). Inline InitialPrompt wins
	// when both are set.
	InitialPromptFile string `yaml:"initial_prompt_file" json:"initial_prompt_file"`
	// IdlePrompt / IdleIntervalSeconds are the workspace-default idle-loop
	// body and cadence (see workspace/heartbeat.py). They were
	// previously dropped by the org importer because the struct didn't
	// declare them — causing live configs to boot without idle_prompts
	// even when org.yaml had them. Phase 1 scalability work adds both
	// inline + file-ref forms.
	IdlePrompt           string `yaml:"idle_prompt" json:"idle_prompt"`
	IdlePromptFile       string `yaml:"idle_prompt_file" json:"idle_prompt_file"`
	IdleIntervalSeconds  int    `yaml:"idle_interval_seconds" json:"idle_interval_seconds"`
	// CategoryRouting maps issue/audit category → list of target roles.
	// Per-workspace blocks UNION + override per-key with these defaults.
	// Rendered into each workspace's config.yaml so agent prompts can read it
	// generically (no hardcoded role names in prompts). See issue #51.
	CategoryRouting map[string][]string `yaml:"category_routing" json:"category_routing"`
	// InitialMemories are default memories seeded into every workspace at
	// creation time unless the workspace overrides them. Issue #1050.
	InitialMemories []models.MemorySeed `yaml:"initial_memories" json:"initial_memories"`
}

type OrgSchedule struct {
	Name     string `yaml:"name" json:"name"`
	CronExpr string `yaml:"cron_expr" json:"cron_expr"`
	Timezone string `yaml:"timezone" json:"timezone"`
	Prompt   string `yaml:"prompt" json:"prompt"`
	// PromptFile is a file ref alternative to inline Prompt. Path is
	// resolved relative to the workspace's files_dir. Inline Prompt wins
	// when both are set. Scalability: hourly/weekly cron prompts are the
	// largest text bodies in org.yaml (~1-5 KB each); externalizing them
	// cuts the file by ~62%.
	PromptFile string `yaml:"prompt_file" json:"prompt_file"`
	Enabled    *bool  `yaml:"enabled" json:"enabled"`
}

// OrgChannel defines a social channel (Telegram, Slack, etc.) to auto-link
// when the workspace is created. Config values may reference env vars
// using ${VAR_NAME} syntax — useful for keeping bot tokens out of YAML.
type OrgChannel struct {
	Type         string            `yaml:"type" json:"type"`
	Config       map[string]string `yaml:"config" json:"config"`
	AllowedUsers []string          `yaml:"allowed_users" json:"allowed_users"`
	Enabled      *bool             `yaml:"enabled" json:"enabled"`
}

type OrgWorkspace struct {
	Name     string `yaml:"name" json:"name"`
	Role     string `yaml:"role" json:"role"`
	Runtime  string `yaml:"runtime" json:"runtime"`
	Tier     int    `yaml:"tier" json:"tier"`
	Template string `yaml:"template" json:"template"`
	FilesDir string `yaml:"files_dir" json:"files_dir"`
	// Spawning gates whether this workspace (AND its descendants) gets
	// provisioned during /org/import. Pointer so we can distinguish
	// "explicitly set to false" from "unset" (default = spawn). Use case:
	// the dev-tree org template declares the full team structure but a
	// developer's local machine only has RAM for a subset; setting
	// spawning: false on a leaf or a sub-tree root skips that branch
	// entirely without editing the canonical template structure.
	// Counted in countWorkspaces same as actual; subtree-skip happens
	// at provision time in createWorkspaceTree.
	Spawning *bool `yaml:"spawning,omitempty" json:"spawning,omitempty"`
	// SystemPrompt is an inline override. Normally each role's system-prompt.md
	// lives at `<files_dir>/system-prompt.md` and is copied via the files_dir
	// template-copy step; inline overrides that path for ad-hoc workspaces.
	SystemPrompt    string   `yaml:"system_prompt" json:"system_prompt"`
	Model           string   `yaml:"model" json:"model"`
	WorkspaceDir    string   `yaml:"workspace_dir" json:"workspace_dir"`
	WorkspaceAccess string   `yaml:"workspace_access" json:"workspace_access"` // #65: "none" (default), "read_only", "read_write"
	Plugins         []string `yaml:"plugins" json:"plugins"`
	// InitialPrompt is the one-shot boot prompt. Agents run this once on first
	// start; the body often clones the repo, reads CLAUDE.md + system-prompt,
	// and commits conventions to memory. InitialPromptFile is the file-ref
	// alternative — read at import time from `<files_dir>/<InitialPromptFile>`.
	// Inline wins when both are set.
	InitialPrompt     string `yaml:"initial_prompt" json:"initial_prompt"`
	InitialPromptFile string `yaml:"initial_prompt_file" json:"initial_prompt_file"`
	// IdlePrompt / IdleIntervalSeconds drive the idle-loop reflection
	// pattern (#205). When IdlePrompt is non-empty, the workspace self-sends
	// this prompt every IdleIntervalSeconds while heartbeat.active_tasks == 0.
	// Both fields were previously dropped by the org importer (struct didn't
	// declare them); Phase 1 scalability PR adds them so engineer + researcher
	// idle loops propagate correctly from org.yaml → /configs/config.yaml.
	// IdlePromptFile is the file-ref alternative — same semantics as
	// InitialPromptFile. Inline wins when both are set.
	IdlePrompt          string `yaml:"idle_prompt" json:"idle_prompt"`
	IdlePromptFile      string `yaml:"idle_prompt_file" json:"idle_prompt_file"`
	IdleIntervalSeconds int    `yaml:"idle_interval_seconds" json:"idle_interval_seconds"`
	// CategoryRouting extends/overrides defaults.category_routing per-workspace.
	// Merge semantics: workspace keys replace defaults' value for the same key
	// (empty list drops the category entirely); new keys are added. See
	// mergeCategoryRouting.
	CategoryRouting map[string][]string `yaml:"category_routing" json:"category_routing"`
	// InitialMemories are memories seeded into this workspace at creation
	// time. If empty, defaults.initial_memories are used. Issue #1050.
	InitialMemories []models.MemorySeed `yaml:"initial_memories" json:"initial_memories"`
	// MaxConcurrentTasks: see models.CreateWorkspacePayload.
	MaxConcurrentTasks int                 `yaml:"max_concurrent_tasks" json:"max_concurrent_tasks"`
	Schedules          []OrgSchedule       `yaml:"schedules" json:"schedules"`
	Channels           []OrgChannel        `yaml:"channels" json:"channels"`
	External        bool                `yaml:"external" json:"external"`
	URL             string              `yaml:"url" json:"url"`
	Canvas          struct {
		X float64 `yaml:"x" json:"x"`
		Y float64 `yaml:"y" json:"y"`
	} `yaml:"canvas" json:"canvas"`
	// RequiredEnv / RecommendedEnv declared at the workspace level
	// narrow down what a specific team needs beyond the org-wide union.
	// When GET /org/templates walks the tree, these flow up into
	// OrgTemplate.RequiredEnv / RecommendedEnv. A workspace's subtree
	// inherits: a parent declaring ANTHROPIC_API_KEY as required
	// means every descendant considers it required too (no override
	// needed at each leaf). Same single|any_of shape as the org-level
	// lists.
	RequiredEnv    []EnvRequirement `yaml:"required_env" json:"required_env"`
	RecommendedEnv []EnvRequirement `yaml:"recommended_env" json:"recommended_env"`
	Children       []OrgWorkspace   `yaml:"children" json:"children"`
}

// ListTemplates handles GET /org/templates — lists available org templates.
func (h *OrgHandler) ListTemplates(c *gin.Context) {
	templates := []map[string]interface{}{}

	entries, err := os.ReadDir(h.orgDir)
	if err != nil {
		c.JSON(http.StatusOK, templates)
		return
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Look for org.yaml inside the directory
		templateDir := filepath.Join(h.orgDir, e.Name())
		orgFile := filepath.Join(templateDir, "org.yaml")
		data, err := os.ReadFile(orgFile)
		if err != nil {
			// Try org.yml
			orgFile = filepath.Join(templateDir, "org.yml")
			data, err = os.ReadFile(orgFile)
			if err != nil {
				// Half-clone detection: a directory that contains a `.git/`
				// but no `org.yaml`/`org.yml` is almost always a manifest
				// clone that got truncated mid-checkout. Surfacing this as
				// a warning instead of a silent skip prevents the
				// "template missing from registry" failure mode (audit
				// 2026-04-24: org-templates/molecule-dev/ had only `.git/`
				// and silently dropped from the Canvas palette for hours
				// before anyone noticed).
				gitDir := filepath.Join(templateDir, ".git")
				if _, gitErr := os.Stat(gitDir); gitErr == nil {
					log.Printf("ListTemplates: WARNING %q has .git but no org.yaml/.yml — likely a half-checkout. Try 'cd %s && git checkout main -- .' to restore the working tree.", e.Name(), templateDir)
				}
				continue
			}
		}
		// Expand !include directives before unmarshal so templates that
		// split across team/role files still report an accurate workspace
		// count on the /org/templates listing. Fail loudly on expansion
		// errors — the previous silent-continue made a broken template
		// show up as "no templates" in the Canvas palette with no log
		// trail, which is how a fresh-clone user first discovers the gap.
		if expanded, err := resolveYAMLIncludes(data, templateDir); err == nil {
			data = expanded
		} else {
			log.Printf("ListTemplates: skipping %s — !include expansion failed: %v", e.Name(), err)
			continue
		}
		var tmpl OrgTemplate
		if err := yaml.Unmarshal(data, &tmpl); err != nil {
			log.Printf("ListTemplates: skipping %s — yaml unmarshal failed: %v", e.Name(), err)
			continue
		}
		count := countWorkspaces(tmpl.Workspaces)
		// Walk the tree to collect required + recommended env union.
		// Canvas uses these to render a preflight modal BEFORE firing
		// the import — saves the user from a 15-workspace import that
		// dies one container at a time on missing creds.
		required, recommended := collectOrgEnv(&tmpl)
		templates = append(templates, map[string]interface{}{
			"dir":             e.Name(),
			"name":            tmpl.Name,
			"description":     tmpl.Description,
			"workspaces":      count,
			"required_env":    required,
			"recommended_env": recommended,
		})
	}

	c.JSON(http.StatusOK, templates)
}

// Import handles POST /org/import — creates an entire org from a template.
func (h *OrgHandler) Import(c *gin.Context) {
	var body struct {
		Dir      string      `json:"dir"`      // org template directory name
		Template OrgTemplate `json:"template"` // or inline template
		// Mode controls cleanup behavior of pre-existing workspaces:
		//   ""        / "merge"     — additive (default; current behavior).
		//                              Existing workspaces matched by
		//                              (parent_id, name) are skipped; nothing
		//                              outside the new tree is touched.
		//   "reconcile"             — additive + cleanup. After import, any
		//                              online workspace whose name matches an
		//                              imported workspace's name but whose id
		//                              isn't in the import result set is
		//                              cascade-deleted. Catches "previous
		//                              import survived a re-import" zombies
		//                              (the 20:13→21:17 dev-tree case).
		Mode string `json:"mode"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	var tmpl OrgTemplate
	var orgBaseDir string // base directory for files_dir resolution

	if body.Dir != "" {
		// Reject traversal attempts — `dir` must resolve inside h.orgDir.
		// Without this, `dir: "../../../etc"` gets joined into h.orgDir and
		// filepath.Join's lexical cleanup resolves it outside the root,
		// letting an unauthenticated caller probe arbitrary filesystem paths.
		resolved, err := resolveInsideRoot(h.orgDir, body.Dir)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid org directory"})
			return
		}
		orgBaseDir = resolved
		orgFile := filepath.Join(orgBaseDir, "org.yaml")
		data, err := os.ReadFile(orgFile)
		if err != nil {
			// Audit 2026-05-09 (Core-Security): the prior message echoed
			// the user-supplied `body.Dir` verbatim. Path traversal is
			// already blocked by resolveInsideRoot above, but echoing
			// the raw input back lets a client probe for the existence
			// of relative paths inside h.orgDir (a 404 with the input
			// vs. a 400 from resolveInsideRoot is itself a signal).
			// Drop the input from the message; log full context server-
			// side via the resolved path for operator triage.
			log.Printf("OrgImport: failed to read %s (requested dir=%q): %v", orgFile, body.Dir, err)
			c.JSON(http.StatusNotFound, gin.H{"error": "org template not found"})
			return
		}
		// Expand !include directives before unmarshal. Splits org.yaml
		// into per-team or per-role files; Phase 3 of the scalability
		// refactor. Fails loudly on missing / cyclic / escaping includes.
		expanded, err := resolveYAMLIncludes(data, orgBaseDir)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "org template expansion failed"})
			return
		}
		if err := yaml.Unmarshal(expanded, &tmpl); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid org template"})
			return
		}
	} else if body.Template.Name != "" {
		tmpl = body.Template
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provide 'dir' or 'template'"})
		return
	}

	// Emit started AFTER the YAML is loaded so payload.name carries the
	// resolved template name (was: empty when caller passed `dir` instead
	// of inline `template`). Pre-parse error paths above return without
	// emitting — semantically "we couldn't even start an import" — so
	// every started event is guaranteed a paired completed/failed below
	// (no orphan started rows in structure_events).
	importStart := time.Now()
	emitOrgEvent(c.Request.Context(), "org.import.started", map[string]any{
		"name": tmpl.Name,
		"dir":  body.Dir,
		"mode": body.Mode,
	})

	// Required-env preflight — refuses import when any required_env is
	// missing from global_secrets. No bypass: the prior `force: true`
	// escape hatch was removed (issue #2290) because it was the silent
	// failure mode that let an org import without ANTHROPIC_API_KEY and
	// ship workspaces that 401'd on every LLM call. The canvas runs the
	// same check client-side against GET /org/templates output and shows
	// a modal so users set keys before clicking Import; this server-side
	// check is the authoritative guard in case a caller bypasses the UI
	// (CLI, API clients, etc.). 412 Precondition Failed carries the
	// missing-key list so tooling can render the same add-key flow.
	required, _ := collectOrgEnv(&tmpl)
	if len(required) > 0 {
		ctx := c.Request.Context()
		configured, err := loadConfiguredGlobalSecretKeys(ctx)
		if err != nil {
			// Fail closed. Previously this fell through and imported
			// anyway, defeating the preflight for exactly the case
			// it's meant to cover. A DB hiccup should look like a
			// retryable 500, not a silent green light for an import
			// that will fail at container-start time on every node.
			log.Printf("Org import preflight: global secrets lookup failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "could not verify required environment variables; try again",
			})
			return
		}
		var missing []EnvRequirement
		for _, req := range required {
			// For a single requirement this is exact-match; for an
			// any-of group, any one member satisfies. Groups whose
			// alternative is already configured drop out here — the
			// user doesn't need to re-configure them.
			if !req.IsSatisfied(configured) {
				missing = append(missing, req)
			}
		}
		if len(missing) > 0 {
			c.JSON(http.StatusPreconditionFailed, gin.H{
				"error":        "missing required environment variables",
				"missing_env":  missing,
				"required_env": required,
				"template":     tmpl.Name,
				"suggestion":   "set these as global secrets (POST /settings/secrets) before importing",
			})
			return
		}
	}

	results := []map[string]interface{}{}
	var createErr error

	// Semaphore limits concurrent provision goroutines (#1084).
	// Cap is configurable via MOLECULE_PROVISION_CONCURRENCY:
	//   unset → 3 (Docker-mode default)
	//   "0"   → effectively unlimited (SaaS / EC2 backend)
	//   N>0   → exactly N
	// See resolveProvisionConcurrency for the full env-parse contract.
	concurrency := resolveProvisionConcurrency()
	provisionSem := make(chan struct{}, concurrency)
	log.Printf("org_import: provision concurrency cap=%d (env MOLECULE_PROVISION_CONCURRENCY=%q)",
		concurrency, os.Getenv("MOLECULE_PROVISION_CONCURRENCY"))

	// Recursively create workspaces. Root workspaces keep their YAML
	// canvas coords; children are positioned by createWorkspaceTree
	// using subtree-aware grid slots (children that are themselves
	// parents get a bigger slot so they don't overflow into siblings).
	for _, ws := range tmpl.Workspaces {
		// Root: relX/relY == absX/absY (no parent to be relative to).
		if err := h.createWorkspaceTree(ws, nil, ws.Canvas.X, ws.Canvas.Y, ws.Canvas.X, ws.Canvas.Y, tmpl.Defaults, orgBaseDir, &results, provisionSem); err != nil {
			createErr = err
			break
		}
	}

	// Seed org-wide global_memories on the first root workspace (issue #1050).
	// These are GLOBAL scope memories visible to all workspaces in the org.
	if len(tmpl.GlobalMemories) > 0 && len(results) > 0 {
		rootID, _ := results[0]["id"].(string)
		if rootID != "" {
			rootNS := workspaceAwarenessNamespace(rootID)
			// Force scope to GLOBAL regardless of what the YAML says.
			globalSeeds := make([]models.MemorySeed, len(tmpl.GlobalMemories))
			for i, gm := range tmpl.GlobalMemories {
				globalSeeds[i] = models.MemorySeed{Content: gm.Content, Scope: "GLOBAL"}
			}
			seedInitialMemories(context.Background(), rootID, globalSeeds, rootNS)
			log.Printf("Org import: seeded %d global memories on root workspace %s", len(globalSeeds), rootID)
		}
	}

	// Hot-reload channel manager once after all channels are inserted
	// (instead of per-workspace, avoiding N redundant DB queries + diffs).
	if h.channelMgr != nil {
		hasAnyChannels := false
		for _, r := range results {
			if _, ok := r["channels"]; ok {
				hasAnyChannels = true
				break
			}
		}
		if hasAnyChannels {
			h.channelMgr.Reload(context.Background())
		}
	}

	// Reconcile mode: prune workspaces present from a previous import that
	// share a name with the new tree but are NOT in the new result set.
	// Catches the additive-import bug where re-running /org/import with a
	// changed tree shape (different parent_id for the same role name) leaves
	// the prior workspace online — visible to the canvas, consuming
	// containers, and looking like a duplicate. Default mode "" / "merge"
	// preserves the old additive behavior.
	reconcileRemovedCount := 0
	reconcileSkipped := 0
	reconcileErrs := []string{}
	if body.Mode == "reconcile" && createErr == nil {
		ctx := c.Request.Context()
		importedNames := []string{}
		walkOrgWorkspaceNames(tmpl.Workspaces, &importedNames)

		importedIDs := make([]string, 0, len(results))
		for _, r := range results {
			if id, ok := r["id"].(string); ok && id != "" {
				importedIDs = append(importedIDs, id)
			}
		}

		// Empty-set guards: if the import didn't produce any names or any
		// IDs, skip — querying with empty arrays would either match
		// nothing (harmless) or, worse, match every workspace if a future
		// query rewrite drops the IN clause. Belt-and-suspenders.
		if len(importedNames) > 0 && len(importedIDs) > 0 {
			rows, err := db.DB.QueryContext(ctx, `
				SELECT id FROM workspaces
				WHERE name = ANY($1::text[])
				  AND id != ALL($2::uuid[])
				  AND status != 'removed'
			`, pq.Array(importedNames), pq.Array(importedIDs))
			if err != nil {
				log.Printf("Org import reconcile: orphan query failed: %v", err)
				reconcileErrs = append(reconcileErrs, fmt.Sprintf("orphan query: %v", err))
			} else {
				orphanIDs := []string{}
				for rows.Next() {
					var orphanID string
					if rows.Scan(&orphanID) == nil {
						orphanIDs = append(orphanIDs, orphanID)
					}
				}
				rows.Close()

				for _, oid := range orphanIDs {
					descendantIDs, stopErrs, err := h.workspace.CascadeDelete(ctx, oid)
					if err != nil {
						log.Printf("Org import reconcile: CascadeDelete(%s) failed: %v", oid, err)
						reconcileErrs = append(reconcileErrs, fmt.Sprintf("delete %s: %v", oid, err))
						reconcileSkipped++
						continue
					}
					reconcileRemovedCount += 1 + len(descendantIDs)
					if len(stopErrs) > 0 {
						log.Printf("Org import reconcile: %s had %d stop errors (orphan sweeper will retry)", oid, len(stopErrs))
					}
				}
				log.Printf("Org import reconcile: %d orphans removed (%d cascade descendants), %d skipped", len(orphanIDs), reconcileRemovedCount-len(orphanIDs), reconcileSkipped)
			}
		}
	}

	status := http.StatusCreated
	resp := gin.H{
		"org":        tmpl.Name,
		"workspaces": results,
		"count":      len(results),
	}
	if body.Mode == "reconcile" {
		resp["mode"] = "reconcile"
		resp["reconcile_removed_count"] = reconcileRemovedCount
		if len(reconcileErrs) > 0 {
			resp["reconcile_errors"] = reconcileErrs
		}
	}
	if createErr != nil {
		status = http.StatusMultiStatus
		resp["error"] = createErr.Error()
	}

	// results contains both freshly-created AND lookupExistingChild skips
	// (entries with "skipped":true). Splitting the count here so the audit
	// row reflects "what changed" vs "what was already there" — telemetry
	// readers shouldn't need to grep stdout to tell an idempotent re-run
	// apart from a fresh-create.
	createdCount, skippedCount := 0, 0
	for _, r := range results {
		if skipped, _ := r["skipped"].(bool); skipped {
			skippedCount++
		} else {
			createdCount++
		}
	}
	log.Printf("Org import: %s — %d created, %d skipped, %d reconciled",
		tmpl.Name, createdCount, skippedCount, reconcileRemovedCount)
	emitOrgEvent(c.Request.Context(), "org.import.completed", map[string]any{
		"name":                    tmpl.Name,
		"dir":                     body.Dir,
		"mode":                    body.Mode,
		"created_count":           createdCount,
		"skipped_count":           skippedCount,
		"reconcile_removed_count": reconcileRemovedCount,
		"reconcile_errors":        len(reconcileErrs),
		"duration_ms":             time.Since(importStart).Milliseconds(),
		"create_error":            errString(createErr),
	})
	c.JSON(status, resp)
}

// walkOrgWorkspaceNames collects every Name in the tree (in any order) into
// names. Used by reconcile to detect orphan workspaces — workspaces with the
// same role name as a freshly-imported one but a different id, surviving from
// a prior import.
func walkOrgWorkspaceNames(workspaces []OrgWorkspace, names *[]string) {
	for _, w := range workspaces {
		// spawning:false subtrees are still part of the imported tree
		// from a logical-tree perspective — DON'T skip the recursion,
		// or reconcile would orphan the rest of the subtree on every
		// re-import where spawning is toggled. Names of skipped
		// workspaces remain registered so reconcile won't double-create
		// them when spawning flips back to true.
		if w.Name != "" {
			*names = append(*names, w.Name)
		}
		walkOrgWorkspaceNames(w.Children, names)
	}
}

// emitOrgEvent records an org-lifecycle event in structure_events so the
// import history is queryable independent of stdout log retention. Errors
// are logged and swallowed — never block the request path on telemetry.
//
// Event-type taxonomy (extend by appending; never rename):
//
//	org.import.started        — handler entered, request body parsed
//	org.import.completed      — handler exiting (success or partial)
//	org.import.failed         — handler exiting with an unrecoverable error
//
// payload fields are documented at each call site.
func emitOrgEvent(ctx context.Context, eventType string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		log.Printf("emitOrgEvent: marshal %s payload failed: %v", eventType, err)
		return
	}
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO structure_events (event_type, payload, created_at)
		VALUES ($1, $2, now())
	`, eventType, payloadJSON); err != nil {
		log.Printf("emitOrgEvent: insert %s failed: %v", eventType, err)
	}
}

// errString returns "" for a nil error, err.Error() otherwise. Lets us put
// nullable error strings in event payloads without checking for nil at every
// call site.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

