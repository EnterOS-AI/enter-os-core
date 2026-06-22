package middleware

import (
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// generatedImageImgSrc returns the img-src directive for canvas (HTML) routes.
//
// Beyond the canvas's own assets ('self' + data:/blob:) we must allow the host
// that serves GENERATED images. The image-gen capability socket (RFC #3105)
// stores each generated image in Cloudflare R2 and hands the agent a time-boxed,
// SigV4-presigned GET URL on `<bucket>.<cf-account-hash>.r2.cloudflarestorage.com`.
// The chat renders that URL directly as
// `<img src="https://…r2.cloudflarestorage.com/…">`, so the host must be in
// img-src or the browser blocks display (broken thumbnail).
//
// The exact bucket + CF account hash are control-plane deploy config, so we
// cannot hardcode the origin. A deploy MAY pin it via MOLECULE_IMAGE_GEN_R2_HOST
// (tightest policy); otherwise we use the documented wildcard
// `https://*.r2.cloudflarestorage.com`.
//
// This must stay in sync with canvas/src/middleware.ts buildImgSrc(): the
// combined tenant image returns BOTH this Go header and the proxied Next.js
// CSP, and browsers enforce the INTERSECTION of multiple CSP headers — so the
// generated-image host has to be present in both or it is still blocked.
//
// Security: this widens only image *display* (img-src). connect-src is left
// unchanged, so fetch()/XHR to R2 stays blocked — no exfiltration channel. The
// URLs are time-boxed, signed GETs of a single object key the agent already
// holds; rendering them grants no capability beyond viewing the produced image.
func generatedImageImgSrc() string {
	r2Host := os.Getenv("MOLECULE_IMAGE_GEN_R2_HOST")
	if r2Host == "" {
		r2Host = "https://*.r2.cloudflarestorage.com"
	}
	return "img-src 'self' data: blob: " + r2Host + "; "
}

// apiPrefixes lists the URL path prefixes that are served by Go platform
// handlers (JSON/binary responses). Canvas-proxied routes (Next.js HTML) are
// everything not in this list — they require 'unsafe-inline' for hydration.
//
// Keep this in sync with the routes registered in router/router.go.  A path
// not on this list gets the permissive (canvas-compatible) CSP, which is
// acceptable: adding a new API prefix here is an opt-in tightening, never a
// silent breakage.
var apiPrefixes = []string{
	"/workspaces",
	"/registry",
	"/health",
	"/admin",
	"/metrics",
	"/settings",
	"/bundles",
	"/org",
	"/orgs", // #610 — per-org plugin allowlist routes
	"/templates",
	"/plugins",
	"/webhooks",
	"/channels",
	"/ws",
	"/events",
	"/approvals",
}

// isAPIPath reports whether a URL path belongs to a Go platform handler.
// Such paths return JSON and do not need 'unsafe-inline' in their CSP.
// Canvas-proxied paths (NoRoute → Next.js) are anything not matched here.
func isAPIPath(path string) bool {
	for _, prefix := range apiPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// SecurityHeaders returns a Gin middleware that sets standard HTTP security
// headers on every response to mitigate common web-application attacks:
//
//   - X-Content-Type-Options: nosniff                        — prevents MIME-type sniffing
//   - X-Frame-Options: DENY                                  — blocks iframe embedding (clickjacking)
//   - Content-Security-Policy                                — two tiers (see below)
//   - Strict-Transport-Security: max-age=31536000; includeSubDomains — enforces HTTPS for 1 year
//   - Referrer-Policy: strict-origin-when-cross-origin       — avoids leaking full paths/queries in Referer
//   - Permissions-Policy: camera=(), microphone=(), geolocation=() — denies sensor access for embedded content
//
// CSP tiers (fix for #450):
//
//  1. API routes (/workspaces, /registry, /health, …) — return JSON, not HTML.
//     Strict "default-src 'self'" with no unsafe directives. XSS in a JSON
//     response body is not executable without being reflected into an HTML
//     page, so the permissive directives would only provide false assurance.
//
//  2. Canvas-proxied routes (NoRoute → Next.js) — serve HTML with inline
//     scripts required for Next.js hydration. 'unsafe-inline' is kept here
//     because removing it breaks the canvas. 'unsafe-eval' was dropped after
//     confirming the production build renders without it.
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		// #282: Referrer-Policy prevents browsers from leaking the full Referer
		// URL to cross-origin resources (which can expose internal paths/queries).
		// Permissions-Policy denies sensor access by default — especially relevant
		// because the canvas embeds iframes for Langfuse traces.
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")

		// #450: differentiate CSP by route type.
		if isAPIPath(c.Request.URL.Path) {
			// API routes return JSON — no inline scripts are ever needed.
			// A strict CSP here is meaningful: it prevents a hypothetical
			// reflected-XSS payload in an error message from executing if
			// the JSON is ever mistakenly served with a text/html content-type.
			c.Header("Content-Security-Policy", "default-src 'self'")
		} else {
			// Canvas routes (NoRoute → reverse-proxy to Next.js) serve HTML
			// that requires inline script injection for React hydration.
			// 'unsafe-eval' was deliberately removed — Next.js production
			// builds do not require it; only the dev server does.
			c.Header("Content-Security-Policy",
				"default-src 'self'; "+
					"script-src 'self' 'unsafe-inline'; "+
					"style-src 'self' 'unsafe-inline'; "+
					// img-src includes the generated-image R2 host so the chat
					// can render image-gen results (presigned R2 URLs). See
					// generatedImageImgSrc() for the security rationale.
					generatedImageImgSrc()+
					"frame-src 'self' blob:; "+
					"connect-src 'self' ws: wss:; "+
					"font-src 'self' data:")
		}
		c.Next()
	}
}
