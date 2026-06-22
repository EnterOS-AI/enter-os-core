package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/crypto"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

var langfuseClient = &http.Client{Timeout: 10 * time.Second}

type TracesHandler struct{}

func NewTracesHandler() *TracesHandler {
	return &TracesHandler{}
}

type langfuseConfig struct {
	Host   string
	Public string
	Secret string
}

// resolveLangfuseConfig resolves Langfuse connection settings from
// admin-controlled global secrets first, then process env for legacy/dev use.
// Workspace secrets are intentionally excluded: a workspace-controlled
// LANGFUSE_HOST would allow SSRF with BasicAuth attached (#2029).
func resolveLangfuseConfig(ctx context.Context) (*langfuseConfig, error) {
	cfg := &langfuseConfig{}

	resolve := func(key string) string {
		var val []byte
		var ver int
		err := db.DB.QueryRowContext(ctx,
			`SELECT encrypted_value, encryption_version FROM global_secrets WHERE key = $1`,
			key).Scan(&val, &ver)
		if err == nil {
			decrypted, decErr := crypto.DecryptVersioned(val, ver)
			if decErr == nil {
				return string(decrypted)
			}
		}
		return os.Getenv(key)
	}

	cfg.Host = resolve("LANGFUSE_HOST")
	cfg.Public = resolve("LANGFUSE_PUBLIC_KEY")
	cfg.Secret = resolve("LANGFUSE_SECRET_KEY")

	if cfg.Host == "" || cfg.Public == "" || cfg.Secret == "" {
		return nil, nil
	}
	return cfg, nil
}

// List handles GET /workspaces/:id/traces
// Proxies to Langfuse API to get recent traces for a workspace.
func (h *TracesHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")

	cfg, err := resolveLangfuseConfig(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve trace config"})
		return
	}
	if cfg == nil {
		c.JSON(http.StatusOK, []interface{}{})
		return
	}

	// Fetch traces from Langfuse, filtered by workspace tag or name
	url := fmt.Sprintf("%s/api/public/traces?limit=20&orderBy=timestamp&orderDir=desc&tags=%s",
		cfg.Host, workspaceID)

	req, err := http.NewRequestWithContext(c.Request.Context(), "GET", url, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create request"})
		return
	}
	req.SetBasicAuth(cfg.Public, cfg.Secret)

	resp, err := langfuseClient.Do(req)
	if err != nil {
		// Langfuse not available — return empty
		c.JSON(http.StatusOK, []interface{}{})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		c.JSON(http.StatusOK, []interface{}{})
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.JSON(http.StatusOK, []interface{}{})
		return
	}
	c.Data(http.StatusOK, "application/json", body)
}
