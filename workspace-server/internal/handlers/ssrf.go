package handlers

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// devModeAllowsLoopback reports whether the SSRF defence should permit
// http://127.0.0.1:<port> workspace URLs. True only when MOLECULE_ENV is
// a dev value — this is the same convention the middleware dev-mode
// escape hatch uses.
//
// Why: on a self-hosted Docker setup the provisioner publishes each
// container's A2A port on 127.0.0.1:<ephemeral> and writes that URL
// to workspaces.url. The A2A proxy on the host platform needs to POST
// to that same 127.0.0.1:<port> to reach the container — there's no
// other reachable address. SaaS never hits this branch because hosted
// tenants run MOLECULE_ENV=production (enforced by the crypto strict-
// init path) and the workspace URL is the tenant EC2's VPC-private IP.
//
// The relaxation is narrowly scoped to loopback IPv4 + ::1 — the
// metadata, CGNAT, TEST-NET, and link-local guards stay blocked even
// in dev mode.
func devModeAllowsLoopback() bool {
	env := strings.ToLower(strings.TrimSpace(os.Getenv("MOLECULE_ENV")))
	return env == "development" || env == "dev"
}

// ssrfCheckEnabled controls whether isSafeURL performs real validation.
// Tests disable it via setSSRFCheckForTest so that httptest.NewServer
// loopback URLs and fake hostnames (*.example) don't trigger SSRF
// rejections. Production code never mutates this.
var ssrfCheckEnabled = true

// setSSRFCheckForTest overrides ssrfCheckEnabled for the duration of a test
// and returns a restore function. Use with defer in *_test.go only.
func setSSRFCheckForTest(enabled bool) func() {
	prev := ssrfCheckEnabled
	ssrfCheckEnabled = enabled
	return func() { ssrfCheckEnabled = prev }
}

// isSafeURL validates that a URL resolves to a publicly-routable address,
// preventing A2A requests from being redirected to internal/cloud-metadata
// infrastructure (SSRF, CWE-918). Workspace URLs come from DB/Redis caches
// so we validate before making any outbound HTTP call.
//
// SaaS relaxation: when saasMode() is true, RFC-1918 private ranges and
// IPv6 ULA are considered safe because workspaces live on sibling EC2s in
// the same VPC and register by their VPC-private IP. Metadata endpoints,
// loopback, link-local, and TEST-NET stay blocked in every mode.
func isSafeURL(rawURL string) error {
	if !ssrfCheckEnabled {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("forbidden scheme: %s (only http/https allowed)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("empty hostname")
	}
	if ip := net.ParseIP(host); ip != nil {
		if (ip.IsLoopback() && !testAllowLoopback && !devModeAllowsLoopback()) || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
			return fmt.Errorf("forbidden loopback/unspecified/link-local IP: %s", ip)
		}
		if isPrivateOrMetadataIP(ip) {
			return fmt.Errorf("forbidden private/metadata IP: %s", ip)
		}
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("DNS resolution blocked for hostname: %s (%w)", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("DNS returned no addresses for: %s", host)
	}
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil {
			continue
		}
		if (ip.IsLoopback() && !testAllowLoopback && !devModeAllowsLoopback()) || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
			return fmt.Errorf("hostname %s resolves to forbidden link-local/loopback IP: %s", host, ip)
		}
		if isPrivateOrMetadataIP(ip) {
			return fmt.Errorf("hostname %s resolves to forbidden IP: %s", host, ip)
		}
	}
	return nil
}

// testAllowLoopback is a test-only escape hatch. When true, isSafeURL
// accepts 127.0.0.0/8 and ::1 so unit tests that stub workspace URLs
// with httptest.NewServer (which binds to loopback) can reach their
// own mock backends. Flipped via allowLoopbackForTest(t) in tests —
// never set in production code paths.
//
// The 169.254 metadata, RFC-1918, TEST-NET, CGNAT, and link-local
// guards are NOT relaxed by this flag — only loopback.
var testAllowLoopback = false

// isPrivateOrMetadataIP returns true for IPs that must not be reached via A2A.
//
// Always blocked (both modes):
//   - 169.254.0.0/16 link-local (cloud metadata endpoints)
//   - 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24 (TEST-NET RFC-5737)
//   - 100.64.0.0/10 (carrier-grade NAT)
//   - IPv6 loopback ::1, link-local fe80::/10, and ULA fc00::/7 in strict mode
//
// Allowed in SaaS mode only (saasMode() == true):
//   - 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 (RFC-1918)
//   - fd00::/8 (IPv6 ULA subset of fc00::/7)
//
// Rationale: SaaS tenants run workspaces on sibling EC2s in the same VPC
// and register them by VPC-private IP. The control plane provisions these
// instances, so intra-VPC routing is trusted. On self-hosted / single-
// container deployments the relaxation is off and every private range
// stays blocked.
func isPrivateOrMetadataIP(ip net.IP) bool {
	// MOLECULE_ENV=development is the dev-host pattern: platform and
	// workspace containers share a docker bridge network (172.18.0.0/16,
	// RFC-1918). Treat that the same as SaaS for private-range relaxation
	// — both share the "trusted intra-network routing" property. Without
	// this, every workspace registration via docker-internal hostname
	// resolves to 172.18.x.x and gets rejected as
	// "workspace URL is not publicly routable", breaking the entire
	// docker-compose dev loop. Always-blocked categories (metadata link-
	// local, TEST-NET, CGNAT) remain blocked regardless.
	saas := saasMode() || devModeAllowsLoopback()

	// IPv4 path.
	if ip4 := ip.To4(); ip4 != nil {
		// Metadata link-local — always blocked.
		if metadataV4.Contains(ip4) {
			return true
		}
		// TEST-NET / documentation — always blocked.
		for _, r := range docRangesV4 {
			if r.Contains(ip4) {
				return true
			}
		}
		// Carrier-grade NAT — always blocked.
		if cgnatV4.Contains(ip4) {
			return true
		}
		// RFC-1918 private — blocked strict, allowed in SaaS.
		for _, r := range privateV4 {
			if r.Contains(ip4) {
				return !saas
			}
		}
		return false
	}

	// IPv6 path — .To4() was nil so this is a real v6 address.
	// ::1 (loopback) — treat as blocked here too for defense-in-depth,
	// unless tests have opted into loopback via testAllowLoopback OR
	// MOLECULE_ENV is a dev value (mirrors the v4 relaxation above).
	if ip.IsLoopback() && !testAllowLoopback && !devModeAllowsLoopback() {
		return true
	}
	// Link-local fe80::/10 — always blocked.
	if ip.IsLinkLocalUnicast() {
		return true
	}
	// ULA fc00::/7. fd00::/8 is the "locally assigned" half AWS hands out;
	// fc00::/8 is reserved. We treat the whole fc00::/7 as private, then
	// let SaaS relax fd00::/8 (matches the tests).
	if ulaV6.Contains(ip) {
		if saas && fd00V6.Contains(ip) {
			return false
		}
		return true
	}
	return false
}

var (
	metadataV4 = mustCIDR("169.254.0.0/16")
	cgnatV4    = mustCIDR("100.64.0.0/10")

	privateV4 = []net.IPNet{
		mustCIDR("10.0.0.0/8"),
		mustCIDR("172.16.0.0/12"),
		mustCIDR("192.168.0.0/16"),
	}
	docRangesV4 = []net.IPNet{
		mustCIDR("192.0.2.0/24"),
		mustCIDR("198.51.100.0/24"),
		mustCIDR("203.0.113.0/24"),
	}

	ulaV6  = mustCIDR("fc00::/7")
	fd00V6 = mustCIDR("fd00::/8")
)

func mustCIDR(s string) net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("ssrf: bad CIDR " + s + ": " + err.Error())
	}
	return *n
}

// validateRelPath checks that a file path is relative and does not escape
// the destination via absolute paths or ".." traversal. Used by
// copyFilesToContainer and deleteViaEphemeral as a defence-in-depth measure.
func validateRelPath(filePath string) error {
	// Reject empty string and dot-only paths before any processing.
	if filePath == "" || filePath == "." {
		return fmt.Errorf("empty or dot-only path not allowed")
	}
	clean := filepath.Clean(filePath)
	// Reject absolute paths (Unix / or Windows C:\).
	if filepath.IsAbs(clean) {
		return fmt.Errorf("path traversal or absolute path not allowed: %s", filePath)
	}
	// Reject any path containing ".." anywhere — check both raw and cleaned
	// because filepath.Clean resolves ".." upward (e.g. "foo/../bar" → "bar"
	// and "foo/.." → ".") which would make the check pass if only clean were checked.
	// We only want explicitly-named files; ".." implies intent to escape.
	if strings.Contains(filePath, "..") || strings.Contains(clean, "..") {
		return fmt.Errorf("path traversal or absolute path not allowed: %s", filePath)
	}
	return nil
}
