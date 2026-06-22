package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func TestValidateWorkspaceCompute_AcceptsPhase1SizingAndDisplayNone(t *testing.T) {
	compute := models.WorkspaceCompute{
		InstanceType: "m6i.xlarge",
		Volume:       models.WorkspaceComputeVolume{RootGB: 100},
		Display:      models.WorkspaceComputeDisplay{Mode: "none"},
	}

	if err := validateWorkspaceCompute(compute); err != nil {
		t.Fatalf("validateWorkspaceCompute returned error for valid compute: %v", err)
	}
}

func TestValidateWorkspaceCompute_RejectsUnknownInstanceType(t *testing.T) {
	compute := models.WorkspaceCompute{InstanceType: "p4d.24xlarge"}

	if err := validateWorkspaceCompute(compute); err == nil {
		t.Fatal("validateWorkspaceCompute accepted unsupported instance type")
	}
}

// Multi-provider: compute.provider must be "" (default AWS) or one of the wired
// cloud backends. Pins the allowlist to the controlplane cloudprovider SSOT
// (Supported = {aws, hetzner, gcp}); if the SSOT changes, update both sides.
func TestValidateWorkspaceCompute_Provider(t *testing.T) {
	for _, ok := range []string{"", "aws", "gcp", "hetzner"} {
		c := models.WorkspaceCompute{Provider: ok}
		if err := validateWorkspaceCompute(c); err != nil {
			t.Errorf("provider=%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"AWS", "azure", "digitalocean", "ec2", "google", "hetzner-cloud"} {
		c := models.WorkspaceCompute{Provider: bad}
		if err := validateWorkspaceCompute(c); err == nil {
			t.Errorf("provider=%q must be rejected", bad)
		}
	}
	// Pin the exact SSOT-mirrored set so a silent drift fails here.
	want := map[string]struct{}{"aws": {}, "gcp": {}, "hetzner": {}}
	if len(workspaceComputeProviderAllowlist) != len(want) {
		t.Fatalf("provider allowlist drifted from SSOT {aws,gcp,hetzner}: %v", workspaceComputeProviderAllowlist)
	}
	for p := range want {
		if _, ok := workspaceComputeProviderAllowlist[p]; !ok {
			t.Fatalf("provider allowlist missing %q (SSOT drift)", p)
		}
	}
}

// Multi-provider / in-place switch: an instance type must belong to the chosen
// provider — an AWS t3.* is meaningless on Hetzner, a cpx* on AWS, etc. Pins the
// provider-keyed allowlist (mirrors the CP provider configs).
func TestValidateWorkspaceCompute_InstanceTypePerProvider(t *testing.T) {
	good := []struct{ provider, instance string }{
		{"", "t3.medium"}, {"aws", "t3.2xlarge"}, {"aws", "c6i.xlarge"},
		{"hetzner", "cpx31"}, {"hetzner", "cax41"},
		{"gcp", "e2-standard-2"}, {"gcp", "e2-small"},
		{"hetzner", ""}, {"gcp", ""}, // empty instance = CP default, always ok
	}
	for _, g := range good {
		c := models.WorkspaceCompute{Provider: g.provider, InstanceType: g.instance}
		if err := validateWorkspaceCompute(c); err != nil {
			t.Errorf("provider=%q instance=%q must be accepted: %v", g.provider, g.instance, err)
		}
	}
	bad := []struct{ provider, instance string }{
		{"hetzner", "t3.medium"}, // AWS type on Hetzner
		{"aws", "cpx31"},         // Hetzner type on AWS
		{"gcp", "t3.large"},      // AWS type on GCP
		{"hetzner", "e2-small"},  // GCP type on Hetzner
		{"", "cpx31"},            // default(aws) + Hetzner type
	}
	for _, b := range bad {
		c := models.WorkspaceCompute{Provider: b.provider, InstanceType: b.instance}
		if err := validateWorkspaceCompute(c); err == nil {
			t.Errorf("provider=%q instance=%q must be rejected (cross-provider instance type)", b.provider, b.instance)
		}
	}
	if normalizeCloudProvider("") != "aws" || normalizeCloudProvider("hetzner") != "hetzner" {
		t.Fatal("normalizeCloudProvider: \"\" must map to aws; explicit providers unchanged")
	}
}

// internal#734: data_persistence enum. "" (auto), "persist", "ephemeral" are
// the only accepted values; anything else is a clear 400 before the CP call.
func TestValidateWorkspaceCompute_DataPersistence(t *testing.T) {
	for _, ok := range []string{"", "persist", "ephemeral"} {
		c := models.WorkspaceCompute{DataPersistence: ok}
		if err := validateWorkspaceCompute(c); err != nil {
			t.Errorf("data_persistence=%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"persistent", "off", "none", "Ephemeral", "true"} {
		c := models.WorkspaceCompute{DataPersistence: bad}
		if err := validateWorkspaceCompute(c); err == nil {
			t.Errorf("data_persistence=%q must be rejected", bad)
		}
	}
}

func TestValidateWorkspaceCompute_RejectsOutOfRangeRootVolume(t *testing.T) {
	for _, rootGB := range []int{29, 501} {
		compute := models.WorkspaceCompute{Volume: models.WorkspaceComputeVolume{RootGB: rootGB}}
		if err := validateWorkspaceCompute(compute); err == nil {
			t.Fatalf("validateWorkspaceCompute accepted root_gb=%d", rootGB)
		}
	}
}

func TestValidateWorkspaceCompute_RejectsOutOfRangeDisplayDimensions(t *testing.T) {
	for _, display := range []models.WorkspaceComputeDisplay{
		{Mode: "desktop-control", Protocol: "novnc", Width: 799, Height: 1080},
		{Mode: "desktop-control", Protocol: "novnc", Width: 3841, Height: 1080},
		{Mode: "desktop-control", Protocol: "novnc", Width: 1920, Height: 599},
		{Mode: "desktop-control", Protocol: "novnc", Width: 1920, Height: 2161},
	} {
		compute := models.WorkspaceCompute{Display: display}
		if err := validateWorkspaceCompute(compute); err == nil {
			t.Fatalf("validateWorkspaceCompute accepted display size %dx%d", display.Width, display.Height)
		}
	}
}

func TestWorkspaceComputeJSON_OmitsEmptyNestedSections(t *testing.T) {
	got, err := workspaceComputeJSON(models.WorkspaceCompute{
		InstanceType: "m6i.xlarge",
		Volume:       models.WorkspaceComputeVolume{RootGB: 100},
	})
	if err != nil {
		t.Fatalf("workspaceComputeJSON returned error: %v", err)
	}

	if strings.Contains(got, `"display"`) {
		t.Fatalf("workspaceComputeJSON included empty display section: %s", got)
	}
	if got != `{"instance_type":"m6i.xlarge","volume":{"root_gb":100}}` {
		t.Fatalf("workspaceComputeJSON = %s", got)
	}
}

// Regression: provider + data_persistence were FORWARDED to CP but dropped from
// the serialized compute, so GET /workspaces never returned them (the canvas
// provider badge always showed AWS, the persistence selector always "auto").
func TestWorkspaceComputeJSON_RoundTripsProviderAndDataPersistence(t *testing.T) {
	got, err := workspaceComputeJSON(models.WorkspaceCompute{
		InstanceType:    "t3.medium",
		Provider:        "gcp",
		DataPersistence: "persist",
	})
	if err != nil {
		t.Fatalf("workspaceComputeJSON returned error: %v", err)
	}
	if !strings.Contains(got, `"provider":"gcp"`) {
		t.Fatalf("workspaceComputeJSON dropped provider: %s", got)
	}
	if !strings.Contains(got, `"data_persistence":"persist"`) {
		t.Fatalf("workspaceComputeJSON dropped data_persistence: %s", got)
	}
}

// A provider-only compute must NOT be treated as zero (else it serializes to
// "{}" and the cloud is lost).
func TestWorkspaceComputeJSON_ProviderOnlyIsNotZero(t *testing.T) {
	got, err := workspaceComputeJSON(models.WorkspaceCompute{Provider: "hetzner"})
	if err != nil {
		t.Fatalf("workspaceComputeJSON returned error: %v", err)
	}
	if got == "{}" || !strings.Contains(got, `"provider":"hetzner"`) {
		t.Fatalf("provider-only compute serialized as zero: %s", got)
	}
}

func TestWorkspaceCreate_WithCompute_PersistsComputeJSON(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workspaces SET compute = \$2::jsonb`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{
		"name":"Sized Agent",
		"external":true,
		"runtime":"external",
		"compute":{
			"instance_type":"m6i.xlarge",
			"volume":{"root_gb":100},
			"display":{"mode":"none"}
		}
	}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceCreate_WithInvalidCompute_ReturnsBadRequest(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{
		"name":"Oversized Agent",
		"model":"claude-opus-4-7",
		"compute":{"instance_type":"p4d.24xlarge"}
	}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceUpdate_WithCompute_PersistsComputeJSONAndRequiresRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	wsID := "00000000-0000-0000-0000-000000000123"

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET compute = \$2::jsonb, updated_at = now\(\) WHERE id = \$1`).
		WithArgs(wsID, `{"display":{"height":1080,"mode":"desktop-control","protocol":"novnc","width":1920},"instance_type":"t3.xlarge","volume":{"root_gb":80}}`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	body := `{
		"compute":{
			"instance_type":"t3.xlarge",
			"volume":{"root_gb":80},
			"display":{"mode":"desktop-control","protocol":"novnc","width":1920,"height":1080}
		}
	}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["needs_restart"] != true {
		t.Fatalf("needs_restart = %v, want true", resp["needs_restart"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceUpdate_WithInvalidCompute_ReturnsBadRequest(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	wsID := "00000000-0000-0000-0000-000000000124"

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	body := `{"compute":{"instance_type":"p4d.24xlarge"}}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBuildProvisionerConfig_CopiesComputeSizingFromPayload(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT COALESCE\(workspace_dir`).
		WithArgs("ws-compute").
		WillReturnRows(sqlmock.NewRows([]string{"workspace_dir", "workspace_access"}).AddRow("", "none"))

	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	cfg := handler.buildProvisionerConfig(
		context.Background(),
		"ws-compute",
		"",
		nil,
		models.CreateWorkspacePayload{
			Tier:    4,
			Runtime: "claude-code",
			Compute: models.WorkspaceCompute{
				InstanceType: "m6i.xlarge",
				Volume:       models.WorkspaceComputeVolume{RootGB: 100},
				Display:      models.WorkspaceComputeDisplay{Mode: "desktop-control", Protocol: "novnc", Width: 1920, Height: 1080},
			},
		},
		nil,
		nil,
		t.TempDir(),
	)

	if cfg.InstanceType != "m6i.xlarge" {
		t.Errorf("cfg.InstanceType = %q, want m6i.xlarge", cfg.InstanceType)
	}
	if cfg.DiskGB != 100 {
		t.Errorf("cfg.DiskGB = %d, want 100", cfg.DiskGB)
	}
	if cfg.Display.Mode != "desktop-control" || cfg.Display.Protocol != "novnc" {
		t.Errorf("cfg.Display mode/protocol = %q/%q, want desktop-control/novnc", cfg.Display.Mode, cfg.Display.Protocol)
	}
	if cfg.Display.Width != 1920 || cfg.Display.Height != 1080 {
		t.Errorf("cfg.Display size = %dx%d, want 1920x1080", cfg.Display.Width, cfg.Display.Height)
	}
}

func TestWithStoredCompute_LoadsComputeForRestartPayloads(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-restart-compute").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"instance_type":"m6i.xlarge","volume":{"root_gb":100}}`))

	payload := models.CreateWorkspacePayload{Name: "Restart Me", Tier: 4, Runtime: "claude-code"}
	got := withStoredCompute(context.Background(), "ws-restart-compute", payload)

	if got.Compute.InstanceType != "m6i.xlarge" {
		t.Errorf("stored compute instance_type = %q, want m6i.xlarge", got.Compute.InstanceType)
	}
	if got.Compute.Volume.RootGB != 100 {
		t.Errorf("stored compute root_gb = %d, want 100", got.Compute.Volume.RootGB)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// core#2489: the allowlist (validation set) MUST be derived from the ordered
// lists the canvas renders, so the UI and the backend can never disagree about
// which (provider, instance-type) pairs are valid. This pins that the derived
// set exactly matches the ordered source — adding to one without the other fails.
func TestComputeOptions_AllowlistDerivedFromOrderedSSOT(t *testing.T) {
	// Every ordered instance type is in the validation set (and vice-versa).
	for provider, types := range workspaceComputeInstanceTypesOrdered {
		set, ok := workspaceComputeInstanceAllowlist[provider]
		if !ok {
			t.Fatalf("allowlist missing provider %q present in ordered SSOT", provider)
		}
		if len(set) != len(types) {
			t.Fatalf("provider %q: ordered list (%d) and allowlist set (%d) drifted", provider, len(types), len(set))
		}
		for _, it := range types {
			if _, ok := set[it]; !ok {
				t.Fatalf("provider %q: ordered instance %q missing from validation allowlist", provider, it)
			}
		}
	}
	// No extra providers in the set that aren't in the ordered list.
	if len(workspaceComputeInstanceAllowlist) != len(workspaceComputeInstanceTypesOrdered) {
		t.Fatalf("allowlist has providers not present in the ordered SSOT")
	}
	// Provider allowlist derived from the ordered providers.
	if len(workspaceComputeProviderAllowlist) != len(workspaceComputeProvidersOrdered) {
		t.Fatalf("provider allowlist (%d) drifted from ordered providers (%d)", len(workspaceComputeProviderAllowlist), len(workspaceComputeProvidersOrdered))
	}
	for _, p := range workspaceComputeProvidersOrdered {
		if _, ok := workspaceComputeProviderAllowlist[p]; !ok {
			t.Fatalf("provider allowlist missing ordered provider %q", p)
		}
	}
}

// core#2489: the per-provider defaults the canvas pre-selects on a provider switch
// MUST themselves be valid instance types for that provider — otherwise the switch
// produces a PATCH the backend immediately rejects.
func TestComputeOptions_DefaultsAreValidForTheirProvider(t *testing.T) {
	for provider, def := range workspaceComputeDefaultInstanceByProvider {
		if !instanceTypeAllowedForProvider(provider, def) {
			t.Errorf("default instance %q for provider %q is not in that provider's allowlist", def, provider)
		}
	}
	// Every provider must have a default (so the switch never lands on "").
	for _, p := range workspaceComputeProvidersOrdered {
		if workspaceComputeDefaultInstanceByProvider[p] == "" {
			t.Errorf("provider %q has no default instance type", p)
		}
	}
}

// TestComputeOptions_DisplayDefaultsAreValidForTheirProvider is
// the core#2489-A phase-2 enabler: the new
// workspaceComputeDisplayDefaultByProvider map (the per-provider
// display-mode default — distinct from the headless default
// because display boxes need a larger default like t3.xlarge)
// MUST satisfy the same allowlist + coverage invariant as the
// existing defaults map. Codifying the prior hardcoded
// `DEFAULT_DISPLAY_INSTANCE_TYPE = "t3.xlarge"` canvas
// constant into the SSOT means the SSOT now drives the
// canvas's display-mode default — the canvas PR that
// REPLACES the hardcoded constant can rely on this test as
// the contract pin.
func TestComputeOptions_DisplayDefaultsAreValidForTheirProvider(t *testing.T) {
	for provider, def := range workspaceComputeDisplayDefaultByProvider {
		if !instanceTypeAllowedForProvider(provider, def) {
			t.Errorf("display-default instance %q for provider %q is not in that provider's allowlist (a bad display-default would silently break the canvas's create flow)", def, provider)
		}
	}
	// Every provider must have a display-default (so the create
	// flow's display-mode branch never lands on "" — would
	// silently fall back to the canvas's hardcoded t3.xlarge,
	// defeating the SSOT).
	for _, p := range workspaceComputeProvidersOrdered {
		if workspaceComputeDisplayDefaultByProvider[p] == "" {
			t.Errorf("provider %q has no display-default instance type (the canvas's CreateWorkspaceDialog would silently fall back to the hardcoded t3.xlarge — exactly the #2489 drift we're consolidating)", p)
		}
	}
}

// core#2489: the GET /compute-options endpoint returns exactly the SSOT data the
// canvas renders dropdowns from. Every (provider, instance-type) it advertises
// MUST pass validateWorkspaceCompute — the whole point of the consolidation.
func TestWorkspaceComputeOptions_ReturnsSSOTAndEveryOptionValidates(t *testing.T) {
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-opts"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-opts/compute-options", nil)

	handler.ComputeOptions(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp workspaceComputeOptionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse compute-options response: %v", err)
	}

	// AWS first (default) in the provider order.
	if len(resp.Providers) == 0 || resp.Providers[0] != "aws" {
		t.Fatalf("providers = %v, want aws first", resp.Providers)
	}
	// Every advertised (provider, instance-type) must pass backend validation.
	for _, provider := range resp.Providers {
		types, ok := resp.InstanceTypes[provider]
		if !ok || len(types) == 0 {
			t.Fatalf("compute-options advertised provider %q with no instance types", provider)
		}
		for _, it := range types {
			if !instanceTypeAllowedForProvider(provider, it) {
				t.Errorf("compute-options advertised %q/%q which the backend rejects (DRIFT)", provider, it)
			}
		}
		def := resp.Defaults[provider]
		if def == "" {
			t.Errorf("compute-options missing default for provider %q", provider)
		} else if !instanceTypeAllowedForProvider(provider, def) {
			t.Errorf("compute-options default %q for %q fails backend validation", def, provider)
		}
	}
}

func TestWorkspaceDisplay_NonDisplayWorkspaceReturnsUnavailable(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-no-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{}`, ""))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-no-display"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-no-display/display", nil)

	handler.Display(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse display response: %v", err)
	}
	if resp["available"] != false {
		t.Fatalf("available = %v, want false", resp["available"])
	}
	if resp["reason"] != "display_not_enabled" {
		t.Fatalf("reason = %v, want display_not_enabled", resp["reason"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplay_DisplayConfiguredReturnsSessionUnavailableContract(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"novnc","width":1920,"height":1080}}`, ""))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-display/display", nil)

	handler.Display(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse display response: %v", err)
	}
	if resp["available"] != false {
		t.Fatalf("available = %v, want false", resp["available"])
	}
	if resp["reason"] != "display_session_unavailable" {
		t.Fatalf("reason = %v, want display_session_unavailable", resp["reason"])
	}
	if resp["status"] != "not_configured" {
		t.Fatalf("status = %v, want not_configured", resp["status"])
	}
	if resp["mode"] != "desktop-control" || resp["protocol"] != "novnc" {
		t.Fatalf("mode/protocol = %v/%v, want desktop-control/novnc", resp["mode"], resp["protocol"])
	}
	if resp["width"] != float64(1920) || resp["height"] != float64(1080) {
		t.Fatalf("width/height = %v/%v, want 1920/1080", resp["width"], resp["height"])
	}
	if _, ok := resp["url"]; ok {
		t.Fatalf("display response exposed url before session infra exists: %v", resp["url"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplay_DisplayConfiguredWithInstanceReturnsAvailableSession(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"novnc","width":1920,"height":1080}}`, "i-display123"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-display/display", nil)

	handler.Display(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse display response: %v", err)
	}
	if resp["available"] != true {
		t.Fatalf("available = %v, want true", resp["available"])
	}
	if resp["viewer_url"] != nil {
		t.Fatalf("viewer_url = %v, want omitted; stream URL is minted by Take control", resp["viewer_url"])
	}
	if resp["reason"] != nil {
		t.Fatalf("reason = %v, want omitted", resp["reason"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplay_DisplayConfiguredWithoutInstanceReturnsUnavailable(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	workspaceID := "ws-display"
	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"novnc","width":1920,"height":1080}}`, ""))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+workspaceID+"/display", nil)

	handler.Display(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse display response: %v", err)
	}
	if resp["available"] != false {
		t.Fatalf("available = %v, want false", resp["available"])
	}
	if resp["viewer_url"] != nil {
		t.Fatalf("viewer_url = %v, want omitted for invalid viewer base", resp["viewer_url"])
	}
	if resp["reason"] != "display_session_unavailable" {
		t.Fatalf("reason = %v, want display_session_unavailable", resp["reason"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplay_IgnoresUnrelatedStoredComputeSizingDrift(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display-sizing-drift").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{"instance_type":"old.large","display":{"mode":"desktop-control","protocol":"novnc","width":1920,"height":1080}}`, ""))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display-sizing-drift"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-display-sizing-drift/display", nil)

	handler.Display(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse display response: %v", err)
	}
	if resp["reason"] != "display_session_unavailable" {
		t.Fatalf("reason = %v, want display_session_unavailable", resp["reason"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplay_InvalidStoredDisplayConfigReturnsServerError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-invalid-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"vnc"}}`, ""))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-invalid-display"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-invalid-display/display", nil)

	handler.Display(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse display response: %v", err)
	}
	if resp["error"] != "invalid display config" {
		t.Fatalf("error = %v, want invalid display config", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplaySession_ProxiesThroughDisplayForward(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "display-session-test-secret")
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	var upstreamAuth, upstreamCookie, upstreamProtocol, gotInstanceID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/websockify" {
			t.Errorf("upstream path = %q, want /websockify", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("upstream raw query = %q, want stripped", r.URL.RawQuery)
		}
		upstreamAuth = r.Header.Get("Authorization")
		upstreamCookie = r.Header.Get("Cookie")
		upstreamProtocol = r.Header.Get("Sec-WebSocket-Protocol")
		_, _ = w.Write([]byte("websockify"))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	prevForward := displayForward
	displayForward = func(_ context.Context, instanceID string, fn func(target *url.URL) error) error {
		gotInstanceID = instanceID
		return fn(upstreamURL)
	}
	t.Cleanup(func() { displayForward = prevForward })

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(
			`{"display":{"mode":"desktop-control","protocol":"novnc","width":1920,"height":1080}}`,
			"i-display123",
		))
	expiresAt := time.Now().Add(5 * time.Minute).UTC()
	mock.ExpectQuery(`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = \$1 AND expires_at > now\(\)`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"controller", "controlled_by", "expires_at"}).AddRow("user", "admin-token", expiresAt))
	token := signDisplaySessionToken("ws-display", "admin-token", expiresAt)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-display"},
		{Key: "proxyPath", Value: "/websockify"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-display/display/session/websockify", nil)
	c.Request.Header.Set("Authorization", "Bearer should-not-reach-upstream")
	c.Request.Header.Set("Cookie", "session=should-not-reach-upstream")
	c.Request.Header.Set("Sec-WebSocket-Protocol", "binary, molecule-display-token."+token)

	handler.DisplaySession(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotInstanceID != "i-display123" {
		t.Fatalf("displayForward instanceID = %q, want i-display123", gotInstanceID)
	}
	if w.Body.String() != "websockify" {
		t.Fatalf("body = %q, want websockify", w.Body.String())
	}
	if upstreamAuth != "" || upstreamCookie != "" {
		t.Fatalf("proxied credentials leaked upstream: auth=%q cookie=%q", upstreamAuth, upstreamCookie)
	}
	if upstreamProtocol != "binary" {
		t.Fatalf("upstream websocket protocol = %q, want binary without display token", upstreamProtocol)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplaySession_NonDisplayWorkspaceDoesNotProxy(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	prevForward := displayForward
	displayForward = func(_ context.Context, _ string, _ func(target *url.URL) error) error {
		t.Fatal("displayForward must not run for non-display workspaces")
		return nil
	}
	t.Cleanup(func() { displayForward = prevForward })

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-no-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{}`, "i-display123"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-no-display"},
		{Key: "proxyPath", Value: "/websockify"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-no-display/display/session/websockify", nil)

	handler.DisplaySession(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestComputeMetadata_ReturnsProviderAllowlist(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/compute/metadata", nil)

	ComputeMetadata(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp workspaceComputeOptionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp.Providers) != 3 {
		t.Fatalf("expected 3 providers, got %d", len(resp.Providers))
	}
	want := []struct {
		id              string
		defaultInstance string
		displayDefault  string
		instanceCount   int
	}{
		{"aws", "t3.medium", "t3.xlarge", 7},
		{"gcp", "e2-standard-2", "e2-standard-4", 5},
		{"hetzner", "cpx31", "cpx41", 9},
	}
	for i, w := range want {
		p := resp.Providers[i]
		if p != w.id {
			t.Errorf("providers[%d] = %q, want %q", i, p, w.id)
		}
		if got := resp.Defaults[p]; got != w.defaultInstance {
			t.Errorf("defaults[%q] = %q, want %q", p, got, w.defaultInstance)
		}
		if got := resp.DisplayDefaults[p]; got != w.displayDefault {
			t.Errorf("display_defaults[%q] = %q, want %q", p, got, w.displayDefault)
		}
		if got := len(resp.InstanceTypes[p]); got != w.instanceCount {
			t.Errorf("instanceTypes[%q] len = %d, want %d", p, got, w.instanceCount)
		}
	}
}

// TestComputeOptions_ResponseIncludesDisplayDefaults pins the
// core#2489-A phase-2 enabler: the /compute/metadata response
// (or buildComputeOptions() directly) must include the new
// `display_defaults` field so the canvas's CreateWorkspaceDialog
// (follow-up PR) can REPLACE the hardcoded
// `DEFAULT_DISPLAY_INSTANCE_TYPE = "t3.xlarge"` constant.
// Codifies the SSOT contract for the display-mode create flow.
func TestComputeOptions_ResponseIncludesDisplayDefaults(t *testing.T) {
	resp := buildComputeOptions()
	if len(resp.DisplayDefaults) == 0 {
		t.Fatal("buildComputeOptions() returned empty DisplayDefaults; the core#2489 phase-2 enabler is missing")
	}
	wantDefaults := map[string]string{
		"aws":     "t3.xlarge",
		"hetzner": "cpx41",
		"gcp":     "e2-standard-4",
	}
	for provider, want := range wantDefaults {
		got, ok := resp.DisplayDefaults[provider]
		if !ok {
			t.Errorf("buildComputeOptions().DisplayDefaults missing provider %q (was: %v)", provider, resp.DisplayDefaults)
			continue
		}
		if got != want {
			t.Errorf("buildComputeOptions().DisplayDefaults[%q] = %q, want %q (matches the canvas's prior hardcoded t3.xlarge constant)", provider, got, want)
		}
	}
	// Every Providers entry must have a DisplayDefaults entry
	// (the SSOT-consistency check enforces this at boot, but a
	// test pin makes the contract greppable).
	for _, p := range resp.Providers {
		if _, ok := resp.DisplayDefaults[p]; !ok {
			t.Errorf("buildComputeOptions() has provider %q with no DisplayDefaults entry", p)
		}
	}
}

// TestComputeMetadata_SSOTInternalConsistency pins that the SSOT
// additions (workspaceComputeProviderLabels, workspaceComputeMetadataRenderOrder)
// are kept in lock-step with the existing SSOT maps
// (workspaceComputeProvidersOrdered, workspaceComputeInstanceTypesOrdered,
// workspaceComputeDefaultInstanceByProvider). A label without a provider
// is a UX dead-end; a render-order entry without a label is a render
// bug; a default/instance-types map without a render-order entry is a
// silent missing-option. The init() panic catches these at boot
// (defense in depth), but this test is the readable contract pin.
//
// Behavior-preserving: the EXISTING TestComputeMetadata_ReturnsProviderAllowlist
// already pins the output shape; this test pins the SSOT internal
// relationships that prevent the output from drifting.
func TestComputeMetadata_SSOTInternalConsistency(t *testing.T) {
	// Labels map keys must match the provider set.
	ssotSet := make(map[string]struct{})
	for _, p := range workspaceComputeProvidersOrdered {
		ssotSet[p] = struct{}{}
	}
	for p := range workspaceComputeProviderLabels {
		if _, ok := ssotSet[p]; !ok {
			t.Errorf("workspaceComputeProviderLabels has key %q not in workspaceComputeProvidersOrdered", p)
		}
	}
	// Every provider in the SSOT must have a label.
	for _, p := range workspaceComputeProvidersOrdered {
		if _, ok := workspaceComputeProviderLabels[p]; !ok {
			t.Errorf("workspaceComputeProvidersOrdered has entry %q with no label in workspaceComputeProviderLabels", p)
		}
	}
	// Render-order slice must be a permutation of the provider set.
	if len(workspaceComputeMetadataRenderOrder) != len(workspaceComputeProvidersOrdered) {
		t.Errorf("workspaceComputeMetadataRenderOrder has %d entries, want %d (one per provider)",
			len(workspaceComputeMetadataRenderOrder), len(workspaceComputeProvidersOrdered))
	}
	renderSet := make(map[string]struct{}, len(workspaceComputeMetadataRenderOrder))
	for _, p := range workspaceComputeMetadataRenderOrder {
		renderSet[p] = struct{}{}
		if _, ok := ssotSet[p]; !ok {
			t.Errorf("workspaceComputeMetadataRenderOrder has entry %q not in workspaceComputeProvidersOrdered", p)
		}
	}
	for p := range ssotSet {
		if _, ok := renderSet[p]; !ok {
			t.Errorf("workspaceComputeProvidersOrdered has entry %q missing from workspaceComputeMetadataRenderOrder", p)
		}
	}
	// Every provider in the render order must have a default + non-empty
	// instance types (a render entry with empty instances is a
	// UX dead-end — the canvas would render an empty dropdown).
	for _, p := range workspaceComputeMetadataRenderOrder {
		if _, ok := workspaceComputeDefaultInstanceByProvider[p]; !ok {
			t.Errorf("workspaceComputeMetadataRenderOrder has entry %q with no default in workspaceComputeDefaultInstanceByProvider", p)
		}
		if len(workspaceComputeInstanceTypesOrdered[p]) == 0 {
			t.Errorf("workspaceComputeMetadataRenderOrder has entry %q with empty instance-types list", p)
		}
	}
}

// TestComputeMetadata_InitPanicsOnLabelMissingFromProviders is
// the negative case for direction 1.a (label without a
// provider). The production init() guard must panic on this
// mutation. The check is the PRODUCTION checkComputeSSOTConsistency
// function (not a local mirror) so a future regression that
// weakens the production check is caught here.
func TestComputeMetadata_InitPanicsOnLabelMissingFromProviders(t *testing.T) {
	// Mutate: add a "future-cloud" label that has no provider entry.
	mutatedLabels := make(map[string]string, len(workspaceComputeProviderLabels)+1)
	for k, v := range workspaceComputeProviderLabels {
		mutatedLabels[k] = v
	}
	mutatedLabels["future-cloud"] = "Future Cloud"

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on label-without-provider, got nil (production checkComputeSSOTConsistency is too lenient)")
		}
		// Sanity check: the panic message must mention the bad key.
		msg, _ := r.(string)
		if !strings.Contains(msg, "future-cloud") {
			t.Errorf("panic message should mention the offending key 'future-cloud', got: %q", msg)
		}
	}()

	checkComputeSSOTConsistency(
		workspaceComputeProvidersOrdered,
		mutatedLabels,
		workspaceComputeMetadataRenderOrder,
		workspaceComputeDefaultInstanceByProvider,
		workspaceComputeDisplayDefaultByProvider,
		workspaceComputeInstanceTypesOrdered,
	)
}

// TestComputeMetadata_InitPanicsOnProviderMissingLabel is the
// negative case for direction 1.b (provider without a label).
// A new provider added to the ordered slice without a matching
// label entry would silently render an empty string in the
// /compute/metadata response — the production check must panic.
func TestComputeMetadata_InitPanicsOnProviderMissingLabel(t *testing.T) {
	// Mutate: add "new-cloud" to providers but no label for it.
	mutatedProviders := append([]string{}, workspaceComputeProvidersOrdered...)
	mutatedProviders = append(mutatedProviders, "new-cloud")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on provider-without-label, got nil (production checkComputeSSOTConsistency is too lenient)")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "new-cloud") {
			t.Errorf("panic message should mention the offending provider 'new-cloud', got: %q", msg)
		}
	}()

	checkComputeSSOTConsistency(
		mutatedProviders,
		workspaceComputeProviderLabels,
		workspaceComputeMetadataRenderOrder,
		workspaceComputeDefaultInstanceByProvider,
		workspaceComputeDisplayDefaultByProvider,
		workspaceComputeInstanceTypesOrdered,
	)
}

// TestComputeMetadata_InitPanicsOnRenderOrderEntryMissingProvider
// is the negative case for direction 2.a (render-order entry
// without a provider). A render entry for an unknown provider
// would silently render a dropdown row with no instances — the
// production check must panic.
func TestComputeMetadata_InitPanicsOnRenderOrderEntryMissingProvider(t *testing.T) {
	// Mutate: add a "ghost-provider" entry to render order
	// (no matching provider).
	mutatedRender := append([]string{}, workspaceComputeMetadataRenderOrder...)
	mutatedRender = append(mutatedRender, "ghost-provider")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on render-order-entry-without-provider, got nil (production checkComputeSSOTConsistency is too lenient)")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "ghost-provider") {
			t.Errorf("panic message should mention the offending entry 'ghost-provider', got: %q", msg)
		}
	}()

	checkComputeSSOTConsistency(
		workspaceComputeProvidersOrdered,
		workspaceComputeProviderLabels,
		mutatedRender,
		workspaceComputeDefaultInstanceByProvider,
		workspaceComputeDisplayDefaultByProvider,
		workspaceComputeInstanceTypesOrdered,
	)
}

// TestComputeMetadata_InitPanicsOnProviderMissingFromRenderOrder
// is the negative case for direction 2.b (provider missing from
// render order). A new provider added without a render-order
// entry would silently be absent from the canvas dropdown —
// the production check must panic.
func TestComputeMetadata_InitPanicsOnProviderMissingFromRenderOrder(t *testing.T) {
	// Mutate: add "new-cloud" to providers + labels, but NOT
	// to render order.
	mutatedProviders := append([]string{}, workspaceComputeProvidersOrdered...)
	mutatedProviders = append(mutatedProviders, "new-cloud")
	mutatedLabels := make(map[string]string, len(workspaceComputeProviderLabels)+1)
	for k, v := range workspaceComputeProviderLabels {
		mutatedLabels[k] = v
	}
	mutatedLabels["new-cloud"] = "New Cloud"

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on provider-missing-from-render-order, got nil (production checkComputeSSOTConsistency is too lenient)")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "new-cloud") {
			t.Errorf("panic message should mention the offending provider 'new-cloud', got: %q", msg)
		}
	}()

	checkComputeSSOTConsistency(
		mutatedProviders,
		mutatedLabels,
		workspaceComputeMetadataRenderOrder,
		workspaceComputeDefaultInstanceByProvider,
		workspaceComputeDisplayDefaultByProvider,
		workspaceComputeInstanceTypesOrdered,
	)
}

// TestComputeMetadata_InitPanicsOnDuplicateRenderOrderEntry is
// the negative case for direction 2.c (duplicate render-order
// entry). A duplicate in the slice would silently overwrite the
// first occurrence in the map — the production check must panic.
func TestComputeMetadata_InitPanicsOnDuplicateRenderOrderEntry(t *testing.T) {
	// Mutate: add a second "aws" entry to render order.
	mutatedRender := append([]string{}, workspaceComputeMetadataRenderOrder...)
	mutatedRender = append(mutatedRender, "aws") // duplicate

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate-render-order-entry, got nil (production checkComputeSSOTConsistency is too lenient)")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "duplicate") {
			t.Errorf("panic message should mention the duplicate, got: %q", msg)
		}
	}()

	checkComputeSSOTConsistency(
		workspaceComputeProvidersOrdered,
		workspaceComputeProviderLabels,
		mutatedRender,
		workspaceComputeDefaultInstanceByProvider,
		workspaceComputeDisplayDefaultByProvider,
		workspaceComputeInstanceTypesOrdered,
	)
}

// TestComputeMetadata_InitPanicsOnRenderOrderEntryMissingDefault
// is the negative case for direction 3.a (render-order entry
// without a default). A render entry whose provider has no
// default would silently fall back to "t3.medium" in the
// consumer helper — the production check must panic.
func TestComputeMetadata_InitPanicsOnRenderOrderEntryMissingDefault(t *testing.T) {
	// Mutate: remove the "gcp" default.
	mutatedDefaults := make(map[string]string, len(workspaceComputeDefaultInstanceByProvider))
	for k, v := range workspaceComputeDefaultInstanceByProvider {
		if k == "gcp" {
			continue
		}
		mutatedDefaults[k] = v
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on render-order-entry-without-default, got nil (production checkComputeSSOTConsistency is too lenient)")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "gcp") {
			t.Errorf("panic message should mention the offending provider 'gcp', got: %q", msg)
		}
	}()

	checkComputeSSOTConsistency(
		workspaceComputeProvidersOrdered,
		workspaceComputeProviderLabels,
		workspaceComputeMetadataRenderOrder,
		mutatedDefaults,
		workspaceComputeDisplayDefaultByProvider,
		workspaceComputeInstanceTypesOrdered,
	)
}

// TestComputeMetadata_InitPanicsOnRenderOrderEntryMissingDisplayDefault
// is the core#2489-A negative case for direction 3.c (render-
// order entry without a display-default). A render entry whose
// provider has no display-default would silently fall back to
// the canvas's hardcoded "t3.xlarge" in CreateWorkspaceDialog
// — silently re-introducing the EXACT drift bug #2489 was
// opened to fix. The production check must panic.
func TestComputeMetadata_InitPanicsOnRenderOrderEntryMissingDisplayDefault(t *testing.T) {
	// Mutate: remove the "gcp" display-default.
	mutatedDisplayDefaults := make(map[string]string, len(workspaceComputeDisplayDefaultByProvider))
	for k, v := range workspaceComputeDisplayDefaultByProvider {
		if k == "gcp" {
			continue
		}
		mutatedDisplayDefaults[k] = v
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on render-order-entry-without-display-default, got nil (production checkComputeSSOTConsistency is too lenient)")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "gcp") {
			t.Errorf("panic message should mention the offending provider 'gcp', got: %q", msg)
		}
		if !strings.Contains(msg, "display-default") {
			t.Errorf("panic message should mention 'display-default' (so a future maintainer can grep the message), got: %q", msg)
		}
	}()

	checkComputeSSOTConsistency(
		workspaceComputeProvidersOrdered,
		workspaceComputeProviderLabels,
		workspaceComputeMetadataRenderOrder,
		workspaceComputeDefaultInstanceByProvider,
		mutatedDisplayDefaults,
		workspaceComputeInstanceTypesOrdered,
	)
}

// TestComputeMetadata_InitPanicsOnRenderOrderEntryEmptyInstanceTypes
// is the negative case for direction 3.b (render-order entry with
// empty instance-types). A render entry whose provider has an
// empty instance-types list would silently render an empty
// dropdown — the production check must panic.
func TestComputeMetadata_InitPanicsOnRenderOrderEntryEmptyInstanceTypes(t *testing.T) {
	// Mutate: empty the "hetzner" instance-types list.
	mutatedInstances := make(map[string][]string, len(workspaceComputeInstanceTypesOrdered))
	for k, v := range workspaceComputeInstanceTypesOrdered {
		if k == "hetzner" {
			mutatedInstances[k] = []string{} // empty
		} else {
			mutatedInstances[k] = v
		}
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on render-order-entry-with-empty-instance-types, got nil (production checkComputeSSOTConsistency is too lenient)")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "hetzner") {
			t.Errorf("panic message should mention the offending provider 'hetzner', got: %q", msg)
		}
	}()

	checkComputeSSOTConsistency(
		workspaceComputeProvidersOrdered,
		workspaceComputeProviderLabels,
		workspaceComputeMetadataRenderOrder,
		workspaceComputeDefaultInstanceByProvider,
		workspaceComputeDisplayDefaultByProvider,
		mutatedInstances,
	)
}

// TestComputeMetadata_InitAcceptsLiveSSOT pins the positive case
// against the PRODUCTION checkComputeSSOTConsistency function
// (not a local mirror) so a future regression in the production
// check that would weaken the LIVE-SSOT case is caught here. The
// package init has already run at process boot; this test is the
// "readable contract pin" while the package init is the
// "boot-time fail-closed." Pairs with the negative
// TestComputeMetadata_InitPanics* family above.
func TestComputeMetadata_InitAcceptsLiveSSOT(t *testing.T) {
	// MUST not panic. (If it did, the package init would have
	// panicked at process boot, and we wouldn't have reached
	// this test.)
	checkComputeSSOTConsistency(
		workspaceComputeProvidersOrdered,
		workspaceComputeProviderLabels,
		workspaceComputeMetadataRenderOrder,
		workspaceComputeDefaultInstanceByProvider,
		workspaceComputeDisplayDefaultByProvider,
		workspaceComputeInstanceTypesOrdered,
	)
}
