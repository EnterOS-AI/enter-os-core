package provisioner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/provlog"
)

// CPProvisionerAPI is the contract WorkspaceHandler uses to talk to the
// control-plane provisioner. Extracted as an interface (#1814) so handler
// tests can substitute a mock without standing up the real CP HTTP client
// + auth chain. Production wires *CPProvisioner directly via
// NewCPProvisioner — see the compile-time assertion below.
//
// Method set is intentionally narrow — only the methods that
// WorkspaceHandler actually calls. Adding a new handler call site that
// reaches into CPProvisioner means widening this interface explicitly,
// which surfaces the dependency in code review.
type CPProvisionerAPI interface {
	Start(ctx context.Context, cfg WorkspaceConfig) (string, error)
	Stop(ctx context.Context, workspaceID string) error
	// StopAndPrune is Stop + "erase the durable data volume" (internal#734),
	// for the permanent-delete-with-erase flow ONLY. Restart/recreate use Stop.
	StopAndPrune(ctx context.Context, workspaceID string) error
	GetConsoleOutput(ctx context.Context, workspaceID string) (string, error)
	// IsRunning reports whether the workspace's compute (EC2 instance) is
	// currently in the running state. Surfaced on the interface (rather than
	// only on *CPProvisioner) so the a2a-proxy reactive-health path can
	// detect dead EC2 agents the same way it detects dead Docker containers.
	// Pre-#NNN, maybeMarkContainerDead only consulted the local Docker
	// provisioner — for SaaS tenants (h.provisioner=nil) the check was a
	// no-op, so a dead EC2 agent would leak 502/503 to canvas with no
	// auto-recovery. (true, err) on transport errors keeps callers on the
	// alive path; (false, nil) is the only definitive "dead" signal.
	IsRunning(ctx context.Context, workspaceID string) (bool, error)
}

// Compile-time assertion: *CPProvisioner satisfies CPProvisionerAPI.
// Catches a future method-signature drift at build time instead of at
// the SetCPProvisioner call site (which would be a runtime "interface
// not implemented" only when the SaaS path is exercised).
var _ CPProvisionerAPI = (*CPProvisioner)(nil)

// CPProvisioner provisions workspace agents by calling the control plane's
// workspace provision API. The control plane creates EC2 instances with
// Docker + the workspace runtime installed at boot from PyPI.
//
// Auto-activated when MOLECULE_ORG_ID is set (SaaS tenant).
type CPProvisioner struct {
	baseURL       string
	orgID         string
	sharedSecret  string // Authorization: Bearer — gates /cp/workspaces/* (provision routes)
	adminToken    string // X-Molecule-Admin-Token — per-tenant identity (controlplane #118/#130)
	cpAdminAPIKey string // Authorization: Bearer — gates /cp/admin/* (read-only ops routes; distinct secret from sharedSecret)
	httpClient    *http.Client
}

// NewCPProvisioner creates a provisioner that delegates to the control plane.
func NewCPProvisioner() (*CPProvisioner, error) {
	orgID := os.Getenv("MOLECULE_ORG_ID")
	if orgID == "" {
		return nil, fmt.Errorf("MOLECULE_ORG_ID required for control plane provisioner")
	}

	// Auto-derive control plane URL.
	baseURL := os.Getenv("CP_PROVISION_URL")
	if baseURL == "" {
		baseURL = os.Getenv("MOLECULE_CP_URL")
	}
	if baseURL == "" {
		baseURL = "https://api.moleculesai.app"
	}

	// CP gates /cp/workspaces/* behind two credentials now:
	//   1. Shared secret (Authorization: Bearer) — gates the route at
	//      the router level, proves the caller is a tenant platform.
	//   2. Admin token (X-Molecule-Admin-Token) — proves WHICH tenant.
	//      Introduced in controlplane #118/#130 to prevent cross-tenant
	//      provisioning when the shared secret leaks from one tenant.
	sharedSecret := os.Getenv("MOLECULE_CP_SHARED_SECRET")
	if sharedSecret == "" {
		// Fall back to PROVISION_SHARED_SECRET so a single env-var name
		// works on both sides of the wire.
		sharedSecret = os.Getenv("PROVISION_SHARED_SECRET")
	}
	// ADMIN_TOKEN is injected into the tenant container at provision
	// time by the control plane (see provisioner/ec2.go Secrets Manager
	// bootstrap path). Without it, post-#118 CP rejects every
	// /cp/workspaces/* call with 401.
	adminToken := os.Getenv("ADMIN_TOKEN")
	// CP_ADMIN_API_TOKEN gates /cp/admin/* (distinct from the provision
	// shared secret so a compromised tenant's provision creds can't read
	// other tenants' serial console). Falls back to sharedSecret only for
	// dev / legacy self-hosted deployments that don't split the two.
	cpAdminAPIKey := os.Getenv("CP_ADMIN_API_TOKEN")
	if cpAdminAPIKey == "" {
		cpAdminAPIKey = sharedSecret
	}

	return &CPProvisioner{
		baseURL:       baseURL,
		orgID:         orgID,
		sharedSecret:  sharedSecret,
		adminToken:    adminToken,
		cpAdminAPIKey: cpAdminAPIKey,
		httpClient:    &http.Client{Timeout: 120 * time.Second},
	}, nil
}

// provisionAuthHeaders sets the auth headers for /cp/workspaces/* routes:
//   - Authorization: Bearer <shared secret> — platform gate
//   - X-Molecule-Admin-Token: <per-tenant token> — identity gate
//
// Either is a no-op when its value is empty so self-hosted / dev
// deployments without a real CP still work (those don't hit a CP that
// enforces either gate). In prod both are set by the controlplane
// bootstrap, so both headers land on every outbound call.
func (p *CPProvisioner) provisionAuthHeaders(req *http.Request) {
	if p.sharedSecret != "" {
		req.Header.Set("Authorization", "Bearer "+p.sharedSecret)
	}
	if p.adminToken != "" {
		req.Header.Set("X-Molecule-Admin-Token", p.adminToken)
	}
}

// adminAuthHeaders sets the auth header for /cp/admin/* routes. The CP
// gates this route family with CP_ADMIN_API_TOKEN — a distinct secret
// from the provision-route shared secret so a compromised tenant can't
// read other tenants' serial console via /cp/admin/workspaces/:id/console.
//
// The per-tenant X-Molecule-Admin-Token is still included for parity
// with the provision path (CP may cross-check it for audit attribution
// even on admin calls).
func (p *CPProvisioner) adminAuthHeaders(req *http.Request) {
	if p.cpAdminAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cpAdminAPIKey)
	}
	if p.adminToken != "" {
		req.Header.Set("X-Molecule-Admin-Token", p.adminToken)
	}
}

type cpProvisionRequest struct {
	OrgID        string `json:"org_id"`
	WorkspaceID  string `json:"workspace_id"`
	Runtime      string `json:"runtime"`
	Template     string `json:"template,omitempty"`
	Tier         int    `json:"tier"`
	InstanceType string `json:"instance_type,omitempty"`
	DiskGB       int32  `json:"disk_gb,omitempty"`
	// Provider routes the CP to the compute backend for this workspace box
	// (multi-provider RFC, per-workspace). Distinct from the LLM/model provider.
	Provider string `json:"provider,omitempty"`
	// DataPersistence is the per-workspace durable-data choice (internal#734);
	// CP validates the enum at its provision edge and resolves the data volume
	// from it. Empty = auto (omitted on the wire).
	DataPersistence string `json:"data_persistence,omitempty"`
	// Kind forwards the workspace kind ("" / "workspace" ordinary, "platform"
	// = the org concierge) so the CP can select the platform-agent image
	// variant — the SaaS mirror of the local Docker provisioner's kind-driven
	// image preference (RFC docs/design/rfc-platform-agent.md; core#2495 SSOT:
	// the concierge is a normal workspace provisioned through this same path,
	// differing ONLY in image + config overlay). Omitted when empty so the
	// wire shape is unchanged for ordinary workspaces; an older CP simply
	// ignores the field.
	Kind        string                 `json:"kind,omitempty"`
	Display     WorkspaceDisplayConfig `json:"display,omitempty"`
	PlatformURL string                 `json:"platform_url"`
	Env         map[string]string      `json:"env"`
	// ConfigFiles are template + generated config files to write into the
	// EC2 instance's /configs directory. OFFSEC-010: collected by
	// collectCPConfigFiles which rejects symlinks and non-regular files
	// before including them. Serialised as base64 to avoid JSON escaping.
	//
	// ConfigFiles is the SM-bound bundle (the CP stages it through AWS
	// Secrets Manager as molecule/workspace/<id>/config). It's the right
	// transport for SMALL non-secret config text only — it has a 256 KiB
	// cap and the SM transport is sized + scoped for *secrets*. See the
	// core-devops 10:13 SM-inventory RCA.
	ConfigFiles map[string]string `json:"config_files,omitempty"`

	// TemplateAssets (RFC #2843 #24) are non-secret template assets
	// (config.yaml + prompts/ — agent-skills are plugins now, RFC#2843
	// #32) fetched from a non-secret asset channel (template repo /
	// Gitea shallow clone per RFC §4.2 transport option (a)). They
	// travel on a SEPARATE wire field from ConfigFiles (the SM-bound
	// bundle) so a future CP can route them through a non-secret
	// transport without going through the SM cap. Serialised as base64
	// to avoid JSON escaping.
	//
	// Keys are restricted to the template-asset allowlist enforced by
	// IsCPTemplateAssetPath (config.yaml / prompts/* only);
	// see collectCPConfigFiles for the enforcement.
	TemplateAssets map[string][]byte `json:"template_assets,omitempty"`
}

type cpProvisionResponse struct {
	InstanceID string `json:"instance_id"`
	PrivateIP  string `json:"private_ip"`
	State      string `json:"state"`
	Error      string `json:"error"`
}

// buildCPTenantEnv assembles the env map the control plane forwards to a
// tenant EC2 workspace container, applying the forensic #145 SCM-write-token
// guard.
//
// The guard strips every key classified by isSCMWriteTokenKey (GITEA_TOKEN,
// GITHUB_TOKEN, …) UNLESS that key is positively workspace-authored —
// i.e. present in cfg.WorkspaceSecretKeys, the provenance set populated from
// the workspace_secrets table. Rationale:
//
//   - Operator / persona-merged (global-scoped) SCM-write tokens are an
//     upstream bleed and MUST NOT reach an agent-controlled container — that
//     keeps the two-eyes review gate structurally self-bypass-proof.
//   - A workspace-scoped GITEA_TOKEN that an org admin deliberately set via
//     the canvas Secrets tab is the INTENDED delivery channel for that
//     workspace's reviewer agent. Stripping it broke codex reviewers
//     (whoami 401/404). It is exempt.
//
// Fail-safe: a nil cfg.WorkspaceSecretKeys yields wsAuthored=false for every
// key, so a missing provenance map strips ALL SCM-write tokens rather than
// leaking them. adminToken, when non-empty, is injected as ADMIN_TOKEN (it is
// never an SCM-write key, so the guard never touches it).
func buildCPTenantEnv(cfg WorkspaceConfig, adminToken string) map[string]string {
	env := make(map[string]string, len(cfg.EnvVars)+1)
	for k, v := range cfg.EnvVars {
		if isSCMWriteTokenKey(k) {
			_, wsAuthored := cfg.WorkspaceSecretKeys[k] // nil map → false (fail-safe)
			if !wsAuthored {
				log.Printf("CPProvisioner.Start: dropped SCM-write credential %q from tenant workspace env (forensic #145 guard; provenance=operator/global)", k)
				continue
			}
			log.Printf("CPProvisioner.Start: preserved workspace-authored SCM credential %q for tenant workspace (forensic #145: workspace_secrets provenance, intended delivery)", k)
		}
		env[k] = v
	}
	if adminToken != "" {
		env["ADMIN_TOKEN"] = adminToken
	}
	return env
}

// Start provisions a workspace by calling the control plane → EC2.
func (p *CPProvisioner) Start(ctx context.Context, cfg WorkspaceConfig) (string, error) {
	// Inject ADMIN_TOKEN into the workspace container env so the agent can call
	// /admin/liveness and other admin-gated platform endpoints (core#831).
	// p.adminToken is read from os.Getenv("ADMIN_TOKEN") at provisioner creation;
	// it is also used for CP→platform HTTP auth but those are separate concerns.
	//
	// Forensic #145 hardening: tenant workspaces run on EC2 via this path, so
	// the SCM-write-token denylist (see buildContainerEnv) is enforced here
	// too. Always build a filtered copy — never pass cfg.EnvVars through
	// verbatim — so a latent persona-merged GITEA_TOKEN can't reach the
	// tenant container regardless of whether ADMIN_TOKEN is set. Extracted to
	// buildCPTenantEnv so the strip/exempt logic is unit-testable without
	// standing up the CP HTTP round-trip.
	env := buildCPTenantEnv(cfg, p.adminToken)
	// Collect template files and generated configs, with OFFSEC-010 guards:
	// - Rejects symlinks at the template root (prevents bypass via symlink traversal)
	// - Skips symlinks during WalkDir (prevents /etc/passwd etc. inclusion)
	// - Validates all paths are relative and non-escaping
	// - Caps total size at cpConfigFilesMaxBytes (a transport-DoS guard,
	//   not the retired 12 KiB user-data ceiling — config now ships off
	//   user-data via the CP's Secrets-Manager seeding path)
	configFiles, templateAssets, err := collectCPConfigFiles(cfg)
	if err != nil {
		return "", fmt.Errorf("cp provisioner: collect config files: %w", err)
	}

	// Only forward kind for platform workspaces; omitempty hides it for ordinary
	// workspaces so the wire shape is unchanged and older CPs see byte-identical
	// requests.  Ordinary workspaces always have kind="workspace" from the DB
	// COALESCE, so we must explicitly suppress it here (core#2498 truth-up).
	kind := ""
	if cfg.Kind == WorkspaceKindPlatform {
		kind = cfg.Kind
	}

	req := cpProvisionRequest{
		OrgID:           p.orgID,
		WorkspaceID:     cfg.WorkspaceID,
		Runtime:         cfg.Runtime,
		Template:        cfg.Template,
		Tier:            cfg.Tier,
		InstanceType:    cfg.InstanceType,
		DiskGB:          cfg.DiskGB,
		DataPersistence: cfg.DataPersistence,
		Provider:        cfg.Provider,
		Kind:            kind,
		Display:         cfg.Display,
		PlatformURL:     cfg.PlatformURL,
		Env:             env,
		ConfigFiles:     configFiles,
		TemplateAssets:  templateAssets,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("cp provisioner: marshal: %w", err)
	}

	url := p.baseURL + "/cp/workspaces/provision"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("cp provisioner: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	p.provisionAuthHeaders(httpReq)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("cp provisioner: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap body read at 64 KiB — the CP only ever returns small JSON
	// responses; an unbounded read could be weaponized into log-flood
	// DoS by a compromised upstream.
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return "", fmt.Errorf("cp provisioner: read response body: %w", readErr)
	}
	var result cpProvisionResponse
	unmarshalErr := json.Unmarshal(respBody, &result)

	if resp.StatusCode != http.StatusCreated {
		// Prefer the structured {"error":"..."} field. Do NOT fall back
		// to string(respBody) — our logs ingest errors, and an upstream
		// misconfiguration that echoed the Authorization header or
		// request body into the response would leak bearer tokens.
		errMsg := result.Error
		if errMsg == "" {
			errMsg = fmt.Sprintf("<unstructured body, %d bytes>", len(respBody))
		}
		return "", fmt.Errorf("cp provisioner: provision failed (%d): %s", resp.StatusCode, errMsg)
	}

	if unmarshalErr != nil {
		return "", fmt.Errorf("cp provisioner: decode 201 response: %w", unmarshalErr)
	}

	log.Printf("CP provisioner: workspace %s → EC2 instance %s (%s)", cfg.WorkspaceID, result.InstanceID, result.State)
	provlog.Event("provision.ec2_started", map[string]any{
		"workspace_id": cfg.WorkspaceID,
		"instance_id":  result.InstanceID,
		"state":        result.State,
		"tier":         cfg.Tier,
		"runtime":      cfg.Runtime,
	})
	return result.InstanceID, nil
}

// cpConfigFilesMaxBytes bounds the aggregate config bundle this tenant
// ships to the control plane. It is a transport-DoS guard, NOT the old
// EC2-user-data ceiling.
//
// History: this was 12 KiB (12<<10) because the CP embedded the bundle in
// EC2 user-data, which AWS caps at 16 KiB (the cap left ~4 KiB for bootstrap
// overhead). That ceiling failed real customers — the demo-auto SEO Agent's
// config (long SEO system prompt + SERVICES_REPO_WEBSITE + a 12-schedule
// block baked into config.yaml) exceeds 12 KiB, so Start() rejected it
// client-side with "config files exceed 12288 bytes" and the workspace
// could never provision.
//
// Config delivery now goes OFF user-data: the CP stages the bundle to AWS
// Secrets Manager (molecule/workspace/<id>/config) at provision time and the
// workspace fetches it into /configs at boot (mirrors the proven tenant
// bootstrap-secrets pattern). The bundle travels here only inside the JSON
// HTTP request body to the CP, which has no 16 KiB limit. The remaining
// bound exists purely so a buggy/hostile tenant can't stream an unbounded
// body and OOM the CP provision path — set generous (256 KiB) so legitimate
// growth (more schedules, longer prompts, more skills) never re-hits a wall.
const cpConfigFilesMaxBytes = 256 << 10

// cpTemplateAssetsMaxBytes bounds the aggregate TEMPLATE-ASSET payload
// (config.yaml + prompts/* — agent-skills are NO LONGER carried here per
// RFC#2843 #32; they are plugins installed post-online) delivered via the
// generic non-secret asset channel (RFC #2843 #24). This is a SEPARATE bound
// from cpConfigFilesMaxBytes: the config bundle is capped at 256 KiB because
// it rides the Secrets-Manager/user-data transport, while template ASSETS
// ride a non-secret channel. With skills removed the legitimate payload is
// tiny (config.yaml ~8 KiB + prompts ~8 KiB), but we keep a generous 16 MiB
// bound as a pure transport-DoS guard (not a secrets-transport limit) so a
// runaway/malicious fetcher can't stream an unbounded body.
const cpTemplateAssetsMaxBytes = 16 << 20

// isCPTemplateConfigFile restricts which files from a template directory are
// eligible for transport to the control plane. Only config.yaml (the runtime
// entrypoint config) and files under prompts/ (system prompts) are needed;
// shipping arbitrary files (e.g. adapter.py, Dockerfile) is both unnecessary
// and a potential data-exfiltration surface.
func isCPTemplateConfigFile(name string) bool {
	name = filepath.ToSlash(filepath.Clean(name))
	return name == "config.yaml" || strings.HasPrefix(name, "prompts/")
}

func collectCPConfigFiles(cfg WorkspaceConfig) (map[string]string, map[string][]byte, error) {
	files := make(map[string]string)
	assets := make(map[string][]byte)
	total := 0
	totalAssets := 0
	addFile := func(name string, data []byte) error {
		name = filepath.ToSlash(filepath.Clean(name))
		if name == "." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") || strings.Contains(name, "/../") {
			return fmt.Errorf("invalid config file path %q", name)
		}
		total += len(data)
		if total > cpConfigFilesMaxBytes {
			return fmt.Errorf("config files exceed %d bytes", cpConfigFilesMaxBytes)
		}
		files[name] = base64.StdEncoding.EncodeToString(data)
		return nil
	}
	addAsset := func(name string, data []byte) error {
		name = filepath.ToSlash(filepath.Clean(name))
		if name == "." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") || strings.Contains(name, "/../") {
			return fmt.Errorf("invalid template asset path %q", name)
		}
		// Blast-radius guard (RC #11690): a fetcher that returns
		// paths outside the template-asset namespace is either
		// a programming error or an attack. Either way, fail closed.
		// Specifically excluded: agent-skills/* (RFC#2843 #32 — skills
		// are plugins now, installed post-online, NOT carried on this
		// provisioning-time channel; keeping them here re-created the
		// #2831/#32 716 KiB skill-tree-in-provision-payload failure),
		// MEMORY.md, USER.md, CLAUDE.md (curated durable memory —
		// agent-owned state, reconciled by the boot entrypoint, NOT by
		// this collect path), .claude/sessions/ (Claude Code session
		// dir, agent-owned), and any other agent-state path. The
		// PM-flagged in #24_clarify invariant is enforced here in code,
		// not in comments.
		if !IsCPTemplateAssetPath(name) {
			return fmt.Errorf("template asset path %q rejected: not in template-asset allowlist (config.yaml / prompts/* only — agent-skills are plugins now, RFC#2843 #32) — see IsCPTemplateAssetPath", name)
		}
		totalAssets += len(data)
		if totalAssets > cpTemplateAssetsMaxBytes {
			return fmt.Errorf("template assets exceed %d bytes", cpTemplateAssetsMaxBytes)
		}
		assets[name] = append([]byte(nil), data...)
		return nil
	}

	if cfg.TemplatePath != "" {
		// Reject symlinks on the root itself — WalkDir follows symlinks,
		// so a symlink TemplatePath that escapes the intended root directory
		// would bypass the subsequent path-relativization checks below.
		rootInfo, err := os.Lstat(cfg.TemplatePath)
		if err != nil {
			return nil, nil, fmt.Errorf("collectCPConfigFiles: lstat template path: %w", err)
		}
		if rootInfo.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("collectCPConfigFiles: template path must not be a symlink")
		}
		err = filepath.WalkDir(cfg.TemplatePath, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			// Skip symlinks — WalkDir follows them by default, which means
			// a symlink inside the template dir pointing to /etc/passwd
			// would be traversed even though the resulting relative-path
			// check would correctly reject it. Defense-in-depth: don't
			// follow symlinks at all. (OFFSEC-010)
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(cfg.TemplatePath, path)
			if err != nil {
				return err
			}
			if !isCPTemplateConfigFile(rel) {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return addFile(rel, data)
		})
		if err != nil {
			return nil, nil, err
		}
	}
	for name, data := range cfg.ConfigFiles {
		if err := addFile(name, data); err != nil {
			return nil, nil, err
		}
	}

	// RFC #2843 #24 — generic template-asset channel. When
	// cfg.TemplateAssetFetcher is wired (SaaS) and
	// cfg.TemplateIdentity is set, fetch the template's
	// config.yaml + prompts/ via the non-secret asset channel
	// (template repo, Gitea shallow clone per §4.2 transport
	// option (a)) and land them in the SEPARATE TemplateAssets
	// field of the provision request — NOT in ConfigFiles (the
	// SM-bound bundle). The split is load-bearing: ConfigFiles is
	// the bundle the CP stages through AWS Secrets Manager, which
	// is sized + scoped for *secrets* and caps at 256 KiB.
	//
	// agent-skills are NOT fetched here anymore (RFC#2843 #32):
	// skills are plugins, installed post-online via the plugin
	// pipeline (gitea:// resolver reads agent-skills/<skill> from
	// the template repo at install time). The asset payload is
	// therefore tiny (config.yaml + prompts), well under any cap —
	// this is the #32 fix for fresh-seo-agent provision failing
	// closed on a 716 KiB skill tree.
	//
	// The fetch is OPT-IN: nil fetcher = no-op (self-host
	// default; falls through to the local TemplatePath path
	// above). Every key in the fetcher's output is gated by
	// IsCPTemplateAssetPath at the addAsset boundary above —
	// paths outside the template-asset namespace (agent-skills/*,
	// MEMORY.md, CLAUDE.md, .claude/sessions/) abort the provision
	// rather than silently sneaking into the bundle.
	//
	// The fetch is fail-closed: a transport error aborts
	// the provision rather than regressing to stub-mode
	// /configs (the same contract as the persisted-bundle
	// provider in #2831 PIECE 1).
	if cfg.TemplateAssetFetcher != nil && cfg.TemplateIdentity != "" {
		fetchedAssets, fetchErr := cfg.TemplateAssetFetcher.Load(context.Background(), cfg.TemplateIdentity)
		if fetchErr != nil {
			return nil, nil, fmt.Errorf("collectCPConfigFiles: fetch template assets (RFC #2843 #24): %w", fetchErr)
		}
		for name, data := range fetchedAssets {
			if err := addAsset(name, data); err != nil {
				return nil, nil, err
			}
		}
	}
	if len(files) == 0 && len(assets) == 0 {
		return nil, nil, nil
	}
	return files, assets, nil
}

// Stop terminates the workspace's EC2 instance via the control plane.
//
// Looks up the actual EC2 instance_id from the workspaces table before
// calling CP — earlier versions passed workspaceID (a UUID) as the
// instance_id query param, which CP forwarded to EC2 TerminateInstances,
// which rejected with InvalidInstanceID.Malformed (EC2 IDs are i-… not
// UUIDs). The terminate failure then left the workspace's SG attached,
// blocking the next provision with InvalidGroup.Duplicate — a full
// "Save & Restart" crash on SaaS.
func (p *CPProvisioner) Stop(ctx context.Context, workspaceID string) error {
	return p.stopInternal(ctx, workspaceID, false)
}

// StopAndPrune terminates the workspace's compute AND requests that its durable
// data volume (browser profile / cookies / downloads / agent memory) be erased
// (internal#734). Used ONLY by the permanent-delete flow when the user chose to
// erase saved data — NEVER by restart/recreate (which call Stop), so a recreate
// can never trigger a prune. CP enforces this defensively too (the prune is a
// short-grace mark-then-sweep gated on the workspace being genuinely gone).
func (p *CPProvisioner) StopAndPrune(ctx context.Context, workspaceID string) error {
	return p.stopInternal(ctx, workspaceID, true)
}

func (p *CPProvisioner) stopInternal(ctx context.Context, workspaceID string, prune bool) error {
	if p == nil {
		return ErrNoBackend
	}
	instanceID, err := resolveInstanceID(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("cp provisioner: stop: resolve instance_id: %w", err)
	}
	if instanceID == "" {
		// No instance was ever provisioned (or already deprovisioned and
		// the column was cleared). Nothing to terminate — idempotent.
		// Reached even when httpClient is nil since the empty-instance
		// path doesn't need HTTP — symmetric with IsRunning.
		log.Printf("CP provisioner: Stop for %s — no instance_id on file, nothing to do", workspaceID)
		return nil
	}
	if p.httpClient == nil {
		// HTTP wiring missing but we have an instance_id to terminate —
		// can't make the DELETE call. Report ErrNoBackend so the
		// orphan sweeper / shutdown path can branch.
		return ErrNoBackend
	}
	provider, err := resolveProvider(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("cp provisioner: stop: resolve provider: %w", err)
	}

	q := url.Values{}
	q.Set("instance_id", instanceID)
	if provider != "" {
		// #2386: CP Deprovision routes by provider so a non-AWS workspace is
		// torn down by its own backend instead of falling through to the AWS
		// terminate path (which would leak the box).
		q.Set("provider", provider)
	}
	if prune {
		// internal#734: ask CP to erase the data volume on this delete.
		q.Set("prune", "true")
	}
	u := fmt.Sprintf("%s/cp/workspaces/%s?%s", p.baseURL, workspaceID, q.Encode())
	req, err := http.NewRequestWithContext(ctx, "DELETE", u, nil)
	if err != nil {
		return fmt.Errorf("cp provisioner: stop: build request: %w", err)
	}
	p.provisionAuthHeaders(req)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cp provisioner: stop: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// http.Client.Do only returns err on transport failure; a 4xx/5xx
	// response is NOT an error. Without this status check, a CP that
	// returns 5xx (AWS hiccup, missing IAM, transient outage) is read
	// as success, the workspace row is then marked status='removed' by
	// the caller, and the EC2 stays alive forever — there's no DB row
	// left to point at the orphan. This is the leak source documented
	// in workspace_crud.go's #1843 comment ("orphan EC2 on a
	// 0-customer account scenario"); the loud-fail path already exists
	// upstream, this just plumbs it through.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read a bounded slice of the body so the error message gives ops
		// enough to triage without risking a multi-MB log line on a
		// pathological response. 512 bytes covers any sane error envelope.
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 512))
		if readErr != nil {
			return fmt.Errorf("cp provisioner: stop %s: unexpected %d (read body failed: %w)",
				workspaceID, resp.StatusCode, readErr)
		}
		return fmt.Errorf("cp provisioner: stop %s: unexpected %d: %s",
			workspaceID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	provlog.Event("provision.ec2_stopped", map[string]any{
		"workspace_id": workspaceID,
		"instance_id":  instanceID,
	})
	return nil
}

// resolveProvider reads workspaces.compute->>'provider' for the given workspace.
// Returns ("", nil) when the row has no provider or the column is missing —
// callers treat empty as "default provider" (AWS). Exposed as a package var
// so tests can substitute a stub, same pattern as resolveInstanceID.
var resolveProvider = func(ctx context.Context, workspaceID string) (string, error) {
	if db.DB == nil {
		return "", nil
	}
	var provider sql.NullString
	err := db.DB.QueryRowContext(ctx,
		`SELECT compute->>'provider' FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&provider)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if !provider.Valid {
		return "", nil
	}
	return provider.String, nil
}

// resolveInstanceID reads workspaces.instance_id for the given workspace.
// Returns ("", nil) when the row exists but has no instance_id recorded
// (edge case for external workspaces or stale rows). Returns an error
// only on real DB failures, not on missing rows — callers (Stop,
// IsRunning) treat the empty string as "nothing to act on."
//
// Exposed as a package var so tests can substitute a stub without
// standing up a sqlmock just to unblock the Stop/IsRunning code path.
// Production code never reassigns it.
var resolveInstanceID = func(ctx context.Context, workspaceID string) (string, error) {
	if db.DB == nil {
		// Defensive: NewCPProvisioner never runs without db.DB being
		// set in main(). If somehow nil, treat as "no instance" rather
		// than panicking in the Stop/IsRunning path.
		return "", nil
	}
	var instanceID sql.NullString
	err := db.DB.QueryRowContext(ctx,
		`SELECT instance_id FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&instanceID)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if !instanceID.Valid {
		return "", nil
	}
	return instanceID.String, nil
}

// IsRunning checks workspace EC2 instance state via the control plane.
//
// Contract (matches the Docker Provisioner.IsRunning contract —
// critical for a2a_proxy's alive-on-transient-error path):
//
//   - transport error           → (true, error)
//   - non-2xx HTTP response     → (true, error)
//   - JSON decode failure       → (true, error)
//   - 2xx with state!="running" → (false, nil)
//   - 2xx with state=="running" → (true, nil)
//
// Why "true on error": a2a_proxy inspects (running, err) and only
// triggers the restart cascade when running==false. Returning false
// on a transient CP outage would cause every brief CP blip to
// stampede every workspace into a restart storm. Returning true
// with the error preserves the signal for logging while keeping the
// workspace on the alive path.
//
// healthsweep.go takes the mirror stance: `if err != nil { continue }`,
// so it skips uncertain results and never marks a workspace offline
// on transport error regardless of the running bool.
//
// Both callers are happy with (true, err); callers that need the
// previous (false, err) shape must inspect err themselves.
func (p *CPProvisioner) IsRunning(ctx context.Context, workspaceID string) (bool, error) {
	if p == nil {
		return false, ErrNoBackend
	}
	instanceID, err := resolveInstanceID(ctx, workspaceID)
	if err != nil {
		// Treat DB errors the same as transport errors — (true, err) keeps
		// a2a_proxy on the alive path and logs the signal.
		return true, fmt.Errorf("cp provisioner: status: resolve instance_id: %w", err)
	}
	if instanceID == "" {
		// No instance recorded. Report "not running" cleanly (no error)
		// so restart cascades can trigger a fresh provision. This path
		// is reached even on a zero-valued provisioner (no httpClient
		// wired) — that's intentional; the resolveInstanceID lookup
		// goes through the package-level db var, not p.httpClient, so
		// a no-instance workspace gets a clean answer regardless of
		// HTTP wiring state.
		return false, nil
	}
	if p.httpClient == nil {
		// HTTP wiring missing but we have an instance_id to query —
		// can't proceed without a client. Report ErrNoBackend so the
		// caller can branch (a2a_proxy keeps alive, healthsweep skips).
		return false, ErrNoBackend
	}
	provider, err := resolveProvider(ctx, workspaceID)
	if err != nil {
		return true, fmt.Errorf("cp provisioner: status: resolve provider: %w", err)
	}
	q := url.Values{}
	q.Set("instance_id", instanceID)
	if provider != "" {
		// Sibling-leak to #2386: CP status routes by provider so a non-AWS
		// workspace is queried by its own backend instead of falling through
		// to the AWS status path (which would report NOT_FOUND / wrong state).
		q.Set("provider", provider)
	}
	u := fmt.Sprintf("%s/cp/workspaces/%s/status?%s", p.baseURL, workspaceID, q.Encode())
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return true, fmt.Errorf("cp provisioner: status: build request: %w", err)
	}
	p.provisionAuthHeaders(req)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return true, fmt.Errorf("cp provisioner: status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Don't leak the body — upstream errors may echo headers.
		return true, fmt.Errorf("cp provisioner: status: unexpected %d", resp.StatusCode)
	}
	var result struct {
		State string `json:"state"`
	}
	// Cap body read at 64 KiB for parity with Start — a misconfigured
	// or compromised CP streaming a huge body could otherwise exhaust
	// memory in this hot path (called reactively per-request from
	// a2a_proxy and periodically from healthsweep).
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return true, fmt.Errorf("cp provisioner: status decode: %w", err)
	}
	return result.State == "running", nil
}

// GetConsoleOutput proxies a call to the CP's
// GET /cp/admin/workspaces/:id/console endpoint, which returns the EC2
// serial console output (AWS ec2:GetConsoleOutput under the hood) for a
// workspace instance. The tenant platform has no AWS credentials by
// design, so CP is the only party that can read the serial console.
//
// Returns ("", err) on transport or non-2xx — the caller decides what
// to render to the user.
func (p *CPProvisioner) GetConsoleOutput(ctx context.Context, workspaceID string) (string, error) {
	url := fmt.Sprintf("%s/cp/admin/workspaces/%s/console", p.baseURL, workspaceID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("cp provisioner: console: build request: %w", err)
	}
	p.adminAuthHeaders(req)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cp provisioner: console: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("cp provisioner: console: unexpected %d", resp.StatusCode)
	}
	// Cap at 256 KiB — EC2 returns at most 64 KiB of serial console, but
	// allow headroom for CP-side wrapping / metadata.
	var body struct {
		Output string `json:"output"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&body); err != nil {
		return "", fmt.Errorf("cp provisioner: console decode: %w", err)
	}
	return body.Output, nil
}

// Close is a no-op.
func (p *CPProvisioner) Close() error { return nil }
