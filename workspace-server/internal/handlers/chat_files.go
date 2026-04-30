package handlers

// chat_files.go — file upload + download for workspace chat,
// both HTTP-forward (RFC #2312, fully landed).
//
// Architecture (v2, post-RFC-#2312):
//
//   - Upload (POST /workspaces/:id/uploads): the platform proxies the
//     multipart request straight to the workspace's own
//     /internal/chat/uploads/ingest endpoint. The workspace agent then
//     writes to local /workspace/.molecule/chat-uploads.
//
//   - Download (GET /workspaces/:id/files): the platform makes an HTTP
//     GET to the workspace's /internal/file/read?path=<abs> endpoint
//     and streams the response body to the caller.
//
// Same code path on local Docker and SaaS — the v1 docker-exec /
// docker-cp paths were structurally broken in SaaS because
// workspace-server's local Docker client has no visibility into
// EC2-hosted workspaces (#2308 root cause). Both surfaces now use the
// per-workspace platform_inbound_secret minted at provision time
// (RFC #2312 PR-F) for auth, and the workspace's HTTP server mounts
// the corresponding receiver at workspace/main.py.
//
// Split from templates.go because these endpoints have a different
// security model (no /configs write, no template fallback) and a
// different wire format (multipart in, binary-stream out). Template
// files are agent workspace configuration; chat files are user-agent
// conversation payloads.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/wsauth"
	"github.com/gin-gonic/gin"
)

// ChatFilesHandler serves file upload + download for chat. Holds a
// reference to TemplatesHandler so the (still docker-exec) Download
// path keeps using the shared findContainer/CopyFromContainer helpers
// without duplicating them. Upload no longer reaches into Docker.
type ChatFilesHandler struct {
	templates *TemplatesHandler

	// httpClient is broken out so tests can swap in an httptest.Server
	// transport. Prod uses a default with a generous Timeout to cover
	// the 50 MB worst case on a slow EC2 link without leaving a
	// connection hanging forever on a sick workspace.
	httpClient *http.Client
}

func NewChatFilesHandler(t *TemplatesHandler) *ChatFilesHandler {
	return &ChatFilesHandler{
		templates: t,
		httpClient: &http.Client{
			// 50 MB total body cap / ~1 MB/s slow-network floor → ~60s.
			// Doubled for headroom on the legitimate-but-slow case.
			Timeout: 120 * time.Second,
		},
	}
}

// chatUploadMaxBytes caps the full multipart request body so a
// malicious / runaway client can't OOM the proxy hop. 50 MB matches
// the workspace-side limit; anything larger is rejected at the
// network boundary before forwarding.
const chatUploadMaxBytes = 50 * 1024 * 1024

// chatUploadDir is the in-container path where user-uploaded chat
// attachments land. Kept here for documentation parity with the
// workspace-side handler — the platform no longer writes files
// directly, but the URI scheme returned in responses still uses this
// path, so any consumer parsing those URIs has the constant to
// reference.
const chatUploadDir = "/workspace/.molecule/chat-uploads"

// resolveWorkspaceForwardCreds resolves the workspace's URL +
// platform_inbound_secret for an /internal/* forward, applying
// lazy-heal on a missing inbound secret (RFC #2312 backfill — the
// 2026-04-30 fix that closes the existing-workspace gap left by the
// shared-mint refactor).
//
// On any failure path the function HAS ALREADY written the appropriate
// status + JSON body to c (404 / 503 / 500) and returns ok=false.
// On success returns the URL + secret + ok=true.
//
// op is the human-readable feature label ("upload"/"download") used
// in log messages and the 503 RFC-#2312 detail copy so operators can
// distinguish which feature ran.
//
// Centralized here (rather than inline in Upload + Download) so the
// next forward-time condition we add — secret rotation, audit, etc. —
// goes in ONE place. Drift between the two handlers is the same class
// of bug as the original SaaS provision drift fixed in #2366; this
// extraction prevents that class on the consumer side.
func resolveWorkspaceForwardCreds(c *gin.Context, ctx context.Context, workspaceID, op string) (wsURL, secret string, ok bool) {
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(url, '') FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&wsURL); err != nil {
		log.Printf("chat_files %s: workspace lookup failed for %s: %v", op, workspaceID, err)
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return "", "", false
	}
	if wsURL == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace url not registered yet"})
		return "", "", false
	}
	// Trust note: workspaces.url passes validateAgentURL at /registry/
	// register write time, blocking SSRF-shaped URLs. We rely on that
	// upstream gate rather than re-validating here. Tracked at #2316
	// for follow-up: forward-time re-validation as defense-in-depth.

	secret, err := wsauth.ReadPlatformInboundSecret(ctx, db.DB, workspaceID)
	if err != nil {
		if errors.Is(err, wsauth.ErrNoInboundSecret) {
			// Lazy-heal: mint the secret now so future requests
			// succeed. Pre-2026-04-30 the SaaS provision path didn't
			// mint — every prod workspace started life with NULL
			// platform_inbound_secret. The shared-mint refactor closes
			// the new-workspace gap; this lazy-heal closes the
			// existing-workspace gap without requiring a destructive
			// reprovision.
			//
			// Why the request still 503s: the workspace's local
			// /configs/.platform_inbound_secret is also empty until the
			// next /registry/register response (registry.go:344-362)
			// propagates the freshly-minted secret. The user retries
			// once the workspace's heartbeat picks up the new secret
			// (typically <30s).
			if _, mintErr := wsauth.IssuePlatformInboundSecret(ctx, db.DB, workspaceID); mintErr != nil {
				log.Printf("chat_files %s: lazy-heal mint failed for %s: %v", op, workspaceID, mintErr)
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error":  "workspace not yet enrolled in v2 " + op + " (RFC #2312)",
					"detail": "Failed to mint inbound secret. Reprovision the workspace if this persists.",
				})
				return "", "", false
			}
			log.Printf("chat_files %s: lazy-healed platform_inbound_secret for %s — retry once workspace re-registers (#2312 backfill)", op, workspaceID)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":               "workspace re-registering — please retry in 30 seconds",
				"detail":              "Inbound secret was just minted. Workspace will pick it up on its next heartbeat.",
				"retry_after_seconds": 30,
			})
			return "", "", false
		}
		log.Printf("chat_files %s: read platform_inbound_secret failed for %s: %v", op, workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read workspace secret"})
		return "", "", false
	}
	return wsURL, secret, true
}

// urlPathEscape percent-encodes every byte outside the RFC 3986
// unreserved set — stricter than net/url.PathEscape (which leaves
// "/" unescaped because it's legal in URL paths). Filenames must
// never contain "/" anyway, so escaping it is defence-in-depth
// against an agent that writes a path-like name.
//
// Used by Download's Content-Disposition header.
func urlPathEscape(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	var b strings.Builder
	for _, c := range []byte(s) {
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// contentDispositionAttachment produces a safe `attachment; filename=...`
// header. Quotes, CR, and LF in the filename are escaped per RFC 6266 /
// RFC 5987: control chars dropped, backslash and double-quote
// backslash-escaped inside the quoted-string. Also emits the
// percent-encoded filename* parameter so non-ASCII names survive.
// This matters because agents can write arbitrary filenames into
// /workspace, and anything they produce reaches this header via
// `filepath.Base(path)` — not all agents sanitize on their side.
func contentDispositionAttachment(name string) string {
	safeQ := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r == '\r' || r == '\n':
			// Drop — any CR/LF would terminate the header early.
			continue
		case r == '"' || r == '\\':
			// Escape per RFC 6266 §4.1 quoted-string.
			safeQ = append(safeQ, '\\', r)
		case r < 0x20 || r == 0x7f:
			// Drop other control chars.
			continue
		default:
			safeQ = append(safeQ, r)
		}
	}
	asciiSafe := string(safeQ)
	// filename=  — double-quoted, escaped. Gives legacy clients a value.
	// filename*= — RFC 5987 percent-encoded UTF-8, preferred when present.
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		asciiSafe, urlPathEscape(name))
}

// Upload handles POST /workspaces/:id/chat/uploads.
//
// Streams the multipart body straight to the workspace's own
// /internal/chat/uploads/ingest endpoint with the platform_inbound_secret
// (RFC #2312, migration 044) in the Authorization header. The workspace
// validates and writes to its local /workspace/.molecule/chat-uploads;
// the response (containing one ChatUploadedFile per upload) is streamed
// back unchanged.
//
// Why streaming, not parse-then-re-encode:
//   - Eliminates the 50 MB intermediate buffer on the platform.
//   - Per-file size + path-safety enforcement is the workspace's job;
//     duplicating it here just creates two places to keep in sync.
//   - The error responses from the workspace (413 with the offending
//     filename, 400 on missing files field, etc.) propagate through
//     unchanged, so the user sees the same shapes regardless of where
//     the failure originated.
func (h *ChatFilesHandler) Upload(c *gin.Context) {
	workspaceID := c.Param("id")
	if err := validateWorkspaceID(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}

	// Hard cap the request body BEFORE forwarding. http.MaxBytesReader
	// enforces lazily as the body is read; a malicious client cannot
	// chunk-upload past the cap, the wrapped reader returns an error
	// when the cap is exceeded and the workspace receives a truncated
	// stream that fails its own multipart parser.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, chatUploadMaxBytes)

	ctx := c.Request.Context()

	wsURL, secret, ok := resolveWorkspaceForwardCreds(c, ctx, workspaceID, "upload")
	if !ok {
		return
	}

	// Build the forward request. Body is the (capped) reader from the
	// inbound request — Go's http.Client streams it directly to the
	// workspace, no intermediate buffering on the platform.
	forwardURL := strings.TrimRight(wsURL, "/") + "/internal/chat/uploads/ingest"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, forwardURL, c.Request.Body)
	if err != nil {
		log.Printf("chat_files Upload: build request failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to construct forward request"})
		return
	}
	// Forward the multipart Content-Type (with boundary) verbatim;
	// without it the workspace's parser cannot find part boundaries.
	if ct := c.Request.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	// Pass through Content-Length so the workspace can short-circuit
	// the total-body cap before parsing. ContentLength on the request
	// struct also lets Go's transport know whether to stream or send
	// chunked-encoded.
	if c.Request.ContentLength > 0 {
		req.ContentLength = c.Request.ContentLength
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("chat_files Upload: forward to %s failed: %v", forwardURL, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "workspace unreachable"})
		return
	}
	defer resp.Body.Close()

	// Stream response back. Copy headers we know are safe + the body.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	}
	c.Status(resp.StatusCode)
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		// Mid-stream failure — too late to write a JSON error, just
		// log so ops can correlate with the workspace's logs.
		log.Printf("chat_files Upload: stream response back failed for %s: %v", workspaceID, err)
	}
}

// Download handles GET /workspaces/:id/chat/download?path=<abs path>.
// Forwards over HTTP to the workspace's own /internal/file/read endpoint
// (RFC #2312 PR-D), replacing the docker-cp tar-stream extraction that
// only worked when the platform binary had local Docker socket access.
//
// Same path-safety contract as the legacy version: caller-side validation
// is duplicated on the workspace side (internal_file_read.py) so a
// platform bug or malicious caller bypassing one layer still hits the
// other. This is "defence in depth via two parallel checks," not "trust
// the workspace to validate" — the workspace doesn't trust the platform
// either.
//
// Body is streamed end-to-end (no buffering on the platform), preserving
// binary safety and arbitrary file size (the 50 MB cap on Upload doesn't
// apply to artefacts the agent produced).
func (h *ChatFilesHandler) Download(c *gin.Context) {
	workspaceID := c.Param("id")
	if err := validateWorkspaceID(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}

	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path query required"})
		return
	}
	if !filepath.IsAbs(path) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path must be absolute"})
		return
	}
	// Path must land under one of the allowed roots — mirrors the
	// ReadFile security model and prevents arbitrary reads of /etc
	// or other system paths via this endpoint.
	rooted := false
	for root := range allowedRoots {
		if path == root || strings.HasPrefix(path, root+"/") {
			rooted = true
			break
		}
	}
	if !rooted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path must be under /configs, /workspace, /home, or /plugins"})
		return
	}
	// Reject anything that canonicalises differently or contains a
	// traversal segment. Defence-in-depth on top of the prefix check.
	if filepath.Clean(path) != path || strings.Contains(path, "..") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}

	ctx := c.Request.Context()

	wsURL, secret, ok := resolveWorkspaceForwardCreds(c, ctx, workspaceID, "download")
	if !ok {
		return
	}

	// Build forward URL with the validated path encoded as a query param.
	// url.Values handles all the percent-encoding correctly — a path with
	// special chars (spaces, &, +) round-trips through both the platform's
	// validator and the workspace-side validator.
	forwardURL := strings.TrimRight(wsURL, "/") + "/internal/file/read?path=" + url.QueryEscape(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, forwardURL, nil)
	if err != nil {
		log.Printf("chat_files Download: build request failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to construct forward request"})
		return
	}
	req.Header.Set("Authorization", "Bearer "+secret)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("chat_files Download: forward to %s failed: %v", forwardURL, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "workspace unreachable"})
		return
	}
	defer resp.Body.Close()

	// Stream response back, including the workspace's headers so the
	// client gets the correct Content-Type + Content-Disposition (the
	// workspace constructs them from the actual file's extension +
	// basename — keeping that logic on the workspace side avoids a
	// double-source-of-truth on filename encoding rules).
	for _, hdr := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
		if v := resp.Header.Get(hdr); v != "" {
			c.Header(hdr, v)
		}
	}
	c.Status(resp.StatusCode)
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		log.Printf("chat_files Download: stream response back failed for %s: %v", workspaceID, err)
	}
}

