package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/gin-gonic/gin"
)

const (
	workspaceComputeDiskFloorGB   = 30
	workspaceComputeDiskCeilingGB = 500
	workspaceDisplayMinWidth      = 800
	workspaceDisplayMaxWidth      = 3840
	workspaceDisplayMinHeight     = 600
	workspaceDisplayMaxHeight     = 2160
)

type workspaceDisplayResponse struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Status    string `json:"status,omitempty"`
}

// SSOT for cloud-provider + instance-type metadata (core#2489).
//
// This file is the SINGLE source of truth the workspace-server validates
// against AND the canvas Container-Config tab renders its dropdowns from (via
// GET /workspaces/:id/compute-options, see ComputeOptions below). Previously the
// canvas hardcoded a parallel copy of these lists in ContainerConfigTab.tsx; the
// two could drift so the UI offered a (provider, instance-type) the backend
// allowlist then rejected with a 400. The canvas now derives its options from
// this endpoint, so drift is impossible by construction.
//
// The ordered slices below are the canonical form. workspaceComputeInstanceAllowlist
// (the O(1) validation set) is DERIVED from them in init(), so the ordered list
// the canvas renders and the set the backend validates can never disagree.
//
// Mirrors the CP provider SSOT — keep in lock-step with the controlplane provider
// configs (Hetzner ServerType cpx*/cax*, GCP MachineType e2-*, AWS EC2
// t3*/m6i*/c6i*). TestValidateWorkspaceCompute_Provider / _InstanceTypePerProvider
// pin the sets. "" provider = AWS default.

// workspaceComputeProvidersOrdered is the canonical provider order (AWS first =
// default). The canvas renders the provider dropdown in this order.
var workspaceComputeProvidersOrdered = []string{"aws", "hetzner", "gcp"}

// workspaceComputeInstanceTypesOrdered lists each provider's machine sizes in the
// order the canvas should render them. An AWS t3.* is meaningless on Hetzner, and
// vice-versa, so the set is provider-scoped.
var workspaceComputeInstanceTypesOrdered = map[string][]string{
	"aws": {
		"t3.medium", "t3.large", "t3.xlarge", "t3.2xlarge",
		"m6i.large", "m6i.xlarge", "c6i.xlarge",
	},
	"hetzner": {
		"cpx11", "cpx21", "cpx31", "cpx41", "cpx51",
		"cax11", "cax21", "cax31", "cax41",
	},
	"gcp": {
		"e2-small", "e2-medium",
		"e2-standard-2", "e2-standard-4", "e2-standard-8",
	},
}

// workspaceComputeDefaultInstanceByProvider is the per-provider default machine
// size the canvas pre-selects when switching providers (an AWS t3.* is invalid on
// Hetzner, so the switch resets to the new provider's default).
var workspaceComputeDefaultInstanceByProvider = map[string]string{
	"aws":     "t3.medium",
	"hetzner": "cpx31",
	"gcp":     "e2-standard-2",
}

// workspaceComputeDisplayDefaultByProvider is the per-provider default machine
// size the canvas pre-selects for DISPLAY-mode create flows. Distinct from
// workspaceComputeDefaultInstanceByProvider because display-mode boxes need
// a larger default (t3.xlarge vs t3.medium on AWS) — the create flow's
// display-mode branch codifies the prior hardcoded
// `DEFAULT_DISPLAY_INSTANCE_TYPE = "t3.xlarge"` constant in
// canvas/src/components/CreateWorkspaceDialog.tsx so the SSOT can drive it
// instead of a parallel canvas-side mirror (core#2489 phase-2 enabler).
//
// MUST stay in lock-step with workspaceComputeProvidersOrdered — the SSOT-
// consistency check in init() panics if a provider is added here without
// a default in the parent map (or vice versa). Same bidirectional invariant
// as workspaceComputeProviderLabels / workspaceComputeDefaultInstanceByProvider.
// Pinned by TestComputeMetadata_SSOTInternalConsistency (extended for this
// map in the #2489-A follow-up).
var workspaceComputeDisplayDefaultByProvider = map[string]string{
	"aws":     "t3.xlarge",
	"hetzner": "cpx41",
	"gcp":     "e2-standard-4",
}

// workspaceComputeInstanceAllowlist is the O(1) validation set, keyed by cloud
// provider. DERIVED from workspaceComputeInstanceTypesOrdered in init() so the
// ordered list (what the canvas renders) and the set (what the backend validates)
// stay in lock-step — you cannot add an instance type to one without the other.
var workspaceComputeInstanceAllowlist = map[string]map[string]struct{}{}

func init() {
	for provider, types := range workspaceComputeInstanceTypesOrdered {
		set := make(map[string]struct{}, len(types))
		for _, t := range types {
			set[t] = struct{}{}
		}
		workspaceComputeInstanceAllowlist[provider] = set
	}
}

// normalizeCloudProvider maps "" → "aws" so the in-place switch comparison
// treats the default and an explicit "aws" as the same cloud (no spurious switch).
func normalizeCloudProvider(p string) string {
	if p == "" {
		return "aws"
	}
	return p
}

// instanceTypeAllowedForProvider reports whether instanceType is valid for the
// given provider ("" → aws). Empty instanceType is always allowed (CP defaults).
func instanceTypeAllowedForProvider(provider, instanceType string) bool {
	if instanceType == "" {
		return true
	}
	p := provider
	if p == "" {
		p = "aws"
	}
	set, ok := workspaceComputeInstanceAllowlist[p]
	if !ok {
		return false
	}
	_, ok = set[instanceType]
	return ok
}

// workspaceComputeProviderAllowlist mirrors the controlplane cloud-provider SSOT
// (controlplane internal/cloudprovider.Supported = {aws, hetzner, gcp}).
// ws-server lives in a different repo and cannot import that package, so this is
// a DELIBERATE mirror; TestValidateWorkspaceCompute_Provider pins the exact set
// and this doc-comment names the SSOT, so a CP-side change forces a matching
// change here (and the CP itself fail-closes an unwired provider with a 422).
// "" = default (AWS) and is always accepted. This is the gate the switch-provider
// flow reuses to reject a bad provider with a clean 400 before any CP round-trip.
// DERIVED from workspaceComputeProvidersOrdered (the SSOT, core#2489) in init() so
// the set the backend validates and the ordered list the canvas renders cannot
// drift.
var workspaceComputeProviderAllowlist = map[string]struct{}{}

// workspaceComputeProviderLabels is the human-readable label the canvas
// renders for each provider (e.g. "AWS (default)" vs the raw "aws"). The
// "(default)" suffix on the default-provider label is the canvas's
// visual cue for the auto-selected provider — preserving it here keeps
// the UX signal the canvas already depends on. Computed labels
// (e.g. "(default)") MUST stay aligned with the empty-string convention
// in normalizeCloudProvider; if a future change makes the default
// non-AWS, update the "aws" entry's label at the same time.
//
// DERIVED validation: the keys must match workspaceComputeProvidersOrdered
// (enforced in init()); a label without a provider (or vice-versa) would
// be a real drift bug. Pinned by TestComputeMetadata_SSOTInternalConsistency.
var workspaceComputeProviderLabels = map[string]string{
	"aws":     "AWS (default)",
	"gcp":     "GCP",
	"hetzner": "Hetzner",
}

// workspaceComputeMetadataRenderOrder is the provider order the canvas
// Container-Config tab renders its dropdown in. Distinct from
// workspaceComputeProvidersOrdered (the validation + ComputeOptions
// order) because the canvas UX wants AWS first, then GCP, then
// Hetzner (so the most-used provider is at the top of the dropdown).
// The internal SSOT and the canvas render order are SEPARATE
// concerns on purpose — a future canvas UX change (e.g. alphabetical)
// should not force a re-order of the validation order.
//
// Must contain the same set of providers as
// workspaceComputeProvidersOrdered; pinned by
// TestComputeMetadata_SSOTInternalConsistency.
var workspaceComputeMetadataRenderOrder = []string{"aws", "gcp", "hetzner"}

// checkComputeSSOTConsistency is the bidirectional SSOT consistency
// check (core#2489, Researcher's RC #11736 + CR2's RC #11738). It
// enforces the full invariant between the SSOT data shapes:
//   - labels map keys == providers slice entries
//   - render-order slice is a permutation of providers slice (same
//     set, no duplicates)
//   - every rendered provider has a default + non-empty instance-types
//   - every rendered provider has a display-default (added in #2489-A
//     follow-up so the canvas's CreateWorkspaceDialog display-mode
//     hardcoded t3.xlarge can be REPLACED, not paralleled)
//
// A mismatch in ANY direction panics. The check is extracted as a
// pure function (no side effects, no init() dependency) so the test
// suite can invoke it against MUTATED SSOT data (negative cases:
// missing label, missing render entry, duplicate render entry,
// missing default, missing display-default, empty instance-types)
// and assert the panic behavior. A future regression in the
// production init() — e.g. someone removing the panic for "weird
// but tolerable" cases — would be caught by the negative tests
// calling this function.
//
// Every direction is enforced:
//   - label without a provider: dead data (a future
//     workspaceComputeProvidersOrdered growth would miss the label)
//   - provider without a label: silent empty label in the
//     response (UX dead-end)
//   - render-order entry without a provider: dead data
//   - provider missing from render order: silent omission from
//     the dropdown (the user couldn't switch to that provider)
//   - render-order entry without a default: silent empty
//     default (the canvas would have to fall back to a hardcoded
//     "t3.medium" or fail)
//   - render-order entry without a display-default: silent empty
//     display-default (the canvas's CreateWorkspaceDialog would
//     fall back to a hardcoded "t3.xlarge" — which is the EXACT
//     drift bug #2489 was opened to fix; this panic prevents a
//     future regression where someone adds a new provider but
//     forgets the display-default)
//   - render-order entry with empty instance-types: silent
//     empty dropdown
//   - duplicate render-order entry: render would silently drop
//     one (the second occurrence overwrites the map)
//
// Pinned in lockstep with TestComputeMetadata_SSOTInternalConsistency
// + the negative TestComputeMetadata_InitPanics* family.
func checkComputeSSOTConsistency(
	providers []string,
	labels map[string]string,
	renderOrder []string,
	defaults map[string]string,
	displayDefaults map[string]string,
	instanceTypes map[string][]string,
) {
	ssotSet := make(map[string]struct{}, len(providers))
	for _, p := range providers {
		ssotSet[p] = struct{}{}
	}
	// 1. labels keys ⊆ providers keys AND providers keys ⊆ labels keys
	//    (bidirectional — every provider has a label, every label
	//    has a provider).
	labelsSet := make(map[string]struct{}, len(labels))
	for p := range labels {
		if _, ok := ssotSet[p]; !ok {
			panic(fmt.Sprintf("workspaceComputeProviderLabels has key %q not in workspaceComputeProvidersOrdered", p))
		}
		labelsSet[p] = struct{}{}
	}
	for _, p := range providers {
		if _, ok := labelsSet[p]; !ok {
			panic(fmt.Sprintf("workspaceComputeProvidersOrdered has entry %q with no label in workspaceComputeProviderLabels", p))
		}
	}
	// 2. render-order is a permutation of providers: every entry
	//    has a provider, every provider has an entry, no duplicates.
	renderSet := make(map[string]struct{}, len(renderOrder))
	for _, p := range renderOrder {
		if _, ok := ssotSet[p]; !ok {
			panic(fmt.Sprintf("workspaceComputeMetadataRenderOrder has entry %q not in workspaceComputeProvidersOrdered", p))
		}
		if _, dup := renderSet[p]; dup {
			panic(fmt.Sprintf("workspaceComputeMetadataRenderOrder has duplicate entry %q", p))
		}
		renderSet[p] = struct{}{}
	}
	for _, p := range providers {
		if _, ok := renderSet[p]; !ok {
			panic(fmt.Sprintf("workspaceComputeProvidersOrdered has entry %q missing from workspaceComputeMetadataRenderOrder", p))
		}
	}
	// 3. every rendered provider has a default + a display-default
	//    + non-empty instance-types (the canvas relies on all three;
	//    an empty default falls back to "t3.medium" via the consumer
	//    helper, an empty display-default falls back to "t3.xlarge"
	//    via the CreateWorkspaceDialog hardcoded constant, and a
	//    missing instance-types is a UX dead-end — we want all
	//    three caught at boot, not in the field).
	for _, p := range renderOrder {
		if _, ok := defaults[p]; !ok {
			panic(fmt.Sprintf("workspaceComputeMetadataRenderOrder has entry %q with no default in workspaceComputeDefaultInstanceByProvider", p))
		}
		if _, ok := displayDefaults[p]; !ok {
			panic(fmt.Sprintf("workspaceComputeMetadataRenderOrder has entry %q with no display-default in workspaceComputeDisplayDefaultByProvider (core#2489 phase-2 enabler) — this would silently re-introduce the CreateWorkspaceDialog hardcoded `t3.xlarge` drift bug", p))
		}
		if len(instanceTypes[p]) == 0 {
			panic(fmt.Sprintf("workspaceComputeMetadataRenderOrder has entry %q with empty instance-types list", p))
		}
	}
}

func init() {
	for _, p := range workspaceComputeProvidersOrdered {
		workspaceComputeProviderAllowlist[p] = struct{}{}
	}
	// SSOT consistency check (core#2489, Researcher's RC #11736
	// bidirectional-init fix): the production init guard delegates
	// to the pure checkComputeSSOTConsistency function above so
	// the test suite can exercise the same logic against MUTATED
	// SSOT data (negative cases) and prove the panic behavior.
	// See checkComputeSSOTConsistency's doc-comment for the full
	// rationale + the list of drift bugs each direction prevents.
	checkComputeSSOTConsistency(
		workspaceComputeProvidersOrdered,
		workspaceComputeProviderLabels,
		workspaceComputeMetadataRenderOrder,
		workspaceComputeDefaultInstanceByProvider,
		workspaceComputeDisplayDefaultByProvider,
		workspaceComputeInstanceTypesOrdered,
	)
}

func validateWorkspaceCompute(compute models.WorkspaceCompute) error {
	// Provider first (so the instance-type check below can be provider-scoped).
	// "" = default (AWS). CP fail-closes an unwired provider with a 422; validating
	// here gives a clean 400 before the round-trip and is the gate reused by the
	// switch-provider flow. Mirrors the controlplane cloudprovider SSOT.
	if compute.Provider != "" {
		if _, ok := workspaceComputeProviderAllowlist[compute.Provider]; !ok {
			return fmt.Errorf("unsupported compute.provider (want aws|gcp|hetzner)")
		}
	}
	// Instance type must belong to the chosen provider (an AWS t3.* is invalid on
	// Hetzner, etc.). Empty = CP default for the provider.
	if !instanceTypeAllowedForProvider(compute.Provider, compute.InstanceType) {
		prov := compute.Provider
		if prov == "" {
			prov = "aws"
		}
		return fmt.Errorf("unsupported compute.instance_type %q for provider %q", compute.InstanceType, prov)
	}
	if compute.Volume.RootGB != 0 {
		if compute.Volume.RootGB < workspaceComputeDiskFloorGB || compute.Volume.RootGB > workspaceComputeDiskCeilingGB {
			return fmt.Errorf("compute.volume.root_gb must be between %d and %d", workspaceComputeDiskFloorGB, workspaceComputeDiskCeilingGB)
		}
	}
	switch compute.Display.Mode {
	case "", "none", "desktop-control", "gpu-desktop-control":
	default:
		return fmt.Errorf("unsupported compute.display.mode")
	}
	switch compute.Display.Protocol {
	case "", "dcv", "novnc":
	default:
		return fmt.Errorf("unsupported compute.display.protocol")
	}
	if err := validateWorkspaceDisplayDimensions(compute.Display.Width, compute.Display.Height); err != nil {
		return err
	}
	// internal#734: the durable-data choice. CP re-validates the same enum at
	// its provision edge (IsValidDataPersistence → 400); validating here too
	// gives the user a clear workspace-server error before the CP round-trip.
	switch compute.DataPersistence {
	case "", "persist", "ephemeral":
	default:
		return fmt.Errorf("unsupported compute.data_persistence (want persist|ephemeral)")
	}
	return nil
}

func validateWorkspaceDisplayConfig(display models.WorkspaceComputeDisplay) error {
	switch display.Mode {
	case "", "none", "desktop-control", "gpu-desktop-control":
	default:
		return fmt.Errorf("unsupported compute.display.mode")
	}
	switch display.Protocol {
	case "", "dcv", "novnc":
	default:
		return fmt.Errorf("unsupported compute.display.protocol")
	}
	if err := validateWorkspaceDisplayDimensions(display.Width, display.Height); err != nil {
		return err
	}
	return nil
}

func validateWorkspaceDisplayDimensions(width, height int) error {
	if width < 0 || height < 0 {
		return fmt.Errorf("compute.display width/height must be non-negative")
	}
	if width != 0 && (width < workspaceDisplayMinWidth || width > workspaceDisplayMaxWidth) {
		return fmt.Errorf("compute.display.width must be between %d and %d", workspaceDisplayMinWidth, workspaceDisplayMaxWidth)
	}
	if height != 0 && (height < workspaceDisplayMinHeight || height > workspaceDisplayMaxHeight) {
		return fmt.Errorf("compute.display.height must be between %d and %d", workspaceDisplayMinHeight, workspaceDisplayMaxHeight)
	}
	return nil
}

// ComputeMetadata handles GET /compute/metadata — SSOT for cloud-provider +
// instance-type allowlists consumed by the canvas ContainerConfigTab (and any
// other client that needs to render a provider/instance selector).
// Public, no auth: the data is platform constraints, not org secrets.
//
// DERIVES from the workspaceCompute* SSOT maps above (core#2489); does NOT
// hardcode any provider/instance/default data inline. The previous inline
// hardcoded list drifted in two places: (a) provider order didn't match the
// validation order (aws/gcp/hetzner here vs aws/hetzner/gcp in the SSOT
// slice), and (b) labels weren't defined anywhere — they were inline strings
// that would silently rot if a new provider was added. Both fixes are SSOT
// additions + this derived read, NOT a behavior change (the test
// TestComputeMetadata_ReturnsProviderAllowlist pins the exact previous
// output).
func ComputeMetadata(c *gin.Context) {
	// Render in the canvas-UX order (distinct from the validation
	// order — see workspaceComputeMetadataRenderOrder doc), pulling
	// the label + default + display-default + instance-types for each
	// from the SSOT maps. O(providers) total.
	providers := make([]string, 0, len(workspaceComputeMetadataRenderOrder))
	instanceTypes := make(map[string][]string, len(workspaceComputeMetadataRenderOrder))
	defaults := make(map[string]string, len(workspaceComputeMetadataRenderOrder))
	displayDefaults := make(map[string]string, len(workspaceComputeMetadataRenderOrder))
	for _, id := range workspaceComputeMetadataRenderOrder {
		providers = append(providers, id)
		instanceTypes[id] = workspaceComputeInstanceTypesOrdered[id]
		defaults[id] = workspaceComputeDefaultInstanceByProvider[id]
		displayDefaults[id] = workspaceComputeDisplayDefaultByProvider[id]
	}
	c.JSON(200, workspaceComputeOptionsResponse{
		Providers:       providers,
		InstanceTypes:   instanceTypes,
		Defaults:        defaults,
		DisplayDefaults: displayDefaults,
	})
}

func workspaceComputeIsZero(compute models.WorkspaceCompute) bool {
	return compute.InstanceType == "" &&
		compute.Volume.RootGB == 0 &&
		compute.Display.Mode == "" &&
		compute.Display.Width == 0 &&
		compute.Display.Height == 0 &&
		compute.Display.Protocol == "" &&
		// A provider- or persistence-only compute is NOT zero — it must
		// round-trip so GET returns those fields (canvas provider badge +
		// data-persistence selector both read them back).
		compute.Provider == "" &&
		compute.DataPersistence == ""
}

func workspaceComputeJSON(compute models.WorkspaceCompute) (string, error) {
	if workspaceComputeIsZero(compute) {
		return "{}", nil
	}
	out := map[string]interface{}{}
	if compute.InstanceType != "" {
		out["instance_type"] = compute.InstanceType
	}
	if compute.Volume.RootGB != 0 {
		out["volume"] = map[string]interface{}{"root_gb": compute.Volume.RootGB}
	}
	display := map[string]interface{}{}
	if compute.Display.Mode != "" {
		display["mode"] = compute.Display.Mode
	}
	if compute.Display.Width != 0 {
		display["width"] = compute.Display.Width
	}
	if compute.Display.Height != 0 {
		display["height"] = compute.Display.Height
	}
	if compute.Display.Protocol != "" {
		display["protocol"] = compute.Display.Protocol
	}
	if len(display) > 0 {
		out["display"] = display
	}
	// Cloud/compute provider + durable-data choice. These were FORWARDED to CP
	// at provision time but never serialized back here, so GET /workspaces
	// dropped them — the canvas provider badge always showed the default AWS and
	// the data-persistence selector always showed "auto". Round-trip them (still
	// omit-when-empty, so existing AWS/default rows serialize unchanged).
	if compute.Provider != "" {
		out["provider"] = compute.Provider
	}
	if compute.DataPersistence != "" {
		out["data_persistence"] = compute.DataPersistence
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func withStoredCompute(ctx context.Context, workspaceID string, payload models.CreateWorkspacePayload) models.CreateWorkspacePayload {
	if !workspaceComputeIsZero(payload.Compute) || db.DB == nil {
		return payload
	}
	var raw string
	err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(compute, '{}'::jsonb) FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&raw)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("withStoredCompute: load compute for %s failed: %v", workspaceID, err)
		}
		return payload
	}
	if raw == "" || raw == "{}" {
		return payload
	}
	var compute models.WorkspaceCompute
	if err := json.Unmarshal([]byte(raw), &compute); err != nil {
		log.Printf("withStoredCompute: invalid compute JSON for %s: %v", workspaceID, err)
		return payload
	}
	if err := validateWorkspaceCompute(compute); err != nil {
		log.Printf("withStoredCompute: stored compute for %s failed validation: %v", workspaceID, err)
		return payload
	}
	payload.Compute = compute
	return payload
}

// storedWorkspaceTemplate returns the template a workspace was created from
// (workspaces.template), or "" if none / unavailable. RFC#2843 #33: the
// auto-restart cycle uses this to restore payload.Template on the SaaS
// re-provision so config.yaml + prompts (and the declared-plugin reconcile)
// are re-delivered from the SAME template — instead of re-provisioning with
// template="" which degraded the box to a 218-byte stub and dropped skills.
// Fail-soft: any error (missing column on an un-migrated DB, no row) → "".
func storedWorkspaceTemplate(ctx context.Context, workspaceID string) string {
	if db.DB == nil {
		return ""
	}
	var tmpl string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(template, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&tmpl); err != nil {
		if err != sql.ErrNoRows {
			log.Printf("storedWorkspaceTemplate: load template for %s failed: %v", workspaceID, err)
		}
		return ""
	}
	return strings.TrimSpace(tmpl)
}

// workspaceComputeOptionsResponse is the SSOT payload the canvas Container-Config
// tab consumes to populate its provider + instance-type dropdowns (core#2489).
// It is derived entirely from the allowlist + defaults in this file, so the UI
// can never offer a (provider, instance-type) the backend then rejects.
type workspaceComputeOptionsResponse struct {
	// Providers in canonical render order (AWS first = default).
	Providers []string `json:"providers"`
	// InstanceTypes per provider, in canonical render order.
	InstanceTypes map[string][]string `json:"instanceTypes"`
	// Defaults maps each provider → its default instance type (the canvas
	// pre-selects this when switching providers; headless create flow).
	Defaults map[string]string `json:"defaults"`
	// DisplayDefaults maps each provider → its default instance type for
	// DISPLAY-mode create flows. Distinct from Defaults because display
	// boxes need a larger default (t3.xlarge vs t3.medium on AWS). The
	// canvas's CreateWorkspaceDialog currently hardcodes the display
	// default as t3.xlarge (parallel to this map's value); the canvas
	// migration to consume this field is a follow-up PR (core#2489
	// phase-2). Codified here as the SSOT so the canvas-side constant
	// can be REPLACED (not paralleled) once the canvas PR lands.
	// Same bidirectional SSOT-consistency invariant as Defaults: keys
	// must match the Providers slice (panicked in init() otherwise).
	DisplayDefaults map[string]string `json:"display_defaults"`
}

// buildComputeOptions assembles the SSOT response from the allowlist + defaults.
// Pure (no DB / no gin) so it can be unit-tested directly and reused.
func buildComputeOptions() workspaceComputeOptionsResponse {
	providers := make([]string, len(workspaceComputeProvidersOrdered))
	copy(providers, workspaceComputeProvidersOrdered)

	instanceTypes := make(map[string][]string, len(workspaceComputeInstanceTypesOrdered))
	for _, p := range providers {
		src := workspaceComputeInstanceTypesOrdered[p]
		dst := make([]string, len(src))
		copy(dst, src)
		instanceTypes[p] = dst
	}

	defaults := make(map[string]string, len(workspaceComputeDefaultInstanceByProvider))
	for k, v := range workspaceComputeDefaultInstanceByProvider {
		defaults[k] = v
	}

	displayDefaults := make(map[string]string, len(workspaceComputeDisplayDefaultByProvider))
	for k, v := range workspaceComputeDisplayDefaultByProvider {
		displayDefaults[k] = v
	}

	return workspaceComputeOptionsResponse{
		Providers:       providers,
		InstanceTypes:   instanceTypes,
		Defaults:        defaults,
		DisplayDefaults: displayDefaults,
	}
}

// ComputeOptions handles GET /workspaces/:id/compute-options. It returns the
// cloud-provider + instance-type metadata the canvas Container-Config tab renders
// its dropdowns from — the SAME data validateWorkspaceCompute enforces (core#2489).
// Static (derived from the in-binary allowlist), so it needs no DB round-trip; the
// :id is scoped only by the WorkspaceAuth middleware on the route group.
func (h *WorkspaceHandler) ComputeOptions(c *gin.Context) {
	c.JSON(200, buildComputeOptions())
}

// Display handles GET /workspaces/:id/display.
func (h *WorkspaceHandler) Display(c *gin.Context) {
	workspaceID := c.Param("id")
	var raw, instanceID string
	err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT COALESCE(compute, '{}'::jsonb), COALESCE(instance_id, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&raw, &instanceID)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
		log.Printf("Display: load compute for %s failed: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load display config"})
		return
	}
	var compute models.WorkspaceCompute
	if raw != "" && raw != "{}" {
		if err := json.Unmarshal([]byte(raw), &compute); err != nil {
			log.Printf("Display: invalid compute JSON for %s: %v", workspaceID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid display config"})
			return
		}
		if err := validateWorkspaceDisplayConfig(compute.Display); err != nil {
			log.Printf("Display: invalid stored compute for %s: %v", workspaceID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid display config"})
			return
		}
	}
	if compute.Display.Mode == "" || compute.Display.Mode == "none" {
		c.JSON(http.StatusOK, workspaceDisplayResponse{
			Available: false,
			Reason:    "display_not_enabled",
		})
		return
	}
	if instanceID != "" {
		c.JSON(http.StatusOK, workspaceDisplayResponse{
			Available: true,
			Mode:      compute.Display.Mode,
			Protocol:  compute.Display.Protocol,
			Width:     compute.Display.Width,
			Height:    compute.Display.Height,
			Status:    "ready",
		})
		return
	}
	c.JSON(http.StatusOK, workspaceDisplayResponse{
		Available: false,
		Reason:    "display_session_unavailable",
		Mode:      compute.Display.Mode,
		Protocol:  compute.Display.Protocol,
		Width:     compute.Display.Width,
		Height:    compute.Display.Height,
		Status:    "not_configured",
	})
}
