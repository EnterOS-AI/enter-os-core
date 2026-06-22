// Package handlers — transcript proxy.
//
// GET /workspaces/:id/transcript proxies to the workspace's own
// /transcript endpoint, which surfaces the live agent session log
// runtimes return supported:false.
//
// Why this lives in the platform: docker exec works for local dev but
// not for remote (Phase 30) workspaces on Fly Machines. The platform's
// network proxy is the only path that scales to both.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// TranscriptHandler proxies /workspaces/:id/transcript to the workspace agent.
type TranscriptHandler struct {
	httpClient *http.Client
}

func NewTranscriptHandler() *TranscriptHandler {
	// SSRF hardening (Researcher #2132 / RC 103771). The transcript
	// proxy is a known SSRF surface: the front-door isSafeURL check
	// happens BEFORE the outbound call (workspaceURL came from
	// agent_card in the DB, which is attacker-writable via
	// /registry/register), but two vectors remain after that gate:
	//
	//   (1) DNS-rebinding TOCTOU: the front-door check resolves the
	//       hostname, sees a public IP, returns success. A split-ms
	//       later the DNS TTL flips and the second resolution (at
	//       actual TCP-dial time) returns 169.254.169.254 (IMDS). The
	//       dialer now connects to the metadata endpoint and forwards
	//       the caller's Authorization bearer to it.
	//
	//   (2) Unvalidated redirects: the upstream returns 302 →
	//       somewhere.evil.example. The default http.Client follows
	//       the redirect with the same Authorization header (per RFC
	//       7235; the bearer is forwarded to the redirect target). If
	//       the redirect target is a private IP (IMDS, internal
	//       services), the bearer leaks.
	//
	// Both vectors are closed by a single mechanism: net.Dialer.Control
	// inspects the POST-resolution net.IP on EVERY dial (catches
	// (1) re-binding AND (2) redirect-target rebinding) + CheckRedirect
	// returns http.ErrUseLastResponse (catches the rest of (2) by
	// refusing to follow). The dialer's Control is called after
	// getaddrinfo, so it sees the IP the dialer is about to use,
	// not the IP the front-door saw.
	return &TranscriptHandler{
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			// Disable redirects entirely: the upstream 302 response is
			// surfaced to the caller (with its body / location) but
			// the client does NOT chase it. This kills the redirect
			// surface + the token-leak (a redirect target cannot
			// receive the caller's Authorization header when no
			// chase happens). The dial-time IP guard (below) is the
			// belt-and-suspenders for any future code that does
			// re-enable redirects.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			// Dial-time IP guard. net.Dialer.Control runs after the
			// socket is created + after getaddrinfo but BEFORE the
			// TCP connect() syscall. Rejecting in Control means no
			// TCP SYN is sent — no port-scan side-channel against
			// IMDS / private / link-local targets. The isSafeURL
			// helper reuses the same allow/deny policy as the
			// front-door (loopback / private / metadata /
			// link-local blocked in production; the SaaS-mode
			// private-range relaxation is honored here too so an
			// intra-VPC workspace target still works).
			//
			// The Control callback runs in the dialer's goroutine,
			// so it must not block. isSafeURL is in-memory + fast
			// (does a single getaddrinfo for the IP, no network
			// round-trip for already-IP addresses).
			Transport: &http.Transport{
				DialContext:           safeDialer().DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
}

// safeDialer returns a *net.Dialer wired with safeDialControl as its
// Control callback. net.Dialer.Control runs after getaddrinfo but
// before the TCP connect() syscall, so an error return blocks the
// SYN from ever going out (no port-scan side-channel against blocked
// targets). The function exists as a constructor so the test suite
// can also construct a dialer that exercises the same control path
// without needing to spin up an http.Transport.
func safeDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:    10 * time.Second,
		KeepAlive:  30 * time.Second,
		Control:    safeDialControl,
	}
}

// safeDialControl is the net.Dialer.Control callback that enforces
// the SSRF policy at the dial layer. It runs after DNS resolution
// (the address parameter carries the resolved IP) and before the
// TCP connect() syscall — returning a non-nil error here prevents
// the SYN from going out, closing the port-scan side-channel that
// the prior POST-DIAL safeDialContext (which dialed then closed)
// left open against IMDS / private / link-local targets.
//
// Reuses isSafeURL as the policy (the same one the front-door gate
// runs) so there's a single source of truth for allow/deny. The
// "is already an IP" fast path is intentional: by the time Control
// runs, getaddrinfo has already resolved, so the address is
// `ip:port` form. If for some reason the dialer passes a hostname
// (e.g., IPv6 bracket form), the policy is still applied via the
// constructed URL — the constructor handles both shapes.
func safeDialControl(network, address string, _ syscall.RawConn) error {
	host, _, splitErr := net.SplitHostPort(address)
	if splitErr != nil {
		// Not host:port — shouldn't happen post-resolution, but if
		// it does, fall through (the dialer will surface the parse
		// error downstream; don't preempt).
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Not a literal IP — Control must have been called with
		// an unresolved hostname (Go's dialer normally resolves
		// first, but custom resolvers can defer). Re-resolve here
		// so the SSRF policy is still applied.
		//
		// FAIL-CLOSED hardening (Researcher RC 103980/12169, non-blocking
		// nit on merged #2967): if LookupIP errored OR returned an
		// empty result, treat the hostname as UNSAFE and block the
		// dial. The prior code's fall-through `return nil` would have
		// let an unresolvable / empty-result hostname through, which
		// is a fail-open. The SSRF policy is deny-by-default — a
		// hostname we can't positively verify is safe is treated as
		// unsafe.
		ips, lookupErr := net.LookupIP(host)
		if lookupErr != nil {
			log.Printf("safeDialControl: hostname %q LookupIP errored: %v (treating as unsafe — FAIL-CLOSED)", host, lookupErr)
			return &ssrfDialError{host: host, reason: fmt.Errorf("hostname resolution failed: %w", lookupErr)}
		}
		if len(ips) == 0 {
			log.Printf("safeDialControl: hostname %q LookupIP returned no addresses (treating as unsafe — FAIL-CLOSED)", host)
			return &ssrfDialError{host: host, reason: errors.New("hostname resolution returned no addresses")}
		}
		for _, candidate := range ips {
			if safeErr := isSafeURL("http://" + candidate.String() + "/"); safeErr != nil {
				return &ssrfDialError{ip: candidate, reason: safeErr}
			}
		}
		return nil
	}
	if safeErr := isSafeURL("http://" + ip.String() + "/"); safeErr != nil {
		return &ssrfDialError{ip: ip, reason: safeErr}
	}
	return nil
}

// ssrfDialError is the error type returned by safeDialControl when
// the resolved IP fails the isSafeURL policy, OR when the hostname
// could not be resolved (LookupIP error / empty result) and the
// dial-time guard is failing closed. The error message includes the
// IP (when known) OR the hostname (when the IP couldn't be resolved)
// plus the policy reason so the platform log surfaces the SSRF
// attempt (the workspace agent_card.url embedding attack in
// #2132 / RC 103771, and the unresolvable-hostname fail-closed
// hardening in RC 103980/12169). Returned from net.Dialer.Control,
// so it propagates out of DialContext as the dial error — the
// http.Client surfaces it to the caller.
type ssrfDialError struct {
	ip     net.IP   // set when the IP is known (blocked-IP case)
	host   string   // set when the hostname couldn't be resolved (LookupIP error / empty)
	reason error
}

func (e *ssrfDialError) Error() string {
	switch {
	case e.ip != nil:
		return "ssrf: dial-time IP " + e.ip.String() + " blocked: " + e.reason.Error()
	case e.host != "":
		return "ssrf: dial-time hostname " + e.host + " blocked: " + e.reason.Error()
	default:
		return "ssrf: dial-time target blocked: " + e.reason.Error()
	}
}

// Get handles GET /workspaces/:id/transcript?since=N&limit=N.
//
// Looks up the workspace's URL, mints a workspace-scoped bearer token,
// forwards the GET, and streams the response back. Caps payload at 1MB
// to keep a runaway transcript from saturating canvas.
func (h *TranscriptHandler) Get(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var workspaceURL string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT agent_card->>'url' FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&workspaceURL); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if workspaceURL == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace not registered (no URL on file)"})
		return
	}

	// workspaceURL comes from agent_card which is attacker-writable via
	// /registry/register — treat it as untrusted and validate before the
	// outbound HTTP call to prevent SSRF (issue #272 / #2130).
	// isSafeURL is the production policy used by A2A/MCP dispatch; it
	// includes DNS resolution checks and blocks loopback/private/metadata
	// targets that validateWorkspaceURL previously allowed.
	if err := isSafeURL(workspaceURL); err != nil {
		log.Printf("transcript: workspace %s URL rejected: %v", workspaceID, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace URL not allowed"})
		return
	}

	target, err := url.Parse(workspaceURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace URL"})
		return
	}
	target.Path = "/transcript"

	// Don't forward the raw query string — an attacker-controlled caller
	// could smuggle params the upstream endpoint didn't intend to expose.
	// Allowlist the two params the transcript endpoint actually uses.
	q := url.Values{}
	if since := c.Query("since"); since != "" {
		q.Set("since", since)
	}
	if limit := c.Query("limit"); limit != "" {
		q.Set("limit", limit)
	}
	target.RawQuery = q.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", target.String(), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build request"})
		return
	}

	// Forward the caller's bearer token so the workspace /transcript handler
	// (secured by #287/#328) can authenticate the proxied request.
	// WorkspaceAuth has already validated the token above — forwarding is safe.
	// Without this the workspace fails-closed (401) and the transcript feature
	// is non-functional in production. Fixes the QA-2026-04-16 finding.
	if auth := c.GetHeader("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		// Log the real error server-side (includes the target URL), but
		// don't leak it to the caller — that would reveal internal host
		// names / IPs reachable from the platform.
		log.Printf("transcript: workspace %s unreachable: %v", workspaceID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "workspace unreachable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap at 1 MB so a giant transcript doesn't melt the canvas.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read workspace response"})
		return
	}
	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

