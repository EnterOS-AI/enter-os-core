package provisioner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
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
	GetConsoleOutput(ctx context.Context, workspaceID string) (string, error)
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
	OrgID       string            `json:"org_id"`
	WorkspaceID string            `json:"workspace_id"`
	Runtime     string            `json:"runtime"`
	Tier        int               `json:"tier"`
	PlatformURL string            `json:"platform_url"`
	Env         map[string]string `json:"env"`
}

type cpProvisionResponse struct {
	InstanceID string `json:"instance_id"`
	PrivateIP  string `json:"private_ip"`
	State      string `json:"state"`
	Error      string `json:"error"`
}

// Start provisions a workspace by calling the control plane → EC2.
func (p *CPProvisioner) Start(ctx context.Context, cfg WorkspaceConfig) (string, error) {
	req := cpProvisionRequest{
		OrgID:       p.orgID,
		WorkspaceID: cfg.WorkspaceID,
		Runtime:     cfg.Runtime,
		Tier:        cfg.Tier,
		PlatformURL: cfg.PlatformURL,
		Env:         cfg.EnvVars,
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
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var result cpProvisionResponse
	json.Unmarshal(respBody, &result)

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

	log.Printf("CP provisioner: workspace %s → EC2 instance %s (%s)", cfg.WorkspaceID, result.InstanceID, result.State)
	return result.InstanceID, nil
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
	url := fmt.Sprintf("%s/cp/workspaces/%s?instance_id=%s", p.baseURL, workspaceID, instanceID)
	req, _ := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	p.provisionAuthHeaders(req)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cp provisioner: stop: %w", err)
	}
	_ = resp.Body.Close()
	return nil
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
	url := fmt.Sprintf("%s/cp/workspaces/%s/status?instance_id=%s", p.baseURL, workspaceID, instanceID)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
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
	var result struct{ State string `json:"state"` }
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
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
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
