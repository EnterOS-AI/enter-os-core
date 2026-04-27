package handlers

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/envx"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/plugins"
	"github.com/docker/docker/api/types/container"
	"github.com/gin-gonic/gin"
)

// Install-layer defaults. Overridable via env for deployments whose
// plugin sources are fast (or slow) enough to warrant different caps.
const (
	defaultInstallBodyMaxBytes = 64 * 1024         // 64 KiB JSON body cap
	defaultInstallFetchTimeout = 5 * time.Minute   // per-fetch deadline
	defaultInstallMaxDirBytes  = 100 * 1024 * 1024 // 100 MiB staged tree
)

// httpErr is the typed error returned by Install helpers. The handler
// matches it with errors.As and emits the attached status + body. Using
// a typed error instead of a 5-value tuple keeps helper signatures Go-
// idiomatic and makes them testable without a gin.Context.
type httpErr struct {
	Status int
	Body   gin.H
}

func (e *httpErr) Error() string {
	return fmt.Sprintf("%d: %v", e.Status, e.Body)
}

// newHTTPErr constructs an *httpErr without the caller worrying about
// pointer receivers. Keeps call sites terse.
func newHTTPErr(status int, body gin.H) *httpErr { return &httpErr{Status: status, Body: body} }

// installLimitsLogOnce gates the single operator-facing log line
// describing the effective install caps + timeout. sync.Once guarantees
// exactly one emission per process lifetime, regardless of how many
// PluginsHandler instances are constructed. Safe to call from any
// goroutine.
var installLimitsLogOnce sync.Once

// logInstallLimitsOnce writes the effective install limits to `w`,
// exactly once per process. Taking the writer as a parameter (instead
// of a package-level var) removes the last piece of mutable global
// state from this file — production passes os.Stderr, tests pass a
// bytes.Buffer with no t.Cleanup dance.
func logInstallLimitsOnce(w io.Writer) {
	installLimitsLogOnce.Do(func() {
		fmt.Fprintf(w,
			"Plugin install limits: body=%d bytes  timeout=%s  staged=%d bytes\n",
			envx.Int64("PLUGIN_INSTALL_BODY_MAX_BYTES", defaultInstallBodyMaxBytes),
			envx.Duration("PLUGIN_INSTALL_FETCH_TIMEOUT", defaultInstallFetchTimeout),
			envx.Int64("PLUGIN_INSTALL_MAX_DIR_BYTES", defaultInstallMaxDirBytes),
		)
	})
}

// validatePluginName ensures the name is safe (no path traversal).
func validatePluginName(name string) error {
	if name == "" {
		return fmt.Errorf("plugin name is required")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid plugin name: must not contain path separators or '..'")
	}
	if name != filepath.Base(name) {
		return fmt.Errorf("invalid plugin name")
	}
	return nil
}

// dirSize returns the total bytes of files under dir. Short-circuits
// as soon as the byte limit is exceeded so pathological inputs don't
// run the full walk.
func dirSize(dir string, limit int64) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() {
			total += info.Size()
			if total > limit {
				return fmt.Errorf("staged plugin exceeds cap of %d bytes", limit)
			}
		}
		return nil
	})
	return total, err
}

// installRequest is the decoded, validated payload a caller submits.
// Held out as its own type so resolveAndStage is testable without a
// gin.Context; the handler just decodes into this shape.
type installRequest struct {
	Source string `json:"source"`
	// SHA256 is an optional hex-encoded SHA-256 of the plugin's plugin.yaml.
	// When present, resolveAndStage verifies the fetched content matches
	// before allowing the install to proceed (SAFE-T1102 supply-chain hardening).
	SHA256 string `json:"sha256,omitempty"`
}

// stageResult bundles the outputs of resolveAndStage for the caller.
// Avoids a 5-value tuple return.
type stageResult struct {
	StagedDir  string
	PluginName string
	Source     plugins.Source
}

// resolveAndStage parses a validated request, dispatches to the right
// SourceResolver, fetches the plugin into a temp dir, and validates the
// returned name + staged size.
//
// On any error the staging tempdir (if created) is removed before return,
// and the returned *stageResult is nil. Callers own cleanup of
// result.StagedDir on success via defer os.RemoveAll.
func (h *PluginsHandler) resolveAndStage(ctx context.Context, req installRequest) (*stageResult, error) {
	if req.Source == "" {
		return nil, newHTTPErr(http.StatusBadRequest, gin.H{
			"error": "'source' is required (e.g. \"local://my-plugin\" or \"github://owner/repo\")",
		})
	}

	source, err := plugins.ParseSource(req.Source)
	if err != nil {
		return nil, newHTTPErr(http.StatusBadRequest, gin.H{"error": "invalid plugin source"})
	}
	resolver, err := h.sources.Resolve(source)
	if err != nil {
		// F1086 / #1206: include schemes so the caller can self-diagnose
		// the fix, but never the raw error message.
		return nil, newHTTPErr(http.StatusBadRequest, gin.H{
			"error":             "failed to resolve plugin source",
			"available_schemes": h.sources.Schemes(),
		})
	}
	// Front-run obvious input validation for local sources so path-
	// traversal attempts yield 400 rather than a resolver-level 502.
	if source.Scheme == "local" {
		if err := validatePluginName(source.Spec); err != nil {
			return nil, newHTTPErr(http.StatusBadRequest, gin.H{"error": "invalid plugin name"})
		}
	}

	// Pinned-ref enforcement for github:// sources (SAFE-T1102).
	// An unpinned spec (no #<tag/sha> suffix) installs from a mutable
	// default-branch tip whose content can change silently between an
	// audit and the actual install. Require explicit pinning unless the
	// operator opts in via PLUGIN_ALLOW_UNPINNED=true.
	if source.Scheme == "github" && !strings.Contains(source.Spec, "#") {
		if os.Getenv("PLUGIN_ALLOW_UNPINNED") != "true" {
			return nil, newHTTPErr(http.StatusUnprocessableEntity, gin.H{
				"error":  `unpinned github source: append a tag or commit SHA (e.g. "github://owner/repo#v1.2.0"). Set PLUGIN_ALLOW_UNPINNED=true to override`,
				"source": source.Raw(),
			})
		}
	}

	stagedDir, err := os.MkdirTemp("", "molecule-plugin-fetch-*")
	if err != nil {
		return nil, newHTTPErr(http.StatusInternalServerError, gin.H{"error": "failed to create staging dir"})
	}
	// From here, we own stagedDir. Every error path below removes it
	// before returning; the caller's defer takes over on success.
	cleanup := func() { _ = os.RemoveAll(stagedDir) }

	pluginName, err := resolver.Fetch(ctx, source.Spec, stagedDir)
	if err != nil {
		cleanup()
		log.Printf("Plugin install: resolver %s failed for %s: %v", source.Scheme, source.Spec, err)
		status := http.StatusBadGateway
		if errors.Is(err, plugins.ErrPluginNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		// F1086 / #1206: do NOT interpolate err into the response — a
		// resolver failure (github API rate-limit text, raw HTTP body,
		// file system path from a local-fs resolver) routinely contains
		// internal detail that has no business landing in the user's
		// browser. The status code already differentiates the failure
		// shape (404 not found vs 504 timeout vs 502 generic) for the
		// caller; full detail stays in the log line above.
		return nil, newHTTPErr(status, gin.H{
			"error":  fmt.Sprintf("failed to fetch plugin from %s", source.Scheme),
			"source": source.Raw(),
		})
	}
	if err := validatePluginName(pluginName); err != nil {
		cleanup()
		return nil, newHTTPErr(http.StatusBadRequest, gin.H{
			"error":  "resolver returned invalid plugin name",
			"source": source.Raw(),
		})
	}
	limit := envx.Int64("PLUGIN_INSTALL_MAX_DIR_BYTES", defaultInstallMaxDirBytes)
	if _, err := dirSize(stagedDir, limit); err != nil {
		cleanup()
		return nil, newHTTPErr(http.StatusRequestEntityTooLarge, gin.H{
			"error":  "staged plugin exceeds size limit",
			"source": source.Raw(),
		})
	}

	// Manifest-declared SHA-256 content integrity check.
	// If the staged plugin ships a manifest.json with a "sha256" field, verify
	// the declared hash matches the actual staged tree contents.
	if err := plugins.VerifyManifestIntegrity(stagedDir); err != nil {
		cleanup()
		return nil, newHTTPErr(http.StatusUnprocessableEntity, gin.H{
			"error":  "plugin manifest integrity check failed",
			"source": source.Raw(),
		})
	}

	// Caller-pinned SHA-256 content integrity check (SAFE-T1102).
	// If the caller pinned a hash, verify it against the staged plugin.yaml.
	// A mismatch means the fetched content differs from what was audited —
	// abort rather than silently install an unexpected plugin.
	if req.SHA256 != "" {
		manifestPath := filepath.Join(stagedDir, "plugin.yaml")
		manifestData, readErr := os.ReadFile(manifestPath)
		if readErr != nil {
			cleanup()
			return nil, newHTTPErr(http.StatusUnprocessableEntity, gin.H{
				"error":  "sha256 check failed: plugin.yaml not found in staged plugin",
				"source": source.Raw(),
			})
		}
		sum := sha256.Sum256(manifestData)
		got := hex.EncodeToString(sum[:])
		if !strings.EqualFold(got, req.SHA256) {
			cleanup()
			return nil, newHTTPErr(http.StatusUnprocessableEntity, gin.H{
				"error":  fmt.Sprintf("sha256 mismatch: expected %s, got %s", req.SHA256, got),
				"source": source.Raw(),
			})
		}
	}

	return &stageResult{StagedDir: stagedDir, PluginName: pluginName, Source: source}, nil
}

// deliverToContainer copies the staged plugin dir into the workspace
// container, chowns it for the agent user, and triggers a restart.
// Returns a typed *httpErr on failure; nil on success.
func (h *PluginsHandler) deliverToContainer(ctx context.Context, workspaceID string, r *stageResult) error {
	containerName := h.findRunningContainer(ctx, workspaceID)
	if containerName == "" {
		return newHTTPErr(http.StatusServiceUnavailable, gin.H{"error": "workspace container not running"})
	}
	if err := h.copyPluginToContainer(ctx, containerName, r.StagedDir, r.PluginName); err != nil {
		log.Printf("Plugin install: failed to copy %s to %s: %v", r.PluginName, workspaceID, err)
		return newHTTPErr(http.StatusInternalServerError, gin.H{"error": "failed to copy plugin to container"})
	}
	h.execAsRoot(ctx, containerName, []string{
		"chown", "-R", "1000:1000", "/configs/plugins/" + r.PluginName,
	})
	if h.restartFunc != nil {
		go h.restartFunc(workspaceID)
	}
	return nil
}

// readPluginSkillsFromContainer reads /configs/plugins/<name>/plugin.yaml
// from the running container and returns the `skills:` list. Returns an
// empty slice if the file is missing or unparseable — uninstall must keep
// running even if the manifest is gone (already half-deleted, etc.).
func (h *PluginsHandler) readPluginSkillsFromContainer(ctx context.Context, containerName, pluginName string) []string {
	out, err := h.execInContainer(ctx, containerName, []string{
		"cat", "/configs/plugins/" + pluginName + "/plugin.yaml",
	})
	if err != nil || len(out) == 0 {
		return nil
	}
	info := parseManifestYAML(pluginName, []byte(out))
	return info.Skills
}

// stripPluginMarkersFromMemory rewrites /configs/CLAUDE.md (the runtime's
// memory file) in-place, removing any block whose marker line starts with
// `# Plugin: <name> /` — mirrors AgentskillsAdaptor.uninstall's stripping
// logic so install/uninstall are symmetric. Best-effort: silent on read or
// write failure, since the rest of uninstall must still succeed.
func (h *PluginsHandler) stripPluginMarkersFromMemory(ctx context.Context, containerName, pluginName string) {
	// Use sed via bash -c for atomic in-place delete: drop the marker line
	// and the blank line that follows it (install adds a leading blank line
	// before the marker via append_to_memory). Three sed passes mirror the
	// install layout: leading blank, marker line, then we also strip empty
	// trailing markers from older installs that didn't add the prefix blank.
	// Falls through silently if CLAUDE.md doesn't exist (fresh workspace).
	marker := "# Plugin: " + pluginName + " /"
	// AgentskillsAdaptor.append_to_memory writes blocks of the shape:
	//   # Plugin: <name> / rule: foo.md
	//   <blank>
	//   <content lines…>
	// separated from the next block by a single blank line. We strip from
	// our marker up to (but not including) the next `# Plugin:` line of
	// any plugin (which marks the boundary), or EOF. Other plugins'
	// blocks and surrounding user content stay intact.
	// Block layout per AgentskillsAdaptor: marker line, one blank, content
	// lines, then a terminating blank (or EOF, or the next plugin's marker).
	// We track blanks-seen-since-marker: the 2nd blank ends our skip; any
	// `# Plugin: ` line also ends our skip (handles back-to-back blocks).
	script := fmt.Sprintf(
		`awk 'BEGIN{skip=0; blanks=0} /^%s/{skip=1; blanks=0; next} skip==1 && /^[[:space:]]*$/{blanks++; if(blanks>=2){skip=0; print; next} next} /^# Plugin: /{if(skip==1)skip=0} skip==1{next} {print}' /configs/CLAUDE.md > /tmp/claude.new && mv /tmp/claude.new /configs/CLAUDE.md`,
		regexpEscapeForAwk(marker),
	)
	_, _ = h.execAsRoot(ctx, containerName, []string{"bash", "-c", script})
}

// regexpEscapeForAwk escapes characters that have special meaning inside an
// awk ERE pattern. Plugin names go through validatePluginName so the input
// is already restricted to [A-Za-z0-9_-], but the literal `# Plugin: …/`
// prefix and a future relaxation of validatePluginName both motivate
// escaping defensively.
func regexpEscapeForAwk(s string) string {
	// `/` is the regex delimiter in awk's /.../ syntax — must be escaped
	// alongside the standard regex specials.
	specials := `\^$.|?*+()[]{}/`
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(specials, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// copyPluginToContainer creates a tar from a host directory and copies it into /configs/plugins/<name>/.
// The tar entries are prefixed with plugins/<name>/ so Docker creates the directory structure.
func (h *PluginsHandler) copyPluginToContainer(ctx context.Context, containerName, hostDir, pluginName string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := filepath.Walk(hostDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(hostDir, path)
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		// Prefix: plugins/<pluginName>/<rel> → extracts under /configs/
		header.Name = filepath.Join("plugins", pluginName, rel)

		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if _, err := tw.Write(data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create tar from %s: %w", hostDir, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("failed to close tar: %w", err)
	}

	// Copy to /configs — the tar's plugins/<name>/ prefix creates the directory
	return h.docker.CopyToContainer(ctx, containerName, "/configs", &buf, container.CopyToContainerOptions{})
}

// streamDirAsTar writes every regular file + dir under `root` to the tar
// writer, using paths relative to root so the caller's unpack produces
// `<name>/<original-layout>` without any leading tempdir components.
// Symlinks are skipped intentionally — they would usually point outside
// the staged tree and we don't want to expose platform filesystem paths.
func streamDirAsTar(root string, tw *tar.Writer) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil // skip symlinks — see doc comment
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}
