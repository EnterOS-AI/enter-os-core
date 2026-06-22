package handlers

import (
	"net"
	"testing"
)

// isSafeURL is defined in a2a_proxy.go.
// isPrivateOrMetadataIP is defined in a2a_proxy.go.
// saasMode is defined in registry.go.

// TestSaasMode covers the env-resolution ladder so a self-hosted
// operator can't accidentally flip into SaaS mode by leaving a stale
// MOLECULE_ORG_ID around, and an explicit MOLECULE_DEPLOY_MODE wins
// over the legacy implicit signal.
func TestSaasMode(t *testing.T) {
	cases := []struct {
		name       string
		deployMode string
		orgID      string
		want       bool
	}{
		{"both unset", "", "", false},
		{"legacy org id only", "", "7b2179dc-8cc6-4581-a3c6-c8bff4481086", true},
		{"explicit saas", "saas", "", true},
		{"explicit saas overrides missing org", "SaaS", "", true}, // case-insensitive
		{"explicit self-hosted wins over legacy org id", "self-hosted", "some-org", false},
		{"explicit selfhosted wins over legacy org id", "selfhosted", "some-org", false},
		{"explicit standalone wins over legacy org id", "standalone", "some-org", false},
		{"whitespace-only deploy mode falls through to legacy", "   ", "some-org", true},
		{"whitespace-only org id falls through to false", "", "   ", false},
		// Typo / unknown values: must fall closed (strict / self-hosted)
		// instead of falling through to the MOLECULE_ORG_ID legacy signal.
		// Any tenant deployment has MOLECULE_ORG_ID set, so a typo like
		// MOLECULE_DEPLOY_MODE=prod used to silently flip into SaaS mode.
		{"typo prod falls closed even with org id set", "prod", "some-org", false},
		{"typo SaaS-mode falls closed even with org id set", "SaaS-mode", "some-org", false},
		{"typo production falls closed", "production", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MOLECULE_DEPLOY_MODE", tc.deployMode)
			t.Setenv("MOLECULE_ORG_ID", tc.orgID)
			if got := saasMode(); got != tc.want {
				t.Errorf("saasMode() = %v, want %v (MOLECULE_DEPLOY_MODE=%q MOLECULE_ORG_ID=%q)",
					got, tc.want, tc.deployMode, tc.orgID)
			}
		})
	}
}

// TestIsPrivateOrMetadataIP_SaaSMode covers the SaaS-mode relaxation:
// RFC-1918 and ULA ranges are allowed, but metadata / loopback / TEST-NET
// classes stay blocked in every mode. Regression guard for the core
// SaaS provisioning fix (issue: workspaces register with their VPC
// private IP, which is 172.31.x.x on AWS default VPCs).
func TestIsPrivateOrMetadataIP_SaaSMode(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")
	t.Setenv("MOLECULE_ORG_ID", "")
	cases := []struct {
		name  string
		ipStr string
		want  bool
	}{
		// RFC-1918 must be ALLOWED in SaaS mode.
		{"172.31 allowed in saas", "172.31.44.78", false},
		{"10/8 allowed in saas", "10.0.0.5", false},
		{"192.168 allowed in saas", "192.168.1.1", false},
		// IPv6 ULA must be ALLOWED in SaaS mode (AWS IPv6 VPC analogue).
		{"fd00 ULA allowed in saas", "fd12:3456:789a::1", false},
		// Metadata stays BLOCKED even in SaaS mode.
		{"169.254 still blocked", "169.254.169.254", true},
		// 127/8 loopback is NOT checked by isPrivateOrMetadataIP itself --
		// the caller (isSafeURL) checks ip.IsLoopback() separately. We assert
		// the helper's own semantics here, not the aggregate gate.
		{"127/8 not checked by this helper (isSafeURL covers it)", "127.0.0.1", false},
		{"::1 still blocked (IPv6 metadata)", "::1", true},
		{"fe80 still blocked", "fe80::1", true},
		// TEST-NET stays blocked.
		{"192.0.2.x still blocked", "192.0.2.5", true},
		{"198.51.100.x still blocked", "198.51.100.5", true},
		{"203.0.113.x still blocked", "203.0.113.5", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ipStr)
			if ip == nil {
				t.Fatalf("ParseIP(%q) returned nil", tc.ipStr)
			}
			if got := isPrivateOrMetadataIP(ip); got != tc.want {
				t.Errorf("isPrivateOrMetadataIP(%s) = %v, want %v", tc.ipStr, got, tc.want)
			}
		})
	}
}

// TestIsPrivateOrMetadataIP_IPv6 covers the IPv6 gap the previous
// implementation had — it returned false for every IPv6 literal
// unconditionally, which would let a registered [::1] or [fe80::…]
// URL bypass the SSRF check entirely.
func TestIsPrivateOrMetadataIP_IPv6(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "")
	t.Setenv("MOLECULE_ORG_ID", "")
	cases := []struct {
		name  string
		ipStr string
		want  bool
	}{
		{"::1 loopback blocked", "::1", true},
		{"fe80 link-local blocked", "fe80::1", true},
		{"fe80 link-local with mac blocked", "fe80::a00:27ff:fe00:1", true},
		{"fc00 ULA blocked (non-saas)", "fc00::1", true},
		{"fd00 ULA blocked (non-saas)", "fd12::1", true},
		{"public v6 allowed", "2606:4700:4700::1111", false}, // 1.1.1.1 v6
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ipStr)
			if ip == nil {
				t.Fatalf("ParseIP(%q) returned nil", tc.ipStr)
			}
			if got := isPrivateOrMetadataIP(ip); got != tc.want {
				t.Errorf("isPrivateOrMetadataIP(%s) = %v, want %v", tc.ipStr, got, tc.want)
			}
		})
	}
}

func TestIsPrivateOrMetadataIP(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "")
	t.Setenv("MOLECULE_ORG_ID", "")
	cases := []struct {
		name  string
		ipStr string
		want  bool
	}{
		// Must be blocked: RFC-1918 private
		{"10.0.0.1", "10.0.0.1", true},
		{"10.255.255.254", "10.255.255.254", true},
		{"172.16.0.0", "172.16.0.0", true},
		{"172.31.255.255", "172.31.255.255", true},
		{"192.168.0.1", "192.168.0.1", true},
		{"192.168.255.255", "192.168.255.255", true},
		// Must be blocked: cloud metadata link-local
		{"169.254.169.254", "169.254.169.254", true},
		{"169.254.0.1", "169.254.0.1", true},
		// Must be blocked: carrier-grade NAT
		{"100.64.0.1", "100.64.0.1", true},
		{"100.127.255.254", "100.127.255.254", true},
		// Must be blocked: documentation ranges
		{"192.0.2.1", "192.0.2.1", true},
		{"198.51.100.1", "198.51.100.1", true},
		{"203.0.113.1", "203.0.113.1", true},
		// Must be allowed: public IP addresses
		{"8.8.8.8", "8.8.8.8", false},
		{"1.1.1.1", "1.1.1.1", false},
		// Previously asserted (incorrectly) that 203.0.113.254 is public --
		// the original test's comment claimed the address was "above 203.0.113.0/24
		// range end", but 203.0.113.0/24 spans 203.0.113.0-255, so .254 IS in
		// range and correctly blocked. Assertion flipped to match reality.
		{"203.0.113.254 (TEST-NET-3)", "203.0.113.254", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ipStr)
			if ip == nil {
				t.Fatalf("ParseIP(%q) returned nil", tc.ipStr)
			}
			got := isPrivateOrMetadataIP(ip)
			if got != tc.want {
				t.Errorf("isPrivateOrMetadataIP(%s) = %v, want %v", tc.ipStr, got, tc.want)
			}
		})
	}
}

func TestIsSafeURL(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "")
	t.Setenv("MOLECULE_ORG_ID", "")
	cases := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		// Valid: public HTTPS. Use example.com (RFC-2606, resolves
		// globally to Cloudflare Anycast) rather than agent.example.com
		// (subdomain NXDOMAIN on many resolvers, makes the test flake).
		{"public https", "https://example.com:8080/a2a", false},
		{"public http", "http://example.com/a2a", false},
		// Loopback is blocked by isSafeURL even in dev — the orchestrator
		// controls access via WorkspaceAuth + CanCommunicate, not via this URL check.
		// Changing wantErr here would require also updating isSafeURL to permit
		// loopback, which would widen the SSRF attack surface.
		{"localhost blocked", "http://127.0.0.1:8000", true},
		{"localhost with path", "http://127.0.0.1:9000", true},

		// Forbidden: non-HTTP(S) scheme
		{"file scheme blocked", "file:///etc/passwd", true},
		{"ftp scheme blocked", "ftp://internal/", true},
		{"mailto scheme blocked", "mailto://user@example.com", true},
		{"data scheme blocked", "data:text/html,<script>alert(1)</script>", true},

		// Forbidden: IP literals — cloud metadata
		{"AWS IMDS blocked", "http://169.254.169.254/latest/meta-data/", true},
		{"IMDS 169.254.0.1 blocked", "http://169.254.0.1/", true},

		// Forbidden: IP literals — loopback
		{"loopback 127.0.0.1 blocked", "http://127.0.0.1:8080", true},
		{"loopback 127.255.255.255 blocked", "http://127.255.255.255:9000", true},

		// Forbidden: IP literals — RFC-1918 private
		{"10.x private blocked", "http://10.0.0.1:8080", true},
		{"172.x private blocked", "http://172.16.0.5:8000", true},
		{"192.x private blocked", "http://192.168.1.1:8000", true},

		// Forbidden: IP literals — link-local multicast
		{"link-local multicast 224.0.0.1 blocked", "http://224.0.0.1/", true},
		{"link-local multicast 224.x.x.x blocked", "http://224.0.0.251:8080", true},

		// Forbidden: empty hostname
		{"empty hostname rejected", "http://:8080/a2a", true},

		// Forbidden: IP literals — unspecified
		{"0.0.0.0 blocked", "http://0.0.0.0:8080", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := isSafeURL(tc.rawURL)
			if tc.wantErr && err == nil {
				t.Errorf("isSafeURL(%q): expected error, got nil", tc.rawURL)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("isSafeURL(%q): expected nil, got %v", tc.rawURL, err)
			}
		})
	}
}

// Dev-mode loopback relaxation — lock in the local-dev SSRF escape
// hatch. The provisioner on a self-hosted Docker setup publishes
// workspace A2A ports on 127.0.0.1:<ephemeral>, so the A2A proxy must
// POST to loopback. Without this relaxation every Canvas chat send
// returned 502 on the host-run platform.
//
// SaaS safety: the relaxation fires ONLY when MOLECULE_ENV is a dev
// value. Production (MOLECULE_ENV=production) must continue to block
// loopback. Every other blocked range (metadata 169.254/16, TEST-NET,
// CGNAT, link-local) must stay blocked even in dev mode.

func TestIsSafeURL_DevModeAllowsLoopback(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "development")
	cases := []string{
		"http://127.0.0.1:59806",
		"http://127.0.0.1:8000/a2a",
		"http://[::1]:8000",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			if err := isSafeURL(u); err != nil {
				t.Errorf("dev mode should allow %q, got %v", u, err)
			}
		})
	}
}

func TestIsSafeURL_DevModeShortAlias(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "dev")
	if err := isSafeURL("http://127.0.0.1:59806"); err != nil {
		t.Errorf("MOLECULE_ENV=dev should allow loopback, got %v", err)
	}
}

func TestIsSafeURL_Production_StillBlocksLoopback(t *testing.T) {
	// SaaS-safety guarantee: production tenants must keep blocking
	// loopback URLs. A workspace registering a loopback URL in prod
	// is almost certainly an attack targeting co-located admin
	// services — the SSRF defence MUST keep firing.
	t.Setenv("MOLECULE_ENV", "production")
	if err := isSafeURL("http://127.0.0.1:8080"); err == nil {
		t.Error("production must block loopback, got nil error")
	}
}

func TestIsSafeURL_DevMode_StillBlocksOtherRanges(t *testing.T) {
	// The relaxation is narrow — only loopback. Metadata / CGNAT /
	// TEST-NET / link-local must still fire in dev mode. A malicious
	// workspace in a dev install must NOT reach cloud metadata.
	t.Setenv("MOLECULE_ENV", "development")
	stillBlocked := []string{
		"http://169.254.169.254/latest/meta-data/", // AWS IMDS
		"http://192.0.2.1:8080",                    // TEST-NET-1
		"http://100.64.0.1:8080",                   // CGNAT
		"http://0.0.0.0:8080",                      // unspecified
		"http://224.0.0.1/",                        // link-local multicast
	}
	for _, u := range stillBlocked {
		t.Run(u, func(t *testing.T) {
			if err := isSafeURL(u); err == nil {
				t.Errorf("dev mode must still block %q", u)
			}
		})
	}
}

func TestDevModeAllowsLoopback_Predicate(t *testing.T) {
	cases := []struct {
		name, env string
		want      bool
	}{
		{"development", "development", true},
		{"dev", "dev", true},
		{"Development (case)", "Development", true},
		{"DEV (case)", "DEV", true},
		{"  dev  (whitespace)", "  dev  ", true},
		{"production", "production", false},
		{"staging", "staging", false},
		{"empty string", "", false},
		{"typo devel", "devel", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MOLECULE_ENV", tc.env)
			got := devModeAllowsLoopback()
			if got != tc.want {
				t.Errorf("devModeAllowsLoopback() with MOLECULE_ENV=%q = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// TestIsSafeURL_SaaSMode_AllowsRFC1918 is the integration-level wrapper test
// for the SaaS-mode SSRF relaxation.  It exercises isSafeURL (the public API),
// not isPrivateOrMetadataIP (the inner helper), ensuring the wrapper correctly
// propagates saasMode() to its helper.
//
// Regression guard: isSafeURL previously hardcoded RFC-1918 rejection and never
// called saasMode(), causing 502 on every A2A call from Docker-networked or VPC
// deployments (issue #1785 / PR #1785).  The inner helper's TestIsPrivateOrMetadataIP_SaaSMode
// was green the whole time — classic "test the intent, not the integration" gap.
func TestIsSafeURL_SaaSMode_AllowsRFC1918(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")
	t.Setenv("MOLECULE_ORG_ID", "")
	for _, url := range []string{
		"http://10.1.2.3/agent",
		"http://10.0.0.5:8000/a2a",
		"http://172.16.0.1/agent",
		"http://172.18.0.42:8000/a2a",
		"http://172.31.44.78/agent",
		"http://192.168.1.100/agent",
		"http://192.168.255.254:9000/a2a",
		"http://[fd00::1]/agent",
		"http://[fd12:3456:789a::42]/a2a",
	} {
		if err := isSafeURL(url); err != nil {
			t.Errorf("isSafeURL(%q) in saasMode: got %v, want nil", url, err)
		}
	}
}

// TestIsSafeURL_SaaSMode_StillBlocksMetadataEtAl verifies that even in SaaS
// mode the always-blocked ranges (metadata, loopback, TEST-NET, CGNAT) stay blocked.
func TestIsSafeURL_SaaSMode_StillBlocksMetadataEtAl(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")
	t.Setenv("MOLECULE_ORG_ID", "")
	for _, url := range []string{
		// Cloud metadata — must stay blocked in every mode.
		"http://169.254.169.254/latest/meta-data/",
		"http://169.254.0.1/",
		// Loopback — must stay blocked.
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
		// TEST-NET documentation ranges — must stay blocked.
		"http://192.0.2.5/agent",
		"http://198.51.100.5/a2a",
		"http://203.0.113.42/agent",
		// CGNAT — must stay blocked.
		"http://100.64.0.1/agent",
		"http://100.127.255.254:8000/a2a",
		// ULA fc00::/8 (non-fd00 half) — must stay blocked in SaaS.
		"http://[fc00::1]/agent",
		// Non-RFC-1918 private ranges still blocked.
		"http://224.0.0.1/",
	} {
		if err := isSafeURL(url); err == nil {
			t.Errorf("isSafeURL(%q) in saasMode: got nil, want block", url)
		}
	}
}

// TestIsSafeURL_DevMode_AllowsRFC1918 pins the dev-mode RFC-1918 + ULA
// relaxation that #2103 widened. The dev-host docker-compose pattern
// puts the platform + workspaces on the same docker bridge (172.18.0.0/16),
// so workspace registration via 172.18.x.x must NOT be rejected in dev.
// SaaS already allowed this; dev mode now matches via
// `saas := saasMode() || devModeAllowsLoopback()` in isPrivateOrMetadataIP.
//
// Without this test, a future refactor that quietly drops the
// `|| devModeAllowsLoopback()` from line 130 wouldn't trip any test —
// the existing `TestIsSafeURL_DevMode_StillBlocksOtherRanges` only
// pins the security floor (metadata / TEST-NET / CGNAT), not the
// behavior change.
func TestIsSafeURL_DevMode_AllowsRFC1918(t *testing.T) {
	// Make sure saasMode() returns false so the test exercises the
	// devModeAllowsLoopback() branch specifically — not a SaaS-mode pass.
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	t.Setenv("MOLECULE_ORG_ID", "")
	t.Setenv("MOLECULE_ENV", "development")

	for _, url := range []string{
		"http://10.1.2.3/agent",
		"http://172.18.0.42:8000/a2a",       // the docker-compose case from the issue
		"http://192.168.1.100/agent",
		"http://[fd00::1]/agent",            // IPv6 ULA fd00::/8 also relaxed
	} {
		if err := isSafeURL(url); err != nil {
			t.Errorf("isSafeURL(%q) in dev mode: got %v, want nil", url, err)
		}
	}
}

// TestIsSafeURL_StrictMode_BlocksRFC1918 is the strict-mode counterpart to
// TestIsSafeURL_SaaSMode_AllowsRFC1918.  In self-hosted / single-container
// deployments there is no legitimate reason to reach RFC-1918 agents, so the
// wrapper must block them.
func TestIsSafeURL_StrictMode_BlocksRFC1918(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")
	t.Setenv("MOLECULE_ORG_ID", "")
	for _, url := range []string{
		"http://10.1.2.3/agent",
		"http://172.16.0.1:8000/a2a",
		"http://172.31.44.78/agent",
		"http://192.168.1.100/agent",
		"http://[fd00::1]/agent",
	} {
		if err := isSafeURL(url); err == nil {
			t.Errorf("isSafeURL(%q) in strict mode: got nil, want block", url)
		}
	}
}

// TestIsSafeURL_SaasMode_LegacyOrgID covers the legacy MOLECULE_ORG_ID signal
// (no MOLECULE_DEPLOY_MODE set).  An org ID alone is sufficient to activate SaaS
// mode per the saasMode() resolution ladder.
func TestIsSafeURL_SaasMode_LegacyOrgID(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "")
	t.Setenv("MOLECULE_ORG_ID", "7b2179dc-8cc6-4581-a3c6-c8bff4481086")
	for _, url := range []string{
		"http://10.1.2.3/agent",
		"http://172.18.0.42:8000/a2a",
		"http://192.168.1.100/agent",
		"http://[fd00::1]/agent",
	} {
		if err := isSafeURL(url); err != nil {
			t.Errorf("isSafeURL(%q) with legacy MOLECULE_ORG_ID: got %v, want nil", url, err)
		}
	}
}