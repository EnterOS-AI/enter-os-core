package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ListSources handles GET /plugins/sources — returns the list of
// registered install-source schemes so clients can show users which
// kinds of plugin sources they can install from.
func (h *PluginsHandler) ListSources(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"schemes": h.sources.Schemes()})
}
