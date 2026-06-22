package handlers

// offered_models.go — SSOT model-discovery endpoint (core#2608, CTO
// 2026-06-11: "teach the agent to look up what is available — runtime,
// provider, models"). Agents (the org concierge first) call this BEFORE
// provisioning instead of guessing a model id; the create-boundary
// MISSING_BYOK_CREDENTIAL hard-reject is the enforcement twin.
//
// The payload is derived entirely from the embedded provider registry (the
// providers.yaml SSOT chain) — no new vocabulary: per model id it reports the
// owning provider, whether it is platform-billed (no key), and which auth env
// names a BYOK pick requires.

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
)

// OfferedModel is one selectable (runtime, model) entry.
type OfferedModel struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	// PlatformBilled: true = served via the platform proxy, no credential
	// needed. False = BYOK: the workspace (or org) must hold one of AuthEnv.
	PlatformBilled bool     `json:"platform_billed"`
	AuthEnv        []string `json:"auth_env,omitempty"`
}

// ListOfferedModels handles GET /admin/llm/offered-models?runtime=<rt>.
// Returns the registry's native model menu for the runtime, each entry
// resolved through the SAME DeriveProvider the create gate and the billing
// resolver use — discovery can never disagree with enforcement.
func ListOfferedModels(c *gin.Context) {
	runtime := c.Query("runtime")
	if runtime == "" {
		runtime = "claude-code"
	}
	m, err := providerRegistry()
	if err != nil || m == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provider registry unavailable"})
		return
	}
	models, err := m.ModelsForRuntime(runtime)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown runtime", "runtime": runtime})
		return
	}
	sort.Strings(models)
	out := make([]OfferedModel, 0, len(models))
	for _, id := range models {
		// nil auth context = the registry's deterministic default arm for
		// the id (exact-id ownership; first-declared arm on a tie) — the
		// same resolution a fresh workspace with no credentials gets.
		p, dErr := m.DeriveProvider(runtime, id, nil)
		if dErr != nil {
			continue // ambiguous without auth context — not offerable blind
		}
		entry := OfferedModel{Model: id, Provider: p.Name, PlatformBilled: p.IsPlatform()}
		if !p.IsPlatform() {
			entry.AuthEnv = append(entry.AuthEnv, p.AuthEnv...)
		}
		out = append(out, entry)
	}
	c.JSON(http.StatusOK, gin.H{"runtime": runtime, "models": out})
}
