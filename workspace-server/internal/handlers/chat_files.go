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
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/pendinguploads"
)

// ChatFilesHandler serves file upload + download for chat. Holds a
// reference to TemplatesHandler so the (still docker-exec) Download
// path keeps using the shared findContainer/CopyFromContainer helpers
// without duplicating them. Upload no longer reaches into Docker.
//
// pendingUploads + broadcaster are wired only when the platform's
// migration 20260505100000 has run; nil values fall back to the
// pre-poll-mode behavior (422 on poll-mode upload, same as before).
// This lets the binary keep booting in environments where the
// migration hasn't run yet — the poll branch is gated by a not-nil
// check at the call site.
type ChatFilesHandler struct {
	templates *TemplatesHandler

	// httpClient is broken out so tests can swap in an httptest.Server
	// transport. Prod uses a default with a generous Timeout to cover
	// the 50 MB worst case on a slow EC2 link without leaving a
	// connection hanging forever on a sick workspace.
	httpClient *http.Client

	// pendingUploads is the platform-side staging layer for poll-mode
	// uploads. nil → poll branch returns 422 unchanged (the pre-feature
	// behavior); non-nil → poll branch parses multipart, persists each
	// file via storage.Put, logs a chat_upload_receive activity row,
	// and returns 200 with synthetic platform-pending: URIs.
	pendingUploads pendinguploads.Storage

	// broadcaster is the events.EventEmitter used to notify the canvas
	// when an activity row lands (so the Agent Comms panel updates
	// live). Same emitter the rest of the platform uses; nil = no
	// broadcast (tests).
	broadcaster events.EventEmitter
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

// WithPendingUploads enables the poll-mode upload branch by wiring a
// Storage + broadcaster. Call site (router.go) does this at
// construction; tests set the fields directly when they want the
// poll path exercised. Returns the handler for chained construction.
func (h *ChatFilesHandler) WithPendingUploads(storage pendinguploads.Storage, broadcaster events.EventEmitter) *ChatFilesHandler {
	h.pendingUploads = storage
	h.broadcaster = broadcaster
	return h
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
	var deliveryMode sql.NullString
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(url, ''), delivery_mode FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&wsURL, &deliveryMode); err != nil {
		log.Printf("chat_files %s: workspace lookup failed for %s: %v", op, workspaceID, err)
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return "", "", false
	}
	if wsURL == "" {
		// Distinguish the two empty-URL classes so the user sees an
		// actionable error rather than a misleading "not registered yet"
		// (which implies waiting will help):
		//
		//  push-mode → URL just isn't on the row yet (workspace
		//    restart in progress, or first /registry/register hasn't
		//    landed). 503 + "not registered yet" is correct — retry
		//    after the next heartbeat (~30s) will likely succeed.
		//
		//  anything else (poll-mode, NULL, empty string) → URL is
		//    structurally absent. The platform never dispatches to a
		//    non-push workspace, so chat upload (which is HTTP-forward
		//    by design) cannot proceed by waiting. Returning 503 here
		//    would loop the canvas client forever. 422 signals "this
		//    request can't succeed against THIS workspace's
		//    configuration" — the only fix is to re-register the
		//    workspace with a publicly-reachable URL.
		//
		// Live-observed 2026-05-04: external runtime workspaces (e.g.
		// molecule-sdk-python on a mac laptop) register with
		// delivery_mode=NULL. The narrow "poll" check missed them; the
		// invariant we actually want is "URL empty + not-push = no
		// dispatch path, ever".
		if !deliveryMode.Valid || deliveryMode.String != "push" {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":  "workspace has no callback URL — chat " + op + " requires push-mode + public URL",
				"detail": "This workspace registered without a publicly-reachable URL (delivery_mode is not 'push'). The platform cannot dispatch chat uploads to it. Re-register the workspace with a public URL in push mode (e.g. via ngrok / Cloudflare tunnel) to enable chat file " + op + ".",
			})
			return "", "", false
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace url not registered yet"})
		return "", "", false
	}
	// Trust note: workspaces.url passes validateAgentURL at /registry/
	// register write time, blocking SSRF-shaped URLs. We rely on that
	// upstream gate rather than re-validating here. Tracked at #2316
	// for follow-up: forward-time re-validation as defense-in-depth.

	secret, healed, err := readOrLazyHealInboundSecret(ctx, workspaceID, "chat_files "+op)
	if err != nil {
		// Either a non-NoInboundSecret read error (DB hiccup) or a mint
		// failure during lazy-heal. The chat_files contract is to surface
		// 503 with the RFC-#2312 reprovision hint in both cases — the user
		// can't proceed and needs ops attention.
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":  "workspace not yet enrolled in v2 " + op + " (RFC #2312)",
			"detail": "Failed to mint inbound secret. Reprovision the workspace if this persists.",
		})
		return "", "", false
	}
	if healed {
		// The platform now has the secret but the workspace's
		// /configs/.platform_inbound_secret is still empty until the next
		// /registry/register response propagates it. User retries after
		// the workspace's next heartbeat picks up the new secret (~30s).
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":               "workspace re-registering — please retry in 30 seconds",
			"detail":              "Inbound secret was just minted. Workspace will pick it up on its next heartbeat.",
			"retry_after_seconds": 30,
		})
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

	// Branch on delivery_mode BEFORE attempting the HTTP forward.
	// Push-mode workspaces continue to do the streaming forward
	// unchanged. Poll-mode workspaces (typically external runtimes
	// on a laptop, no public callback URL) get the platform-side
	// staging path — the file lands in pending_uploads, an activity
	// row goes into the inbox queue, and the workspace pulls on its
	// next poll cycle.
	if h.pendingUploads != nil {
		mode, modeOK := lookupUploadDeliveryMode(c, ctx, workspaceID)
		if !modeOK {
			return
		}
		if mode == "poll" {
			h.uploadPollMode(c, ctx, workspaceID)
			return
		}
	}

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

	h.streamWorkspaceResponse(c, "upload", workspaceID, forwardURL, req, []string{"Content-Type"})
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

	h.streamWorkspaceResponse(c, "download", workspaceID, forwardURL, req,
		[]string{"Content-Type", "Content-Length", "Content-Disposition"})
}

// streamWorkspaceResponse executes the prepared forward request and
// streams the workspace's response back to the inbound caller.
// Forwards the named response headers verbatim. Centralizes the
// "do request → check err → defer close → copy headers → set status →
// io.Copy" tail that's identical between Upload and Download.
//
// op is the human-readable feature label ("upload"/"download") used
// in log messages so operators can distinguish which feature ran.
func (h *ChatFilesHandler) streamWorkspaceResponse(
	c *gin.Context,
	op, workspaceID, forwardURL string,
	req *http.Request,
	forwardHeaders []string,
) {
	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("chat_files %s: forward to %s failed: %v", op, forwardURL, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "workspace unreachable"})
		return
	}
	defer resp.Body.Close()

	for _, hdr := range forwardHeaders {
		if v := resp.Header.Get(hdr); v != "" {
			c.Header(hdr, v)
		}
	}
	c.Status(resp.StatusCode)
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		// Mid-stream failure — too late to write a JSON error, just
		// log so ops can correlate with the workspace's logs.
		log.Printf("chat_files %s: stream response back failed for %s: %v", op, workspaceID, err)
	}
}


// lookupUploadDeliveryMode returns the workspace's delivery_mode
// for the chat upload branch. Returns ("", false) and writes the
// HTTP error response on lookup failure (caller stops). NULL or
// empty delivery_mode is treated as "push" — that's the schema
// default and matches the legacy pre-#2339 behavior. Only the
// explicit string "poll" routes the upload through the poll-mode
// branch.
//
// Why a dedicated helper instead of reusing lookupDeliveryMode
// from a2a_proxy_helpers.go: that one swallows errors and falls
// back to "push" so the proxy keeps working on a transient DB
// hiccup. For upload we want to surface the not-found case as 404
// (which the workspace-poll branch wouldn't otherwise hit, since
// the workspace-side row IS the source of truth for the mode).
func lookupUploadDeliveryMode(c *gin.Context, ctx context.Context, workspaceID string) (string, bool) {
	var mode sql.NullString
	err := db.DB.QueryRowContext(ctx,
		`SELECT delivery_mode FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return "", false
	}
	if err != nil {
		log.Printf("chat_files Upload: delivery_mode lookup failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delivery_mode lookup failed"})
		return "", false
	}
	if !mode.Valid || mode.String == "" {
		return "push", true
	}
	return mode.String, true
}

// unsafeFilenameChars matches every character that isn't in the safe
// alphanumeric + dot/dash/underscore set. Mirrors the Python regex
// _UNSAFE_FILENAME_CHARS in workspace/internal_chat_uploads.py — drift
// here would mean canvas-emitted URIs differ between push and poll
// paths for the same upload.
var unsafeFilenameChars = regexp.MustCompile(`[^a-zA-Z0-9._\-]`)

// SanitizeFilename reduces a user-supplied filename to a safe form.
// Behaviorally identical to sanitize_filename in workspace/
// internal_chat_uploads.py. Exported so tests in other packages can
// pin behavior parity, and so a future shared library can move both
// implementations behind one source of truth.
func SanitizeFilename(name string) string {
	base := filepath.Base(name)
	// filepath.Base on a path-traversal input ("../../etc/passwd")
	// returns "passwd" (just the last component) — which matches what
	// Python's os.path.basename does. Tests pin both here and on the
	// Python side.
	base = strings.ReplaceAll(base, " ", "_")
	base = unsafeFilenameChars.ReplaceAllString(base, "_")
	if len(base) > 100 {
		ext := ""
		dot := strings.LastIndex(base, ".")
		if dot >= 0 && len(base)-dot <= 16 {
			ext = base[dot:]
		}
		base = base[:100-len(ext)] + ext
	}
	if base == "" || base == "." || base == ".." {
		return "file"
	}
	return base
}

// uploadedFile is the per-file response shape the workspace-side
// /internal/chat/uploads/ingest also produces. Mirroring the schema
// keeps the canvas client unaware of which path handled the upload.
type uploadedFile struct {
	URI      string `json:"uri"`
	Name     string `json:"name"`
	Mimetype string `json:"mimeType"`
	Size     int64  `json:"size"`
}

// uploadPollMode handles a chat upload bound for a poll-mode
// workspace. Parses the multipart in-place, persists each file via
// pendinguploads.Storage, and logs one chat_upload_receive activity
// row per file so the workspace's inbox poller picks them up on its
// next cycle.
//
// Why one activity row per file (not one per multipart batch):
//   - Each row carries one URI; agents that consume the inbox treat
//     each row as one inbound event. A batch row would force every
//     consumer to deserialize a list, doubling the field-shape
//     surface for no UX win.
//   - At-least-once semantics: a workspace can ack files
//     individually. Batch ack would leak partial-success state on
//     a fetcher crash mid-batch.
//
// Limits enforced here mirror the workspace-side ingest_handler:
//   - Total body cap: 50 MB (set on c.Request.Body before reaching us)
//   - Per-file cap: 25 MB (pendinguploads.MaxFileBytes; rejected as 413)
//   - Filename: sanitized + capped at 100 chars (SanitizeFilename)
//
// Logging: every persisted file logs an INFO line with workspace_id,
// file_id, size, and sanitized name. Failure modes (oversize, missing
// files field, malformed multipart) log at WARN with the same fields.
// Phase 3 metrics will hook these structured logs.
func (h *ChatFilesHandler) uploadPollMode(c *gin.Context, ctx context.Context, workspaceID string) {
	// Parse multipart with the same per-file/per-form limits the
	// workspace-side handler uses (workspace/internal_chat_uploads.py:
	// max_files=64, max_fields=32). gin's MultipartForm does not
	// expose those limits directly — the underlying ParseMultipartForm
	// caps memory at 32 MB by default and spills to disk. For poll-
	// mode we read each file into memory to hand to Storage.Put;
	// 25 MB-per-file × 64-files ceiling means worst-case is 1.6 GB of
	// peak memory. Bound the per-file size at the multipart layer so
	// the spill never gets close.
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		log.Printf("chat_files uploadPollMode: parse multipart failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "malformed multipart body"})
		return
	}
	form := c.Request.MultipartForm
	if form == nil || len(form.File["files"]) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no files field in request"})
		return
	}
	headers := form.File["files"]
	if len(headers) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "too many files (limit 64)"})
		return
	}

	wsUUID, err := uuid.Parse(workspaceID)
	if err != nil {
		// validateWorkspaceID at the top of Upload already gates this;
		// the re-parse is defence in depth in case validateWorkspaceID
		// drifts. Keep the error class consistent so a bad-id reaches
		// the same 400 path. Not separately tested because the gate at
		// the call site is structurally the same uuid.Parse.
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}

	// Phase 1: pre-validate + read every part BEFORE any DB write.
	// A multi-file upload must commit all-or-nothing; a per-file
	// failure halfway through used to leave rows 1..K-1 in the table
	// while the client got a 500 and retried the whole batch — duplicate
	// rows, orphan activity rows. Validating up-front + atomic PutBatch
	// closes that gap.
	type prepped struct {
		Sanitized string
		Mimetype  string
		Content   []byte
		Original  string // original (unsanitized) filename for error messages
	}
	prepReady := make([]prepped, 0, len(headers))
	items := make([]pendinguploads.PutItem, 0, len(headers))
	for _, fh := range headers {
		if fh.Size > pendinguploads.MaxFileBytes {
			log.Printf("chat_files uploadPollMode: per-file cap exceeded for %s: %s (%d bytes)",
				workspaceID, fh.Filename, fh.Size)
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":    "file exceeds per-file cap",
				"filename": fh.Filename,
				"size":     fh.Size,
				"max":      pendinguploads.MaxFileBytes,
			})
			return
		}
		content, err := readMultipartFile(fh)
		if err != nil {
			log.Printf("chat_files uploadPollMode: read part failed for %s/%s: %v",
				workspaceID, fh.Filename, err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "could not read file part"})
			return
		}
		// Belt-and-braces post-read cap (multipart.FileHeader.Size can lie
		// on some clients that don't set Content-Length per part).
		if len(content) > pendinguploads.MaxFileBytes {
			log.Printf("chat_files uploadPollMode: per-file cap exceeded post-read for %s: %s (%d bytes)",
				workspaceID, fh.Filename, len(content))
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":    "file exceeds per-file cap",
				"filename": fh.Filename,
				"size":     len(content),
				"max":      pendinguploads.MaxFileBytes,
			})
			return
		}
		sanitized := SanitizeFilename(fh.Filename)
		mimetype := safeMimetype(fh.Header.Get("Content-Type"))
		prepReady = append(prepReady, prepped{
			Sanitized: sanitized, Mimetype: mimetype, Content: content, Original: fh.Filename,
		})
		items = append(items, pendinguploads.PutItem{
			Content: content, Filename: sanitized, Mimetype: mimetype,
		})
	}

	// Phase 2+3: PutBatch + N activity-row inserts run in ONE Tx so
	// either every pending_uploads row + every activity_logs row commits,
	// or none do. Per-file pre-validation already happened above so the
	// only failure modes inside the Tx are DB-side; either way Rollback
	// leaves the table state unchanged and the client retries the whole
	// multipart upload cleanly. Broadcasts are deferred until after
	// Commit — emitting an ACTIVITY_LOGGED event for a row that ends up
	// rolled back would leak a ghost message into the canvas's
	// optimistic UI.
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("chat_files uploadPollMode: begin tx for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not stage files"})
		return
	}
	// Defer-rollback is safe even after a successful Commit — the second
	// Rollback is a no-op (database/sql tracks tx state).
	defer func() {
		_ = tx.Rollback()
	}()

	fileIDs, err := h.pendingUploads.PutBatchTx(ctx, tx, wsUUID, items)
	if err != nil {
		if errors.Is(err, pendinguploads.ErrTooLarge) {
			// Belt + suspenders: pre-validation above already caught
			// this; surface a clean 413 if a malformed FileHeader
			// somehow slipped through.
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": "one or more files exceed per-file cap",
				"max":   pendinguploads.MaxFileBytes,
			})
			return
		}
		log.Printf("chat_files uploadPollMode: storage.PutBatchTx failed for %s: %v",
			workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not stage files"})
		return
	}

	out := make([]uploadedFile, 0, len(prepReady))
	broadcasts := make([]func(), 0, len(prepReady))
	for i, p := range prepReady {
		fileID := fileIDs[i]
		uri := fmt.Sprintf("platform-pending:%s/%s", workspaceID, fileID)
		summary := "chat_upload_receive: " + p.Sanitized
		method := "chat_upload_receive"
		hook, err := LogActivityTx(ctx, tx, h.broadcaster, ActivityParams{
			WorkspaceID:  workspaceID,
			ActivityType: "a2a_receive",
			TargetID:     &workspaceID,
			Method:       &method,
			Summary:      &summary,
			RequestBody: map[string]interface{}{
				"file_id":  fileID.String(),
				"name":     p.Sanitized,
				"mimeType": p.Mimetype,
				"size":     len(p.Content),
				"uri":      uri,
			},
			Status: "ok",
		})
		if err != nil {
			log.Printf("chat_files uploadPollMode: activity insert failed for %s/%s: %v",
				workspaceID, p.Sanitized, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not log upload activity"})
			return
		}
		broadcasts = append(broadcasts, hook)
		out = append(out, uploadedFile{
			URI:      uri,
			Name:     p.Sanitized,
			Mimetype: p.Mimetype,
			Size:     int64(len(p.Content)),
		})
	}

	if err := tx.Commit(); err != nil {
		log.Printf("chat_files uploadPollMode: commit failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not stage files"})
		return
	}

	// Post-commit: fire deferred broadcasts and emit the staged log
	// lines now that the rows are durable. Broadcasts are pure in-memory
	// (no I/O); panicking here would NOT leak a row but would leak a
	// log line, so the order doesn't matter for correctness.
	for _, b := range broadcasts {
		b()
	}
	for i, p := range prepReady {
		log.Printf("chat_files uploadPollMode: staged %s/%s (file_id=%s size=%d mimetype=%q)",
			workspaceID, p.Sanitized, fileIDs[i], len(p.Content), p.Mimetype)
	}

	c.JSON(http.StatusOK, gin.H{"files": out})
}

// safeMimetype validates a multipart-supplied Content-Type header and
// returns a sanitized value safe to store + serve back unmodified.
//
// The platform's GET /content handler reflects the stored mimetype as
// the response Content-Type. An attacker-controlled header that
// embedded CR/LF could split the response (header injection); a value
// containing semicolons could carry an unexpected charset parameter
// that confuses a downstream renderer. Strip CR/LF/control chars +
// keep only the type/subtype prefix; reject anything that doesn't
// match a basic `type/subtype` regex by falling back to the safe
// default (application/octet-stream — the workspace-side handler does
// the same fallback).
func safeMimetype(raw string) string {
	const fallback = "application/octet-stream"
	// Trim parameters (`text/html; charset=utf-8` → `text/html`).
	if i := strings.IndexByte(raw, ';'); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Reject if any control char or whitespace is present (header
	// injection defense). RFC 7231 mimetype grammar forbids whitespace.
	for _, r := range raw {
		if r < 0x21 || r > 0x7e {
			return fallback
		}
	}
	// Require exactly one slash separating type and subtype.
	parts := strings.Split(raw, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fallback
	}
	return raw
}

// readMultipartFile reads a multipart part fully into memory. Wraps
// the open + io.ReadAll + close idiom so the call site stays clean,
// and so a future change (chunked reads / hashing) has one place to
// land.
func readMultipartFile(fh *multipartFileHeader) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, fmt.Errorf("open part: %w", err)
	}
	defer f.Close()
	return io.ReadAll(f)
}

// multipartFileHeader is a local alias so the readMultipartFile
// signature doesn't pull "mime/multipart" into every test that
// touches uploadPollMode.
type multipartFileHeader = multipart.FileHeader
