package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerimage "github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/gin-gonic/gin"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
)

// WorkspaceImageService is the production-side end of the runtime CD chain.
// It (1) pulls workspace template images from GHCR via the Docker SDK and
// (2) recreates running ws-* containers so they adopt the new image.
//
// Two callers:
//   - AdminWorkspaceImagesHandler — POST /admin/workspace-images/refresh, the
//     manual end-of-chain trigger documented in
//     docs/workspace-runtime-package.md.
//   - imagewatch.Watcher — the auto-refresh goroutine that polls GHCR
//     digests and invokes Refresh when an image changes upstream. This is
//     what closes the chain to "merge → containers running new code" with
//     no human in between.
type WorkspaceImageService struct {
	docker *dockerclient.Client
}

func NewWorkspaceImageService(docker *dockerclient.Client) *WorkspaceImageService {
	return &WorkspaceImageService{docker: docker}
}

// AllRuntimes is the canonical list mirroring docs/workspace-runtime-package.md.
// Update both when a new template is added.
var AllRuntimes = []string{
	"claude-code", "langgraph", "crewai", "autogen",
	"deepagents", "hermes", "gemini-cli", "openclaw",
}

// RefreshResult is the per-call outcome surfaced to HTTP callers AND logged
// by the auto-refresh watcher.
type RefreshResult struct {
	Pulled    []string `json:"pulled"`
	Failed    []string `json:"failed"`
	Recreated []string `json:"recreated"`
}

// TemplateImageRef returns the canonical GHCR ref for a runtime's template
// image. Single source of truth shared with imagewatch.
func TemplateImageRef(runtime string) string {
	return fmt.Sprintf("ghcr.io/molecule-ai/workspace-template-%s:latest", runtime)
}

// ghcrAuthHeader returns the base64-encoded JSON auth payload Docker's
// ImagePull expects in PullOptions.RegistryAuth, or empty string when no
// GHCR_USER/GHCR_TOKEN env is set (lets public images pull through).
//
// The Docker SDK doesn't read ~/.docker/config.json — every authenticated
// pull needs an explicit RegistryAuth string.
func ghcrAuthHeader() string {
	user := strings.TrimSpace(os.Getenv("GHCR_USER"))
	token := strings.TrimSpace(os.Getenv("GHCR_TOKEN"))
	if user == "" || token == "" {
		return ""
	}
	payload := map[string]string{
		"username":      user,
		"password":      token,
		"serveraddress": "ghcr.io",
	}
	js, err := json.Marshal(payload)
	if err != nil {
		log.Printf("workspace-images: failed to marshal GHCR auth: %v", err)
		return ""
	}
	return base64.URLEncoding.EncodeToString(js)
}

// Refresh pulls the requested runtimes' template images from GHCR and (if
// recreate) force-removes any matching ws-* containers so the platform
// re-provisions them on next interaction.
//
// Soft-fails per runtime: one missing image (e.g. unpublished template)
// doesn't abort the others. Per-runtime failures are in RefreshResult.Failed.
// Returns a non-nil error only when the recreate phase couldn't enumerate
// containers at all (caller should surface that as 500).
func (s *WorkspaceImageService) Refresh(ctx context.Context, runtimes []string, recreate bool) (RefreshResult, error) {
	res := RefreshResult{Pulled: []string{}, Failed: []string{}, Recreated: []string{}}
	auth := ghcrAuthHeader()

	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for _, rt := range runtimes {
		image := TemplateImageRef(rt)
		opts := dockerimage.PullOptions{Platform: provisioner.DefaultImagePlatform()}
		if auth != "" {
			opts.RegistryAuth = auth
		}
		rc, err := s.docker.ImagePull(pullCtx, image, opts)
		if err != nil {
			log.Printf("workspace-images/refresh: pull %s failed: %v", rt, err)
			res.Failed = append(res.Failed, rt)
			continue
		}
		// Drain to completion. The engine treats early-close as "abandon",
		// leaving partial layers around with no reference.
		if _, err := io.Copy(io.Discard, rc); err != nil {
			rc.Close()
			log.Printf("workspace-images/refresh: drain %s failed: %v", rt, err)
			res.Failed = append(res.Failed, rt)
			continue
		}
		rc.Close()
		res.Pulled = append(res.Pulled, rt)
	}

	if !recreate {
		return res, nil
	}

	listCtx, listCancel := context.WithTimeout(ctx, 30*time.Second)
	defer listCancel()
	containers, err := s.docker.ContainerList(listCtx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", "ws-")),
	})
	if err != nil {
		log.Printf("workspace-images/refresh: container list failed: %v", err)
		return res, fmt.Errorf("container list: %w", err)
	}

	pulledSet := map[string]struct{}{}
	for _, rt := range res.Pulled {
		pulledSet[rt] = struct{}{}
	}
	for _, ctr := range containers {
		// ContainerList's ctr.Image is the *resolved digest* (sha256:…),
		// not the human-readable tag. Inspect to get Config.Image so we
		// can match against the pulled-runtime set.
		inspectCtx, inspectCancel := context.WithTimeout(ctx, 10*time.Second)
		full, err := s.docker.ContainerInspect(inspectCtx, ctr.ID)
		inspectCancel()
		if err != nil {
			log.Printf("workspace-images/refresh: inspect %s failed: %v", ctr.ID[:12], err)
			continue
		}
		imageRef := ""
		if full.Config != nil {
			imageRef = full.Config.Image
		}
		matched := ""
		for rt := range pulledSet {
			if strings.Contains(imageRef, "workspace-template-"+rt) {
				matched = rt
				break
			}
		}
		if matched == "" {
			continue
		}
		name := strings.TrimPrefix(ctr.Names[0], "/")
		rmCtx, rmCancel := context.WithTimeout(ctx, 30*time.Second)
		err = s.docker.ContainerRemove(rmCtx, ctr.ID, container.RemoveOptions{Force: true})
		rmCancel()
		if err != nil {
			log.Printf("workspace-images/refresh: remove %s failed: %v", name, err)
			continue
		}
		res.Recreated = append(res.Recreated, name)
	}
	return res, nil
}

// AdminWorkspaceImagesHandler serves POST /admin/workspace-images/refresh.
//
//	?runtime=claude-code   (optional; default = all 8 templates)
//	&recreate=true|false   (default true; false = pull only)
//
// Returns JSON {pulled: [...], failed: [...], recreated: [...]}
type AdminWorkspaceImagesHandler struct {
	svc *WorkspaceImageService
}

func NewAdminWorkspaceImagesHandler(docker *dockerclient.Client) *AdminWorkspaceImagesHandler {
	return &AdminWorkspaceImagesHandler{svc: NewWorkspaceImageService(docker)}
}

// Service exposes the underlying refresh logic so the auto-refresh watcher
// in cmd/server can share the exact code path the HTTP handler uses.
func (h *AdminWorkspaceImagesHandler) Service() *WorkspaceImageService {
	return h.svc
}

func (h *AdminWorkspaceImagesHandler) Refresh(c *gin.Context) {
	runtimes := AllRuntimes
	if r := c.Query("runtime"); r != "" {
		found := false
		for _, known := range AllRuntimes {
			if known == r {
				found = true
				break
			}
		}
		if !found {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":          fmt.Sprintf("unknown runtime: %s", r),
				"known_runtimes": AllRuntimes,
			})
			return
		}
		runtimes = []string{r}
	}
	recreate := c.DefaultQuery("recreate", "true") == "true"

	res, err := h.svc.Refresh(c.Request.Context(), runtimes, recreate)
	authStatus := "no GHCR auth (public images only)"
	if ghcrAuthHeader() != "" {
		authStatus = "GHCR_USER/GHCR_TOKEN auth"
	}
	log.Printf("workspace-images/refresh: pulled=%d failed=%d recreated=%d (%s)",
		len(res.Pulled), len(res.Failed), len(res.Recreated), authStatus)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "partial_result": res})
		return
	}
	c.JSON(http.StatusOK, res)
}
