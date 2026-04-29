package handlers

// chat_files.go — file upload/download for workspace chat.
//
// Split from templates.go because these endpoints have a different
// security model (no /configs write, no template fallback) and a
// different wire format (multipart in, binary-stream out). Template
// files are agent workspace configuration; chat files are user-agent
// conversation payloads.

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/gin-gonic/gin"
)

// ChatFilesHandler serves file upload + download for chat. It
// composes the existing TemplatesHandler's Docker plumbing
// (findContainer, execInContainer, copyFilesToContainer) rather than
// duplicating them, so a bug fix in the Docker layer propagates to
// both endpoints.
type ChatFilesHandler struct {
	templates *TemplatesHandler
}

func NewChatFilesHandler(t *TemplatesHandler) *ChatFilesHandler {
	return &ChatFilesHandler{templates: t}
}

// chatUploadMaxBytes caps the full multipart request body so a
// malicious / runaway client can't OOM the server. 50 MB covers most
// documents + a handful of images per message; larger artefacts
// should go through git/S3 rather than chat.
const chatUploadMaxBytes = 50 * 1024 * 1024

// chatUploadMaxFileBytes caps individual files in a multi-file upload.
// Keeping the per-file cap below the total lets a user send, say, a
// 5 MB PDF + 10 screenshots without tripping the batch limit on any
// single attachment.
const chatUploadMaxFileBytes = 25 * 1024 * 1024

// chatUploadDir is the in-container path where user-uploaded chat
// attachments land. Under /workspace so the file persists with the
// workspace volume and is readable by the agent without any extra
// plumbing — the agent just reads from the URI path we return.
const chatUploadDir = "/workspace/.molecule/chat-uploads"

// unsafeFilenameChars matches anything outside the conservative
// {alnum, dot, underscore, dash} set. Filenames get rewritten
// character-class at a time, so embedded paths, control chars,
// newlines, quotes, and shell metachars never reach the filesystem.
var unsafeFilenameChars = regexp.MustCompile(`[^a-zA-Z0-9._\-]`)

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

// urlPathEscape percent-encodes every byte outside the RFC 3986
// unreserved set — stricter than net/url.PathEscape (which leaves
// "/" unescaped because it's legal in URL paths). Filenames must
// never contain "/" anyway, so escaping it is defence-in-depth
// against an agent that writes a path-like name.
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

func sanitizeFilename(in string) string {
	base := filepath.Base(in)
	base = strings.ReplaceAll(base, " ", "_")
	base = unsafeFilenameChars.ReplaceAllString(base, "_")
	if len(base) > 100 {
		ext := filepath.Ext(base)
		if len(ext) > 16 {
			ext = ""
		}
		base = base[:100-len(ext)] + ext
	}
	if base == "" || base == "." || base == ".." {
		return "file"
	}
	return base
}

// ChatUploadedFile is the per-file response returned from POST
// /workspaces/:id/chat/uploads. Clients include this payload (or a
// trimmed subset) in their outgoing A2A `message/send` parts.
type ChatUploadedFile struct {
	// URI uses a custom "workspace:" scheme so clients can resolve it
	// against the streaming Download endpoint regardless of where the
	// canvas itself is hosted. The path component is always absolute
	// within the workspace container.
	URI      string `json:"uri"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType,omitempty"`
	Size     int64  `json:"size"`
}

// Upload handles POST /workspaces/:id/chat/uploads.
// Accepts multipart/form-data with one or more `files` fields, stages
// each under /workspace/.molecule/chat-uploads with a UUID prefix,
// and returns the list of URIs for the caller to attach to an A2A
// message.
func (h *ChatFilesHandler) Upload(c *gin.Context) {
	workspaceID := c.Param("id")
	if err := validateWorkspaceID(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}

	// Hard cap the request body BEFORE ParseMultipartForm — otherwise
	// a client could chunk-upload past the cap before Go notices.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, chatUploadMaxBytes)
	if err := c.Request.ParseMultipartForm(chatUploadMaxBytes); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to parse multipart form"})
		return
	}

	form := c.Request.MultipartForm
	var headers []*multipart.FileHeader
	if form != nil && form.File != nil {
		headers = form.File["files"]
	}
	if len(headers) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected at least one 'files' field"})
		return
	}

	ctx := c.Request.Context()

	containerName := h.templates.findContainer(ctx, workspaceID)
	if containerName == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace container not running"})
		return
	}

	// Build the archive in memory. Files are byte-preserving through
	// Go's string<->[]byte (the tar helper takes map[string]string but
	// the conversion is a literal copy, not a UTF-8 reinterpretation).
	archive := map[string]string{}
	uploaded := make([]ChatUploadedFile, 0, len(headers))
	for _, fh := range headers {
		if fh.Size > chatUploadMaxFileBytes {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": fmt.Sprintf("%s exceeds per-file limit (%d MB)", fh.Filename, chatUploadMaxFileBytes/(1024*1024)),
			})
			return
		}
		f, err := fh.Open()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read upload"})
			return
		}
		// LimitReader guards against a truthful-but-lying Size header:
		// if the multipart stream carries more bytes than declared, we
		// stop at the cap instead of growing the buffer.
		data, err := io.ReadAll(io.LimitReader(f, chatUploadMaxFileBytes+1))
		f.Close()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read upload"})
			return
		}
		if int64(len(data)) > chatUploadMaxFileBytes {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": fmt.Sprintf("%s exceeds per-file limit (%d MB)", fh.Filename, chatUploadMaxFileBytes/(1024*1024)),
			})
			return
		}

		name := sanitizeFilename(fh.Filename)
		// 16-byte (UUID-equivalent) random prefix. Within a single
		// batch we also check for collisions — birthday on 128 bits
		// is astronomical, but a bad PRNG or single re-used draw
		// would silently overwrite a sibling upload with its own
		// content and return two URIs pointing at one file.
		var stored string
		for attempt := 0; attempt < 4; attempt++ {
			idBytes := make([]byte, 16)
			if _, err := rand.Read(idBytes); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to allocate upload ID"})
				return
			}
			candidate := hex.EncodeToString(idBytes) + "-" + name
			if _, taken := archive[candidate]; !taken {
				stored = candidate
				break
			}
		}
		if stored == "" {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to allocate unique upload ID"})
			return
		}
		archive[stored] = string(data)

		mt := fh.Header.Get("Content-Type")
		if mt == "" {
			mt = mime.TypeByExtension(filepath.Ext(name))
		}
		uploaded = append(uploaded, ChatUploadedFile{
			URI:      "workspace:" + chatUploadDir + "/" + stored,
			Name:     name,
			MimeType: mt,
			Size:     int64(len(data)),
		})
	}

	// mkdir -p is idempotent; we fire it every upload instead of
	// caching state here so container restarts don't surprise us.
	_, _ = h.templates.execInContainer(ctx, containerName, []string{"mkdir", "-p", chatUploadDir})

	// Defence in depth: pre-remove each target path before extracting
	// the tar. An agent with write access to /workspace could in
	// theory race-create a symlink at <chatUploadDir>/<stored-name>
	// pointing at a sensitive in-container path (its own /etc/*,
	// mounted secrets). Docker's tar extraction on some drivers
	// follows pre-existing symlinks at the destination. `rm -f` the
	// exact stored-name closes that window — the UUID prefix on the
	// name makes a successful race effectively impossible, but this
	// guard costs nothing and documents the intent.
	rmArgs := []string{"rm", "-f", "--"}
	for stored := range archive {
		rmArgs = append(rmArgs, chatUploadDir+"/"+stored)
	}
	_, _ = h.templates.execInContainer(ctx, containerName, rmArgs)

	if err := h.copyFlatToContainer(ctx, containerName, chatUploadDir, archive); err != nil {
		log.Printf("Chat upload copy failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to stage files in workspace"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"files": uploaded})
}

// copyFlatToContainer extracts one tar of flat files into destPath
// inside the container. Unlike the shared copyFilesToContainer helper
// (which prepends destPath into tar entry names — correct for its
// callers whose files relative-live inside a nested tree), this
// helper writes tar entries with ONLY the flat filename so Docker's
// extraction at destPath lands them directly in destPath, not at
// destPath/destPath/... as the shared helper would.
// Filenames are validated to contain no path separator so nothing
// can escape destPath via an embedded "../" or a leading "/".
func (h *ChatFilesHandler) copyFlatToContainer(ctx context.Context, containerName, destPath string, files map[string]string) error {
	if h.templates.docker == nil {
		return fmt.Errorf("docker not available")
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		if strings.ContainsAny(name, "/\\") || name == ".." || name == "." || name == "" {
			return fmt.Errorf("unsafe flat filename: %q", name)
		}
		data := []byte(content)
		if err := tw.WriteHeader(&tar.Header{
			Name:     name, // relative — Docker resolves against destPath
			Mode:     0644,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			return fmt.Errorf("tar header %q: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return fmt.Errorf("tar write %q: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar close: %w", err)
	}
	return h.templates.docker.CopyToContainer(ctx, containerName, destPath, &buf, container.CopyToContainerOptions{})
}

// Download handles GET /workspaces/:id/chat/download?path=<abs path>.
// Streams the file bytes from the container with a correct
// Content-Type and attachment Content-Disposition. Binary-safe —
// unlike the existing JSON ReadFile endpoint which carries content
// as a string (lossy for non-UTF-8 bytes).
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
	if h.templates.docker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "docker unavailable"})
		return
	}
	containerName := h.templates.findContainer(ctx, workspaceID)
	if containerName == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace container not running"})
		return
	}

	// docker cp returns a tar stream containing the requested path.
	// For a regular file that's a single tar entry; we extract and
	// stream the body through.
	reader, _, err := h.templates.docker.CopyFromContainer(ctx, containerName, path)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}
	defer reader.Close()

	tr := tar.NewReader(reader)
	hdr, err := tr.Next()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read archive"})
		return
	}
	if hdr.Typeflag != tar.TypeReg {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is not a regular file"})
		return
	}

	name := filepath.Base(path)
	mt := mime.TypeByExtension(filepath.Ext(name))
	if mt == "" {
		mt = "application/octet-stream"
	}
	c.Header("Content-Type", mt)
	c.Header("Content-Length", fmt.Sprintf("%d", hdr.Size))
	c.Header("Content-Disposition", contentDispositionAttachment(name))
	c.Status(http.StatusOK)

	// Stream exactly hdr.Size bytes. CopyN was chosen over LimitReader
	// because it returns an error when the source is short — that
	// surfaces a bug in the tar extraction path immediately instead
	// of silently truncating. Agents can legitimately produce files
	// larger than the 50 MB upload cap (that's a per-request inbound
	// cap, not a per-artifact one), so we cannot clamp here.
	if _, err := io.CopyN(c.Writer, tr, hdr.Size); err != nil {
		log.Printf("Chat download stream error for %s (%s): %v", workspaceID, path, err)
	}
}
