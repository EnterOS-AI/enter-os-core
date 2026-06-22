package provisioner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStart_OversizedConfigBundleProvisions is the Prove-It reproduction for
// the demo-auto SEO Agent provisioning failure:
//
//	CPProvisioner: workspace start failed: cp provisioner: collect config
//	files: config files exceed 12288 bytes
//
// Root cause: collectCPConfigFiles hard-capped the *eligible* config bundle
// (config.yaml + prompts/*) at 12 KiB because the controlplane embedded it in
// EC2 user-data (16 KiB AWS ceiling − bootstrap overhead). The SEO agent's
// config (long SEO system prompt + SERVICES_REPO_WEBSITE + the 12-schedule
// block baked into config.yaml) exceeds 12 KiB, so Start() failed before it
// ever reached the wire — blocking a paying customer from provisioning.
//
// After moving config delivery OFF user-data and onto the persistent
// secondary volume (CP stages the bundle to Secrets Manager; the workspace
// fetches it at boot into /configs), the 12 KiB ceiling is obsolete: the
// bundle travels in the JSON HTTP body to CP, which has no 16 KiB limit. This
// test pins that a realistically-oversized (>12288 B) config bundle now
// reaches the CP request body intact instead of being rejected client-side.
func TestStart_OversizedConfigBundleProvisions(t *testing.T) {
	// SEO-sized config.yaml: a 12-schedule block + SERVICES_REPO_WEBSITE +
	// a long system prompt, comfortably over the retired 12 KiB cap.
	var sb strings.Builder
	sb.WriteString("name: demo-auto-seo\nruntime: claude-code\n")
	sb.WriteString("env:\n  SERVICES_REPO_WEBSITE: https://example.com/demo-auto/website-repo\n")
	sb.WriteString("schedules:\n")
	for i := 0; i < 12; i++ {
		sb.WriteString("  - id: seo-task-")
		sb.WriteString(strings.Repeat("x", 8))
		sb.WriteString("\n    cron: \"0 */2 * * *\"\n    prompt: |\n")
		sb.WriteString("      Run the SEO audit pass, refresh keyword rankings, regenerate the\n")
		sb.WriteString("      sitemap, and publish the digest to the marketing channel.\n")
	}
	configYAML := sb.String()
	seoPrompt := strings.Repeat(
		"You are an expert SEO agent. Audit pages, find ranking gaps, and act. ", 200)

	cfg := map[string][]byte{
		"config.yaml":       []byte(configYAML),
		"prompts/system.md": []byte(seoPrompt),
	}
	total := len(configYAML) + len(seoPrompt)
	if total <= 12<<10 {
		t.Fatalf("fixture not representative: bundle is %d bytes, must exceed 12288 to reproduce the failure", total)
	}
	t.Logf("oversized config bundle: %d bytes (> old 12288 cap)", total)

	var body cpProvisionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"instance_id":"i-seo","state":"pending"}`)
	}))
	defer srv.Close()

	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-seo", httpClient: srv.Client()}
	_, err := p.Start(context.Background(), WorkspaceConfig{
		WorkspaceID: "ws-seo",
		Runtime:     "claude-code",
		Tier:        4,
		PlatformURL: "http://tenant",
		ConfigFiles: cfg,
	})
	if err != nil {
		t.Fatalf("Start with oversized config bundle failed: %v — the 12288-byte cap must be gone now config delivery is off user-data", err)
	}

	// The full bundle must have reached the CP request body intact.
	wantCfg := base64.StdEncoding.EncodeToString([]byte(configYAML))
	if got := body.ConfigFiles["config.yaml"]; got != wantCfg {
		t.Errorf("config.yaml not delivered intact to CP (len got=%d want=%d)", len(got), len(wantCfg))
	}
	wantPrompt := base64.StdEncoding.EncodeToString([]byte(seoPrompt))
	if got := body.ConfigFiles["prompts/system.md"]; got != wantPrompt {
		t.Errorf("prompts/system.md not delivered intact to CP (len got=%d want=%d)", len(got), len(wantPrompt))
	}
}

// TestCollectCPConfigFiles_DoSGuardStillBounds pins that retiring the 12 KiB
// cap did NOT remove the bound entirely — an absurdly large bundle (a buggy
// or hostile tenant) is still rejected so a compromised workspace-server
// can't OOM the CP request path. The guard just moved from a 12 KiB
// user-data ceiling to a generous transport-DoS ceiling.
func TestCollectCPConfigFiles_DoSGuardStillBounds(t *testing.T) {
	huge := make([]byte, cpConfigFilesMaxBytes+1)
	for i := range huge {
		huge[i] = 'a'
	}
	_, _, err := collectCPConfigFiles(WorkspaceConfig{
		ConfigFiles: map[string][]byte{"config.yaml": huge},
	})
	if err == nil {
		t.Fatalf("expected the DoS guard to reject a %d-byte bundle, got nil", len(huge))
	}
	if !strings.Contains(err.Error(), "config files exceed") {
		t.Errorf("unexpected error %q, want the size-guard message", err.Error())
	}
}

// TestCollectCPConfigFiles_AcceptsSEOSizedBundle is the unit-level companion:
// collectCPConfigFiles itself (not just Start) must accept the SEO-sized
// bundle. Guards the exact constant that caused the outage.
func TestCollectCPConfigFiles_AcceptsSEOSizedBundle(t *testing.T) {
	// 30 KiB of eligible config — far over the retired 12288 cap, far under
	// the new DoS guard.
	cfgBlob := make([]byte, 18<<10)
	for i := range cfgBlob {
		cfgBlob[i] = 'c'
	}
	promptBlob := make([]byte, 12<<10)
	for i := range promptBlob {
		promptBlob[i] = 'p'
	}
	files, _, err := collectCPConfigFiles(WorkspaceConfig{
		ConfigFiles: map[string][]byte{
			"config.yaml":       cfgBlob,
			"prompts/system.md": promptBlob,
		},
	})
	if err != nil {
		t.Fatalf("collectCPConfigFiles rejected a %d-byte SEO-sized bundle: %v", len(cfgBlob)+len(promptBlob), err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files collected, got %d", len(files))
	}
	// Also confirm a template-dir path stays size-bounded the same way.
	tmpl := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpl, "config.yaml"), cfgBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collectCPConfigFiles(WorkspaceConfig{TemplatePath: tmpl}); err != nil {
		t.Fatalf("collectCPConfigFiles rejected an SEO-sized template config.yaml: %v", err)
	}
}
