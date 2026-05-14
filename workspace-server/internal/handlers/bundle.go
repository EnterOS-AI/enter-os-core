package handlers

import (
	"net/http"
	"strings"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/bundle"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
	"github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
)

type BundleHandler struct {
	broadcaster *events.Broadcaster
	provisioner *provisioner.Provisioner
	platformURL string
	configsDir  string
	docker      *client.Client
}

func NewBundleHandler(b *events.Broadcaster, p *provisioner.Provisioner, platformURL, configsDir string, dockerCli *client.Client) *BundleHandler {
	return &BundleHandler{
		broadcaster: b,
		provisioner: p,
		platformURL: platformURL,
		configsDir:  configsDir,
		docker:      dockerCli,
	}
}

// Export handles GET /bundles/export/:id
func (h *BundleHandler) Export(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	b, err := bundle.Export(ctx, workspaceID, h.configsDir, h.docker)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "bundle not found"})
		return
	}

	c.JSON(http.StatusOK, b)
}

// Import handles POST /bundles/import
func (h *BundleHandler) Import(c *gin.Context) {
	var b bundle.Bundle
	if err := c.ShouldBindJSON(&b); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid bundle"})
		return
	}
	if strings.TrimSpace(b.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bundle name is required"})
		return
	}

	ctx := c.Request.Context()
	result := bundle.Import(ctx, &b, nil, h.broadcaster, h.provisioner, h.platformURL)

	c.JSON(http.StatusCreated, result)
}
