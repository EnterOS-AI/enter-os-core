package handlers

// chat_files.go — file upload (HTTP-forward) + download (Docker-exec)
// for workspace chat.
//
// Upload is the v2 architecture (RFC #2312): the platform proxies the
// multipart request straight to the workspace's own /internal/chat/
// uploads/ingest endpoint. The workspace agent then writes to local
// /workspace/.molecule/chat-uploads. Same code path on local Docker
// and SaaS — the v1 docker-exec path was structurally broken in SaaS
// because workspace-server's local Docker client has no visibility
// into EC2-hosted workspaces (#2308 root cause).
//
// Download still uses the v1 docker-cp path; migrating it lives in the
// next PR in this stack so each surface is reviewable in isolation.
//
// Split from templates.go because these endpoints have a different
// security model (no /configs write, no template fallback) and a
// different wire format (multipart in, binary-stream out). Template
// files are agent workspace configuration; chat files are user-agent
// conversation payloads.

import (
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

	// Resolve workspace URL + inbound secret. Both must be present;
	// either one missing means the workspace was provisioned before
	// migration 044 or the row got into a bad state. Surface as 503
	// rather than silently failing — operators should notice.
	var wsURL string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(url, '') FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&wsURL); err != nil {
		log.Printf("chat_files Upload: workspace lookup failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if wsURL == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace url not registered yet"})
		return
	}
	// Trust note: workspaces.url passes validateAgentURL at /registry/
	// register write time, blocking SSRF-shaped URLs. We rely on that
	// upstream gate rather than re-validating here. Tracked at #2316
	// for follow-up: forward-time re-validation as defense-in-depth.

	secret, err := wsauth.ReadPlatformInboundSecret(ctx, db.DB, workspaceID)
	if err != nil {
		if errors.Is(err, wsauth.ErrNoInboundSecret) {
			// Workspace predates migration 044 OR the provisioner's
			// secret-mint hop failed. Both are operational issues,
			// not user errors. Log loudly so ops can backfill.
			log.Printf("chat_files Upload: no platform_inbound_secret for %s — workspace needs reprovision (#2312)", workspaceID)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":  "workspace not yet enrolled in v2 upload (RFC #2312)",
				"detail": "Reprovisioning the workspace will mint the platform_inbound_secret it's missing.",
			})
			return
		}
		log.Printf("chat_files Upload: read platform_inbound_secret failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read workspace secret"})
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

	// Resolve workspace URL + inbound secret. Same shape as Upload —
	// see chat_files.go::Upload for the rationale on why each missing-
	// piece path surfaces as 404 / 503.
	var wsURL string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(url, '') FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&wsURL); err != nil {
		log.Printf("chat_files Download: workspace lookup failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if wsURL == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace url not registered yet"})
		return
	}

	secret, err := wsauth.ReadPlatformInboundSecret(ctx, db.DB, workspaceID)
	if err != nil {
		if errors.Is(err, wsauth.ErrNoInboundSecret) {
			log.Printf("chat_files Download: no platform_inbound_secret for %s — workspace needs reprovision (#2312)", workspaceID)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":  "workspace not yet enrolled in v2 download (RFC #2312)",
				"detail": "Reprovisioning the workspace will mint the platform_inbound_secret it's missing.",
			})
			return
		}
		log.Printf("chat_files Download: read platform_inbound_secret failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read workspace secret"})
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

